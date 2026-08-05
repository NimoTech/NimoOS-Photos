"""Dump per-image OCR output (text, textScore, box, boxScore) for every
image in the golden dataset, straight from OcrPipeline.run() -- no
score/threshold post-filtering beyond what ocrmodel.py itself applies.
Used as a byte-parity check for perf/ocr-crop-throughput: dump before
the crop-loop optimization, dump again after, diff the two files --
must be identical.

Usage:
  .venv/bin/python golden/dump_ocr_raw.py golden/dataset /DATA/.system_data/photos/ml-cache --device cpu --out /tmp/before.json
"""
import argparse
import gzip
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from server.ocrmodel import OcrPipeline
from server.providers import resolve_providers


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset")
    ap.add_argument("mlcache")
    ap.add_argument("--device", default="cpu")
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    ds, mlcache = Path(args.dataset), Path(args.mlcache)
    providers = resolve_providers(args.device, mlcache)
    print(f"device={args.device} providers={providers}")
    op = OcrPipeline(mlcache / "ocr" / "PP-OCRv5_server", providers)

    # Only the golden images (same set compare_ocr.py drives), keyed by
    # digest -> resolved via glob, exactly like compare_ocr.py -- the
    # dataset dir also has manifest.json/queries.json alongside the images.
    base = json.load(gzip.open(Path(__file__).resolve().parent / "baseline.json.gz", "rt"))
    paths = []
    for digest in sorted(base["images"]):
        matches = list(ds.glob(f"{digest}.*"))
        if matches:
            paths.append(matches[0])
    print(f"{len(paths)} images")

    results = {}
    for i, p in enumerate(paths):
        data = p.read_bytes()
        out = op.run(data)
        results[p.name] = out
        if (i + 1) % 20 == 0:
            print(f"  {i + 1}/{len(paths)}")

    Path(args.out).write_text(json.dumps(results, sort_keys=True))
    print(f"wrote {args.out}")


if __name__ == "__main__":
    main()
