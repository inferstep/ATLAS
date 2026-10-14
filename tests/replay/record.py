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

    python -m tests.replay.record <case> --model-at <port>

The cases of REAL_MODEL have no written replies. A real model server answers
them, at that port of this machine, and the recording keeps what it said and
which model it was. Such a session is one sample: the model answers in
another way the next time. With a real model the commands of a session are
the model's own choice, so record such a session in a container.

No recording holds a path of the machine that it was made on: the sandbox
gets a small fixed environment, and a recording whose text holds the home
folder of the user who records, or the folder of the model's file, is not
written.
"""
from __future__ import annotations

import argparse
import http.client
import importlib.util
import json
import os
import subprocess
import sys
import tempfile
import time
from pathlib import Path

from tests.replay import recording, stage

# The word that the tool sends as a token. It is no key of any service: the sandbox of a recording is started with
# this word, and a model server that asks for a real key cannot be recorded from.
PLACEHOLDER = "replay-placeholder-token"
BUGGY_APP = ('def greeting(name):\n    return "Hello, " + nmae\n\n\nif __name__ == "__main__":\n'
             '    print(greeting("world"))\n')

TASK = "the scenario of tests/e2e/test_acceptance.py (read, edit, check, done), with a run of the program as its check"
MESSAGE = "Fix the NameError in app.py, then run it to check that it prints the greeting."
READ = {"type": "tool_call", "name": "read_file", "args": {"path": "app.py"}}
EDIT = {"type": "tool_call", "name": "edit_file", "args": {
    "path": "app.py", "old_str": 'return "Hello, " + nmae', "new_str": 'return "Hello, " + name'}}
RUN = {"type": "tool_call", "name": "run_command", "args": {"command": "python3 app.py", "timeout": 30}}
DONE = {"type": "done", "summary": "Fixed the NameError in app.py. It now prints the greeting."}


def case(session: str, what: str, model: list, again: bool = False) -> dict:
    """One case on the e2e task. `what` says which replies of the model are the point of the case."""
    return {"source": {"task": TASK, "kind": "e2e", "model_replies": "written", "what": what},
            "files": {"app.py": BUGGY_APP},
            "request": {"message": MESSAGE, "working_dir": stage.WORKSPACE, "mode": "default", "session_id": session,
                        "task_contract": {"task_mode": "work"}},
            "model": model, "model_again": again}


CASES = {
    "normal_edit": case("replay-normal-edit", "a normal session: read, edit, run, done", [READ, EDIT, RUN, DONE]),
    "malformed_tool_call": case(
        "replay-malformed-tool-call", "two tool calls that are not well formed (no arguments; arguments that are "
        "text and not an object), then a normal session",
        ['{"type": "tool_call", "name": "read_file"}', '{"type": "tool_call", "name": "read_file", "args": "app.py"}',
         READ, EDIT, RUN, DONE]),
    "cut_off_reply": case(
        "replay-cut-off-reply", "a reply that stops in the middle of a tool call, then the whole call",
        [READ, '{"type": "tool_call", "name": "edit_file", "args": {"path": "app.py", "old_str": "return \\"Hello, \\" '
               '+ nmae", "new_str": "return \\"Hel', EDIT, RUN, DONE]),
    "looping_reply": case(
        "replay-looping-reply", "the same tool call again and again, until the proxy stops the session", [READ],
        again=True),
    "syntax_break_refused": case(
        "replay-syntax-break-refused", "an edit that would leave the file unparseable, then the right edit",
        [READ, {"type": "tool_call", "name": "edit_file", "args": {
            "path": "app.py", "old_str": 'return "Hello, " + nmae', "new_str": 'return "Hello, " + name)'}},
         EDIT, RUN, DONE]),
    "write_existing_redirected": case(
        "replay-write-existing-redirected", "a write of the whole existing file, which is refused and redirected, "
        "a structural edit that fails because V3 is not available, then an edit",
        [READ, {"type": "tool_call", "name": "write_file", "args": {
            "path": "app.py", "content": BUGGY_APP.replace("nmae", "name")}},
         {"type": "tool_call", "name": "structural_edit", "args": {
             "path": "app.py", "selector": "function:greeting",
             "content": 'def greeting(name):\n    return "Hello, " + name\n'}},
         EDIT, RUN, DONE]),
    "done_before_the_run": case(
        "replay-done-before-the-run", "a `done` before anything was run, which is refused, then the run and a `done` "
        "that is accepted", [READ, EDIT, DONE, RUN, DONE]),
}


# The sessions that a real model answers: the name of the recording, the set its task comes from, and the task.
# A task of the smoke set is one that the nightly run drives; its text and its files are read from the driver.
REAL_MODEL = {
    "real_model_edit": ("e2e", ""), "real_model_edit_again": ("e2e", ""),
    "real_model_add_function": ("smoke", "add_function"), "real_model_offbyone": ("smoke", "offbyone"),
    "real_model_ask_explain": ("smoke", "ask_explain"),
}
DRIVER = stage.REPO / "scripts" / "e2e-reliability.py"


def real_model_case(name: str) -> dict:
    """A case whose replies come from a real model server: the e2e task of the cases above, or a task of the smoke set."""
    kind, smoke_task = REAL_MODEL[name]
    task, files, message, contract = TASK, {"app.py": BUGGY_APP}, MESSAGE, {"task_mode": "work"}
    if kind == "smoke":
        spec = importlib.util.spec_from_file_location("the_driver_of_the_smoke_set", DRIVER)
        driver = importlib.util.module_from_spec(spec)
        sys.modules[spec.name] = driver
        spec.loader.exec_module(driver)
        chosen = driver.TASKS[smoke_task]
        task = f"the task `{smoke_task}` of the smoke set (scripts/e2e-reliability.py)"
        files, message, contract = chosen.files, chosen.prompt, driver.task_contract(chosen)
    return {"source": {"task": task, "kind": kind, "model_replies": "captured", "what": "a session that a real model answers"},
            "files": files,
            "request": {"message": message, "working_dir": stage.WORKSPACE, "mode": "default",
                        "session_id": "replay-" + name.replace("_", "-"), "task_contract": contract}}


def model_stream(reply) -> str:
    """One model reply as the stream the model server sends. A reply given as text is sent as it is written."""
    content = reply if isinstance(reply, str) else json.dumps(reply)
    lines = [json.dumps({"choices": [{"delta": {"content": content}}]}),
             json.dumps({"choices": [], "usage": {"total_tokens": 20, "prompt_tokens": 15, "completion_tokens": 5}}),
             "[DONE]"]
    return "".join(f"data: {line}\n\n" for line in lines)


def scripted_model(replies: list, again: bool = False):
    """The model of a case: one written reply per call. With `again` the last reply is given to every later call."""
    calls = []

    def answer(method, path, body, _give_on):
        calls.append(path)
        if len(calls) > len(replies) and not again:
            raise RuntimeError(f"the case has {len(replies)} model replies and the proxy asked for one more")
        return 200, "text/event-stream", model_stream(replies[min(len(calls), len(replies)) - 1])
    return answer


def lens(method, path, body, _give_on):
    if path == "/internal/lens/score-per-step":
        return 200, "application/json", json.dumps({
            "enabled": True, "gx_available": True, "n_tokens": 12, "latency_ms": 1,
            "aggregate": {"first_off_rails_idx": -1, "gx_score_min": 0.9, "gx_score_mean": 0.92, "cx_norm_max": 0.3,
                          "cx_norm_mean": 0.2},
            "thresholds": {"off_rails": 0.34, "low": 0.34, "severe": 0.28}})
    return 200, "application/json", json.dumps({"status": "ok"})


def v3_not_available(method, path, body, _give_on):
    return 503, "text/plain", "unavailable"


def model_server(port: int):
    """Send each request on to a real model server, and give its answer on to the proxy piece by piece, as it comes.
    So the proxy waits for the model as it does in a live session, and not until the whole reply is there."""
    def answer(method, path, body, give_on):
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=300)
        connection.request(method, path, body.encode("utf-8"), {"Content-Type": "application/json"})
        response = connection.getresponse()
        kind, pieces = response.headers.get("Content-Type", "application/json"), []
        while piece := response.read1(65536):
            give_on(response.status, kind, piece)
            pieces.append(piece)
        connection.close()
        return response.status, kind, b"".join(pieces).decode("utf-8", "replace")
    return answer


def pass_through(port: int, token: str):
    """Send each request on to a real service and give back its answer."""
    def answer(method, path, body, _give_on):
        connection = http.client.HTTPConnection("127.0.0.1", port, timeout=120)
        connection.request(method, path, body.encode("utf-8"), {"Content-Type": "application/json",
                                                               "Authorization": f"Bearer {token}"})
        response = connection.getresponse()
        text = response.read().decode("utf-8", "replace")
        connection.close()
        return response.status, response.headers.get("Content-Type", "application/json"), text
    return answer


def about_the_model(port: int) -> tuple:
    """Which model answered, for the `source` of a recording: the name of its file and the build of its server.
    And, apart from that, the folder of the file: it is a path of the machine that the model server runs on, so it
    is left out of the source, and no recording may hold it."""
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=30)
    connection.request("GET", "/props")
    response = connection.getresponse()
    text = response.read().decode("utf-8", "replace")
    connection.close()
    if response.status != 200:
        raise SystemExit(f"the model server at port {port} did not say which model it is: /props gave status {response.status}")
    said = json.loads(text)
    file = Path(str(said.get("model_path") or "not named"))
    return {"model": file.name, "model_server": str(said.get("build_info") or "not named")}, str(file.parent)


def of_a_machine(text: str, model_folder: str) -> list:
    """The words in a text that belong to a machine and to no recording: the home folder of the user who records,
    and the folder of the model's file on the model server's machine. The bare name of that user is not looked for:
    it can be an ordinary word (`runner` is one), and the proxy's own prompt holds ordinary words.

    A word of the model's reply is found only where the proxy put the reply together: in an event, or in the next
    request. In the kept stream the reply stands in small pieces, and a word that is split over two pieces is not
    found there."""
    folder = model_folder.rstrip("/") + "/" if model_folder.strip("/.") else ""
    return sorted(word for word in {str(Path.home()), folder} if word.strip("/") and word in text)


def start_sandbox(root: Path, workspace_root: Path):
    """The sandbox executor of this checkout, as the e2e acceptance job starts it."""
    port, token = stage.free_port(), root / "sandbox-token"
    token.write_text(PLACEHOLDER + "\n", encoding="utf-8")
    (root / "sandbox-scratch").mkdir()
    # The executor runs `python`; the interpreter that runs this recording provides it.
    path = os.pathsep.join([str(Path(sys.executable).parent), os.environ.get("PATH", "")])
    # A small fixed environment, as the proxy of a recording gets: a command of the session can print its
    # environment, and what it prints is kept in the recording.
    (root / "sandbox-home").mkdir()
    env = {"PATH": path, "HOME": str(root / "sandbox-home"), "WORKSPACE_BASE": str(root / "sandbox-scratch"), "MAX_EXECUTION_TIME": "60",
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


def record(name: str, model_at: int | None = None) -> dict:
    """Make the recording of a case. `model_at` is the port of this machine at which a real model server answers:
    a case of REAL_MODEL needs it, and a case with written replies takes none."""
    real = name in REAL_MODEL
    if real != (model_at is not None):
        raise SystemExit(f"the case {name} " + ("takes its replies from a real model server: give its port with --model-at"
                                                if real else "has written replies: it takes no --model-at"))
    case_ = real_model_case(name) if real else CASES[name]
    model = model_server(model_at) if real else scripted_model(case_["model"], case_["model_again"])
    which_model, model_folder = about_the_model(model_at) if real else ({}, "")
    commit = subprocess.run(["git", "rev-parse", "--short", "HEAD"], cwd=stage.REPO, capture_output=True, text=True,
                            check=True).stdout.strip()
    made = {"source": {**case_["source"], "commit": commit, **which_model}, "files": case_["files"], "request": case_["request"]}
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp).resolve()
        binary = stage.build_proxy(root)
        (root / "record").mkdir()
        sandbox_port, sandbox = start_sandbox(root, root / "record")
        try:
            result = recording.run(made, binary, root / "record", upstreams={
                "model": model, "sandbox": pass_through(sandbox_port, PLACEHOLDER), "v3": v3_not_available, "lens": lens})
        finally:
            sandbox.terminate()
            sandbox.communicate(timeout=10)
        made.update(exchanges=result["exchanges"], expected={"events": result["events"], "files": {}})
        replayed = recording.run(made, binary, root / "replay")
        made["expected"]["files"] = replayed["files"]
        problems = recording.differences(made, replayed)
    if problems:
        raise SystemExit(f"the recording of {name} does not replay cleanly, so it was not written:\n  " + "\n  ".join(problems))
    words = of_a_machine(json.dumps(made, ensure_ascii=False), model_folder)
    if words:
        raise SystemExit(f"the recording of {name} holds words of a machine, so it was not written: {', '.join(words)}")
    return made


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("case", choices=sorted(CASES) + sorted(REAL_MODEL) + ["all"], help="`all` is every case with written replies")
    parser.add_argument("--model-at", type=int, metavar="PORT",
                        help="the port of this machine (127.0.0.1) at which a real model server answers; for a case of a real model")
    args = parser.parse_args()
    for name in sorted(CASES) if args.case == "all" else [args.case]:
        path = recording.save(name, record(name, args.model_at))
        made = recording.load(name)
        print(f"wrote {path.relative_to(stage.REPO)}: {len(made['exchanges'])} exchange(s), "
              f"{len(made['expected']['events'])} event(s), ends {made['expected']['events'][-1]['data']}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
