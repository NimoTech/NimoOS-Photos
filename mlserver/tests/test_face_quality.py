"""Quality-signal unit tests: pure functions, no model needed."""
import json

import numpy as np
import cv2

from server.facemodel import frontality_from_kps, sharpness_from_crop


def _kps(nose_shift: float) -> np.ndarray:
    # eyes at (40,50) and (72,50); nose below midpoint, shifted by nose_shift px
    return np.array([
        [40.0, 50.0], [72.0, 50.0],
        [56.0 + nose_shift, 70.0],
        [46.0, 90.0], [66.0, 90.0],
    ], dtype=np.float32)


def test_frontality_ordering():
    frontal = frontality_from_kps(_kps(0.0))
    slight = frontality_from_kps(_kps(4.0))
    profile = frontality_from_kps(_kps(14.0))
    assert frontal == 1.0
    assert frontal > slight > profile
    assert 0.0 <= profile < 0.2


def test_frontality_degenerate_eyes():
    kps = np.zeros((5, 2), dtype=np.float32)  # coincident landmarks
    assert frontality_from_kps(kps) == 0.0


def test_sharpness_ordering():
    rng = np.random.default_rng(7)
    sharp = rng.integers(0, 256, (112, 112, 3), dtype=np.uint8)  # high-frequency noise = very sharp
    blurred = cv2.GaussianBlur(sharp, (21, 21), 8)
    s1, s2 = sharpness_from_crop(sharp), sharpness_from_crop(blurred)
    assert 0.0 <= s2 < s1 <= 1.0
    assert s1 > 0.75
    assert s2 < 0.5


def test_quality_signals_are_python_floats():
    """Regression: kps/crop derive from numpy arrays, so the arithmetic in
    frontality_from_kps/sharpness_from_crop can silently yield np.float32
    (e.g. via max(0.0, np.float32(...))). FastAPI/pydantic cannot serialize
    numpy scalar types, so these must be native Python floats, not just
    numerically float-like."""
    frontal_kps = _kps(0.0)  # frontality > 0 branch (the one that regressed in prod)
    rng = np.random.default_rng(11)
    crop = rng.integers(0, 256, (112, 112, 3), dtype=np.uint8)

    frontality = frontality_from_kps(frontal_kps)
    sharpness = sharpness_from_crop(crop)

    assert type(frontality) is float, f"got {type(frontality)}"
    assert type(sharpness) is float, f"got {type(sharpness)}"

    # Degenerate/early-return branches must also be native floats.
    assert type(frontality_from_kps(np.zeros((5, 2), dtype=np.float32))) is float


def test_face_response_json_serializable():
    """Regression: build the per-face response dict exactly as
    FacePipeline.detect() does, using the real quality functions on
    synthetic inputs, and confirm json.dumps (the boundary FastAPI/pydantic
    hits) does not raise PydanticSerializationError-equivalent TypeErrors."""
    kps = _kps(0.0)
    rng = np.random.default_rng(13)
    crop = rng.integers(0, 256, (112, 112, 3), dtype=np.uint8)
    emb = rng.random(512).astype(np.float32)

    face = {
        "boundingBox": {"x1": 1.0, "y1": 2.0, "x2": 3.0, "y2": 4.0},
        "embedding": json.dumps([float(v) for v in emb]),
        "score": 0.99,
        "frontality": frontality_from_kps(kps),
        "sharpness": sharpness_from_crop(crop),
    }

    # This is the exact boundary that raised
    # PydanticSerializationError: Unable to serialize unknown type: numpy.float32
    json.dumps(face)
