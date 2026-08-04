#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = [
#   "huggingface_hub==1.26.0",
#   "rapidocr==3.6.0",
# ]
# ///
"""Build a fresh ml-cache-layout model tree on a packaging machine.

Produces exactly the directory layout server/main.py's ModelRegistry
factories expect (see mlserver/server/main.py's _FACTORIES subpaths):

    <out>/clip/<clip-model>/{visual,textual}/model.onnx (+ config.json,
                                                            tokenizer.json, ...)
    <out>/facial-recognition/<face-model>/{detection,recognition}/model.onnx
    <out>/ocr/<ocr-model>/{detection,recognition}/model.onnx

No .blob / .cl_cache / HF download-cache scratch files -- those are runtime
state written by an actually-running immich-ml/OpenVINO, not model weights,
and have no place in a distribution bundle.

Usage:
    uv run mlserver/golden/download_models.py --out /path/to/ml-cache
    # or, with huggingface_hub==1.26.0 + rapidocr==3.6.0 already installed:
    python3 mlserver/golden/download_models.py --out /path/to/ml-cache

Model names default to the ones pinned in common/constants.go;
script/package-photos-ml.sh passes them explicitly (parsed straight out of
that file) so the bundle and the code can never drift apart.

Optional --verify-against DIR sha256-compares every downloaded file against
an existing ml-cache tree (e.g. a live, golden-gate-validated cache) at the
same relative path -- proof the download path reproduces byte-identical
weights to whatever was actually validated, not just "a version of the
model". A mismatch means the upstream HF repo moved since the golden gates
ran; re-run with --clip-revision/--face-revision pinned to the commit that
matches, and record that revision in the packaging notes.
"""
import argparse
import hashlib
import logging
import shutil
import sys
from pathlib import Path

DEFAULT_CLIP_MODEL = "ViT-SO400M-16-SigLIP2-384__webli"
DEFAULT_FACE_MODEL = "antelopev2"
DEFAULT_OCR_MODEL = "PP-OCRv5_server"

CLIP_HF_REPO = "immich-app/ViT-SO400M-16-SigLIP2-384__webli"
FACE_HF_REPO = "immich-app/antelopev2"

# rknpu/*.rknn (RockChip NPU) and *.armnn variants are for embedded targets
# mlserver never runs on; openvino/ and .cache/ are runtime scratch state
# an actually-running server writes into the cache dir, not model weights.
CLIP_ALLOW = ["visual/*", "textual/*", "config.json"]
CLIP_IGNORE = ["*.rknn", "*.blob", "*.cl_cache", "openvino/*", ".cache/*"]
# immich-app/antelopev2's repo layout already matches the ml-cache layout
# (detection/model.onnx, recognition/model.onnx directly at the repo root),
# so no transform step is needed here -- just narrow allow_patterns to skip
# the armnn/rknpu variants living alongside the onnx ones.
FACE_ALLOW = ["detection/model.onnx", "recognition/model.onnx"]

EXPECTED_ONNX = [
    "clip/{clip}/visual/model.onnx",
    "clip/{clip}/textual/model.onnx",
    "facial-recognition/{face}/detection/model.onnx",
    "facial-recognition/{face}/recognition/model.onnx",
    "ocr/{ocr}/detection/model.onnx",
    "ocr/{ocr}/recognition/model.onnx",
]

_logger = logging.getLogger("download_models")


def _scrub_hub_leftovers(dest: Path) -> None:
    """huggingface_hub's local_dir download always leaves its own .cache/
    (etag/lock/metadata bookkeeping) inside the target dir -- confirmed
    empirically against huggingface_hub==1.26.0, not just a legacy-version
    concern. Older hub versions additionally left a models--org--repo/
    blob-store dir (that's what's still sitting in the live ml-cache on
    this box, dated from before this cache was last regenerated); scrubbed
    too, defensively, in case a future/older hub version reintroduces it."""
    cache_dir = dest / ".cache"
    if cache_dir.is_dir():
        shutil.rmtree(cache_dir, ignore_errors=True)
    for p in dest.glob("models--*"):
        shutil.rmtree(p, ignore_errors=True)


def download_clip(out: Path, model_name: str, revision: str | None) -> Path:
    from huggingface_hub import snapshot_download

    dest = out / "clip" / model_name
    _logger.info("Downloading CLIP model %s -> %s", CLIP_HF_REPO, dest)
    snapshot_download(
        CLIP_HF_REPO,
        revision=revision,
        local_dir=dest,
        allow_patterns=CLIP_ALLOW,
        ignore_patterns=CLIP_IGNORE,
    )
    _scrub_hub_leftovers(dest)
    return dest


def download_face(out: Path, model_name: str, revision: str | None) -> Path:
    from huggingface_hub import snapshot_download

    dest = out / "facial-recognition" / model_name
    _logger.info("Downloading face model %s -> %s", FACE_HF_REPO, dest)
    snapshot_download(
        FACE_HF_REPO,
        revision=revision,
        local_dir=dest,
        allow_patterns=FACE_ALLOW,
    )
    _scrub_hub_leftovers(dest)
    return dest


