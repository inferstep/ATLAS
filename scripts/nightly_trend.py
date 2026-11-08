#!/usr/bin/env python3
"""The trend of the nightly run: the seconds and the tokens of each task, compared with the nights before.

The file beside this one, scripts/nightly_run.py, loads it. It starts nothing and reads no file: the run gives it
the reports.

One slow night says little, because the model and chance move the numbers. So a change counts only when it stays.
For each task and each measure, the median of the WINDOW counted nights before the last LAST nights is the value of
before. A change stayed when each of the last LAST nights is more than RATIO times that median, or each is less than
1 / RATIO times it. A median needs the values of AT_LEAST nights. The nights are counted as docs/project/gates.md
says under "Counting nights": a smoke run and a night that is `not run` do not count, and only the first run of a
night counts.
"""
from __future__ import annotations

import re
import statistics

# The limits are a start: each report keeps its ratios, so that the limits can be set from the data later.
WINDOW, LAST, AT_LEAST, RATIO = 7, 3, 5, 1.5
MEASURES = ("seconds", "tokens")
TOO_FEW, STAYED, NO_CHANGE = "too few nights", "a change stayed", "no change stayed"


def counted_nights(reports: list) -> list:
    """The reports that count, oldest first: for each night (the date of its start in UTC), the first run whose
    result is not `not run`. `reports` is oldest first."""
    first = {}
    for report in reports:
        if report.get("mode") == "night" and not str(report.get("result", "")).startswith("not run"):
            first.setdefault(str(report.get("started", ""))[:10], report)
    return list(first.values())


def value_of(report: dict, task: str, measure: str):
    """The number of one task in one night, or None: the task did not run, its session had a harness defect, or the
    report has no such number (a report from before the run kept the tokens)."""
    rows = [row for row in report.get("tasks") or [] if isinstance(row, dict) and row.get("task") == task]
    value = rows[0].get(measure) if rows and not rows[0].get("defects") else None
    return value if isinstance(value, (int, float)) and value > 0 else None


def compared(nights: list, task: str, measure: str) -> dict:
    """One task and one measure over the counted nights. The last of `nights` is this night."""
    before = [value for value in (value_of(night, task, measure) for night in nights[-WINDOW - LAST:-LAST]) if value is not None]
    if len(before) < AT_LEAST:
        return {"judged": TOO_FEW, "nights_with_a_value": len(before)}
    median = statistics.median(before)
    ratios = [None if value is None else round(value / median, 2) for value in (value_of(night, task, measure) for night in nights[-LAST:])]
    stayed = all(ratio is not None and ratio > RATIO for ratio in ratios) or all(ratio is not None and ratio < 1 / RATIO for ratio in ratios)
    return {"median": median, "ratios": ratios, "stayed": stayed}


def of(earlier: list, report: dict, tasks) -> dict:
    """The trend part of the report of a night. `earlier` holds the reports of the runs before it, oldest first."""
    nights = counted_nights([*earlier, report])
    if not nights or nights[-1] is not report:
        return {"judged": "not judged: this run is not the counted run of its night"}
    used = nights[-WINDOW - LAST:]
    found = {task: {measure: compared(used, task, measure) for measure in MEASURES} for task in tasks}
    entries = [entry for measures in found.values() for entry in measures.values()]
    if all(entry.get("judged") == TOO_FEW for entry in entries):
        judged = TOO_FEW
    else:
        judged = STAYED if any(entry.get("stayed") for entry in entries) else NO_CHANGE
    return {"judged": judged, "nights": [night.get("started") for night in used], "tasks": found}


def status_of(report: dict, name: str) -> tuple:
    """The trend status for the commit of the night, or None with the reason why it gets none.

    Red when a change stayed, green when none did, and none with too few nights. The text has fixed words, the names
    of the tasks and the measures, and numbers: the first change that stayed and the ratio of this night.
    """
    trend, commit = report.get("trend") or {}, str(report.get("commit", ""))
    if trend.get("judged") not in (STAYED, NO_CHANGE):
        return None, f"no status: {trend.get('judged')}"
    if not re.fullmatch(r"[0-9a-f]{40}", commit):
        return None, "no status: the report does not say which commit ran"
    changed = [f"{task} {measure}: {entry['ratios'][-1]:g} times the median" for task, measures in trend["tasks"].items()
               for measure, entry in measures.items() if entry.get("stayed")]
    said = [trend["judged"], f"started {report['started']}", *changed[:1]]
    said += [f"{len(changed) - 1} more in the report"] if len(changed) > 1 else []
    return {"context": name, "sha": commit, "state": "failure" if changed else "success", "description": "; ".join(said)}, ""
