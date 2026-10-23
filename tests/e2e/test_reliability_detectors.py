"""Unit tests for scripts/e2e-reliability.py: its harness-defect detectors, and what a run sends and records.

The detectors decide whether a live session's failure was ATLAS's fault or the
model's, so a detector that silently stops firing turns the reliability number
into a rubber stamp. Each detector's table holds event streams reproducing
defects that were actually observed, beside the clean streams, so a detector
cannot pass by flagging everything.

No live stack: streams are literals and the workspace is a tmp_path.
"""
import ast
import http.server
import importlib.util
import inspect
import io
import json
import os
import re
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]


@pytest.fixture(scope="module")
def rel():
    spec = importlib.util.spec_from_file_location(
        "atlas_reliability", REPO / "scripts" / "e2e-reliability.py")
    mod = importlib.util.module_from_spec(spec)
    sys.modules["atlas_reliability"] = mod
    spec.loader.exec_module(mod)
    return mod


def _session(rel, events, workspace, stream_ok=True):
    return rel.Session(task="t", rep=1, events=events, workspace=workspace,
                       wall_s=1.0, stream_ok=stream_ok)


def _call(name, **args):
    return {"type": "tool_call", "data": {"name": name, "args": args}}


def _ok(tool="read_file"):
    # Real tool_result events carry the tool name (data keys: data, elapsed,
    # error, success, tool). The detectors read it from the RESULT rather than
    # pairing positionally with the calls, because one unanswered call — a
    # client timeout mid-stream — shifted every pair after it.
    return {"type": "tool_result", "data": {"tool": tool, "success": True, "error": ""}}


def _fail(error, tool="read_file"):
    return {"type": "tool_result", "data": {"tool": tool, "success": False, "error": error}}


# The error event this runner appends when it stops reading at --timeout.
RUNNER_CAP = {"type": "error", "data": {"error": "harness cap: session exceeded 900s"}}


# --- H1 protocol ----------------------------------------------------------

@pytest.mark.parametrize("events, stream_ok, defects", [
    pytest.param([_call("read_file", path="a.py"), _ok(), {"type": "done", "data": {"summary": "x"}}], True, [],
                 id="clean"),
    pytest.param([_call("read_file", path="a.py"), _ok(), _call("edit_file", path="a.py"),
                  {"type": "done", "data": {"summary": "x"}}], True,
                 ["H1 protocol: 2 tool_call vs 1 tool_result (orphaned call)"],
                 id="orphaned-call"),
    pytest.param([{"type": "brand_new_event", "data": {}}, {"type": "done", "data": {"summary": "x"}}], True,
                 ["H1 protocol: event type(s) the TUI cannot render: ['brand_new_event']"],
                 id="an-event-the-tui-cannot-render"),
    # The runner's cap: the proxy never got to send `done` and the socket closed mid-stream. That is our deadline,
    # reported as a timeout, not two protocol defects of the proxy.
    pytest.param([_call("read_file", path="a.py"), _ok(), RUNNER_CAP], False,
                 ["H1 timeout: runner cap cut the session at 1s before it finished"],
                 id="the-runner-cap"),
    # A capped session tolerates the one call that was in flight when the runner stopped reading, and no more.
    pytest.param([_call("read_file", path="a.py"), _ok(), _call("edit_file", path="a.py"), RUNNER_CAP], False,
                 ["H1 timeout: runner cap cut the session at 1s before it finished"],
                 id="the-runner-cap-with-one-call-in-flight"),
    pytest.param([_call("read_file", path="a.py"), _call("edit_file", path="a.py"), RUNNER_CAP], False,
                 ["H1 protocol: 2 tool_call vs 0 tool_result (orphaned call)",
                  "H1 timeout: runner cap cut the session at 1s before it finished"],
                 id="the-runner-cap-with-two-calls-unanswered"),
])
def test_h1_protocol(rel, tmp_path, events, stream_ok, defects):
    s = _session(rel, events, tmp_path, stream_ok=stream_ok)
    assert rel.h1_protocol(s, {"tool_call", "tool_result", "done", "error"}) == defects


# --- H2 false rejection (the D9 class) ------------------------------------

