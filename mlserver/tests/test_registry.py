import threading
import time
from pathlib import Path

import pytest

from server.registry import LazyBackend, ModelRegistry, default_providers


class Dummy:
    loads = 0

    def __init__(self, model_dir: Path, providers: list) -> None:
        Dummy.loads += 1
        self.model_dir = model_dir
        self.providers = providers

    def embed_image(self, data: bytes) -> str:
        return f"{self.model_dir.name}:{data!r}"


def test_lazy_load_once_under_concurrency(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=300,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    Dummy.loads = 0
    threads = [threading.Thread(target=lambda: reg.get("clip_visual", "m")) for _ in range(8)]
    [t.start() for t in threads]
    [t.join() for t in threads]
    assert Dummy.loads == 1            # double-checked locking


def test_ttl_unload(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=1,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    Dummy.loads = 0
    reg.get("clip_visual", "m")
    time.sleep(1.2)
    reg.unload_idle(time.monotonic())
    reg.get("clip_visual", "m")
    assert Dummy.loads == 2            # was unloaded, loaded again


def test_get_passes_cache_dir_and_providers(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=300,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    model = reg.get("clip_visual", "some-model")
    assert model.model_dir == tmp_path / "clip" / "some-model"
    assert model.providers == ["CPUExecutionProvider"]


def test_distinct_model_names_load_distinct_entries(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=300,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    Dummy.loads = 0
    a = reg.get("clip_visual", "model-a")
    b = reg.get("clip_visual", "model-b")
    a_again = reg.get("clip_visual", "model-a")
    assert Dummy.loads == 2
    assert a is a_again
    assert a is not b


def test_default_providers_is_cpu_placeholder():
    assert default_providers("auto") == ["CPUExecutionProvider"]
    assert default_providers("gpu.0") == ["CPUExecutionProvider"]


def test_lazy_backend_requires_with_model_before_use(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=300,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    backend = LazyBackend(reg, "clip_visual")
    with pytest.raises(RuntimeError):
        backend.embed_image(b"x")


def test_lazy_backend_with_model_resolves_through_registry(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=300,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    Dummy.loads = 0
    backend = LazyBackend(reg, "clip_visual")
    bound = backend.with_model("m")
    assert bound is not backend            # with_model returns a fresh proxy
    assert backend.model_name is None      # original left untouched
    out = bound.embed_image(b"x")
    assert out == "m:b'x'"
    assert Dummy.loads == 1
    # a second call with the same model name reuses the loaded instance
    bound.embed_image(b"y")
    assert Dummy.loads == 1


def test_unload_idle_triggers_malloc_trim(tmp_path, monkeypatch):
    """RSS regression guard: unload_idle must actually try to return
    freed heap memory to the OS, not just drop the Python reference --
    see _release_freed_memory's docstring for the empirical why."""
    import server.registry as registry_mod

    calls = []
    monkeypatch.setattr(registry_mod, "_release_freed_memory", lambda: calls.append(1))

    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=1,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    reg.get("clip_visual", "m")
    reg.unload_idle(time.monotonic())          # nothing idle yet -- ttl_s=1, no time passed
    assert calls == []
    time.sleep(1.2)
    reg.unload_idle(time.monotonic())          # now idle -- should trigger a trim
    assert calls == [1]


def test_sweeper_survives_unload_idle_exception(tmp_path, monkeypatch):
    """A single bad sweep must not kill the daemon thread -- TTL eviction
    should keep ticking on subsequent polls instead of silently dying."""
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=0,   # idle immediately on the next tick
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    Dummy.loads = 0
    reg.get("clip_visual", "m")

    real_unload_idle = reg.unload_idle
    calls = {"n": 0}

    def flaky_unload_idle(now):
        calls["n"] += 1
        if calls["n"] == 1:
            raise RuntimeError("boom: simulated sweep failure")
        return real_unload_idle(now)

    monkeypatch.setattr(reg, "unload_idle", flaky_unload_idle)

    reg.start_sweeper(poll_s=0.05)
    deadline = time.monotonic() + 3.0
    while calls["n"] < 2 and time.monotonic() < deadline:
        time.sleep(0.05)

    assert calls["n"] >= 2, "sweeper loop died after the first exception instead of retrying"
    assert reg._entries["clip_visual:m"].model is None, \
        "a later successful sweep should still have unloaded the idle model"


def test_lazy_backend_different_calls_can_bind_different_models(tmp_path):
    reg = ModelRegistry(cache_dir=tmp_path, ttl_s=300,
                         factories={"clip_visual": (Dummy, "clip/{name}")},
                         providers=["CPUExecutionProvider"])
    Dummy.loads = 0
    backend = LazyBackend(reg, "clip_visual")
    out_a = backend.with_model("model-a").embed_image(b"x")
    out_b = backend.with_model("model-b").embed_image(b"x")
    assert out_a == "model-a:b'x'"
    assert out_b == "model-b:b'x'"
    assert Dummy.loads == 2
