# scripts/aesthetic

美学评分头(`pkg/aesthetic`)的权重转换/训练脚本目录。**探针性质**:v2.5
探针头基于 SigLIP v1(SO400M/patch14/384,1152 维)训练,与本机实际使用的
SigLIP2 图向量空间不同,不保证打分效果——如果验收(人工抽样对比排序)不
合格,阶段二会转为在 AVA 数据集上自训一个对齐 SigLIP2 向量空间的线性头,
复用同一份 NAES 二进制格式与 `pkg/aesthetic.Load`/`LoadFrom` 接口,替换
`pkg/aesthetic/weights/head_v1.bin` 即可切换,不需要改 Go 侧代码。

## convert_v25.py

把上游 [discus0434/aesthetic-predictor-v2-5](https://github.com/discus0434/aesthetic-predictor-v2-5)
训练好的 MLP 打分头导出为本仓库的 NAES 二进制格式(小端:magic "NAES" +
版本串 + 层数 + 每层 `in/out/weights[out][in]/bias`,详见
`pkg/aesthetic/aesthetic.go` 里 `LoadFrom` 的文档注释)。

**权重来源说明**:该项目把训练好的头权重(`AestheticPredictorV2_5Head.
scoring_head`,一个 `nn.Sequential`)直接放在 GitHub 仓库里,**不在
Hugging Face Hub 上**(`discus0434/aesthetic-predictor-v2-5` 这个名字在 HF
上不存在,请求返回 404/401,曾据此排查确认)。上游源码
`src/aesthetic_predictor_v2_5/siglip_v2_5.py` 用
`torch.hub.load_state_dict_from_url` 直接从 GitHub raw 下载 `.pth`,本脚本
照此实现,不需要 `huggingface_hub`/`safetensors`。

结构(实测 state_dict key,dtype 为 bfloat16,导出时转 fp32):

```
scoring_head.0.weight/bias   1152 -> 1024
scoring_head.2.weight/bias   1024 -> 128
scoring_head.4.weight/bias    128 -> 64
scoring_head.6.weight/bias     64 -> 16
scoring_head.8.weight/bias     16 -> 1
```

(索引 1/3/5/7 是 `nn.Dropout`,推理/eval 模式下为恒等,不含参数,不导出。)

### 依赖安装

```bash
cd scripts/aesthetic
python3 -m venv .venv
.venv/bin/pip install torch --index-url https://download.pytorch.org/whl/cpu
```

### 用法

```bash
mkdir -p ../../pkg/aesthetic/weights
.venv/bin/python convert_v25.py --out ../../pkg/aesthetic/weights/head_v1.bin
```

首次运行会把 `.pth` 缓存到 `~/.cache/torch/hub/checkpoints/`。产物约 5MB
(fp32,5 层线性层参数量约 132 万,`5,284,801` 字节含 61 字节头部)。

## 换头 / 阶段二自训

若切换到 AVA 自训头,新增一个 `train_ava.py`(或类似)脚本产出同样的 NAES
格式二进制,替换 `pkg/aesthetic/weights/head_v1.bin` 后记得同步改
`embed.go` 里的注释与文件名(如需要),并确保新头的 `Version()` 字符串变化,
触发依赖它做全库重打分的上层逻辑(`aesthetic_head_ver`)。
