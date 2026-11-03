"""Graph-backed call resolution for the structural veto (issue #39, Phase 1).

The shipped veto (v3-service `structural_score`) accepts a bare call name if ANY
project file defines a top-level symbol with that name, even when the candidate
never imports it. That lets through genuine broken cross-file references (a
NameError the sandbox can miss on an unexecuted path). This module resolves
direct-identifier calls precisely against the import graph instead:

- a call resolves if its name is defined locally, is a builtin, is an imported
  name, or is supplied by a wildcard import whose module's *actual* exports
  include it (resolved via the call graph, not blanket-accepted);
- when a wildcard import can't be resolved to an in-batch file (stdlib /
  third-party), resolution is treated as uncertain and nothing is flagged
  (conservative, matching today's leniency);
- attribute / method calls (`obj.foo()`) are out of scope, exactly as the
  shipped veto, because the receiver type isn't statically known.

`strict=False` restores the old behavior (accept any project symbol) so the veto
can be tightened gradually.
"""

from __future__ import annotations

from typing import Dict, List, Optional, Set

from . import extract as _extract
from .resolve import resolve_imports
from .types import CodeGraph

# The single tree-sitter walk implementations live in symbols.py (the
# structural-veto core); this module reuses them rather than keeping a
# parallel pair. PY_BUILTINS is the complete interpreter-derived builtin
# namespace — symbols.py documents why a hand-curated subset is a bug
# class (any gap is a false VETO of valid code).
from symbols import (
    PY_BUILTINS,
    _extract_python_bound_names,
    _extract_python_call_targets,
)


def _defs_by_file(graph: CodeGraph) -> Dict[str, Set[str]]:
    out: Dict[str, Set[str]] = {}
    for d in graph.defines:
        out.setdefault(d.file, set()).add(d.name)
    return out


def unresolved_calls(
    candidate_path: str,
    candidate_code: str,
    project_files: Optional[Dict[str, str]] = None,
    builtins: Optional[Set[str]] = None,
    strict: bool = True,
) -> dict:
    """Resolve the candidate's direct calls against the import graph.

    Returns {"ok", "unresolved": [...], "n_calls_total", "lenient": bool}.
    `lenient` is True when an unresolvable wildcard import means nothing can be
    confidently flagged. `ok=False` (with "error") when extraction is
    unavailable or the candidate is not Python.

    Python only. The walks below use the Python grammar, which parses any text
    tolerantly: an HTML page whose <script> called setInterval came back with
    setInterval and `function` unresolved, so the page with working JavaScript
    was vetoed and a static one kept.
    """
    if not _extract.is_python(candidate_path):
        return {"ok": False, "error": "call-graph resolution supports Python only"}
    if not _extract.available():
        return {"ok": False, "error": "tree-sitter not installed"}

    project_files = project_files or {}
    builtins = builtins or PY_BUILTINS
    candidate_bytes = candidate_code.encode("utf-8")

    cand = _extract.extract_file(candidate_path, candidate_code)
    # All names bound anywhere in the file (params, locals, assignments, defs),
    # not just def/class names — otherwise callbacks and assigned callables
    # false-positive. See symbols._extract_python_bound_names.
    local = _extract_python_bound_names(candidate_bytes) | {d.name for d in cand.defines}
    import_names: Set[str] = set()
    for i in cand.imports:
        if i.name == "*":
            continue  # handled below via wildcard resolution
        import_names.add(i.name)
        # `import a.b.c` binds only the top package `a` (you'd call a.b.c.x()
        # as an attribute, never bare `c`). Bind the first segment, not the last.
        import_names.add(i.name.split(".")[0])

    # Build the project graph (cached) to resolve wildcard modules to their
    # actual exported names, and for the non-strict project-symbol fallback.
    all_files = dict(project_files)
    all_files[candidate_path] = candidate_code
    from . import build_graph  # local import avoids a cycle at module load
    proj = build_graph(all_files)
    defs_by_file = _defs_by_file(proj)
    project_symbols = {d.name for d in proj.defines}

    # Resolve the candidate's wildcard imports to in-batch files.
    resolve_imports(cand, list(all_files.keys()))
    wildcard_names: Set[str] = set()
    lenient = False
    for i in cand.imports:
        if i.name != "*":
            continue
        if i.resolved and i.resolved in defs_by_file:
            wildcard_names |= defs_by_file[i.resolved]
        else:
            # Wildcard from an unresolved module (stdlib / third-party): we can't
            # know what it supplies, so don't flag anything.
            lenient = True

    resolved = local | set(builtins) | import_names | wildcard_names
    if not strict:
        resolved |= project_symbols

    calls = _extract_python_call_targets(candidate_bytes)
    unresolved: List[str] = []
    seen: Set[str] = set()
    if not lenient:
        for name in calls:
            if name in resolved or name in seen:
                continue
            seen.add(name)
            unresolved.append(name)

    return {
        "ok": True,
        "unresolved": unresolved[:10],
        "n_unresolved": len(unresolved),
        "n_calls_total": len(calls),
        "lenient": lenient,
    }
