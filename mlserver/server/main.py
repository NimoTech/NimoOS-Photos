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

def create_app(backends: dict[str, Any] | None = None) -> FastAPI:
    app = FastAPI()
    app.state.backends = backends if backends is not None else {}

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
                out["clip"] = await anyio.to_thread.run_sync(b["clip_visual"].embed_image, img)
            elif "textual" in cfg:
                if text is None:
                    raise HTTPException(400, "clip.textual requires text")
                out["clip"] = await anyio.to_thread.run_sync(b["clip_textual"].embed_text, text)
            else:
                raise HTTPException(400, "clip task needs visual or textual")
        if "facial-recognition" in tasks:
            handled = True
            if img is None:
                raise HTTPException(400, "facial-recognition requires an image")
            out.update(await anyio.to_thread.run_sync(b["face"].detect, img))
        if "ocr" in tasks:
            handled = True
            if img is None:
                raise HTTPException(400, "ocr requires an image")
            out.update(await anyio.to_thread.run_sync(b["ocr"].run, img))
        if not handled:
            raise HTTPException(400, f"no recognized task in entries: {list(tasks)}")
        return out

    return app

app = create_app()
