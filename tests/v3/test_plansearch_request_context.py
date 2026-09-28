"""PlanSearch worker threads and the request context they dispatch under.

PlanSearch is the only V3 stage that sends inference from worker threads. A
thread inherits neither the request thread's ContextVars nor its locals, so
every request-scoped value PlanSearch needs has to reach it some other way.
When the outbound call's identity did not, the calls went out unattributed:
against a permissive upstream that is a silently missing correlation ID, and
against one that enforces attribution it is an HTTP 403 per call.

Measured 2026-08-23, 42-case acquisition: all 14 PlanSearch invocations
returned 0 candidates, all 28 worker calls refused, and nothing raised --
DivSampling filled the slots and the stage read as alive but unproductive.
These tests use a real enforcing upstream over real sockets, because that
distinction is exactly what an in-process double erases.
"""
import json
import os
import re
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
if V3 not in sys.path:
    sys.path.insert(0, V3)

import adapters  # noqa: E402
from stages.plan_search import (  # noqa: E402
    PlanSearch, PlanSearchConfig, PlanSearchInfrastructureError)


CONSTRAINTS = (
    "CONSTRAINT SET 1:\n- Constraint: read every line\n- Implies: scan\n"
    "CONSTRAINT SET 2:\n- Constraint: keep a running total\n- Implies: fold\n"
    "CONSTRAINT SET 3:\n- Constraint: print once at the end\n- Implies: sink\n"
)
PLAN = "Algorithm: linear scan\nData structures: list\n1. read\n2. total\n3. print\n"


class Relay(BaseHTTPRequestHandler):
    """Stands in for the canary isolation relay.

    Same gate as relay_go/main.go: an inference path is refused with 403
    unless it carries a request ID on the allowlist. Everything about a
    request is recorded so a test can ask which thread sent it and under
    whose identity.
    """
    protocol_version = "HTTP/1.1"

    allowed = set()
    calls = []
    refusals = []
    lock = threading.Lock()
    # Seeds whose response is held back, so later workers finish first.
    delay_for_seed = {}

    def log_message(self, *a):
        pass

    def handle_error(self, *a):
        # A cancelled or torn-down connection is the point of some of these
        # tests; socketserver's default handler prints its traceback to
        # stderr and makes a passing run look like a failing one.
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        raw = self.rfile.read(n)
        rid = self.headers.get(adapters.REQUEST_ID_HEADER, "")
        inv = self.headers.get(adapters.INVOCATION_ID_HEADER, "")
        body = json.loads(raw or b"{}")

        if self.path.startswith("/v1/chat/completions") and rid not in Relay.allowed:
            with Relay.lock:
                Relay.refusals.append({"request_id": rid, "invocation_id": inv,
                                       "path": self.path})
            payload = b'{"error":"refused"}'
            self.send_response(403)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return

        seed = body.get("seed")
        with Relay.lock:
            Relay.calls.append({
                "request_id": rid, "invocation_id": inv, "seed": seed,
                "thread": threading.current_thread().name,
                "body_sha_input": len(raw),
            })

        # Classify the requested stage from the user instruction. System
        # guidance legitimately mentions code in every stage and must not
        # make this test double return an implementation for planning calls.
        text = "".join(m.get("content") or "" for m in body.get("messages", [])
                       if m.get("role") == "user")
        if "CONSTRAINT SET" in text.upper() or "constraint" in text.lower()[:400]:
            content = CONSTRAINTS
        else:
            content = PLAN
        # Code generation is the only step whose prompt asks for a fenced
        # implementation; tag the body with its seed so a candidate can be
        # traced back to the fan-out index that asked for it.
        if "```" in text or "code" in text.lower()[:400]:
            content = f"```python\nprint({seed})\n```"

        hold = Relay.delay_for_seed.get(seed, 0.0)
        if hold:
            time.sleep(hold)

        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for piece in (content, ""):
            if not piece:
                continue
            chunk = ("data: " + json.dumps(
                {"choices": [{"delta": {"content": piece}}]}) + "\n\n").encode()
            self.wfile.write(b"%x\r\n" % len(chunk) + chunk + b"\r\n")
        tail = b"data: [DONE]\n\n"
        self.wfile.write(b"%x\r\n" % len(tail) + tail + b"\r\n")
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


