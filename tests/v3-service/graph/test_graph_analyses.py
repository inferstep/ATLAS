"""Conformance suite for the native call-graph analyses (issue #39, Phase 0).

Includes a golden-parity test whose expected values were captured by running
chiasmus's own `src/graph/native-analyses.ts` (the code this is ported from) on
the identical graph via `npx tsx`. Divergence flags a behavioral fork.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "services" / "v3-service"))

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
        # chiasmus formats path as the Prolog-list string "[main,helper,leaf]";
        # the node sequence is identical.
        assert analyses.path(g, "main", "leaf") == ["main", "helper", "leaf"]
        # Order matters: chiasmus emits impact in BFS discovery order.
        assert analyses.impact(g, "leaf") == ["helper", "main"]


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


class TestImpact:
    def test_impact_of_a_function_nobody_calls_is_empty(self):
        assert analyses.impact(_graph(), "main") == []

    def test_a_recursive_function_is_its_own_caller(self):
        assert analyses.impact(_graph(), "recurse") == ["recurse"]


class TestPath:
    def test_a_recursive_function_reaches_itself_in_one_call(self):
        assert analyses.path(_graph(), "recurse", "recurse") == ["recurse", "recurse"]

    def test_a_cycle_without_a_route_ends_with_no_path(self):
        g = CodeGraph(calls=[CallsFact("a", "b"), CallsFact("b", "a")],
                      defines=[DefinesFact("c.py", "c", "function", 1)])
        assert analyses.path(g, "a", "c") == []

    @pytest.mark.parametrize("frm, to", [
        pytest.param("leaf", "main", id="no-route"),
        pytest.param("nosuch", "leaf", id="unknown-start"),
        pytest.param("main", "nosuch", id="unknown-end"),
    ])
    def test_no_route_gives_no_path(self, frm, to):
        assert analyses.path(_graph(), frm, to) == []


class TestNamespacedTargets:
    """A bare `f` names both `a/f` and `b/f`; `a/f` calls `b/f`, and `x` calls `a/f`."""

    GRAPH = CodeGraph(calls=[CallsFact("x", "a/f"), CallsFact("a/f", "b/f")])

    def test_the_impact_of_a_bare_name_leaves_out_its_other_targets(self):
        assert analyses.impact(self.GRAPH, "f") == ["x"]

    def test_a_path_to_a_bare_name_ends_at_the_first_target_reached(self):
        assert analyses.path(self.GRAPH, "x", "f") == ["x", "a/f"]


class TestEntryPoints:
    def test_zero_in_degree_exports(self):
        g = _graph()
        # main (exported, zero in-degree) is an entry point.
        assert "main" in analyses.detect_entry_points(g)
