"""Tests for graph-backed repair context (Phase 2, #39 pt 3) and symbol
neighborhood (Phase 3, #39 pt 4)."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import graph
from graph.extract import available as extraction_available

pytestmark = pytest.mark.skipif(not extraction_available(),
                                reason="tree-sitter Python grammar not installed")

# main -> service.run -> load -> read; process is a sibling consumer of load.
PROJECT = {
    "app.py": (
        "from svc import load, read, process\n\n"
        "def main():\n"
        "    rows = load(read())\n"
        "    return process(rows)\n"
    ),
    "svc.py": (
        "def read():\n    return []\n\n"
        "def load(data):\n    return clean(data)\n\n"
        "def clean(data):\n    return data\n\n"
        "def process(rows):\n    return rows\n"
    ),
}


class TestRepairContext:
    def test_includes_callers_callees_and_path(self):
        ctx = graph.repair_context(PROJECT, "load")
        assert "load" in ctx
        assert "Defined in" in ctx
        # main calls load -> main is a direct caller
        assert "main" in ctx
        # load calls clean -> clean is a callee
        assert "clean" in ctx
        # path from an entry point (main) to load
        assert "Call path" in ctx and "→" in ctx

    def test_empty_when_function_absent(self):
        assert graph.repair_context(PROJECT, "does_not_exist") == ""

    def test_empty_when_no_python(self):
        assert graph.repair_context({"a.md": "x"}, "load") == ""

    def test_leaf_function(self):
        ctx = graph.repair_context(PROJECT, "clean")
        assert "leaf" in ctx  # clean calls nothing


class TestSymbolNeighborhood:
    def test_callers_callees_impact(self):
        nb = graph.symbol_neighborhood(PROJECT, "load")
        assert nb["symbol"] == "load"
        assert "svc.py" in nb["defined_in"]
        assert "main" in nb["callers"]
        assert "clean" in nb["callees"]
        # impact = transitive callers; main reaches load
        assert "main" in nb["impact"]

    def test_unknown_symbol_empty(self):
        nb = graph.symbol_neighborhood(PROJECT, "nope")
        assert nb["callers"] == [] and nb["callees"] == [] and nb["impact"] == []
        assert nb["defined_in"] == []

    def test_prebuilt_graph_reused(self):
        # Passing a prebuilt graph avoids rebuilding per symbol and gives the
        # same result as building internally.
        g = graph.build_graph(PROJECT)
        a = graph.symbol_neighborhood(PROJECT, "load", graph=g)
        b = graph.symbol_neighborhood(PROJECT, "load")
        assert a == b
