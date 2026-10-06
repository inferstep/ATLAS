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
import json
import re

import pytest

from tests.replay import recording, stage


@pytest.fixture(scope="session")
def proxy_binary(tmp_path_factory):
    return stage.build_proxy(tmp_path_factory.mktemp("proxy-binary"))


def replay(made, proxy_binary, tmp_path):
    return recording.differences(made, recording.run(made, proxy_binary, tmp_path))


@pytest.mark.parametrize("name", recording.names())
def test_the_proxy_does_what_the_recording_says(name, proxy_binary, tmp_path):
    problems = replay(recording.load(name), proxy_binary, tmp_path)
    assert not problems, (
        f"the proxy no longer behaves as recorded in tests/replay/recordings/{name}.json:\n  " + "\n  ".join(problems)
        + "\nIf the change is not meant, the proxy has a regression: fix it. If it is meant, record the session "
          "again and say in the pull request which behaviour changed and why.")


# --- the recordings ------------------------------------------------------------

@pytest.mark.parametrize("name", recording.names())
def test_a_recording_says_where_it_came_from(name):
    source = recording.load(name)["source"]
    assert source["kind"] in recording.KINDS, "recordings come from e2e, smoke and development tasks only"
    assert source["model_replies"] in recording.REPLIES
    assert source["task"].strip()
    assert re.fullmatch(r"[0-9a-f]{7,40}", source["commit"])


@pytest.mark.parametrize("name", recording.names())
def test_a_recording_holds_no_path_of_a_machine_and_no_credential(name):
    text = (recording.RECORDINGS / f"{name}.json").read_text(encoding="utf-8")
    found = re.findall(r"/Users/|/home/|/private/|/var/folders/|/tmp/|Bearer |Authorization|service-token", text)
    assert not found, f"{name}.json holds {sorted(set(found))}: replace machine paths and drop credentials when recording"


def test_there_is_a_recording_of_a_normal_session():
    made = recording.load("normal_edit")
    assert made["expected"]["events"][-1]["data"]["status"] == "completed"
    assert {exchange["service"] for exchange in made["exchanges"]} == set(stage.SERVICES)


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


def test_a_request_the_recording_does_not_have_is_named(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    last_model = max(i for i, exchange in enumerate(made["exchanges"]) if exchange["service"] == "model")
    del made["exchanges"][last_model]
    problems = replay(made, proxy_binary, tmp_path)
    assert problems
    assert "a request to the model that the recording does not have" in problems[0]


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


def test_a_file_that_differs_at_the_end_is_named(normal, proxy_binary, tmp_path):
    made = copy.deepcopy(normal)
    made["expected"]["files"]["app.py"] += "# one more line\n"
    assert replay(made, proxy_binary, tmp_path) == ["the files at the end differ from the recording: app.py"]


def test_what_varies_between_runs_is_left_out_and_nothing_else(normal):
    st = stage.Stage(workspace="/a/workspace")
    varies = {name: 1 for name in recording.VARIES_IN_EVENTS}
    event = {"type": "tool_result", "data": {**varies, "latency_ms": 1, "tool": "read_file", "path": "/a/workspace/x"}}
    assert recording.plain_events([event], st) == [{"type": "tool_result", "data": {
        "latency_ms": 1, "tool": "read_file", "path": "<workspace>/x"}}]
    kept = json.dumps(normal["expected"]["events"])
    assert '"elapsed"' not in kept
    assert '"turn"' in kept
