"""Regression test for ocrmodel._build_session's provider-availability
filter.

Bug: `_build_session` filtered candidate providers with
`[p for p in providers if p in available]`, where `available` is a
`set` of provider-name strings. Once resolve_providers() started handing
every pipeline a `(name, {options})` *tuple* for the OpenVINO entry (T7),
this raised `TypeError: unhashable type: 'dict'` -- a tuple containing a
dict can't be hashed for the `in available_set` membership test, so the
crash happened before OCR ever got a chance to fall back to CPU. Fixed by
comparing provider *names* only (`_provider_name`).

No ml-cache/real ONNX model needed: onnxruntime.InferenceSession is
monkeypatched to a recorder, since this is purely about what providers
list gets passed through -- not about actually running inference.
"""
from pathlib import Path

import pytest

import server.ocrmodel as ocrmodel

OV_ENTRY = ("OpenVINOExecutionProvider", {
    "device_type": "GPU", "precision": "FP32", "cache_dir": "/cache/ov-cache",
})


class _RecordingSession:
    """Stand-in for onnxruntime.InferenceSession that just records what
    it was constructed with, so the test never touches a real model file
    or a real ONNX Runtime provider."""

    def __init__(self, model_path, providers=None, **kwargs):
        self.model_path = model_path
        self.providers = providers


def test_build_session_accepts_openvino_tuple_without_crashing(monkeypatch):
    """This is the exact crash shape: a (name, dict) tuple in the
    providers list used to blow up with TypeError before ever reaching
    ort.InferenceSession. Must not raise, and must keep the OpenVINO
    entry (available) ahead of CPUExecutionProvider."""
    monkeypatch.setattr(ocrmodel.ort, "get_available_providers",
                         lambda: ["OpenVINOExecutionProvider", "CPUExecutionProvider"])
    monkeypatch.setattr(ocrmodel.ort, "InferenceSession", _RecordingSession)

    providers = [OV_ENTRY, "CPUExecutionProvider"]
    session = ocrmodel._build_session(Path("model.onnx"), providers)

    assert session.providers == [OV_ENTRY, "CPUExecutionProvider"]


def test_build_session_drops_unavailable_tuple_entry(monkeypatch):
    """The OpenVINO tuple should be filtered out (by name) just like a
    bare unavailable string would be, falling back to whatever remains
    -- here, plain CPU."""
    monkeypatch.setattr(ocrmodel.ort, "get_available_providers",
                         lambda: ["CPUExecutionProvider"])
    monkeypatch.setattr(ocrmodel.ort, "InferenceSession", _RecordingSession)

    providers = [OV_ENTRY, "CPUExecutionProvider"]
    session = ocrmodel._build_session(Path("model.onnx"), providers)

    assert session.providers == ["CPUExecutionProvider"]


def test_build_session_falls_back_to_cpu_when_nothing_available(monkeypatch):
    """If filtering leaves an empty list (e.g. every requested provider,
    tuple or not, is unavailable), _build_session must still hand ORT a
    non-empty providers list rather than an empty one."""
    monkeypatch.setattr(ocrmodel.ort, "get_available_providers",
                         lambda: ["SomeOtherExecutionProvider"])
    monkeypatch.setattr(ocrmodel.ort, "InferenceSession", _RecordingSession)

    session = ocrmodel._build_session(Path("model.onnx"), [OV_ENTRY])

    assert session.providers == ["CPUExecutionProvider"]


@pytest.mark.parametrize("entry, expected", [
    ("CPUExecutionProvider", "CPUExecutionProvider"),
    (OV_ENTRY, "OpenVINOExecutionProvider"),
])
def test_provider_name_unwraps_tuple_entries(entry, expected):
    assert ocrmodel._provider_name(entry) == expected
