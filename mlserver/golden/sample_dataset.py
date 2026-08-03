"""Sample a stratified golden dataset from the live photo library.

Usage:
  python sample_dataset.py --src /path/to/watchdir [--src ...] \
      --db <DataPath>/photos.db --thumbs <DataPath>/thumbs \
      --out dataset/ --count 200

Buckets covered (see the "collect_baseline" step for how each is fed to
immich-ml):
  - regular          random sample of ordinary photos found under --src
  - exif-rotated     orientation != 1 (calibrates EXIF-aware preprocessing)
  - faces-heavy      assets with several detected faces (multi-face portraits)
  - document         assets already classified is_doc=1 (screenshots, scans,
                     receipts, forms) -- dense-OCR ground truth
  - huge             assets whose EXIF width*height is close to (but under)
                     the 170MP maxMLInputPixels downgrade threshold; sampled
                     from the already-generated thumbs/<id>/large.jpg, since
                     that's what production actually feeds to ML once an
                     original is oversized (see service/mlinput.go)
  - video-keyframe   video assets' thumbs/<id>/large.jpg keyframe JPEG

--db and --thumbs are optional: when omitted, only the regular/exif-rotated
buckets (pure filesystem walk, no DB) are produced -- useful for a quick
smoke run, but real coverage requires both.
"""
import argparse
import hashlib
import json
import random
import shutil
import sqlite3
from pathlib import Path

from PIL import Image

# These are trusted local library files (some deliberately near/over the
# 170MP ML downgrade threshold), not attacker-supplied uploads, so disable
# PIL's decompression-bomb guard rather than have orientation() crash on them.
Image.MAX_IMAGE_PIXELS = None

# Everyday scene/object/animal/action/color queries plus the exact document
# classification prompt strings from service/docscore.go (docPrompts +
# photoPrompts), included verbatim so the collected query embeddings can be
# reused as-is by the docverdict hybrid criterion comparison in later tasks.
QUERIES = [
    # scenes
    "a photo of a dog playing on the grass",
    "sunset over the ocean",
    "birthday cake with candles",
    "a group of people at dinner",
    "screenshot of a chat conversation",
    "a scanned receipt with printed text",
    "a mountain landscape covered in snow",
    "a city skyline at night",
    "a beach with palm trees",
    "a forest trail in autumn",
    "a rainy street with reflections",
    "a waterfall in a national park",
    "a desert with sand dunes",
    "a lake surrounded by mountains",
    "fireworks in the night sky",
    "a garden full of flowers",
    # people / portraits
    "a close-up portrait of a smiling person",
    "a family posing for a photo",
    "a baby laughing",
    "two friends taking a selfie",
    "a wedding couple dancing",
    "a person hiking with a backpack",
    "children playing in a park",
    "a crowd of people at a concert",
    # pets / animals
    "a cat sleeping on a couch",
    "a dog running on the beach",
    "a bird perched on a branch",
    "a horse in a field",
    "a fish swimming in an aquarium",
    # objects / food
    "a plate of pasta with tomato sauce",
    "a cup of coffee on a wooden table",
    "a red sports car parked on the street",
    "a bicycle leaning against a wall",
    "a laptop computer on a desk",
    "a bouquet of colorful flowers",
    # colors
    "a bright blue sky with white clouds",
    "a green field of grass",
    "an orange sunset",
    "a black and white photo",
    # actions
    "a person riding a bicycle",
    "someone cooking in a kitchen",
    "a basketball player jumping to shoot",
    "a person reading a book",
    "someone typing on a keyboard",
    # document classification prompts (service/docscore.go docPrompts), verbatim
    "a scan of a document",
    "a photo of a receipt",
    "a screenshot of a phone or computer screen",
    "a page of a book with text",
    "a whiteboard with handwriting",
    "an identity card or a paper form",
    # document classification prompts (service/docscore.go photoPrompts), verbatim
    "a photo of a restaurant menu",
    "a photo of a storefront with signs",
    "a poster on a wall",
    "a street scene with signs and billboards",
    "a natural photograph of people or scenery",
]

IMG_SUFFIXES = {".jpg", ".jpeg", ".png"}

