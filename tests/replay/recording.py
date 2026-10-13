"""A recording of one session, and how a run of the proxy is compared with it.

A recording is one JSON file. It holds where it came from, the files the
session starts with, the request, every exchange between the proxy and its
four services in the order the proxy made them, and what the proxy is
expected to do: the events it sends to the client and the files it leaves.
"""
from __future__ import annotations

import json
from pathlib import Path

from tests.replay import stage

RECORDINGS = Path(__file__).with_name("recordings")
KINDS = ("e2e", "smoke", "development")
REPLIES = ("written", "captured")
# What an event carries that differs from run to run. Everything else in an
# event is compared, so a field that is added later is compared too.
#   the six time fields   how long a step took, as the proxy measured it. A
#                         time the lens reports (latency_ms) is not one of
#                         them: it comes from the recorded answer.
#   prompt_tokens         an estimate from the length of the prompt, and the
#                         prompt holds the path of the workspace, which is as
#                         long as the machine makes it. The prompt itself is
#                         compared in full, as the request to the model.
VARIES_IN_EVENTS = ("elapsed", "elapsed_ms", "ms", "prompt_ms", "first_frame_ms", "total_duration_ms", "prompt_tokens")


def load(name: str) -> dict:
    return json.loads((RECORDINGS / f"{name}.json").read_text(encoding="utf-8"))


def save(name: str, recording: dict) -> Path:
    path = RECORDINGS / f"{name}.json"
    path.write_text(json.dumps(recording, indent=1, ensure_ascii=False) + "\n", encoding="utf-8")
    return path


def names() -> list[str]:
    return sorted(path.stem for path in RECORDINGS.glob("*.json"))


def lay_files(workspace: Path, files: dict[str, str]) -> None:
    for name, content in files.items():
        path = workspace / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content, encoding="utf-8")


def files_of(workspace: Path) -> dict[str, str]:
    """Every file in the workspace, by its path, except the proxy's own probe."""
    out = {}
    for path in sorted(workspace.rglob("*")):
        if path.is_file() and path.name != stage.PROBE_FILE:
            out[path.relative_to(workspace).as_posix()] = path.read_text(encoding="utf-8", errors="replace")
    return out


def plain_events(events: list[dict], st: stage.Stage) -> list[dict]:
    """The events without what differs from run to run, as they are kept in a recording."""
    out = []
    for event in events:
        data = event.get("data")
        if isinstance(data, dict):
            event = {**event, "data": {key: value for key, value in data.items() if key not in VARIES_IN_EVENTS}}
        out.append(json.loads(st.plain(json.dumps(event, ensure_ascii=False))))
    return out


def run(recording: dict, binary: Path, root: Path, upstreams=None, accept=False, more_env=None) -> dict:
    """Run the proxy once on the recording's files and request.

    With upstreams the services are those functions and the exchanges are
    kept (recording). Without, the services play the recording (replay); with
    accept, a request that differs only in its text is taken into the
    recording. Returns the exchanges, the events, the end files, the first
    mismatch, and how many requests were accepted.
    """
    workspace, home = root / "workspace", root / "home"
    workspace.mkdir(parents=True)
    home.mkdir()
    workspace = workspace.resolve()
    lay_files(workspace, recording["files"])
    st = stage.Stage(upstreams=upstreams, recording=None if upstreams else recording, workspace=workspace,
                     accept=accept)
    ports = st.start()
    port, process = stage.start_proxy(binary, ports, home, more_env)
    try:
        request = json.loads(json.dumps(recording["request"]).replace(stage.WORKSPACE, str(workspace)))
        events = stage.drive(port, request)
    finally:
        process.terminate()
        process.communicate(timeout=20)
        st.stop()
    return {"exchanges": st.exchanges, "events": plain_events(events, st), "files": files_of(workspace),
            "mismatch": st.mismatch, "not_asked": st.not_asked(), "accepted": st.accepted}


def first_difference(expected: str, got: str) -> str:
    """Where two texts part, with a little of both sides."""
    at = next((i for i, (a, b) in enumerate(zip(expected, got)) if a != b), min(len(expected), len(got)))
    return (f"first difference at character {at}:\n  recorded: ...{expected[max(0, at - 80):at + 80]!r}\n"
            f"  now:      ...{got[max(0, at - 80):at + 80]!r}")


def differences(recording: dict, result: dict) -> list[str]:
    """Everything in which a replay differs from its recording, in the order a reader should look at it."""
    out = []
    mismatch = result["mismatch"]
    if mismatch and mismatch["expected"] is None:
        got = mismatch["got"]
        out.append(f"the proxy sent a request to the {mismatch['service']} that the recording does not have: "
                   f"{got['method']} {got['path']} {got['request'][:300]}")
    elif mismatch:
        expected, got = mismatch["expected"], mismatch["got"]
        number = len([e for e in result["exchanges"] if e["service"] == expected["service"]]) + 1
        where = f"request {number} to the {expected['service']} ({expected['method']} {expected['path']})"
        if expected["service"] != got["service"]:
            out.append(f"the proxy called the {got['service']} ({got['method']} {got['path']}) where the recording "
                       f"has a call to the {expected['service']} ({expected['method']} {expected['path']}): the order "
                       "of its calls changed")
        elif (expected["method"], expected["path"]) != (got["method"], got["path"]):
            out.append(f"{where} is now {got['method']} {got['path']}")
        else:
            out.append(f"{where} differs from the recording; " + first_difference(expected["request"], got["request"]))
    for service, count in result["not_asked"].items():
        if not mismatch:
            out.append(f"the proxy did not send {count} recorded request(s) to the {service}")
    expected_events, events = recording["expected"]["events"], result["events"]
    for number, (expected, got) in enumerate(zip(expected_events, events), 1):
        if expected != got:
            out.append(f"event {number} ({expected.get('type')}) differs; " + first_difference(
                json.dumps(expected, ensure_ascii=False, sort_keys=True), json.dumps(got, ensure_ascii=False, sort_keys=True)))
            break
    else:
        if len(expected_events) != len(events):
            out.append(f"the proxy sent {len(events)} event(s) and the recording has {len(expected_events)}; the first "
                       f"one without a partner is {(events + expected_events)[min(len(events), len(expected_events))]}")
    if recording["expected"]["files"] != result["files"]:
        names_ = sorted(set(recording["expected"]["files"]) | set(result["files"]))
        changed = [n for n in names_ if recording["expected"]["files"].get(n) != result["files"].get(n)]
        out.append(f"the files at the end differ from the recording: {', '.join(changed)}")
    return out
