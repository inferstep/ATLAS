#!/usr/bin/env python3
"""Do the tests of a fix fail without the fix?

For a pull request that fixes a bug, this takes the tests the pull request
adds or changes and runs them twice: on the base's code with the pull
request's test files laid over it, and on the pull request itself. A new
test that passes both times does not guard the fix.

What it says for each test (three classes, never added together):
  fails on the base        it fails there for what the code does, and passes
                           with the fix
  needs the fix's code     on the base the test cannot run as written: it does
                           not build or import, it names something that is
                           not there yet, or it calls a function in a way the
                           base's function does not take
  passes on the base too   it does not guard the fix. For a test the fix adds
                           it is a finding when no other test of the fix
                           fails on the base; beside such a test it is most
                           likely a control, and is listed for information
and, outside the three:
  not judged here          it fails on the base and with the fix (the place
                           it runs in may lack something it needs), it is
                           skipped, or a run did not end in its time
  fails with the fix       it passes on the base and fails on the pull request

For the fix as a whole it says one of: guarded (a test fails on the base),
not shown (tests ran on the base and none failed), cannot tell (no test could
run on the base), no test.

It judges only a pull request whose title has the type `fix`. It reports and
exits 0 for every verdict, and exits 2 when it could not do its work.

Usage: fix_tests.py --base COMMIT --title "fix(scope): summary" [--github]
"""
from __future__ import annotations

import argparse
import ast
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
from dataclasses import dataclass
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
FIX_TITLE = re.compile(r"^fix(\([^)]*\))?!?:")
HUNK = re.compile(r"^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@")
GO_TEST = re.compile(r"^func (Test\w+)\(")
# What a test says when it cannot run as written on the base: a name, a
# module, a fixture or a parameter that only the fix brings.
CANNOT_RUN = re.compile(
    r"\b(?:ImportError|ModuleNotFoundError|NameError)\b"
    r"|AttributeError: (?:module|type object|partially initialized module) '[^']+' has no attribute"
    r"|fixture '[^']+' not found"
    r"|TypeError: .*(?:unexpected keyword argument|positional arguments? but \d+ (?:was|were) given"
    r"|missing \d+ required (?:positional|keyword-only) arguments?|multiple values for argument)")
# A file that is not there, as Python and a shell name it.
NO_SUCH_FILE = re.compile(r"(?:No such file or directory|can't open file)[^'\"]*['\"]([^'\"]+)['\"]")
GO_BUILD_FAILED = re.compile(r"\[build failed\]|\[setup failed\]")
ERROR_KIND = re.compile(r"^(?:E\s+)?([A-Za-z_][\w.]*(?:Error|Exception|Exit|Interrupt|Warning))\b")
# The line pytest prints for each test with -rA, the one for a skipped test,
# and the heading and the frames of a failure with --tb=short.
PYTEST_RESULT = re.compile(r"^(PASSED|FAILED|ERROR) ([^\s\[]+)(.*)$")
PYTEST_SKIPPED = re.compile(r"^SKIPPED \[\d+\] ([^:]+):(\d+): (.*)$")
PYTEST_FAILURE = re.compile(r"^_{3,} (.+?) _{3,}$")
FRAME = re.compile(r"^(\S+?):\d+: in ")
ISOLATE_AT_MOST = 6
# Test folders whose packages the job does not install (the lens needs torch).
NOT_INSTALLED = ("geometric-lens/",)


class CannotJudge(Exception):
    """The script could not do its work, so it says nothing about the tests."""


@dataclass(frozen=True)
class Test:
    kind: str  # "python" or "go"
    path: str  # from the root of the repository
    name: str  # a pytest function (with its class) or a Go test function
    line: int  # where it starts, with its decorators
    last: int  # where it ends
    new: bool = True  # the fix adds it; False: the test was there and the fix changes it

    @property
    def label(self) -> str:
        return f"{self.path}::{self.name}"


@dataclass(frozen=True)
class Outcome:
    state: str  # "pass", "fail", "missing", "skip" or "none"
    detail: str = ""


def integrity():
    """The integrity check, for its one definition of a test file and of product code."""
    spec = importlib.util.spec_from_file_location("atlas_integrity_check", Path(__file__).with_name("integrity_check.py"))
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


