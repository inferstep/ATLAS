"""Write the expected side of a recording again, from the proxy of this checkout and the same recorded answers.

    python -m tests.replay.rewrite <case>|all

For a change that is meant to alter what the proxy does on a recorded
session: a text the model reads, a field of a request, an event. The answers
of the four services stay as they were recorded. What is written again is
what the proxy sends (the text of each request), the events and the end
files, so the diff of the recording shows exactly what the change altered.

It cannot follow a change in WHICH calls the proxy makes or in their order:
then the recorded answers no longer fit, and the session has to be recorded
again (tests/replay/record.py). It says so and writes nothing.
"""
from __future__ import annotations

import argparse
import copy
import tempfile
from pathlib import Path

from tests.replay import recording, stage


def rewritten(made: dict, binary: Path, root: Path):
    """The recording with its expected side from this binary, and what kept it from being written (or None)."""
    new = copy.deepcopy(made)
    result = recording.run(new, binary, root, accept=True)
    if result["mismatch"] or result["not_asked"]:
        return new, recording.differences(new, result)[0]
    new["expected"] = {"events": result["events"], "files": result["files"]}
    return new, None


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("case", choices=recording.names() + ["all"])
    args = parser.parse_args()
    failed = 0
    with tempfile.TemporaryDirectory() as tmp:
        binary = stage.build_proxy(Path(tmp))
        for name in recording.names() if args.case == "all" else [args.case]:
            made = recording.load(name)
            new, problem = rewritten(made, binary, Path(tmp) / name)
            if problem:
                failed += 1
                print(f"{name}: NOT written. {problem}\n  fix: the calls of the proxy changed, so the recorded answers no "
                      f"longer fit. Record the session again: python -m tests.replay.record {name}")
            elif new == made:
                print(f"{name}: no change")
            else:
                recording.save(name, new)
                requests = sum(1 for old, now in zip(made["exchanges"], new["exchanges"]) if old != now)
                print(f"{name}: written again ({requests} request(s) changed; events "
                      f"{'changed' if made['expected']['events'] != new['expected']['events'] else 'the same'}; files "
                      f"{'changed' if made['expected']['files'] != new['expected']['files'] else 'the same'})")
    return int(bool(failed))


if __name__ == "__main__":
    raise SystemExit(main())
