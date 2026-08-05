"""Ad-hoc profiling harness for OCR throughput work (perf/ocr-crop-throughput).

Not part of the golden-parity gate -- just a scratch tool to confirm
where wall time goes before/after optimizing ocrmodel.py. Runs the
OcrPipeline in-process over the N text-heaviest images in the golden
dataset (ranked by baseline line count) and reports a cProfile
breakdown plus simple wall-clock rps.

Usage:
  .venv/bin/python golden/profile_ocr.py golden/dataset /DATA/.system_data/photos/ml-cache --n 30
"""
import argparse
import cProfile
import gzip
import io
import json
import pstats
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from server.ocrmodel import OcrPipeline
from server.providers import resolve_providers


def pick_images(dataset: Path, n: int) -> list:
    base = json.load(gzip.open(Path(__file__).resolve().parent / "baseline.json.gz", "rt"))
    ranked = sorted(base["images"].items(), key=lambda kv: -len(kv[1]["ocr"]["ocr"]["text"]))
    digests = [d for d, _ in ranked[:n]]
    paths = []
    for digest in digests:
        matches = list(dataset.glob(f"{digest}.*"))
        if matches:
            paths.append(matches[0])
    return paths


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset")
    ap.add_argument("mlcache")
    ap.add_argument("--n", type=int, default=30)
    ap.add_argument("--device", default="cpu")
    ap.add_argument("--warmup", type=int, default=3)
    args = ap.parse_args()

    ds, mlcache = Path(args.dataset), Path(args.mlcache)
    paths = pick_images(ds, args.n)
    print(f"selected {len(paths)} text-heavy images")

    providers = resolve_providers(args.device, mlcache)
    op = OcrPipeline(mlcache / "ocr" / "PP-OCRv5_server", providers)

    payloads = [p.read_bytes() for p in paths]

    # warmup (session/EP compile, first-call overhead)
    for data in payloads[: args.warmup]:
        op.run(data)

    pr = cProfile.Profile()
    t0 = time.perf_counter()
    pr.enable()
    for data in payloads:
        op.run(data)
    pr.disable()
    elapsed = time.perf_counter() - t0

    print(f"\n=== wall clock: {elapsed:.3f}s for {len(payloads)} images -> {len(payloads)/elapsed:.3f} rps ===\n")

    s = io.StringIO()
    ps = pstats.Stats(pr, stream=s).sort_stats("cumulative")
    ps.print_stats(25)
    print(s.getvalue())

    print("\n=== by tottime ===\n")
    s2 = io.StringIO()
    ps2 = pstats.Stats(pr, stream=s2).sort_stats("tottime")
    ps2.print_stats(20)
    print(s2.getvalue())


if __name__ == "__main__":
    main()
