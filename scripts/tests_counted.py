#!/usr/bin/env python3
"""Whether a run of the tests that need a running service ran all of them, and passed.

The plain test jobs leave these tests out: each needs a service that such a
job does not have. They are in three groups, and each group is run in one
place. This file is the one list of the groups and the one judge of a run.

A run of a group holds only when every test of the group was collected, none
failed and none was skipped. pytest ends with status 0 for a run in which
tests were skipped, and a missing package skips a whole file with no other
sign. So the status of pytest is not read: its result file is.

Usage: tests_counted.py <group> <result file>    judge the result file of a run of that group
       tests_counted.py --files <group>           print the files of the group, for the pytest command

The result file is the one that pytest writes with `--junitxml=<file>`. Only
the numbers in the opening line of each suite and the names and messages of
its cases are read; the file is not given to an XML parser.

Exit status: 0 when the run holds; 1 when a test failed, was skipped or was
not collected; 2 when the result file cannot be read or the group is not one.
"""
from __future__ import annotations

import html
import re
import sys
from pathlib import Path
from typing import NamedTuple


class Group(NamedTuple):
    needs: str      # what the tests of the group need, in the words of the gates page
    runs_in: str    # the one place that runs the group
    expected: int   # how many tests the group has
    files: tuple


# The tests that need a running service. A file is in one group, and a group is run in one place.
GROUPS = {
    "sandbox": Group("a running sandbox service", "the job `sandbox tests (containerized)`", 80, (
        "tests/infrastructure/test_sandbox.py",
        "tests/infrastructure/test_sandbox_java_kotlin.py",
        "tests/infrastructure/test_sandbox_ruby_php.py")),
    "proxy": Group("the built TUI and a running proxy", "the job `proxy and TUI tests (no model)`", 13, (
        "tests/infrastructure/test_tui_render.py",
        "tests/infrastructure/test_tui_commands.py",
        "tests/infrastructure/test_control_plane.py")),
    "model": Group("a running model server", "the nightly run, on the development server", 32, (
        "tests/infrastructure/test_llm.py",)),
}
SHOWN = 5


class Unreadable(Exception):
    """The result file is not there, or it is not a result file of pytest."""


def read(result: str) -> dict:
    """The numbers of a result file: collected, passed, failed, skipped, the tests that failed and why tests were skipped."""
    suites = re.findall(r"<testsuite\b[^>]*>", result)
    if not suites:
        raise Unreadable("it has no test suite in it")
    count = {key: sum(int(number) for suite in suites for number in re.findall(rf'\b{key}="(\d+)"', suite))
             for key in ("tests", "failures", "errors", "skipped")}
    failed = count["failures"] + count["errors"]
    reasons: dict = {}
    for said, more in re.findall(r'<skipped\b[^>]*?\bmessage="([^"]*)"[^>]*>([^<]*)', result):
        reason = " ".join(skip_reason(html.unescape(said), html.unescape(more)).split())[:160] or "no reason given"
        reasons[reason] = reasons.get(reason, 0) + 1
    return {"collected": count["tests"], "passed": count["tests"] - failed - count["skipped"], "failed": failed,
            "skipped": count["skipped"], "failed_tests": failed_tests(result),
            "skip_reasons": dict(sorted(reasons.items(), key=lambda item: (-item[1], item[0])))}


def skip_reason(said: str, more: str) -> str:
    """Why a test was skipped. For a file that was skipped as a whole pytest gives the reason only in the text below."""
    if said != "collection skipped" or "Skipped: " not in more:
        return said
    # The text is a tuple as Python prints it: the reason is its last part, in quotes, before the closing bracket.
    return "a whole file was skipped: " + re.sub(r"[\"']\)\s*$", "", more.split("Skipped: ", 1)[1])


def failed_tests(result: str) -> list:
    """The names of the tests that failed or ended with an error, as the result file has them."""
    names = []
    # Each piece runs from one case to the next, so a failure in it is the failure of that case.
    for case in result.split("<testcase")[1:]:
        if "<failure" in case or "<error" in case:
            name = re.search(r'\bname="([^"]*)"', case.split(">", 1)[0])
            names.append(html.unescape(name.group(1)) if name else "a test with no name")
    return names


def shown(items: list, what: str) -> str:
    """The first few of a list, and how many more there are."""
    more = f"; and {len(items) - SHOWN} more {what}" if len(items) > SHOWN else ""
    return "; ".join(items[:SHOWN]) + more


def faults(numbers: dict, expected: int) -> list:
    """What keeps the run from holding, each in words. Empty when every expected test ran and passed."""
    found = []
    if numbers["collected"] != expected:
        found.append(f"{numbers['collected']} tests were collected, and there are {expected}")
    if numbers["failed"]:
        named = shown(numbers.get("failed_tests") or [], "test(s)")
        found.append(f"{numbers['failed']} test(s) failed" + (f" ({named})" if named else ""))
    if numbers["skipped"]:
        # A skipped test counts as collected, so the number of collected tests does not show it. It was not run.
        reasons = [f"{count}: {reason}" for reason, count in (numbers.get("skip_reasons") or {}).items()]
        said = shown(reasons, "reason(s)") or "the result file gives no reason"
        found.append(f"{numbers['skipped']} test(s) were skipped ({said})")
    return found


def main(argv: list | None = None) -> int:
    args = sys.argv[1:] if argv is None else argv
    wants_files = args[:1] == ["--files"]
    names = args[1:] if wants_files else args[:1]
    if len(args) != 2 or names[0] not in GROUPS:
        print(f"tests counted: give one of the groups {', '.join(GROUPS)}, as `<group> <result file>` or `--files <group>`. "
              "Fix: the call in the workflow or in the nightly run.", file=sys.stderr)
        return 2
    group = GROUPS[names[0]]
    if wants_files:
        print(" ".join(group.files))
        return 0
    try:
        numbers = read(Path(args[1]).read_text(encoding="utf-8"))
    except (OSError, Unreadable) as error:
        print(f"::error title=tests counted::the result file {args[1]} of the tests that need {group.needs} cannot be read "
              f"({error}), so no test is known to have run. Fix: read the output of the pytest step above; pytest writes "
              "the file also when tests fail, and writes none when it could not start.")
        return 2
    found = faults(numbers, group.expected)
    if not found:
        print(f"tests counted: all {group.expected} tests that need {group.needs} ran and passed.")
        return 0
    print(f"::error title=tests counted::the run of the tests that need {group.needs} does not hold: {'; '.join(found)}. "
          "A test of this group is never skipped: it runs and passes, or the job is red. Fix: a failed test, as any "
          "failed test; a skipped test, by what its reason names (the service or the tool is to be there, the condition "
          "is not to be taken out of the judge); another number of tests, in the group's number in "
          "scripts/tests_counted.py and in the table of docs/quality/gates.md, in the same change.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
