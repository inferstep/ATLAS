"""How much of the proxy the recorded sessions run through.

    python -m tests.replay.reach            the share of statements, in all and by file
    python -m tests.replay.reach --checks   and the check functions that no case reaches

"The replays are the same" proves only as much as the recordings touch. This
builds the proxy with Go's coverage for binaries, replays every recording, and
counts the statements that ran. A check function is one whose name says that
it judges something (gate, check, valid, verify, refuse, deny, guard, detect,
allow, permission, contain).
"""
from __future__ import annotations

import argparse
import re
import subprocess
import tempfile
from collections import defaultdict
from pathlib import Path

from tests.replay import recording, stage

CHECK_NAME = re.compile(r"gate|check|valid|verif|refus|deny|denie|guard|detect|allow|permission|contain", re.I)
BLOCK = re.compile(r"^(?P<file>[^:]+):[\d.]+,[\d.]+ (?P<statements>\d+) (?P<count>\d+)$")
FUNCTION = re.compile(r"^(?P<file>[^:]+):\d+:\s+(?P<name>\S+)\s+(?P<share>[\d.]+)%$")


def measure(root: Path) -> tuple[dict, list]:
    """Replay every recording with a coverage build. Returns statements by file and the share of each function."""
    binary, data = root / "atlas-proxy-cover", root / "coverage"
    data.mkdir()
    subprocess.run(["go", "build", "-cover", "-o", str(binary), "."], cwd=stage.REPO / "proxy", check=True)
    for name in recording.names():
        recording.run(recording.load(name), binary, root / name, more_env={"GOCOVERDIR": str(data)})
    profile = root / "profile.txt"
    subprocess.run(["go", "tool", "covdata", "textfmt", f"-i={data}", f"-o={profile}"], cwd=stage.REPO / "proxy", check=True)
    by_file = defaultdict(lambda: [0, 0])
    for line in profile.read_text(encoding="utf-8").splitlines():
        block = BLOCK.match(line)
        if block:
            totals = by_file[Path(block["file"]).name]
            totals[0] += int(block["statements"])
            totals[1] += int(block["statements"]) if int(block["count"]) else 0
    listed = subprocess.run(["go", "tool", "cover", f"-func={profile}"], cwd=stage.REPO / "proxy", capture_output=True,
                            text=True, check=True).stdout
    functions = [(Path(m["file"]).name, m["name"], float(m["share"])) for m in map(FUNCTION.match, listed.splitlines()) if m]
    return dict(by_file), functions


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--checks", action="store_true", help="also list the check functions that no case reaches")
    args = parser.parse_args()
    with tempfile.TemporaryDirectory() as tmp:
        by_file, functions = measure(Path(tmp))
    total = sum(t for t, _ in by_file.values())
    reached = sum(r for _, r in by_file.values())
    print(f"{len(recording.names())} recorded session(s) run through {reached:,} of {total:,} statements of the proxy "
          f"({100 * reached / total:.1f}%)")
    for name, (statements, hit) in sorted(by_file.items(), key=lambda item: -item[1][0]):
        print(f"  {name:34} {hit:6,} of {statements:6,}  {100 * hit / statements:5.1f}%")
    checks = [(file, name, share) for file, name, share in functions if CHECK_NAME.search(name)]
    untouched = [(file, name) for file, name, share in checks if share == 0]
    print(f"check functions: {len(checks) - len(untouched)} of {len(checks)} are reached by at least one case")
    if args.checks:
        for file, name in sorted(untouched):
            print(f"  not reached: {file}: {name}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
