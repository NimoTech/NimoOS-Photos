"""OCR via rapidocr (Apache-2.0) -- the same library immich-ml wraps.

immich-ml (v2.7.5) does NOT drive rapidocr through its high-level
`RapidOCR` convenience class. It reaches into two of the library's
lower-level Apache-2.0 components directly -- `TextDetector` (whose
`DBPostProcess` step we pin to immich's measured thresholds) and
`TextRecognizer` -- and supplies its own detection-side resize/pad
step in between, and its own ONNX Runtime session construction. Three
facts, read from the locally-cached
`ghcr.io/immich-app/immich-machine-learning:v2.7.5-openvino` image
purely to determine *behavior* (never copied -- this module is an
independent implementation against rapidocr's public API):

1. `rapidocr==3.6.0`'s own `TextDetector.get_preprocess()` ignores a
   pinned `Det.limit_side_len` whenever `limit_type != "min"`: it
   substitutes 960/1500/2000 tiered by image size instead. immich
   never hits this path because it never calls the full `RapidOCR()`
   pipeline -- it resizes itself before invoking the detector's ONNX
   session. Matching immich therefore requires bypassing
   `RapidOCR.__call__`/`preprocess_img` and doing the resize here.
2. immich's recognizer keeps a line only when `textScore > 0.9`
   (its `TextRecognizer.min_score` default, applied whenever the
   caller -- Go's mlclient -- passes no `minScore` override, which it
   never does). This is a stricter, separate filter from rapidocr's
   own `Global.text_score` (0.5) and from Go's own post-hoc >=0.5
   threshold; confirmed empirically against the T1 golden baseline
   (1322 recognized lines across the 207-image set, textScore range
   [0.90028, 0.99994] -- i.e. every baseline line already clears 0.9).
3. The `-openvino` image variant actually runs both OCR ONNX models on
   `OpenVINOExecutionProvider` (confirmed via the container's own
   startup log: "Setting execution providers to
   ['OpenVINOExecutionProvider', 'CPUExecutionProvider']"), not plain
   CPUExecutionProvider -- immich's `OrtSession` builds its own
   `onnxruntime.InferenceSession(..., providers=[...])` rather than
   going through rapidocr's provider selection (whose onnxruntime
   engine only ever knows about CUDA/DirectML/CANN -- it has no
   OpenVINO branch at all). This looked like a promising lead for the
   golden-parity gap below (OpenVINO's CPU kernels genuinely do round
   differently -- confirmed with a controlled onnxruntime-openvino
   vs. plain-onnxruntime run on random input, max abs diff ~3e-7 on the
   detection head), but swapping our own sessions to
   `OpenVINOExecutionProvider` (via `onnxruntime-openvino`, same
   `providers=[...]` list, same `nproc`/thread defaults as the
   container) reproduced the *exact* same golden mismatches, byte for
   byte, on the full 207-image set -- so it was tested and ruled out,
   not confirmed. `_build_session`'s custom-session injection (the
   same "support custom session" extension point rapidocr's
   `OrtInferSession` exposes, and the one immich itself uses) is kept
   regardless: it makes the `providers` constructor argument a real,
   respected choice for whoever wires up device selection in T7,
   instead of the dead parameter the brief's skeleton left it as.

The remaining gap after exhausting the above (measured at 90.82% exact
match on the golden set, see compare_ocr.py's output in the Task 5
report) is, as far as this investigation could pin down, residual
floating-point sensitivity in the recognition network: every single
non-exact image was individually confirmed (including by re-querying
the *live* immich-ml container with `minScore` overridden to 0 to see
its raw, unfiltered scores) to be the same underlying text, correctly
located and correctly read in substance, with a confidence score
landing a few thousandths on one side or the other of the hard 0.9
cutoff in point 2 -- never a dropped, hallucinated, or structurally
misread line.

Contract quirks required by pkg/mlclient (per T2's duck-type contract):
- box is a FLAT float array, 8 values per line, normalized to [0,1]
  by the ORIGINAL (pre-resize) image width/height.
- Decoding must NOT apply EXIF auto-rotation, matching immich-ml's
  `decode_pil` (`PIL.Image.open` + `.convert("RGB")`, never
  `ImageOps.exif_transpose`). Decoded via PIL here (rather than cv2 +
  `IMREAD_IGNORE_ORIENTATION` as in facemodel.py) to mirror immich's
  own decode call exactly; empirically the two decoders produce
  pixel-identical arrays on this stack, so this is a fidelity/
  readability choice, not something the golden gate depended on.
"""
from io import BytesIO
from pathlib import Path

import numpy as np
import onnxruntime as ort
import rapidocr
from PIL import Image
from rapidocr.ch_ppocr_det import TextDetector
from rapidocr.ch_ppocr_rec import TextRecInput, TextRecognizer
from rapidocr.utils.parse_parameters import ParseParams
from rapidocr.utils.process_img import get_rotate_crop_image

