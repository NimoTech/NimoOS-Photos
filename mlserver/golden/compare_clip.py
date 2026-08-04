"""Compare our CLIP embeddings against the immich-ml baseline."""
import gzip
import json
import sys
from pathlib import Path

import numpy as np

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

from server.clipmodel import ClipTextual, ClipVisual
from server.providers import resolve_providers


def parse_vec(raw) -> np.ndarray:
    while isinstance(raw, str):
        raw = json.loads(raw)
    return np.asarray(raw, dtype=np.float32)


def main() -> None:
    # dataset/, ml-cache root, optional device (cpu|auto|gpu|gpu.N, default cpu
    # -- matches every prior CPU-only golden run's behavior unchanged).
    ds, cache = Path(sys.argv[1]), Path(sys.argv[2])
    device = sys.argv[3] if len(sys.argv) > 3 else "cpu"
    base = json.load(gzip.open(Path(__file__).resolve().parent / "baseline.json.gz", "rt"))
    mdir = cache / "clip" / "ViT-SO400M-16-SigLIP2-384__webli"
    providers = resolve_providers(device, cache)
    print(f"device={device} providers={providers}")
    vis = ClipVisual(mdir, providers)
    txt = ClipTextual(mdir, providers)
    print(f"visual session providers: {vis.session.get_providers()}")
    print(f"textual session providers: {txt.session.get_providers()}")
    cos_i = [float(np.dot(parse_vec(rec["clip"]["clip"]),
                          np.asarray(vis.embed_image((ds / rec["file"]).read_bytes()))))
             for rec in base["images"].values()]
    cos_t = [float(np.dot(parse_vec(r["clip"]), np.asarray(txt.embed_text(q))))
             for q, r in base["queries"].items()]
    for name, cos in (("visual", cos_i), ("textual", cos_t)):
        arr = np.asarray(cos)
        print(f"{name}: n={len(arr)} min={arr.min():.6f} mean={arr.mean():.6f}")
        worst = np.argsort(arr)[:5]
        print("  worst5 idx:", worst.tolist(), arr[worst].round(5).tolist())
    ok = min(np.min(cos_i), np.min(cos_t)) >= 0.999 and \
         (np.mean(cos_i) + np.mean(cos_t)) / 2 >= 0.9995
    print("PASS" if ok else "FAIL")
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
