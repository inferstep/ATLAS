#!/usr/bin/env python3
"""Code-health check: function and file size, and function complexity, against a baseline.

docs/project/CODE_STYLE.md sets the limits: a function over FUNC_MAX lines, or a
file over FILE_MAX lines, fails. Code that was already over the limit is
listed in .github/code-health-baseline.json with its size then. It may
shrink but not grow. So the check stops new oversized code and keeps the
old from getting worse while it is refactored.

Complexity works the same way, with COMPLEXITY_MAX for each function: its
cognitive complexity (how hard it is to follow; SonarQube's rule S3776) for
Go and Python, and its cyclomatic complexity (how many paths it has) for Go.
Go is measured with gocognit and gocyclo, at the versions golangci-lint
bundles, through `go run`; Go's tests are measured too. Python is measured
with the copy of cognitive_complexity in scripts/ci/cognitive_complexity/.
Python's cyclomatic complexity is ruff's C901 (pyproject.toml), whose
markers and RUF100 hold it the same way.

The proxy's main package may not gain a file: its file count is pinned in
the baseline as well, so a new subject goes into its own package under
services/proxy/internal/ instead of the flat folder. Its line count is printed on
every run, as a trend, and never fails.

It also names the folders with more than FOLDER_SOFT_MAX entries. That is a
note and never a failure: a sub-folder that makes a list harder to read is no
gain, so a folder can stay flat with a reason.

Usage:
  scripts/ci/code_health.py           check; exit 1 on a new or grown item
  scripts/ci/code_health.py --update  write the baseline from the current code
                                   (after a refactor made items smaller)
  scripts/ci/code_health.py --report  list the largest items and the totals

Go functions are found in gofmt-formatted source: `func` and the closing
`}` at column 0. Python functions come from the ast module. Tests are not
measured for size.
"""

from __future__ import annotations

import argparse
import ast
import json
import os
import re
import shutil
import subprocess
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__))))
HERE = os.path.dirname(os.path.abspath(__file__))
BASELINE = os.path.join(ROOT, ".github", "code-health-baseline.json")
FUNC_MAX = 100
FILE_MAX = 1500
COMPLEXITY_MAX = 15
FOLDER_SOFT_MAX = 25
# GitHub reads workflows from this folder only, and never from a sub-folder.
FLAT_BY_NECESSITY = {".github/workflows"}

GO_DIRS = ("services/proxy", "clients/tui")
PY_DIRS = ("atlas", "services/v3-service", "services/geometric-lens", "services/sandbox", "scripts")
SKIP_DIRS = {"tests", "test", "node_modules", ".venv", "__pycache__", "vendor"}
# The Go package that may only shrink: the files directly in this folder.
MAIN_PACKAGE = "services/proxy"
GO_FUNC = re.compile(r"^func (?:\(\w+ \*?(\w+)(?:\[[^\]]*\])?\) )?(\w+)")

# The versions golangci-lint v2.14.0 bundles, so this check and the lint judge alike.
GOCOGNIT = "github.com/uudashr/gocognit/cmd/gocognit@v1.2.1"
GOCYCLO = "github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0"
# Both print one line for each function over the limit: "<score> <package> <function> <file>:<line>:<column>".
GO_SCORE = re.compile(r"^(\d+) \S+ (\S+) (.+):\d+:\d+$")
# A method as both print it, "(*server).handle", named the way go_functions names it: "server.handle".
GO_RECEIVER = re.compile(r"^\(\*?(\w+)(?:\[[^\]]*\])?\)\.")
# Not measured for complexity. These two are not edited until their rework lands, so they
# cannot rise, and the baseline names none of their functions.
NOT_MEASURED = {"services/proxy/tool_args.go", "services/proxy/tool_args_test.go"}


class CannotMeasure(Exception):
    """A complexity tool is missing or did not run; the message says what to do."""


def is_measured(name: str, ext: str, go_tests: bool) -> bool:
    """Whether a file of this name is measured: it has the extension, and is no test unless go_tests."""
    if not name.endswith(ext) or name.startswith("test_"):
        return False
    return go_tests or not name.endswith("_test.go")


def source_files(go_tests: bool = False):
    for base, ext in [(d, ".go") for d in GO_DIRS] + [(d, ".py") for d in PY_DIRS]:
        for dirpath, dirnames, filenames in os.walk(os.path.join(ROOT, base)):
            dirnames[:] = sorted(d for d in dirnames if d not in SKIP_DIRS)
            for name in sorted(filenames):
                if not is_measured(name, ext, go_tests):
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


def py_function_nodes(node, prefix=""):
    """(qualified name, node) for each function and method under node."""
    for child in ast.iter_child_nodes(node):
        if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
            qual = f"{prefix}{child.name}"
            yield qual, child
            yield from py_function_nodes(child, qual + ".")
        elif isinstance(child, ast.ClassDef):
            yield from py_function_nodes(child, f"{prefix}{child.name}.")


