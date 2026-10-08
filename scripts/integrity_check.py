#!/usr/bin/env python3
"""Check a change for the ways it can weaken the project's own checks.

Reads the diff between a base commit and HEAD and reports:
  - tests that were deleted, skipped or loosened (a skip with its reason, and
    a call to a function that skips, are named with the reason; a test whose
    name line changed and whose body still runs is listed, not judged);
  - history markers in new comments (dates, commit IDs, run names);
  - names of evaluation tasks in product code;
  - new documentation files;
  - changes to the files that configure the checks, and to the scripts a
    workflow runs with a credential that can write;
  - new suppression markers, each read in the kind of file its linter reads,
    and workflow settings that turn a guard off;
  - new dependencies, large changes, and a size baseline that only went down
    (listed, not judged).

Each finding says what was found, why it matters and what to do about it. The
script reports and exits 0. With --strict it exits 1 when a finding needs
action or approval.
"""

from __future__ import annotations

import argparse
import ast
import importlib.util
import io
import json
import os
import re
import subprocess
import sys
import tokenize
from collections import Counter
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TASK_SOURCE = Path("scripts") / "e2e-reliability.py"

CODE_EXTENSIONS = (".go", ".py", ".ts", ".js", ".mjs", ".sh")
PRODUCT_DIRS = ("proxy/", "tui/", "atlas/", "v3-service/", "geometric-lens/",
                "sandbox/", "inference/", "extensions/vscode/src/")
DOC_DIRS_ALLOWED = (".github/",)
# Files that decide what a check accepts: workflows, settings, baselines,
# expected values, and the scripts whose result is a check's result.
GATE_FILES = (
    ".github/workflows/", ".github/actions/", ".github/requirements/", ".github/code-health-baseline.json",
    ".github/canary.json",
    ".golangci.yml", ".golangci.yaml", ".codescene/", "codecov.yml",
    ".sonarcloud.properties", "sonar-project.properties", "pyproject.toml",
    "extensions/vscode/eslint.config.mjs", "tests/perf/budgets.json", "tests/replay/recordings/",
    "scripts/integrity_check.py", "scripts/code_health.py", "scripts/production-readiness.py",
    "scripts/checks_ran.py", "scripts/change_base.py", "scripts/queue_run.py", "scripts/go_test_build.py",
    "scripts/verify.py", "scripts/canary.py",
    "scripts/dockerfile_lint.py", "scripts/check_dockerfile_sources.py", "scripts/check_min_python.py",
    "scripts/staging-integration.py",
    "scripts/ci-lock.sh", "scripts/release-tag.sh", "scripts/setup/environments.sh", "scripts/setup/rulesets.sh",
    # The settings file of each linter of the pipeline, should one appear.
    ".hadolint.yaml", ".hadolint.yml", ".github/zizmor.yml", "zizmor.yml", ".github/actionlint.yaml",
    ".github/actionlint.yml", ".yamllint", ".yamllint.yml", ".yamllint.yaml", ".shellcheckrc", "ruff.toml",
    ".ruff.toml", "staticcheck.conf", ".trivyignore", ".github/codeql/", ".github/codeql-config.yml",
    "codeql-config.yml", ".github/dependency-review-config.yml",
)
# Scripts that a workflow runs with a credential that can write: a token with
# a write permission, an app token, a right to publish.
WRITE_CREDENTIAL_SCRIPTS = ("scripts/bot/", "scripts/star-history-chart.py", "scripts/weekly_cleanup.py")
# Scripts that a workflow runs and that are neither: the thing under test, in
# a job that cannot write. Each with its reason.
RUN_NOT_GATE = {
    "scripts/atlas-bootstrap.sh": "the installer that the bootstrap jobs test; those jobs have a read-only token",
}
# Each linter of the pipeline and the kinds of file its markers are read in
# (keys of MARKERS). "none": the linter has no marker to write into a file.
LINTER_MARKERS = {
    "golangci-lint": ("go",), "staticcheck": ("go",), "ruff": ("python",), "mypy": ("python",),
    "shellcheck": ("shell",), "yamllint": ("workflow",), "zizmor": ("workflow",), "hadolint": ("dockerfile",),
    "ESLint": ("script",), "vitest coverage": ("script",),
    "SonarQube": ("go", "python", "script", "shell", "workflow", "dockerfile"),
    "actionlint": "none", "CodeScene": "none", "Codecov": "none", "Trivy": "none", "CodeQL": "none",
}
LOCK_FILES = ("package-lock.json", "go.sum", ".github/requirements/ci.txt")
SIZE_BASELINE = ".github/code-health-baseline.json"
# Where the test runner's own settings are: which marks a plain run leaves out.
RUNNER_SETTINGS = "pyproject.toml"
LARGE_CHANGE_LINES = 400

TEST_DEF = re.compile(r"^\s*(?:func (Test\w+)\(|(?:async )?def (test_\w+)\("
                      r"|(?:it|test)(?:\.\w+)*(?:\((?:[^()]|\([^()]*\))*\))?\(\s*['\"`](.+?)['\"`])")
# The forms by which a test stops running, for each runner. Each row: the
# pattern of a new line, and how far the form reaches: "one" (the test it
# stands on or in), "named" (a skip mark kept under a name of its own; its
# uses are counted apart), "class" (every test of the class), "group" (every
# test of the group), "file" (every test of the file), "others" (every other
# test of the file), "files" (whole test files), "hook" (the tests a hook
# picks). The first row that fits a line decides, so a form at the start of
# the line (the whole file) stands above the same form further in. A form is
# a statement or a decorator, so most patterns start at the start of the
# line; the same words inside a string are data.
SKIP_MARK = r"pytest\.mark\.(?:skip|skipif|xfail)\b"
STOPS = {
    "python": (
        (r"^pytestmark\s*=.*\b(?:skip|skipif|xfail)\b", "file"),
        (rf"^\s*\w+\s*=\s*{SKIP_MARK}", "named"),
        (rf"^\s*@{SKIP_MARK}", "one"),
        (r"^pytest\.skip\(", "file"),
        (r"^\s*pytest\.(?:skip|xfail)\(", "one"),
        (rf"\bmarks\s*=\s*[\[(]?\s*{SKIP_MARK}", "one"),
        (r"^(?:\w+\s*=\s*)?pytest\.importorskip\(", "file"),
        (r"^\s+(?:\w+\s*=\s*)?pytest\.importorskip\(", "one"),
        (r"^__test__\s*=\s*False\b", "file"),
        (r"^\s+__test__\s*=\s*False\b", "class"),
        (r"^collect_ignore(?:_glob)?\b", "files"),
        (r"^(?:async\s+)?def pytest_ignore_collect\(", "files"),
        (rf"\.add_marker\(\s*{SKIP_MARK}", "hook"),
        (r"^\s*@?unittest\.(?:skip|skipIf|skipUnless|expectedFailure)\b", "one"),
        (r"^\s*self\.skipTest\(", "one"),
        (r"^raise\s+(?:unittest\.)?SkipTest\b", "file"),
        (r"^\s+raise\s+(?:unittest\.)?SkipTest\b", "one"),
    ),
    "go": (
        (r"^\s*(?:t|tb)\.Skip(?:f|Now)?\(", "one"),
        (r"^//go:build\b|^// \+build\b", "file"),
    ),
    "script": (
        (r"^\s*(?:it|test|describe)(?:\.\w+)*\.only\b", "others"),
        (r"^\s*describe(?:\.\w+)*\.(?:skip|todo|skipIf|runIf)\b|^\s*xdescribe\(", "group"),
        (r"^\s*(?:it|test)(?:\.\w+)*\.(?:skip|todo|fails)\b", "one"),
        (r"^\s*(?:it|test)(?:\.\w+)*\.(?:skipIf|runIf)\(", "one"),
        (r"^\s*x(?:it|test)\(", "one"),
    ),
}
STOPS = {kind: tuple((re.compile(pattern), reach) for pattern, reach in rows) for kind, rows in STOPS.items()}
# What a form that reaches further than one test says of itself.
REACH = {
    "class": "every test of this class stops running",
    "group": "every test of this group stops running",
    "file": "every test of this file stops running",
    "others": "every other test of this file stops running",
    "files": "whole test files are left out",
    "hook": "the tests the hook picks stop running",
}
NAMED_MARK = re.compile(rf"^\s*(\w+)\s*=\s*{SKIP_MARK}")
# `importorskip` skips when a module is not there: the module it names is its reason.
IMPORT_OR_SKIP = re.compile(r"\bimportorskip\(\s*['\"]([\w.]+)['\"]")
# The line that a decorator stands on: a function or a class.
DECORATED = re.compile(r"^\s*(?:(?:async\s+)?def|class)\s")
ASSERTION = re.compile(r"(?:^\s*assert\b)|\bt\.(?:Error|Errorf|Fatal|Fatalf)\(|\b(?:require|assert)\.\w+\(|\bexpect\(")
# The line that starts a function, in the languages the tests are written in.
DEFINITION = re.compile(r"^\s*(?:(?:async\s+)?def|func|function)\s+(?:\([^)]*\)\s*)?(\w+)\s*[(\[]")
# What a skip says about itself: `reason=`, or the message of a call that takes one.
SKIP_REASON = re.compile(r"(?:\breason\s*=\s*|(?:\bt\.Skipf?\(|\bpytest\.(?:skip|xfail)\(|\bunittest\.skip\("
                         r"|\bself\.skipTest\(|\bSkipTest\()\s*(?!\w+\s*=))([^\s),][^),]*)")
