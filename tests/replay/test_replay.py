"""The proxy, built from this checkout, does on each recorded session what the recording says.

The proxy runs as a binary. Its four services are stand-ins that play the
recording, and each request the proxy sends them is compared with the
recorded one. So a test here does not know how the proxy is built inside,
and a change that only moves code does not touch it. A change in what the
proxy sends, says or writes on a recorded session fails it, at the first
place the run differs.

The tests of the harness itself come after the replays: a recording that is
changed in one place must be reported at that place, or the net has a hole.
"""
import copy
import http.client
import http.server
import json
import re
import shutil
import subprocess
import threading
import time
import warnings

import pytest

from tests.replay import record, recording, stage


@pytest.fixture(scope="session")
def proxy_binary(tmp_path_factory):
    return stage.build_proxy(tmp_path_factory.mktemp("proxy-binary"))


def replay(made, proxy_binary, tmp_path):
    return recording.differences(made, recording.run(made, proxy_binary, tmp_path))


@pytest.mark.parametrize("name", recording.names())
def test_the_proxy_does_what_the_recording_says(name, proxy_binary, tmp_path):
    made = recording.load(name)
    result = recording.run(made, proxy_binary, tmp_path)
    problems = recording.differences(made, result)
    assert not problems, (
        f"the proxy no longer behaves as recorded in tests/replay/recordings/{name}.json:\n  " + "\n  ".join(problems)
        + "\nIf the change is not meant, the proxy has a regression: fix it. If it is meant, record the session "
          "again and say in the pull request which behaviour changed and why.")
    if result["text_note"]:
        warnings.warn(f"tests/replay/recordings/{name}.json was {result['text_note']}", stacklevel=1)


# --- the recordings ------------------------------------------------------------

@pytest.mark.parametrize("name", recording.names())
def test_a_recording_says_where_it_came_from(name):
    source = recording.load(name)["source"]
    assert source["kind"] in recording.KINDS, "recordings come from e2e, smoke and development tasks only"
    assert source["model_replies"] in recording.REPLIES
    assert source["task"].strip()
    assert source["what"].strip()
    assert re.fullmatch(r"[0-9a-f]{7,40}", source["commit"])


@pytest.mark.parametrize("name", recording.names())
def test_the_request_of_a_recording_declares_its_task_mode(name):
    assert recording.load(name)["request"]["task_contract"]["task_mode"] in ("work", "question")


@pytest.mark.parametrize("name", recording.names())
def test_a_recording_holds_no_path_of_a_machine_and_no_credential(name):
    text = (recording.RECORDINGS / f"{name}.json").read_text(encoding="utf-8")
    found = re.findall(r"/Users/|/home/|/private/|/var/folders/|/tmp/|Bearer |Authorization|service-token", text)
    assert not found, f"{name}.json holds {sorted(set(found))}: replace machine paths and drop credentials when recording"


def test_there_is_a_recording_of_a_normal_session():
    made = recording.load("normal_edit")
    assert made["expected"]["events"][-1]["data"]["status"] == "completed"
    assert {exchange["service"] for exchange in made["exchanges"]} == set(stage.SERVICES)


def test_the_three_bad_replies_are_recorded_with_what_the_proxy_does_about_each():
    def events(name, kind):
        return [event["data"] for event in recording.load(name)["expected"]["events"] if event["type"] == kind]

    refused = [result["error"] for result in events("malformed_tool_call", "tool_result") if not result["success"]]
    assert len(refused) == 2
    assert "no arguments provided" in refused[0]
    assert "invalid input" in refused[1]
    assert events("malformed_tool_call", "done")[-1]["status"] == "completed"
    assert [error["category"] for error in events("cut_off_reply", "error")] == ["truncated_tool"]
    assert events("cut_off_reply", "done")[-1]["status"] == "completed"
    assert events("looping_reply", "done")[-1]["reason"] == "repeat_detector"
    assert events("looping_reply", "done")[-1]["status"] == "stopped"


# --- the harness: a recording changed in one place is reported at that place --------

@pytest.fixture
def normal():
    return recording.load("normal_edit")


def changed_request(made, service, find, put):
    """The recording with one text replaced in the first recorded request to a service that holds it."""
    made = copy.deepcopy(made)
    for exchange in made["exchanges"]:
        if exchange["service"] == service and find in exchange["request"]:
            exchange["request"] = exchange["request"].replace(find, put, 1)
            return made
    raise AssertionError(f"no recorded request to the {service} holds {find!r}")


