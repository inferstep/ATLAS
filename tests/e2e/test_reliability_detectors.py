"""Unit tests for the harness-defect detectors in scripts/e2e-reliability.py.

The detectors decide whether a live session's failure was ATLAS's fault or the
model's, so a detector that silently stops firing turns the reliability number
into a rubber stamp. Each test below is a synthetic event stream reproducing a
defect that was actually observed on 2026-07-31, plus the corresponding clean
stream, so a detector cannot pass by flagging everything.

No live stack: streams are literals and the workspace is a tmp_path.
"""
import importlib.util
import json
import os
import re
import signal
import sys
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


# --- H1 protocol ----------------------------------------------------------

def test_h1_flags_orphaned_tool_call(rel, tmp_path):
    s = _session(rel, [_call("read_file", path="a.py"), _ok(),
                       _call("edit_file", path="a.py"),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert any("orphaned call" in d for d in rel.h1_protocol(s, {"tool_call", "tool_result", "done"}))


def test_h1_flags_event_the_tui_cannot_render(rel, tmp_path):
    s = _session(rel, [{"type": "brand_new_event", "data": {}},
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    found = rel.h1_protocol(s, {"done"})
    assert any("cannot render" in d and "brand_new_event" in d for d in found)


def test_h1_does_not_charge_the_runner_cap_to_the_proxy(rel, tmp_path):
    """This runner stops reading at --timeout and appends its own error
    event. The proxy never got to send `done` and the socket closed
    mid-stream, so counting those as two protocol violations scores our
    deadline as its defect."""
    cap = {"type": "error",
           "data": {"error": "harness cap: session exceeded 900s"}}
    s = _session(rel, [_call("read_file", path="a.py"), _ok(), cap],
                 tmp_path, stream_ok=False)
    found = rel.h1_protocol(s, {"tool_call", "tool_result", "done", "error"})
    assert not any("protocol" in d for d in found)
    assert any("timeout" in d and "cap" in d for d in found)


def test_h1_capped_session_tolerates_only_the_in_flight_call(rel, tmp_path):
    cap = {"type": "error",
           "data": {"error": "harness cap: session exceeded 900s"}}
    known = {"tool_call", "tool_result", "done", "error"}

    one = _session(rel, [_call("read_file", path="a.py"), _ok(),
                         _call("edit_file", path="a.py"), cap],
                   tmp_path, stream_ok=False)
    assert not any("orphaned" in d for d in rel.h1_protocol(one, known))

    two = _session(rel, [_call("read_file", path="a.py"),
                         _call("edit_file", path="a.py"), cap],
                   tmp_path, stream_ok=False)
    assert any("orphaned" in d for d in rel.h1_protocol(two, known))


def test_h1_clean_stream_is_clean(rel, tmp_path):
    s = _session(rel, [_call("read_file", path="a.py"), _ok(),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert rel.h1_protocol(s, {"tool_call", "tool_result", "done"}) == []


# --- H2 false rejection (the D9 class) ------------------------------------

def test_h2_flags_rejection_blaming_a_file_that_is_fine(rel, tmp_path):
    """V3 authored the break, the gate blocked it, the message named the file.

    The model then hunts a defect that is not on disk. Decided by re-checking
    the file, which is exactly what a human does to catch this.
    """
    (tmp_path / "app.py").write_text("def index():\n    return 'ok'\n")
    s = _session(rel, [_call("edit_file", path="app.py"),
                       _fail("/workspace/app.py has a JavaScript syntax error in "
                             "the <script> block — it was NOT written.\nline 121: "
                             "unexpected `)`"),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert any("blamed for a syntax error it does not have"
               in d for d in rel.h2_false_rejection(s))


def test_h2_silent_when_the_file_really_is_broken(rel, tmp_path):
    (tmp_path / "app.py").write_text("def index(:\n")  # genuinely unparseable
    s = _session(rel, [_call("edit_file", path="app.py"),
                       _fail("/workspace/app.py has a Python syntax error — it "
                             "was NOT written."),
                       {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert rel.h2_false_rejection(s) == []


# --- H3 dead-end steering (the D10 class) ---------------------------------

def test_h3_flags_advice_the_next_call_is_refused_for_taking(rel, tmp_path):
    """Advice offered `<body>`; the model reached for `<script>`.

    Different tag, same unsupported shape — matching the literal string would
    miss the case this exists to catch.
    """
    s = _session(rel, [
        _call("edit_file", path="app.py"),
        _fail("string to replace not found in file. To replace a whole element "
              "use structural_edit with a selector (e.g. `function:NAME`, "
              "`class:NAME`, `<body>`) and the new content."),
        _call("structural_edit", path="app.py", selector="<script>"),
        _fail("unknown selector '<script>' for python. Supported: "
              "function:NAME, class:NAME"),
        {"type": "done", "data": {"summary": "x"}}], tmp_path)
    found = rel.h3_dead_end_steering(s)
    assert any("<tag>" in d for d in found), found


def test_h3_silent_when_advice_matched_the_file(rel, tmp_path):
    s = _session(rel, [
        _call("edit_file", path="app.py"),
        _fail("string to replace not found. Use structural_edit with a "
              "selector (`function:NAME` or `class:NAME`)."),
        _call("structural_edit", path="app.py", selector="function:index"),
        _fail("structural_edit: your replacement is IDENTICAL to the code "
              "already in the file"),
        {"type": "done", "data": {"summary": "x"}}], tmp_path)
    assert rel.h3_dead_end_steering(s) == []


# --- H4 gate escape (the D11 class) ---------------------------------------

def test_h4_flags_exit_with_no_write_on_an_action_prompt(rel, tmp_path):
    s = _session(rel, [_call("read_file", path="app.py"), _ok(),
                       {"type": "text", "data": {"content":
                        "The pause logic is already present in the template."}},
                       {"type": "done", "data": {"summary": ""}}], tmp_path)
    assert rel.h4_gate_escape(s) != []


def test_h4_silent_when_the_breaker_ended_honestly(rel, tmp_path):
    """An honest 'I stopped' is the machinery working, not an escape."""
    s = _session(rel, [_call("read_file", path="app.py"), _ok(),
                       {"type": "done", "data": {"summary":
                        "Stopped after 3 tool failures on the same target "
                        "with no successful changes."}}], tmp_path)
    assert rel.h4_gate_escape(s) == []


def test_h4_silent_when_a_write_landed(rel, tmp_path):
    s = _session(rel, [_call("edit_file", path="app.py"), _ok("edit_file"),
                       {"type": "done", "data": {"summary": "Added the toggle."}}],
                 tmp_path)
    assert rel.h4_gate_escape(s) == []


# --- H5 corrupt write -----------------------------------------------------

def test_h5_flags_unparseable_file_left_on_disk(rel, tmp_path):
    (tmp_path / "broken.py").write_text("def f(:\n")
    task = rel.Task(name="t", prompt="", files={}, check=lambda p: (True, ""))
    s = _session(rel, [], tmp_path)
    assert any("broken.py" in d for d in rel.h5_corrupt_write(s, task))


def test_h5_flags_a_required_file_that_was_deleted(rel, tmp_path):
    task = rel.Task(name="t", prompt="", files={}, check=lambda p: (True, ""),
                    must_exist=("gone.py",))
    s = _session(rel, [], tmp_path)
    assert any("gone.py was deleted" in d for d in rel.h5_corrupt_write(s, task))


def test_h5_clean_workspace_is_clean(rel, tmp_path):
    (tmp_path / "fine.py").write_text("x = 1\n")
    task = rel.Task(name="t", prompt="", files={}, check=lambda p: (True, ""),
                    must_exist=("fine.py",))
    s = _session(rel, [], tmp_path)
    assert rel.h5_corrupt_write(s, task) == []


# --- TUI coverage ---------------------------------------------------------

def test_tui_handled_types_is_populated_and_has_the_core_events(rel):
    """Located by content marker, so a file move must not empty the set."""
    handled = rel.tui_handled_types()
    assert len(handled) > 30, f"only found {len(handled)} — dispatcher not located?"
    for core in ("tool_call", "tool_result", "done", "text", "error"):
        assert core in handled, f"TUI dispatcher has no case for {core!r}"


# --- H7 silent background leak -------------------------------------------

def test_h7_is_silent_when_the_session_announced_the_jobs(rel, tmp_path, monkeypatch):
    """Persistence is deliberate; the defect is persistence nobody was told about.

    An agent loop is one user message, so killing jobs at its end would break
    "start the dev server" then "now curl it". H7 must therefore fire on
    silence, not on the jobs existing.
    """
    monkeypatch.setattr(rel.subprocess, "run",
                        lambda *a, **k: type("P", (), {"stdout": "2", "returncode": 0})())
    announced = _session(rel, [{"type": "done", "data": {"summary":
        "Done.\n\nStill running in the sandbox:\n  abc — python app.py\n"
        "These keep their ports until stopped. Use stop_background to end them."}}],
        tmp_path)
    assert rel.h7_background_leak("atlas-sandbox-1", announced) == []

    silent = _session(rel, [{"type": "done", "data": {"summary": "Added the toggle."}}],
                      tmp_path)
    found = rel.h7_background_leak("atlas-sandbox-1", silent)
    assert found and "silent background leak" in found[0]


# --- H8 anchored on ATLAS-injected text ----------------------------------

def test_h8_flags_old_str_copied_from_the_call_graph_footer(rel, tmp_path):
    """read_file appends a footer that is not on disk.

    A measured session anchored edit_file on "## Call graph (within this
    file)\\n- mean calls: ..." and spent all three of its failures on an edit
    that could never match. Scoring that against the model would make the
    harness understate the defects it exists to find.
    """
    s = _session(rel, [
        _call("edit_file", path="stats.py",
              old_str="\n\n\n## Call graph (within this file)\n"
                      "- mean calls: ValueError, sum, len"),
        _fail("string to replace not found in file."),
        {"type": "done", "data": {"summary": "Stopped after 3 tool failures."}},
    ], tmp_path)
    found = rel.h8_anchored_on_injected_text(s)
    assert found and "Call graph" in found[0]


def test_h8_silent_on_a_normal_old_str(rel, tmp_path):
    s = _session(rel, [
        _call("edit_file", path="stats.py", old_str="def mean(values):"),
        _fail("string to replace not found in file."),
        {"type": "done", "data": {"summary": "x"}},
    ], tmp_path)
    assert rel.h8_anchored_on_injected_text(s) == []


def test_h2_silent_on_a_message_that_blames_the_models_content(rel, tmp_path):
    """"Your content for X has a syntax error" is the CORRECT message.

    It blames the submission, and the file on disk is clean precisely because
    the write was refused. An earlier version of this detector matched it and
    reported a harness defect on a session where ATLAS behaved perfectly —
    a false-positive detector is worse than a missing one, because it sends
    someone chasing a bug that is not there.
    """
    (tmp_path / "store.py").write_text("x = 1\n")
    s = _session(rel, [
        _call("write_file", path="store.py"),
        _fail("Your content for store.py has a syntax error (SyntaxError: "
              "unmatched ')') — it was NOT written. The content is NOT "
              "truncated; it is complete but INVALID."),
        {"type": "done", "data": {"summary": "x"}},
    ], tmp_path)
    assert rel.h2_false_rejection(s) == []


# --- H9 tier appropriateness ---------------------------------------------

def test_h9_flags_v3_running_on_a_question(rel, tmp_path):
    """The tiers exist so the heavy pipeline does not run on everything.

    A wrong answer is a model limit. Spending a multi-minute V3 pipeline on
    "what does this function do" is a product defect that costs real minutes.
    """
    task = rel.Task(name="ask", prompt="what does f do?", files={},
                    check=lambda p, s=None: (True, ""), conversational=True)
    s = _session(rel, [
        {"type": "v3_probe", "data": {"stage": "probe"}},
        {"type": "text", "data": {"content": "It counts duplicates."}},
        {"type": "done", "data": {"summary": ""}},
    ], tmp_path)
    found = rel.h9_tier_misapplied(s, task)
    assert found and "V3 pipeline ran on a question" in found[0]


def test_h9_flags_a_question_that_edited_files(rel, tmp_path):
    task = rel.Task(name="ask", prompt="what does f do?", files={},
                    check=lambda p, s=None: (True, ""), conversational=True)
    s = _session(rel, [
        _call("edit_file", path="orders.py"), _ok("edit_file"),
        {"type": "done", "data": {"summary": "done"}},
    ], tmp_path)
    found = rel.h9_tier_misapplied(s, task)
    assert found and "caused file writes" in found[0]


def test_h9_silent_on_a_clean_conversational_turn(rel, tmp_path):
    task = rel.Task(name="ask", prompt="what does f do?", files={},
                    check=lambda p, s=None: (True, ""), conversational=True)
    s = _session(rel, [
        _call("read_file", path="orders.py"), _ok(),
        {"type": "text", "data": {"content": "It is quadratic."}},
        {"type": "done", "data": {"summary": ""}},
    ], tmp_path)
    assert rel.h9_tier_misapplied(s, task) == []


def test_h9_does_not_constrain_a_work_task(rel, tmp_path):
    """V3 and writes are exactly what a coding task should produce."""
    task = rel.Task(name="fix", prompt="fix the bug", files={},
                    check=lambda p: (True, ""))
    s = _session(rel, [
        {"type": "v3_probe", "data": {}},
        _call("edit_file", path="a.py"), _ok(),
        {"type": "done", "data": {"summary": "fixed"}},
    ], tmp_path)
    assert rel.h9_tier_misapplied(s, task) == []


def test_h4_silent_on_a_conversational_task(rel, tmp_path):
    """A question SHOULD exit without writing.

    Scoring that as a gate escape reported a harness defect on both
    conversational probes for behaving exactly as asked — the inverse of the
    H9 tier check.
    """
    task = rel.Task(name="ask", prompt="what does f do?", files={},
                    check=lambda p, s=None: (True, ""), conversational=True)
    s = _session(rel, [
        _call("read_file", path="orders.py"), _ok(),
        {"type": "text", "data": {"content": "It is quadratic."}},
        {"type": "done", "data": {"summary": ""}},
    ], tmp_path)
    assert rel.h4_gate_escape(s, task) == []


def test_h4_still_fires_on_a_work_task(rel, tmp_path):
    task = rel.Task(name="fix", prompt="fix the bug", files={},
                    check=lambda p: (True, ""))
    s = _session(rel, [_call("read_file", path="a.py"), _ok(),
                       {"type": "done", "data": {"summary": ""}}], tmp_path)
    assert rel.h4_gate_escape(s, task) != []


# --- bug-find check must not accept an invented mechanism ----------------

def test_bugfind_rejects_the_right_file_with_a_wrong_mechanism(rel, tmp_path):
    """The real cycle-6 answer, which an earlier version of the check passed.

    It named planning.py and the symptom correctly, then attributed the cause
    to "how min() is used with a custom key" — there is no min() there, and
    the function it named was the scorer, not the selection loop. Right file,
    invented mechanism, and a loose check called it a pass.
    """
    task = rel.TASKS["bugfind_tiebreak"]
    s = _session(rel, [{"type": "text", "data": {"content":
        "The issue is in `planning.py` within the `_score_plan` function. When two "
        "plans have the same score, the code selects the one with the maximum number "
        "of steps because of how the `min()` function is being used with a custom key."}},
        {"type": "done", "data": {"summary": ""}}], tmp_path)
    passed, _ = task.check(tmp_path, s)
    assert not passed


def test_bugfind_accepts_the_actual_comparison(rel, tmp_path):
    task = rel.TASKS["bugfind_tiebreak"]
    s = _session(rel, [{"type": "text", "data": {"content":
        "planning.py: the selection loop breaks ties with n_steps > best_steps, "
        "so a tie keeps the longer plan. It should be <."}},
        {"type": "done", "data": {"summary": ""}}], tmp_path)
    passed, _ = task.check(tmp_path, s)
    assert passed


# --- H6 service fault ------------------------------------------------------

def test_h6_does_not_charge_the_runner_cap_to_the_proxy(rel, tmp_path):
    """The cap event is this runner's own, appended when it stops reading at
    --timeout. h1_protocol already reports it as the timeout it is; counting
    it again here charged one deadline as two separate proxy defects."""
    cap = {"type": "error",
           "data": {"error": "harness cap: session exceeded 900s"}}
    s = _session(rel, [_call("read_file", path="a.py"), _ok(), cap],
                 tmp_path, stream_ok=False)
    assert rel.h6_service_fault(s) == []


def test_h6_still_reports_a_real_service_fault(rel, tmp_path):
    boom = {"type": "error", "data": {"error": "v3 service: connection refused"}}
    s = _session(rel, [boom], tmp_path, stream_ok=False)
    found = rel.h6_service_fault(s)
    assert any("connection refused" in d for d in found)


def test_h6_ignores_a_parse_failure_the_session_recovered_from(rel, tmp_path):
    """flask_pause rep2, 2026-08-03: the model emitted a 20 KB tool call that
    ran out of tokens mid-JSON, the proxy classified it and told the model,
    and the session went on to pass the task — scored a harness defect for
    it. Recovered model behaviour is the proxy working."""
    err = {"type": "error", "data": {"category": "truncated_tool",
                                     "error": "failed to parse model response"}}
    s = _session(rel, [_call("edit_file", path="app.py"), err,
                       _call("replace_lines", path="app.py"), _ok("replace_lines"),
                       {"type": "done", "data": {"summary": "added the toggle"}}],
                 tmp_path)
    assert rel.h6_service_fault(s) == []


def test_h6_still_reports_a_parse_failure_the_session_died_on(rel, tmp_path):
    err = {"type": "error", "data": {"error": "failed to parse model response"}}
    s = _session(rel, [_call("edit_file", path="app.py"), err], tmp_path,
                 stream_ok=False)
    assert any("parse model response" in d for d in rel.h6_service_fault(s))


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


def test_h6_does_not_charge_the_work_deadline_to_a_service(rel, tmp_path):
    """Smoke run 2026-09-28 (multifile_cli rep 2): the session's own work
    deadline cut an LLM stream; the terminal status already says timed_out."""
    cut = {"type": "error",
           "data": {"error": "read LLM stream: context deadline exceeded"}}
    done = {"type": "done", "data": {"status": "timed_out", "reason": "work_deadline"}}
    s = _session(rel, [_call("read_file", path="x.py"), _ok(), cut, done], tmp_path)
    assert rel.h6_service_fault(s) == []
    # The same cut in a session that did not end on its deadline still counts.
    s = _session(rel, [cut], tmp_path, stream_ok=False)
    assert any("context deadline exceeded" in d for d in rel.h6_service_fault(s))


# --- V3 is always on -------------------------------------------------------
#
# The runners measure the shipped system. No request field turns V3 off: the
# proxy refuses bypass_v3, v3_mode and feasibility_mode when they ask for a
# different system, so a V3-free comparison takes a research build, never a
# flag on the product.


def _capture_body(rel, monkeypatch, **kwargs):
    """Run one session against a stubbed transport and return the request."""
    seen = {}

    class _Resp:
        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def __iter__(self):
            return iter([b'data: {"type":"done"}\n\n'])

    def fake_urlopen(req, timeout=None):
        seen["url"] = req.full_url
        seen["body"] = json.loads(req.data.decode())
        return _Resp()

    monkeypatch.setattr(rel.urllib.request, "urlopen", fake_urlopen)
    return seen


def test_the_runner_sends_no_removed_switch(rel, monkeypatch, tmp_path):
    import json as _json
    globals()["json"] = _json
    seen = _capture_body(rel, monkeypatch)
    task = rel.Task(name="t", prompt="do it", files={}, check=lambda ws, s=None: (True, ""))
    rel.run_session(task, 0, "http://proxy", tmp_path, "e2e", 30)
    assert seen["body"]["message"] == "do it"
    for field in ("bypass_v3", "v3_mode", "feasibility_mode"):
        assert field not in seen["body"], f"the runner still sends {field}"


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
    import http.server
    import threading

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


# --- stack stability (#240) ---------------------------------------------------
#
# A restart or an OOM kill during a session changes its outcome, and the
# result did not say so. The runner now snapshots each container of the
# compose project before and after every session.

class _Proc:
    def __init__(self, stdout="", returncode=0):
        self.stdout, self.returncode = stdout, returncode


def _fake_docker(states):
    """A stand-in for subprocess.run: `docker ps` lists the containers, and
    `docker inspect` reports each one's (restarts, oom_killed, started_at)."""
    def run(argv, **kw):
        if argv[:2] == ["docker", "ps"]:
            return _Proc("\n".join(states) + "\n")
        if argv[:2] == ["docker", "inspect"]:
            return _Proc("".join(f"/{n} {r} {'true' if o else 'false'} {t}\n"
                                 for n, (r, o, t) in states.items()))
        raise AssertionError(f"unexpected command {argv}")
    return run


def _states(rel, **containers):
    return rel.container_states("atlas", run=_fake_docker(containers))


def test_a_restart_during_the_session_is_in_its_result(rel, tmp_path):
    before = _states(rel, lens=(0, False, "T1"), proxy=(0, False, "T1"))
    after = _states(rel, lens=(1, False, "T2"), proxy=(0, False, "T1"))
    s = _session(rel, [], tmp_path)
    s.stack_changes = rel.stack_changes(before, after)
    assert s.stack_changes == ["lens restarted 1 time"]
    assert rel.result_row(s, {}, {})["stack_changes"] == ["lens restarted 1 time"]


def test_an_oom_kill_is_named(rel):
    before = _states(rel, llama=(0, False, "T1"))
    after = _states(rel, llama=(2, True, "T3"))
    assert rel.stack_changes(before, after) == ["llama restarted 2 times", "llama was OOM-killed"]


def test_a_manual_restart_or_recreate_is_named(rel):
    # `docker restart` and a compose recreate leave the restart count alone.
    before = _states(rel, sandbox=(0, False, "T1"))
    after = _states(rel, sandbox=(0, False, "T2"))
    assert rel.stack_changes(before, after) == ["sandbox was restarted or recreated"]


def test_a_container_that_went_away_or_appeared_is_named(rel):
    before = _states(rel, lens=(0, False, "T1"), v3=(0, False, "T1"))
    after = _states(rel, lens=(0, False, "T1"), extra=(0, False, "T2"))
    assert rel.stack_changes(before, after) == ["extra appeared", "v3 is gone"]


def test_a_stable_stack_records_nothing(rel):
    before = _states(rel, lens=(3, True, "T1"))
    after = _states(rel, lens=(3, True, "T1"))
    assert rel.stack_changes(before, after) == []


def test_docker_that_cannot_be_asked_is_not_read_as_stable_mid_session(rel):
    def broken(argv, **kw):
        raise OSError("docker not found")
    assert rel.container_states("atlas", run=broken) is None
    # Unavailable throughout: nothing to compare, nothing claimed.
    assert rel.stack_changes(None, None) == []
    snap = _states(rel, lens=(0, False, "T1"))
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
            return _Proc("\n".join(f"atlas-{s}-1" for s in images) + "\n")
        if argv[:2] == ["docker", "inspect"]:
            return _Proc("".join(f"{s} {i}\n" for s, i in images.items()))
        raise AssertionError(f"unexpected command {argv}")
    return run


def _deploy_record(tmp_path, sha="b0e6013", images=None):
    d = tmp_path / "atlas-ralph"
    (d / "deployed").mkdir(parents=True)
    (d / "DEPLOYED_SHA").write_text(sha + "\n")
    (d / "deployed" / f"running-{sha}.json").write_text(json.dumps(
        {"sha": sha, "running": {s: {"image_id": i} for s, i in (images or _IMAGES).items()}}))
    return d


def test_a_single_commit_stack_is_verified_and_its_images_recorded(rel, tmp_path):
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), "b0e6013",
                                  run=_fake_images(_IMAGES))
    assert ident["verified"] and not ident["mismatch"], ident["problems"]
    assert ident["commit"] == "b0e6013"
    assert ident["images"] == _IMAGES


def test_a_mixed_stack_is_refused(rel, tmp_path):
    live = dict(_IMAGES, **{"v3-service": "sha256:999"})
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), "b0e6013",
                                  run=_fake_images(live))
    assert ident["mismatch"] and not ident["verified"]
    assert any("v3-service runs sha256:999" in p for p in ident["problems"]), ident["problems"]


def test_a_checkout_that_is_not_the_deployed_commit_is_refused(rel, tmp_path):
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), "47907c9",
                                  run=_fake_images(_IMAGES))
    assert ident["mismatch"]
    assert any("deployed from b0e6013" in p for p in ident["problems"])
    # The same commit, named at another length, is the same commit.
    ok = rel.deployed_identity("atlas", _deploy_record(tmp_path / "x"), "b0e6013a1b2c",
                               run=_fake_images(_IMAGES))
    assert ok["verified"], ok["problems"]


def test_a_service_that_is_not_running_is_refused(rel, tmp_path):
    live = {s: i for s, i in _IMAGES.items() if s != "llama-server"}
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), "b0e6013",
                                  run=_fake_images(live))
    assert ident["mismatch"]
    assert "llama-server is not running" in ident["problems"]


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


def test_the_identity_is_kept_with_every_result(rel, tmp_path):
    ident = rel.deployed_identity("atlas", _deploy_record(tmp_path), "b0e6013",
                                  run=_fake_images(_IMAGES))
    stack = {"grammar_mode": "loose", "commit": ident["commit"], "images": ident["images"],
             "identity_verified": ident["verified"]}
    row = rel.result_row(_session(rel, [], tmp_path), {}, stack)
    assert row["stack"]["commit"] == "b0e6013"
    assert row["stack"]["images"]["atlas-proxy"] == "sha256:eee"
    assert row["stack"]["identity_verified"] is True


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
