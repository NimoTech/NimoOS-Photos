"""Quality-signal unit tests: pure functions, no model needed."""
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