@pytest.mark.parametrize("service, find, put", [
    ("model", "You are ATLAS", "You are not ATLAS"),
    ("model", '"max_tokens"', '"max_token_count"'),
    ("sandbox", "python3 app.py", "python3 other.py"),
    ("v3", '"user_message"', '"user_text"'),
    ("lens", "text", "words"),
])
def test_a_request_that_differs_from_the_recording_is_named_with_its_service(normal, proxy_binary, tmp_path, service, find, put):
    problems = replay(changed_request(normal, service, find, put), proxy_binary, tmp_path)
    assert problems
    assert f"to the {service}" in problems[0]
    assert "differs from the recording" in problems[0]


def test_two_calls_in_the_other_order_are_named_unless_the_recording_holds_the_order_per_service(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    first, second = made["exchanges"][0], made["exchanges"][1]
    assert first["service"] != second["service"]
    made["exchanges"][0], made["exchanges"][1] = second, first
    problems = replay(made, proxy_binary, tmp_path)
    assert problems
    assert f"the proxy called the {first['service']}" in problems[0]
    assert "the order of its calls changed" in problems[0]
    made["order"] = "per service"
    assert replay(made, proxy_binary, tmp_path / "per-service") == []


def test_a_request_the_recording_does_not_have_is_named(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    last = made["exchanges"].pop()
    problems = replay(made, proxy_binary, tmp_path)
    assert problems
    assert f"a request to the {last['service']} that the recording does not have" in problems[0]


def test_a_recorded_request_the_proxy_does_not_send_is_named(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    extra = next(exchange for exchange in made["exchanges"] if exchange["service"] == "sandbox")
    made["exchanges"].append(copy.deepcopy(extra))
    problems = replay(made, proxy_binary, tmp_path)
    assert "the proxy did not send 1 recorded request(s) to the sandbox" in problems


def test_an_event_that_differs_is_named(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    made["expected"]["events"][-1]["data"]["reason"] = "another_reason"
    problems = replay(made, proxy_binary, tmp_path)
    assert len(problems) == 1
    assert problems[0].startswith(f"event {len(made['expected']['events'])} (done) differs")


@pytest.fixture(scope="session")
def proxy_with_other_carried_text(tmp_path_factory):
    """The proxy of this checkout with one word more in its system prompt: the text that every request carries."""
    source = tmp_path_factory.mktemp("proxy-source") / "proxy"
    shutil.copytree(stage.REPO / "proxy", source, ignore=shutil.ignore_patterns("*_test.go", "testdata"))
    agent = source / "agent.go"
    text = agent.read_text(encoding="utf-8")
    assert text.count("You are ATLAS, a coding assistant") == 1
    agent.write_text(text.replace("You are ATLAS, a coding assistant", "You are ATLAS, a careful coding assistant"), encoding="utf-8")
    subprocess.run(["go", "build", "-o", str(source / "atlas-proxy"), "."], cwd=source, check=True)
    return source / "atlas-proxy"


@pytest.mark.parametrize("replies, find, put, problem", [
    ("captured", None, None, None),
    ("written", None, None, "request 1 to the model (POST /v1/chat/completions) differs from the recording"),
    ("captured", "Fix the NameError in app.py", "Fix the TypeError in app.py", "TypeError"),
], ids=["a real model's replies", "written replies", "a real model's replies and another difference"])
def test_a_change_of_the_carried_text_holds_only_a_real_models_recording_without_those_parts(
        normal, proxy_with_other_carried_text, tmp_path, replies, find, put, problem):
    # A real model's replies cannot be written again from new text, so its recording is compared without the system
    # prompt, the grammar and the schema of the reply once they changed, and the run says so. Written ones stay strict.
    made = changed_request(normal, "model", find, put) if find else copy.deepcopy(normal)
    made["source"]["model_replies"] = replies
    result = recording.run(made, proxy_with_other_carried_text, tmp_path)
    problems = recording.differences(made, result)
    if problem is None:
        assert problems == []
    else:
        assert problems and problem in problems[0], problems
    if replies == "captured":
        made_with, today = re.fullmatch(r"made with text ([0-9a-f]{12}), today's is ([0-9a-f]{12}); the carried parts are "
                                        r"not compared", result["text_note"]).groups()
        first = next(exchange for exchange in made["exchanges"] if stage.is_a_model_request(exchange))
        assert made_with == stage.text_mark(first) != today
    else:
        assert result["text_note"] == ""


def test_only_the_three_carried_parts_are_left_out_of_a_request_to_the_model(normal):
    first = next(exchange for exchange in normal["exchanges"] if stage.is_a_model_request(exchange))
    request = json.loads(first["request"])
    kept = {key: value for key, value in request.items() if key not in ("grammar", "response_format")}
    kept["messages"] = [{**request["messages"][0], "content": "<the system prompt>"}, *request["messages"][1:]]
    assert json.loads(stage.without_the_carried_parts(first["request"])) == kept


def test_a_file_that_differs_at_the_end_is_named(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    made["expected"]["files"]["app.py"] += "# one more line\n"
    assert replay(made, proxy_binary, tmp_path) == ["the files at the end differ from the recording: app.py"]


def test_the_expected_side_can_be_written_again_and_the_answers_stay(normal, proxy_binary, tmp_path):
    from tests.replay import rewrite
    stale = changed_request(normal, "model", "You are ATLAS", "You were ATLAS")
    stale["expected"]["events"][-1]["data"]["reason"] = "an_old_reason"
    stale["expected"]["files"]["app.py"] = "old content\n"
    new, problem = rewrite.rewritten(stale, proxy_binary, tmp_path)
    assert problem is None
    assert new == normal
    assert [exchange["response"] for exchange in new["exchanges"]] == [e["response"] for e in stale["exchanges"]]


def test_a_change_in_the_order_of_calls_cannot_be_written_again(normal, proxy_binary, tmp_path):
    from tests.replay import rewrite
    made = copy.deepcopy(normal)
    made["exchanges"][0], made["exchanges"][1] = made["exchanges"][1], made["exchanges"][0]
    new, problem = rewrite.rewritten(made, proxy_binary, tmp_path)
    assert problem and "the order of its calls changed" in problem
    assert new["expected"] == made["expected"]


def test_what_varies_between_runs_is_left_out_and_nothing_else(normal):
    st = stage.Stage(workspace="/a/workspace")
    varies = {name: 1 for name in recording.VARIES_IN_EVENTS}
    event = {"type": "tool_result", "data": {**varies, "latency_ms": 1, "tool": "read_file", "path": "/a/workspace/x"}}
    assert recording.plain_events([event], st) == [{"type": "tool_result", "data": {
        "latency_ms": 1, "tool": "read_file", "path": "<workspace>/x"}}]
    kept = json.dumps(normal["expected"]["events"])
    assert '"elapsed"' not in kept
    assert '"turn"' in kept


class _ModelServer(http.server.BaseHTTPRequestHandler):
    """A stand-in for a real model server. It says which model it is. It gives each reply of its list as a stream
    in small pieces, and it takes about a second for a reply, as a real one takes time: half a second before the
    first piece, and a little between the pieces."""

    replies, tokens = [], []

    def do_GET(self):
        self.tokens.append(self.headers.get("Authorization"))
        body = json.dumps({"build_info": "b1-stand-in", "model_path": "/a/folder/of/the/server/a-model.gguf"}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        self.tokens.append(self.headers.get("Authorization"))
        self.rfile.read(int(self.headers["Content-Length"]))
        reply = json.dumps(self.replies.pop(0))
        lines = [json.dumps({"choices": [{"delta": {"content": reply[at:at + 9]}}]}) for at in range(0, len(reply), 9)]
        lines += [json.dumps({"choices": [], "usage": {"total_tokens": 20, "prompt_tokens": 15, "completion_tokens": 5}}), "[DONE]"]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        time.sleep(0.5)
        for line in lines:
            self.wfile.write(f"data: {line}\n\n".encode("utf-8"))
            self.wfile.flush()
            time.sleep(0.5 / len(lines))

    def log_message(self, *args):
        pass


@pytest.fixture
def model_server():
    """The stand-in for a model server, started; gives its port."""
    _ModelServer.tokens.clear()
    server = http.server.HTTPServer(("127.0.0.1", 0), _ModelServer)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield server.server_port
    server.shutdown()


def test_a_session_that_a_real_model_answers_is_recorded_with_what_it_said_and_which_model_it_was(model_server):
    _ModelServer.replies = [record.READ, record.EDIT, record.RUN, record.DONE]
    made = record.record("real_model_edit", model_at=model_server)
    # The folder of the model's file is a path of the server's machine, and it is not in the recording.
    assert made["source"] == {"task": record.TASK, "kind": "e2e", "model_replies": "captured", "what": "a session that a real model answers",
                              "commit": made["source"]["commit"], "model": "a-model.gguf", "model_server": "b1-stand-in"}
    # What the model server said is kept as it came, each reply in its small pieces.
    said = [exchange["response"]["body"] for exchange in made["exchanges"] if exchange["service"] == "model"]
    assert len(said) == 4 and all(body.count("data: ") > 5 for body in said)
    assert made["expected"]["events"][-1]["type"] == "done"
    # The model server was asked five things, which model it is and four replies, and no token went with any.
    assert _ModelServer.tokens == [None] * 5


def test_a_piece_of_the_models_reply_reaches_the_proxy_when_it_comes_and_not_when_the_reply_is_whole(model_server):
    _ModelServer.replies = [record.READ]
    st = stage.Stage(upstreams={"model": record.model_server(model_server)})
    ports = st.start()
    try:
        # What the proxy does: it asks the stage's stand-in for the model, and reads the answer as it comes.
        connection = http.client.HTTPConnection("127.0.0.1", ports["model"], timeout=30)
        connection.request("POST", "/v1/chat/completions", "{}", {"Content-Type": "application/json"})
        response, came, pieces = connection.getresponse(), [], []
        while piece := response.read1(65536):
            came.append(time.monotonic())
            pieces.append(piece)
    finally:
        st.stop()
    # The stand-in model server takes half a second from its first piece to its last. So do the pieces here.
    assert len(came) > 5 and came[-1] - came[0] > 0.3
    # What came is the reply, once, and it is what the stage keeps.
    kept = st.exchanges[0]["response"]["body"]
    assert b"".join(pieces).decode("utf-8") == kept and kept.endswith("data: [DONE]\n\n") and kept.count("[DONE]") == 1


def test_a_recording_that_holds_a_folder_of_the_model_servers_machine_is_not_written(model_server):
    done = {"type": "done", "summary": "Fixed the NameError. The model is in /a/folder/of/the/server/ on my machine."}
    _ModelServer.replies = [record.READ, record.EDIT, record.RUN, done]
    with pytest.raises(SystemExit, match="holds words of a machine, so it was not written: /a/folder/of/the/server/"):
        record.record("real_model_edit", model_at=model_server)


def test_the_words_of_a_machine_are_the_home_folder_and_the_folder_of_the_models_file_and_not_the_users_bare_name(monkeypatch):
    monkeypatch.setenv("HOME", "/home/runner")
    monkeypatch.setenv("LOGNAME", "runner")
    # The name of this user is an ordinary word, and the proxy's own prompt has it.
    assert record.of_a_machine("watch test runner output", "/models/of/it") == []
    assert record.of_a_machine("a path /home/runner/x and a file in /models/of/it/file.gguf", "/models/of/it") == ["/home/runner", "/models/of/it/"]
    assert record.of_a_machine("a text with /models/of/its own and nothing else", "/models/of/it") == []


def test_the_sandbox_of_a_recording_gets_a_small_fixed_environment_and_nothing_of_the_person_who_records(tmp_path, monkeypatch):
    monkeypatch.setenv("A_VALUE_OF_THE_PERSON_WHO_RECORDS", "it must not come into a recording")
    (tmp_path / "workspace").mkdir()
    port, sandbox = record.start_sandbox(tmp_path, tmp_path / "workspace")
    try:
        said = record.pass_through(port, record.PLACEHOLDER)("POST", "/shell", json.dumps({"command": "env", "cwd": str(tmp_path / "workspace"), "timeout": 30}), None)
    finally:
        sandbox.terminate()
        sandbox.communicate(timeout=10)
    environment = dict(line.split("=", 1) for line in json.loads(said[2])["stdout"].splitlines() if "=" in line)
    assert "A_VALUE_OF_THE_PERSON_WHO_RECORDS" not in environment and "PATH" in environment, environment
    # Its home folder is a folder of the recording, and not the home of the person who records.
    assert environment["HOME"] == str(tmp_path / "sandbox-home")


def test_a_case_of_a_real_model_and_a_port_go_together():
    with pytest.raises(SystemExit, match="takes its replies from a real model server: give its port with --model-at"):
        record.record("real_model_edit")
    with pytest.raises(SystemExit, match="has written replies: it takes no --model-at"):
        record.record("normal_edit", model_at=1)


def test_a_case_of_the_smoke_set_takes_its_text_and_its_files_from_the_driver_of_the_nightly_run():
    case = record.real_model_case("real_model_ask_explain")
    assert case["source"]["kind"] == "smoke" and "find_duplicates" in case["request"]["message"]
    assert list(case["files"]) == ["orders.py"] and case["request"]["task_contract"] == {"task_mode": "question"}
