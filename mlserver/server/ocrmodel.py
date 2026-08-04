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
   not confirmed, as a *fix* for the gap. `_build_session`'s
   custom-session injection (the same "support custom session"
   extension point rapidocr's `OrtInferSession` exposes, and the one
   immich itself uses) is kept regardless: it makes the `providers`
   constructor argument a real, respected choice for whoever wires up
   device selection.

   T7 update: with real device selection wired in (`server/providers.py`),
   `resolve_providers("auto", ...)` hands this module a (name, options)
   *tuple* for the OpenVINO entry, not a bare string -- `_build_session`'s
   original availability filter (`p in available_providers_set`) raised
   `TypeError: unhashable type: 'dict'` on that tuple; fixed by comparing
   provider *names* only (`_provider_name` below). Once fixed, OCR's
   det+rec sessions DO run on `OpenVINOExecutionProvider` under
   `MLSERVER_DEVICE=auto`/`gpu*`, and the full 207-image golden re-run on
   the GPU path reproduced the exact same 197/207 (95.17%) result, same
   mismatched images, same recognized text -- i.e. still neutral, this
   time confirmed on our own GPU rather than just immich's, so OCR is NOT
   pinned to CPU: it rides whatever `providers` the registry hands every
   pipeline.

4. Bisecting the pipeline (dump detection boxes+scores in isolation,
   greedily IoU-pair them against a live immich-ml re-query, THEN
   compare recognized text/score per matched box) showed detection was
   already bit-for-bit exact -- IoU 1.0000 on every single matched box
   across every mismatching image, same box count -- so 100% of the
   golden-gate gap lived in recognition. immich's `TextRecognizer`
   wrapper (`immich_ml/models/ocr/recognition.py`) does NOT crop lines
   via rapidocr's public `get_rotate_crop_image` (cv2 `warpPerspective`,
   `BORDER_REPLICATE`, `INTER_CUBIC`); it solves its own homography
   (`_get_perspective_transform`, an SVD null-space solve) and samples
   via PIL's `Image.transform(..., PERSPECTIVE, resample=BICUBIC)`
   instead. `_crop_line` below is an independent implementation of the
   same standard homography-crop math (`cv2.getPerspectiveTransform`'s
   direct 4-point linear solve exactly agrees with an SVD null-space
   solve for an exactly-determined 4-point system) feeding the same
   PIL `PERSPECTIVE`/`BICUBIC` sampling step, rather than a copy of
   immich's AGPL code. Swapping from rapidocr's crop to this one raised
   the golden exact-match rate from 90.82% to 95.17% (188/207 ->
   197/207) on the full 207-image set -- crossing the >=95% gate.

The remaining ~5% gap (10/207 images) was individually confirmed, the
same way as before, to be the same class of benign near-0.9-cutoff
confidence noise on already-hard-to-read text/punctuation (e.g. a
single dropped bank-slogan line, "Suite 322" vs "Suite322" spacing on a
synthetic invoice) -- never a dropped, hallucinated, or word-level
misread line. A further hypothesis (rapidocr/immich's recognizer
batches crops `rec_batch_num` at a time, sorted and padded to the
batch's max aspect ratio -- if batch *membership* differed, per-crop
padding width would differ and could explain residual score drift) was
tested directly: forcing batch size 1 on the two remaining
Chinese-bank-photo mismatches changed several *other* lines' scores
and even recognized text (proving batching does have a real, measurable
effect in general), but left the two specific borderline lines
responsible for those images' mismatch completely unchanged -- so
batch composition was ruled out as the explanation for what's left.

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

import cv2
import numpy as np
import onnxruntime as ort
import rapidocr
from PIL import Image
from rapidocr.ch_ppocr_det import TextDetector
from rapidocr.ch_ppocr_rec import TextRecInput, TextRecognizer
from rapidocr.utils.parse_parameters import ParseParams

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
# Fallback only -- used when the caller passes no providers at all (e.g.
# direct instantiation outside the registry). The registry always passes
# resolve_providers(settings.device, ...), which for "auto"/"gpu"/"gpu.N"
# includes the OpenVINO EP; see module docstring point 3's T7 update for
# why that's safe (byte-identical golden result to CPU on this pipeline).
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


def _provider_name(entry) -> str:
    """A providers-list entry is either a bare name ("CPUExecutionProvider")
    or a (name, options_dict) tuple (e.g. resolve_providers' OpenVINO
    entry) -- extract just the name for availability filtering below.
    Needed because a (name, dict) tuple is unhashable, so `entry in
    available_set` raises TypeError instead of just being False."""
    return entry[0] if isinstance(entry, tuple) else entry


def _build_session(model_path: Path, providers: list) -> ort.InferenceSession:
    available = set(ort.get_available_providers())
    chosen = [p for p in providers if _provider_name(p) in available] or ["CPUExecutionProvider"]
    return ort.InferenceSession(str(model_path), providers=chosen)


def _crop_line(img: np.ndarray, box: np.ndarray) -> np.ndarray:
    """Perspective-crop one detected text line out of the full image.

    immich's own recognition wrapper does NOT use rapidocr's public
    `get_rotate_crop_image` (cv2 warpPerspective, BORDER_REPLICATE,
    INTER_CUBIC) -- it solves its own homography and samples via PIL's
    `Image.transform(..., PERSPECTIVE, resample=BICUBIC)` instead (read
    from the cached immich-ml image for behavior only; this is an
    independent implementation of the same standard homography-crop
    algorithm, using cv2.getPerspectiveTransform's direct 4-point
    linear solve rather than immich's SVD -- both solve the same exactly-
    determined system and agree up to floating point precision). Swapping
    to this crop path (identical target width/height and the same
    height/width>=1.5 -> rotate-90 rule) closed most of the golden-gate
    gap (see module docstring point 4): rapidocr's cv2-based crop was
    producing slightly different pixels than immich's PIL-based one on
    the exact same box coordinates, which is what fed slightly
    different pixels into the recognizer.
    """
    pts = box.astype(np.float32)
    crop_w = int(max(np.linalg.norm(pts[0] - pts[1]), np.linalg.norm(pts[2] - pts[3])))
    crop_h = int(max(np.linalg.norm(pts[0] - pts[3]), np.linalg.norm(pts[1] - pts[2])))
    crop_w, crop_h = max(crop_w, 1), max(crop_h, 1)
    std_rect = np.array([[0, 0], [crop_w, 0], [crop_w, crop_h], [0, crop_h]], dtype=np.float32)
    # Homography mapping the standard output rectangle -> the box's
    # quad in the original image -- exactly the output-to-input
    # direction PIL's PERSPECTIVE transform data expects.
    h_mat = cv2.getPerspectiveTransform(std_rect, pts)
    h_mat = h_mat / h_mat[2, 2]
    coeffs = tuple(h_mat[:2].reshape(-1)) + tuple(h_mat[2, :2])
    cropped = Image.fromarray(img).transform(
        (crop_w, crop_h), Image.Transform.PERSPECTIVE, data=coeffs, resample=Image.Resampling.BICUBIC
    )
    arr = np.asarray(cropped)
    if arr.shape[0] * 1.0 / arr.shape[1] >= 1.5:
        arr = np.rot90(arr)
    return np.ascontiguousarray(arr)


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
                crops = [_crop_line(img, box) for box in det_boxes]
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
