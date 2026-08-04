"""Bisection diagnostic for the 19 knife-edge OCR golden mismatches.

For each failing image, query the LIVE immich-ml container with
recognition.options.minScore=0 (detection box_thresh left at its
pinned 0.5 default, matching ours) to get immich's full, unfiltered
per-box text+score -- true apples-to-apples with our own pipeline run
with REC_MIN_SCORE disabled. Then:

1. Greedily IoU-pair boxes (pixel space) between the two box sets.
2. Report count match and per-pair IoU (isolates DET: do boxes agree
   on position/count at all, independent of recognition).
3. For matched pairs, print base vs our text + textScore side by side
   (isolates REC: same box, does the recognized text/confidence
   differ, and by how much).

Usage: python golden/bisect_ocr.py golden/dataset /DATA/.system_data/photos/ml-cache
"""
import argparse
import json
import sys
from pathlib import Path

import numpy as np
import requests

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import server.ocrmodel as ocrmodel
from server.ocrmodel import OcrPipeline

EP = "http://127.0.0.1:3003/predict"
OCR_MODEL = "PP-OCRv5_server"

FAILING = [
    "46fa21d787c95f3dc69340d9b098d0fa1c95e13add475684e6b4562c4e6e862b",
    "e9f5c18de4d5bb3c58895e919a1b3a1a37b837bdbc213ee44d06b1f6cc7ad78f",
    "a348b9effce95399d8b3bd8954264a46a2040b82da5917831e21dcd65ae5918a",
    "c30ec93f4bad8a9f9bcf7746d776acb8656a30db3fc9bffb01f01bbf48bc25fb",
    "d72f584cdcc199ddf37677e52be0fa51261b3a593795f279f379077907ac1cce",
    "c15d90d405625af7a2bdf9f6a7217a3bbbf5b7d8152dc8c7c27156b773459375",
    "3d9e676025ba5f223a7fb0426406e4422adb63b90dae355d8dbe72d436ba9772",
    "e7dfdcdd5aa42363ef61845467cf7da878b2853709ea4d8b12a07754ad5ea006",
    "dd32c5f76388ea4b124d18ae3d3649801e11d852359c3a7d9fd1a3e1f41cd039",
    "72dd01dcc6f99e91d5afccaa02b75b4c13c1b3265d3d3856eb41ec36ea5f956b",
    "9daef07ca96e28c93ec312d7c4bbb0771e094179f0b4a54151fce9a798174728",
    "5256cfaadbef34329a52637daebd57e616256fe2bdc7f7cc37ce5e62489a80a5",
    "42438c5963396deb7c3e4fb56f5d97657380bb1b7890c1f5effabca8df2d9b8b",
    "6bfabf8fa62d23078503518fc075aab064f25e75b0fd42a7e659cdcedaf472af",
    "e6efcf235f40568b3856479a13b38149c0f370d378704b5786fe109f4a4a993f",
    "764855589c4fab15db6fd0c4499511f694875284f15f51c955f53a89cd2cea6b",
    "b55a4ef57e11e34dd537d84bb36276078b6d8f428ea97b1c30812ce5e5b88ad1",
    "c083f1d04d1a9413a1f7ba628c95da917931177a927b8ef34398a0b563b5c308",
    "add42ccbc25d38123eef97b9d260bbebbb7d66b6b9ed051902cd3b16d2eeac6d",
]

CACHE_FILE = Path(__file__).resolve().parent / "bisect_live_cache.json"


def query_live_unfiltered(data: bytes) -> dict:
    entries = {"ocr": {"detection": {"modelName": OCR_MODEL},
                       "recognition": {"modelName": OCR_MODEL, "options": {"minScore": 0.0}}}}
    r = requests.post(EP, data={"entries": json.dumps(entries)},
                       files={"image": ("image.jpg", data, "application/octet-stream")}, timeout=300)
    r.raise_for_status()
    return r.json()["ocr"]


def boxes_px(flat_box: list, w: int, h: int) -> np.ndarray:
    arr = np.asarray(flat_box, dtype=np.float64).reshape(-1, 4, 2)
    arr[:, :, 0] *= w
    arr[:, :, 1] *= h
    return arr


