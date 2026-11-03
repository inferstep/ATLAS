"""Graph-backed context builders for the repair loop and symbol injection.

Phase 2 (#39 point 3): `repair_context` builds the failure-context block for
the repair model — with `transitive=True` a real reachability slice (the path
from an entry point down to the failing function, its transitive callers, and
its callees), with `transitive=False` the direct 1-hop callers/callees only
(the flag-off repair path). Phase 3 (#39 point 4): `symbol_neighborhood`
returns a named symbol's graph neighborhood so context injection can pull in
the structurally related code instead of name-matched snippets.

Both reuse the native analyses; no new traversal logic.
"""

from __future__ import annotations

from typing import Dict, List

from . import analyses
from .extract import is_supported


def _project_graph(file_map: Dict[str, str]):
    from . import build_graph
    return build_graph(file_map)


def symbol_neighborhood(
    file_map: Dict[str, str], symbol: str, max_items: int = 10, graph=None
) -> dict:
    """The graph neighborhood of `symbol`: direct callers, direct callees, the
    transitive impact set (everything that can reach it), and the files that
    define it. Empty lists when the symbol isn't in the graph.

    Pass a prebuilt `graph` to neighborhood many symbols without rebuilding the
    project graph per symbol (the caller should build it once)."""
    if graph is None:
        graph = _project_graph(file_map)
    defined_files = sorted({d.file for d in graph.defines if d.name == symbol})
    return {
        "symbol": symbol,
        "defined_in": defined_files,
        "callers": analyses.callers(graph, symbol)[:max_items],
        "callees": analyses.callees(graph, symbol)[:max_items],
        "impact": analyses.impact(graph, symbol)[:max_items],
    }


def _entry_path_to(graph, function_name: str) -> List[str]:
    """A call path from some entry point down to `function_name`, or []."""
    for entry in analyses.detect_entry_points(graph):
        if entry == function_name:
            continue
        chain = analyses.path(graph, entry, function_name)
        if chain:
            return chain
    return []


def repair_context(
    file_map: Dict[str, str],
    function_name: str,
    max_items: int = 8,
    transitive: bool = True,
) -> str:
    """Markdown call-graph slice around a failing function for the repair
    model. `transitive=True` adds the entry-point witness path and the
    transitive impact set; `transitive=False` stays at direct callers and
    callees (1 hop). Returns "" when the function isn't in the graph (caller
    skips the block rather than diluting the error with a useless
    'no matches')."""
    if not function_name or not file_map:
        return ""
    if not any(is_supported(p) for p in file_map):
        return ""

    graph = _project_graph(file_map)
    defined_files = sorted({d.file for d in graph.defines if d.name == function_name})
    if not defined_files:
        return ""

    direct_callers = analyses.callers(graph, function_name)
    callees = analyses.callees(graph, function_name)
    impact = analyses.impact(graph, function_name) if transitive else []
    witness = _entry_path_to(graph, function_name) if transitive else []

    sb: List[str] = [f"## Call-graph context for failing function `{function_name}`", ""]
    sb.append(f"Defined in: {', '.join('`' + f + '`' for f in defined_files)}")
    sb.append("")

    if witness:
        sb.append("**Call path from an entry point (how execution reaches it):**")
        sb.append("`" + " → ".join(witness) + "`")
        sb.append("")

    if direct_callers:
        capped = direct_callers[:max_items]
        sb.append(f"**Direct callers ({len(direct_callers)}):** "
                  + ", ".join("`" + c + "`" for c in capped)
                  + (f" … +{len(direct_callers) - max_items} more" if len(direct_callers) > max_items else ""))
    else:
        sb.append("**Direct callers:** none (entry point or called only externally)")

    # The impact set beyond the direct callers is the transitive blast radius.
    direct_set = set(direct_callers)
    transitive = [n for n in impact if n not in direct_set]
    if transitive:
        capped = transitive[:max_items]
        sb.append(f"**Also transitively affected ({len(transitive)}):** "
                  + ", ".join("`" + c + "`" for c in capped)
                  + (f" … +{len(transitive) - max_items} more" if len(transitive) > max_items else ""))

    if callees:
        capped = callees[:max_items]
        sb.append(f"**Calls out to ({len(callees)}):** "
                  + ", ".join("`" + c + "`" for c in capped)
                  + (f" … +{len(callees) - max_items} more" if len(callees) > max_items else ""))
    else:
        sb.append(f"**Calls out to:** none (`{function_name}` is a leaf)")

    sb.append("")
    sb.append(f"Scope the fix with this: changing what `{function_name}` returns or "
              "raises can break everything in the affected set above; changing what "
              "it calls usually can't.")
    return "\n".join(sb)
