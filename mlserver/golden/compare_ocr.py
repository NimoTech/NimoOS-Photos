"""Compare our OCR pipeline against the immich-ml baseline.

Primary metric (the hard gate): for each image, take the lines with
textScore >= 0.5 on BOTH sides (this mirrors what Go's mlclient
actually consumes -- it drops lines below 0.5 and otherwise only sums
box areas and joins text by index; see pkg/mlclient), sort each side's
text list, and compare for an exact sequence match. Order is not
gated: immich's own detection sort (`TextDetector.sorted_boxes`) and
ours may legitimately differ line-for-line while agreeing on content.

Secondary/diagnostic stats (not gating): all-lines (unfiltered) exact
match rate, and a difflib.SequenceMatcher ratio for every non-exact
image so a human can spot-check whether the diff is a real miss
(dropped/garbled line) or a cosmetic split/merge/punctuation blip.

Per-image OCR results are cached to disk, one file per --device
(golden/report_ocr_cache.<device>.json, gitignored via the `report*.json`
pattern) since inference over the full 207-image dataset is slow; pass
--refresh to recompute after changing ocrmodel.py. Keying the cache by
device (not just by digest) is deliberate: a run that switches --device
without --refresh must never silently reuse -- and certify -- another
device's cached results.
"""
import argparse
import difflib
import gzip
import json
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from server.ocrmodel import OcrPipeline
from server.providers import resolve_providers

def cache_file_for(device: str) -> Path:
    """Per-device cache file -- see module docstring for why device is
    part of the cache identity, not just an out-of-band flag."""
    return Path(__file__).resolve().parent / f"report_ocr_cache.{device}.json"


GO_MIN_SCORE = 0.5  # mlclient's own textScore floor, see module docstring


def filtered_sorted_texts(texts: list, scores: list, min_score: float = GO_MIN_SCORE) -> list:
    return sorted(t for t, s in zip(texts, scores) if s >= min_score)


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("dataset")
    ap.add_argument("cache")
    ap.add_argument("--refresh", action="store_true", help="ignore per-image result cache")
    ap.add_argument("--device", default="cpu", help="cpu|auto|gpu|gpu.N (default: cpu)")
    args = ap.parse_args()

    ds, mlcache = Path(args.dataset), Path(args.cache)
    base = json.load(gzip.open(Path(__file__).resolve().parent / "baseline.json.gz", "rt"))
    cache_file = cache_file_for(args.device)

    our_cache: dict = {}
    if cache_file.exists() and not args.refresh:
        our_cache = json.loads(cache_file.read_text())

    op = None
    total = 0
    exact_filtered = 0
    exact_all = 0
    mismatches = []

    for digest, rec in base["images"].items():
        total += 1
        base_ocr = rec["ocr"]["ocr"]

        if digest not in our_cache:
            if op is None:
                providers = resolve_providers(args.device, mlcache)
                print(f"device={args.device} providers={providers}")
                op = OcrPipeline(mlcache / "ocr" / "PP-OCRv5_server", providers)
                # rapidocr's TextDetector/TextRecognizer.session is an
                # OrtInferSession wrapper; the raw ORT session (and its
                # get_providers()) is one level deeper at .session.session.
                print(f"det session providers: {op.det.session.session.get_providers()}")
                print(f"rec session providers: {op.rec.session.session.get_providers()}")
            data = (ds / rec["file"]).read_bytes()
            our_cache[digest] = op.run(data)
            if total % 20 == 0:
                cache_file.write_text(json.dumps(our_cache))

    cache_file.write_text(json.dumps(our_cache))

    for digest, rec in base["images"].items():
        base_ocr = rec["ocr"]["ocr"]
        our_ocr = our_cache[digest]["ocr"]

        # Sanity checks (do not gate, but should always hold).
        n_text, n_score, n_box, n_boxscore = (
            len(our_ocr["text"]), len(our_ocr["textScore"]),
            len(our_ocr["box"]) // 8, len(our_ocr["boxScore"]),
        )
        assert n_text == n_score == n_box == n_boxscore, (
            f"{digest[:12]}: shape mismatch text={n_text} score={n_score} "
            f"box_lines={n_box} boxscore={n_boxscore}"
        )
        assert all(0.0 <= v <= 1.0 for v in our_ocr["box"]), f"{digest[:12]}: box out of [0,1]"

        base_filtered = filtered_sorted_texts(base_ocr["text"], base_ocr["textScore"])
        our_filtered = filtered_sorted_texts(our_ocr["text"], our_ocr["textScore"])
        filtered_match = base_filtered == our_filtered
        exact_filtered += int(filtered_match)

        base_all = sorted(base_ocr["text"])
        our_all = sorted(our_ocr["text"])
        exact_all += int(base_all == our_all)

        if not filtered_match:
            ratio = difflib.SequenceMatcher(
                None, "\n".join(base_filtered), "\n".join(our_filtered)
            ).ratio()
            mismatches.append({
                "digest": digest[:12], "file": rec["file"],
                "base": base_filtered, "our": our_filtered, "ratio": ratio,
            })

    filtered_pct = 100.0 * exact_filtered / total if total else 0.0
    all_pct = 100.0 * exact_all / total if total else 0.0

    print(f"images: n={total}")
    print(f"exact match (score>=0.5, sorted) [PRIMARY]: {exact_filtered}/{total} = {filtered_pct:.2f}%")
    print(f"exact match (all lines, sorted) [secondary]: {exact_all}/{total} = {all_pct:.2f}%")

    if mismatches:
        print(f"\nnon-exact images ({len(mismatches)}):")
        for m in sorted(mismatches, key=lambda m: m["ratio"]):
            print(f"  {m['digest']} {m['file']}  diff-ratio={m['ratio']:.3f}")
            print(f"    base: {m['base']}")
            print(f"    our : {m['our']}")

    ok = filtered_pct >= 95.0
    print("\nPASS" if ok else "\nFAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