# The suppression markers of each kind of file. A marker counts in a comment
# of a file of its kind; the same words in another kind of file are text.
MARKERS = {
    "go": (r"\bnolint\b", r"\blint:ignore\b", r"\blint:file-ignore\b", r"\bNOSONAR\b", r"\bnosec\b"),
    "python": (r"\bnoqa\b", r"type:\s*ignore", r"\bnosec\b", r"pragma:\s*no cover", r"\bNOSONAR\b"),
    "script": (r"eslint-disable", r"@ts-ignore", r"@ts-expect-error", r"\b(?:v8|c8|istanbul) ignore\b",
               r"\bNOSONAR\b"),
    "shell": (r"shellcheck disable=", r"\bNOSONAR\b"),
    "workflow": (r"zizmor:\s*ignore\[", r"yamllint disable", r"\bNOSONAR\b"),
    "dockerfile": (r"hadolint ignore=", r"hadolint global ignore=", r"\bNOSONAR\b"),
}
# Settings of a workflow that turn a guard off without a marker.
WORKFLOW_SETTINGS = (
    (re.compile(r"^\s*(?:-\s+)?continue-on-error:\s*true\b"),
     "continue-on-error: true", "A step or a job with this setting can fail and the job still passes.",
     "Remove it. If the failure must not count, say why on the line above; a maintainer has to approve it."),
    (re.compile(r"^\s*(?:-\s+)?persist-credentials:\s*true\b"),
     "persist-credentials: true",
     "The checkout keeps the job's token in its git settings, where every later step can use it.",
     ("Set it to false. If a later step must push with the token, say why on the line above; a maintainer has "
      "to approve it.")),
)
# An import that has to come after the line that sets the import path, marked
# with the linter's code for exactly that and no other.
IMPORT_AFTER_PATH = re.compile(r"^\s*(?:import\s+\S|from\s+\S+\s+import\s).*#\s*noqa:\s*E402\s*$")
SETS_IMPORT_PATH = re.compile(r"^\s*sys\.path\.(?:insert|append)\(")
HISTORY = (
    ("a date", re.compile(r"\b20\d{2}-\d{2}-\d{2}\b")),
    ("a commit ID", re.compile(r"\b(?=[0-9a-f]*[a-f])(?=[0-9a-f]*\d)[0-9a-f]{7,40}\b")),
    ("a run name", re.compile(r"\b(?:cycle|smoke|block|stabilization)[ -]?\d+\b|\bseed-\d+\b", re.I)),
)
NEW_DEPENDENCY = {
    "go.mod": re.compile(r"^\s*(?:require\s+)?(\S+)\s+v\d"),
    "package.json": re.compile(r"^\s*\"(@?[\w.\-/]+)\":\s*\"[\^~]?\d"),
    "requirements": re.compile(r"^([A-Za-z][\w.\-\[\]]*)\s*(?:[=<>~!]=|$)"),
}


@dataclass(frozen=True)
class Finding:
    level: str  # "approval", "warning" or "note"
    path: str
    line: int  # 0 when the finding is about the whole file
    what: str
    why: str
    fix: str


@dataclass
class FileChange:
    path: str
    status: str = "modified"  # "added", "deleted" or "modified"
    added: list = field(default_factory=list)  # (hunk, line number, text)
    removed: list = field(default_factory=list)  # (hunk, text)


def parse_diff(text: str) -> list[FileChange]:
    """Read `git diff --unified=0` output into one FileChange per file."""
    changes: list[FileChange] = []
    current, hunk, line_no = None, 0, 0
    for raw in text.splitlines():
        if raw.startswith("diff --git "):
            current = FileChange(path=raw.split(" b/", 1)[-1])
            changes.append(current)
        elif current is None:
            continue
        elif raw.startswith("new file mode"):
            current.status = "added"
        elif raw.startswith("deleted file mode"):
            current.status = "deleted"
        elif raw.startswith("+++ b/"):
            current.path = raw[6:]
        elif raw.startswith("@@"):
            match = re.match(r"@@ -\d+(?:,\d+)? \+(\d+)", raw)
            hunk, line_no = hunk + 1, int(match.group(1)) if match else 0
        elif raw.startswith("+") and not raw.startswith("+++"):
            current.added.append((hunk, line_no, raw[1:]))
            line_no += 1
        elif raw.startswith("-") and not raw.startswith("---"):
            current.removed.append((hunk, raw[1:]))
    return changes


@dataclass
class Tree:
    """The files behind a diff: as they are now, and as they were at the base.

    An empty Tree reads nothing. A rule that needs a file it cannot read keeps
    its finding, so a missing file never hides one.
    """
    root: Path | None = None
    base: str = ""  # the commit the change started from

    def read(self, path: str) -> str | None:
        if self.root is None:
            return None
        try:
            return (self.root / path).read_text(encoding="utf-8", errors="replace")
        except OSError:
            return None

    def read_base(self, path: str) -> str | None:
        if self.root is None or not self.base:
            return None
        done = subprocess.run(["git", "show", f"{self.base}:{path}"], cwd=self.root, capture_output=True,
                              text=True, check=False)
        return done.stdout if done.returncode == 0 else None

    def tracked(self) -> list[str] | None:
        """The files that git tracks in the checkout, as paths from the root. None where they cannot be listed."""
        if self.root is None:
            return None
        try:
            done = subprocess.run(["git", "ls-files", "-z"], cwd=self.root, capture_output=True, text=True, check=False)
        except OSError:
            return None
        return [path for path in done.stdout.split("\0") if path] if done.returncode == 0 else None

    def tests_beside(self, path: str) -> list[str]:
        """The test files in the folder of `path`, as paths from the root."""
        if self.root is None:
            return []
        folder = path.rsplit("/", 1)[0] + "/" if "/" in path else ""
        try:
            names = sorted(entry.name for entry in (self.root / folder).iterdir() if entry.is_file())
        except OSError:
            return []
        return [folder + name for name in names if is_test(folder + name) or name == "conftest.py"]


