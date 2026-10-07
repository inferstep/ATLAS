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
import importlib.util
import json
import os
import re
import subprocess
import sys
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
    ".github/workflows/", ".github/requirements/", ".github/code-health-baseline.json", ".github/canary.json",
    ".golangci.yml", ".golangci.yaml", ".codescene/", "codecov.yml",
    ".sonarcloud.properties", "sonar-project.properties", "pyproject.toml",
    "extensions/vscode/eslint.config.mjs", "tests/perf/budgets.json", "tests/replay/recordings/",
    "scripts/integrity_check.py", "scripts/code_health.py", "scripts/production-readiness.py",
    "scripts/checks_ran.py", "scripts/verify.py", "scripts/canary.py", "scripts/dockerfile_lint.py",
    "scripts/check_dockerfile_sources.py", "scripts/check_min_python.py", "scripts/staging-integration.py",
    "scripts/ci-lock.sh", "scripts/release-tag.sh", "scripts/setup/environments.sh", "scripts/setup/rulesets.sh",
    # The settings file of each linter of the pipeline, should one appear.
    ".hadolint.yaml", ".hadolint.yml", ".github/zizmor.yml", "zizmor.yml", ".github/actionlint.yaml",
    ".github/actionlint.yml", ".yamllint", ".yamllint.yml", ".yamllint.yaml", ".shellcheckrc", "ruff.toml",
    ".ruff.toml", "staticcheck.conf", ".trivyignore",
)
# Scripts that a workflow runs with a credential that can write: a token with
# a write permission, an app token, a right to publish.
WRITE_CREDENTIAL_SCRIPTS = ("scripts/bot/", "scripts/star-history-chart.py")
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
    "ESLint": ("script",), "SonarQube": ("go", "python", "script"),
    "actionlint": "none", "CodeScene": "none", "Codecov": "none", "Trivy": "none", "CodeQL": "none",
}
LOCK_FILES = ("package-lock.json", "go.sum", ".github/requirements/ci.txt")
SIZE_BASELINE = ".github/code-health-baseline.json"
LARGE_CHANGE_LINES = 400

TEST_DEF = re.compile(r"^\s*(?:func (Test\w+)\(|(?:async )?def (test_\w+)\(|(?:it|test)\(\s*['\"`](.+?)['\"`])")
# A skip is a statement or a decorator, so it starts its line; the same words
# inside a string are data.
SKIP = re.compile(r"^\s*(?:\w+\s*=\s*)?(?:@?pytest\.mark\.(?:skip|xfail)|pytest\.skip\(|@?unittest\.skip"
                  r"|t\.Skip(?:f|Now)?\(|(?:it|test|describe)\.(?:skip|todo)\(|xit\()")
ASSERTION = re.compile(r"(?:^\s*assert\b)|\bt\.(?:Error|Errorf|Fatal|Fatalf)\(|\b(?:require|assert)\.\w+\(|\bexpect\(")
# The line that starts a function, in the languages the tests are written in.
DEFINITION = re.compile(r"^\s*(?:(?:async\s+)?def|func|function)\s+(?:\([^)]*\)\s*)?(\w+)\s*[(\[]")
# What a skip says about itself: `reason=`, or the message of a call that takes one.
SKIP_REASON = re.compile(r"(?:\breason\s*=\s*|\bt\.Skipf?\(\s*|\bpytest\.skip\(\s*|\bunittest\.skip\(\s*)([^\s),][^),]*)")
# The suppression markers of each kind of file. A marker counts in a comment
# of a file of its kind; the same words in another kind of file are text.
MARKERS = {
    "go": (r"\bnolint\b", r"\blint:ignore\b", r"\blint:file-ignore\b", r"\bNOSONAR\b", r"\bnosec\b"),
    "python": (r"\bnoqa\b", r"type:\s*ignore", r"\bnosec\b", r"pragma:\s*no cover", r"\bNOSONAR\b"),
    "script": (r"eslint-disable", r"@ts-ignore", r"@ts-expect-error", r"\bNOSONAR\b"),
    "shell": (r"shellcheck disable=",),
    "workflow": (r"zizmor:\s*ignore\[", r"yamllint disable"),
    "dockerfile": (r"hadolint ignore=", r"hadolint global ignore="),
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
    return (name.endswith("_test.go") or name.startswith("test_") or name.endswith("_test.py")
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
                    + changed_assertions(c, product_changed))
    return out


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
    removed test.
    """
    body_removed = [text for h, text in c.removed
                    if h == hunk and text.strip() and not text.strip().startswith("@") and not test_name(text)]
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
    it. An empty text is no reason. `lines` holds the lines that can be read,
    by line number.
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
    reason = given.group(1).strip().strip("\"'`").strip() if given else ""
    if reason:
        return reason
    above = lines.get(line_no - 1, "").strip()
    comment = comment_text(path, lines[line_no]) or (above.lstrip("#/ ") if above.startswith(("#", "//")) else "")
    return comment.strip()


def skip_finding(path: str, line_no: int, what: str, reason: str) -> Finding:
    if reason:
        return Finding("approval", path, line_no, f"{what}, with its reason: {reason}", TEST_WHY,
                       "A maintainer has to approve the reason. There is nothing else to do.")
    return Finding("warning", path, line_no, what, TEST_WHY,
                   "Make the test run. If it cannot run here, say why next to the skip.")


def new_skips(c: FileChange, tree: Tree) -> list[Finding]:
    added = {line_no: text for _, line_no, text in c.added}
    # The whole file when it can be read, so that a comment on an unchanged line above a new skip is seen.
    source = tree.read(c.path)
    lines = dict(enumerate(source.splitlines(), 1)) if source is not None else added
    return [skip_finding(c.path, line_no, "new skip or todo marker in a test", skip_reason(c.path, lines, line_no))
            for line_no, text in added.items() if SKIP.search(text)]


def skip_helpers(tree: Tree, path: str) -> dict[str, str]:
    """The functions beside `path` that skip the test that calls them: name -> the reason they give."""
    helpers: dict[str, str] = {}
    for other in tree.tests_beside(path):
        lines = dict(enumerate((tree.read(other) or "").splitlines(), 1))
        name = ""
        for line_no, line in lines.items():
            defined = DEFINITION.match(line)
            if defined:
                name = "" if test_name(line) else defined.group(1)
            elif name and SKIP.search(line):
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