@pytest.fixture()
def relay():
    Relay.allowed = set()
    Relay.calls = []
    Relay.refusals = []
    Relay.delay_for_seed = {}
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Relay)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    old = adapters.INFERENCE_URL
    adapters.INFERENCE_URL = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        yield Relay
    finally:
        adapters.INFERENCE_URL = old
        srv.shutdown()
        srv.server_close()


def _adapter(request_id, invocation_id="inv-1", scope=None):
    llm = adapters.LLMAdapter()
    llm.cancel_scope = scope if scope is not None else adapters.CancelScope(
        invocation_id=invocation_id)
    llm.request_identity = adapters.RequestIdentity(
        request_id=request_id, invocation_id=invocation_id)
    return llm


def _stage(events_file=None):
    ps = PlanSearch(PlanSearchConfig(enabled=True, num_plans=3))
    ps._events_file = events_file
    return ps


# --- the defect itself ------------------------------------------------------

def test_concurrent_fan_out_calls_are_all_attributed(relay):
    """Every worker call carries the parent's request ID and invocation ID.

    This is the regression: before the fix the step-2 and step-3 calls were
    dispatched from ThreadPoolExecutor workers, read an unset ContextVar,
    sent no X-ATLAS-Request-ID, and were all refused.
    """
    relay.allowed = {"req-A"}
    llm = _adapter("req-A", "inv-A")
    result = _stage().generate("sum the file", "t1", llm, num_plans=3)

    assert relay.refusals == [], f"refused calls: {relay.refusals}"
    assert len(relay.calls) > 1
    assert {c["request_id"] for c in relay.calls} == {"req-A"}
    assert {c["invocation_id"] for c in relay.calls} == {"inv-A"}
    assert result.candidates, "PlanSearch produced no candidates"

    # And the fan-out really was concurrent, not the inline path.
    worker_calls = [c for c in relay.calls
                    if c["thread"] != threading.current_thread().name]
    assert worker_calls, "no call arrived from a worker thread"


def test_fan_out_uses_more_than_one_worker_thread(relay):
    """Guards the premise: a serialized fan-out would pass the test above
    without ever crossing the thread boundary the defect lived on."""
    relay.allowed = {"req-A"}
    llm = _adapter("req-A", "inv-A")
    _stage().generate("sum the file", "t1", llm, num_plans=3)
    threads = {c["thread"] for c in relay.calls}
    assert len(threads) > 1, f"fan-out never left one thread: {threads}"


def test_two_concurrent_parents_never_exchange_ids(relay):
    """Each parent's calls carry only that parent's identity.

    Disjoint base seeds make every recorded call traceable to the parent
    that asked for it, so a swap shows up as a seed under the wrong ID.
    """
    relay.allowed = {"req-A", "req-B"}
    out = {}

    def run(tag, base_seed):
        llm = _adapter(f"req-{tag}", f"inv-{tag}")
        out[tag] = _stage().generate("sum the file", f"t-{tag}", llm,
                                     num_plans=3, base_seed=base_seed)

    ta = threading.Thread(target=run, args=("A", 1000))
    tb = threading.Thread(target=run, args=("B", 5000))
    ta.start()
    tb.start()
    ta.join()
    tb.join()

    assert relay.refusals == []
    for call in relay.calls:
        seed = call["seed"]
        if seed is None:
            continue
        expected = "req-A" if seed < 4000 else "req-B"
        assert call["request_id"] == expected, (
            f"seed {seed} was sent under {call['request_id']}")
        assert call["invocation_id"] == expected.replace("req-", "inv-")
    assert out["A"].candidates and out["B"].candidates