def file_kind(path: str) -> str:
    """Which markers a file can hold: a key of MARKERS, or "" for a file the marker scan does not read."""
    name = path.rsplit("/", 1)[-1]
    if path.startswith(".github/workflows/") and name.endswith((".yml", ".yaml")):
        return "workflow"
    if name == "Dockerfile" or name.startswith("Dockerfile."):
        return "dockerfile"
    for kind, endings in (("go", (".go",)), ("python", (".py",)), ("shell", (".sh",)),
                          ("script", (".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"))):
        if name.endswith(endings):
            return kind
    return ""


def is_test(path: str) -> bool:
    name = path.rsplit("/", 1)[-1]
    return (name.endswith("_test.go") or name.startswith("test_") or name.endswith("_test.py") or name == "conftest.py"
            or ".test." in name or "/tests/" in f"/{path}" or "/test/" in f"/{path}")


def is_product(path: str) -> bool:
    return path.startswith(PRODUCT_DIRS) and path.endswith(CODE_EXTENSIONS) and not is_test(path)


def comment_text(path: str, line: str) -> str:
    """The comment part of a source line, or "" when it has none."""
    stripped = line.strip()
    if file_kind(path) in ("go", "script"):
        if stripped.startswith(("/*", "*")):
            return stripped
        match = re.search(r"(?:^|\s)//(.*)", line)
    else:
        match = None if stripped.startswith("#!") else re.search(r"(?:^|\s)#(.*)", line)
    return match.group(1) if match else ""


def test_name(line: str) -> str:
    """The name of the test a line defines, or "" when it defines none."""
    match = TEST_DEF.match(line)
    return next((group for group in match.groups() if group), "") if match else ""


def task_names(source: Path = ROOT / TASK_SOURCE) -> set[str]:
    """The evaluation suite's task names, read from the runner that defines them.

    The runner builds some names in a loop, so it is loaded, not searched.
    """
    spec = importlib.util.spec_from_file_location("atlas_task_source", source)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module  # its dataclasses resolve annotations through it
    try:
        spec.loader.exec_module(module)
        return set(module.TASKS)
    finally:
        sys.modules.pop(spec.name, None)


TEST_WHY = ("A test that is removed, skipped or loosened in the same change as the code it "
            "covers can hide a regression.")


def check_tests(changes: list[FileChange], tree: Tree) -> list[Finding]:
    added_names = {test_name(text) for c in changes for _, _, text in c.added} - {""}
    product_changed = any(is_product(c.path) for c in changes)
    out = []
    for c in changes:
        if not is_test(c.path):
            continue
        if c.status == "deleted":
            out.append(Finding("warning", c.path, 0, "test file deleted", TEST_WHY,
                               "Keep the file, or say in the pull request why its tests no longer apply."))
        else:
            out += (removed_tests(c, added_names, tree) + new_skips(c, tree) + skip_helper_calls(c, tree)
                    + named_mark_uses(c, tree) + changed_assertions(c, product_changed))
    return out + left_out_tests(changes, tree)


def removed_tests(c: FileChange, added_names: set[str], tree: Tree) -> list[Finding]:
    """One finding for each test whose name is gone from the change: removed, or only renamed."""
    gone = {}
    for hunk, text in c.removed:
        name = test_name(text)
        if name and name not in added_names:
            gone.setdefault(name, hunk)
    return [renamed_test(c, hunk, name, tree)
            or Finding("warning", c.path, 0, f"test removed: {name}", TEST_WHY,
                       "Keep the test, or say in the pull request why it no longer applies.")
            for name, hunk in sorted(gone.items())]


def tests_that_call(helper: str, c: FileChange, tree: Tree) -> set[str]:
    """The tests of this file that the change adds or edits and that call `helper` on a new line.

    The test a line belongs to is the nearest line above it that starts a
    function with less indentation. It is read from the file; when the file
    cannot be read, no test is known to call the helper.
    """
    source = tree.read(c.path)
    if source is None:
        return set()
    lines = source.splitlines()
    call, found = re.compile(rf"\b{re.escape(helper)}\("), set()
    for _, line_no, text in c.added:
        if not call.search(text) or DEFINITION.match(text) or line_no > len(lines):
            continue
        indent = len(text) - len(text.lstrip())
        for above in reversed(lines[:line_no - 1]):
            starts = test_name(above) or DEFINITION.match(above)
            if starts and len(above) - len(above.lstrip()) < indent:
                found |= {test_name(above)} - {""}
                break
    return found


def renamed_test(c: FileChange, hunk: int, name: str, tree: Tree) -> Finding | None:
    """A note when only the name line of a test went and its body still runs; else None.

    The body still runs when it now sits under another name the runner
    collects, or under a helper that a test of the change calls. A test under
    a name the runner does not collect, with no test that calls it, is a
    removed test. So is a test that loses a decorator with its name line: a
    decorator decides how the test runs, and with which cases.
    """
    added_back = {text.strip() for h, _, text in c.added if h == hunk and text.strip().startswith("@")}
    body_removed = [text for h, text in c.removed
                    if h == hunk and text.strip() and not test_name(text) and text.strip() not in added_back]
    owner = next((text for h, _, text in reversed(c.added)
                  if h == hunk and (test_name(text) or DEFINITION.match(text))), "")
    if body_removed or not owner:
        return None
    if test_name(owner):
        return Finding("note", c.path, 0, f"test renamed: {name} -> {test_name(owner)}",
                       "Only the name line of the test changed; its body is still in the file.", "Nothing to do.")
    helper = DEFINITION.match(owner).group(1)
    callers = tests_that_call(helper, c, tree)
    if not callers:
        return None
    return Finding("note", c.path, 0,
                   f"test turned into a helper: {name} is now {helper}, which {len(callers)} test(s) call",
                   "The body of the test is still in the file, and tests of this change run it.", "Nothing to do.")


def skip_reason(path: str, lines: dict[int, str], line_no: int) -> str:
    """The reason a skip gives for itself, or "" when it gives none.

    A reason is `reason=` or the message of the call, on the lines of the
    call, or a comment on the line of the skip or on the line directly above
    it. An empty text is no reason. A reason that is a name is read as the
    text which the file gives that name at its top level. `importorskip` with no `reason=` has the
    module it needs as its reason, and a comment above it is not read: that
    comment is about the test. `lines` holds the lines that can be read, by
    line number.
    """
    call, depth = "", 0
    for offset in range(8):
        line = lines.get(line_no + offset)
        if line is None:
            break
        call += " " + line.split(" #")[0]
        depth += line.count("(") - line.count(")")
        if depth <= 0:
            break
    given = SKIP_REASON.search(call)
    written = given.group(1).strip() if given else ""
    reason = written.strip("\"'`").strip()
    if reason:
        # A reason with no quotes is a name: the text that the file gives that name is the reason.
        return reason if written[:1] in "\"'`" else named_text(lines, reason)
    module = IMPORT_OR_SKIP.search(call)
    if module:
        return f"needs the module {module.group(1)}"
    return comment_beside(path, lines, line_no)