# Skip filesystem-level noise that isn't real photo-library content: btrfs
# snapshot subvolumes (massively duplicate everything under them) and the
# app's own trash folder.
EXCLUDE_DIR_NAMES = {".snapshots", ".trash"}

# Near (but under) the 170MP maxMLInputPixels downgrade threshold in
# service/mlinput.go. Real camera/phone photos rarely get anywhere close to
# this, so the range is deliberately wide to catch whatever stock/test
# fixtures the library happens to have.
HUGE_MIN_PIXELS = 100_000_000
HUGE_MAX_PIXELS = 170_000_000


def orientation(p: Path) -> int:
    try:
        exif = Image.open(p).getexif()
        return int(exif.get(0x0112, 1))
    except Exception:
        return 1


def add_manifest_entry(manifest: dict, out: Path, digest: str, data: bytes,
                        dst_suffix: str, origin: str, orient: int, bucket: str) -> None:
    dst = out / f"{digest}{dst_suffix}"
    if not dst.exists():
        dst.write_bytes(data)
    manifest[digest] = {
        "file": dst.name,
        "origin": origin,
        "orientation": orient,
        "bucket": bucket,
    }


def copy_from_path(manifest: dict, out: Path, src: Path, bucket: str, orient: int = None) -> str | None:
    try:
        data = src.read_bytes()
    except OSError:
        return None
    digest = hashlib.sha256(data).hexdigest()
    if digest in manifest:
        return digest
    add_manifest_entry(manifest, out, digest, data, src.suffix.lower(),
                        str(src), orient if orient is not None else orientation(src), bucket)
    return digest


def collect_faces_heavy(db: sqlite3.Connection, manifest: dict, out: Path, n: int) -> int:
    cur = db.execute(
        "SELECT a.id, a.file_path, COUNT(*) c FROM face_detections f "
        "JOIN assets a ON a.id = f.asset_id "
        "WHERE f.excluded = 0 GROUP BY f.asset_id HAVING c >= 2 "
        "ORDER BY c DESC LIMIT ?", (n * 3,))
    added = 0
    for asset_id, file_path, _cnt in cur.fetchall():
        if added >= n or not file_path:
            continue
        if copy_from_path(manifest, out, Path(file_path), "faces-heavy") is not None:
            added += 1
    return added


def collect_documents(db: sqlite3.Connection, manifest: dict, out: Path, n: int) -> int:
    cur = db.execute(
        "SELECT a.id, a.file_path FROM asset_ocr o JOIN assets a ON a.id = o.asset_id "
        "WHERE o.is_doc = 1 ORDER BY o.line_count DESC LIMIT ?", (n * 2,))
    added = 0
    for asset_id, file_path in cur.fetchall():
        if added >= n or not file_path:
            continue
        if copy_from_path(manifest, out, Path(file_path), "document") is not None:
            added += 1
    return added


def collect_huge(db: sqlite3.Connection, thumbs: Path, manifest: dict, out: Path, n: int) -> int:
    cur = db.execute(
        "SELECT asset_id, width, height FROM asset_exif "
        "WHERE width IS NOT NULL AND width * height BETWEEN ? AND ? "
        "ORDER BY width * height DESC LIMIT ?",
        (HUGE_MIN_PIXELS, HUGE_MAX_PIXELS, n * 3))
    added = 0
    for asset_id, width, height in cur.fetchall():
        if added >= n:
            break
        large = thumbs / asset_id / "large.jpg"
        if not large.exists():
            continue
        digest = copy_from_path(manifest, out, large, "huge")
        if digest is not None:
            manifest[digest]["origin_pixels"] = width * height
            added += 1
    return added


def collect_video_keyframes(db: sqlite3.Connection, thumbs: Path, manifest: dict, out: Path, n: int) -> int:
    cur = db.execute("SELECT id, file_path FROM assets WHERE mime_type LIKE 'video%' LIMIT ?", (n * 3,))
    added = 0
    for asset_id, file_path in cur.fetchall():
        if added >= n:
            break
        large = thumbs / asset_id / "large.jpg"
        if not large.exists():
            continue
        digest = copy_from_path(manifest, out, large, "video-keyframe")
        if digest is not None:
            manifest[digest]["video_origin"] = file_path
            added += 1
    return added