def git(root: Path, *args: str) -> str:
    done = subprocess.run(["git", "-C", str(root), *args], capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise CannotJudge(f"`git {' '.join(args)}` failed: {done.stderr.strip()} Fix: run this in a checkout that "
                          "holds the base commit (fetch it, or check out with enough history).")
    return done.stdout


def changed_lines(root: Path, base: str) -> dict[str, set[int]]:
    """For each file that the change adds or edits: the lines of the new file that it touches."""
    touched: dict[str, set[int]] = {}
    path = ""
    for line in git(root, "diff", "--no-color", "--unified=0", "--no-renames", "--end-of-options", base, "HEAD", "--").splitlines():
        if line.startswith("+++ "):
            path = line[6:] if line.startswith("+++ b/") else ""
            if path:
                touched.setdefault(path, set())
        hunk = HUNK.match(line)
        if hunk and path:
            start, count = int(hunk.group(1)), int(hunk.group(2) or "1")
            # A hunk that only removes lines touches the place where they were.
            touched[path] |= set(range(start, start + count)) if count else {start, start + 1}
    return touched


def python_tests(path: str, source: str) -> list[Test]:
    """Every test function of a pytest file."""
    try:
        tree = ast.parse(source)
    except SyntaxError:
        return []
    found = []

    def walk(node, prefix: str) -> None:
        for child in ast.iter_child_nodes(node):
            if isinstance(child, ast.ClassDef) and child.name.startswith("Test"):
                walk(child, f"{prefix}{child.name}::")
            elif isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)) and child.name.startswith("test"):
                first = min([child.lineno] + [d.lineno for d in child.decorator_list])
                found.append(Test("python", path, prefix + child.name, first, child.end_lineno))
    walk(tree, "")
    return found


def go_tests(path: str, source: str) -> list[Test]:
    """Every test function of a Go test file."""
    found, start, name = [], 0, ""
    for number, line in enumerate(source.split("\n"), 1):
        match = GO_TEST.match(line)
        if match:
            start, name = number, match.group(1)
        if name and (line == "}" or (match and line.rstrip().endswith("}"))):
            found.append(Test("go", path, name, start, number))
            name = ""
    return found


def tests_of(path: str, source: str) -> list[Test] | None:
    """The tests of a file this check can run, or None for a file it does not read."""
    if path.endswith("_test.go"):
        return go_tests(path, source)
    if path.endswith(".py") and (Path(path).name.startswith("test_") or path.endswith("_test.py")):
        return python_tests(path, source)
    return None


def changed_tests(root: Path, base: str, touched: dict[str, set[int]], is_test) -> tuple[list[Test], list[str]]:
    """The tests the change adds or changes, and the changed test files this cannot judge."""
    tests, not_judged = [], []
    for path, lines in sorted(touched.items()):
        if not is_test(path) or not (root / path).is_file():
            continue
        found = tests_of(path, (root / path).read_text(encoding="utf-8", errors="replace"))
        if path.startswith(NOT_INSTALLED):
            not_judged.append(f"{path}: the packages its tests need are not installed in this job")
        elif found is None:
            if re.search(r"\.test\.[cm]?[jt]sx?$", path):
                not_judged.append(f"{path}: a TypeScript or JavaScript test file; this check does not read those yet")
        else:
            before = subprocess.run(["git", "-C", str(root), "show", f"{base}:{path}"], capture_output=True, text=True,
                                    check=False)
            was_there = {t.name for t in tests_of(path, before.stdout) or []} if before.returncode == 0 else set()
            tests += [Test(t.kind, t.path, t.name, t.line, t.last, t.name not in was_there) for t in found
                      if lines & set(range(t.line, t.last + 1))]
    return tests, not_judged


def is_test_material(path: str, is_test) -> bool:
    return is_test(path) or "/testdata/" in f"/{path}" or "/fixtures/" in f"/{path}"


def lay_over(root: Path, tree: Path, paths: list[str]) -> None:
    """Put the pull request's version of these files into the base tree."""
    for path in paths:
        target = tree / path
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(root / path, target)


def run(command: list[str], cwd: Path, limit: int, more_env: dict | None = None) -> tuple[int | None, str]:
    """The exit status and the output of a command, or None when it did not end in time."""
    try:
        done = subprocess.run(command, cwd=cwd, capture_output=True, text=True, timeout=limit, check=False,
                              env={**os.environ, **(more_env or {})})
    except subprocess.TimeoutExpired:
        return None, ""
    except OSError as error:
        raise CannotJudge(f"`{command[0]}` did not start: {error}. Fix: install it, or pass its path.") from error
    return done.returncode, done.stdout + done.stderr


