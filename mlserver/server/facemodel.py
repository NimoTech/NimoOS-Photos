"""Face detection + recognition using the insightface (MIT) library —
the very same library immich-ml calls internally, with identical
parameters (det_thresh=0.7, input_size=640x640), so numerical parity
is by construction rather than by reimplementation.

Contract quirks required by pkg/mlclient:
- boundingBox is in ABSOLUTE pixels of the submitted image;
- embedding is serialized as a JSON string, not an array.

Decoding must NOT apply EXIF transpose, to match immich-ml: its face
pipeline decodes via PIL (`decode_pil`/`decode_cv2` in
immich_ml/models/transforms.py), which never calls
`ImageOps.exif_transpose`, so images are fed to SCRFD/ArcFace in their
raw stored orientation regardless of the EXIF orientation tag.

IMPORTANT: cv2.imdecode's *default* behavior is NOT orientation-neutral
-- since OpenCV 3.2, IMREAD_COLOR implicitly auto-rotates according to
the EXIF orientation tag unless IMREAD_IGNORE_ORIENTATION is also
passed. Using the plain cv2.imdecode(..., cv2.IMREAD_COLOR) as in the
task brief's skeleton silently auto-corrects rotated photos, which
diverges from immich-ml on every EXIF-rotated sample (confirmed via the
golden dataset's exif-rotated bucket: cv2 default decode reported
1920x1080 -> 1080x1920 for an orientation=6 photo, while immich-ml's
baseline kept the raw 1920x1080 dimensions and, on several samples,
detected zero faces in the still-sideways image). We therefore add
IMREAD_IGNORE_ORIENTATION to reproduce immich-ml's un-rotated decode.

OpenCV version pin: requirements.txt pins opencv-python-headless to
4.13.0.92, matching the exact cv2 build inside
ghcr.io/immich-app/immich-machine-learning:v2.7.5-openvino (verified via
`docker run --entrypoint python <image> -c "import cv2; print(cv2.__version__)"`).
pip's default opencv-python-headless resolves to the OpenCV 5.x series,
which -- for a small number of harder/lower-confidence detections in
the golden set -- produced embeddings with cosine similarity as low as
0.9969 against the baseline (vs. 0.999995+ for the rest), even though
the decoded input pixels, detection scores, and bounding boxes were all
bit-identical between OpenCV versions. This isolates the divergence to
norm_crop's cv2.warpAffine (the only cv2 geometry op between detection
and the recognition model): OpenCV 5.x apparently changed
warpAffine/resize's interpolation kernel or floating-point rounding
just enough to perturb ArcFace's already-alignment-sensitive input.
Pinning to 4.13.0.92 raised every one of the previously-failing pairs
to cosine >= 0.99997.
"""
import json
from pathlib import Path

import cv2
import numpy as np
from insightface.model_zoo import get_model
from insightface.utils.face_align import norm_crop

DET_THRESH = 0.7
DET_SIZE = (640, 640)


def decode_bgr(data: bytes) -> np.ndarray:
    """Decode to a BGR array without EXIF auto-rotation (see module docstring)."""
    img = cv2.imdecode(np.frombuffer(data, np.uint8),
                        cv2.IMREAD_COLOR | cv2.IMREAD_IGNORE_ORIENTATION)
    if img is None:
        raise ValueError("cannot decode image")
    return img


def frontality_from_kps(kps: np.ndarray) -> float:
    """Symmetry heuristic from the 5-point landmarks (left eye, right eye,
    nose, mouth-left, mouth-right): a frontal face has its nose x near the
    eye midpoint. Returns 1.0 for perfectly frontal, approaching 0.0 for
    strong profiles. Heuristic, not a pose model — good enough to rank
    cover candidates."""
    le, re_, nose = kps[0], kps[1], kps[2]
    eye_dist = float(np.linalg.norm(re_ - le))
    if eye_dist < 1e-6:
        return 0.0
    mid_x = (le[0] + re_[0]) / 2.0
    dev = abs(float(nose[0]) - mid_x) / eye_dist
    # float(...) is required here: dev is derived from numpy scalar
    # arithmetic, so max(0.0, 1.0 - 2.0 * dev) can yield np.float32 (when
    # that branch wins), which FastAPI/pydantic cannot JSON-serialize.
    return float(max(0.0, 1.0 - 2.0 * dev))


SHARPNESS_K = 100.0  # Laplacian-variance half-point: var==K maps to 0.5


def sharpness_from_crop(crop_bgr: np.ndarray) -> float:
    """Blur measure on the 112x112 aligned crop: variance of the Laplacian
    on grayscale, squashed via v/(v+K) to a value in [0,1); exactly 0.0 only
    for a zero-gradient (flat) crop. Monotonic; K chosen so a typically sharp
    face (var >= ~300) scores >= 0.75 and a heavily blurred one (var <= ~30)
    scores <= 0.23."""
    gray = cv2.cvtColor(crop_bgr, cv2.COLOR_BGR2GRAY)
    v = float(cv2.Laplacian(gray, cv2.CV_64F).var())
    # float(...) is required here: cv2 return types are not guaranteed to be
    # native Python floats, and FastAPI/pydantic cannot JSON-serialize numpy
    # scalar types at the API serialization boundary.
    return float(v / (v + SHARPNESS_K))


class FacePipeline:
    def __init__(self, model_dir: Path, providers: list) -> None:
        self.det = get_model(str(model_dir / "detection" / "model.onnx"), providers=providers)
        self.det.prepare(ctx_id=0, det_thresh=DET_THRESH, input_size=DET_SIZE)
        self.rec = get_model(str(model_dir / "recognition" / "model.onnx"), providers=providers)
        self.rec.prepare(ctx_id=0)

    def detect(self, data: bytes) -> dict:
        img = decode_bgr(data)
        h, w = img.shape[:2]
        bboxes, kpss = self.det.detect(img, max_num=0)
        faces = []
        for bbox, kps in zip(bboxes, kpss if kpss is not None else []):
            x1, y1, x2, y2, score = [float(v) for v in bbox]
            crop = norm_crop(img, kps)
            emb = self.rec.get_feat(crop).flatten().astype(np.float32)
            faces.append({
                "boundingBox": {"x1": x1, "y1": y1, "x2": x2, "y2": y2},
                "embedding": json.dumps([float(v) for v in emb]),
                "score": score,
                "frontality": frontality_from_kps(kps),
                "sharpness": sharpness_from_crop(crop),
            })
        return {"facial-recognition": faces, "imageWidth": w, "imageHeight": h}
