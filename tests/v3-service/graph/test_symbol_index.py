"""symbols.symbol_index: a symbol defined once at the top level of the project's Python files comes back as a
snippet of its definition, cut to its first lines when it is long; every other symbol is skipped with its reason."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "services" / "v3-service"))

import symbols


def a_definition(lines):
    """A function of the given number of lines: its def line, then one assignment per line."""
    return ["def target():"] + [f"    x{i} = {i}" for i in range(1, lines)]


@pytest.mark.parametrize("lines, snippet, truncated", [
    pytest.param(4, a_definition(4), False, id="under the limit: the whole definition"),
    pytest.param(5, a_definition(5), False, id="at the limit: the whole definition"),
    pytest.param(8, a_definition(5) + ["# ... (3 more lines truncated)"], True,
                 id="3 lines over the limit: the first 5 and how many are left out"),
])
def test_a_long_definition_is_cut_to_its_first_lines(lines, snippet, truncated):
    source = "\n".join(a_definition(lines)) + "\n"

    index = symbols.symbol_index({"app.py": source}, ["target"], max_lines_per_snippet=5)

    assert index == {"matched": [{"name": "target", "kind": "function", "file": "app.py",
                                  "snippet": "\n".join(snippet), "n_lines": lines, "truncated": truncated}],
                     "skipped": []}


def test_only_python_files_are_read():
    index = symbols.symbol_index({"app.js": "function target() {\n  return 1;\n}\n",
                                  "notes.txt": "def target():\n    return 1\n"}, ["target"])

    assert index == {"matched": [], "skipped": [{"name": "target", "reason": "not defined in scanned project files"}]}


@pytest.mark.parametrize("source, kind", [
    pytest.param("class Target:\n    pass", "class", id="a class"),
    pytest.param("@wrap\ndef Target():\n    return 1", "function", id="a decorated function, with its decorator"),
    pytest.param("@wrap\nclass Target:\n    pass", "class", id="a decorated class, with its decorator"),
])
def test_each_kind_of_definition_comes_back_whole(source, kind):
    index = symbols.symbol_index({"app.py": source + "\n"}, ["Target"])

    assert index == {"matched": [{"name": "Target", "kind": kind, "file": "app.py",
                                  "snippet": source, "n_lines": len(source.split("\n")), "truncated": False}],
                     "skipped": []}


@pytest.mark.parametrize("source", [
    pytest.param("target = 1\ndel target\n", id="a top-level statement"),
    pytest.param("class Box:\n    def target(self):\n        return 1\n", id="a method"),
])
def test_only_top_level_definitions_are_indexed(source):
    index = symbols.symbol_index({"app.py": source}, ["target"])

    assert index == {"matched": [], "skipped": [{"name": "target", "reason": "not defined in scanned project files"}]}


def test_a_file_name_ending_in_upper_case_py_is_read():
    index = symbols.symbol_index({"APP.PY": "def target():\n    return 1\n"}, ["target"])

    assert [match["file"] for match in index["matched"]] == ["APP.PY"]


def test_a_file_that_is_not_text_is_skipped_and_the_others_are_read():
    index = symbols.symbol_index({"broken.py": None, "app.py": "def target():\n    return 1\n"}, ["target"])

    assert [match["file"] for match in index["matched"]] == ["app.py"]


def test_a_symbol_is_looked_up_once_and_the_snippets_stop_at_the_cap():
    source = "def first():\n    return 1\n\n\ndef second():\n    return 2\n"

    index = symbols.symbol_index({"app.py": source}, ["first", "first", "second"], max_snippets=1)

    assert [match["name"] for match in index["matched"]] == ["first"]
    assert index["skipped"] == [{"name": "second", "reason": "snippet cap reached"}]


def test_a_symbol_defined_in_two_files_is_skipped_as_ambiguous():
    source = "def target():\n    return 1\n"

    index = symbols.symbol_index({"a.py": source, "b.py": source}, ["target"])

    assert index == {"matched": [], "skipped": [{"name": "target", "reason": "ambiguous (2 definitions)"}]}


def test_without_tree_sitter_every_symbol_is_skipped_with_the_reason(monkeypatch):
    monkeypatch.setattr(symbols, "_STRUCTURAL_EDIT_AVAILABLE", False)

    index = symbols.symbol_index({"app.py": "def target():\n    return 1\n"}, ["target", "other"])

    assert index == {"matched": [], "skipped": [{"name": "target", "reason": "tree-sitter not installed"},
                                                {"name": "other", "reason": "tree-sitter not installed"}]}