def first_line(text: str) -> str:
    return next((line.strip() for line in text.splitlines() if line.strip()), "")[:160]


def failure(text: str, where: str = "", added: tuple | list = ()) -> Outcome:
    """A failed test: it cannot run as written on this code, or it fails for what the code does.

    `where` says where the error was raised: in the test, in product code, or
    in a file outside the repository.
    `added` holds the files the fix adds: a test that stops because one of
    them is not there cannot run as written on the base.
    """
    cannot = next((line.strip() for line in text.splitlines() if CANNOT_RUN.search(line)), "")
    if cannot:
        return Outcome("missing", cannot.removeprefix("E ").strip()[:160])
    brought = next((path for named in NO_SUCH_FILE.findall(text) for path in added
                    if named == path or named.endswith("/" + path)), "")
    if brought:
        return Outcome("missing", f"the base has no {brought}, a file the fix adds")
    message = first_line(text).removeprefix("E ").strip()
    named = ERROR_KIND.match(message)
    if re.match(r"assert\b|AssertionError", message) or "--- FAIL" in text and not named:
        kind = "an assertion"
    else:
        kind = f"an error ({named.group(1)})" if named else "an error"
    return Outcome("fail", f"{kind}{', raised ' + where if where else ''}: {message}"[:220])


def place(frames: list[str], is_test) -> str:
    """Where an error was raised, from the files of its traceback in the order pytest prints them.

    pytest names a file of the repository from the folder it runs in, and
    any other file by its whole path. A file outside the repository is the
    standard library or an installed package, and not product code: it is
    named as such, with the last file of the repository that called it.
    """
    last = frames[-1]
    if not os.path.isabs(last):
        return "in the test" if is_test(last) else f"in product code ({last})"
    kind = "an installed package" if "-packages/" in last else "the standard library"
    inside = [path for path in frames if not os.path.isabs(path)]
    caller = ("the test" if is_test(inside[-1]) else inside[-1]) if inside else ""
    return f"in {kind} ({Path(last).name})" + (f", called from {caller}" if caller else "")


def raised_where(output: str, is_test) -> dict[str, str]:
    """For each failed pytest function: where its error was raised (see `place`)."""
    frames: dict[str, list[str]] = {}
    name = ""
    for line in output.splitlines():
        heading = PYTEST_FAILURE.match(line)
        if heading:
            name = heading.group(1).split("[")[0].split(".")[-1].split(" ")[-1]
        frame = FRAME.match(line)
        if frame and name:
            frames.setdefault(name, []).append(frame.group(1))
    return {name: place(files, is_test) for name, files in frames.items()}


def run_python(tree: Path, tests: list[Test], python: str, limit: int, is_test,
               added: tuple | list = ()) -> dict[Test, Outcome]:
    """The outcome of each pytest function, read from the lines pytest prints for each test with -rA."""
    if not tests:
        return {}
    nodes = [f"{t.path}::{t.name}" for t in tests]
    status, output = run([python, "-m", "pytest", "-q", "-p", "no:cacheprovider", "-rA", "--tb=short", *nodes], tree,
                         limit, {"COLUMNS": "400"})
    if status is None:
        return {t: Outcome("none", f"no result in {limit} s") for t in tests}
    results: dict[str, list[tuple[str, str]]] = {}
    skips: list[tuple[str, int, str]] = []
    for line in output.splitlines():
        found, skipped = PYTEST_RESULT.match(line), PYTEST_SKIPPED.match(line)
        if found:
            # What follows the name: the case of a parametrized test in brackets, then " - " and the message.
            rest = found.group(3)
            mark = "] - " if rest.startswith("[") else " - "
            message = rest.split(mark, 1)[1] if mark in rest else ""
            results.setdefault(found.group(2), []).append((found.group(1), message))
        elif skipped:
            skips.append((skipped.group(1), int(skipped.group(2)), skipped.group(3)))
    where = raised_where(output, is_test)
    # The first line pytest marks as the error itself, for a file it could not collect.
    stopped = next((line[1:].strip() for line in output.splitlines() if line.startswith("E ")), "")
    out = {}
    for test, node in zip(tests, nodes):
        mine = results.get(node, [])
        bad = [text for state, text in mine if state != "PASSED"]
        skip = next((reason for path, line, reason in skips if path == test.path and test.line <= line <= test.last), None)
        if test.path in results:
            # pytest could not even collect the file: its imports or its module code stopped.
            out[test] = Outcome("missing", (results[test.path][0][1] or stopped)[:160])
        elif bad:
            out[test] = failure(bad[0], where.get(test.name.split("::")[-1], ""), added)
        elif mine:
            out[test] = Outcome("pass")
        elif skip is not None:
            out[test] = Outcome("skip", f"skipped ({skip})")
        else:
            out[test] = failure(output) if status not in (0, 5) else Outcome("none", "pytest did not run this test")
    return out


