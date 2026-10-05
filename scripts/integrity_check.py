#!/usr/bin/env python3
"""Check a change for the ways it can weaken the project's own checks.

Reads the diff between a base commit and HEAD and reports:
  - tests that were deleted, skipped or loosened;
  - history markers in new comments (dates, commit IDs, run names);
  - names of evaluation tasks in product code;
  - new documentation files;
  - changes to the files that configure the checks, and new suppressions;
  - new dependencies and large changes (listed, not judged).

Each finding says what was found, why it matters and what to do about it. The
script reports and exits 0. With --strict it exits 1 when a finding needs
action or approval.
"""

from __future__ import annotations

import argparse
import importlib.util
import os
import re
import subprocess
import sys
from dataclasses import dataclass, field
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
TASK_SOURCE = Path("scripts") / "e2e-reliability.py"

CODE_EXTENSIONS = (".go", ".py", ".ts", ".js", ".mjs", ".sh")
PRODUCT_DIRS = ("proxy/", "tui/", "atlas/", "v3-service/", "geometric-lens/",
                "sandbox/", "inference/", "extensions/vscode/src/")
DOC_DIRS_ALLOWED = (".github/",)
GATE_FILES = (
    ".github/workflows/", ".github/requirements/", ".github/code-health-baseline.json",
    ".golangci.yml", ".golangci.yaml", ".codescene/", "codecov.yml",
    ".sonarcloud.properties", "sonar-project.properties", "pyproject.toml",
    "extensions/vscode/eslint.config.mjs", "tests/perf/budgets.json",
    "scripts/integrity_check.py", "scripts/code_health.py",
    "scripts/production-readiness.py",
)
LOCK_FILES = ("package-lock.json", "go.sum", ".github/requirements/ci.txt")
LARGE_CHANGE_LINES = 400

TEST_DEF = re.compile(r"^\s*(?:func (Test\w+)\(|(?:async )?def (test_\w+)\(|(?:it|test)\(\s*['\"`](.+?)['\"`])")
# A skip is a statement or a decorator, so it starts its line; the same words
# inside a string are data.
SKIP = re.compile(r"^\s*(?:\w+\s*=\s*)?(?:@?pytest\.mark\.(?:skip|xfail)|pytest\.skip\(|@?unittest\.skip"
                  r"|t\.Skip(?:f|Now)?\(|(?:it|test|describe)\.(?:skip|todo)\(|xit\()")
ASSERTION = re.compile(r"(?:^\s*assert\b)|\bt\.(?:Error|Errorf|Fatal|Fatalf)\(|\b(?:require|assert)\.\w+\(|\bexpect\(")
SUPPRESSION = re.compile(r"\bnolint\b|\bnoqa\b|\bNOSONAR\b|type:\s*ignore|eslint-disable|\bnosec\b"
                         r"|shellcheck disable=|@ts-ignore|@ts-expect-error|pragma:\s*no cover")
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


def is_test(path: str) -> bool:
    name = path.rsplit("/", 1)[-1]
    return (name.endswith("_test.go") or name.startswith("test_") or name.endswith("_test.py")
            or ".test." in name or "/tests/" in f"/{path}" or "/test/" in f"/{path}")


def is_product(path: str) -> bool:
    return path.startswith(PRODUCT_DIRS) and path.endswith(CODE_EXTENSIONS) and not is_test(path)


def comment_text(path: str, line: str) -> str:
    """The comment part of a source line, or "" when it has none."""
    stripped = line.strip()
    if path.endswith((".go", ".ts", ".js", ".mjs")):
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


def check_tests(changes: list[FileChange]) -> list[Finding]:
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
            out += removed_and_skipped_tests(c, added_names) + changed_assertions(c, product_changed)
    return out


def removed_and_skipped_tests(c: FileChange, added_names: set[str]) -> list[Finding]:
    removed = {test_name(text) for _, text in c.removed} - {""} - added_names
    out = [Finding("warning", c.path, 0, f"test removed: {name}", TEST_WHY,
                   "Keep the test, or say in the pull request why it no longer applies.")
           for name in sorted(removed)]
    out += [Finding("warning", c.path, line_no, "new skip or todo marker in a test", TEST_WHY,
                    "Make the test run. If it cannot run here, say why next to the skip.")
            for _, line_no, text in c.added if SKIP.search(text)]
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


def check_comments_and_names(changes: list[FileChange], tasks: set[str]) -> list[Finding]:
    task_re = re.compile(r"\b(" + "|".join(sorted(map(re.escape, tasks))) + r")\b") if tasks else None
    out = []
    for c in changes:
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
    if SUPPRESSION.search(comment):
        out.append(Finding(
            "approval", path, line_no, "new suppression marker",
            "A suppression turns a check off for this line, and it stays after the reason is gone.",
            "Fix what the check reports. If it is a false alarm, say why next to the marker; "
            "a maintainer has to approve it."))
    return out


def check_files(changes: list[FileChange]) -> list[Finding]:
    out = []
    for c in changes:
        if c.status == "added" and c.path.endswith(".md") and not c.path.startswith(DOC_DIRS_ALLOWED):
            out.append(Finding(
                "approval", c.path, 0, "new documentation file",
                "Each new file is one more to keep true. Notes, plans and results belong in the "
                "issue or the pull request.",
                "Update the document that owns the topic. A new document needs a maintainer's approval."))
        if c.path.startswith(GATE_FILES) and not c.path.endswith(LOCK_FILES):
            out.append(Finding(
                "approval", c.path, 0, "change to a file that configures the checks",
                "These files decide what the checks accept, so a change here can weaken every later check.",
                "Say in the pull request what the change allows or forbids. A maintainer has to approve it."))
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


def check(diff_text: str, tasks: set[str]) -> list[Finding]:
    changes = parse_diff(diff_text)
    return (check_tests(changes) + check_comments_and_names(changes, tasks)
            + check_files(changes) + check_notes(changes))


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
    findings = check(diff.stdout, tasks)
    report(findings, args.github)
    return int(args.strict and any(f.level != "note" for f in findings))


if __name__ == "__main__":
    raise SystemExit(main())
