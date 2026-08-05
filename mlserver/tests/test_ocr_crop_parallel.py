"""Regression tests for perf/ocr-crop-throughput's OCR crop-loop changes:

1. `_crop_line` now takes a pre-built PIL Image (hoisted once per
   `run()` call, not re-converted from the numpy array once per
   detected line) instead of the raw ndarray.
2. `_crop_lines_parallel` fans the per-line crops out across a small
   thread pool (`_CROP_EXECUTOR`) for wall-clock parallelism, since
   PIL/cv2's C code releases the GIL for these calls.

Both are pure execution-mechanics changes -- the actual crop math in
`_crop_line` is untouched. These tests lock in the property the golden
byte-parity gate depends on: parallel execution must produce the exact
same crops, in the exact same order, as a plain sequential loop would,
regardless of which box's crop happens to finish first.

No ml-cache/real ONNX model needed -- these exercise `_crop_line`/
`_crop_lines_parallel` directly against a synthetic image, never
touching TextDetector/TextRecognizer or an ONNX Runtime session.
"""
from pathlib import Path

import numpy as np
import pytest
from PIL import Image

import server.ocrmodel as ocrmodel

CACHE = Path("/DATA/.system_data/photos/ml-cache")
needs_cache = pytest.mark.skipif(not CACHE.exists(), reason="ml-cache not present")


