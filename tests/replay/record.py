"""Make a recording: run the proxy once against services that answer, and keep every exchange.

    python -m tests.replay.record <case>

A case names its task and the set the task comes from (the e2e scenarios, the
smoke set or the development set; no other source is known here). The model
replies of a case are written by hand, one per call, so the bad replies of a
weak model can be put in front of the proxy on purpose. The sandbox is the
real executor of this checkout, started for the recording. V3 answers "not
available", as in the e2e acceptance job, and the lens answers with a healthy
score.

After the recording pass the same binary replays it once in a fresh
workspace. That replay must be clean, and the files it leaves are the
recording's expected files: at a replay no command really runs, so only what
the proxy itself writes can be expected.
"""
from __future__ import annotations

import argparse
import http.client
import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from tests.replay import recording, stage

BUGGY_APP = ('def greeting(name):\n    return "Hello, " + nmae\n\n\nif __name__ == "__main__":\n'
             '    print(greeting("world"))\n')

CASES = {
    "normal_edit": {
        "source": {"task": "the scenario of tests/e2e/test_acceptance.py (read, edit, check, done), with a run of the "
                           "program as its check", "kind": "e2e", "model_replies": "written"},
        "files": {"app.py": BUGGY_APP},
        "request": {"message": "Fix the NameError in app.py, then run it to check that it prints the greeting.",
                    "working_dir": stage.WORKSPACE, "mode": "default", "session_id": "replay-normal-edit",
                    "task_contract": {"task_mode": "work"}},
        "model": [
            {"type": "tool_call", "name": "read_file", "args": {"path": "app.py"}},
            {"type": "tool_call", "name": "edit_file", "args": {
                "path": "app.py", "old_str": 'return "Hello, " + nmae', "new_str": 'return "Hello, " + name'}},
            {"type": "tool_call", "name": "run_command", "args": {"command": "python3 app.py", "timeout": 30}},
            {"type": "done", "summary": "Fixed the NameError in app.py. It now prints the greeting."},
        ],
    },
}


def model_stream(reply: dict) -> str:
    """One model reply as the stream the model server sends."""
    lines = [json.dumps({"choices": [{"delta": {"content": json.dumps(reply)}}]}),
             json.dumps({"choices": [], "usage": {"total_tokens": 20, "prompt_tokens": 15, "completion_tokens": 5}}),
             "[DONE]"]
    return "".join(f"data: {line}\n\n" for line in lines)


def scripted_model(replies: list[dict]):
    calls = []

    def answer(method, path, body):
        calls.append(path)
        if len(calls) > len(replies):
            raise RuntimeError(f"the case has {len(replies)} model replies and the proxy asked for one more")
        return 200, "text/event-stream", model_stream(replies[len(calls) - 1])
    return answer


def lens(method, path, body):
    if path == "/internal/lens/score-per-step":
        return 200, "application/json", json.dumps({
            "enabled": True, "gx_available": True, "n_tokens": 12, "latency_ms": 1,
            "aggregate": {"first_off_rails_idx": -1, "gx_score_min": 0.9, "gx_score_mean": 0.92, "cx_norm_max": 0.3,
                          "cx_norm_mean": 0.2},
            "thresholds": {"off_rails": 0.34, "low": 0.34, "severe": 0.28}})
    return 200, "application/json", json.dumps({"status": "ok"})


def v3_not_available(method, path, body):
    return 503, "text/plain", "unavailable"


def pass_through(port: int, token: str):
    """Send each request on to a real service and give back its answer."""
    def answer(method, path, body):
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=120)
        connection.request(method, path, body.encode("utf-8"), {"Content-Type": "application/json",
                                                               "Authorization": f"Bearer {token}"})
        response = connection.getresponse()
        text = response.read().decode("utf-8", "replace")
        connection.close()
        return response.status, response.headers.get("Content-Type", "application/json"), text
    return answer


def start_sandbox(root: Path, workspace_root: Path):
    """The sandbox executor of this checkout, as the e2e acceptance job starts it."""
    port, token = stage.free_port(), root / "sandbox-token"
    token.write_text("replay-placeholder-token\n", encoding="utf-8")
    (root / "sandbox-scratch").mkdir()
    # The executor runs `python`; the interpreter that runs this recording provides it.
    path = os.pathsep.join([str(Path(sys.executable).parent), os.environ.get("PATH", "")])
    env = {**os.environ, "PATH": path, "WORKSPACE_BASE": str(root / "sandbox-scratch"), "MAX_EXECUTION_TIME": "60",
           "ATLAS_SANDBOX_WORKSPACE_ROOT": str(workspace_root), "ATLAS_SERVICE_TOKEN_FILE": str(token)}
    process = subprocess.Popen([sys.executable, "-m", "uvicorn", "executor_server:app", "--host", "127.0.0.1",
                                "--port", str(port), "--log-level", "error"], cwd=stage.REPO / "sandbox", env=env,
                               stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    deadline = time.time() + 30
    while time.time() < deadline:
        try:
            http.client.HTTPConnection("127.0.0.1", port, timeout=1).request("GET", "/health")
            return port, process
        except OSError:
            time.sleep(0.1)
    process.terminate()
    raise RuntimeError("the sandbox executor did not start: " + process.communicate(timeout=5)[1].decode()[-1500:])


def record(name: str) -> dict:
    case = CASES[name]
    commit = subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=stage.REPO, capture_output=True, text=True,
                            check=True).stdout.strip()
    made = {"source": {**case["source"], "commit": commit}, "files": case["files"], "request": case["request"]}
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp).resolve()
        binary = stage.build_proxy(root)
        (root / "record").mkdir()
        sandbox_port, sandbox = start_sandbox(root, root / "record")
        try:
            result = recording.run(made, binary, root / "record", upstreams={
                "model": scripted_model(case["model"]), "sandbox": pass_through(sandbox_port, "replay-placeholder-token"),
                "v3": v3_not_available, "lens": lens})
        finally:
            sandbox.terminate()
            sandbox.communicate(timeout=10)
        made.update(exchanges=result["exchanges"], expected={"events": result["events"], "files": {}})
        replayed = recording.run(made, binary, root / "replay")
        made["expected"]["files"] = replayed["files"]
        problems = recording.differences(made, replayed)
    if problems:
        raise SystemExit(f"the recording of {name} does not replay cleanly, so it was not written:\n  " + "\n  ".join(problems))
    return made


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("case", choices=sorted(CASES))
    args = parser.parse_args()
    path = recording.save(args.case, record(args.case))
    made = recording.load(args.case)
    print(f"wrote {path.relative_to(stage.REPO)}: {len(made['exchanges'])} exchange(s), "
          f"{len(made['expected']['events'])} event(s), {len(made['expected']['files'])} file(s) at the end")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