# immich TextDetector.max_resolution -- shorter side is scaled up to at
# most this many pixels, but never upscaled past the original size.
MAX_RESOLUTION = 736
# immich TextRecognizer(min_score=0.9) default, in effect since Go's
# mlclient sends no OCR options (see module docstring point 2).
REC_MIN_SCORE = 0.9
# immich's detection normalization constants (measured, not the
# "textbook" PP-OCR (px/255 - 0.5)/0.5 -- see module docstring): the
# raw 0..255 pixel value has 0.5 subtracted and is scaled by
# 1/(0.5*255), i.e. norm = (px - 0.5) / 127.5.
_NORM_MEAN = 0.5
_NORM_STD_INV = 1.0 / (0.5 * 255.0)
# Plain CPU by default -- OpenVINOExecutionProvider was tried and ruled
# out as a fix for the golden-gate gap (see module docstring point 3);
# device selection proper is T7's job. `providers` remains a real,
# respected constructor argument via _build_session below.
_PREFERRED_PROVIDERS = ["CPUExecutionProvider"]

_DEFAULT_CFG_PATH = Path(rapidocr.__file__).resolve().parent / "config.yaml"


def decode_bgr(data: bytes) -> np.ndarray:
    """Decode to BGR via PIL, matching immich-ml's decode_pil (no EXIF
    transpose -- PIL never rotates unless asked)."""
    img = Image.open(BytesIO(data))
    img.load()
    if img.mode != "RGB":
        img = img.convert("RGB")
    return np.asarray(img)[:, :, ::-1]


def _resize_for_detection(img: np.ndarray) -> np.ndarray:
    """Reproduce immich TextDetector._transform's resize: scale the
    shorter side up to MAX_RESOLUTION (never upscaling), round both
    dimensions to the nearest multiple of 32, LANCZOS-resample."""
    h, w = img.shape[:2]
    ratio = min(MAX_RESOLUTION / min(h, w), 1.0)
    resize_h = max(32, int(round(int(h * ratio) / 32) * 32))
    resize_w = max(32, int(round(int(w * ratio) / 32) * 32))
    resized = Image.fromarray(img).resize((resize_w, resize_h), Image.Resampling.LANCZOS)
    return np.asarray(resized)


def _det_input_tensor(img: np.ndarray) -> np.ndarray:
    x = img.astype(np.float32)
    x -= _NORM_MEAN
    x *= _NORM_STD_INV
    x = np.transpose(x, (2, 0, 1))
    return np.expand_dims(x, axis=0)


def _build_session(model_path: Path, providers: list) -> ort.InferenceSession:
    available = set(ort.get_available_providers())
    chosen = [p for p in providers if p in available] or ["CPUExecutionProvider"]
    return ort.InferenceSession(str(model_path), providers=chosen)


class OcrPipeline:
    def __init__(self, model_dir: Path, providers: list) -> None:
        # `providers` selects the ONNX Runtime execution provider list
        # (see module docstring point 3 re: why OpenVINO isn't the
        # default); broader device selection is T7's job.
        providers = providers or _PREFERRED_PROVIDERS
        cfg = ParseParams.load(_DEFAULT_CFG_PATH)
        cfg = ParseParams.update_batch(cfg, {
            "Det.thresh": 0.3,
            "Det.box_thresh": 0.5,
            "Det.unclip_ratio": 1.6,
            "Det.use_dilation": True,
            "Det.score_mode": "fast",
        })
        # Build Det/Rec directly (skip RapidOCR()'s own __init__, which
        # unconditionally constructs a TextClassifier too -- and would
        # try to download a default cls model over the network even
        # though immich's OCR pipeline never uses one).
        cfg.Det.engine_cfg = cfg.EngineConfig[cfg.Det.engine_type.value]
        cfg.Rec.engine_cfg = cfg.EngineConfig[cfg.Rec.engine_type.value]
        cfg.Rec.font_path = cfg.Global.font_path
        # Inject our own pre-built ONNX Runtime sessions -- rapidocr's
        # OrtInferSession explicitly supports this ("support custom
        # session (PR #451)"), which is how we route around rapidocr's
        # onnxruntime engine never offering an OpenVINO branch.
        # omegaconf's DictConfig rejects arbitrary object values by
        # default, so allow_objects has to be opted into on these nodes.
        cfg.Det._set_flag("allow_objects", True)
        cfg.Rec._set_flag("allow_objects", True)
        cfg.Det.session = _build_session(model_dir / "detection" / "model.onnx", providers)
        cfg.Rec.session = _build_session(model_dir / "recognition" / "model.onnx", providers)
        self.det = TextDetector(cfg.Det)
        self.rec = TextRecognizer(cfg.Rec)

    def run(self, data: bytes) -> dict:
        img = decode_bgr(data)
        h, w = img.shape[:2]
        texts, tscores, boxes, bscores = [], [], [], []

        if h >= 32 and w >= 32:
            resized = _resize_for_detection(img)
            preds = self.det.session(_det_input_tensor(resized))
            det_boxes, det_scores = self.det.postprocess_op(preds, (h, w))

            if len(det_boxes):
                crops = [get_rotate_crop_image(img, box.astype(np.float32)) for box in det_boxes]
                rec_out = self.rec(TextRecInput(img=crops))
                for box, box_score, text, text_score in zip(det_boxes, det_scores, rec_out.txts, rec_out.scores):
                    if text_score <= REC_MIN_SCORE:
                        continue
                    texts.append(text)
                    tscores.append(float(text_score))
                    bscores.append(float(box_score))
                    for px, py in box.astype(np.float64):
                        boxes.extend([float(px) / w, float(py) / h])

        return {"ocr": {"text": texts, "textScore": tscores,
                        "box": boxes, "boxScore": bscores},
                "imageWidth": w, "imageHeight": h}
