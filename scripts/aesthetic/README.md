# scripts/aesthetic

Weight-conversion/training scripts for the aesthetic scoring head
(`pkg/aesthetic`). **Probe status**: the v2.5 probe head is trained on
SigLIP v1 (SO400M/patch14/384, 1152-dim), a different vector space from the
SigLIP2 image vectors actually used on this box — scoring quality is not
guaranteed. If acceptance (manual spot-check comparison of rankings) fails,
stage two will train our own linear head on the AVA dataset aligned to the
SigLIP2 vector space, reusing the same NAES binary format and the
`pkg/aesthetic.Load`/`LoadFrom` interface — just swap out
`pkg/aesthetic/weights/head_v1.bin`, no Go-side code changes needed.

## convert_v25.py

Exports the trained MLP scoring head from upstream
[discus0434/aesthetic-predictor-v2-5](https://github.com/discus0434/aesthetic-predictor-v2-5)
into this repo's NAES binary format (little-endian: magic "NAES" + version
string + layer count + per-layer `in/out/weights[out][in]/bias`; see the
doc comment on `LoadFrom` in `pkg/aesthetic/aesthetic.go` for details).

**Weight source note**: this project puts the trained head weights
(`AestheticPredictorV2_5Head.scoring_head`, an `nn.Sequential`) directly in
its GitHub repo, **not on the Hugging Face Hub** (the name
`discus0434/aesthetic-predictor-v2-5` doesn't exist on HF — requests return
404/401, confirmed while investigating this). The upstream source
`src/aesthetic_predictor_v2_5/siglip_v2_5.py` downloads the `.pth` directly
from GitHub raw via `torch.hub.load_state_dict_from_url`; this script follows
the same approach and doesn't need `huggingface_hub`/`safetensors`.

Structure (verified state_dict keys, dtype bfloat16, converted to fp32 on export):

```
scoring_head.0.weight/bias   1152 -> 1024
scoring_head.2.weight/bias   1024 -> 128
scoring_head.4.weight/bias    128 -> 64
scoring_head.6.weight/bias     64 -> 16
scoring_head.8.weight/bias     16 -> 1
```

(Indices 1/3/5/7 are `nn.Dropout`, identity in inference/eval mode, no
parameters, not exported.)

### Installing dependencies

```bash
cd scripts/aesthetic
python3 -m venv .venv
.venv/bin/pip install torch --index-url https://download.pytorch.org/whl/cpu
```

### Usage

```bash
mkdir -p ../../pkg/aesthetic/weights
.venv/bin/python convert_v25.py --out ../../pkg/aesthetic/weights/head_v1.bin
```

The first run caches the `.pth` to `~/.cache/torch/hub/checkpoints/`. The
output is about 5MB (fp32, ~1.32M parameters across the 5 linear layers,
`5,284,801` bytes including a 61-byte header).

## Swapping heads / stage-two self-training

To switch to a self-trained AVA head, add a `train_ava.py` (or similarly
named) script that produces a binary in the same NAES format, then replace
`pkg/aesthetic/weights/head_v1.bin` — remember to also update the comments
and filename in `embed.go` (if needed), and make sure the new head's
`Version()` string changes so it triggers the upstream logic
(`aesthetic_head_ver`) that depends on it to rescore the whole library.
