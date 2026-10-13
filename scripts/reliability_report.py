"""The e2e-reliability.py run summary, and what the runner keeps on disk as it goes.

Split out of scripts/e2e-reliability.py (#222). A run that was stopped part way
used to lose everything: the defect text was printed only in the final summary,
and nothing handled SIGINT or SIGTERM. Now each session's defects are appended
to a JSONL file as the session ends, and a stop writes the partial results and
the summary before the runner exits.
"""
from __future__ import annotations

import contextlib
import json
import signal
from collections.abc import Callable, Iterator
from pathlib import Path
from typing import Any


class Stopped(Exception):
    """Raised by the SIGINT/SIGTERM handler, so the runner can keep what it has."""

    def __init__(self, signum: int) -> None:
        super().__init__(signum)
        self.signum = signum


@contextlib.contextmanager
def stop_on_signals() -> Iterator[None]:
    """Turn SIGINT and SIGTERM into Stopped for the duration, then restore."""
    def handler(signum: int, frame: Any) -> None:
        raise Stopped(signum)
    previous = {sig: signal.signal(sig, handler) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        yield
    finally:
        for sig, h in previous.items():
            signal.signal(sig, h)


def sibling(json_out: str, suffix: str) -> Path | None:
    """`out.json` -> `out<suffix>`, next to the run's JSON result; None without --json."""
    return Path(json_out).with_suffix(suffix) if json_out else None


def log_defects(path: Path | None, task: str, rep: int, defects: list[str]) -> None:
    """Append one JSON line per defect, as the session ends."""
    if path is None or not defects:
        return
    with path.open("a", encoding="utf-8") as fh:
        for d in defects:
            fh.write(json.dumps({"task": task, "rep": rep, "defect": d}) + "\n")


def write_summary(path: Path | None, sessions: list, known: set[str],
                  guards_of: Callable[[Any], list[str]], tail: list[str]) -> None:
    """The summary report() prints, written to `path` for a run that was stopped."""
    if path is None or not sessions:
        return
    with path.open("w", encoding="utf-8") as fh, contextlib.redirect_stdout(fh):
        report(sessions, known, guards_of)
        print("\n".join(tail))


def report(sessions: list, known: set[str], guards_of: Callable[[Any], list[str]]) -> None:
    """Print the run summary: rates, defects by class, per task, code quality.

    `guards_of` is e2e-reliability.py's model_output_guards, passed in so this
    module does not import the runner."""
    total = len(sessions)
    clean = sum(1 for s in sessions if not s.defects)
    passed = sum(1 for s in sessions if s.task_passed)
    print("\n" + "=" * 72)
    print(f"Harness Integrity Rate   {clean}/{total} "
          f"({100.0 * clean / total:.0f}%)   <- ATLAS's own plumbing")
    print(f"Task Success Rate        {passed}/{total} "
          f"({100.0 * passed / total:.0f}%)   <- task outcome (cause not classified)")
    v3 = [s.v3 for s in sessions]
    writes = sum(x["write_calls"] for x in v3)
    generated = sum(x["generated"] for x in v3)
    delivered = sum(x["delivered"] for x in v3)
    print(f"V3 generation            ran on {generated}/{writes} write calls, "
          f"delivered {delivered} candidate(s)")
    if writes and not generated:
        print("  ! no write reached V3 generation: this run measured the "
              "agent loop without V3")
    print("=" * 72)

    by_class: dict[str, int] = {}
    for s in sessions:
        for d in s.defects:
            by_class[d.split(":")[0]] = by_class.get(d.split(":")[0], 0) + 1
    if by_class:
        print("\nHarness defects by class:")
        for cls, cnt in sorted(by_class.items(), key=lambda kv: -kv[1]):
            print(f"  {cnt:3d}  {cls}")
    else:
        print("\nNo harness defects detected.")
    guards = [guards_of(s) for s in sessions]
    if any(guards):
        kinds: dict[str, int] = {}
        for g in guards:
            for k in g:
                kinds[k] = kinds.get(k, 0) + 1
        print(f"Model-output guards (the proxy caught malformed model output; "
              f"not harness defects): {sum(len(g) for g in guards)} in "
              f"{sum(1 for g in guards if g)} session(s): "
              + ", ".join(f"{k} {n}" for k, n in sorted(kinds.items())))

    unstable = [s for s in sessions if s.stack_changes]
    if unstable:
        print(f"Stack not stable in {len(unstable)}/{total} session(s): the outcomes "
              f"of these ran over a restart, an OOM kill or a missing container:")
        for s in unstable:
            print(f"  {s.task} rep {s.rep}: {'; '.join(s.stack_changes)}")

    print("\nPer task:")
    for name in sorted({s.task for s in sessions}):
        rows = [s for s in sessions if s.task == name]
        cl = sum(1 for s in rows if not s.defects)
        pa = sum(1 for s in rows if s.task_passed)
        turns = [len(s.of_type("turn_start")) for s in rows]
        print(f"  {name:14s} harness {cl}/{len(rows)}  task {pa}/{len(rows)}  "
              f"turns min/med/max {min(turns)}/{sorted(turns)[len(turns)//2]}/{max(turns)}")

    q = [s.quality for s in sessions if s.quality and "error" not in s.quality]
    if q:
        print("\nCode quality of what the agent wrote:")
        worst_cx = max(q, key=lambda r: r.get("max_complexity", 0))
        worst_fn = max(q, key=lambda r: r.get("max_function_lines", 0))
        worst_file = max(q, key=lambda r: r.get("max_file_lines", 0))
        defects = sum(r.get("lint_defects", 0) for r in q)
        style = sum(r.get("lint_style", 0) for r in q)
        unused = sum(r.get("unused_imports", 0) for r in q)
        broken = sum(len(r.get("syntax_errors") or []) for r in q)
        clean = sum(1 for r in q if not r.get("findings"))
        print(f"  sessions with no quality finding   {clean}/{len(q)}")
        print(f"  worst function complexity          {worst_cx.get('max_complexity', 0)}"
              f" ({worst_cx.get('max_complexity_where') or 'n/a'})")
        print(f"  longest function                   {worst_fn.get('max_function_lines', 0)} lines"
              f" ({worst_fn.get('max_function_where') or 'n/a'})")
        print(f"  longest file                       {worst_file.get('max_file_lines', 0)} lines"
              f" ({worst_file.get('max_file_where') or 'n/a'})")
        codes = sorted({c for r in q for c in (r.get("defect_codes") or [])})
        print(f"  real lint defects                  {defects}"
              + (f" {codes}" if codes else ""))
        print(f"  unused imports                     {unused}")
        print(f"  style nits (not scored)            {style}")
        print(f"  files left unparseable             {broken}")

    observed: set[str] = set()
    for s in sessions:
        observed |= {e.get("type") for e in s.events if e.get("type")}
    unrendered = sorted(observed - known)
    print(f"\nTUI coverage: {len(observed)} event types emitted, "
          f"{len(unrendered)} the TUI cannot render"
          + (f": {unrendered}" if unrendered else ""))

