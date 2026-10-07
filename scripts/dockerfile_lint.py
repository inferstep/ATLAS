#!/usr/bin/env python3
"""Lint every Dockerfile the repository tracks with hadolint, and report what it finds.

A finding does not fail the run. Every finding is printed, the counts go to
the job summary, and on GitHub the findings in a Dockerfile the change touches
become annotations, so a pull request shows the findings of its own files.

The run fails when the check did not check: the repository tracks no
Dockerfile, hadolint did not run, or hadolint could not read a Dockerfile.

Usage: dockerfile_lint.py [--hadolint PATH] [--changed-from COMMIT] [--changed-only]
"""
from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
from collections import Counter
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
DOCKERFILE = re.compile(r"(^|/)Dockerfile(\.[^/]+)?$")
UNREADABLE = "DL1000"
LEVELS = ("error", "warning", "info", "style")
INSTALL = ("Fix: install the hadolint version that .github/workflows/hadolint.yml records "
           "(https://github.com/hadolint/hadolint/releases), or pass its path with --hadolint.")


class LintError(Exception):
    """The lint could not run, so it judged nothing."""


def dockerfiles(root: Path) -> list[str]:
    """The Dockerfiles git tracks: `Dockerfile` and `Dockerfile.<variant>`, in any folder."""
    done = subprocess.run(["git", "-C", str(root), "ls-files"], capture_output=True, text=True, check=False)
    if done.returncode != 0:
        raise LintError(f"`git ls-files` failed in {root}, so the Dockerfiles are not known: {done.stderr.strip()} "
                        "Fix: run this from a checkout of the repository.")
    return sorted(path for path in done.stdout.splitlines() if DOCKERFILE.search(path))


def changed_files(root: Path, base: str) -> set[str] | None:
    """The files that differ from `base`, or None when that cannot be read."""
    done = subprocess.run(["git", "-C", str(root), "diff", "--name-only", f"{base}...HEAD"],
                          capture_output=True, text=True, check=False)
    return set(done.stdout.splitlines()) if done.returncode == 0 else None


def run_hadolint(binary: str, files: list[str], root: Path) -> list[dict]:
    """hadolint's findings for these files. --no-fail: a finding is reported here, not by the exit status."""
    try:
        done = subprocess.run([binary, "--no-fail", "--format", "json", *files], cwd=root, capture_output=True,
                              text=True, check=False)
    except OSError as error:
        raise LintError(f"hadolint did not start ({error}), so no Dockerfile was linted. {INSTALL}") from error
    try:
        findings = json.loads(done.stdout) if done.returncode == 0 else None
    except ValueError:
        findings = None
    if not isinstance(findings, list):
        raise LintError(f"hadolint ended with status {done.returncode} and no list of findings, so no Dockerfile "
                        f"was linted. Its output: {(done.stderr or done.stdout).strip()[:300]} "
                        "Fix: run the same command by hand and correct what it names.")
    return findings


def escaped(text: str, in_property: bool = False) -> str:
    """Text as a GitHub workflow command takes it."""
    text = text.replace("%", "%25").replace("\r", "%0D").replace("\n", "%0A")
    return text.replace(":", "%3A").replace(",", "%2C") if in_property else text


def annotation(finding: dict) -> str:
    kind = "notice" if finding["level"] in ("info", "style") else "warning"
    return (f"::{kind} file={escaped(finding['file'], True)},line={finding['line']},"
            f"title={escaped('hadolint (' + finding['code'] + ')', True)}::{escaped(finding['message'])}")


def summary(findings: list[dict], files: list[str]) -> list[str]:
    """The counts by level, then by rule, most frequent first."""
    by_level = Counter(finding["level"] for finding in findings)
    levels = ", ".join(f"{by_level[level]} {level}" for level in LEVELS if by_level[level] or level != "style")
    lines = [(f"hadolint: {len(findings)} finding(s) in {len(files)} Dockerfile(s): {levels}. "
              "It reports and does not fail for a finding.")]
    by_rule = Counter((finding["code"], finding["level"]) for finding in findings)
    lines += [f"- {code} ({level}): {count}" for (code, level), count in sorted(by_rule.items(), key=lambda kv: (-kv[1], kv[0]))]
    return lines


def unreadable(findings: list[dict]) -> list[str]:
    """One message for each Dockerfile hadolint could not read."""
    return [(f"{finding['file']}:{finding['line']}: hadolint cannot read this file as a Dockerfile: "
             f"{finding['message'].splitlines()[0]}. Nothing else in the file was linted. "
             "Fix: correct the instruction at that line.")
            for finding in findings if finding["code"] == UNREADABLE]


def lint(root: Path, binary: str, base: str, changed_only: bool) -> int:
    files = dockerfiles(root)
    if not files:
        raise LintError("the repository tracks no file named `Dockerfile` or `Dockerfile.<variant>`, so nothing was "
                        f"linted. Fix: if the Dockerfiles have new names, change the pattern in {Path(__file__).name}.")
    findings = run_hadolint(binary, files, root)
    touched = changed_files(root, base) if base else None
    if base and touched is None:
        print(f"note commit {base} is not in this checkout, so every finding is shown, not only those in the "
              "Dockerfiles the change touches.")
    shown = [f for f in findings if not changed_only or touched is None or f["file"] in touched]
    for finding in shown:
        print(f"{finding['file']}:{finding['line']}: {finding['code']} {finding['level']}: {finding['message']}")
    if os.environ.get("GITHUB_ACTIONS") == "true":
        for finding in findings:
            if touched is None or finding["file"] in touched:
                print(annotation(finding))
    lines = summary(findings, files)
    print("\n".join(lines))
    if os.environ.get("GITHUB_STEP_SUMMARY"):
        with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as out:
            out.write("\n".join(lines) + "\n")
    problems = unreadable(findings)
    for problem in problems:
        print(f"FAIL {problem}", file=sys.stderr)
    return 1 if problems else 0


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--hadolint", default="hadolint", help="the hadolint binary (default: the one on PATH)")
    parser.add_argument("--changed-from", default="", metavar="COMMIT",
                        help="annotate only the Dockerfiles that differ from this commit")
    parser.add_argument("--changed-only", action="store_true",
                        help="print only the findings in the Dockerfiles that differ from --changed-from")
    args = parser.parse_args(argv)
    try:
        return lint(ROOT, args.hadolint, args.changed_from, args.changed_only)
    except LintError as error:
        print(f"FAIL dockerfile lint: {error}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    sys.exit(main())
