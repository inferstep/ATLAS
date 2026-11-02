#!/usr/bin/env python3
"""Fail when shipped code uses syntax newer than the declared Python floor.

`pyproject.toml` sets `requires-python`, but CI runs the test matrix on
3.11/3.12 only, so nothing was comparing the two. They drifted:
`sandbox/executor_server.py` carried `def f(...) -> str | None`, a PEP 604
union evaluated at definition time, which raises TypeError on 3.9 while the
package advertised `>=3.9`. It surfaced as 29 fixture errors for anyone on
the floor version, including RHEL 9 hosts where 3.9 is the system Python.

The check is AST-only: no imports, no dependencies, and it runs correctly on
any interpreter regardless of which version it is being run by, which is what
makes it cheap enough to gate on without doubling the CI matrix.

Flagged, when the floor is below 3.10 and the file lacks
`from __future__ import annotations`:

  * PEP 604 unions (`X | Y`) in an annotation position

`from __future__ import annotations` (3.7+) defers annotation evaluation and
resolves this, so files carrying it are exempt.

Usage: check_min_python.py [paths...]   (defaults to the shipped tree)
"""

from __future__ import annotations

import ast
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]

# scripts/ is included because it runs under the developer's own
# interpreter — `atlas bench` and `atlas lens build` are driven from the
# host, not a container, so the floor applies the same way (the bench
# harness itself lives under atlas/).
DEFAULT_TARGETS = ("atlas", "geometric-lens", "v3-service", "sandbox",
                   "scripts", "tests")

# Directories that never ship to a user's interpreter.
SKIP_PARTS = {"__pycache__", ".venv", "node_modules", "build", "dist"}


def declared_floor() -> tuple[int, int] | None:
    """Parse `requires-python` from pyproject.toml into (major, minor).

    Read with a regex rather than tomllib, which is 3.11+ — this script has
    to run on the floor interpreter it is checking for.
    """
    text = (ROOT / "pyproject.toml").read_text(encoding="utf-8")
    m = re.search(r'^\s*requires-python\s*=\s*["\']([^"\']+)["\']',
                  text, re.MULTILINE)
    if not m:
        return None
    v = re.search(r'>=\s*(\d+)\.(\d+)', m.group(1))
    if not v:
        return None
    return int(v.group(1)), int(v.group(2))


class AnnotationUnionVisitor(ast.NodeVisitor):
    """Collect `X | Y` appearing in annotation position."""

    def __init__(self) -> None:
        self.hits: list[tuple[int, str]] = []

    def _scan(self, node: ast.AST | None) -> None:
        if node is None:
            return
        for sub in ast.walk(node):
            if isinstance(sub, ast.BinOp) and isinstance(sub.op, ast.BitOr):
                self.hits.append((getattr(sub, "lineno", 0),
                                  ast.dump(sub, annotate_fields=False)[:60]))

    def visit_FunctionDef(self, node: ast.FunctionDef) -> None:
        self._scan(node.returns)
        for arg in (*node.args.args, *node.args.kwonlyargs,
                    *node.args.posonlyargs, node.args.vararg, node.args.kwarg):
            if arg is not None:
                self._scan(arg.annotation)
        self.generic_visit(node)

    visit_AsyncFunctionDef = visit_FunctionDef  # type: ignore[assignment]

    def visit_AnnAssign(self, node: ast.AnnAssign) -> None:
        self._scan(node.annotation)
        self.generic_visit(node)


def has_future_annotations(tree: ast.Module) -> bool:
    for node in tree.body:
        if isinstance(node, ast.ImportFrom) and node.module == "__future__":
            if any(a.name == "annotations" for a in node.names):
                return True
    return False


def display_path(path: Path) -> str:
    """Repo-relative when possible, absolute otherwise.

    Explicit path arguments may point outside the repo (a temp file, a
    checkout elsewhere), and `Path.relative_to` raises rather than falling
    back for those.
    """
    try:
        return str(path.relative_to(ROOT))
    except ValueError:
        return str(path)


def iter_python_files(targets: list[str]):
    for t in targets:
        p = (ROOT / t) if not Path(t).is_absolute() else Path(t)
        if p.is_file() and p.suffix == ".py":
            yield p
        elif p.is_dir():
            for f in sorted(p.rglob("*.py")):
                if not SKIP_PARTS & set(f.parts):
                    yield f


def main(argv: list[str]) -> int:
    floor = declared_floor()
    if floor is None:
        print("check_min_python: no `requires-python = \">=X.Y\"` in "
              "pyproject.toml — nothing to enforce", file=sys.stderr)
        return 0
    if floor >= (3, 10):
        print(f"check_min_python: floor is {floor[0]}.{floor[1]}, "
              "PEP 604 is native — nothing to check")
        return 0

    targets = argv[1:] or list(DEFAULT_TARGETS)
    problems: list[str] = []
    scanned = 0

    for path in iter_python_files(targets):
        try:
            source = path.read_text(encoding="utf-8")
            tree = ast.parse(source, filename=str(path))
        except (SyntaxError, UnicodeDecodeError) as e:
            problems.append(f"{display_path(path)}: cannot parse ({e})")
            continue
        scanned += 1
        if has_future_annotations(tree):
            continue
        v = AnnotationUnionVisitor()
        v.visit(tree)
        for lineno, _ in v.hits:
            rel = display_path(path)
            problems.append(
                f"{rel}:{lineno}: PEP 604 union in an annotation, evaluated at "
                f"definition time on Python < 3.10 (floor is "
                f"{floor[0]}.{floor[1]}). Add "
                f"`from __future__ import annotations` to this file.")

    if problems:
        print(f"check_min_python: {len(problems)} problem(s) against the "
              f"declared floor {floor[0]}.{floor[1]}:", file=sys.stderr)
        for p in problems:
            print(f"  {p}", file=sys.stderr)
        print("\nEither add the __future__ import, or raise "
              "`requires-python` in pyproject.toml if the floor "
              "is no longer supported.", file=sys.stderr)
        return 1

    print(f"check_min_python: {scanned} files clean against floor "
          f"{floor[0]}.{floor[1]}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