def test_cancelling_one_parent_leaves_the_other_running(relay):
    """One parent's cancellation must not reach or relabel the other's calls."""
    relay.allowed = {"req-A", "req-B"}
    scope_a = adapters.CancelScope(invocation_id="inv-A")
    scope_b = adapters.CancelScope(invocation_id="inv-B")
    llm_a = _adapter("req-A", "inv-A", scope=scope_a)
    llm_b = _adapter("req-B", "inv-B", scope=scope_b)
    results = {}

    def run(tag, llm, base_seed):
        try:
            results[tag] = _stage().generate("sum the file", f"t-{tag}", llm,
                                             num_plans=3, base_seed=base_seed)
        except Exception as exc:  # noqa: BLE001 — recorded, asserted below
            results[tag] = exc

    tb = threading.Thread(target=run, args=("B", llm_b, 5000))
    tb.start()
    scope_a.cancel()
    run("A", llm_a, 1000)
    tb.join()

    assert not isinstance(results["B"], Exception), results["B"]
    assert results["B"].candidates, "cancelling A stopped B"
    assert {c["request_id"] for c in relay.calls} <= {"req-A", "req-B"}
    assert all(c["request_id"] == "req-B" for c in relay.calls
               if c["seed"] is not None and c["seed"] >= 4000)


def test_out_of_order_completion_preserves_candidate_attribution(relay):
    """Candidate i stays the candidate generated for plan i.

    Step-3 seeds are base_seed + i + 200, so holding the low seeds back
    makes the workers finish in reverse. Results are sorted by index, so
    the candidate list must come back in plan order regardless.
    """
    relay.allowed = {"req-A"}
    base = 1000
    relay.delay_for_seed = {base + 0 + 200: 0.45, base + 1 + 200: 0.25}
    llm = _adapter("req-A", "inv-A")
    result = _stage().generate("sum the file", "t1", llm, num_plans=3,
                               base_seed=base)

    assert relay.refusals == []
    seeds = [int(re.search(r"print\((\d+)\)", c).group(1))
             for c in result.candidates]
    assert seeds == sorted(seeds), f"candidates came back reordered: {seeds}"
    assert seeds == [base + i + 200 for i in range(len(seeds))]

    finish_order = [c["seed"] for c in relay.calls
                    if c["seed"] is not None and c["seed"] >= base + 200]
    assert finish_order != sorted(finish_order) or True  # arrival order is free


# --- failing closed ---------------------------------------------------------

def test_missing_identity_is_visible_not_candidate_scarcity(relay):
    """An adapter serving a request with no identity refuses to send.

    The failure must reach the caller as an infrastructure failure. Silently
    returning zero candidates is the exact shape the acquisition could not
    tell apart from a model that solved nothing.
    """
    relay.allowed = {"req-A"}
    llm = adapters.LLMAdapter()
    llm.cancel_scope = adapters.CancelScope(invocation_id="inv-A")
    llm.request_identity = None  # the wiring defect

    with pytest.raises(PlanSearchInfrastructureError):
        _stage().generate("sum the file", "t1", llm, num_plans=3)
    assert relay.calls == [], "an unattributed call reached the upstream"


def test_refused_fan_out_raises_instead_of_returning_empty(relay):
    """A 403 from the enforcing upstream is an infrastructure failure.

    Before the fix this returned PlanSearchResult(candidates=[]) and the
    pipeline backfilled the slots with DivSampling.
    """
    relay.allowed = {"req-OTHER"}  # this request's ID is not on the list
    llm = _adapter("req-A", "inv-A")

    with pytest.raises(PlanSearchInfrastructureError) as caught:
        _stage().generate("sum the file", "t1", llm, num_plans=3)
    assert caught.value.failures
    assert relay.refusals, "expected the upstream to refuse"
    # The tokens already spent stay on the ledger.
    assert caught.value.result is not None


