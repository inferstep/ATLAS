"""build_cvector_prompts.py's main: contrast pairs in, a positive and a negative prompt file out.

The model is not called: each pair renders as its user text and the server
address, so the written lines are known in advance.
"""
import importlib.util
import json
import sys
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[1]
SCRIPT = ROOT / "asa_calibration" / "build_cvector_prompts.py"


def _load():
    spec = importlib.util.spec_from_file_location("build_cvector_prompts", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def _pair(label, user):
    return json.dumps({"label": label, "user": user, "assistant_prefix": ""})


def _render(pair, llama_url):
    return f"{pair['user']}@{llama_url}"


def test_each_label_goes_to_its_own_file_in_order(tmp_path, monkeypatch, capsys):
    module = _load()
    monkeypatch.setattr(module, "render", _render)
    pairs = tmp_path / "pairs.jsonl"
    # A blank line between pairs is skipped.
    pairs.write_text("\n".join([_pair("structural_edit", "s1"), _pair("edit_file", "e1"), "",
                                _pair("structural_edit", "s2"), _pair("edit_file", "e2")]) + "\n")
    positive, negative = tmp_path / "positive.txt", tmp_path / "negative.txt"
    monkeypatch.setattr(sys, "argv", ["build_cvector_prompts.py", "--pairs", str(pairs), "--positive", str(positive),
                                      "--negative", str(negative), "--llama-url", "http://llama:1"])

    assert module.main() == 0

    assert positive.read_text() == "s1@http://llama:1\ns2@http://llama:1\n"
    assert negative.read_text() == "e1@http://llama:1\ne2@http://llama:1\n"
    printed = capsys.readouterr()
    assert f"wrote 2 positive prompts to {positive}" in printed.out
    assert f"wrote 2 negative prompts to {negative}" in printed.out
    assert printed.err == ""


def test_unequal_sides_are_written_with_a_warning(tmp_path, monkeypatch, capsys):
    module = _load()
    monkeypatch.setattr(module, "render", _render)
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text("\n".join([_pair("structural_edit", "s1"), _pair("edit_file", "e1"),
                                _pair("structural_edit", "s2")]) + "\n")
    positive, negative = tmp_path / "positive.txt", tmp_path / "negative.txt"
    monkeypatch.setattr(sys, "argv", ["build_cvector_prompts.py", "--pairs", str(pairs), "--positive", str(positive),
                                      "--negative", str(negative), "--llama-url", "http://llama:1"])

    assert module.main() == 0

    assert positive.read_text() == "s1@http://llama:1\ns2@http://llama:1\n"
    assert negative.read_text() == "e1@http://llama:1\n"
    assert "warning: 2 positive vs 1 negative" in capsys.readouterr().err


# Each run has a pair of both kinds before the line that stops it.
@pytest.mark.parametrize("bad_line, message", [
    ("{not json", "line 3: bad JSON"),
    (json.dumps({"user": "u", "assistant_prefix": ""}), "line 3: missing field 'label'"),
    (json.dumps({"label": "edit_file", "assistant_prefix": ""}), "line 3: missing field 'user'"),
    (json.dumps({"label": "edit_file", "user": "u"}), "line 3: missing field 'assistant_prefix'"),
], ids=["bad-json", "no-label", "no-user", "no-assistant-prefix"])
def test_a_line_that_is_not_a_pair_stops_the_run_and_writes_nothing(bad_line, message, tmp_path, monkeypatch, capsys):
    module = _load()
    monkeypatch.setattr(module, "render", _render)
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text("\n".join([_pair("structural_edit", "s"), _pair("edit_file", "e"), bad_line]) + "\n")
    positive, negative = tmp_path / "positive.txt", tmp_path / "negative.txt"
    monkeypatch.setattr(sys, "argv", ["build_cvector_prompts.py", "--pairs", str(pairs), "--positive", str(positive),
                                      "--negative", str(negative), "--llama-url", "http://llama:1"])

    assert module.main() == 1

    assert message in capsys.readouterr().err
    assert not positive.exists()
    assert not negative.exists()


@pytest.mark.parametrize("label, message", [
    ("structural_edit", "need both structural_edit and edit_file pairs; got 2 positive, 0 negative"),
    ("edit_file", "need both structural_edit and edit_file pairs; got 0 positive, 2 negative"),
])
def test_pairs_of_one_label_only_stop_the_run_and_write_nothing(label, message, tmp_path, monkeypatch, capsys):
    module = _load()
    monkeypatch.setattr(module, "render", _render)
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text("\n".join([_pair(label, "a"), _pair(label, "b")]) + "\n")
    positive, negative = tmp_path / "positive.txt", tmp_path / "negative.txt"
    monkeypatch.setattr(sys, "argv", ["build_cvector_prompts.py", "--pairs", str(pairs), "--positive", str(positive),
                                      "--negative", str(negative), "--llama-url", "http://llama:1"])

    assert module.main() == 1

    assert message in capsys.readouterr().err
    assert not positive.exists()
    assert not negative.exists()
