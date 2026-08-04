from pathlib import Path

import pytest

from server import providers as providers_mod
from server.providers import resolve_providers


def test_cpu_is_cpu_only(tmp_path):
    assert resolve_providers("cpu", tmp_path) == ["CPUExecutionProvider"]


def test_auto_without_openvino_falls_back_to_cpu(tmp_path, monkeypatch):
    monkeypatch.setattr(providers_mod.ort, "get_available_providers",
                         lambda: ["AzureExecutionProvider", "CPUExecutionProvider"])
    assert resolve_providers("auto", tmp_path) == ["CPUExecutionProvider"]


def test_auto_with_openvino_prepends_gpu_entry(tmp_path, monkeypatch):
    monkeypatch.setattr(providers_mod.ort, "get_available_providers",
                         lambda: ["OpenVINOExecutionProvider", "CPUExecutionProvider"])
    result = resolve_providers("auto", tmp_path)
    assert len(result) == 2
    name, opts = result[0]
    assert name == "OpenVINOExecutionProvider"
    assert opts["device_type"] == "GPU"
    assert opts["precision"] == "FP32"
    assert opts["cache_dir"] == str(tmp_path / "ov-cache")
    assert result[1] == "CPUExecutionProvider"


def test_explicit_gpu_dot_index_maps_to_device_type(tmp_path, monkeypatch):
    monkeypatch.setattr(providers_mod.ort, "get_available_providers",
                         lambda: ["OpenVINOExecutionProvider", "CPUExecutionProvider"])
    result = resolve_providers("gpu.1", tmp_path)
    name, opts = result[0]
    assert opts["device_type"] == "GPU.1"


def test_explicit_gpu_without_index_uses_plain_gpu_device_type(tmp_path, monkeypatch):
    monkeypatch.setattr(providers_mod.ort, "get_available_providers",
                         lambda: ["OpenVINOExecutionProvider", "CPUExecutionProvider"])
    result = resolve_providers("gpu", tmp_path)
    name, opts = result[0]
    assert opts["device_type"] == "GPU"


def test_explicit_gpu_without_openvino_falls_back_to_cpu(tmp_path, monkeypatch):
    monkeypatch.setattr(providers_mod.ort, "get_available_providers",
                         lambda: ["CPUExecutionProvider"])
    assert resolve_providers("gpu.0", tmp_path) == ["CPUExecutionProvider"]
