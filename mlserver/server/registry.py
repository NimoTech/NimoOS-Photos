"""Lazy model registry with idle-TTL eviction.

Mirrors immich-ml's MODEL_TTL semantics but in-process: no worker suicide,
no gunicorn respawn -- which removes the historical wedge path where the
respawned worker booted but never finished loading. Loading happens in the
caller's (thread-pool) thread; the event loop and /ping are never blocked.
"""
import ctypes
import gc
import logging
import threading
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

_logger = logging.getLogger(__name__)


def _release_freed_memory() -> None:
    """Return glibc's freed-but-retained heap pages to the OS.

    Dropping the last reference to a loaded model (see unload_idle below)
    frees it immediately via CPython refcounting -- confirmed empirically:
    VmRSS after `entry.model = None` + `gc.collect()` alone barely moves
    (e.g. ~2.0GB -> ~1.65GB for one loaded CLIP visual tower), because
    glibc's malloc arena keeps the freed chunks mapped rather than
    munmap()-ing them back to the OS. Calling malloc_trim(0) after the
    fact is what actually returns that memory (same case dropped RSS from
    ~1.65GB to ~67MB in the same experiment) -- without it, TTL eviction
    would be correct in principle but invisible to any RSS-based check
    (ours, monitoring, or an OOM-averse process supervisor) for an
    indefinite amount of time. Best-effort: silently a no-op on non-glibc
    libc (e.g. musl-based containers) where malloc_trim doesn't exist.
    """
    gc.collect()
    try:
        ctypes.CDLL("libc.so.6").malloc_trim(0)
    except OSError:
        pass


@dataclass
class _Entry:
    model: Any = None
    last_used: float = 0.0
    lock: threading.Lock = field(default_factory=threading.Lock)


class ModelRegistry:
    def __init__(self, cache_dir: Path, ttl_s: int,
                 factories: dict[str, tuple[Callable, str]],
                 providers: list) -> None:
        self.cache_dir, self.ttl_s = cache_dir, ttl_s
        self.factories, self.providers = factories, providers
        self._entries: dict[str, _Entry] = {}
        self._map_lock = threading.Lock()

    def get(self, kind: str, model_name: str) -> Any:
        key = f"{kind}:{model_name}"
        with self._map_lock:
            entry = self._entries.setdefault(key, _Entry())
        with entry.lock:
            if entry.model is None:
                cls, subpath = self.factories[kind]
                entry.model = cls(self.cache_dir / subpath.format(name=model_name),
                                   self.providers)
            entry.last_used = time.monotonic()
            return entry.model

    def unload_idle(self, now: float) -> None:
        with self._map_lock:
            items = list(self._entries.items())
        unloaded = False
        for key, entry in items:
            with entry.lock:
                if entry.model is not None and now - entry.last_used > self.ttl_s:
                    entry.model = None  # drop; ORT frees native memory on GC
                    unloaded = True
        if unloaded:
            _release_freed_memory()

    def start_sweeper(self, poll_s: int) -> None:
        def loop() -> None:
            while True:
                time.sleep(poll_s)
                try:
                    self.unload_idle(time.monotonic())
                except Exception:
                    # A single bad sweep (e.g. a factory/eviction bug, or
                    # _release_freed_memory hitting something unexpected)
                    # must never kill this daemon thread -- there is no
                    # supervisor to restart it, so an uncaught exception
                    # here would silently disable TTL eviction forever,
                    # which is exactly the kind of slow-drift OOM this
                    # project has been bitten by before. Log and keep
                    # ticking; the next poll_s cycle tries again.
                    _logger.warning("ttl-sweeper: unload_idle failed; will retry next tick",
                                     exc_info=True)
        threading.Thread(target=loop, daemon=True, name="ttl-sweeper").start()


def default_providers(device: str) -> list[str]:
    """Placeholder device -> ONNX Runtime providers mapping.

    Always returns CPUExecutionProvider for now; Task 7 replaces this with
    real device selection (CUDA/OpenVINO/etc based on `device`). Kept as a
    standalone function so the registry's `providers` constructor argument
    stays a real seam rather than a dead parameter.
    """
    return ["CPUExecutionProvider"]


class LazyBackend:
    """Duck-type proxy standing in for a concrete backend (ClipVisual,
    FacePipeline, ...) behind a ModelRegistry.

    Exposes the same call signatures the fake backends in test_api.py use
    (single positional arg: bytes or str) -- the model name is NOT part of
    those signatures. Instead, `with_model(name)` returns a *new* bound
    proxy carrying that name; the predict handler in main.py calls it once
    per request with the modelName pulled from that request's `entries`
    JSON, then hands the returned proxy to anyio.to_thread.run_sync. This
    keeps LazyBackend immutable per request (safe under concurrent
    requests picking different model names) and keeps model resolution --
    and any first-load cost -- inside the worker thread, never on the
    event loop.
    """

    def __init__(self, registry: ModelRegistry, kind: str, model_name: str | None = None) -> None:
        self.registry = registry
        self.kind = kind
        self.model_name = model_name

    def with_model(self, model_name: str) -> "LazyBackend":
        return LazyBackend(self.registry, self.kind, model_name)

    def _resolve(self) -> Any:
        if self.model_name is None:
            raise RuntimeError(f"LazyBackend({self.kind}) used without with_model()")
        return self.registry.get(self.kind, self.model_name)

    # -- duck-type methods mirroring ClipVisual/ClipTextual/FacePipeline/OcrPipeline --

    def embed_image(self, data: bytes) -> list[float]:
        return self._resolve().embed_image(data)

    def embed_text(self, text: str) -> list[float]:
        return self._resolve().embed_text(text)

    def detect(self, data: bytes) -> dict:
        return self._resolve().detect(data)

    def run(self, data: bytes) -> dict:
        return self._resolve().run(data)
