"""Aggregates over session records: rates with intervals, and no task text."""
from __future__ import annotations

import math
from collections import Counter, defaultdict

from grading import GRADER_ERROR, PASS
from result import COMPLETED

Z95 = 1.959964


def wilson(k: int, n: int, z: float = Z95) -> tuple:
    """The Wilson score interval for k successes in n trials."""
    if n == 0:
        return (0.0, 1.0)
    p = k / n
    centre = (p + z * z / (2 * n)) / (1 + z * z / n)
    half = z * math.sqrt(p * (1 - p) / n + z * z / (4 * n * n)) / (1 + z * z / n)
    return (max(0.0, centre - half), min(1.0, centre + half))


def difference(k1: int, n1: int, k2: int, n2: int) -> tuple:
    """p1 - p2 with Newcombe's hybrid score interval."""
    p1, p2 = k1 / n1, k2 / n2
    l1, u1 = wilson(k1, n1)
    l2, u2 = wilson(k2, n2)
    d = p1 - p2
    return (d, d - math.sqrt((p1 - l1) ** 2 + (u2 - p2) ** 2),
            d + math.sqrt((u1 - p1) ** 2 + (p2 - l2) ** 2))


def arm_summary(records: list) -> dict:
    """One arm's numbers. Grader errors are counted apart and graded neither way."""
    graded = [r for r in records if r["grade"] != GRADER_ERROR]
    passed = sum(1 for r in graded if r["grade"] == PASS)
    completed = [r for r in graded if r["status"] == COMPLETED]
    false_completed = sum(1 for r in completed if r["grade"] != PASS)
    return {
        "sessions": len(records),
        "grader_errors": len(records) - len(graded),
        "passed": passed, "graded": len(graded),
        "pass_rate": _rate(passed, len(graded)),
        "completed": len(completed),
        "false_completed": false_completed,
        "false_completed_rate": _rate(false_completed, len(completed)),
        "passed_not_completed": sum(1 for r in graded
                                    if r["grade"] == PASS and r["status"] != COMPLETED),
        "endings_of_failures": dict(Counter(f"{r['status']}/{r['reason']}"
                                            for r in graded if r["grade"] != PASS)),
        "tasks_whose_repeats_disagree": _disagreeing_tasks(graded),
        "pass_rate_by_kind": _by_kind(graded),
    }


def compare(atlas: list, baseline: list) -> dict:
    """Both arms, and ATLAS's pass rate minus the baseline's."""
    a, b = arm_summary(atlas), arm_summary(baseline)
    out = {"atlas": a, "baseline": b}
    if a["graded"] and b["graded"]:
        d, lo, hi = difference(a["passed"], a["graded"], b["passed"], b["graded"])
        out["pass_rate_difference"] = {"estimate": round(d, 4), "ci95": [round(lo, 4), round(hi, 4)]}
    return out


def _rate(k: int, n: int) -> dict:
    lo, hi = wilson(k, n)
    return {"k": k, "n": n, "rate": round(k / n, 4) if n else None,
            "ci95": [round(lo, 4), round(hi, 4)]}


def _by_kind(graded: list) -> dict:
    """Pass rates per suite category. A kind is a label the suite gives to
    several tasks, never a task's name."""
    kinds = defaultdict(list)
    for r in graded:
        kinds[r.get("kind") or "unlabelled"].append(r)
    return {k: _rate(sum(1 for r in rs if r["grade"] == PASS), len(rs))
            for k, rs in sorted(kinds.items())}


def _disagreeing_tasks(records: list) -> int:
    """Tasks whose repeats did not all grade the same: a count, never a name."""
    grades = defaultdict(set)
    for r in records:
        grades[r["task"]].add(r["grade"])
    return sum(1 for g in grades.values() if len(g) > 1)
