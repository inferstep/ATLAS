"""What the reliability runner does when a session asks for a permission.

A stand-in proxy asks for one permission in the middle of a session, waits
for the answer up to its own limit, and then says in the stream what it was
told. The runner must answer by the run's policy, at once, and keep the
answer with the session.
"""
import http.server
import importlib.util
import io
import json
import sys
import threading
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
ASKED = {"tool_name": "delete_file", "tool_call_id": "call_3", "message": "Delete notes.txt"}


@pytest.fixture(scope="module")
def rel():
    spec = importlib.util.spec_from_file_location("atlas_reliability_prompts", REPO / "scripts" / "e2e-reliability.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


class StandInProxy:
    """Asks for one permission, waits up to `limit` seconds, then streams what it was told."""

    def __init__(self, limit: float):
        self.limit, self.answers, self.told = limit, [], threading.Event()
        proxy = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def do_POST(self):
                body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
                if self.path == "/v1/permission":
                    waiting = body.get("tool_call_id") == ASKED["tool_call_id"] and not proxy.told.is_set()
                    if waiting:
                        proxy.answers.append(body)
                        proxy.told.set()
                    self.answer(200 if waiting else 404, {"delivered": waiting})
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.end_headers()
                self.event({"type": "turn_start", "data": {}})
                self.event({"type": "permission_request", "data": ASKED})
                proxy.told.wait(proxy.limit)
                decision = proxy.answers[-1]["decision"] if proxy.answers else "no answer in time"
                self.event({"type": "permission_denied", "data": {"reason": decision}})
                self.event({"type": "done", "data": {"summary": "stopped"}})
                self.wfile.write(b"data: [DONE]\n\n")

            def event(self, event):
                self.wfile.write(b"data: " + json.dumps(event).encode() + b"\n\n")
                self.wfile.flush()

            def answer(self, status, body):
                data = json.dumps(body).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

        self.server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self.url = f"http://127.0.0.1:{self.server.server_address[1]}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()
        self.server.server_close()


@pytest.fixture
def proxy():
    made = []

    def make(limit):
        made.append(StandInProxy(limit))
        return made[-1]
    yield make
    for one in made:
        one.close()


def session(rel, proxy, tmp_path, policy):
    task = rel.Task(name="asks", prompt="Remove notes.txt.", files={"notes.txt": "x\n"}, check=lambda ws: (True, "ok"))
    started = time.monotonic()
    result = rel.run_session(task, 1, proxy.url, tmp_path / "ws", "", 30, prompt_policy=policy)
    return result, time.monotonic() - started


@pytest.mark.parametrize("policy", ["deny", "allow"])
def test_the_runner_answers_a_prompt_at_once_by_its_policy(rel, proxy, tmp_path, policy):
    stand_in = proxy(limit=20)
    result, took = session(rel, stand_in, tmp_path, policy)
    assert stand_in.answers == [{"session_id": "reliability-asks-1", "tool_call_id": "call_3", "decision": policy,
                                 "scope": "once"}]
    assert took < 10
    assert result.stream_ok
    assert [e["data"]["reason"] for e in result.of_type("permission_denied")] == [policy]


@pytest.mark.parametrize("policy", ["deny", "allow"])
def test_the_session_keeps_what_was_asked_and_what_was_answered(rel, proxy, tmp_path, policy):
    result, _ = session(rel, proxy(limit=20), tmp_path, policy)
    assert len(result.prompts) == 1
    kept = result.prompts[0]
    assert {k: kept[k] for k in ("tool", "tool_call_id", "policy", "answer", "delivered")} == {
        "tool": "delete_file", "tool_call_id": "call_3", "policy": policy, "answer": policy, "delivered": True}
    assert kept["_t"] == result.of_type("permission_request")[0]["_t"]


def test_with_the_wait_policy_the_runner_sends_nothing_and_the_proxy_limit_ends_the_wait(rel, proxy, tmp_path):
    stand_in = proxy(limit=0.5)
    result, took = session(rel, stand_in, tmp_path, "wait")
    assert stand_in.answers == []
    assert took >= 0.5
    assert [e["data"]["reason"] for e in result.of_type("permission_denied")] == ["no answer in time"]
    assert [(p["policy"], p["answer"], p["delivered"]) for p in result.prompts] == [("wait", "", None)]


def test_a_session_with_no_prompt_keeps_an_empty_list(rel, tmp_path):
    result = rel.Session(task="t", rep=1, events=[], workspace=tmp_path, wall_s=1.0, stream_ok=True)
    assert result.prompts == []
    assert rel.result_row(result, {}, {"prompt_policy": "deny"})["prompts"] == []


def test_the_result_row_holds_the_prompts_and_the_run_holds_the_policy(rel, tmp_path):
    asked = [{"tool": "delete_file", "tool_call_id": "call_3", "policy": "deny", "answer": "deny", "delivered": True}]
    result = rel.Session(task="t", rep=1, events=[], workspace=tmp_path, wall_s=1.0, stream_ok=True, prompts=asked)
    row = rel.result_row(result, {}, {"prompt_policy": "deny"})
    assert row["prompts"] == asked
    assert row["stack"]["prompt_policy"] == "deny"


def test_the_policy_is_an_option_of_the_runner_and_no_is_the_default(rel):
    assert rel.parse_args([]).prompts == "deny"
    assert rel.parse_args(["--prompts", "wait"]).prompts == "wait"
    with pytest.raises(SystemExit):
        rel.parse_args(["--prompts", "sometimes"])


def test_an_answer_for_a_call_that_no_longer_waits_is_kept_as_not_delivered(rel, proxy):
    stand_in = proxy(limit=1)
    event = {"type": "permission_request", "data": {**ASKED, "tool_call_id": "call_9"}}
    kept = rel.answer_prompt(stand_in.url, "reliability-asks-1", event, "deny")
    assert (kept["answer"], kept["delivered"]) == ("deny", False)
    assert "error" not in kept


def test_an_answer_that_cannot_be_sent_is_kept_with_the_reason(rel):
    kept = rel.answer_prompt("http://127.0.0.1:9", "reliability-asks-1", {"type": "permission_request", "data": ASKED},
                             "deny")
    assert kept["delivered"] is None
    assert kept["error"].startswith("the answer could not be sent")


def lines(*events, done=True):
    out = [b"data: " + (e if isinstance(e, bytes) else json.dumps(e).encode()) + b"\n" for e in events]
    return out + ([b"data: [DONE]\n"] if done else [])


def test_the_stream_reader_says_how_the_reading_ended(rel):
    seen = []
    assert rel.read_stream(lines({"type": "text"}), seen.append, time.time() + 30) == "done"
    assert rel.read_stream(lines({"type": "text"}, done=False), seen.append, time.time() + 30) == "end"
    assert rel.read_stream(lines({"type": "text"}), seen.append, time.time() - 1) == "cap"
    assert seen == [{"type": "text"}, {"type": "text"}]


def test_the_stream_reader_keeps_the_exact_lines_and_names_a_line_it_cannot_read(rel):
    seen, raw = [], io.StringIO()
    rel.read_stream([b": comment\n", *lines(b"{not json", {"type": "text"})], seen.append, time.time() + 30, raw)
    assert seen == [{"type": "__unparseable__", "raw": "{not json"}, {"type": "text"}]
    assert raw.getvalue() == ': comment\ndata: {not json\ndata: {"type": "text"}\ndata: [DONE]\n'
