"""Python import resolution: fill ImportsFact.resolved with an in-batch file.

Python has no tsconfig, so this is the analogue of chiasmus's suffix-index
import resolution (`src/graph/suffix-index.ts`) adapted to Python module paths:
map a file's repo-relative path to its dotted module name, then resolve an
import specifier to the file that defines it. Best-effort and cross-file aware;
unresolved specifiers (stdlib, third-party, dynamic) are left as None.
"""

from __future__ import annotations

from typing import Dict, List

from .types import CodeGraph


def _module_name(rel_path: str) -> str:
    p = rel_path
    if p.endswith("/__init__.py"):
        p = p[: -len("/__init__.py")]
    elif p.endswith(".py"):
        p = p[: -len(".py")]
    elif p.endswith((".pyi", ".pyx")):
        p = p.rsplit(".", 1)[0]
    return p.replace("/", ".").replace("\\", ".").strip(".")


def resolve_imports(graph: CodeGraph, file_paths: List[str]) -> None:
    """Fill `resolved` on each ImportsFact in place. `file_paths` is every
    repo-relative path in the extraction batch."""
    # module dotted name -> file path. Last write wins on collision, which is
    # fine for our best-effort resolution.
    module_to_file: Dict[str, str] = {}
    for path in file_paths:
        if path.endswith((".py", ".pyi", ".pyx")):
            module_to_file[_module_name(path)] = path

    if not module_to_file:
        return

    modules = list(module_to_file.keys())
    for imp in graph.imports:
        src = imp.source.strip().lstrip(".")  # relative-import dots dropped
        if not src:
            continue
        # 1. `from pkg import mod` where pkg/mod.py exists -> prefer the submodule
        #    file (where mod's code lives) over pkg/__init__.py. Checked first so
        #    a submodule import points at the submodule, not the package init.
        candidate = f"{src}.{imp.name}" if imp.name else src
        if candidate in module_to_file:
            imp.resolved = module_to_file[candidate]
            continue
        # 2. exact module match (covers `import pkg.mod` and `from pkg.mod import x`)
        if src in module_to_file:
            imp.resolved = module_to_file[src]
            continue
        # 3. suffix match: a bare `import mod` resolves to any `*/mod`.
        suffix = f".{src}"
        hits = [m for m in modules if m == src or m.endswith(suffix)]
        if len(hits) == 1:
            imp.resolved = module_to_file[hits[0]]