def main() -> None:
    ap = argparse.ArgumentParser()
    ap.add_argument("--src", action="append", required=True)
    ap.add_argument("--db", default=None, help="path to photos.db, enables DB-aware buckets")
    ap.add_argument("--thumbs", default=None, help="path to thumbs dir (thumbDir), needed for huge/video buckets")
    ap.add_argument("--out", default="dataset")
    ap.add_argument("--count", type=int, default=200)
    ap.add_argument("--rotated", type=int, default=20, help="target count for exif-rotated bucket")
    ap.add_argument("--faces", type=int, default=15, help="target count for faces-heavy bucket")
    ap.add_argument("--docs", type=int, default=15, help="target count for document bucket")
    ap.add_argument("--huge", type=int, default=5, help="target count for huge-resolution bucket")
    ap.add_argument("--videos", type=int, default=10, help="target count for video-keyframe bucket")
    args = ap.parse_args()
    random.seed(42)

    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)
    manifest: dict = {}

    # Sorted so random.seed(42) yields a reproducible sample: rglob's traversal
    # order is filesystem-dependent (inode/dirent order), not guaranteed
    # stable across runs or machines, so the raw walk result must be sorted
    # before it is ever fed to random.sample/choice.
    files = sorted(p for s in args.src for p in Path(s).rglob("*")
                    if p.suffix.lower() in IMG_SUFFIXES and p.is_file() and p.stat().st_size > 10_000
                    and not EXCLUDE_DIR_NAMES.intersection(p.parts))
    print(f"discovered {len(files)} candidate images under {args.src}")

    # DB-aware special buckets first (deterministic, not subject to random sampling).
    db_conn = None
    thumbs = Path(args.thumbs) if args.thumbs else None
    if args.db:
        db_conn = sqlite3.connect(args.db)
        n_faces = collect_faces_heavy(db_conn, manifest, out, args.faces)
        print(f"faces-heavy: {n_faces}/{args.faces}")
        n_docs = collect_documents(db_conn, manifest, out, args.docs)
        print(f"document: {n_docs}/{args.docs}")
        if thumbs:
            n_huge = collect_huge(db_conn, thumbs, manifest, out, args.huge)
            print(f"huge: {n_huge}/{args.huge}")
            n_videos = collect_video_keyframes(db_conn, thumbs, manifest, out, args.videos)
            print(f"video-keyframe: {n_videos}/{args.videos}")
        else:
            print("no --thumbs given: skipping huge and video-keyframe buckets")
    else:
        print("no --db given: skipping faces-heavy/document/huge/video-keyframe buckets")

    # exif-rotated bucket: sample from a random subset of the filesystem walk.
    # `files` is already sorted (see above); `rotated` inherits that
    # deterministic order via the list-comprehension filter, but it is
    # sorted again explicitly here since it is itself the direct input to a
    # random.sample call.
    sample_pool = random.sample(files, min(3000, len(files)))
    rotated = sorted(p for p in sample_pool if orientation(p) != 1)
    picked_rotated = random.sample(rotated, min(args.rotated, len(rotated)))
    n_rot = 0
    for p in picked_rotated:
        if copy_from_path(manifest, out, p, "exif-rotated") is not None:
            n_rot += 1
    print(f"exif-rotated: {n_rot}/{args.rotated} (pool had {len(rotated)} candidates)")

    # regular bucket: fill up to --count with random ordinary photos.
    remaining = max(0, args.count - len(manifest))
    regular_candidates = random.sample(files, min(remaining * 4, len(files))) if remaining else []
    n_reg = 0
    for p in regular_candidates:
        if n_reg >= remaining:
            break
        if copy_from_path(manifest, out, p, "regular") is not None:
            n_reg += 1
    print(f"regular: {n_reg}/{remaining}")

    if db_conn:
        db_conn.close()

    (out / "manifest.json").write_text(json.dumps(manifest, indent=1))
    (out / "queries.json").write_text(json.dumps(QUERIES, indent=1))

    buckets: dict = {}
    for info in manifest.values():
        buckets[info["bucket"]] = buckets.get(info["bucket"], 0) + 1
    print(f"sampled {len(manifest)} images -> {out}")
    print(f"bucket breakdown: {buckets}")
    print(f"queries: {len(QUERIES)}")


if __name__ == "__main__":
    main()