def poly_iou(a: np.ndarray, b: np.ndarray) -> float:
    """Axis-aligned bbox IoU over the quad's bounding box (good enough
    for near-axis-aligned OCR line boxes)."""
    ax1, ay1 = a[:, 0].min(), a[:, 1].min()
    ax2, ay2 = a[:, 0].max(), a[:, 1].max()
    bx1, by1 = b[:, 0].min(), b[:, 1].min()
    bx2, by2 = b[:, 0].max(), b[:, 1].max()
    ix1, iy1 = max(ax1, bx1), max(ay1, by1)
    ix2, iy2 = min(ax2, bx2), min(ay2, by2)
    iw, ih = max(0.0, ix2 - ix1), max(0.0, iy2 - iy1)
    inter = iw * ih
    area_a = max(0.0, ax2 - ax1) * max(0.0, ay2 - ay1)
    area_b = max(0.0, bx2 - bx1) * max(0.0, by2 - by1)
    union = area_a + area_b - inter
    return inter / union if union > 0 else 0.0


def greedy_pair(a_boxes, b_boxes):
    pairs = []
    for i in range(len(a_boxes)):
        for j in range(len(b_boxes)):
            v = poly_iou(a_boxes[i], b_boxes[j])
            if v > 0:
                pairs.append((v, i, j))
    pairs.sort(reverse=True, key=lambda t: t[0])
    used_a, used_b, matched = set(), set(), []
    for v, i, j in pairs:
        if i in used_a or j in used_b:
            continue
        used_a.add(i)
        used_b.add(j)
        matched.append((i, j, v))
    return matched


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset")
    ap.add_argument("cache")
    ap.add_argument("--refresh", action="store_true")
    args = ap.parse_args()
    ds, mlcache = Path(args.dataset), Path(args.cache)

    live_cache = {}
    if CACHE_FILE.exists() and not args.refresh:
        live_cache = json.loads(CACHE_FILE.read_text())

    op = OcrPipeline(mlcache / "ocr" / "PP-OCRv5_server", ["CPUExecutionProvider"])
    manifest = json.loads((ds / "manifest.json").read_text())

    det_mismatches = []
    rec_only_mismatches = []

    for digest in FAILING:
        fname = manifest[digest]["file"]
        data = (ds / fname).read_bytes()

        if digest not in live_cache:
            live_cache[digest] = query_live_unfiltered(data)
            CACHE_FILE.write_text(json.dumps(live_cache))
        base = live_cache[digest]

        # our raw (unfiltered) output
        old_thresh = ocrmodel.REC_MIN_SCORE
        ocrmodel.REC_MIN_SCORE = -1.0
        full = op.run(data)
        ocrmodel.REC_MIN_SCORE = old_thresh
        ours = full["ocr"]
        w, h = full["imageWidth"], full["imageHeight"]

        base_boxes = boxes_px(base["box"], w, h) if base["text"] else np.zeros((0, 4, 2))
        our_boxes = boxes_px(ours["box"], w, h) if ours["text"] else np.zeros((0, 4, 2))

        n_base, n_our = len(base_boxes), len(our_boxes)
        matched = greedy_pair(base_boxes, our_boxes)

        print(f"\n=== {digest[:12]} ===  det: base_n={n_base} our_n={n_our} matched={len(matched)}")
        if n_base != n_our or len(matched) < max(n_base, n_our):
            det_mismatches.append(digest[:12])

        for i, j, iou in matched:
            bt, bs = base["text"][i], base["textScore"][i]
            ot, os_ = ours["text"][j], ours["textScore"][j]
            flag = "" if bt == ot else "  <-- REC DIFF"
            if iou < 0.98:
                flag += "  <-- BOX SHAPE DIFF"
            print(f"  iou={iou:.4f}  base=({bs:.5f}) {bt!r}  our=({os_:.5f}) {ot!r}{flag}")
            if bt != ot or abs(bs - os_) > 0.01:
                rec_only_mismatches.append(digest[:12])

        unmatched_base = set(range(n_base)) - {i for i, _, _ in matched}
        unmatched_our = set(range(n_our)) - {j for _, j, _ in matched}
        for i in unmatched_base:
            print(f"  UNMATCHED base-only: ({base['textScore'][i]:.5f}) {base['text'][i]!r}")
        for j in unmatched_our:
            print(f"  UNMATCHED our-only:  ({ours['textScore'][j]:.5f}) {ours['text'][j]!r}")

    print("\n\n=== SUMMARY ===")
    print(f"images with det count/pairing mismatch: {sorted(set(det_mismatches))}")
    print(f"images with rec-only (score/text) mismatch on matched boxes: {sorted(set(rec_only_mismatches))}")


if __name__ == "__main__":
    main()
