"""SigLIP2 visual/textual towers via onnxruntime.

Preprocessing matches immich-ml's actual `OpenClipVisualEncoder.transform`
(verified by extracting immich-machine-learning:v2.7.5's source), NOT the
"squash" resize_mode declared in the model's preprocess_cfg.json -- that
field is present in the config but is never read by immich-ml's code. The
real pipeline is: resize the shorter side to 384 preserving aspect ratio
(BICUBIC), then center-crop to 384x384. This was discovered empirically:
golden cosine was stuck around ~0.94-0.98 (never near 0.999) under a naive
squash-resize for every image regardless of EXIF orientation, which ruled
out EXIF as the cause; extracting immich-ml's real transforms.py confirmed
resize-then-crop is what actually generated the baseline. No EXIF
transpose is applied (immich-ml's decode_pil does not call it either).
Output embeddings are L2-normalized server-side (idempotent) -- the Go
consumer scores with sim = 1 - d^2/2 which assumes unit length.
"""
import io
import string
from pathlib import Path

import numpy as np
import onnxruntime as ort
from PIL import Image
from tokenizers import Tokenizer

SIZE = 384
CONTEXT = 64
PAD_ID = 0


def _resize_shorter_side(img: Image.Image, size: int) -> Image.Image:
    if img.width < img.height:
        return img.resize((size, int((img.height / img.width) * size)), resample=Image.BICUBIC)
    return img.resize((int((img.width / img.height) * size), size), resample=Image.BICUBIC)


def _center_crop(img: Image.Image, size: int) -> Image.Image:
    left = int((img.size[0] / 2) - (size / 2))
    upper = int((img.size[1] / 2) - (size / 2))
    return img.crop((left, upper, left + size, upper + size))


def preprocess_image(data: bytes) -> np.ndarray:
    img = Image.open(io.BytesIO(data))
    img = img.convert("RGB") if img.mode != "RGB" else img
    img = _resize_shorter_side(img, SIZE)
    img = _center_crop(img, SIZE)
    x = np.asarray(img, dtype=np.float32) / 255.0
    x = (x - 0.5) / 0.5
    return x.transpose(2, 0, 1)[None].astype(np.float32)


def _l2(v: np.ndarray) -> list[float]:
    v = v.astype(np.float32).reshape(-1)
    n = float(np.linalg.norm(v))
    return (v / n).tolist() if n > 0 else v.tolist()


class ClipVisual:
    def __init__(self, model_dir: Path, providers: list) -> None:
        self.session = ort.InferenceSession(str(model_dir / "visual" / "model.onnx"),
                                             providers=providers)
        self.input = self.session.get_inputs()[0].name

    def embed_image(self, data: bytes) -> list[float]:
        out = self.session.run(None, {self.input: preprocess_image(data)})[0][0]
        return _l2(out)


_PUNCTUATION_TRANS = str.maketrans("", "", string.punctuation)


def canonicalize(text: str) -> str:
    """Matches immich-ml's `clean_text(text, canonicalize=True)`: collapse
    whitespace first, then strip punctuation and lowercase (order verified
    against immich-machine-learning's transforms.py)."""
    text = " ".join(text.split())
    return text.translate(_PUNCTUATION_TRANS).lower()


class ClipTextual:
    def __init__(self, model_dir: Path, providers: list) -> None:
        d = model_dir / "textual"
        self.session = ort.InferenceSession(str(d / "model.onnx"), providers=providers)
        self.input = self.session.get_inputs()[0].name
        self.tokenizer = Tokenizer.from_file(str(d / "tokenizer.json"))

    def embed_text(self, text: str) -> list[float]:
        # tokenizer.json's post-processor already appends <eos> (id=1) per
        # add_eos_token=True in tokenizer_config.json -- verified via
        # tokenizer.encode("hello").ids == [17534, 1]. Truncate to CONTEXT
        # then right-pad with PAD_ID (0), matching padding_side="right".
        ids = self.tokenizer.encode(canonicalize(text)).ids[:CONTEXT]
        ids = ids + [PAD_ID] * (CONTEXT - len(ids))
        x = np.asarray([ids], dtype=np.int32)
        out = self.session.run(None, {self.input: x})[0][0]
        return _l2(out)
