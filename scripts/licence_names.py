#!/usr/bin/env python3
"""Fail the dependency review for a new dependency whose licence GitHub cannot name.

The review action judges a licence that it can read against the allowed
list (.github/dependency-review-config.yml). For a dependency with no named
licence it only writes a note and passes. This reads what the action found
and fails for each such dependency that the pull request adds, unless the
package is on the list `allow-dependencies-licenses` of that file (read by
hand, with its licence beside it) or is a GitHub Action.

Reads INVALID_LICENSE_CHANGES: the action's output `invalid-license-changes`.

The workflow runs the base branch's copy of this script, so a change cannot
rewrite its own judge. The settings file is the change's copy (--settings):
it is meant to be changed in a pull request, with a maintainer's approval.

Exit status: 0 when no new dependency is without a named licence; 1 when one
is; 2 when the action's output or the settings file cannot be read.
"""
from __future__ import annotations

import argparse
import json
import os
import re
import sys
from pathlib import Path

SETTINGS = Path(__file__).resolve().parents[1] / ".github" / "dependency-review-config.yml"
# GitHub's data names no licence for any action, and an action is run by a workflow, not shipped.
ACTIONS = "pkg:githubactions/"


def name_of(purl: str) -> str:
    """A package address without its version and the parts after it."""
    return re.split(r"[@?#]", purl or "", maxsplit=1)[0]


def read_by_hand(text: str) -> set:
    """The packages of `allow-dependencies-licenses` in the settings file, each without a version."""
    block = re.search(r"^allow-dependencies-licenses:\n((?:[ \t]+.*\n|[ \t]*\n)*)", text, re.M)
    if not block:
        raise ValueError("the settings file has no list `allow-dependencies-licenses`")
    return {name_of(entry) for entry in re.findall(r"^[ \t]+-[ \t]+['\"]?(pkg:[^\s'\"#]+)", block.group(1), re.M)}


def not_named(found: dict, by_hand: set) -> list:
    """The added dependencies with no named licence that are neither read by hand nor an action."""
    out = []
    for change in found.get("unlicensed") or []:
        purl = change.get("package_url") or ""
        if change.get("change_type") != "added" or purl.startswith(ACTIONS) or name_of(purl) in by_hand:
            continue
        out.append(change)
    return out


def main(argv: list | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--settings", type=Path, default=SETTINGS, help="the settings file of the review, as the change has it")
    settings = parser.parse_args(argv).settings
    try:
        found = json.loads(os.environ["INVALID_LICENSE_CHANGES"])
        by_hand = read_by_hand(settings.read_text(encoding="utf-8"))
        if not isinstance(found, dict):
            raise ValueError("it is not a JSON object")
    except (KeyError, ValueError, OSError) as error:
        print(f"licence names: what the review action found, or the settings file, cannot be read: {error!r}. Fix: the "
              "step must get the action's output `invalid-license-changes` in INVALID_LICENSE_CHANGES, and "
              f"{settings} must have the list `allow-dependencies-licenses`.", file=sys.stderr)
        return 2
    missing = not_named(found, by_hand)
    if not missing:
        print("licence names: every dependency that this change adds has a named licence, or was read by hand.")
        return 0
    for change in missing:
        print(f"::error title=no named licence::{change.get('name')} {change.get('version')} ({change.get('manifest')}): "
              "GitHub's data names no licence for it, so the review cannot judge it. Fix: read the package's licence. "
              "When a work under AGPL-3.0 can take it in, add the package to `allow-dependencies-licenses` in "
              f".github/{SETTINGS.name}, with the licence beside it. When it cannot, take another package.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
