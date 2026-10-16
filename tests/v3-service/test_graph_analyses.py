"""Conformance suite for the native call-graph analyses (issue #39, Phase 0).

Includes a golden-parity test whose expected values were captured by running
chiasmus's own `src/graph/native-analyses.ts` (the code this is ported from) on
the identical graph via `npx tsx`. Divergence flags a behavioral fork.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

from graph.types import (
    CallsFact, CodeGraph, DefinesFact, ExportsFact,
)
from graph import analyses


def _graph():
    # Same graph the chiasmus golden was captured on: main->helper->leaf, an
    # orphan, and a self-recursive function.
    return CodeGraph(
        defines=[
            DefinesFact("a.py", "main", "function", 1),
            DefinesFact("a.py", "helper", "function", 2),
            DefinesFact("a.py", "leaf", "function", 3),
            DefinesFact("a.py", "orphan", "function", 4),
            DefinesFact("a.py", "recurse", "function", 5),
        ],
        calls=[
            CallsFact("main", "helper"),
            CallsFact("helper", "leaf"),
            CallsFact("recurse", "recurse"),
        ],
        exports=[ExportsFact("a.py", "main")],
    )


class TestGoldenParity:
    """Expected values captured from chiasmus native-analyses.ts via npx tsx."""

    def test_matches_chiasmus(self):
        g = _graph()
        assert analyses.callers(g, "leaf") == ["helper"]
        assert analyses.callees(g, "main") == ["helper"]
        assert analyses.reachability(g, "main", "leaf") is True
        assert analyses.reachability(g, "leaf", "main") is False
        # chiasmus formats path as the Prolog-list string "[main,helper,leaf]";
        # the node sequence is identical.
        assert analyses.path(g, "main", "leaf") == ["main", "helper", "leaf"]
        # Order matters: chiasmus emits impact in BFS discovery order.
        assert analyses.impact(g, "leaf") == ["helper", "main"]
        assert analyses.dead_code(g, ["main"]) == ["orphan"]
        assert analyses.cycles(g) == ["recurse"]


class TestDeterminism:
    """First-seen edge order must drive path tie-breaks and impact order, matching
    chiasmus (golden captured via npx tsx). Guards against Python set reordering."""

    def test_path_tiebreak_first_seen_edge(self):
        # adj[A] first-seen = [zebra, alpha]; two equal-length paths to T.
        g = CodeGraph(calls=[
            CallsFact("A", "zebra"), CallsFact("A", "alpha"),
            CallsFact("zebra", "T"), CallsFact("alpha", "T"),
        ])
        # chiasmus golden: path goes through zebra (first-seen out of A).
        assert analyses.path(g, "A", "T") == ["A", "zebra", "T"]

    def test_impact_bfs_discovery_order(self):
        # rev[T] first-seen = [B, C]; then D (via B), E (via C).
        g = CodeGraph(calls=[
            CallsFact("B", "T"), CallsFact("C", "T"),
            CallsFact("D", "B"), CallsFact("E", "C"),
        ])
        assert analyses.impact(g, "T") == ["B", "C", "D", "E"]


class TestReachability:
    def test_self_pair_needs_self_edge(self):
        g = CodeGraph(
            defines=[DefinesFact("a.py", "f", "function", 1)],
            calls=[CallsFact("f", "f")],
        )
        assert analyses.reachability(g, "f", "f") is True
        g2 = CodeGraph(
            defines=[DefinesFact("a.py", "f", "function", 1),
                     DefinesFact("a.py", "g", "function", 2)],
            calls=[CallsFact("f", "g")],
        )
        assert analyses.reachability(g2, "f", "f") is False

    def test_unknown_nodes(self):
        g = _graph()
        assert analyses.reachability(g, "nope", "leaf") is False
        assert analyses.path(g, "main", "nope") == []


class TestCycles:
    def test_multi_node_cycle(self):
        g = CodeGraph(
            defines=[DefinesFact("a.py", n, "function", i) for i, n in enumerate("abc")],
            calls=[CallsFact("a", "b"), CallsFact("b", "c"), CallsFact("c", "a")],
        )
        assert set(analyses.cycles(g)) == {"a", "b", "c"}

    def test_no_cycle(self):
        g = _graph()
        g.calls = [CallsFact("main", "helper"), CallsFact("helper", "leaf")]
        assert analyses.cycles(g) == []

    def test_methods_excluded_from_cycles(self):
        # Two methods named the same across classes must not produce a phantom
        # cycle. m -> m where m is kind=method is dropped.
        g = CodeGraph(
            defines=[DefinesFact("a.py", "m", "method", 1)],
            calls=[CallsFact("m", "m")],
        )
        assert analyses.cycles(g) == []


class TestImpactAndDead:
    def test_impact_transitive(self):
        g = _graph()
        # leaf's impact is everything that can reach it: helper, main.
        assert analyses.impact(g, "leaf") == ["helper", "main"]
        assert analyses.impact(g, "main") == []  # nothing calls main

    def test_dead_code_default_entrypoints(self):
        g = _graph()
        # Default entry points = exports (main). orphan + recurse + helper + leaf
        # are not exported; helper/leaf are called, recurse calls itself.
        dead = analyses.dead_code(g)
        assert "orphan" in dead
        assert "main" not in dead  # it's an export/entry

    def test_dead_code_excludes_methods(self):
        g = CodeGraph(defines=[DefinesFact("a.py", "m", "method", 1)], calls=[])
        assert analyses.dead_code(g) == []


class TestEntryPoints:
    def test_zero_in_degree_exports(self):
        g = _graph()
        # main (exported, zero in-degree) is an entry point.
        assert "main" in analyses.detect_entry_points(g)


class TestDispatch:
    def test_run_analysis_routes(self):
        g = _graph()
        assert analyses.run_analysis(g, "callers", target="leaf") == ["helper"]
        assert analyses.run_analysis(g, "reachability", frm="main", to="leaf") is True
        assert analyses.run_analysis(g, "cycles") == ["recurse"]

    def test_unknown_analysis_raises(self):
        import pytest
        with pytest.raises(ValueError):
            analyses.run_analysis(_graph(), "nonsense")
