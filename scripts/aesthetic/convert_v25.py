#!/usr/bin/env python3
"""把 aesthetic-predictor-v2-5 的 MLP 头导出为 pkg/aesthetic 的 NAES 格式。

该头基于 SigLIP v1 SO400M/patch14/384(输入 1152 维),与本机 SigLIP2 向量
空间不同——这是探针实验,不保证效果,验收不合格转 AVA 自训(spec 阶段二)。

权重来源:上游 discus0434/aesthetic-predictor-v2-5 仓库把训练好的 MLP 头
(`AestheticPredictorV2_5Head.scoring_head`,一个 nn.Sequential)以 state_dict
形式直接放在 GitHub 仓库里(而非 Hugging Face Hub——运行前曾尝试
`hf_hub_download("discus0434/aesthetic-predictor-v2-5", ...)`,对应的 HF repo
返回 404/401,说明权重根本不在 HF 上;上游源码 `src/aesthetic_predictor_v2_5/
siglip_v2_5.py` 里 `convert_v2_5_from_siglip()` 用
`torch.hub.load_state_dict_from_url(URL, ...)` 直接从 GitHub raw 下载,本脚本
照此实现)。实测下载到的 state_dict key 为 `scoring_head.{0,2,4,6,8}.
{weight,bias}`(nn.Sequential 里 Linear 与 Dropout 交替,故索引隔 2),
dtype 为 bfloat16,五层线性变换维度链 1152→1024→128→64→16→1,与本仓库
`pkg/aesthetic` 的上限(maxDim=4096、maxLayers=16)相容。

依赖: pip install torch --index-url https://download.pytorch.org/whl/cpu
用法: python3 convert_v25.py --out ../../pkg/aesthetic/weights/head_v1.bin
"""
import argparse
import struct

import torch

VERSION = b"v25probe1"

# 上游仓库把训练好的头权重直接放在 GitHub(不在 Hugging Face Hub 上;
# `discus0434/aesthetic-predictor-v2-5` 这个仓库名在 HF 上不存在,探测阶段
# 已确认返回 404/401)。地址取自
# src/aesthetic_predictor_v2_5/siglip_v2_5.py 里的 URL 常量。
WEIGHTS_URL = (
    "https://github.com/discus0434/aesthetic-predictor-v2-5/"
    "raw/main/models/aesthetic_predictor_v2_5.pth"
)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--out", required=True)
    args = ap.parse_args()

    sd = torch.hub.load_state_dict_from_url(WEIGHTS_URL, map_location="cpu")

    # 只保留 MLP 头(scoring_head.N.weight/bias),丢弃其它可能混入的参数。
    pairs = []
    i = 0
    while True:
        wk, bk = f"scoring_head.{i}.weight", f"scoring_head.{i}.bias"
        if wk not in sd:
            # 兼容其它前缀(如无 "scoring_head." 或直接 "layers.N.*")的探测 fallback。
            cand = [k for k in sd if k.endswith(f".{i}.weight") or k == f"{i}.weight"]
            if not cand:
                break
            wk = cand[0]
            bk = wk[: -len("weight")] + "bias"
        pairs.append((sd[wk].float(), sd[bk].float()))
        i += 2  # nn.Sequential 里 Linear 与 Dropout 交替,索引隔 2。
    assert pairs, f"未找到线性层,keys={list(sd)[:10]}"
    assert pairs[0][0].shape[1] == 1152, f"输入维度 {pairs[0][0].shape} != 1152"
    assert pairs[-1][0].shape[0] == 1, "最后一层输出应为 1"

    with open(args.out, "wb") as f:
        f.write(b"NAES")
        f.write(struct.pack("<I", len(VERSION)))
        f.write(VERSION)
        f.write(struct.pack("<I", len(pairs)))
        for w, b in pairs:
            out_d, in_d = w.shape
            f.write(struct.pack("<II", in_d, out_d))
            f.write(w.contiguous().numpy().astype("<f4").tobytes())  # 行主序 [out][in]
            f.write(b.numpy().astype("<f4").tobytes())
    dims = "->".join(str(w.shape[1]) for w, _ in pairs) + f"->{pairs[-1][0].shape[0]}"
    print(f"OK: {len(pairs)} layers ({dims}) -> {args.out}")


if __name__ == "__main__":
    main()
