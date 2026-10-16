"""Native O(V+E) graph analyses.

Faithful port of chiasmus `src/graph/native-analyses.ts` and
`src/graph/entry-points.ts`. These run directly on the CodeGraph with plain
traversal (BFS / Tarjan SCC), no solver — the same design choice chiasmus made:
everyday structural queries are native.

`path` returns the call chain as a list of names (Python-friendly) rather than
chiasmus's Prolog-list string; the node sequence is identical.
"""

from __future__ import annotations

from typing import Dict, List, Optional, Set

from .types import CodeGraph


class _Index:
    __slots__ = ("adj", "rev", "nodes", "methods", "functions")

    def __init__(self):
        self.adj: Dict[str, List[str]] = {}
        self.rev: Dict[str, List[str]] = {}
        self.nodes: Set[str] = set()
        self.methods: Set[str] = set()
        self.functions: Set[str] = set()


def _build_index(graph: CodeGraph) -> _Index:
    # dict-as-ordered-set: preserves first-seen order AND dedups, matching the
    # JS Set the chiasmus reference uses. A plain set() would reorder (Python set
    # iteration is hash-seeded, not insertion-ordered), making path/impact/cycles
    # nondeterministic.
    adj: Dict[str, dict] = {}
    rev: Dict[str, dict] = {}
    idx = _Index()
    for c in graph.calls:
        idx.nodes.add(c.caller)
        idx.nodes.add(c.callee)
        adj.setdefault(c.caller, {})[c.callee] = None
        rev.setdefault(c.callee, {})[c.caller] = None
    for d in graph.defines:
        idx.nodes.add(d.name)
        if d.kind == "method":
            idx.methods.add(d.name)
        elif d.kind == "function":
            idx.functions.add(d.name)
    for k, vs in adj.items():
        idx.adj[k] = list(vs.keys())
    for k, vs in rev.items():
        idx.rev[k] = list(vs.keys())
    return idx


def _resolve_targets(graph: CodeGraph, target: str) -> List[str]:
    """Match a user-supplied name to graph nodes. Exact match wins; otherwise a
    bare name matches any `<ns>/target` suffix (namespaced languages). A target
    that already contains `/` is treated as fully qualified (exact only)."""
    # Ordered set (first-seen) so suffix matches come out in a stable order.
    all_names: "dict[str, None]" = {}
    for d in graph.defines:
        all_names.setdefault(d.name, None)
    for c in graph.calls:
        all_names.setdefault(c.caller, None)
        all_names.setdefault(c.callee, None)
    if target in all_names:
        return [target]
    if "/" in target:
        return []
    suffix = f"/{target}"
    return [n for n in all_names if n.endswith(suffix)]


def _bfs(adj: Dict[str, List[str]], source: str) -> List[str]:
    """Nodes reachable from `source` (including it), in BFS discovery order.
    Order matters: `impact` emits results in this order to match chiasmus, whose
    JS Set preserves insertion order."""
    seen = {source}
    queue = [source]
    head = 0
    while head < len(queue):
        u = queue[head]
        head += 1
        for v in adj.get(u, ()):
            if v not in seen:
                seen.add(v)
                queue.append(v)
    return queue


def callers(graph: CodeGraph, target: str) -> List[str]:
    """Direct callers of `target`, deduplicated, in first-seen order."""
    targets = set(_resolve_targets(graph, target))
    if not targets:
        return []
    seen: Set[str] = set()
    out: List[str] = []
    for c in graph.calls:
        if c.callee in targets and c.caller not in seen:
            seen.add(c.caller)
            out.append(c.caller)
    return out


def callees(graph: CodeGraph, source: str) -> List[str]:
    """Direct callees of `source`, deduplicated, in first-seen order."""
    sources = set(_resolve_targets(graph, source))
    if not sources:
        return []
    seen: Set[str] = set()
    out: List[str] = []
    for c in graph.calls:
        if c.caller in sources and c.callee not in seen:
            seen.add(c.callee)
            out.append(c.callee)
    return out


def reachability(graph: CodeGraph, frm: str, to: str) -> bool:
    """Is `to` reachable from `frm` through any call chain? A self-pair holds
    only via a real self-edge (matching the Prolog reaches/2 semantics)."""
    idx = _build_index(graph)
    from_targets = _resolve_targets(graph, frm)
    to_targets = set(_resolve_targets(graph, to))
    if not from_targets or not to_targets:
        return False
    for f in from_targets:
        if f not in idx.nodes:
            continue
        if f in to_targets and f in idx.adj.get(f, ()):
            return True
        reached = set(_bfs(idx.adj, f))
        for t in to_targets:
            if t == f:
                continue
            if t in reached:
                return True
    return False


def path(graph: CodeGraph, frm: str, to: str) -> List[str]:
    """Shortest call chain from `frm` to `to` as a list of names, or []."""
    idx = _build_index(graph)
    from_targets = _resolve_targets(graph, frm)
    to_targets = set(_resolve_targets(graph, to))
    if not from_targets or not to_targets:
        return []
    for f in from_targets:
        if f not in idx.nodes:
            continue
        if f in to_targets and f in idx.adj.get(f, ()):
            return [f, f]
        parent: Dict[str, str] = {}
        seen = {f}
        queue = [f]
        head = 0
        target: Optional[str] = None
        while head < len(queue):
            u = queue[head]
            head += 1
            hit = False
            for v in idx.adj.get(u, ()):
                if v in seen:
                    continue
                seen.add(v)
                parent[v] = u
                if v in to_targets:
                    target = v
                    hit = True
                    break
                queue.append(v)
            if hit:
                break
        if target is None:
            continue
        chain: List[str] = []
        cur: Optional[str] = target
        while cur is not None:
            chain.append(cur)
            cur = parent.get(cur)
        chain.reverse()
        return chain
    return []


