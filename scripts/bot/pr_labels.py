#!/usr/bin/env python3
"""The size label and the risk label of a pull request, and what each is computed from.

Rules only: nothing here talks to GitHub. The caller gives the files that the
pull request changes, each with its lines added and removed, and whether the
author is new. The settings are the block `pull_requests` of
.github/atlas-bot.yml.

Size: the changed lines (added and removed) of every file that counts. Lock
files, tests and documents do not count (`not_counted`). The label is the
largest size whose first number of lines is not above that sum.

High risk, when one of these holds:
  - the pull request changes a core path (`core_paths`: the agent loop, the
    tool handlers, the guards);
  - its size is `high_risk_from` or larger;
  - it is the author's first pull request here.

A change to a workflow is not high risk by this label: the integrity check
names it on the pull request, for a maintainer's approval. So `risk:high`
means a change that can alter what the product does.

Both labels are computed again each time. A label that was set or removed by
hand is put back as the rules say. What each label is computed from stands in
its description on GitHub (`descriptions`; scripts/setup/labels.sh sets it).
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


def counts(path: str, settings: dict) -> bool:
    """Whether the lines of a file count for the size. An entry of `not_counted` is the end of a path, or a folder at
    any depth with all that is in it (the entry ends with `/`)."""
    for entry in settings.get("not_counted") or ():
        if ("/" + entry in "/" + path) if entry.endswith("/") else path.endswith(entry):
            return False
    return True


def counted_lines(files: list, settings: dict) -> int:
    """The changed lines of the pull request, in the files that count."""
    return sum(int(f.get("additions") or 0) + int(f.get("deletions") or 0) for f in files if counts(f["filename"], settings))


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
        reasons.append(f"it has {lines} counted lines ({first_large} or more)")
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


def descriptions(settings: dict) -> dict:
    """What each label says about itself on GitHub: what it is computed from, in at most 100 characters.

    The reason stands where a reviewer looks, on the label. scripts/setup/labels.sh holds these texts, and a test
    keeps them the same as the settings.
    """
    kinds = list(dict.fromkeys((settings.get("not_counted") or {}).values()))
    left_out = ", ".join(kinds[:-1]) + " and " + kinds[-1] if len(kinds) > 1 else "".join(kinds)
    steps = sorted(settings["sizes"].items(), key=lambda step: step[1])
    said = {}
    for (name, first), after in zip(steps, [*steps[1:], None]):
        lines = f"{first:,} or more" if after is None else f"Under {after[1]:,}" if first == 0 else f"{first:,} to {after[1] - 1:,}"
        said[SIZE_PREFIX + name] = f"{lines} changed lines" + (f", without {left_out}" if left_out else "")
    parts = list(dict.fromkeys(what[4:] if what.startswith("the ") else what for what in (settings.get("core_paths") or {}).values()))
    first_large = settings["sizes"][settings["high_risk_from"]]
    said[settings["risk_label"]] = f"A core path ({', '.join(parts)}), {first_large:,} or more counted lines, or a first pull request"
    return said