def go_module(root: Path, path: str) -> tuple[Path, str]:
    """The module folder of a Go file, and the file's package as `go test` takes it."""
    folder = (root / path).parent
    module = folder
    while module != root and not (module / "go.mod").is_file():
        module = module.parent
    relative = folder.relative_to(module).as_posix()
    return module, "." if relative == "." else f"./{relative}"


def go_failure(lines: list[str]) -> Outcome:
    """A failed Go test: by a check of the test itself, or by a panic in the code it runs."""
    text = "".join(lines)
    if "panic:" not in text:
        said = next((line.split(": ", 1)[1].strip() for line in lines if re.match(r"\s+\S+_test\.go:\d+: ", line)), "")
        none = "the test reported a failure, and its output has no message"
        return Outcome("fail", f"an assertion, raised in the test: {said or none}"[:220])
    frames = [m.group(1) for m in re.finditer(r"^\s+(\S+\.go):\d+", text, re.MULTILINE)
              if "/runtime/" not in m.group(1) and "/testing/" not in m.group(1)]
    place = Path(frames[0]).name if frames else ""
    where = "in the test" if place.endswith("_test.go") else f"in product code ({place})" if place else ""
    panic = next(line.strip() for line in text.splitlines() if "panic:" in line)
    return Outcome("fail", f"an error (panic){', raised ' + where if where else ''}: {panic}"[:220])


def run_go(tree: Path, tests: list[Test], go: str, limit: int) -> dict[Test, Outcome]:
    out: dict[Test, Outcome] = {}
    packages: dict[tuple[Path, str], list[Test]] = {}
    for test in tests:
        packages.setdefault(go_module(tree, test.path), []).append(test)
    for (module, package), group in sorted(packages.items(), key=lambda item: str(item[0])):
        pattern = "^(" + "|".join(sorted({t.name for t in group})) + ")$"
        status, output = run([go, "test", "-json", "-count=1", "-run", pattern, package], module, limit)
        if status is None:
            out.update({t: Outcome("none", f"no result in {limit} s") for t in group})
            continue
        ended, said, by_test = {}, [], {}
        for line in output.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                event = None
            if not isinstance(event, dict):
                said.append(line + "\n")
            elif event.get("Action") in ("output", "build-output"):
                (by_test.setdefault(event["Test"], []) if event.get("Test") else said).append(event.get("Output", ""))
            elif event.get("Test") and event.get("Action") in ("pass", "fail", "skip"):
                ended[event["Test"]] = event["Action"]
        for test in group:
            # A check that fails inside `t.Run` is reported under the subtest's name: its lines are the test's too.
            lines = [line for name, printed in by_test.items() if name == test.name or name.startswith(test.name + "/")
                     for line in printed]
            out[test] = go_outcome(ended.get(test.name), lines, said, status)
    return out


def go_outcome(state: str | None, lines: list[str], said: list[str], status: int) -> Outcome:
    if state == "pass":
        return Outcome("pass")
    if state == "skip":
        reason = next((line.split(": ", 1)[1].strip() for line in lines if re.match(r"\s+\S+\.go:\d+: ", line)), "")
        return Outcome("skip", f"skipped ({reason})" if reason else "skipped")
    if state == "fail":
        return go_failure(lines)
    package = "".join(said)
    if status and GO_BUILD_FAILED.search(package):
        compiler = next((line.strip() for line in package.splitlines() if re.match(r"\S+\.go:\d+:\d+: ", line.strip())), "")
        return Outcome("missing", (compiler or first_line(package))[:160])
    return Outcome("fail", f"an error: {first_line(package)}") if status else Outcome("none", "go test did not run this test")


def run_tests(tree: Path, tests: list[Test], tools: dict, limit: int) -> dict[Test, Outcome]:
    return {**run_python(tree, [t for t in tests if t.kind == "python"], tools["python"], limit, tools["is_test"],
                         tools.get("added", ())),
            **run_go(tree, [t for t in tests if t.kind == "go"], tools["go"], limit)}


