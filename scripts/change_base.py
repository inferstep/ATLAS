#!/usr/bin/env python3
"""The commit a check compares a change with.

A job of a pull request checks out the merge of the pull request into its
base branch as that branch is now. The commit to compare with is the first
parent of that merge: the base branch as it is now. The base commit that the
event names is the branch as it was when the event was made. Compared with
that one, everything the base branch got since reads as part of the pull
request.

In the merge queue the checked-out commit has one parent, the base the queue
built it on, and the event names that same commit.

This prints the commit. It stops with a message when the checkout is not what
the rule expects, and never falls back to the event's base.

Usage: change_base.py          (in a job of a pull_request or merge_group event)
"""
from __future__ import annotations

import json
import os
import subprocess
import sys
from pathlib import Path

CHECKOUT = ("Fix: let the checkout step take the event's own commit (no `ref:`), with `fetch-depth: 2` or more, "
            "so the parents of the commit are there.")


class NotTheExpectedCheckout(Exception):
    """The checked-out commit is not the one the rule reads its base from."""


def parents(root: Path) -> tuple[str, list[str]]:
    """The checked-out commit and its parents."""
    done = subprocess.run(["git", "-C", str(root), "rev-list", "--parents", "-n", "1", "HEAD"],
                          capture_output=True, text=True, check=False)
    if done.returncode != 0 or not done.stdout.split():
        raise NotTheExpectedCheckout(f"git cannot read the checked-out commit: {done.stderr.strip()} {CHECKOUT}")
    commit, *rest = done.stdout.split()
    return commit, rest


def base_of(root: Path, event: str, payload: dict) -> str:
    """The commit to compare the change with, by the rule above."""
    commit, found = parents(root)
    if event == "pull_request":
        head = payload["pull_request"]["head"]["sha"]
        if len(found) != 2 or found[1] != head:
            raise NotTheExpectedCheckout(
                f"the checked-out commit {commit[:12]} is not the merge of this pull request into its base branch: "
                f"it has {len(found)} parent(s) ({', '.join(p[:12] for p in found) or 'none'}), and the rule expects "
                f"two, the second of them the head of the pull request ({head[:12]}). So the base cannot be read "
                f"from it, and no other base is taken in its place. {CHECKOUT}")
        return found[0]
    if event == "merge_group":
        base = payload["merge_group"]["base_sha"]
        if found[:1] != [base]:
            raise NotTheExpectedCheckout(
                f"the checked-out commit {commit[:12]} is not built on the base the merge queue names "
                f"({base[:12]}): its parent(s) are {', '.join(p[:12] for p in found) or 'none'}. {CHECKOUT}")
        return base
    raise NotTheExpectedCheckout(f"the event {event!r} is not one this rule reads (pull_request, merge_group). "
                                 "Fix: use it only in a job of one of those two events.")


def main() -> int:
    try:
        payload = json.loads(Path(os.environ["GITHUB_EVENT_PATH"]).read_text(encoding="utf-8"))
        print(base_of(Path.cwd(), os.environ["GITHUB_EVENT_NAME"], payload))
    except (KeyError, ValueError, OSError) as error:
        print(f"FAIL change base: the event cannot be read: {error!r}. Fix: run this in a GitHub Actions job of a "
              "pull_request or merge_group event.", file=sys.stderr)
        return 2
    except NotTheExpectedCheckout as error:
        print(f"FAIL change base: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