def comment_beside(path: str, lines: dict[int, str], line_no: int) -> str:
    """The comment on a line, or on the line directly above it: what a form with no reason of its own says."""
    above = lines.get(line_no - 1, "").strip()
    # A line that is a comment itself (a build tag) is the form, not a reason.
    own = "" if lines.get(line_no, "").lstrip().startswith(("//", "#")) else comment_text(path, lines.get(line_no, ""))
    return (own or (above.lstrip("#/ ") if above.startswith(("#", "//")) else "")).strip()


def skip_finding(path: str, line_no: int, what: str, reason: str) -> Finding:
    if reason:
        return Finding("approval", path, line_no, f"{what}, with its reason: {reason}", TEST_WHY,
                       "A maintainer has to approve the reason. There is nothing else to do.")
    return Finding("warning", path, line_no, what, TEST_WHY,
                   "Make the test run. If it cannot run here, say why next to the skip.")


def as_code(pattern: re.Pattern, line: str) -> bool:
    """Whether the pattern is found on the line outside a string. Inside one, the same words are text."""
    found = pattern.search(line)
    if not found:
        return False
    quote = ""
    for index, char in enumerate(line[:found.start()]):
        if quote:
            quote = "" if char == quote and line[index - 1] != "\\" else quote
        elif char in "\"'`":
            quote = char
    return not quote


def text_lines(path: str, source: str | None) -> set[int]:
    """The lines of a Python file that lie inside a string of more than one line. They are text, not code.

    None when the file cannot be read as Python: then every line counts as code.
    """
    inside: set[int] = set()
    if source is None or file_kind(path) != "python":
        return inside
    starts = []
    try:
        for token in tokenize.generate_tokens(io.StringIO(source).readline):
            name = tokenize.tok_name[token.type]
            if name == "FSTRING_START":
                starts.append(token.start[0])
            elif name == "FSTRING_END" and starts:
                inside.update(range(starts.pop() + 1, token.end[0] + 1))
            elif name == "STRING":
                inside.update(range(token.start[0] + 1, token.end[0] + 1))
    except (tokenize.TokenError, SyntaxError):
        return set()
    return inside


def top_level(source: str | None) -> dict[str, ast.expr] | None:
    """The names a Python file sets outside every function and class, each with the value it is given last.

    None when the text cannot be read as Python.
    """
    try:
        module = ast.parse(source or "")
    except SyntaxError:
        return None
    found: dict[str, ast.expr] = {}

    def read(body: list) -> None:
        for node in body:
            if isinstance(node, ast.Assign):
                found.update({target.id: node.value for target in node.targets if isinstance(target, ast.Name)})
            elif isinstance(node, ast.AnnAssign) and node.value is not None and isinstance(node.target, ast.Name):
                found[node.target.id] = node.value
            elif isinstance(node, (ast.If, ast.Try, ast.With)):
                for part in ("body", "orelse", "finalbody"):
                    read(getattr(node, part, []))
                for handler in getattr(node, "handlers", []):
                    read(handler.body)

    read(module.body)
    return found


def imports_of(source: str | None, path: str) -> dict[str, list[tuple[str, str]]]:
    """What a Python file takes from other modules, by the name the file writes.

    Each name has the places it can come from: (the file of a module, the name
    there), with "" as the name when the written name is the module itself.
    """
    try:
        module = ast.parse(source or "")
    except SyntaxError:
        return {}
    found: dict[str, list[tuple[str, str]]] = {}
    for node in module.body:
        if isinstance(node, ast.Import):
            for alias in node.names:
                found[alias.asname or alias.name] = [(alias.name.replace(".", "/") + ".py", "")]
        elif isinstance(node, ast.ImportFrom):
            folder = path.split("/")[:-1]
            parts = (folder[:len(folder) - node.level + 1] if node.level else []) + (node.module or "").split(".")
            base = "/".join(part for part in parts if part)
            for alias in node.names:
                found[alias.asname or alias.name] = [(base + ".py", alias.name), (base + "/__init__.py", alias.name),
                                                     (f"{base}/{alias.name}.py", "")]
    return found


def names_in_reach(tree: Tree, path: str, names_of_a_file) -> dict:
    """What a Python file can name: what `names_of_a_file` finds at its own top level, and what it imports of that.

    `names_of_a_file(source, path)` gives name -> value for one file. A name in
    a file that this one does not import is not in reach: it is another name.
    """
    source = tree.read(path)
    found = dict(names_of_a_file(source, path))
    for written, places in imports_of(source, path).items():
        for module_path, name in places:
            text = tree.read(module_path)
            if text is None:
                continue
            theirs = names_of_a_file(text, module_path)
            if name and name in theirs:
                found.setdefault(written, theirs[name])
            elif not name:
                found.update({f"{written}.{key}": value for key, value in theirs.items() if f"{written}.{key}" not in found})
            break
    return found


def named_text(lines: dict[int, str], name: str) -> str:
    """The text that a top-level name of the file holds, when `name` is such a name. Else `name` as it is."""
    if not re.fullmatch(r"[A-Za-z_]\w*", name):
        return name
    value = (top_level("\n".join(lines.get(number, "") for number in range(1, max(lines, default=0) + 1))) or {}).get(name)
    return " ".join(value.value.split()) if isinstance(value, ast.Constant) and isinstance(value.value, str) else name


def stop_of(path: str, line: str) -> str:
    """How far a line reaches when it stops tests from running (a key of the rows of STOPS), or "" when it does not."""
    return next((reach for pattern, reach in STOPS.get(file_kind(path), ()) if as_code(pattern, line)), "")


def stop_text(line: str, reach: str) -> str:
    """What a finding says for a new line that stops tests from running."""
    if reach == "one":
        return "new skip or todo marker in a test"
    if reach == "named":
        return f"new skip marker with a name of its own: {NAMED_MARK.match(line).group(1)}"
    head = re.sub(r"^(?:async\s+)?def\s+", "", line.strip().split("(")[0])
    name, _, call = head.partition("=")
    # On a line that gives a name to what `importorskip` returns, the call is the form and not the name.
    form = re.sub(r"\s+", " ", (call if call.strip().endswith("importorskip") else name).strip())[:40]
    return f"new `{form}`: {REACH[reach]}"


def on_a_class(lines: dict[int, str], line_no: int) -> bool:
    """Whether the decorator on this line stands on a class. Then it reaches every test of the class."""
    if not lines.get(line_no, "").lstrip().startswith("@"):
        return False
    below = (lines.get(n, "") for n in range(line_no + 1, line_no + 30))
    return next((line for line in below if DECORATED.match(line)), "").lstrip().startswith("class ")


def new_skips(c: FileChange, tree: Tree) -> list[Finding]:
    added = {line_no: text for _, line_no, text in c.added}
    # The whole file when it can be read, so that a comment on an unchanged line above a new skip is seen.
    source = tree.read(c.path)
    lines = dict(enumerate(source.splitlines(), 1)) if source is not None else added
    out, imports, text_only = [], {}, text_lines(c.path, source)
    for line_no, text in added.items():
        reach = "" if line_no in text_only else stop_of(c.path, text)
        if reach == "one" and on_a_class(lines, line_no):
            reach = "class"
        reason = skip_reason(c.path, lines, line_no) if reach else ""
        if reach == "one" and IMPORT_OR_SKIP.search(text):
            # Many tests of a file often need the same module: one finding for each reason, with the number.
            imports.setdefault(reason, []).append(line_no)
        elif reach:
            out.append(skip_finding(c.path, line_no, stop_text(text, reach), reason))
    return out + [skip_finding(c.path, found[0], f"{len(found)} new call(s) to `pytest.importorskip` in a test", reason)
                  for reason, found in imports.items()]


