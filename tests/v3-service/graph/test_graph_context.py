"""Tests for graph-backed repair context (Phase 2, #39 pt 3) and symbol
neighborhood (Phase 3, #39 pt 4)."""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "services" / "v3-service"))

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

# helper has three direct callers, and top reaches it only through them.
WIDE = {"wide.py": (
    "def helper():\n    return 1\n\n"
    "def a():\n    return helper()\n\n"
    "def b():\n    return helper()\n\n"
    "def c():\n    return helper()\n\n"
    "def top():\n    return a() + b() + c()\n"
)}


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
        assert "Call path" in ctx
        assert "→" in ctx

    @pytest.mark.parametrize("files, function", [
        pytest.param(PROJECT, "does_not_exist", id="function-absent"),
        pytest.param({"a.md": "x"}, "load", id="no-python-files"),
    ])
    def test_empty_when_function_not_in_graph(self, files, function):
        assert graph.repair_context(files, function) == ""

    def test_leaf_function(self):
        ctx = graph.repair_context(PROJECT, "clean")
        assert "leaf" in ctx  # clean calls nothing

    @pytest.mark.parametrize("function, options, lines", [
        pytest.param("helper", {"max_items": 2}, [
            "**Call path from an entry point (how execution reaches it):**",
            "`top → a → helper`",
            "",
            "**Direct callers (3):** `a`, `b` … +1 more",
            "**Also transitively affected (1):** `top`",
            "**Calls out to:** none (`helper` is a leaf)",
        ], id="lists-over-the-cap"),
        # Exactly as many callers as the cap: all of them, and no "more".
        pytest.param("helper", {"max_items": 3}, [
            "**Call path from an entry point (how execution reaches it):**",
            "`top → a → helper`",
            "",
            "**Direct callers (3):** `a`, `b`, `c`",
            "**Also transitively affected (1):** `top`",
            "**Calls out to:** none (`helper` is a leaf)",
        ], id="a-list-at-the-cap"),
        # No call path and no transitive line, though top reaches helper.
        pytest.param("helper", {"transitive": False}, [
            "**Direct callers (3):** `a`, `b`, `c`",
            "**Calls out to:** none (`helper` is a leaf)",
        ], id="one-hop-only"),
        pytest.param("a", {}, [
            "**Call path from an entry point (how execution reaches it):**",
            "`top → a`",
            "",
            "**Direct callers (1):** `top`",
            "**Calls out to (1):** `helper`",
        ], id="a-function-that-calls-out"),
    ])
    def test_the_whole_block(self, function, options, lines):
        assert graph.repair_context(WIDE, function, **options) == "\n".join([
            f"## Call-graph context for failing function `{function}`",
            "",
            "Defined in: `wide.py`",
            "",
            *lines,
            "",
            f"Scope the fix with this: changing what `{function}` returns or raises can break everything "
            "in the affected set above; changing what it calls usually can't.",
        ])

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
        assert nb["callers"] == []
        assert nb["callees"] == []
        assert nb["impact"] == []
        assert nb["defined_in"] == []

    def test_prebuilt_graph_reused(self):
        # A prebuilt graph is used as it is: the file map is not read again
        # (here it is empty), and the answer is the same as building one.
        g = graph.build_graph(PROJECT)
        assert (graph.symbol_neighborhood({}, "load", graph=g)
                == graph.symbol_neighborhood(PROJECT, "load"))
