"""build_steering_vector.py's main: contrast pairs in, one control vector out.

The model is not called: each prompt's mean residual is a fixed vector, so the
written direction is known in advance.
"""
import importlib.util
import json
import sys
from pathlib import Path

import gguf
import numpy as np
import pytest

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

SCRIPT = ROOT / "asa_calibration" / "build_steering_vector.py"


def _load():
    spec = importlib.util.spec_from_file_location("build_steering_vector", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _pair(label, user):
    return json.dumps({"label": label, "user": user, "assistant_prefix": ""})


def test_the_vector_is_the_structural_edit_mean_minus_the_edit_file_mean(tmp_path, monkeypatch, capsys):
    module = _load()
    means = {"s1": [3.0, 1.0], "s2": [5.0, 3.0], "e1": [1.0, 1.0], "e2": [1.0, 3.0]}
    layers = []

    def mean_residual(prompt, layer):
        layers.append(layer)
        return np.array(means[prompt], dtype=np.float32)

    monkeypatch.setattr(module, "render_prompt", lambda pair: pair["user"])
    monkeypatch.setattr(module, "extract_mean_residual", mean_residual)
    pairs = tmp_path / "pairs.jsonl"
    # A blank line between pairs is skipped.
    pairs.write_text("\n".join([_pair("structural_edit", "s1"), _pair("edit_file", "e1"), "",
                                _pair("structural_edit", "s2"), _pair("edit_file", "e2")]) + "\n")
    out = tmp_path / "new-folder" / "vector.gguf"
    monkeypatch.setattr(sys, "argv", ["build_steering_vector.py", "--pairs", str(pairs), "--out", str(out),
                                      "--layer", "3", "--model-hint", "qwen3", "--layer-count", "36"])

    assert module.main() == 0

    assert layers == [3, 3, 3, 3]
    reader = gguf.GGUFReader(str(out))
    assert [t.name for t in reader.tensors] == ["direction.3"]
    assert reader.tensors[0].data.tolist() == [3.0, 0.0]  # (4, 2) - (1, 2)
    assert reader.fields["general.architecture"].contents() == "controlvector"
    assert reader.fields["controlvector.model_hint"].contents() == "qwen3"
    assert reader.fields["controlvector.layer_count"].contents() == 36
    assert "(n=2 pairs, layer 3)" in reader.fields["general.description"].contents()
    printed = capsys.readouterr().out
    assert "loaded 4 prompts (2 contrast pairs)" in printed
    assert "[   4/4] pos=2 neg=2" in printed


def test_with_no_layer_count_the_vector_names_the_layer_after_its_own(tmp_path, monkeypatch):
    module = _load()
    monkeypatch.setattr(module, "render_prompt", lambda pair: pair["user"])
    monkeypatch.setattr(module, "extract_mean_residual",
                        lambda prompt, layer: np.array([1.0 if prompt == "s" else 0.0], dtype=np.float32))
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text(_pair("structural_edit", "s") + "\n" + _pair("edit_file", "e") + "\n")
    out = tmp_path / "vector.gguf"
    monkeypatch.setattr(sys, "argv", ["build_steering_vector.py", "--pairs", str(pairs), "--out", str(out),
                                      "--layer", "5"])

    assert module.main() == 0

    reader = gguf.GGUFReader(str(out))
    assert reader.fields["controlvector.layer_count"].contents() == 6
    assert reader.fields["controlvector.model_hint"].contents() == "unknown"


def test_limit_keeps_the_first_pairs_and_progress_shows_every_50_and_the_last(tmp_path, monkeypatch, capsys):
    module = _load()
    seen = []

    def mean_residual(prompt, layer):
        seen.append(prompt)
        return np.array([1.0], dtype=np.float32)

    monkeypatch.setattr(module, "render_prompt", lambda pair: pair["user"])
    monkeypatch.setattr(module, "extract_mean_residual", mean_residual)
    pairs = tmp_path / "pairs.jsonl"
    lines = [_pair("structural_edit" if i % 2 == 0 else "edit_file", f"p{i}") for i in range(120)]
    pairs.write_text("\n".join(lines) + "\n")
    monkeypatch.setattr(sys, "argv", ["build_steering_vector.py", "--pairs", str(pairs),
                                      "--out", str(tmp_path / "vector.gguf"), "--layer", "1", "--limit", "26"])

    assert module.main() == 0

    assert seen == [f"p{i}" for i in range(52)]
    progress = [line.split("]")[0] for line in capsys.readouterr().out.splitlines() if line.startswith("  [")]
    assert progress == ["  [  50/52", "  [  52/52"]


# Each run but the last two has a pair of both kinds besides the line that stops it.
@pytest.mark.parametrize("lines, message", [
    ([_pair("structural_edit", "s"), _pair("edit_file", "e"), _pair("tool_call", "t")],
     "[2] unknown label 'tool_call'"),
    ([_pair("structural_edit", "s"), _pair("edit_file", "e"), _pair("structural_edit", "fails")],
     "[2] extraction failed: no hidden states"),
    ([_pair("structural_edit", "s1"), _pair("structural_edit", "s2")],
     "need both structural_edit and edit_file pairs; got pos=2 neg=0"),
    ([_pair("edit_file", "e1"), _pair("edit_file", "e2")],
     "need both structural_edit and edit_file pairs; got pos=0 neg=2"),
], ids=["unknown-label", "extraction-fails", "no-edit-file-pair", "no-structural-edit-pair"])
def test_a_run_that_cannot_build_the_vector_stops_with_2_and_writes_nothing(
        lines, message, tmp_path, monkeypatch, capsys):
    module = _load()

    def mean_residual(prompt, layer):
        if prompt == "fails":
            raise RuntimeError("no hidden states")
        return np.array([1.0], dtype=np.float32)

    monkeypatch.setattr(module, "render_prompt", lambda pair: pair["user"])
    monkeypatch.setattr(module, "extract_mean_residual", mean_residual)
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text("\n".join(lines) + "\n")
    out = tmp_path / "vector.gguf"
    monkeypatch.setattr(sys, "argv", ["build_steering_vector.py", "--pairs", str(pairs), "--out", str(out),
                                      "--layer", "1"])

    assert module.main() == 2

    assert message in capsys.readouterr().err
    assert not out.exists()