def skip_helpers(tree: Tree, path: str) -> dict[str, str]:
    """The functions beside `path` that skip the test that calls them: name -> the reason they give."""
    helpers: dict[str, str] = {}
    for other in tree.tests_beside(path):
        lines = dict(enumerate((tree.read(other) or "").splitlines(), 1))
        name, depth = "", 0
        for line_no, line in lines.items():
            indent = len(line) - len(line.lstrip())
            defined = DEFINITION.match(line)
            if defined:
                name, depth = ("" if test_name(line) else defined.group(1)), indent
            elif line.strip() and indent <= depth and not line.lstrip().startswith(("#", "//", ")", "}")):
                # A line no deeper than the `def` line ends the function; a skip from here on is not in it.
                name = ""
            elif name and stop_of(other, line) == "one":
                helpers.setdefault(name, skip_reason(other, lines, line_no))
    return helpers


def skip_helper_calls(c: FileChange, tree: Tree) -> list[Finding]:
    """One finding for each skip helper the change calls from this file, with the number of new calls."""
    out = []
    for name, reason in sorted(skip_helpers(tree, c.path).items()):
        call = re.compile(rf"^\s*(?:[\w, ]+(?::=|=)\s*)?{re.escape(name)}\(")
        lines = [line_no for _, line_no, text in c.added if call.match(text)]
        if lines:
            out.append(skip_finding(c.path, lines[0], f"{len(lines)} new call(s) to the skip helper {name}", reason))
    return out


def skip_marks_of(source: str | None, path: str) -> dict[str, str]:
    """The names a Python file gives to a skip mark at its top level: name -> the reason the mark gives.

    A line inside a function, a class or a string gives no name. Where the
    file cannot be read as Python, a line with no indent that sets such a
    name counts.
    """
    lines = dict(enumerate((source or "").splitlines(), 1))
    values = top_level(source)
    if values is None:
        found = {NAMED_MARK.match(line).group(1): number for number, line in lines.items()
                 if NAMED_MARK.match(line) and not line[:1].isspace()}
    else:
        found = {name: value.lineno for name, value in values.items() if re.match(SKIP_MARK, ast.unparse(value))}
    return {name: skip_reason(path, lines, number) for name, number in found.items() if name != "pytestmark"}


def named_marks(tree: Tree, path: str) -> dict[str, str]:
    """The skip marks with a name of their own that a test file can use: the name as the file writes it -> the reason.

    A name counts when the file sets it at its own top level, or takes it
    from a module by an import. The same name in a file that this one does
    not import is another name.
    """
    return names_in_reach(tree, path, skip_marks_of)


def named_mark_uses(c: FileChange, tree: Tree) -> list[Finding]:
    """One finding for each named skip mark the change puts on tests of this file, with the number of uses."""
    out = []
    source = tree.read(c.path)
    whole_file = dict(enumerate((source or "").splitlines(), 1))
    text_only = text_lines(c.path, source)
    added = [(line_no, text) for _, line_no, text in c.added if line_no not in text_only]
    for name, reason in sorted(named_marks(tree, c.path).items()):
        # `pytest.mark.<name>` is an attribute of pytest's own, and never a use of a name of this file.
        use = re.compile(rf"^\s*@{re.escape(name)}\b(?!\.)|\bmarks\s*=\s*[\[(]?\s*{re.escape(name)}\b(?!\.)")
        whole = re.compile(rf"^pytestmark\s*=(?:.*[^\w.])?{re.escape(name)}\b(?!\.)")
        lines = [line_no for line_no, text in added if as_code(use, text)]
        files = [line_no for line_no, text in added if as_code(whole, text)]
        if lines:
            classes = sum(on_a_class(whole_file, line_no) for line_no in lines)
            on_classes = f", {classes} of them on a whole class" if classes else ""
            out.append(skip_finding(c.path, lines[0], f"{len(lines)} new use(s) of the skip marker {name}{on_classes}",
                                    reason))
        if files:
            out.append(skip_finding(c.path, files[0], f"new `pytestmark` with the skip marker {name}: {REACH['file']}",
                                    reason))
    return out


# --- tests that leave the plain test jobs ---------------------------------------

LEFT_OUT_WHY = ("The plain test jobs do not run a test that carries this mark. A fault that the test would catch can "
                "then pass a pull request.")
LEFT_OUT_FIX = ("Keep the test in the plain jobs. If it needs a running service, say so in a comment next to the mark "
                "and name the job that runs it.")
BORN_LEFT_OUT_FIX = "Check that a job runs it (docs/quality/gates.md, \"Tests that the plain jobs leave out\")."


def left_out_marks(settings: str | None) -> tuple[str, ...]:
    """The marks that the runner's settings leave out of a plain run: each `not <mark>` of `-m` in `addopts`."""
    line = re.search(r"^addopts\s*=\s*(.+)$", settings or "", re.M)
    selection = re.search(r"-m[\"']?[\s,=]*([\"'])(.+?)\1", line.group(1)) if line else None
    return tuple(dict.fromkeys(re.findall(r"\bnot\s+(\w+)", selection.group(2)))) if selection else ()


def mark_in_code(node: ast.AST, marks: tuple[str, ...]) -> str:
    """The first of `marks` that the code of a node names as `mark.<name>`, or "". The same words in a text are not code."""
    for part in ast.walk(node):
        if (isinstance(part, ast.Attribute) and part.attr in marks
                and "mark" in (getattr(part.value, "attr", ""), getattr(part.value, "id", ""))):
            return part.attr
    return ""


def mark_names_of(marks: tuple[str, ...]):
    """A reader for `names_in_reach`: the names a Python file gives, at its top level, to one of `marks`."""
    def of(source: str | None, _path: str) -> dict[str, str]:
        found = {name: mark_in_code(value, marks) for name, value in (top_level(source) or {}).items()}
        return {name: mark for name, mark in found.items() if mark and name != "pytestmark"}
    return of


def mark_among(nodes: list, marks: tuple[str, ...], names: dict[str, str] | None = None) -> tuple[str, int] | None:
    """The first of `marks` that one of the nodes names, with the line of that node.

    A node names a mark as `mark.<name>`, or by a name that stands for it
    (`names`: the name as written -> the mark): the node itself, an item of a
    list, or what a call is given.
    """
    names = names or {}
    for node in nodes:
        mark = mark_in_code(node, marks)
        if mark:
            return mark, node.lineno
        for part in [node, *getattr(node, "elts", []), *getattr(node, "args", [])]:
            written = ast.unparse(part) if isinstance(part, (ast.Name, ast.Attribute)) else ""
            if names.get(written) in marks:
                return names[written], node.lineno
    return None


