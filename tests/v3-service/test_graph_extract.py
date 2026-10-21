"""Tests for tree-sitter Python extraction into CodeGraph (issue #39, Phase 0).

Skipped cleanly when the tree-sitter grammar isn't installed; in CI it is.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

from graph.extract import available as extraction_available, extract_file, is_python

pytestmark = pytest.mark.skipif(not extraction_available(),
                                reason="tree-sitter Python grammar not installed")

# Golden parity against chiasmus's own extractGraph (src/graph/extractor.ts),
# captured via `npx tsx` on this exact fixture. Locks the extraction port to the
# reference. Exports are intentionally excluded: chiasmus emits none for Python
# (no export syntax); ATLAS synthesizes them from top-level defs for entry-point
# defaulting, so that field is an intentional divergence, not a parity target.
_GOLDEN_FIXTURE = """import os
import sys as system
from pathlib import Path
from .helpers import clean, validate as v


class Pipeline:
    def __init__(self, config):
        self.config = config

    def run(self, data):
        rows = self.load(data)
        return self.process(rows)

    def load(self, data):
        return clean(data)

    @staticmethod
    def process(rows):
        return [transform(r) for r in rows]


def transform(row):
    return validate_row(row)


def validate_row(row):
    return v(row)


def main():
    p = Pipeline({})
    return p.run(read_input())


def read_input():
    return []
"""

_GOLDEN = {
    "defines": [["Pipeline", "class", 7], ["__init__", "method", 8],
                ["load", "method", 15], ["main", "function", 31],
                ["process", "method", 19], ["read_input", "function", 36],
                ["run", "method", 11], ["transform", "function", 23],
                ["validate_row", "function", 27]],
    "calls": [["load", "clean"], ["main", "Pipeline"], ["main", "read_input"],
              ["main", "run"], ["process", "transform"], ["run", "load"],
              ["run", "process"], ["transform", "validate_row"],
              ["validate_row", "v"]],
    "imports": [["Path", "pathlib"], ["clean", ".helpers"], ["os", "os"],
                ["system", "sys"], ["v", ".helpers"]],
    "contains": [["Pipeline", "__init__"], ["Pipeline", "load"],
                 ["Pipeline", "process"], ["Pipeline", "run"]],
}


def test_matches_chiasmus_extractor():
    g = extract_file("pipe.py", _GOLDEN_FIXTURE)
    got = {
        "defines": sorted([[d.name, d.kind, d.line] for d in g.defines]),
        "calls": sorted([[c.caller, c.callee] for c in g.calls]),
        "imports": sorted([[i.name, i.source] for i in g.imports]),
        "contains": sorted([[c.parent, c.child] for c in g.contains]),
    }
    for key, expected in _GOLDEN.items():
        assert got[key] == sorted(expected), f"{key} diverged from chiasmus"


def test_signatures_captured():
    g = extract_file("pipe.py", _GOLDEN_FIXTURE)
    run = next(d for d in g.defines if d.name == "run")
    assert run.signature == "(self, data)"


def test_exports_are_top_level_defs():
    # Methods are not exports.
    g = extract_file("pipe.py", _GOLDEN_FIXTURE)
    assert sorted(e.name for e in g.exports) == [
        "Pipeline", "main", "read_input", "transform", "validate_row"]


def test_from_import_of_one_aliased_name():
    # In the golden fixture the alias is a second name; an alias that comes
    # first takes another path through the extractor.
    g = extract_file("a.py", "from .utils import helper as h\n")
    assert [(i.name, i.source) for i in g.imports] == [("h", ".utils")]


def test_decorated_function():
    g = extract_file("d.py", "@app.route('/x')\ndef handler():\n    return 1\n")
    assert any(d.name == "handler" and d.kind == "function" for d in g.defines)


def test_empty_and_nonpython():
    assert extract_file("a.py", "").defines == []
    assert is_python("x.py") and not is_python("x.md")
