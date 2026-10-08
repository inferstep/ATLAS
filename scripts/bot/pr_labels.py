#!/usr/bin/env python3
"""The size label and the risk label of a pull request, and what each is computed from.

Rules only: nothing here talks to GitHub. The caller gives the files that the
pull request changes, each with its lines added and removed, and whether the
author is new. The settings are the block `pull_requests` of
.github/atlas-bot.yml.

Size: the changed lines (added and removed) of every file, except the files
that a tool writes (`not_counted`). The label is the largest size whose first
number of lines is not above that sum.

High risk, when one of these holds:
  - the pull request changes a core path (`core_paths`: the agent loop, the
    tool handlers, the guards, the workflows);
  - its size is `high_risk_from` or larger;
  - it is the author's first pull request here.

Both labels are computed again each time. A label that was set or removed by
hand is put back as the rules say.
"""
from __future__ import annotations

from typing import NamedTuple

SIZE_PREFIX = "size/"


class Labels(NamedTuple):
    size: str                 # the size label, for example "size/M"
    lines: int                # the changed lines that were counted
    reasons: tuple            # why the change is high risk, in words; empty when it is not

    @property
    def high_risk(self) -> bool:
        return bool(self.reasons)


def counted_lines(files: list, settings: dict) -> int:
    """The changed lines of the pull request, without the files that a tool writes."""
    left_out = tuple(settings.get("not_counted") or ())
    return sum(int(f.get("additions") or 0) + int(f.get("deletions") or 0)
               for f in files if not (left_out and f["filename"].endswith(left_out)))


def size_of(lines: int, settings: dict) -> str:
    """The name of the largest size whose first number of lines is not above `lines`."""
    steps = sorted(settings["sizes"].items(), key=lambda step: step[1])
    return [name for name, first in steps if lines >= first][-1]


def is_under(path: str, entry: str) -> bool:
    """Whether a file belongs to an entry of `core_paths`: a folder (the entry ends with `/`), the start of a name
    (it ends with `*`), or one file."""
    if entry.endswith("/"):
        return path.startswith(entry)
    if entry.endswith("*"):
        return path.startswith(entry[:-1])
    return path == entry


def core_parts(files: list, settings: dict) -> list:
    """The core parts that the pull request changes: (what it is, the first changed file of it), in the settings' order."""
    found: dict = {}
    for entry, what in (settings.get("core_paths") or {}).items():
        for f in files:
            if is_under(f["filename"], entry):
                found.setdefault(what, f["filename"])
                break
    return list(found.items())


def labels_for(files: list, first_time: bool, settings: dict) -> Labels:
    """The labels of a pull request that changes these files. `first_time`: it is the author's first one here."""
    lines = counted_lines(files, settings)
    size = size_of(lines, settings)
    reasons = [f"it changes {what} ({path})" for what, path in core_parts(files, settings)]
    first_large = settings["sizes"][settings["high_risk_from"]]
    if lines >= first_large:
        reasons.append(f"it has {lines} changed lines ({first_large} or more)")
    if first_time:
        reasons.append("it is the author's first pull request here")
    return Labels(SIZE_PREFIX + size, lines, tuple(reasons))


def label_changes(have: set, want: Labels, settings: dict) -> tuple:
    """The labels to add and the labels to remove, so that the pull request has the labels of `want`.

    Only the size labels and the risk label are touched; every other label stays.
    """
    risk = settings["risk_label"]
    right = {want.size} | ({risk} if want.high_risk else set())
    ours = {label for label in have if label.startswith(SIZE_PREFIX) or label == risk}
    return sorted(right - have), sorted(ours - right)