def tests_and_marks(source: str | None, marks: tuple[str, ...],
                    names: dict[str, str] | None = None) -> tuple[set[str], dict[str, tuple[str, int]]] | None:
    """The tests of a Python file, and those that carry one of `marks`: name -> (the mark, its line).

    A test in a class is named `Class::test`. "*" is the whole file, by a
    `pytestmark`. `names` holds the names that stand for a mark and come from
    another file; the names the file sets itself are read here. None when the
    file cannot be read as Python.
    """
    try:
        module = ast.parse(source or "")
    except SyntaxError:
        return None
    tests: set[str] = set()
    marked: dict[str, tuple[str, int]] = {}
    names = {**(names or {}), **mark_names_of(marks)(source, "")}

    def file_mark(body: list) -> tuple[str, int] | None:
        return mark_among([node.value for node in body if isinstance(node, ast.Assign)
                           and any(isinstance(t, ast.Name) and t.id == "pytestmark" for t in node.targets)], marks, names)

    def read(body: list, prefix: str, inherited: tuple[str, int] | None) -> None:
        for node in body:
            if isinstance(node, ast.ClassDef):
                read(node.body, f"{node.name}::",
                     mark_among(node.decorator_list, marks, names) or file_mark(node.body) or inherited)
            elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith("test"):
                tests.add(prefix + node.name)
                mark = mark_among(node.decorator_list, marks, names) or inherited
                if mark:
                    marked[prefix + node.name] = mark

    whole = file_mark(module.body)
    if whole:
        marked["*"] = whole
    read(module.body, "", None)
    return tests, marked


def leaves_finding(path: str, line_no: int, what: str, reason: str) -> Finding:
    if reason:
        return Finding("approval", path, line_no, f"{what}, with its reason: {reason}", LEFT_OUT_WHY,
                       "A maintainer has to approve the reason. There is nothing else to do.")
    return Finding("warning", path, line_no, what, LEFT_OUT_WHY, LEFT_OUT_FIX)


def names_of(names: list[str]) -> str:
    return ", ".join(names[:5]) + (f" and {len(names) - 5} more" if len(names) > 5 else "")


def whole_file_finding(c: FileChange, lines: dict[int, str], mark: str, line_no: int) -> Finding:
    if c.status == "added":
        return Finding("note", c.path, line_no, f"new test file with the mark `{mark}` on every test: they do not run "
                       "in the plain test jobs", LEFT_OUT_WHY, BORN_LEFT_OUT_FIX)
    what = f"new `pytestmark` with the mark `{mark}`: every test of this file leaves the plain test jobs"
    return leaves_finding(c.path, line_no, what, comment_beside(c.path, lines, line_no))


def newly_marked(c: FileChange, tree: Tree, marks: tuple[str, ...]) -> list[Finding]:
    """The tests of a changed Python test file that carry a left-out mark now and did not at the base.

    A test that was there at the base leaves the plain jobs. A test that is
    new in the change never ran in them, and is listed for information.
    """
    source = tree.read(c.path)
    names = names_in_reach(tree, c.path, mark_names_of(marks))
    now = tests_and_marks(source, marks, names) if file_kind(c.path) == "python" else None
    if now is None or not now[1]:
        return []
    base_source = None if c.status == "added" else tree.read_base(c.path)
    base = tests_and_marks(base_source, marks, names) if base_source is not None else None
    before_tests, before_marked = base or (set(), {})
    if "*" in before_marked:
        return []
    new = {name: found for name, found in now[1].items() if name not in before_marked}
    lines = dict(enumerate((source or "").splitlines(), 1))
    if "*" in new:
        return [whole_file_finding(c, lines, *new["*"])]
    # With no copy of the base to compare with, every test counts as one that was there.
    cannot_compare = base is None and c.status != "added"
    leave, born = {}, {}
    for name, (mark, line_no) in sorted(new.items()):
        if cannot_compare or name in before_tests:
            leave.setdefault((mark, comment_beside(c.path, lines, line_no)), []).append((line_no, name))
        else:
            born.setdefault(mark, []).append((line_no, name))
    return ([leaves_finding(c.path, found[0][0], f"{len(found)} test(s) leave the plain test jobs through the mark "
                            f"`{mark}`: {names_of([name for _, name in found])}", reason)
             for (mark, reason), found in leave.items()]
            + [Finding("note", c.path, found[0][0], f"{len(found)} new test(s) with the mark `{mark}`: they do not run "
                       "in the plain test jobs", LEFT_OUT_WHY, BORN_LEFT_OUT_FIX) for mark, found in born.items()])