def one_file_at_a_time(root: Path, base_tree: Path, material: list[str], tests: list[Test], on_base: dict,
                       tools: dict, limit: int) -> tuple[dict[Test, Outcome], list[str]]:
    """Judge again, one changed test file at a time, the Go tests that did not build with all of them laid over.

    Go builds the tests of a package together, so one changed file that uses
    code the fix adds stops every test of the package. A changed file that
    uses only old code can still be judged when it is laid over alone.
    """
    stuck = sorted({t.path for t in tests if t.kind == "go" and on_base[t].state == "missing"})
    if len(stuck) < 2:
        return on_base, []
    if len(stuck) > ISOLATE_AT_MOST:
        return on_base, [(f"{len(stuck)} changed Go test files did not build together on the base; they were not "
                          f"tried one by one (the limit is {ISOLATE_AT_MOST})")]
    out = dict(on_base)
    for path in stuck:
        git(base_tree, "checkout", "--quiet", "--", ".")
        git(base_tree, "clean", "--quiet", "-fd")
        lay_over(root, base_tree, [path] + [m for m in material if not m.endswith("_test.go")])
        out.update(run_go(base_tree, [t for t in tests if t.path == path], tools["go"], limit))
    return out, []


VERDICTS = {
    ("fail", "pass"): ("fails on the base", "it guards the fix"),
    ("missing", "pass"): ("needs the fix's code", "on the base the test cannot run as written"),
    ("pass", "pass"): ("passes on the base too", "it does not guard the fix"),
}


def verdict(base: Outcome, head: Outcome) -> tuple[str, str]:
    """The class of a test and what to say with it."""
    if head.state in ("fail", "missing"):
        if base.state == "pass":
            return "fails with the fix", head.detail
        # The first line of the failure shows at once when the cause is the place the test runs in.
        return "not judged here", f"it fails on the base and with the fix: {head.detail}"
    if "none" in (base.state, head.state) or "skip" in (base.state, head.state):
        return "not judged here", (base if base.state in ("none", "skip") else head).detail
    name, text = VERDICTS[(base.state, head.state)]
    return name, f"{text}: {base.detail}" if base.detail else text


def judge(root: Path, base: str, tools: dict, limit: int) -> tuple[list[tuple[Test, str, str]], list[str], str]:
    """A verdict for each new or changed test, what could not be judged, and what the fix changes (a key of CHANGES)."""
    check = integrity()
    touched = changed_lines(root, base)
    added = git(root, "diff", "--name-only", "--diff-filter=A", "--no-renames", "--end-of-options", base, "HEAD", "--")
    tools = {**tools, "is_test": check.is_test, "added": added.split()}
    tests, notes = changed_tests(root, base, touched, check.is_test)
    product = changes(touched, check)
    if not tests:
        return [], notes, product
    material = [path for path in touched if is_test_material(path, check.is_test) and (root / path).is_file()]
    with tempfile.TemporaryDirectory() as tmp:
        base_tree = Path(tmp) / "base"
        git(root, "worktree", "add", "--quiet", "--detach", str(base_tree), base)
        try:
            lay_over(root, base_tree, material)
            on_base = run_tests(base_tree, tests, tools, limit)
            on_base, more = one_file_at_a_time(root, base_tree, material, tests, on_base, tools, limit)
        finally:
            git(root, "worktree", "remove", "--force", str(base_tree))
    at_head = run_tests(root, tests, tools, limit)
    return [(test, *verdict(on_base[test], at_head[test])) for test in tests], notes + more, product


CLASSES = ("fails on the base", "needs the fix's code", "passes on the base too", "not judged here",
           "fails with the fix")
# What a fix changes, for the first words of its line: code of the product, a
# file of a product folder that is not code (a Dockerfile, a settings file),
# or nothing in a product folder.
CHANGES = {"code": "a fix of product code",
           "folder": "a fix in a product folder that changes no code file there (a build or settings file)",
           "outside": "a fix outside product code (CI, docs or scripts)"}


def changes(touched, check) -> str:
    """Which of CHANGES a fix is, by the files it touches."""
    if any(check.is_product(path) for path in touched):
        return "code"
    in_a_product_folder = any(path.startswith(check.PRODUCT_DIRS) and not check.is_test(path) for path in touched)
    return "folder" if in_a_product_folder else "outside"


