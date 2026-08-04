"""Execution-provider selection: OpenVINO EP with GPU auto-detect, CPU
fallback.

Degradation semantics are copied straight from the Parser sidecar's
backendselect: only *load* failures demote to the next provider in the
list -- inference failures propagate unchanged. That's not something this
module has to implement by hand: it is exactly what ONNX Runtime's
`providers` list already does natively (each entry is tried in order at
session-construction time; a provider that fails to initialize -- e.g. no
matching GPU -- is skipped and the next one in the list is used instead).
Listing CPUExecutionProvider after the OpenVINO entry is therefore enough
to get "load failure only" fallback for free.

FP32 is always used, never FP16/INT8 -- this is a parity constraint, not a
performance choice: the golden baseline was collected against immich-ml's
own container, which also runs its OpenVINO EP at FP32.
"""
from pathlib import Path

import onnxruntime as ort

_OV_EP = "OpenVINOExecutionProvider"


def _openvino_available() -> bool:
    return _OV_EP in ort.get_available_providers()


def _device_type(device: str) -> str:
    """Map an MLSERVER_DEVICE value to an OpenVINO EP device_type string.

    auto/gpu -> "GPU" (OpenVINO's own default GPU, i.e. GPU.0 on this
    machine's dual-Intel-GPU setup -- the iGPU); gpu.N -> "GPU.N".
    """
    if device in ("auto", "gpu"):
        return "GPU"
    # device == "gpu.N"
    return "GPU." + device.split(".", 1)[1]


def resolve_providers(device: str, cache_dir: Path) -> list:
    """Return an ONNX Runtime `providers` list for `device`.

    device: "cpu" | "auto" | "gpu" | "gpu.N" (N = OpenVINO device index,
    e.g. "gpu.1" for a second GPU such as an Arc Pro B60 alongside an
    iGPU at index 0).

    `cache_dir` is the base ml-cache directory (settings.cache_dir); the
    OpenVINO EP's compiled-model cache is persisted under
    `<cache_dir>/ov-cache` so the (expensive) EP compilation happens once
    ever, not once per worker respawn like immich-ml's gunicorn workers.
    """
    if device == "cpu":
        return ["CPUExecutionProvider"]

    if not _openvino_available():
        return ["CPUExecutionProvider"]

    entry = (_OV_EP, {
        "device_type": _device_type(device),
        "precision": "FP32",
        "cache_dir": str(cache_dir / "ov-cache"),
    })
    return [entry, "CPUExecutionProvider"]
