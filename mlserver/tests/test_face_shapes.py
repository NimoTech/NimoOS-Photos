import io, json
import numpy as np
import pytest
from PIL import Image
from pathlib import Path

CACHE = Path("/DATA/.system_data/photos/ml-cache")
needs_cache = pytest.mark.skipif(not CACHE.exists(), reason="ml-cache not present")


@needs_cache
def test_face_response_shape():
    from server.facemodel import FacePipeline
    fp = FacePipeline(CACHE / "facial-recognition" / "antelopev2", ["CPUExecutionProvider"])
    buf = io.BytesIO()
    Image.new("RGB", (640, 480), (128, 128, 128)).save(buf, format="JPEG")
    out = fp.detect(buf.getvalue())
    assert out["imageWidth"] == 640 and out["imageHeight"] == 480
    assert out["facial-recognition"] == []          # gray image: no faces


def test_decode_ignores_exif_orientation():
    """Regression for the golden-parity bug found during calibration:
    cv2.imdecode's default IMREAD_COLOR auto-rotates per the EXIF
    orientation tag, but immich-ml decodes via PIL without ever calling
    ImageOps.exif_transpose, so its face pipeline always sees the raw,
    un-rotated pixel grid. decode_bgr must reproduce that (no rotation)
    or bounding boxes / face counts silently diverge on every rotated
    photo (this does not require the ml-cache models, so it always runs)."""
    from server.facemodel import decode_bgr

    buf = io.BytesIO()
    img = Image.new("RGB", (300, 200), (0, 0, 0))  # landscape, w != h
    exif = img.getexif()
    exif[274] = 6  # Orientation: rotate 90 CW to display upright
    img.save(buf, format="JPEG", exif=exif)

    decoded = decode_bgr(buf.getvalue())
    h, w = decoded.shape[:2]
    assert (w, h) == (300, 200)  # unchanged -- NOT auto-rotated to (200, 300)