def hook_targets(source: str | None, marks: tuple[str, ...]) -> dict[str, tuple[str, int]]:
    """What the hooks of a conftest.py give a left-out mark to: each path text in such a hook -> (the mark, its line).

    A hook here is a function that calls `add_marker(pytest.mark.<mark>)`, or
    gives it a name that the file sets to that mark. A text of it that holds
    a `/` is a path it picks tests by: a folder when it ends with `/`, else
    the end of a file's path.
    """
    try:
        module = ast.parse(source or "")
    except SyntaxError:
        return {}
    targets: dict[str, tuple[str, int]] = {}
    names = mark_names_of(marks)(source, "")
    for function in ast.walk(module):
        if not isinstance(function, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        calls = [node for node in ast.walk(function)
                 if isinstance(node, ast.Call) and getattr(node.func, "attr", "") == "add_marker"]
        mark = mark_among(calls, marks, names)
        if not mark:
            continue
        for node in ast.walk(function):
            if (isinstance(node, ast.Constant) and isinstance(node.value, str) and "/" in node.value
                    and " " not in node.value and re.search(r"\w", node.value)):
                targets.setdefault(node.value, (mark[0], node.lineno))
    return targets


def picked(path: str, target: str) -> bool:
    """Whether a hook's path text picks the file at `path`."""
    return target in f"/{path}" if target.endswith("/") else f"/{path}".endswith(target)


def hook_list_changes(c: FileChange, added: set[str], tree: Tree, marks: tuple[str, ...]) -> list[Finding]:
    """A path that a conftest.py's hook newly gives a left-out mark to. `added` holds the files the change adds."""
    source = tree.read(c.path)
    now = hook_targets(source, marks)
    before = hook_targets(None if c.status == "added" else tree.read_base(c.path), marks)
    lines = dict(enumerate((source or "").splitlines(), 1))
    out = []
    for target, (mark, line_no) in sorted(now.items()):
        if target in before:
            continue
        if target.lstrip("/") in added:
            out.append(Finding("note", c.path, line_no, f"the hook gives the mark `{mark}` to the new file `{target}`: "
                               "its tests do not run in the plain test jobs", LEFT_OUT_WHY, BORN_LEFT_OUT_FIX))
        else:
            what = f"the hook gives the mark `{mark}` to `{target}`: the tests there leave the plain test jobs"
            out.append(leaves_finding(c.path, line_no, what, comment_beside(c.path, lines, line_no)))
    return out


def conftests_above(path: str) -> list[str]:
    """The conftest.py files that can hold a hook for the test file at `path`, nearest first."""
    parts = path.split("/")[:-1]
    return ["/".join(parts[:depth] + ["conftest.py"]) for depth in range(len(parts), -1, -1)]


def placed_under_a_hook(c: FileChange, changes: list[FileChange], tree: Tree, marks: tuple[str, ...]) -> list[Finding]:
    """A test file that the change puts where a hook gives a left-out mark.

    The hook is read as it was at the base: a path that the change adds to a
    hook is named by `hook_list_changes`.
    """
    for conftest in conftests_above(c.path):
        for target, (mark, _) in sorted(hook_targets(tree.read_base(conftest) or tree.read(conftest), marks).items()):
            if not picked(c.path, target):
                continue
            name = c.path.rsplit("/", 1)[-1]
            moved_from = [d.path for d in changes if d.status == "deleted" and d.path.rsplit("/", 1)[-1] == name]
            if moved_from:
                return [Finding("warning", c.path, 0, f"test file moved from {moved_from[0]} to where the hook of "
                                f"{conftest} gives the mark `{mark}`: its tests leave the plain test jobs",
                                LEFT_OUT_WHY, LEFT_OUT_FIX)]
            return [Finding("note", c.path, 0, f"new test file where the hook of {conftest} gives the mark `{mark}`: its "
                            "tests do not run in the plain test jobs", LEFT_OUT_WHY, BORN_LEFT_OUT_FIX)]
    return []


def tests_with(tree: Tree, files: list[str], marks: tuple[str, ...]) -> dict[str, set[str]]:
    """The tests that carry one of `marks`, for each Python test file among `files` that has such a test.

    A test carries a mark on itself, on its class or on its file, by a name
    that stands for the mark, or from the hook of a conftest.py above it. It
    is in the set once, by whichever of these.
    """
    if not marks:
        return {}
    hooks = {path: hook_targets(tree.read(path), marks) for path in files if path.rsplit("/", 1)[-1] == "conftest.py"}
    found: dict[str, set[str]] = {}
    for path in files:
        if not path.endswith(".py") or not is_test(path) or path in hooks:
            continue
        read = tests_and_marks(tree.read(path), marks, names_in_reach(tree, path, mark_names_of(marks)))
        if read is None:
            continue
        tests, marked = read
        by_hook = any(picked(path, target) for conftest in conftests_above(path) for target in hooks.get(conftest, {}))
        mine = tests if "*" in marked or by_hook else set(marked)
        if mine:
            found[path] = set(mine)
    return found


SETTINGS_FIX = ("Keep these tests in the plain jobs. If they cannot run there, say why in a comment beside the line and "
                "name the job that runs them.")


def more_left_out(changes: list[FileChange], tree: Tree, marks: tuple[str, ...]) -> list[Finding]:
    """A mark that the runner's settings newly leave out, with the tests that leave the plain test jobs through it.

    A test that a mark of the base's settings had taken out already does not
    leave now, and is not counted. A mark that comes back gives no finding.
    """
    change = next((c for c in changes if c.path == RUNNER_SETTINGS and c.status == "modified"), None)
    before = tree.read_base(RUNNER_SETTINGS) if change else None
    if before is None:
        return []
    old = left_out_marks(before)
    lines = dict(enumerate((tree.read(RUNNER_SETTINGS) or "").splitlines(), 1))
    line_no = next((number for number, line in lines.items() if re.match(r"addopts\s*=", line)), 0)
    reason = comment_beside(RUNNER_SETTINGS, lines, line_no)
    files = tree.tracked()
    out_already = tests_with(tree, files or [], old)
    out = []
    for mark in (mark for mark in marks if mark not in old):
        head = f"the runner's settings leave out one more mark, `{mark}`"
        carry = tests_with(tree, files or [], (mark,))
        leaving = {path: tests - out_already.get(path, set()) for path, tests in carry.items()}
        leaving = {path: tests for path, tests in leaving.items() if tests}
        if files is None:
            what = f"{head}: its tests leave the plain test jobs, and they could not be counted (the files could not be listed)"
        elif leaving:
            what = (f"{head}: {sum(map(len, leaving.values()))} test(s) in {len(leaving)} file(s) leave the plain test "
                    f"jobs ({names_of(sorted(leaving))})")
        else:
            carried = sum(map(len, carry.values()))
            said = (f"the {carried} test(s) that carry it are left out already through another mark" if carried
                    else "no test carries it today")
            out.append(Finding("note", RUNNER_SETTINGS, line_no, f"{head}: {said}, so none leaves the plain test jobs",
                               LEFT_OUT_WHY, BORN_LEFT_OUT_FIX))
            continue
        out.append(Finding("approval", RUNNER_SETTINGS, line_no, f"{what}, with its reason: {reason}", LEFT_OUT_WHY,
                           "A maintainer has to approve the reason. There is nothing else to do.") if reason
                   else Finding("warning", RUNNER_SETTINGS, line_no, what, LEFT_OUT_WHY, SETTINGS_FIX))
    return out


def left_out_tests(changes: list[FileChange], tree: Tree) -> list[Finding]:
    """Every way a change takes a test out of the plain test jobs through a mark that the runner's settings leave out."""
    marks = left_out_marks(tree.read(RUNNER_SETTINGS))
    out = more_left_out(changes, tree, marks)
    for c in changes:
        if not marks or c.status == "deleted" or not c.path.endswith(".py"):
            continue
        if c.path.rsplit("/", 1)[-1] == "conftest.py":
            out += hook_list_changes(c, {d.path for d in changes if d.status == "added"}, tree, marks)
        elif is_test(c.path):
            out += newly_marked(c, tree, marks)
            if c.status == "added":
                out += placed_under_a_hook(c, changes, tree, marks)
    return out


def changed_assertions(c: FileChange, product_changed: bool) -> list[Finding]:
    removed = [hunk for hunk, text in c.removed if ASSERTION.search(text)]
    added = [hunk for hunk, _, text in c.added if ASSERTION.search(text)]
    rewritten = set(removed) & set(added)
    if len(removed) > len(added):
        return [Finding("warning", c.path, 0, f"{len(removed)} assertion(s) removed, {len(added)} added",
                        TEST_WHY, "Keep the assertions, or say in the pull request why they were wrong.")]
    if product_changed and rewritten:
        return [Finding("warning", c.path, 0,
                        f"{len(rewritten)} assertion(s) rewritten while product code changed", TEST_WHY,
                        "Confirm in the pull request that the old expectation was wrong, not the new code.")]
    return []


def check_comments_and_names(changes: list[FileChange], tasks: set[str], tree: Tree) -> list[Finding]:
    task_re = re.compile(r"\b(" + "|".join(sorted(map(re.escape, tasks))) + r")\b") if tasks else None
    out = []
    for c in changes:
        out += new_markers(c, tree) + workflow_settings(c)
        if not c.path.endswith(CODE_EXTENSIONS):
            continue
        product = is_product(c.path)
        for _, line_no, text in c.added:
            out += comment_findings(c.path, line_no, comment_text(c.path, text))
            named = task_re.search(text) if task_re and product else None
            if named:
                out.append(Finding(
                    "warning", c.path, line_no, f"evaluation task named in product code: {named.group(0)!r}",
                    "Product code that names a task from the evaluation suite is fitted to the suite, "
                    "not to the kind of problem.",
                    "State the general condition the code handles. Keep task names in the runner and its tests."))
    return out


def comment_findings(path: str, line_no: int, comment: str) -> list[Finding]:
    out = []
    for label, pattern in HISTORY:
        found = pattern.search(comment)
        if found:
            out.append(Finding(
                "warning", path, line_no, f"{label} in a new comment: {found.group(0)!r}",
                "A comment describes the code as it is. History goes stale in the code and "
                "is already kept by the commit, the pull request and the issue.",
                "Say what the code does and why. Move dates, IDs and run names to the commit message."))
    return out


def new_markers(c: FileChange, tree: Tree) -> list[Finding]:
    """A finding for each suppression marker the change adds to a file of the marker's kind.

    Not new: a marked line that only moved inside its file, and an import
    that follows the line that sets the import path, marked for exactly that.
    """
    patterns = MARKERS.get(file_kind(c.path))
    if not patterns:
        return []
    marker = re.compile("|".join(patterns))
    moved = Counter(text.strip() for _, text in c.removed if marker.search(comment_text(c.path, text)))
    source = None
    out = []
    for _, line_no, text in c.added:
        if not marker.search(comment_text(c.path, text)):
            continue
        if moved[text.strip()] > 0:
            moved[text.strip()] -= 1
            continue
        if IMPORT_AFTER_PATH.match(text) and file_kind(c.path) == "python":
            source = tree.read(c.path) if source is None else source
            if any(SETS_IMPORT_PATH.match(line) for line in (source or "").splitlines()[:line_no - 1]):
                continue
        out.append(Finding(
            "approval", c.path, line_no, "new suppression marker",
            "A suppression turns a check off for this line, and it stays after the reason is gone.",
            "Fix what the check reports. If it is a false alarm, say why next to the marker; "
            "a maintainer has to approve it."))
    return out


def workflow_settings(c: FileChange) -> list[Finding]:
    if file_kind(c.path) != "workflow":
        return []
    return [Finding("approval", c.path, line_no, f"new workflow setting: {setting}", why, fix)
            for _, line_no, text in c.added for pattern, setting, why, fix in WORKFLOW_SETTINGS
            if pattern.match(text)]


def lowered_baseline(before: str | None, after: str | None) -> str:
    """What a change to the size baseline lowered, or "" when it did anything else or cannot be read."""
    try:
        old, new = json.loads(before), json.loads(after)
    except (TypeError, ValueError):
        return ""
    lowered = removed = 0
    for section in sorted(set(old) | set(new)):
        was, now = old.get(section), new.get(section)
        if not isinstance(was, dict) or not isinstance(now, dict):
            return ""
        if any(key not in was or not isinstance(value, int) or value > was[key] for key, value in now.items()):
            return ""
        if section == "limits" and set(was) != set(now):
            return ""
        lowered += sum(1 for key, value in now.items() if value < was[key])
        removed += len(set(was) - set(now))
    return f"{lowered} number(s) lower, {removed} entr{'y' if removed == 1 else 'ies'} removed" if lowered or removed else ""


def check_files(changes: list[FileChange], tree: Tree) -> list[Finding]:
    out = []
    for c in changes:
        if c.status == "added" and c.path.endswith(".md") and not c.path.startswith(DOC_DIRS_ALLOWED):
            out.append(Finding(
                "approval", c.path, 0, "new documentation file",
                "Each new file is one more to keep true. Notes, plans and results belong in the "
                "issue or the pull request.",
                "Update the document that owns the topic. A new document needs a maintainer's approval."))
        lowered = lowered_baseline(tree.read_base(c.path), tree.read(c.path)) if c.path == SIZE_BASELINE else ""
        if lowered:
            out.append(Finding(
                "note", c.path, 0, f"size baseline lowered: {lowered}",
                "A baseline whose numbers only go down, or lose an entry, can only make the size check stricter.",
                "Nothing to do."))
        elif c.path.startswith(GATE_FILES) and not c.path.endswith(LOCK_FILES):
            out.append(Finding(
                "approval", c.path, 0, "change to a file that configures the checks",
                "These files decide what the checks accept, so a change here can weaken every later check.",
                "Say in the pull request what the change allows or forbids. A maintainer has to approve it."))
        elif c.path.startswith(WRITE_CREDENTIAL_SCRIPTS):
            out.append(Finding(
                "approval", c.path, 0, "change to a script that runs with a credential that can write",
                "A workflow runs this script with a token that can change the repository, so a change here "
                "acts with that right.",
                "Say in the pull request what the script now does with the credential. A maintainer has to "
                "approve it."))
    return out


def check_notes(changes: list[FileChange]) -> list[Finding]:
    out = []
    for c in changes:
        name = c.path.rsplit("/", 1)[-1]
        if name in ("go.mod", "package.json"):
            kind = name
        elif name.startswith("requirements") or name.endswith(".in"):
            kind = "requirements"
        else:
            continue
        for _, line_no, text in c.added:
            dep = NEW_DEPENDENCY[kind].match(text)
            if dep and dep.group(1) != "version" and (kind != "go.mod" or "." in dep.group(1)):
                out.append(Finding("note", c.path, line_no, f"new or changed dependency: {dep.group(1)}",
                                   "Every dependency is code the project runs without having written it.",
                                   "Check that the package is the intended one and is maintained."))
    size = sum(len(c.added) + len(c.removed) for c in changes if not c.path.endswith(LOCK_FILES))
    if size >= LARGE_CHANGE_LINES:
        out.append(Finding("note", "", 0, f"large change: {size} changed lines",
                           "A large change is hard to review line by line.",
                           "Split it where the parts can land on their own."))
    return out


def check(diff_text: str, tasks: set[str], tree: Tree | None = None) -> list[Finding]:
    changes, tree = parse_diff(diff_text), tree or Tree()
    return (check_tests(changes, tree) + check_comments_and_names(changes, tasks, tree)
            + check_files(changes, tree) + check_notes(changes))


LEVEL_TITLES = {"approval": "Needs a maintainer's approval", "warning": "Needs action",
                "note": "For information"}


def report_lines(findings: list[Finding]) -> list[str]:
    lines = []
    for level, title in LEVEL_TITLES.items():
        group = [f for f in findings if f.level == level]
        if group:
            lines.append(f"{title} ({len(group)})")
        for f in group:
            where = f"{f.path}:{f.line}" if f.line else f.path or "(whole change)"
            lines += [f"  {where}: {f.what}", f"    why: {f.why}", f"    fix: {f.fix}"]
    return lines or ["integrity check: nothing found"]


def report(findings: list[Finding], github: bool) -> None:
    text = "\n".join(report_lines(findings))
    print(text)
    if not github:
        return
    for f in findings:
        if f.level != "note":
            location = f"file={f.path},line={f.line or 1}," if f.path else ""
            print(f"::warning {location}title=integrity: {f.what}::{f.why} {f.fix}")
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with open(summary, "a", encoding="utf-8") as handle:
            handle.write("### integrity check\n\n```\n" + text + "\n```\n")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", default="origin/dev", help="commit or branch to compare HEAD with")
    parser.add_argument("--strict", action="store_true",
                        help="exit 1 when a finding needs action or approval")
    parser.add_argument("--github", action="store_true",
                        help="also write GitHub annotations and the job summary")
    parser.add_argument("--root", type=Path, default=ROOT,
                        help="the checkout to read (default: the one this script is in)")
    args = parser.parse_args()
    diff = subprocess.run(
        ["git", "diff", "--no-color", "--unified=0", "--no-renames",
         "--end-of-options", f"{args.base}...HEAD", "--"],
        cwd=args.root, capture_output=True, text=True)
    if diff.returncode != 0:
        print(f"integrity check: cannot read the diff against {args.base!r}: {diff.stderr.strip()}\n"
              "  fix: fetch the base branch (git fetch origin) or pass --base <commit>.", file=sys.stderr)
        return 2
    try:
        tasks = task_names(args.root / TASK_SOURCE)
    except Exception as error:  # the runner is a script: anything can go wrong loading it
        print(f"integrity check: cannot read the evaluation task names from {TASK_SOURCE}: {error!r}\n"
              "  why: without them, a task name in product code would pass unseen.\n"
              "  fix: make the runner importable again, or point TASK_SOURCE at the file that defines TASKS.",
              file=sys.stderr)
        return 2
    merge_base = subprocess.run(["git", "merge-base", "--end-of-options", args.base, "HEAD"],
                                cwd=args.root, capture_output=True, text=True, check=False)
    findings = check(diff.stdout, tasks, Tree(args.root, merge_base.stdout.strip()))
    report(findings, args.github)
    return int(args.strict and any(f.level != "note" for f in findings))


if __name__ == "__main__":
    raise SystemExit(main())
