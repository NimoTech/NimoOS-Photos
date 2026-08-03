"""Collect ground-truth responses from the currently running immich-ml.

Payloads mirror pkg/mlclient/mlclient.go byte-for-byte (entries JSON, field
names, filename image.jpg). Raw JSON responses are stored untouched so later
tasks can replay the exact comparison immich-ml would have produced.

Usage:
  python collect_baseline.py dataset/ [--out baseline.json.gz]

The first /predict call per model incurs a cold model load on the immich-ml
side (can take 1-3 minutes); every request uses a generous 300s timeout to
absorb this without a false failure.
"""
import argparse
import gzip
import json
import math
import sys
import time
from pathlib import Path

import requests

EP = "http://127.0.0.1:3003/predict"
CLIP = "ViT-SO400M-16-SigLIP2-384__webli"
FACE_MODEL = "antelopev2"
OCR_MODEL = "PP-OCRv5_server"
TIMEOUT = 300


def predict_image(entries: dict, data: bytes) -> dict:
    r = requests.post(EP, data={"entries": json.dumps(entries)},
                       files={"image": ("image.jpg", data, "application/octet-stream")},
                       timeout=TIMEOUT)
    r.raise_for_status()
    return r.json()


def predict_text(text: str) -> dict:
    r = requests.post(EP, data={"entries": json.dumps(
        {"clip": {"textual": {"modelName": CLIP}}}), "text": text}, timeout=TIMEOUT)
    r.raise_for_status()
    return r.json()


def parse_clip_vec(resp: dict) -> list:
    """Mirror mlclient.go's parseClipField: "clip" may be a JSON array or a
    JSON-encoded string containing an array."""
    raw = resp.get("clip")
    if isinstance(raw, list):
        return raw
    if isinstance(raw, str):
        return json.loads(raw)
    raise ValueError(f"unexpected clip field type: {type(raw)}")


def sanity_check(out: dict, manifest: dict) -> list:
    """Step 5 spot checks. Returns a list of problem strings (empty = clean)."""
    problems = []
    doc_buckets = {"document"}
    face_buckets = {"faces-heavy"}

    for digest, entry in out["images"].items():
        info = manifest.get(digest, {})
        bucket = info.get("bucket", "?")

        # clip: length 1152, L2 norm ~= 1.0
        try:
            vec = parse_clip_vec(entry["clip"])
            n = len(vec)
            if n != 1152:
                problems.append(f"{digest[:12]} ({bucket}): clip len {n} != 1152")
            norm = math.sqrt(sum(v * v for v in vec))
            if abs(norm - 1.0) > 0.01:
                problems.append(f"{digest[:12]} ({bucket}): clip L2 norm {norm:.4f} not ~1.0")
        except Exception as e:
            problems.append(f"{digest[:12]} ({bucket}): clip parse failed: {e}")

        # faces: for faces-heavy bucket, expect >=1 face with a 512-dim embedding
        if bucket in face_buckets:
            faces = entry.get("faces", {}).get("facial-recognition", [])
            if not faces:
                problems.append(f"{digest[:12]} ({bucket}): no faces detected")
            else:
                for i, f in enumerate(faces):
                    emb_raw = f.get("embedding")
                    try:
                        emb = json.loads(emb_raw) if isinstance(emb_raw, str) else emb_raw
                        if len(emb) != 512:
                            problems.append(
                                f"{digest[:12]} ({bucket}): face[{i}] embedding len {len(emb)} != 512")
                    except Exception as e:
                        problems.append(f"{digest[:12]} ({bucket}): face[{i}] embedding parse failed: {e}")

        # ocr: for document bucket, expect non-empty text
        if bucket in doc_buckets:
            ocr = entry.get("ocr", {}).get("ocr", {})
            texts = [t for t in ocr.get("text", []) if t and t.strip()]
            if not texts:
                problems.append(f"{digest[:12]} ({bucket}): ocr.text empty")

    return problems


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset", nargs="?", default="dataset")
    ap.add_argument("--out", default="baseline.json.gz")
    args = ap.parse_args()

    ds = Path(args.dataset)
    manifest = json.loads((ds / "manifest.json").read_text())
    queries = json.loads((ds / "queries.json").read_text())
    out = {"images": {}, "queries": {}, "meta": {"endpoint": EP,
           "collected_at": time.strftime("%Y-%m-%dT%H:%M:%S")}}

    errors = []
    for digest, info in manifest.items():
        data = (ds / info["file"]).read_bytes()
        try:
            out["images"][digest] = {
                "file": info["file"],
                "clip": predict_image({"clip": {"visual": {"modelName": CLIP}}}, data),
                "faces": predict_image({"facial-recognition": {
                    "detection": {"modelName": FACE_MODEL},
                    "recognition": {"modelName": FACE_MODEL}}}, data),
                "ocr": predict_image({"ocr": {
                    "detection": {"modelName": OCR_MODEL},
                    "recognition": {"modelName": OCR_MODEL}}}, data),
            }
        except Exception as e:
            errors.append(f"{digest} ({info.get('file')}): {e}")
            print(f"  ERROR: {digest[:12]} {info.get('file')}: {e}", file=sys.stderr)
            continue
        print(f"[{len(out['images'])}/{len(manifest)}] {info['file']} ({info.get('bucket', '?')})")

    for i, q in enumerate(queries, 1):
        try:
            out["queries"][q] = predict_text(q)
        except Exception as e:
            errors.append(f"query {q!r}: {e}")
            print(f"  ERROR: query {q!r}: {e}", file=sys.stderr)
            continue
        print(f"[query {i}/{len(queries)}] {q}")

    out["meta"]["image_count"] = len(out["images"])
    out["meta"]["query_count"] = len(out["queries"])
    out["meta"]["errors"] = errors

    with gzip.open(args.out, "wt") as f:
        json.dump(out, f)
    print(f"{args.out} written: {len(out['images'])} images, {len(out['queries'])} queries, "
          f"{len(errors)} errors")

    print("running sanity checks (Step 5)...")
    problems = sanity_check(out, manifest)
    if problems:
        print(f"SANITY CHECK FAILURES ({len(problems)}):", file=sys.stderr)
        for p in problems:
            print(f"  - {p}", file=sys.stderr)
        sys.exit(1)
    print("sanity checks passed: clip vectors are 1152-dim/L2~=1.0, "
          "faces-heavy images have parseable 512-dim embeddings, "
          "document images have non-empty ocr text.")


if __name__ == "__main__":
    main()
