import json
import pytest
from httpx import ASGITransport, AsyncClient

from server.main import create_app

class FakeClipVisual:
    def embed_image(self, data: bytes) -> list[float]:
        return [0.1] * 1152

class FakeClipTextual:
    def embed_text(self, text: str) -> list[float]:
        return [0.2] * 1152

class FakeFace:
    def detect(self, data: bytes) -> dict:
        return {"facial-recognition": [{
            "boundingBox": {"x1": 10.0, "y1": 20.0, "x2": 110.0, "y2": 140.0},
            "embedding": json.dumps([0.3] * 512),   # MUST be a JSON string
            "score": 0.98}],
            "imageWidth": 640, "imageHeight": 480}

class FakeOcr:
    def run(self, data: bytes) -> dict:
        return {"ocr": {"text": ["hello"], "textScore": [0.99],
                        "box": [0.1, 0.1, 0.9, 0.1, 0.9, 0.2, 0.1, 0.2],  # flat, normalized
                        "boxScore": [0.9]},
                "imageWidth": 640, "imageHeight": 480}

@pytest.fixture
def client():
    app = create_app(backends={"clip_visual": FakeClipVisual(), "clip_textual": FakeClipTextual(),
                               "face": FakeFace(), "ocr": FakeOcr()})
    return AsyncClient(transport=ASGITransport(app=app), base_url="http://t")

async def _predict(client, entries: dict, image: bytes | None = None, text: str | None = None):
    data = {"entries": json.dumps(entries)}
    files = {"image": ("image.jpg", image, "application/octet-stream")} if image else None
    if text is not None:
        data["text"] = text
    r = await client.post("/predict", data=data, files=files)
    assert r.status_code == 200, r.text
    return r.json()

@pytest.mark.anyio
async def test_ping(client):
    r = await client.get("/ping")
    assert r.status_code == 200 and "pong" in r.text

@pytest.mark.anyio
async def test_clip_visual_returns_1152_array(client):
    out = await _predict(client, {"clip": {"visual": {"modelName": "m"}}}, image=b"x")
    assert isinstance(out["clip"], list) and len(out["clip"]) == 1152

@pytest.mark.anyio
async def test_clip_textual(client):
    out = await _predict(client, {"clip": {"textual": {"modelName": "m"}}}, text="hello")
    assert len(out["clip"]) == 1152

@pytest.mark.anyio
async def test_face_embedding_is_json_string(client):
    out = await _predict(client, {"facial-recognition": {
        "detection": {"modelName": "m"}, "recognition": {"modelName": "m"}}}, image=b"x")
    face = out["facial-recognition"][0]
    assert isinstance(face["embedding"], str)          # the landmine: string, not array
    assert len(json.loads(face["embedding"])) == 512
    assert set(face["boundingBox"]) == {"x1", "y1", "x2", "y2"}

@pytest.mark.anyio
async def test_ocr_flat_normalized_box(client):
    out = await _predict(client, {"ocr": {
        "detection": {"modelName": "m"}, "recognition": {"modelName": "m"}}}, image=b"x")
    o = out["ocr"]
    assert len(o["box"]) == 8 * len(o["text"])          # flat array, 8 per line
    assert all(0.0 <= v <= 1.0 for v in o["box"])       # normalized

@pytest.mark.anyio
async def test_unknown_task_is_400(client):
    r = await client.post("/predict", data={"entries": json.dumps({"nope": {}})})
    assert r.status_code == 400