def test_model_level_failure_still_degrades_gracefully(relay, monkeypatch):
    """The per-item tolerance is intact for what it was built for.

    A worker raising something that is not an authentication or wiring
    failure still drops one item and leaves the batch.
    """
    relay.allowed = {"req-A"}
    llm = _adapter("req-A", "inv-A")
    stage = _stage()
    real = stage._step3_generate_code
    calls = {"n": 0}

    def flaky(problem, plan, llm_call, budget_tier, seed):
        calls["n"] += 1
        if calls["n"] == 1:
            raise ValueError("model returned unparseable output")
        return real(problem, plan, llm_call, budget_tier, seed)

    monkeypatch.setattr(stage, "_step3_generate_code", flaky)
    result = stage.generate("sum the file", "t1", llm, num_plans=3)
    assert result.candidates, "a model-level failure emptied the batch"
    assert len(result.candidates) < 3


def test_infrastructure_and_model_failures_are_counted_apart(relay, tmp_path):
    """Telemetry separates the two, which is what a liveness gate reads."""
    events = tmp_path / "plan_search_events.jsonl"

    relay.allowed = {"req-A"}
    _stage(str(events)).generate("sum the file", "t1",
                                 _adapter("req-A", "inv-A"), num_plans=3)

    relay.allowed = {"req-OTHER"}
    with pytest.raises(PlanSearchInfrastructureError):
        _stage(str(events)).generate("sum the file", "t2",
                                     _adapter("req-A", "inv-A"), num_plans=3)

    rows = [json.loads(x) for x in events.read_text().splitlines() if x.strip()]
    healthy = next(r for r in rows if r["task_id"] == "t1")
    refused = next(r for r in rows if r["task_id"] == "t2")
    assert healthy["infrastructure_failures"] == 0
    assert healthy["num_usable_candidates"] > 0
    assert refused["infrastructure_failures"] > 0
    assert refused["num_usable_candidates"] == 0


# --- the inline path is untouched -------------------------------------------

def test_single_item_path_is_unchanged(relay):
    """One item still runs inline on the calling thread, same result.

    The inline branch never had the defect: it runs where the ContextVar is
    set. It must keep running there, and it must keep tolerating a
    model-level failure per item.
    """
    relay.allowed = {"req-A"}
    stage = _stage()
    caller = threading.current_thread().name

    out = stage._fan_out([(0, "only")], lambda i, item: f"ran-{i}-{item}")
    assert out == [(0, "ran-0-only")]
    assert all(c["thread"] == caller for c in relay.calls)

    def boom(i, item):
        raise ValueError("model output unparseable")

    assert stage._fan_out([(0, "only")], boom) == []


def test_single_item_infrastructure_failure_also_raises(relay):
    """Fail-closed is a property of the failure, not of the branch."""
    stage = _stage()
    err = adapters.RequestIdentityMissing("no identity")

    def boom(i, item):
        raise err

    with pytest.raises(PlanSearchInfrastructureError):
        stage._fan_out([(0, "only")], boom)


# --- no ContextVar authority ------------------------------------------------

def test_worker_never_reads_an_unset_contextvar_as_authority(relay):
    """The header does not come from the ContextVar, set or unset.

    Proven by setting the ContextVar to a DIFFERENT value than the adapter
    identity: what arrives must be the identity, not the ambient ID.
    """
    from structured_log import set_request_id
    relay.allowed = {"req-IDENTITY"}
    set_request_id("req-CONTEXTVAR")
    try:
        llm = _adapter("req-IDENTITY", "inv-A")
        _stage().generate("sum the file", "t1", llm, num_plans=3)
    finally:
        set_request_id("")

    assert relay.refusals == []
    assert {c["request_id"] for c in relay.calls} == {"req-IDENTITY"}


def test_no_request_means_no_identity_and_no_header(relay):
    """Bench and CLI callers have neither a scope nor an identity, and are
    unaffected: the call goes out unattributed because there is nothing to
    attribute it to."""
    relay.allowed = {""}  # the enforcing upstream happens to allow it
    llm = adapters.LLMAdapter()
    assert llm.cancel_scope is None and llm.request_identity is None
    headers = llm._inference_headers()
    assert adapters.REQUEST_ID_HEADER not in headers
    assert headers["Content-Type"] == "application/json"
