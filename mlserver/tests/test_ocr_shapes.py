import io
from pathlib import Path

import pytest
from PIL import Image, ImageDraw

CACHE = Path("/DATA/.system_data/photos/ml-cache")
needs_cache = pytest.mark.skipif(not CACHE.exists(), reason="ml-cache not present")


def _synthetic_text_jpeg() -> bytes:
    img = Image.new("RGB", (640, 200), (255, 255, 255))
    draw = ImageDraw.Draw(img)
    # Default PIL font at a large synthetic scale via multiple overdraws:
    # draw the text several times with 1px offsets to fatten the default
    # bitmap font so the detector's DB thresholding reliably fires on it.
    text = "HELLO WORLD 2026"
    for dx in range(3):
        for dy in range(3):
            draw.text((20 + dx, 80 + dy), text, fill=(0, 0, 0))
    buf = io.BytesIO()
    img.save(buf, format="JPEG")
    return buf.getvalue()


@needs_cache
def test_ocr_response_shape():
    from server.ocrmodel import OcrPipeline

    op = OcrPipeline(CACHE / "ocr" / "PP-OCRv5_server", ["CPUExecutionProvider"])
    out = op.run(_synthetic_text_jpeg())

    assert out["imageWidth"] == 640
    assert out["imageHeight"] == 200

    ocr = out["ocr"]
    assert len(ocr["text"]) > 0
    assert len(ocr["textScore"]) == len(ocr["text"])
    assert len(ocr["boxScore"]) == len(ocr["text"])
    assert len(ocr["box"]) == 8 * len(ocr["text"])
    assert all(0.0 <= v <= 1.0 for v in ocr["box"])


@needs_cache
def test_ocr_response_shape_blank_image():
    from server.ocrmodel import OcrPipeline

    op = OcrPipeline(CACHE / "ocr" / "PP-OCRv5_server", ["CPUExecutionProvider"])
    buf = io.BytesIO()
    Image.new("RGB", (640, 480), (128, 128, 128)).save(buf, format="JPEG")
    out = op.run(buf.getvalue())

    assert out["imageWidth"] == 640 and out["imageHeight"] == 480
    assert out["ocr"]["text"] == []
    assert out["ocr"]["box"] == []
