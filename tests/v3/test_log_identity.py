"""Every request-serving V3 log record names its request and its invocation.

Route attribution in the 2026-08-23 acquisition had to be recovered from
proxy logs by time window, because the V3 container emitted 158,641 lines and
not one carried a request ID. That works only at concurrency 1 with proven
non-overlapping windows. These tests pin the identity that replaces it.

Observability only: enabling structured logs must not change what the model
sees, what is generated, what is selected, or what is delivered.
"""
import ast
import io
import json
import os
import sys
import threading

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
if V3 not in sys.path:
    sys.path.insert(0, V3)

import structured_log as SL  # noqa: E402


@pytest.fixture(autouse=True)
def _clean_identity():
    SL.bind_identity("", "")
    yield
    SL.bind_identity("", "")


def _records(buf):
    return [json.loads(x) for x in buf.getvalue().splitlines() if x.strip()]


# --- the identity itself ------------------------------------------------------

def test_request_and_invocation_are_separate_contextvars():
    SL.set_request_id("req-1")
    SL.set_invocation_id("inv-1")
    assert SL.current_identity() == ("req-1", "inv-1")
    SL.set_invocation_id("inv-2")
    assert SL.current_identity() == ("req-1", "inv-2")


def test_formatter_emits_both_identities():
    import logging
    SL.bind_identity("req-A", "inv-A")
    fmt = SL.JsonFormatter("v3-service")
    rec = logging.LogRecord("t", logging.INFO, __file__, 1, "hello", None, None)
    out = json.loads(fmt.format(rec))
    assert out["request_id"] == "req-A"
    assert out["invocation_id"] == "inv-A"
    assert out["msg"] == "hello"


def test_absent_identity_omits_the_fields_rather_than_emitting_empties():
    import logging
    SL.bind_identity("", "")
    out = json.loads(SL.JsonFormatter("v3-service").format(
        logging.LogRecord("t", logging.INFO, __file__, 1, "x", None, None)))
    assert "request_id" not in out and "invocation_id" not in out


# --- worker threads -----------------------------------------------------------

def test_worker_threads_inherit_nothing_and_must_be_bound():
    """The premise. A worker sees the defaults until something binds them."""
    SL.bind_identity("req-A", "inv-A")
    seen = {}

    def worker():
        seen["unbound"] = SL.current_identity()
        SL.bind_identity("req-A", "inv-A")
        seen["bound"] = SL.current_identity()

    t = threading.Thread(target=worker)
    t.start()
    t.join()
    assert seen["unbound"] == ("", "")
    assert seen["bound"] == ("req-A", "inv-A")


def test_plansearch_fan_out_binds_both_identities_in_workers():
    from stages import plan_search as PS
    SL.bind_identity("req-OWNER", "inv-OWNER")
    stage = PS.PlanSearch(PS.PlanSearchConfig(enabled=True, num_plans=3))
    stage._events_file = None
    seen = []

    def fn(idx, item):
        seen.append((idx, SL.current_identity(), threading.current_thread().name))
        return f"r{idx}"

    caller = threading.current_thread().name
    out = stage._fan_out([(0, "a"), (1, "b"), (2, "c")], fn)
    assert [o[1] for o in out] == ["r0", "r1", "r2"]
    assert {s[1] for s in seen} == {("req-OWNER", "inv-OWNER")}
    # The pool may reuse one worker for three instant calls; what matters is
    # that the work left the calling thread, which is where the ContextVars live.
    assert all(s[2] != caller for s in seen), "fan-out ran on the calling thread"


def test_two_concurrent_requests_cannot_exchange_identities():
    from stages import plan_search as PS
    stage = PS.PlanSearch(PS.PlanSearchConfig(enabled=True, num_plans=3))
    stage._events_file = None
    seen = {"A": set(), "B": set()}
    errs = []

    def parent(tag):
        try:
            SL.bind_identity(f"req-{tag}", f"inv-{tag}")

            def fn(idx, item):
                seen[tag].add(SL.current_identity())
                return idx

            stage._fan_out([(0, "x"), (1, "y"), (2, "z")], fn)
        except Exception as exc:  # noqa: BLE001
            errs.append(exc)

    ta = threading.Thread(target=parent, args=("A",))
    tb = threading.Thread(target=parent, args=("B",))
    ta.start(); tb.start(); ta.join(); tb.join()
    assert not errs, errs
    assert seen["A"] == {("req-A", "inv-A")}
    assert seen["B"] == {("req-B", "inv-B")}


# --- joinability, structurally -------------------------------------------------

def test_both_handlers_bind_the_invocation_id():
    """Planner, generation, cancellation and teardown records all come from a
    handler that ran _watch_parent_for, which is where the binding lives."""
    src = ast.parse(open(os.path.join(V3, "main.py"), encoding="utf-8").read())
    watcher = next(n for n in ast.walk(src)
                   if isinstance(n, ast.FunctionDef) and n.name == "_watch_parent_for")
    names = {getattr(c.func, "id", None) or getattr(c.func, "attr", None)
             for c in ast.walk(watcher) if isinstance(c, ast.Call)}
    assert "_set_inv" in names or "set_invocation_id" in names, (
        "_watch_parent_for does not bind the invocation id; records from that "
        "invocation would not be joinable to its relay calls")


def test_print_wrapper_carries_both_identities(monkeypatch):
    """V3 logs through print(), so the stdout wrapper is the main producer."""
    monkeypatch.setenv("ATLAS_LOG_FORMAT", "json")
    import main
    buf = io.StringIO()
    wrapper = main._PrivateValueStream(buf)
    SL.bind_identity("req-P", "inv-P")
    wrapper.write("a printed line\n")
    recs = _records(buf)
    assert recs and recs[0]["request_id"] == "req-P"
    assert recs[0]["invocation_id"] == "inv-P"
    assert recs[0]["msg"] == "a printed line"


def test_print_wrapper_off_by_default_leaves_output_untouched(monkeypatch):
    """Logging on versus off must not change anything but the log text."""
    monkeypatch.delenv("ATLAS_LOG_FORMAT", raising=False)
    import main
    buf = io.StringIO()
    main._PrivateValueStream(buf).write("plain line\n")
    assert buf.getvalue() == "plain line\n"


def test_logs_carry_no_secrets_prompts_or_candidate_bytes():
    """The record schema is fixed and small: nothing in it is a place for a
    prompt, a candidate, or a token to land."""
    import logging
    SL.bind_identity("req-A", "inv-A")
    out = json.loads(SL.JsonFormatter("v3-service").format(
        logging.LogRecord("t", logging.INFO, __file__, 1, "msg", None, None)))
    assert set(out) <= {"ts", "level", "service", "logger", "msg",
                        "request_id", "invocation_id", "exc"}


def test_identity_binding_does_not_touch_generation_inputs():
    """Observability-only: binding an identity changes no adapter state that
    could reach a prompt, a candidate, or a selection."""
    import adapters
    llm = adapters.LLMAdapter()
    llm.request_identity = adapters.RequestIdentity("req-A", "inv-A")
    before = (llm.thinking, llm.deadline, llm.call_count, llm.total_tokens)
    SL.bind_identity("req-A", "inv-A")
    after = (llm.thinking, llm.deadline, llm.call_count, llm.total_tokens)
    assert before == after
    # And the wire identity is the adapter's, never the ContextVar's.
    SL.bind_identity("req-DIFFERENT", "inv-DIFFERENT")
    assert llm._inference_headers()[adapters.REQUEST_ID_HEADER] == "req-A"
    assert llm._inference_headers()[adapters.INVOCATION_ID_HEADER] == "inv-A"
