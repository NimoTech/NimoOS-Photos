"""HTTP surface replicating the immich-ml contract consumed by pkg/mlclient.

Only /ping and /predict exist. Inference runs in a worker thread so /ping
stays responsive during cold model loads (the root cause of the historical
"port listening, worker wedged" outage mode).
"""
import json
from typing import Any

import anyio
from fastapi import FastAPI, File, Form, HTTPException, UploadFile
from fastapi.responses import PlainTextResponse

from .clipmodel import ClipTextual, ClipVisual
from .config import settings
from .facemodel import FacePipeline
from .ocrmodel import OcrPipeline
from .registry import LazyBackend, ModelRegistry, default_providers

_FACTORIES: dict[str, tuple[Any, str]] = {
    "clip_visual": (ClipVisual, "clip/{name}"),
    "clip_textual": (ClipTextual, "clip/{name}"),
    "face": (FacePipeline, "facial-recognition/{name}"),
    "ocr": (OcrPipeline, "ocr/{name}"),
}


def _build_default_backends() -> tuple[dict[str, Any], ModelRegistry]:
    """Real (lazy, TTL-evicting) backend assembly used when create_app is
    called with backends=None, i.e. actual server startup rather than
    tests passing fakes."""
    registry = ModelRegistry(
        cache_dir=settings.cache_dir,
        ttl_s=settings.model_ttl_s,
        factories=_FACTORIES,
        providers=default_providers(settings.device),
    )
    registry.start_sweeper(settings.ttl_poll_s)
    backends = {kind: LazyBackend(registry, kind) for kind in _FACTORIES}
    return backends, registry


def _bind_model(backend: Any, model_name: str | None) -> Any:
    """Return `backend` bound to `model_name` if it's a LazyBackend
    (needs a with_model() call to know which model to resolve), otherwise
    return it unchanged -- this is what keeps the explicit-backends test
    path (fakes with no with_model attribute) working untouched."""
    with_model = getattr(backend, "with_model", None)
    return with_model(model_name) if with_model is not None else backend


def create_app(backends: dict[str, Any] | None = None) -> FastAPI:
    app = FastAPI(docs_url=None, redoc_url=None, openapi_url=None)
    if backends is not None:
        app.state.backends = backends
        app.state.registry = None
    else:
        app.state.backends, app.state.registry = _build_default_backends()

    @app.get("/ping", response_class=PlainTextResponse)
    def ping() -> str:
        return "pong"

    @app.post("/predict")
    async def predict(entries: str = Form(...),
                      image: UploadFile | None = File(None),
                      text: str | None = Form(None)) -> dict[str, Any]:
        try:
            tasks = json.loads(entries)
        except json.JSONDecodeError as e:
            raise HTTPException(400, f"invalid entries JSON: {e}") from e
        img = await image.read() if image is not None else None
        b = app.state.backends
        out: dict[str, Any] = {}
        handled = False
        if "clip" in tasks:
            handled = True
            cfg = tasks["clip"]
            if "visual" in cfg:
                if img is None:
                    raise HTTPException(400, "clip.visual requires an image")
                visual = _bind_model(b["clip_visual"], cfg["visual"].get("modelName"))
                out["clip"] = await anyio.to_thread.run_sync(visual.embed_image, img)
            elif "textual" in cfg:
                if text is None:
                    raise HTTPException(400, "clip.textual requires text")
                textual = _bind_model(b["clip_textual"], cfg["textual"].get("modelName"))
                out["clip"] = await anyio.to_thread.run_sync(textual.embed_text, text)
            else:
                raise HTTPException(400, "clip task needs visual or textual")
        if "facial-recognition" in tasks:
            handled = True
            if img is None:
                raise HTTPException(400, "facial-recognition requires an image")
            fr_cfg = tasks["facial-recognition"]
            # detection and recognition share one on-disk model bundle
            # (facial-recognition/{name}/{detection,recognition}/), so a
            # single modelName drives both stages; detection's wins if the
            # two ever disagree.
            model_name = (fr_cfg.get("detection") or {}).get("modelName") or \
                (fr_cfg.get("recognition") or {}).get("modelName")
            face = _bind_model(b["face"], model_name)
            out.update(await anyio.to_thread.run_sync(face.detect, img))
        if "ocr" in tasks:
            handled = True
            if img is None:
                raise HTTPException(400, "ocr requires an image")
            ocr_cfg = tasks["ocr"]
            model_name = (ocr_cfg.get("detection") or {}).get("modelName") or \
                (ocr_cfg.get("recognition") or {}).get("modelName")
            ocr = _bind_model(b["ocr"], model_name)
            out.update(await anyio.to_thread.run_sync(ocr.run, img))
        if not handled:
            raise HTTPException(400, f"no recognized task in entries: {list(tasks)}")
        return out

    return app

app = create_app()