def _synthetic_bgr_image(h: int = 300, w: int = 400) -> np.ndarray:
    """A deterministic, non-uniform image (not random) so pixel-level
    equality checks below are reproducible without a fixed RNG seed."""
    y, x = np.mgrid[0:h, 0:w]
    img = np.zeros((h, w, 3), dtype=np.uint8)
    img[:, :, 0] = (x * 255 // max(w - 1, 1)).astype(np.uint8)
    img[:, :, 1] = (y * 255 // max(h - 1, 1)).astype(np.uint8)
    img[:, :, 2] = ((x + y) % 256).astype(np.uint8)
    return img


def _box(x0: float, y0: float, x1: float, y1: float) -> np.ndarray:
    """An axis-aligned quad (4 corners, clockwise from top-left) -- the
    same box shape `TextDetector.postprocess_op` hands `run()`."""
    return np.array([[x0, y0], [x1, y0], [x1, y1], [x0, y1]], dtype=np.float32)


def _sample_boxes() -> list:
    # Varying sizes/aspect ratios (some tall enough to hit the
    # rotate-90 rule in _crop_line) so the thread pool's workers see
    # genuinely different amounts of work, not uniform no-op crops.
    return [
        _box(10, 10, 120, 40),
        _box(50, 60, 90, 220),  # tall -> triggers h/w >= 1.5 rotate-90
        _box(200, 100, 380, 130),
        _box(30, 200, 370, 280),
        _box(150, 20, 170, 260),  # tall/narrow
        _box(5, 5, 395, 30),
    ]


def test_crop_lines_parallel_matches_serial_pixel_for_pixel():
    """The whole point of parallelizing: identical output to a plain
    per-box loop, not just "close" -- byte parity is the hard gate."""
    img = _synthetic_bgr_image()
    img_pil = Image.fromarray(img)
    boxes = _sample_boxes()

    serial = [ocrmodel._crop_line(img_pil, b) for b in boxes]
    parallel = ocrmodel._crop_lines_parallel(img_pil, boxes)

    assert len(parallel) == len(serial) == len(boxes)
    for i, (s, p) in enumerate(zip(serial, parallel)):
        assert s.shape == p.shape, f"box {i}: shape mismatch {s.shape} vs {p.shape}"
        assert np.array_equal(s, p), f"box {i}: pixel mismatch"


def test_crop_lines_parallel_preserves_input_order():
    """Recognition zips crops back against det_boxes/det_scores by
    position -- if the thread pool ever returned results in completion
    order instead of submission order, lines would silently pair with
    the wrong box. Distinguish boxes by crop shape (each box here has a
    unique width) so a reordering would be caught even if two crops
    happened to contain identical pixel data."""
    img = _synthetic_bgr_image()
    img_pil = Image.fromarray(img)
    boxes = _sample_boxes()

    parallel = ocrmodel._crop_lines_parallel(img_pil, boxes)
    expected_widths = [int(max(np.linalg.norm(b[0] - b[1]), np.linalg.norm(b[2] - b[3]))) for b in boxes]

    for i, (box, crop) in enumerate(zip(boxes, parallel)):
        # _crop_line rotates 90 degrees whenever h/w >= 1.5, so the
        # pre-rotation width can end up in either dimension -- either
        # is fine, as long as it's THIS box's width, not some other
        # box's (which is what a scrambled order would produce).
        h, w = crop.shape[0], crop.shape[1]
        assert expected_widths[i] in (w, h), (
            f"box {i}: crop dims {crop.shape[:2]} don't match expected width {expected_widths[i]} "
            f"(order likely scrambled)"
        )


def test_crop_lines_parallel_single_box_uses_serial_path():
    """len(boxes) <= 1 skips the executor entirely (not worth the
    dispatch overhead for one line) -- still must match _crop_line."""
    img = _synthetic_bgr_image()
    img_pil = Image.fromarray(img)
    box = _sample_boxes()[0]

    result = ocrmodel._crop_lines_parallel(img_pil, [box])
    expected = ocrmodel._crop_line(img_pil, box)

    assert len(result) == 1
    assert np.array_equal(result[0], expected)


def test_crop_lines_parallel_empty_boxes():
    img = _synthetic_bgr_image()
    img_pil = Image.fromarray(img)
    assert ocrmodel._crop_lines_parallel(img_pil, []) == []


def test_crop_workers_bounded_and_positive():
    """Module-level worker count must be a small, positive bound (not
    0, not unbounded) -- see the constant's comment for why (this
    server also runs CLIP/face inference concurrently)."""
    assert 1 <= ocrmodel._CROP_WORKERS <= 6


@needs_cache
def test_run_converts_full_image_to_pil_exactly_once_per_call(monkeypatch):
    """The actual perf bug this branch fixes: `run()` must build the
    whole-image PIL object ONCE per call, not once per detected text
    line. Uses a real OcrPipeline (needs real det/rec ONNX sessions to
    construct) but stubs out detection/recognition themselves with
    fakes that report 20 boxes -- so if a future change reintroduces a
    per-line `Image.fromarray(img)` inside the crop loop, this test
    catches it regardless of how many lines a real photo happens to
    detect."""
    op = ocrmodel.OcrPipeline(CACHE / "ocr" / "PP-OCRv5_server", ["CPUExecutionProvider"])

    n_boxes = 20
    fake_boxes = np.array([
        [[10, 10], [60, 10], [60, 30], [10, 30]] for _ in range(n_boxes)
    ], dtype=np.float32)
    fake_scores = [0.99] * n_boxes

    monkeypatch.setattr(op.det, "session", lambda _x: None)
    monkeypatch.setattr(op.det, "postprocess_op", lambda _preds, _hw: (fake_boxes, fake_scores))

    class _FakeRecOut:
        def __init__(self, n):
            self.txts = ["x"] * n
            self.scores = [0.99] * n

    monkeypatch.setattr(op, "rec", lambda _rec_input: _FakeRecOut(n_boxes))

    # Build the test input BEFORE patching Image.fromarray below -- this
    # uses PIL itself to construct the payload and must not be counted.
    import io
    img = Image.fromarray(_synthetic_bgr_image(120, 120))
    buf = io.BytesIO()
    img.save(buf, format="PNG")
    payload = buf.getvalue()

    calls = {"n": 0}
    real_fromarray = ocrmodel.Image.fromarray

    def counting_fromarray(*args, **kwargs):
        calls["n"] += 1
        return real_fromarray(*args, **kwargs)

    monkeypatch.setattr(ocrmodel.Image, "fromarray", counting_fromarray)

    out = op.run(payload)

    assert len(out["ocr"]["text"]) == n_boxes
    # Exactly 2 calls total, independent of n_boxes: one inside
    # `_resize_for_detection` (detection-side resize, unrelated to this
    # change) and one hoisted whole-image conversion for the crop loop
    # in `run()`. Before this branch, the crop-loop conversion happened
    # once PER LINE -- with n_boxes=20 that would be 21 calls total;
    # catching that regression is the point of this assertion.
    assert calls["n"] == 2, f"expected 2 Image.fromarray calls (resize + hoisted crop), got {calls['n']}"
