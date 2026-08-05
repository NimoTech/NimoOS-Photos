"""Scratch OCR-only throughput bench against OUR server only (port 3004),
never touches immich-ml -- for perf/ocr-crop-throughput's before/after
comparison. Reuses bench.py's _run_family/load_payloads so the
methodology (1 untimed warmup, N sequential requests, same payload
round-robin) matches the original golden/bench.py steady measurement
exactly.

Usage:
  .venv/bin/python golden/bench_ocr_ours.py --dataset golden/dataset --ours http://127.0.0.1:3004 --n 60 --out /tmp/bench_before.json
"""
import argparse
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
from bench import FAMILIES, load_payloads  # noqa: E402
from bench import _run_family  # noqa: E402


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--dataset", default="golden/dataset")
    ap.add_argument("--ours", default="http://127.0.0.1:3004")
    ap.add_argument("--n", type=int, default=60)
    ap.add_argument("--concurrency", type=int, default=1)
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    images, _queries = load_payloads(Path(args.dataset))
    kind, fn = FAMILIES["ocr"]
    print(f"ocr: n={args.n} concurrency={args.concurrency} against {args.ours}", file=sys.stderr)
    result = _run_family(args.ours, "ocr", fn, images, args.n, args.concurrency)
    print(f"  p50={result['p50_ms']:.1f}ms p95={result['p95_ms']:.1f}ms "
          f"throughput={result['throughput_rps']:.2f}rps errors={result['errors']}/{result['n']}",
          file=sys.stderr)
    Path(args.out).write_text(json.dumps(result, indent=2))


if __name__ == "__main__":
    main()