def download_ocr(out: Path, model_name: str) -> Path:
    """Fetch PP-OCRv5 server det/rec via rapidocr's own model-download
    machinery -- the exact same URLs (modelscope.cn) and sha256 pins immich
    itself uses, since immich's OCR pipeline is rapidocr underneath (see
    server/ocrmodel.py's module docstring)."""
    from rapidocr.inference_engine.base import FileInfo, InferSession
    from rapidocr.utils.download_file import DownloadFile, DownloadFileInput
    from rapidocr.utils.typings import EngineType, LangDet, LangRec, ModelType, OCRVersion, TaskType

    if model_name != DEFAULT_OCR_MODEL:
        raise ValueError(
            f"download_models.py's rapidocr path is hard-wired to "
            f"{DEFAULT_OCR_MODEL!r} (PP-OCRv5 server det/rec) -- got "
            f"--ocr-model={model_name!r}. Extend download_ocr() if the "
            f"pinned OCR model ever changes."
        )

    dest = out / "ocr" / model_name
    jobs = [
        ("detection", TaskType.DET, LangDet.CH),
        ("recognition", TaskType.REC, LangRec.CH),
    ]
    for subdir, task_type, lang_type in jobs:
        info = InferSession.get_model_url(FileInfo(
            engine_type=EngineType.ONNXRUNTIME,
            ocr_version=OCRVersion.PPOCRV5,
            task_type=task_type,
            lang_type=lang_type,
            model_type=ModelType.SERVER,
        ))
        save_path = dest / subdir / "model.onnx"
        _logger.info("Downloading OCR %s model -> %s", subdir, save_path)
        DownloadFile.run(DownloadFileInput(
            file_url=info["model_dir"],
            sha256=info["SHA256"],
            save_path=save_path,
            logger=_logger,
        ))
    return dest


def _sha256(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1024 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def _human(n: int) -> str:
    v = float(n)
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if v < 1024 or unit == "TB":
            return f"{v:.1f}{unit}"
        v /= 1024


def verify_expected(out: Path, names: dict[str, str]) -> bool:
    """Confirm every model.onnx the server actually loads exists, and print
    a size table. Returns False (and prints to stderr) if anything is
    missing."""
    print("\n== expected model.onnx files ==")
    ok = True
    for tmpl in EXPECTED_ONNX:
        rel = tmpl.format(clip=names["clip"], face=names["face"], ocr=names["ocr"])
        p = out / rel
        if p.is_file():
            print(f"  OK   {_human(p.stat().st_size):>8}  {rel}")
        else:
            print(f"  MISSING           {rel}", file=sys.stderr)
            ok = False
    total = sum(f.stat().st_size for f in out.rglob("*") if f.is_file())
    print(f"\nTotal downloaded tree size: {_human(total)} ({total} bytes)")
    return ok


def verify_against(out: Path, live: Path) -> bool:
    """sha256-compare every file under `out` against the same relative path
    under `live` (an existing, golden-gate-validated ml-cache). Files that
    only exist on one side (e.g. openvino/ conversion caches that only the
    live, actually-run cache has) are skipped -- this only checks that
    what WE downloaded matches what the golden gates validated, byte for
    byte."""
    print(f"\n== sha256 verification against live cache: {live} ==")
    compared = matched = 0
    mismatches: list[str] = []
    for f in sorted(out.rglob("*")):
        if not f.is_file():
            continue
        rel = f.relative_to(out)
        counterpart = live / rel
        if not counterpart.is_file():
            continue
        compared += 1
        a, b = _sha256(f), _sha256(counterpart)
        if a == b:
            matched += 1
        else:
            mismatches.append(str(rel))

    print(f"Compared {compared} overlapping files, {matched} matched, "
          f"{len(mismatches)} mismatched.")
    if mismatches:
        print("Mismatched files:", file=sys.stderr)
        for m in mismatches:
            print(f"  {m}", file=sys.stderr)
    return not mismatches


def main() -> int:
    logging.basicConfig(level=logging.INFO, format="%(message)s")
    ap = argparse.ArgumentParser(description=__doc__,
                                  formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--out", required=True, type=Path,
                     help="Output directory (becomes the ml-cache root)")
    ap.add_argument("--clip-model", default=DEFAULT_CLIP_MODEL)
    ap.add_argument("--face-model", default=DEFAULT_FACE_MODEL)
    ap.add_argument("--ocr-model", default=DEFAULT_OCR_MODEL)
    ap.add_argument("--clip-revision", default=None,
                     help="Pin the HF repo revision (commit sha) if upstream "
                          "has moved since the golden gates were validated")
    ap.add_argument("--face-revision", default=None)
    ap.add_argument("--verify-against", type=Path, default=None,
                     help="Existing ml-cache dir to sha256-compare downloads against")
    args = ap.parse_args()

    out: Path = args.out
    out.mkdir(parents=True, exist_ok=True)

    download_clip(out, args.clip_model, args.clip_revision)
    download_face(out, args.face_model, args.face_revision)
    download_ocr(out, args.ocr_model)

    names = {"clip": args.clip_model, "face": args.face_model, "ocr": args.ocr_model}
    ok = verify_expected(out, names)
    if not ok:
        print("\n✗ one or more expected model.onnx files are missing", file=sys.stderr)
        return 1

    if args.verify_against is not None:
        if not verify_against(out, args.verify_against):
            print("\n✗ sha256 mismatch against the live cache -- investigate "
                  "before packaging (see --clip-revision/--face-revision)", file=sys.stderr)
            return 1

    print("\n✓ download_models.py done:", out)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