@pytest.mark.parametrize("on_disk, error, defects", [
    # V3 authored the break, the gate blocked it, and the message named the file: the model then hunts a defect
    # that is not on disk. Decided by re-checking the file, which is exactly what a human does to catch this.
    pytest.param("def index():\n    return 'ok'\n",
                 "/workspace/app.py has a JavaScript syntax error in the <script> block — it was NOT written.\n"
                 "line 121: unexpected `)`",
                 ["H2 false rejection: /workspace/app.py was blamed for a syntax error it does not have"],
                 id="the-file-blamed-is-fine"),
    pytest.param("def index(:\n", "/workspace/app.py has a Python syntax error — it was NOT written.", [],
                 id="the-file-blamed-really-is-broken"),
    # "Your content for X has a syntax error" is the CORRECT message: it blames the submission, and the file on
    # disk is clean precisely because the write was refused.
    pytest.param("x = 1\n",
                 "Your content for app.py has a syntax error (SyntaxError: unmatched ')') — it was NOT written. "
                 "The content is NOT truncated; it is complete but INVALID.", [],
                 id="the-message-blames-the-models-content"),
])
def test_h2_false_rejection(rel, tmp_path, on_disk, error, defects):
    (tmp_path / "app.py").write_text(on_disk)
    s = _session(rel, [_call("edit_file", path="app.py"), _fail(error),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert rel.h2_false_rejection(s) == defects


# --- H3 dead-end steering (the D10 class) ---------------------------------

@pytest.mark.parametrize("advice, selector, next_error, defects", [
    # Advice offered `<body>`; the model reached for `<script>`. A different tag, the same unsupported shape:
    # matching the literal string would miss the case this exists to catch.
    pytest.param("string to replace not found in file. To replace a whole element use structural_edit with a "
                 "selector (e.g. `function:NAME`, `class:NAME`, `<body>`) and the new content.",
                 "<script>", "unknown selector '<script>' for python. Supported: function:NAME, class:NAME",
                 ["H3 dead-end steering: a rejection recommended a <tag> selector, and the next call was refused "
                  "for using '<script>'"],
                 id="the-next-call-is-refused-for-taking-the-advice"),
    pytest.param("string to replace not found. Use structural_edit with a selector (`function:NAME` or `class:NAME`).",
                 "function:index", "structural_edit: your replacement is IDENTICAL to the code already in the file",
                 [],
                 id="the-next-call-failed-for-another-reason"),
])
def test_h3_dead_end_steering(rel, tmp_path, advice, selector, next_error, defects):
    s = _session(rel, [_call("edit_file", path="app.py"), _fail(advice),
                       _call("structural_edit", path="app.py", selector=selector), _fail(next_error),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert rel.h3_dead_end_steering(s) == defects


# --- H4 gate escape (the D11 class) ---------------------------------------

@pytest.mark.parametrize("events, conversational, escaped", [
    pytest.param([_call("read_file", path="app.py"), _ok(),
                  {"type": "text", "data": {"content": "The pause logic is already present in the template."}},
                  {"type": "done", "data": {"summary": ""}}], None, True,
                 id="no-write-on-an-action-prompt"),
    # An honest "I stopped" is the machinery working, not an escape.
    pytest.param([_call("read_file", path="app.py"), _ok(),
                  {"type": "done", "data": {"summary": "Stopped after 3 tool failures on the same target "
                                                       "with no successful changes."}}], None, False,
                 id="the-breaker-ended-honestly"),
    pytest.param([_call("edit_file", path="app.py"), _ok("edit_file"),
                  {"type": "done", "data": {"summary": "Added the toggle."}}], None, False,
                 id="a-write-landed"),
    # A question SHOULD exit without writing: the inverse of the H9 tier check.
    pytest.param([_call("read_file", path="orders.py"), _ok(),
                  {"type": "text", "data": {"content": "It is quadratic."}},
                  {"type": "done", "data": {"summary": ""}}], True, False,
                 id="a-question"),
    pytest.param([_call("read_file", path="a.py"), _ok(), {"type": "done", "data": {"summary": ""}}], False, True,
                 id="a-work-task"),
])
def test_h4_gate_escape(rel, tmp_path, events, conversational, escaped):
    # conversational=None asks the detector without a task.
    task = None if conversational is None else rel.Task(
        name="t", prompt="", files={}, check=lambda *a: (True, ""), conversational=conversational)
    found = rel.h4_gate_escape(_session(rel, events, tmp_path), task)
    assert found == (["H4 gate escape: exited with no successful write on an action-intent prompt, "
                      "without saying it had stopped"] if escaped else [])


# --- H5 corrupt write -----------------------------------------------------

@pytest.mark.parametrize("files, must_exist, defect", [
    pytest.param({"fine.py": "x = 1\n"}, ("fine.py",), None, id="a-clean-workspace"),
    pytest.param({"broken.py": "def f(:\n"}, (), "H5 corrupt write: broken.py left unparseable",
                 id="an-unparseable-file-left-on-disk"),
    pytest.param({}, ("gone.py",), "H5 corrupt write: gone.py was deleted", id="a-required-file-was-deleted"),
])
def test_h5_corrupt_write(rel, tmp_path, files, must_exist, defect):
    for name, text in files.items():
        (tmp_path / name).write_text(text)
    task = rel.Task(name="t", prompt="", files={}, check=lambda p: (True, ""), must_exist=must_exist)
    found = rel.h5_corrupt_write(_session(rel, [], tmp_path), task)
    if defect is None:
        assert found == []
    else:
        assert len(found) == 1 and found[0].startswith(defect), found


# --- H6 service fault ------------------------------------------------------

@pytest.mark.parametrize("events, defects", [
    # The cap event is this runner's own. h1_protocol already reports it as the timeout it is; counting it here too
    # charged one deadline as two separate proxy defects.
    pytest.param([_call("read_file", path="a.py"), _ok(), RUNNER_CAP], [], id="the-runner-cap"),
    pytest.param([{"type": "error", "data": {"error": "v3 service: connection refused"}}],
                 ["H6 service fault: error event 'v3 service: connection refused'"],
                 id="a-real-service-fault"),
    # flask_pause rep2, 2026-08-03: the model's tool call ran out of tokens mid-JSON, the proxy told the model, and
    # the session went on to pass the task. Recovered model behaviour is the proxy working. (The event carries no
    # category: a categorised one is a model-output guard, never counted at all.)
    pytest.param([_call("edit_file", path="app.py"),
                  {"type": "error", "data": {"error": "failed to parse model response"}},
                  _call("replace_lines", path="app.py"), _ok("replace_lines"),
                  {"type": "done", "data": {"summary": "added the toggle"}}], [],
                 id="a-parse-failure-the-session-recovered-from"),
    pytest.param([_call("edit_file", path="app.py"),
                  {"type": "error", "data": {"error": "failed to parse model response"}}],
                 ["H6 service fault: error event 'failed to parse model response'"],
                 id="a-parse-failure-the-session-died-on"),
    # Smoke run 2026-09-28 (multifile_cli rep 2): the session's own work deadline cut an LLM stream, and the
    # terminal status already says timed_out.
    pytest.param([_call("read_file", path="x.py"), _ok(),
                  {"type": "error", "data": {"error": "read LLM stream: context deadline exceeded"}},
                  {"type": "done", "data": {"status": "timed_out", "reason": "work_deadline"}}], [],
                 id="a-cut-by-the-work-deadline"),
    pytest.param([{"type": "error", "data": {"error": "read LLM stream: context deadline exceeded"}}],
                 ["H6 service fault: error event 'read LLM stream: context deadline exceeded'"],
                 id="the-same-cut-in-a-session-that-did-not-end-on-its-deadline"),
])
def test_h6_service_fault(rel, tmp_path, events, defects):
    assert rel.h6_service_fault(_session(rel, events, tmp_path)) == defects


def test_h6_does_not_count_a_model_output_guard(rel, tmp_path):
    """Smoke run 2026-09-27 (smallrung_toml): the only error event was the
    swallowed_content guard, which caught a tool call cut by an unescaped
    quote and told the model. The run still showed "1 harness defect". A
    guard that worked is not a service fault, even when the session later
    fails; it is counted for the summary instead."""
    guard = {"type": "error", "data": {
        "category": "swallowed_content",
        "error": "tool call content was truncated by an unescaped quote"}}
    s = _session(rel, [_call("read_file", path="x.py"), _ok(), guard,
                       {"type": "done", "data": {"status": "incomplete",
                                                 "reason": "text_instead_of_work"}}],
                 tmp_path)
    assert rel.h6_service_fault(s) == []
    assert rel.model_output_guards(s) == ["swallowed_content"]


# --- H7 silent background leak -------------------------------------------

def test_h7_is_silent_when_the_session_announced_the_jobs(rel, tmp_path, monkeypatch):
    """Persistence is deliberate; the defect is persistence nobody was told about.

    An agent loop is one user message, so killing jobs at its end would break
    "start the dev server" then "now curl it". H7 must therefore fire on
    silence, not on the jobs existing.
    """
    # The sandbox counts two jobs.
    monkeypatch.setattr(rel.subprocess, "run", lambda argv, **kw: subprocess.CompletedProcess(argv, 0, stdout="2"))
    announced = _session(rel, [{"type": "done", "data": {"summary":
        "Done.\n\nStill running in the sandbox:\n  abc — python app.py\n"
        "These keep their ports until stopped. Use stop_background to end them."}}],
        tmp_path)
    assert rel.h7_background_leak("atlas-sandbox-1", announced) == []

    silent = _session(rel, [{"type": "done", "data": {"summary": "Added the toggle."}}], tmp_path)
    assert rel.h7_background_leak("atlas-sandbox-1", silent) == [
        "H7 silent background leak: 2 job(s) still holding ports and the session never said so"]


# --- H8 anchored on ATLAS-injected text ----------------------------------

@pytest.mark.parametrize("old_str, defects", [
    # read_file appends a footer that is not on disk. A measured session anchored edit_file on it and spent all three
    # of its failures on an edit that could never match; scoring that against the model would make the harness
    # understate the defects it exists to find.
    pytest.param("\n\n\n## Call graph (within this file)\n- mean calls: ValueError, sum, len",
                 ["H8 injected-text anchor: edit_file old_str copied ATLAS's own '## Call graph (within this file)', "
                  "which is not on disk"],
                 id="old-str-copied-from-the-call-graph-footer"),
    pytest.param("def mean(values):", [], id="a-normal-old-str"),
])
def test_h8_anchored_on_injected_text(rel, tmp_path, old_str, defects):
    s = _session(rel, [_call("edit_file", path="stats.py", old_str=old_str),
                       _fail("string to replace not found in file."),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert rel.h8_anchored_on_injected_text(s) == defects


# --- H9 tier appropriateness ---------------------------------------------

@pytest.mark.parametrize("events, conversational, defects", [
    # A wrong answer is a model limit. Spending a multi-minute V3 pipeline on "what does this function do" is a
    # product defect that costs real minutes.
    pytest.param([{"type": "v3_probe", "data": {"stage": "probe"}},
                  {"type": "text", "data": {"content": "It counts duplicates."}},
                  {"type": "done", "data": {"summary": ""}}], True,
                 ["H9 tier: the V3 pipeline ran on a question (['v3_probe'])"],
                 id="v3-ran-on-a-question"),
    pytest.param([_call("edit_file", path="orders.py"), _ok("edit_file"),
                  {"type": "done", "data": {"summary": "done"}}], True,
                 ["H9 tier: a question caused file writes (['edit_file'])"],
                 id="a-question-edited-files"),
    pytest.param([_call("read_file", path="orders.py"), _ok(),
                  {"type": "text", "data": {"content": "It is quadratic."}},
                  {"type": "done", "data": {"summary": ""}}], True, [],
                 id="a-clean-answer-to-a-question"),
    # V3 and writes are exactly what a coding task should produce.
    pytest.param([{"type": "v3_probe", "data": {}}, _call("edit_file", path="a.py"), _ok("edit_file"),
                  {"type": "done", "data": {"summary": "fixed"}}], False, [],
                 id="a-work-task"),
])
def test_h9_tier_misapplied(rel, tmp_path, events, conversational, defects):
    task = rel.Task(name="t", prompt="", files={}, check=lambda *a: (True, ""), conversational=conversational)
    assert rel.h9_tier_misapplied(_session(rel, events, tmp_path), task) == defects


# --- TUI coverage ---------------------------------------------------------

def test_tui_handled_types_is_populated_and_has_the_core_events(rel):
    """Located by content marker, so a file move must not empty the set."""
    handled = rel.tui_handled_types()
    assert len(handled) > 30, f"only found {len(handled)} — dispatcher not located?"
    for core in ("tool_call", "tool_result", "done", "text", "error"):
        assert core in handled, f"TUI dispatcher has no case for {core!r}"


# --- bug-find check must not accept an invented mechanism ----------------

@pytest.mark.parametrize("answer, passes", [
    # The real cycle-6 answer, which an earlier version of the check passed. It named planning.py and the symptom
    # correctly, then attributed the cause to "how min() is used with a custom key": there is no min() there, and
    # the function it named was the scorer, not the selection loop.
    pytest.param("The issue is in `planning.py` within the `_score_plan` function. When two plans have the same "
                 "score, the code selects the one with the maximum number of steps because of how the `min()` "
                 "function is being used with a custom key.", False,
                 id="the-right-file-with-an-invented-mechanism"),
    pytest.param("planning.py: the selection loop breaks ties with n_steps > best_steps, so a tie keeps the longer "
                 "plan. It should be <.", True,
                 id="the-actual-comparison"),
])
def test_the_bugfind_check_passes_only_the_actual_mechanism(rel, tmp_path, answer, passes):
    s = _session(rel, [{"type": "text", "data": {"content": answer}},
                       {"type": "done", "data": {"summary": ""}}], tmp_path)
    passed, _ = rel.TASKS["bugfind_tiebreak"].check(tmp_path, s)
    assert passed is passes


# --- V3 is always on -------------------------------------------------------
#
# The runners measure the shipped system. No request field turns V3 off: the
# proxy refuses bypass_v3, v3_mode and feasibility_mode when they ask for a
# different system, so a V3-free comparison takes a research build, never a
# flag on the product.


def test_the_runner_sends_no_removed_switch(rel, monkeypatch, tmp_path):
    sent = []

    def fake_urlopen(req, timeout=None):
        sent.append(json.loads(req.data.decode()))
        return io.BytesIO(b'data: {"type":"done"}\n\n')

    monkeypatch.setattr(rel.urllib.request, "urlopen", fake_urlopen)
    task = rel.Task(name="t", prompt="do it", files={}, check=lambda ws, s=None: (True, ""))
    rel.run_session(task, 0, "http://proxy", tmp_path, "e2e", 30)
    assert sent[0]["message"] == "do it"
    for field in ("bypass_v3", "v3_mode", "feasibility_mode"):
        assert field not in sent[0], f"the runner still sends {field}"


# Names that once read a switch turning V3, its gates or its generation off.
_REMOVED_SWITCHES = ("BypassV3", "V3Mode", "effectiveV3Mode", "V3Bypassed",
                     "V3GenerationEnabled", "V3PlanningEnabled",
                     "FeasibilityEnforce", "generationSkipped")

# The gate implementations, and the v3-service route each one must still reach.
_MUTATION_GATES = {
    "checkStructuralUnresolved": "/internal/structural_check",
    "embeddedScriptOutcome": "/internal/embedded_script_check",
}


def _go_funcs(src):
    """{name: body} for every top-level func in a gofmt-formatted Go file.

    gofmt puts `func` in column 0 and closes a top-level declaration with a
    lone `}` in column 0, so this needs no brace matching and cannot be misled
    by a brace inside a string literal or a comment.
    """
    funcs, lines = {}, src.splitlines()
    for i, line in enumerate(lines):
        if not line.startswith("func "):
            continue
        sig = line[len("func "):]
        if sig.startswith("("):                     # method receiver
            sig = sig[sig.index(")") + 1:].lstrip()
        name = re.match(r"[A-Za-z_][A-Za-z0-9_]*", sig)
        if not name:
            continue
        for j in range(i + 1, len(lines)):
            if lines[j] == "}":
                funcs[name.group(0)] = "\n".join(lines[i:j + 1])
                break
    return funcs


def test_the_mutation_gates_depend_on_no_switch():
    """Every mutation gate runs on every request.

    The gates still reach the v3-service routes they check with, and no
    production source names a switch that could turn V3, a gate or candidate
    generation off: the three the proxy had are gone.
    """
    sources = {p.name: p.read_text()
               for p in (REPO / "proxy").glob("*.go")
               if not p.name.endswith("_test.go")}
    gates = _go_funcs(sources["gates.go"])
    for fn, route in _MUTATION_GATES.items():
        body = gates.get(fn)
        assert body, f"{fn} is gone; repoint this test at its replacement"
        assert route in body, f"{fn} no longer reaches {route}"
    for name, src in sorted(sources.items()):
        for symbol in _REMOVED_SWITCHES:
            assert not re.search(rf"\b{symbol}\b", src), (
                f"{name} names the removed switch {symbol}")


# --- what a run records about itself --------------------------------------

def test_the_harness_declares_question_for_its_conversational_probes(rel):
    """Declaring work for a question sent it to the work tier and its
    planner, and the H9 detector then blamed ATLAS for running V3 on it."""
    for task in rel.TASKS.values():
        want = "question" if task.conversational else "work"
        assert rel.task_contract(task) == {"task_mode": want}, task.name
    assert any(t.conversational for t in rel.TASKS.values()), \
        "no conversational probe left to test the question contract"


def test_v3_counts_generation_and_delivery_per_write(rel, tmp_path):
    """A run labelled as measuring ATLAS with zero V3 generations measured
    the agent loop alone; the session has to say how much V3 did."""
    delivered = {"type": "tool_result", "data": {
        "tool": "write_file", "success": True, "error": "",
        "data": json.dumps({"bytes_written": 10, "v3_used": True})}}
    events = [
        {"type": "v3_plan", "data": {}},
        _call("write_file", path="a.py"), {"type": "v3_probe", "data": {}},
        {"type": "v3_select", "data": {}}, delivered,
        _call("write_file", path="b.py"), _ok("write_file"),
        # V3 events outside a write are not a generation for it.
        _call("read_file", path="a.py"), {"type": "v3_progress", "data": {}}, _ok(),
    ]
    got = _session(rel, events, tmp_path).v3
    assert got == {"planner_events": 1, "write_calls": 2, "generated": 1, "delivered": 1}, got


def test_stack_identity_records_what_the_proxy_reports(rel):
    """Every recorded dev-server run was steered and ran loose, and nothing
    in the evidence said so."""
    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *args):
            pass

        def do_GET(self):
            body = {"/version": {"api_version": "1.0.0", "grammar_mode": "loose"},
                    "/v1/calibration/status": {
                        "lens": {"verdict": "supported"},
                        "asa": {"verdict": "active", "hint": "control vector active for m"}},
                    }.get(self.path)
            raw = json.dumps(body or {}).encode()
            self.send_response(200 if body else 404)
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    srv = http.server.HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        got = rel.stack_identity(f"http://127.0.0.1:{srv.server_port}")
    finally:
        srv.shutdown()
    assert got["grammar_mode"] == "loose"
    assert got["asa"] == "active" and got["lens"] == "supported"
    assert got["errors"] == {}
    # Unreachable: recorded as such, never a crash.
    down = rel.stack_identity("http://127.0.0.1:9")
    assert down["grammar_mode"] is None and set(down["errors"]) == {"version", "calibration"}


# --- the workspace mounts of the stack under measurement --------------------

def _mounts(mounts, folders=("_reliability",)):
    """A stand-in for subprocess.run: `docker inspect <container>` gives the host folder that the container has at
    /workspace, and `docker exec <container> test -d <folder>` passes for the folders that are there."""
    asked = []

    def run(argv, **kw):
        asked.append(argv[2])
        if argv[:2] == ["docker", "inspect"]:
            return subprocess.CompletedProcess(argv, 0, stdout=mounts.get(argv[2], "") + "\n")
        if argv[:2] == ["docker", "exec"]:
            return subprocess.CompletedProcess(argv, 0 if argv[-1].split("/")[-1] in folders else 1, stdout="")
        raise AssertionError(f"unexpected command {argv}")
    run.asked = asked
    return run


def test_the_mounts_that_are_compared_are_those_of_the_stack_under_measurement(rel):
    # Two stacks on one host: the usual one, and one under another project name with a workspace of its own.
    mounts = {"atlas-atlas-proxy-1": "/home/u/demo", "atlas-sandbox-1": "/home/u/demo",
              "atlas-nightly-atlas-proxy-1": "/srv/nightly/workspace", "atlas-nightly-sandbox-1": "/srv/nightly/workspace"}
    run = _mounts(mounts)
    assert rel.preflight("atlas-nightly-sandbox-1", "_reliability", "atlas-nightly", run=run) == []
    assert run.asked[:2] == ["atlas-nightly-atlas-proxy-1", "atlas-nightly-sandbox-1"]
    assert rel.preflight("atlas-sandbox-1", "_reliability", run=_mounts(mounts)) == []


def test_a_stack_whose_proxy_and_sandbox_have_other_folders_is_refused_under_any_project_name(rel):
    for project, proxy, sandbox in (("atlas", "atlas-atlas-proxy-1", "atlas-sandbox-1"),
                                    ("atlas-nightly", "atlas-nightly-atlas-proxy-1", "atlas-nightly-sandbox-1")):
        problems = rel.preflight(sandbox, "_reliability", project, run=_mounts({proxy: "/a", sandbox: "/b"}))
        assert len(problems) == 1 and "proxy=/a sandbox=/b" in problems[0], problems


def test_a_run_gives_the_check_of_mounts_the_project_name_it_was_started_with(rel):
    calls = [node for node in ast.walk(ast.parse(inspect.getsource(rel.main)))
             if isinstance(node, ast.Call) and getattr(node.func, "id", "") == "preflight"]
    assert len(calls) == 1
    assert [ast.unparse(arg) for arg in calls[0].args] == ["args.sandbox_container", "args.subdir", "args.compose_project"]


def test_a_folder_that_the_sandbox_does_not_have_is_refused(rel):
    mounts = {"atlas-atlas-proxy-1": "/a", "atlas-sandbox-1": "/a"}
    problems = rel.preflight("atlas-sandbox-1", "_reliability", run=_mounts(mounts, folders=()))
    assert len(problems) == 1 and "/workspace/_reliability does not exist inside atlas-sandbox-1" in problems[0]


# --- stack stability (#240) ---------------------------------------------------
#
# A restart or an OOM kill during a session changes its outcome, and the
# result did not say so. The runner now snapshots each container of the
# compose project before and after every session.

def _fake_docker(states):
    """A stand-in for subprocess.run: `docker ps` lists the containers, and
    `docker inspect` reports each one's (restarts, oom_killed, started_at)."""
    def run(argv, **kw):
        if argv[:2] == ["docker", "ps"]:
            return subprocess.CompletedProcess(argv, 0, stdout="\n".join(states) + "\n")
        if argv[:2] == ["docker", "inspect"]:
            return subprocess.CompletedProcess(argv, 0, stdout="".join(
                f"/{n} {r} {'true' if o else 'false'} {t}\n" for n, (r, o, t) in states.items()))
        raise AssertionError(f"unexpected command {argv}")
    return run


@pytest.mark.parametrize("before, after, changes", [
    pytest.param({"lens": (0, False, "T1"), "proxy": (0, False, "T1")},
                 {"lens": (1, False, "T2"), "proxy": (0, False, "T1")},
                 ["lens restarted 1 time"], id="a-restart"),
    pytest.param({"llama": (0, False, "T1")}, {"llama": (2, True, "T3")},
                 ["llama restarted 2 times", "llama was OOM-killed"], id="an-oom-kill"),
    # `docker restart` and a compose recreate leave the restart count alone.
    pytest.param({"sandbox": (0, False, "T1")}, {"sandbox": (0, False, "T2")},
                 ["sandbox was restarted or recreated"], id="a-manual-restart-or-recreate"),
    pytest.param({"lens": (0, False, "T1"), "v3": (0, False, "T1")},
                 {"lens": (0, False, "T1"), "extra": (0, False, "T2")},
                 ["extra appeared", "v3 is gone"], id="a-container-went-away-and-one-appeared"),
    pytest.param({"lens": (3, True, "T1")}, {"lens": (3, True, "T1")}, [], id="a-stable-stack"),
])
def test_stack_changes_names_what_happened_to_each_container(rel, before, after, changes):
    assert rel.stack_changes(rel.container_states("atlas", run=_fake_docker(before)),
                             rel.container_states("atlas", run=_fake_docker(after))) == changes


def test_docker_that_cannot_be_asked_is_not_read_as_stable_mid_session(rel):
    def broken(argv, **kw):
        raise OSError("docker not found")
    assert rel.container_states("atlas", run=broken) is None
    # Unavailable throughout: nothing to compare, nothing claimed.
    assert rel.stack_changes(None, None) == []
    snap = rel.container_states("atlas", run=_fake_docker({"lens": (0, False, "T1")}))
    assert rel.stack_changes(snap, None) == [
        "container state could not be read at the end of the session"]


# --- one commit, five images (#241) --------------------------------------------
#
# A result is evidence only for the stack that produced it. Before a run, the
# runner checks the stated commit against the gated deploy's record and each
# running service's image against the image recorded for that commit.

_IMAGES = {"llama-server": "sha256:aaa", "geometric-lens": "sha256:bbb",
           "v3-service": "sha256:ccc", "sandbox": "sha256:ddd", "atlas-proxy": "sha256:eee"}


def _fake_images(images):
    """`docker ps` lists one container per service; `docker inspect` reports
    each one's compose service label and image id."""
    def run(argv, **kw):
        if argv[:2] == ["docker", "ps"]:
            return subprocess.CompletedProcess(argv, 0, stdout="\n".join(f"atlas-{s}-1" for s in images) + "\n")
        if argv[:2] == ["docker", "inspect"]:
            return subprocess.CompletedProcess(argv, 0, stdout="".join(f"{s} {i}\n" for s, i in images.items()))
        raise AssertionError(f"unexpected command {argv}")
    return run


def _deploy_record(tmp_path, sha="b0e6013", images=None):
    d = tmp_path / "atlas-ralph"
    (d / "deployed").mkdir(parents=True)
    (d / "DEPLOYED_SHA").write_text(sha + "\n")
    (d / "deployed" / f"running-{sha}.json").write_text(json.dumps(
        {"sha": sha, "running": {s: {"image_id": i} for s, i in (images or _IMAGES).items()}}))
    return d


@pytest.mark.parametrize("commit, live, problems", [
    pytest.param("b0e6013", _IMAGES, [], id="the-deployed-commit-and-its-images"),
    pytest.param("b0e6013a1b2c", _IMAGES, [], id="the-same-commit-named-at-another-length"),
    pytest.param("b0e6013", dict(_IMAGES, **{"v3-service": "sha256:999"}),
                 ["v3-service runs sha256:999, not the sha256:ccc deployed for b0e6013"],
                 id="a-mixed-stack"),
    pytest.param("47907c9", _IMAGES, ["the checkout is at 47907c9, but the stack was deployed from b0e6013"],
                 id="a-checkout-that-is-not-the-deployed-commit"),
    pytest.param("b0e6013", {s: i for s, i in _IMAGES.items() if s != "llama-server"},
                 ["llama-server is not running"],
                 id="a-service-that-is-not-running"),
])
def test_a_stack_is_verified_only_with_the_images_deployed_for_its_commit(rel, tmp_path, commit, live, problems):
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), commit, run=_fake_images(live))
    assert ident["problems"] == problems
    # With a deploy record, a stack with no problem is verified, and any problem refuses it.
    assert ident["verified"] is (problems == [])
    assert ident["mismatch"] is (problems != [])
    assert ident["commit"] == "b0e6013"
    assert ident["images"] == live


def test_a_stack_with_no_deploy_record_is_unverified_not_refused(rel, tmp_path):
    ident = rel.deployed_identity("atlas", tmp_path / "none", "b0e6013",
                                  run=_fake_images(_IMAGES))
    assert not ident["mismatch"] and not ident["verified"]
    assert ident["images"] == _IMAGES
    assert "unverified" in ident["problems"][0]


def test_the_runner_refuses_a_mixed_stack_before_any_session(rel, tmp_path, monkeypatch):
    ws = tmp_path / "ws"
    ws.mkdir()
    record = _deploy_record(tmp_path)
    monkeypatch.setattr(rel, "stack_identity", lambda url: {})
    monkeypatch.setattr(rel, "running_images",
                        lambda project, run=None: dict(_IMAGES, sandbox="sha256:777"))

    def no_session(*a, **k):
        raise AssertionError("a session ran on a mixed stack")
    monkeypatch.setattr(rel, "run_session", no_session)
    monkeypatch.setattr(sys, "argv", ["e2e-reliability.py", "--workspace", str(ws),
                                      "--deploy-dir", str(record), "--commit", "b0e6013",
                                      "--sandbox-container", "", "--tasks", "offbyone"])
    assert rel.main() == 2


def test_the_identity_and_the_stack_changes_are_kept_with_every_result(rel, tmp_path):
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), "b0e6013",
                                  run=_fake_images(_IMAGES))
    stack = {"grammar_mode": "loose", "commit": ident["commit"], "images": ident["images"],
             "identity_verified": ident["verified"]}
    s = _session(rel, [], tmp_path)
    s.stack_changes = ["lens restarted 1 time"]
    row = rel.result_row(s, {}, stack)
    assert row["stack"]["commit"] == "b0e6013"
    assert row["stack"]["images"]["atlas-proxy"] == "sha256:eee"
    assert row["stack"]["identity_verified"] is True
    assert row["stack_changes"] == ["lens restarted 1 time"]


# --- A stopped run keeps what it has (#222) -------------------------------

_DETECTORS = ("h1_protocol", "h2_false_rejection", "h3_dead_end_steering", "h4_gate_escape",
              "h5_corrupt_write", "h8_anchored_on_injected_text", "h9_tier_misapplied",
              "h7_background_leak")


def _recorded(task, rep, workspace):
    """A finished session, as run_session returns it."""
    return {"task": task.name, "rep": rep, "workspace": workspace, "wall_s": 1.0,
            "stream_ok": True, "events": [{"type": "turn_start", "data": {}},
                                          {"type": "done", "data": {"summary": "x"}}]}


def _sigterm(task, rep, workspace):
    """The operator stops the run while this session is in flight."""
    if signal.getsignal(signal.SIGTERM) in (signal.SIG_DFL, None):
        raise AssertionError("no SIGTERM handler: the run would die with nothing written")
    os.kill(os.getpid(), signal.SIGTERM)
    time.sleep(5)
    raise AssertionError("SIGTERM did not stop the run")


def _run_main(rel, tmp_path, monkeypatch, sessions, reps):
    """main() over recorded sessions, one H6 defect each, results in out.json."""
    ws = tmp_path / "ws"
    ws.mkdir()
    out = tmp_path / "out.json"
    monkeypatch.setattr(rel, "stack_identity", lambda url: {})
    for name in _DETECTORS:
        monkeypatch.setattr(rel, name, lambda *a: [])
    monkeypatch.setattr(rel, "h6_service_fault", lambda s: ["H6 service fault: sandbox answered 500"])
    queue = iter(sessions)
    monkeypatch.setattr(rel, "run_session",
                        lambda task, rep, *a, **k: rel.Session(**next(queue)(task, rep, ws)))
    monkeypatch.setattr(sys, "argv", ["e2e-reliability.py", "--workspace", str(ws),
                                      "--deploy-dir", str(tmp_path / "none"),
                                      "--compose-project", "", "--sandbox-container", "",
                                      "--tasks", "offbyone", "--reps", str(reps),
                                      "--json", str(out)])
    return rel.main(), out


def test_a_stopped_run_keeps_its_defects_and_a_partial_summary(rel, tmp_path, monkeypatch):
    before = signal.getsignal(signal.SIGTERM)
    code, out = _run_main(rel, tmp_path, monkeypatch, [_recorded, _sigterm, _recorded], reps=3)
    assert code == 128 + signal.SIGTERM
    logged = [json.loads(line)
              for line in out.with_suffix(".defects.jsonl").read_text().splitlines()]
    assert logged == [{"task": "offbyone", "rep": 1,
                       "defect": "H6 service fault: sandbox answered 500"}]
    assert [row["rep"] for row in json.loads(out.read_text())] == [1]
    summary = out.with_suffix(".summary.txt").read_text()
    assert "Harness Integrity Rate   0/1" in summary
    assert "H6 service fault" in summary
    assert "stopped by SIGTERM after 1 session(s)" in summary
    assert signal.getsignal(signal.SIGTERM) is before


def test_a_full_run_logs_each_defect_and_writes_no_partial_summary(rel, tmp_path, monkeypatch):
    code, out = _run_main(rel, tmp_path, monkeypatch, [_recorded, _recorded], reps=2)
    assert code == 1
    assert [row["rep"] for row in json.loads(out.read_text())] == [1, 2]
    assert len(out.with_suffix(".defects.jsonl").read_text().splitlines()) == 2
    assert not out.with_suffix(".summary.txt").exists()
