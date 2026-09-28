"""Structural call-graph reasoning (issue #39).

A faithful Python port of the chiasmus call-graph engine
(https://github.com/yogthos/chiasmus, `src/graph/`), scoped to what the shipped
#39 integration points need. Builds a precomputed CodeGraph from tree-sitter
extraction and answers callers / callees / reachability / path / impact /
cycles / dead-code / entry-points natively (O(V+E), no solver), with a
per-file-hash cache for incremental recompute.

Provenance (ported behavior-for-behavior):
  types.py    <- src/graph/types.ts
  extract.py  <- src/graph/extractor.ts (walkPython)
  analyses.py <- src/graph/native-analyses.ts + entry-points.ts
  resolve.py  <- src/graph/suffix-index.ts (adapted to Python modules)
  cache.py    <- src/graph/cache.ts (in-process)

See docs/reports/CALL_GRAPH_REASONING_V3.md.

The package facade exports only what production imports (pipeline.py and
main.py); tests import the submodules directly.
"""

from __future__ import annotations

from typing import Dict, List, Optional

from .types import CodeGraph
from .extract import is_python, is_supported
from .resolve import resolve_imports
from .cache import FileGraphCache, default_cache
from .resolve_calls import unresolved_calls
from .context import repair_context, symbol_neighborhood


def build_graph(file_map: Dict[str, str], cache: Optional[FileGraphCache] = None) -> CodeGraph:
    """Build a project CodeGraph from {rel_path: content}.

    Python and JavaScript files contribute (issue #39 Phase 6); others are
    skipped. Per-file extraction is cached on a content hash (incremental
    recompute), then Python imports are resolved across the batch. Pass a
    FileGraphCache to reuse extraction across calls; omit it for a one-shot."""
    cache = cache or default_cache()
    graph = CodeGraph()
    py_paths: List[str] = []
    for rel, content in file_map.items():
        if not is_supported(rel):
            continue
        if is_python(rel):
            py_paths.append(rel)
        graph.merge(cache.get_or_extract(rel, content))
    # Import resolution is Python-only for now; JS imports stay unresolved
    # (the analyses don't depend on resolution).
    resolve_imports(graph, py_paths)
    return graph


__all__ = [
    "build_graph",
    "repair_context",
    "symbol_neighborhood",
    "unresolved_calls",
]
