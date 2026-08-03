import io

import numpy as np
from PIL import Image

from server.clipmodel import preprocess_image


def _jpeg(w, h, color=(200, 30, 30)) -> bytes:
    buf = io.BytesIO()
    Image.new("RGB", (w, h), color).save(buf, format="JPEG")
    return buf.getvalue()


def test_squash_resize_shape():
    x = preprocess_image(_jpeg(1000, 500))
    assert x.shape == (1, 3, 384, 384)  # output is always 384x384 regardless of input aspect
    assert x.dtype == np.float32


def test_normalization_range():
    x = preprocess_image(_jpeg(64, 64, (255, 255, 255)))
    assert np.allclose(x, 1.0, atol=0.02)  # (1.0 - 0.5) / 0.5 = 1.0
    x = preprocess_image(_jpeg(64, 64, (0, 0, 0)))
    assert np.allclose(x, -1.0, atol=0.02)


def _striped_png(exif: bytes | None = None) -> bytes:
    """800x400 lossless PNG: red|white|blue vertical bands at x=[0,200)/
    [200,600)/[600,800). Resize-shorter-side-to-384 scales by 384/400=0.96
    (exact), landing the white band exactly on [192, 576) in the 768-wide
    resized image -- so a center crop of width 384 (left=192..576) captures
    pure white with no red/blue bleed (PNG is lossless, so there is no JPEG
    chroma-subsampling blur to worry about at the exact boundary). A squash
    resize would instead squeeze all three bands into the 384x384 output,
    still showing red and blue. This locks in the golden-verified behavior:
    resize-shorter-side + center-crop, not squash (see clipmodel.py module
    docstring)."""
    img = Image.new("RGB", (800, 400), (255, 255, 255))
    for x0, x1, color in ((0, 200, (255, 0, 0)), (600, 800, (0, 0, 255))):
        img.paste(Image.new("RGB", (x1 - x0, 400), color), (x0, 0))
    buf = io.BytesIO()
    img.save(buf, format="PNG", exif=exif or b"")
    return buf.getvalue()


def test_resize_then_center_crop_not_squash():
    x = preprocess_image(_striped_png())
    # normalized white == 1.0 across the interior; BICUBIC resampling causes
    # a little ringing right at the crop's outermost 2 columns (which land
    # exactly on the band boundary), so exclude those from the check.
    assert np.allclose(x[:, :, :, 2:-2], 1.0, atol=1e-4)


def test_exif_orientation_is_ignored():
    """immich-ml's decode_pil never calls ImageOps.exif_transpose, so an
    orientation=6 (rotate 90 CW) tag must not affect preprocessing -- the
    raw stored pixel grid (800x400) is used as-is. Verified against the
    golden baseline (golden/compare_clip.py): applying exif_transpose
    dropped rotated-EXIF samples' cosine to ~0.95, while ignoring it (as
    immich-ml does) reaches >= 0.999."""
    exif = Image.Exif()
    exif[274] = 6  # Orientation: rotate 90 CW
    exif_bytes = exif.tobytes()
    plain = preprocess_image(_striped_png())
    rotated_tag = preprocess_image(_striped_png(exif=exif_bytes))
    np.testing.assert_array_equal(plain, rotated_tag)
