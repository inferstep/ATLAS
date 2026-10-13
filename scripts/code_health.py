#!/usr/bin/env python3
"""Code-health check: function and file size, against a baseline.

docs/CODE_STYLE.md sets the limits: a function over FUNC_MAX lines, or a
file over FILE_MAX lines, fails. Code that was already over the limit is
listed in .github/code-health-baseline.json with its size then. It may
shrink but not grow. So the check stops new oversized code and keeps the
old from getting worse while it is refactored.

Usage:
  scripts/code_health.py           check; exit 1 on a new or grown item
  scripts/code_health.py --update  write the baseline from the current code
                                   (after a refactor made items smaller)
  scripts/code_health.py --report  list the largest items and the totals

Go functions are found in gofmt-formatted source: `func` and the closing
`}` at column 0. Python functions come from the ast module. Tests are not
measured.
"""

from __future__ import annotations

import argparse
import ast
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
BASELINE = os.path.join(ROOT, ".github", "code-health-baseline.json")
FUNC_MAX = 100
FILE_MAX = 1500

GO_DIRS = ("proxy", "tui")
PY_DIRS = ("atlas", "v3-service", "geometric-lens", "sandbox", "scripts")
SKIP_DIRS = {"tests", "test", "node_modules", ".venv", "__pycache__", "vendor"}
GO_FUNC = re.compile(r"^func (?:\(\w+ \*?(\w+)(?:\[[^\]]*\])?\) )?(\w+)")


def source_files():
    for base, ext in [(d, ".go") for d in GO_DIRS] + [(d, ".py") for d in PY_DIRS]:
        for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, base)):
            dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
            for name in sorted(filenames):
                if not name.endswith(ext) or name.endswith("_test.go") or name.startswith("test_"):
                    continue
                path = os.path.join(dirpath, name)
                yield os.path.relpath(path, ROOT).replace(os.sep, "/")


def go_functions(text: str):
    """(name, lines) for each top-level function or method."""
    start = name = None
    for i, line in enumerate(text.split("\n")):
        m = GO_FUNC.match(line)
        if m:
            recv, fn = m.group(1), m.group(2)
            start, name = i, f"{recv}.{fn}" if recv else fn
            if line.rstrip().endswith("}"):  # one-line function
                yield name, 1
                start = None
        elif start is not None and line == "}":
            yield name, i - start + 1
            start = None


def py_functions(text: str, path: str):
    """(qualified name, lines) for each function and method."""
    try:
        tree = ast.parse(text, filename=path)
    except SyntaxError:
        return

    def walk(node, prefix):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                qual = f"{prefix}{child.name}"
                yield qual, child.end_lineno - child.lineno + 1
                yield from walk(child, qual + ".")
            elif isinstance(child, ast.ClassDef):
                yield from walk(child, f"{prefix}{child.name}.")

    yield from walk(tree, "")


def measure() -> tuple:
    """({path:function: lines} over FUNC_MAX, {path: lines} over FILE_MAX, totals)."""
    funcs, files = {}, {}
    totals = {"files": 0, "functions": 0, "lines": 0}
    for path in source_files():
        with open(os.path.join(ROOT, path), encoding="utf-8", errors="replace") as fh:
            text = fh.read()
        n = text.count("\n") + (0 if text.endswith("\n") else 1)
        totals["files"] += 1
        totals["lines"] += n
        if n > FILE_MAX:
            files[path] = n
        found = go_functions(text) if path.endswith(".go") else py_functions(text, path)
        seen: dict = {}
        for name, length in found:
            totals["functions"] += 1
            seen[name] = seen.get(name, 0) + 1
            key = f"{path}:{name}" + (f"#{seen[name]}" if seen[name] > 1 else "")
            if length > FUNC_MAX:
                funcs[key] = length
    return funcs, files, totals


def load_baseline() -> dict:
    try:
        with open(BASELINE, encoding="utf-8") as fh:
            return json.load(fh)
    except FileNotFoundError:
        return {"functions": {}, "files": {}}


def check(funcs: dict, files: dict, base: dict) -> list:
    problems = []
    for kind, current, limit in (("function", funcs, FUNC_MAX), ("file", files, FILE_MAX)):
        known = base.get(kind + "s", {})
        for key, size in sorted(current.items()):
            if key not in known:
                problems.append(f"new {kind} over {limit} lines: {key} ({size})")
            elif size > known[key]:
                problems.append(f"{kind} grew: {key} ({known[key]} -> {size}); the baseline allows no growth")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--update", action="store_true")
    mode.add_argument("--report", action="store_true")
    a = ap.parse_args()
    funcs, files, totals = measure()

    if a.update:
        with open(BASELINE, "w", encoding="utf-8") as fh:
            json.dump({"limits": {"function_lines": FUNC_MAX, "file_lines": FILE_MAX},
                       "functions": dict(sorted(funcs.items())), "files": dict(sorted(files.items()))},
                      fh, indent=1)
            fh.write("\n")
        print(f"baseline written: {len(funcs)} functions over {FUNC_MAX}, {len(files)} files over {FILE_MAX}")
        return 0

    if a.report:
        print(f"{totals['files']} files, {totals['functions']} functions, {totals['lines']} lines")
        print(f"{len(funcs)} functions over {FUNC_MAX} lines, {len(files)} files over {FILE_MAX} lines")
        for key, size in sorted(funcs.items(), key=lambda kv: -kv[1])[:15]:
            print(f"  {size:6d}  {key}")
        return 0

    base = load_baseline()
    problems = check(funcs, files, base)
    shrunk = [k for kind, cur in (("functions", funcs), ("files", files))
              for k, v in base.get(kind, {}).items() if cur.get(k, 0) < v]
    for p in problems:
        print(f"FAIL {p}")
    if shrunk:
        print(f"note: {len(shrunk)} baseline item(s) got smaller or went away; "
              "run scripts/code_health.py --update and commit the baseline")
    if problems:
        print("See docs/CODE_STYLE.md: split the function or file into parts with one job each.")
        return 1
    print(f"code health ok: no new or grown items ({len(funcs)} functions and {len(files)} files on the baseline)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