def py_functions(text: str, path: str):
    """(qualified name, lines) for each function and method."""
    try:
        tree = ast.parse(text, filename=path)
    except SyntaxError:
        return
    for qual, node in py_function_nodes(tree):
        yield qual, node.end_lineno - node.lineno + 1


def function_key(path: str, name: str, seen: dict) -> str:
    """path:name, with #2, #3... for a name the file holds more than once (seen counts them)."""
    seen[name] = seen.get(name, 0) + 1
    return f"{path}:{name}" + (f"#{seen[name]}" if seen[name] > 1 else "")


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
            key = function_key(path, name, seen)
            if length > FUNC_MAX:
                funcs[key] = length
    return funcs, files, totals


def load_cognitive_measure():
    """get_cognitive_complexity from the copy in this folder.

    The copy imports itself by its own name, so this folder is on the path for the
    import only, and nothing else imported later can be shadowed by a script here.
    """
    sys.path.insert(0, HERE)
    try:
        from cognitive_complexity.api import get_cognitive_complexity
    finally:
        sys.path.remove(HERE)
    return get_cognitive_complexity


def python_cognitive(paths: list) -> dict:
    """{path:function: score} for each Python function over COMPLEXITY_MAX."""
    get_cognitive_complexity = load_cognitive_measure()
    scores = {}
    for path in paths:
        with open(os.path.join(ROOT, path), encoding="utf-8", errors="replace") as fh:
            text = fh.read()
        try:
            tree = ast.parse(text, filename=path)
        except SyntaxError:
            continue
        seen: dict = {}
        for name, node in py_function_nodes(tree):
            key = function_key(path, name, seen)
            score = get_cognitive_complexity(node)
            if score > COMPLEXITY_MAX:
                scores[key] = score
    return scores


def go_scores(tool: str, paths: list) -> dict:
    """{path:function: score} for each Go function over COMPLEXITY_MAX, as tool measures it."""
    if shutil.which("go") is None:
        raise CannotMeasure(f"go is not on the PATH: install Go (https://go.dev/dl/); this check runs {tool} "
                            "through `go run`")
    run = subprocess.run(["go", "run", tool, "-over", str(COMPLEXITY_MAX), *paths],
                         cwd=ROOT, capture_output=True, text=True, check=False)
    # The tool exits 1 when it lists a function, and `go run` then adds "exit status 1",
    # so the exit code says nothing. Any other line on stderr is a failure to measure.
    errors = [line for line in run.stderr.splitlines()
              if line != "exit status 1" and not line.startswith("go: downloading ")]
    if errors:
        raise CannotMeasure(f"{tool} did not run: " + " | ".join(errors[:5]))
    scores = {}
    seen_in: dict = {}
    for line in run.stdout.splitlines():
        m = GO_SCORE.match(line)
        if not m:
            raise CannotMeasure(f"{tool} printed a line this check cannot read: {line!r}")
        score, name, path = int(m.group(1)), GO_RECEIVER.sub(r"\1.", m.group(2)), m.group(3)
        scores[function_key(path, name, seen_in.setdefault(path, {}))] = score
    return scores


def measure_complexity() -> dict:
    """{"cognitive": {path:function: score}, "cyclomatic": {...}} for each function over COMPLEXITY_MAX."""
    python = [p for p in source_files() if p.endswith(".py")]
    go = [p for p in source_files(go_tests=True) if p.endswith(".go") and p not in NOT_MEASURED]
    return {"cognitive": {**python_cognitive(python), **go_scores(GOCOGNIT, go)},
            "cyclomatic": go_scores(GOCYCLO, go)}


def main_package_size() -> dict:
    """{"files": n, "lines": n} for the non-test Go files directly in MAIN_PACKAGE."""
    size = {"files": 0, "lines": 0}
    folder = os.path.join(ROOT, MAIN_PACKAGE)
    for name in sorted(os.listdir(folder)):
        if not name.endswith(".go") or name.endswith("_test.go"):
            continue
        with open(os.path.join(folder, name), encoding="utf-8", errors="replace") as fh:
            text = fh.read()
        size["files"] += 1
        size["lines"] += text.count("\n") + (0 if text.endswith("\n") else 1)
    return size


def main_package_problems(size: dict, base: dict) -> list:
    """A failure when the main package has more files than its pin, or no pin.

    A missing pin fails rather than passes: the integrity check reads a removed
    baseline entry as a tightening, and for this pin a removal loosens the check.
    """
    pinned = base.get("proxy_main", {}).get("files")
    if pinned is None:
        return [f"{MAIN_PACKAGE}'s main package has no pinned file count in the baseline; "
                "run scripts/ci/code_health.py --update"]
    if size["files"] <= pinned:
        return []
    return [f"{MAIN_PACKAGE}'s main package grew: files {pinned} -> {size['files']}; "
            f"a new subject goes into its own package under {MAIN_PACKAGE}/internal/"]