def is_finding(test: Test, name: str, guarded: bool) -> bool:
    """The one finding: a test the fix adds passes without the fix, and no test of the fix fails without it.

    Beside a test that fails on the base, a new test that passes there is
    most likely a control: it is listed for information.
    """
    return test.new and name == "passes on the base too" and not guarded


def counts(rows: list[tuple[Test, str, str]], new: bool) -> str:
    mine = [name for test, name, _ in rows if test.new == new]
    said = "; ".join(f"{mine.count(name)} {name}" for name in CLASSES if mine.count(name))
    return f"{len(mine)} {'new' if new else 'changed'} test(s): {said}." if mine else ""


def headline(rows: list[tuple[Test, str, str]], product: str) -> str:
    """One line for the fix as a whole: guarded, not shown, cannot tell, or no test. The strongest that holds.

    "Not shown" is a finding about the fix: its tests ran on the base and
    none failed. "Cannot tell" is the limit of this check: no test could run
    on the base, so nothing is known either way.
    """
    kind = CHANGES[product]
    names = [name for _, name, _ in rows]
    guards, needs, ran = (names.count(name) for name in CLASSES[:3])
    if not rows:
        return f"fix tests: {kind}. No test: this fix adds or changes no test, so there is nothing to run."
    if guards:
        return f"fix tests: {kind}. Guarded: {guards} test(s) fail on the base."
    if ran:
        more = f" {needs} more need code or a file the fix adds." if needs else ""
        return f"fix tests: {kind}. Not shown: {ran} test(s) ran on the base and none failed.{more}"
    if needs == len(rows):
        return (f"fix tests: {kind}. Cannot tell: every test needs code or a file the fix adds, so none could run on "
                "the base.")
    return f"fix tests: {kind}. Cannot tell: no test of the fix could be judged on the base ({needs} need the fix's code)."


def summary(rows: list[tuple[Test, str, str]], notes: list[str], product: str) -> list[str]:
    lines = [headline(rows, product)]
    if not rows:
        return lines + [f"- not judged: {note}" for note in notes]
    guarded = any(name == "fails on the base" for _, name, _ in rows)
    passing = sum(1 for test, name, _ in rows if test.new and name == "passes on the base too")
    if passing and guarded:
        lines.append(f"For information: {passing} new test(s) pass on the base too. Beside a test that fails on the base "
                     "they are most likely controls.")
    elif passing:
        lines.append(f"{passing} new test(s) pass on the base too: a control, or a test that does not test the fix.")
    lines += [" ".join(filter(None, (counts(rows, True), counts(rows, False)))),
              "", "| Test | New or changed | It | Detail |", "|---|---|---|---|"]
    lines += [f"| `{test.label}` | {'new' if test.new else 'changed'} | {name} | {detail.replace('|', '/')} |"
              for test, name, detail in rows]
    return lines + [f"- not judged: {note}" for note in notes]


def report(rows: list[tuple[Test, str, str]], notes: list[str], product: str, github: bool) -> None:
    lines = summary(rows, notes, product)
    print("\n".join(lines))
    if not github:
        return
    guarded = any(name == "fails on the base" for _, name, _ in rows)
    for test, name, _ in rows:
        if is_finding(test, name, guarded):
            print(f"::warning file={test.path},line={test.line},title=this new test passes without the fix::"
                  f"{test.name} is new and passes on the base's code too, so it does not guard the fix. Fix: make "
                  "the test fail on the fault the pull request fixes. If it is a control that holds what the fix "
                  "must not change, say so in the pull request.")
    if not rows:
        print(f"::notice title=this fix adds or changes no test::{lines[0]}")
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as out:
            out.write("\n".join(lines) + "\n")


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--base", required=True, help="the commit the pull request is compared with")
    parser.add_argument("--title", required=True, help="the title of the pull request")
    parser.add_argument("--root", type=Path, default=ROOT, help="the checkout of the pull request")
    parser.add_argument("--time-limit", type=int, default=900, help="seconds for one run of the tests")
    parser.add_argument("--github", action="store_true", help="also write annotations and the job summary")
    args = parser.parse_args(argv)
    if not FIX_TITLE.match(args.title):
        print("fix tests: nothing to check: the title of this pull request does not have the type `fix`.")
        return 0
    try:
        rows, notes, product = judge(args.root, args.base, {"python": sys.executable, "go": "go"}, args.time_limit)
    except CannotJudge as error:
        print(f"FAIL fix tests: {error}", file=sys.stderr)
        return 2
    report(rows, notes, product, args.github)
    return 0


if __name__ == "__main__":
    sys.exit(main())
