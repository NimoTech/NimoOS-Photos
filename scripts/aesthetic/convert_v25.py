#!/usr/bin/env python3
"""Export the aesthetic-predictor-v2-5 MLP head into pkg/aesthetic's NAES format.

This head is trained on SigLIP v1 SO400M/patch14/384 (1152-dim input), which
is a different vector space from the SigLIP2 vectors used on this box —
this is a probe experiment, not guaranteed to work; if it doesn't pass
acceptance, fall back to training on AVA ourselves (spec stage two).

Weight source: the upstream discus0434/aesthetic-predictor-v2-5 repo ships
the trained MLP head (`AestheticPredictorV2_5Head.scoring_head`, an
nn.Sequential) as a state_dict placed directly in the GitHub repo (not on the
Hugging Face Hub — before writing this, `hf_hub_download(
"discus0434/aesthetic-predictor-v2-5", ...)` was tried and the corresponding
HF repo returned 404/401, confirming the weights simply aren't on HF; the
upstream source `src/aesthetic_predictor_v2_5/siglip_v2_5.py`'s
`convert_v2_5_from_siglip()` downloads directly from GitHub raw via
`torch.hub.load_state_dict_from_url(URL, ...)`, and this script follows that
approach). The downloaded state_dict keys were verified to be
`scoring_head.{0,2,4,6,8}.{weight,bias}` (Linear and Dropout alternate in the
nn.Sequential, hence the step of 2 between indices), dtype bfloat16, a
five-layer linear chain with dims 1152->1024->128->64->16->1, which fits
within this repo's `pkg/aesthetic` limits (maxDim=4096, maxLayers=16).

Deps: pip install torch --index-url https://download.pytorch.org/whl/cpu
Usage: python3 convert_v25.py --out ../../pkg/aesthetic/weights/head_v1.bin
"""
import argparse
import struct

import torch

VERSION = b"v25probe1"

# The upstream repo puts the trained head weights directly on GitHub (not on
# the Hugging Face Hub; the repo name `discus0434/aesthetic-predictor-v2-5`
# doesn't exist on HF, confirmed returning 404/401 during probing). The URL
# is taken from the constant in
# src/aesthetic_predictor_v2_5/siglip_v2_5.py.
WEIGHTS_URL = (
    "https://github.com/discus0434/aesthetic-predictor-v2-5/"
    "raw/main/models/aesthetic_predictor_v2_5.pth"
)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    sd = torch.hub.load_state_dict_from_url(WEIGHTS_URL, map_location="cpu")

    # Keep only the MLP head (scoring_head.N.weight/bias), discard any other
    # parameters that might be mixed in.
    pairs = []
    i = 0
    while True:
        wk, bk = f"scoring_head.{i}.weight", f"scoring_head.{i}.bias"
        if wk not in sd:
            # Fallback probe for other prefixes (e.g. no "scoring_head." or
            # plain "layers.N.*").
            cand = [k for k in sd if k.endswith(f".{i}.weight") or k == f"{i}.weight"]
            if not cand:
                break
            wk = cand[0]
            bk = wk[: -len("weight")] + "bias"
        pairs.append((sd[wk].float(), sd[bk].float()))
        i += 2  # Linear and Dropout alternate in the nn.Sequential, hence step 2.
    assert pairs, f"no linear layers found, keys={list(sd)[:10]}"
    assert pairs[0][0].shape[1] == 1152, f"input dim {pairs[0][0].shape} != 1152"
    assert pairs[-1][0].shape[0] == 1, "last layer output should be 1"

    with open(args.out, "wb") as f:
        f.write(b"NAES")
        f.write(struct.pack("<I", len(VERSION)))
        f.write(VERSION)
        f.write(struct.pack("<I", len(pairs)))
        for w, b in pairs:
            out_d, in_d = w.shape
            f.write(struct.pack("<II", in_d, out_d))
            f.write(w.contiguous().numpy().astype("<f4").tobytes())  # row-major [out][in]
            f.write(b.numpy().astype("<f4").tobytes())
    dims = "->".join(str(w.shape[1]) for w, _ in pairs) + f"->{pairs[-1][0].shape[0]}"
    print(f"OK: {len(pairs)} layers ({dims}) -> {args.out}")


if __name__ == "__main__":
    main()
