"""Compare our face detection+recognition pipeline against the immich-ml
baseline: face count parity, greedy-IoU-paired bounding box overlap, and
paired embedding cosine similarity.

Per-image detection results are cached to disk (golden/report_faces_cache.json,
gitignored via the `report*.json` pattern) since CPU detection over the full
207-image dataset is slow (~tens of minutes); pass --refresh to recompute
after changing facemodel.py.
"""
import argparse
import gzip
import json
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from server.facemodel import FacePipeline
from server.providers import resolve_providers

CACHE_FILE = Path(__file__).resolve().parent / "report_faces_cache.json"


def parse_vec(raw) -> np.ndarray:
    while isinstance(raw, str):
        raw = json.loads(raw)
    return np.asarray(raw, dtype=np.float32)


def l2(v: np.ndarray) -> np.ndarray:
    n = np.linalg.norm(v)
    return v / n if n > 0 else v


def iou(a: dict, b: dict) -> float:
    ax1, ay1, ax2, ay2 = a["x1"], a["y1"], a["x2"], a["y2"]
    bx1, by1, bx2, by2 = b["x1"], b["y1"], b["x2"], b["y2"]
    ix1, iy1 = max(ax1, bx1), max(ay1, by1)
    ix2, iy2 = min(ax2, bx2), min(ay2, by2)
    iw, ih = max(0.0, ix2 - ix1), max(0.0, iy2 - iy1)
    inter = iw * ih
    area_a = max(0.0, ax2 - ax1) * max(0.0, ay2 - ay1)
    area_b = max(0.0, bx2 - bx1) * max(0.0, by2 - by1)
    union = area_a + area_b - inter
    return inter / union if union > 0 else 0.0


def greedy_pair(base_faces: list, our_faces: list) -> list[tuple[int, int, float]]:
    """Returns list of (base_idx, our_idx, iou) greedily matched by descending IoU."""
    pairs = []
    for i, bf in enumerate(base_faces):
        for j, of in enumerate(our_faces):
            v = iou(bf["boundingBox"], of["boundingBox"])
            if v > 0:
                pairs.append((v, i, j))
    pairs.sort(reverse=True, key=lambda t: t[0])
    used_b, used_o = set(), set()
    matched = []
    for v, i, j in pairs:
        if i in used_b or j in used_o:
            continue
        used_b.add(i)
        used_o.add(j)
        matched.append((i, j, v))
    return matched


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset")
    ap.add_argument("cache")
    ap.add_argument("--refresh", action="store_true", help="ignore per-image result cache")
    ap.add_argument("--device", default="cpu", help="cpu|auto|gpu|gpu.N (default: cpu)")
    args = ap.parse_args()

    ds, mlcache = Path(args.dataset), Path(args.cache)
    base = json.load(gzip.open(Path(__file__).resolve().parent / "baseline.json.gz", "rt"))

    our_cache: dict = {}
    if CACHE_FILE.exists() and not args.refresh:
        our_cache = json.loads(CACHE_FILE.read_text())

    fp = None
    total = 0
    count_matches = 0
    all_iou = []
    all_cos = []
    per_image_report = []

    for digest, rec in base["images"].items():
        total += 1
        base_faces = rec["faces"]["facial-recognition"]

        if digest not in our_cache:
            if fp is None:
                mdir = mlcache / "facial-recognition" / "antelopev2"
                providers = resolve_providers(args.device, mlcache)
                print(f"device={args.device} providers={providers}")
                fp = FacePipeline(mdir, providers)
                print(f"det session providers: {fp.det.session.get_providers()}")
                print(f"rec session providers: {fp.rec.session.get_providers()}")
            data = (ds / rec["file"]).read_bytes()
            our_cache[digest] = fp.detect(data)
            # periodically flush so a crash mid-run doesn't lose progress
            if total % 10 == 0:
                CACHE_FILE.write_text(json.dumps(our_cache))

        our_faces = our_cache[digest]["facial-recognition"]

        n_base, n_our = len(base_faces), len(our_faces)
        count_match = n_base == n_our
        count_matches += int(count_match)

        matched = greedy_pair(base_faces, our_faces)
        for i, j, v in matched:
            all_iou.append(v)
            cb = l2(parse_vec(base_faces[i]["embedding"]))
            co = l2(parse_vec(our_faces[j]["embedding"]))
            all_cos.append(float(np.dot(cb, co)))

        per_image_report.append({
            "digest": digest[:12], "file": rec["file"],
            "n_base": n_base, "n_our": n_our, "count_match": count_match,
            "n_matched": len(matched),
        })

    CACHE_FILE.write_text(json.dumps(our_cache))

    count_pct = 100.0 * count_matches / total if total else 0.0
    iou_arr = np.asarray(all_iou) if all_iou else np.asarray([0.0])
    cos_arr = np.asarray(all_cos) if all_cos else np.asarray([0.0])

    print(f"images: n={total}")
    print(f"count-match: {count_matches}/{total} = {count_pct:.2f}%")
    print(f"IoU (n={len(all_iou)}): mean={iou_arr.mean():.6f} min={iou_arr.min():.6f}")
    print(f"cosine (n={len(all_cos)}): mean={cos_arr.mean():.6f} min={cos_arr.min():.6f}")

    mismatches = [r for r in per_image_report if not r["count_match"]]
    if mismatches:
        print(f"count mismatches ({len(mismatches)}):")
        for r in mismatches[:20]:
            print(f"  {r['digest']} {r['file']}: base={r['n_base']} our={r['n_our']}")

    worst_iou_idx = np.argsort(iou_arr)[:5] if len(all_iou) else []
    if len(worst_iou_idx):
        print("  worst5 IoU:", iou_arr[worst_iou_idx].round(4).tolist())
    worst_cos_idx = np.argsort(cos_arr)[:5] if len(all_cos) else []
    if len(worst_cos_idx):
        print("  worst5 cosine:", cos_arr[worst_cos_idx].round(6).tolist())

    ok = (count_pct >= 98.0 and iou_arr.mean() >= 0.9 and
          (cos_arr.min() >= 0.999 if len(all_cos) else False))
    print("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