def tracked_paths() -> list:
    """Every path git tracks. With no git or no checkout there are none, so no note."""
    try:
        out = subprocess.run(["git", "-C", ROOT, "ls-files"], capture_output=True, text=True, check=False)
    except OSError:
        return []
    return out.stdout.splitlines()


def crowded_folders(paths: list) -> dict:
    """{folder: entries} over FOLDER_SOFT_MAX, counting files and sub-folders."""
    entries: dict = {}
    for path in paths:
        parts = path.split("/")
        for depth, part in enumerate(parts):
            entries.setdefault("/".join(parts[:depth]) or ".", set()).add(part)
    return {folder: len(names) for folder, names in entries.items()
            if len(names) > FOLDER_SOFT_MAX and folder not in FLAT_BY_NECESSITY}


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


def complexity_problems(scores: dict, base: dict) -> list:
    """A failure for each function newly over COMPLEXITY_MAX, and for each listed one that rose."""
    problems = []
    for measure, current in sorted(scores.items()):
        known = base.get(measure, {})
        for key, score in sorted(current.items()):
            if key not in known:
                problems.append(f"new function over {measure} complexity {COMPLEXITY_MAX}: {key} ({score})")
            elif score > known[key]:
                problems.append(f"{measure} complexity grew: {key} ({known[key]} -> {score}); "
                                "the baseline allows no growth")
    return problems


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    mode = ap.add_mutually_exclusive_group()
    mode.add_argument("--update", action="store_true")
    mode.add_argument("--report", action="store_true")
    a = ap.parse_args()
    funcs, files, totals = measure()
    main_size = main_package_size()
    try:
        scores = measure_complexity()
    except CannotMeasure as e:
        print(f"FAIL {e}")
        return 1

    if a.update:
        with open(BASELINE, "w", encoding="utf-8") as fh:
            json.dump({"limits": {"function_lines": FUNC_MAX, "file_lines": FILE_MAX, "complexity": COMPLEXITY_MAX},
                       "functions": dict(sorted(funcs.items())), "files": dict(sorted(files.items())),
                       "proxy_main": {"files": main_size["files"]},
                       "cognitive": dict(sorted(scores["cognitive"].items())),
                       "cyclomatic": dict(sorted(scores["cyclomatic"].items()))},
                      fh, indent=1)
            fh.write("\n")
        print(f"baseline written: {len(funcs)} functions over {FUNC_MAX}, {len(files)} files over {FILE_MAX}, "
              f"{len(scores['cognitive'])} functions over cognitive and {len(scores['cyclomatic'])} over "
              f"cyclomatic complexity {COMPLEXITY_MAX}")
        return 0

    if a.report:
        print(f"{totals['files']} files, {totals['functions']} functions, {totals['lines']} lines")
        print(f"{len(funcs)} functions over {FUNC_MAX} lines, {len(files)} files over {FILE_MAX} lines")
        for key, size in sorted(funcs.items(), key=lambda kv: -kv[1])[:15]:
            print(f"  {size:6d}  {key}")
        print(f"{len(scores['cognitive'])} functions over cognitive and {len(scores['cyclomatic'])} over "
              f"cyclomatic complexity {COMPLEXITY_MAX}")
        return 0

    base = load_baseline()
    problems = check(funcs, files, base) + complexity_problems(scores, base) + main_package_problems(main_size, base)
    measured = (("functions", funcs), ("files", files), ("proxy_main", main_size),
                ("cognitive", scores["cognitive"]), ("cyclomatic", scores["cyclomatic"]))
    shrunk = [k for kind, cur in measured for k, v in base.get(kind, {}).items() if cur.get(k, 0) < v]
    pinned_files = base.get("proxy_main", {}).get("files", "none")
    print(f"note: {MAIN_PACKAGE}'s main package: {main_size['files']} files (pinned: {pinned_files}), "
          f"{main_size['lines']:,} lines")
    for p in problems:
        print(f"FAIL {p}")
    if shrunk:
        print(f"note: {len(shrunk)} baseline item(s) got smaller or went away; "
              "run scripts/ci/code_health.py --update and commit the baseline")
    crowded = crowded_folders(tracked_paths())
    if crowded:
        listed = ", ".join(f"{folder} ({n})" for folder, n in sorted(crowded.items(), key=lambda kv: -kv[1]))
        print(f"note: {len(crowded)} folder(s) over {FOLDER_SOFT_MAX} entries: {listed}")
    if problems:
        print("See docs/project/CODE_STYLE.md: split the function or file into parts with one job each.")
        return 1
    print(f"code health ok: no new or grown items ({len(funcs)} functions and {len(files)} files on the size "
          f"baseline, {len(scores['cognitive'])} cognitive and {len(scores['cyclomatic'])} cyclomatic)")
    return 0


if __name__ == "__main__":
    sys.exit(main())