def impact(graph: CodeGraph, target: str) -> List[str]:
    """Transitive callers of `target` — everything that could break if it
    changes. Unions impact sets across namespace-qualified matches."""
    idx = _build_index(graph)
    targets = _resolve_targets(graph, target)
    if not targets:
        return []
    target_set = set(targets)
    union: List[str] = []
    seen: Set[str] = set()
    for t in targets:
        if t not in idx.nodes:
            continue
        reached = _bfs(idx.rev, t)
        self_loop = t in idx.adj.get(t, ())
        for n in reached:
            if n == t:
                if self_loop and n not in seen:
                    seen.add(n)
                    union.append(n)
                continue
            if n in target_set or n in seen:
                continue
            seen.add(n)
            union.append(n)
    return union


def cycles(graph: CodeGraph) -> List[str]:  # noqa: C901
    """Function-level cycles via iterative Tarjan SCC. Methods are excluded
    because unqualified method names collide across classes and produce phantom
    cycles (faithful to chiasmus)."""
    idx = _build_index(graph)
    func_adj: Dict[str, List[str]] = {}
    func_nodes: "dict[str, None]" = {}  # ordered set: deterministic SCC start order
    for u, vs in idx.adj.items():
        if u in idx.methods:
            continue
        kept = [v for v in vs if v not in idx.methods]
        if kept:
            func_adj[u] = kept
            func_nodes.setdefault(u, None)
            for v in kept:
                func_nodes.setdefault(v, None)

    index = 0
    indices: Dict[str, int] = {}
    lowlink: Dict[str, int] = {}
    on_stack: Set[str] = set()
    stack: List[str] = []
    result: "dict[str, None]" = {}  # ordered set: stable result order

    for start in func_nodes:
        if start in indices:
            continue
        work = [[start, 0, func_adj.get(start, [])]]  # [v, it, succs]
        indices[start] = index
        lowlink[start] = index
        index += 1
        stack.append(start)
        on_stack.add(start)

        while work:
            frame = work[-1]
            v, it, succs = frame[0], frame[1], frame[2]
            if it < len(succs):
                w = succs[it]
                frame[1] += 1
                if w not in indices:
                    indices[w] = index
                    lowlink[w] = index
                    index += 1
                    stack.append(w)
                    on_stack.add(w)
                    work.append([w, 0, func_adj.get(w, [])])
                elif w in on_stack:
                    if indices[w] < lowlink[v]:
                        lowlink[v] = indices[w]
            else:
                if lowlink[v] == indices[v]:
                    scc: List[str] = []
                    while True:
                        w = stack.pop()
                        on_stack.discard(w)
                        scc.append(w)
                        if w == v:
                            break
                    if len(scc) > 1:
                        for n in scc:
                            result.setdefault(n, None)
                    else:
                        solo = scc[0]
                        if solo in func_adj.get(solo, ()):
                            result.setdefault(solo, None)
                work.pop()
                if work:
                    parent = work[-1][0]
                    if lowlink[v] < lowlink[parent]:
                        lowlink[parent] = lowlink[v]

    return list(result.keys())


def detect_entry_points(graph: CodeGraph) -> List[str]:
    """Heuristic entry points for dead-code: exported functions with zero
    in-degree, then all exports, then zero-in-degree functions. Methods
    excluded. Faithful to chiasmus entry-points.ts."""
    called = {c.callee for c in graph.calls}
    method_names = {d.name for d in graph.defines if d.kind == "method"}
    function_names = {d.name for d in graph.defines if d.kind == "function"}

    exported_fns = [e.name for e in graph.exports if e.name not in method_names]
    if exported_fns:
        zero_in = [n for n in exported_fns if n not in called]
        if zero_in:
            return sorted(set(zero_in))
        return sorted(set(exported_fns))
    roots = [n for n in function_names if n not in called]
    return sorted(set(roots))


def dead_code(graph: CodeGraph, entry_points: Optional[List[str]] = None) -> List[str]:
    """Functions defined, called by nobody, and not an entry point. Methods
    excluded (dynamic dispatch can't be statically resolved)."""
    called = {c.callee for c in graph.calls}
    entries: Set[str] = set()
    if entry_points:
        for ep in entry_points:
            resolved = _resolve_targets(graph, ep)
            if not resolved:
                entries.add(ep)
            else:
                entries.update(resolved)
    else:
        entries = {e.name for e in graph.exports}

    out: List[str] = []
    seen: Set[str] = set()
    for d in graph.defines:
        if d.kind != "function" or d.name in seen:
            continue
        if d.name in called or d.name in entries:
            continue
        seen.add(d.name)
        out.append(d.name)
    return out


def run_analysis(graph: CodeGraph, analysis: str, target: Optional[str] = None,
                 frm: Optional[str] = None, to: Optional[str] = None,
                 entry_points: Optional[List[str]] = None):
    """Dispatch a named analysis. Returns the analysis-specific result."""
    if analysis == "callers":
        return callers(graph, target or "")
    if analysis == "callees":
        return callees(graph, target or "")
    if analysis == "reachability":
        return reachability(graph, frm or "", to or "")
    if analysis == "path":
        return path(graph, frm or "", to or "")
    if analysis == "impact":
        return impact(graph, target or "")
    if analysis == "cycles":
        return cycles(graph)
    if analysis == "entry-points":
        return detect_entry_points(graph)
    if analysis == "dead-code":
        return dead_code(graph, entry_points)
    raise ValueError(f"unknown analysis: {analysis}")
