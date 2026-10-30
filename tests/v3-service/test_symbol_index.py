"""symbols.symbol_index: a symbol defined once in the project's Python files comes back as a snippet of its
definition, cut to its first lines when it is long."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

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
    index = symbols.symbol_index({"app.js": "function target() {\n  return 1;\n}\n"}, ["target"])

    assert index == {"matched": [], "skipped": [{"name": "target", "reason": "not defined in scanned project files"}]}
