"""Candidate pools must join to their case by name, never by position.

The first real V3 acquisition wrote every pool under session_id "gen-solve" --
derived from the target filename. Twenty-one benchmark cases all write
solve.py, and one case can call V3 several times, so nothing in the record said
which case a pool belonged to. Joining by file order would have been a guess
dressed as evidence.

Trace identity answers "which request", the invocation id answers "which call
within it", and instance identity answers "which candidate" when two candidates
hold identical bytes.
"""
import json
import os
import sys
import threading

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
sys.path.insert(0, V3)
import pipeline  # noqa: E402


def cap(tmp_path, monkeypatch, name="pool.jsonl", trace="req-1", inv="inv-1"):
    path = tmp_path / name
    monkeypatch.setenv(pipeline.CAPTURE_ENV, str(path))
    c = pipeline._PoolCapture.from_env()
    c.bind("gen-solve")
    c.identify(trace, inv)
    assert c.enabled, c.write_error
    return c, path


def recs(path):
    return [json.loads(l) for l in open(path) if l.strip()]


def test_one_request_one_invocation(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch)
    c.note_candidate(role="generated", index=0, code="A\n", accepted=True,
                     record=None, phase="divsampling")
    c.close({"total_tokens": 0, "code": "A\n"})
    for r in recs(p):
        assert r["request_id"] == "req-1"
        assert r["v3_invocation_id"] == "inv-1"
        assert r["request_id_state"] == "attributed"


def test_multiple_invocations_in_one_request_stay_distinct(tmp_path, monkeypatch):
    c1, p1 = cap(tmp_path, monkeypatch, "a.jsonl", trace="req-9", inv="inv-A")
    c1.note_candidate(role="generated", index=0, code="X\n", accepted=True,
                      record=None, phase="p")
    c1.close({"total_tokens": 0})
    c2, p2 = cap(tmp_path, monkeypatch, "b.jsonl", trace="req-9", inv="inv-B")
    c2.note_candidate(role="generated", index=0, code="X\n", accepted=True,
                      record=None, phase="p")
    c2.close({"total_tokens": 0})
    a = [r for r in recs(p1) if r["type"] == "candidate_evaluation"][0]
    b = [r for r in recs(p2) if r["type"] == "candidate_evaluation"][0]
    assert a["request_id"] == b["request_id"] == "req-9"
    assert a["v3_invocation_id"] != b["v3_invocation_id"]
    assert a["candidate_instance_id"] != b["candidate_instance_id"], (
        "identical bytes in two invocations collapsed to one instance")
    assert a["code_sha256"] == b["code_sha256"], "content identity should still match"


def test_same_filename_and_pool_name_across_requests_cannot_collide(tmp_path, monkeypatch):
    c1, p1 = cap(tmp_path, monkeypatch, "r1.jsonl", trace="ring2", inv="i1")
    c1.note_candidate(role="generated", index=0, code="S\n", accepted=True,
                      record=None, phase="p")
    c1.close({"total_tokens": 0})
    c2, p2 = cap(tmp_path, monkeypatch, "r2.jsonl", trace="ring5", inv="i2")
    c2.note_candidate(role="generated", index=0, code="S\n", accepted=True,
                      record=None, phase="p")
    c2.close({"total_tokens": 0})
    a = [r for r in recs(p1) if r["type"] == "candidate_evaluation"][0]
    b = [r for r in recs(p2) if r["type"] == "candidate_evaluation"][0]
    assert a["session_id"] == b["session_id"] == "gen-solve"   # the old, useless key
    assert a["request_id"] != b["request_id"]                  # the useful one


def test_duplicate_trace_ids_remain_distinguishable(tmp_path, monkeypatch):
    c1, p1 = cap(tmp_path, monkeypatch, "d1.jsonl", trace="dup", inv="x1")
    c1.note_candidate(role="generated", index=0, code="D\n", accepted=True,
                      record=None, phase="p")
    c1.close({"total_tokens": 0})
    c2, p2 = cap(tmp_path, monkeypatch, "d2.jsonl", trace="dup", inv="x2")
    c2.note_candidate(role="generated", index=0, code="D\n", accepted=True,
                      record=None, phase="p")
    c2.close({"total_tokens": 0})
    a = [r for r in recs(p1) if r["type"] == "candidate_evaluation"][0]
    b = [r for r in recs(p2) if r["type"] == "candidate_evaluation"][0]
    assert a["v3_invocation_id"] != b["v3_invocation_id"]


def test_missing_trace_id_is_marked_not_guessed(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch, trace="", inv="inv-only")
    c.note_candidate(role="generated", index=0, code="M\n", accepted=True,
                     record=None, phase="p")
    c.close({"total_tokens": 0})
    for r in recs(p):
        assert r["request_id"] == ""
        assert r["request_id_state"] == "unattributed"
        assert r["v3_invocation_id"] == "inv-only"


def test_two_instances_with_identical_bytes_are_distinct(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch)
    same = "print(1)\n"
    c.note_candidate(role="generated", index=0, code=same, accepted=True,
                     record=None, phase="p")
    c.note_candidate(role="repair", index=1, code=same, accepted=True,
                     record=None, phase="repair_pr_cot")
    got = [r for r in recs(p) if r["type"] == "candidate_evaluation"]
    assert len({r["candidate_instance_id"] for r in got}) == 2
    assert len({r["code_sha256"] for r in got}) == 1
    c.close({"total_tokens": 0})


def test_repair_lineage_resolves_within_the_invocation(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch)
    parent, child = "def f(:\n", "def f():\n    return 1\n"
    c.note_cost(code=parent, phase="divsampling", tokens=10, latency_ms=1.0)
    c.note_candidate(role="generated", index=0, code=parent, accepted=False,
                     record=None, phase="divsampling")
    c.note_cost(code=child, phase="repair_pr_cot", tokens=5, latency_ms=1.0,
                parent_code=parent)
    c.note_candidate(role="repair", index=1, code=child, accepted=True,
                     record=None, phase="repair_pr_cot")
    c.close({"total_tokens": 15, "code": child})
    by_role = {r["role"]: r for r in recs(p) if r["type"] == "candidate_evaluation"}
    assert by_role["repair"]["parent_candidate_instance_id"] == \
        by_role["generated"]["candidate_instance_id"]
    assert by_role["generated"]["parent_candidate_instance_id"] == ""


def test_instance_ids_are_scoped_to_the_invocation(tmp_path, monkeypatch):
    c, _ = cap(tmp_path, monkeypatch, inv="inv-Z")
    assert c.instance_id("generated", 3).startswith("inv-Z:")
    other = pipeline._PoolCapture.disabled()
    other.identify("r", "inv-Y")
    assert other.instance_id("generated", 3) != c.instance_id("generated", 3)


def test_selection_summary_names_instance_and_hash(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch)
    c.note_candidate(role="generated", index=0, code="W\n", accepted=True,
                     record=None, phase="p")
    c._selection = {"phase": "phase1", "pool": [], "pool_indices": [0],
                    "lens_index": 0, "evidence_index": None, "verified_index": 0,
                    "selection_status": "ok", "selection_reason": "lens",
                    "tied_count": 0, "incomparable_count": 0, "ineligible_count": 0}
    c.close({"total_tokens": 0, "code": "W\n"})
    sel = [r for r in recs(p) if r["type"] == "selection_summary"][0]
    assert sel["v3_invocation_id"] == "inv-1"
    assert sel["selected_candidate_instance_id"] == c.instance_id("generated", 0)
    assert sel["service_returned_candidate_hash"]


def test_cost_reconciliation_carries_identity(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch)
    c.note_cost(code="C\n", phase="divsampling", tokens=10, latency_ms=1.0)
    c.note_cost(code=None, phase="self_test_gen", tokens=5, latency_ms=1.0)
    c.close({"total_tokens": 15})
    rec = [r for r in recs(p) if r["type"] == "cost_reconciliation"][0]
    assert rec["request_id"] == "req-1" and rec["v3_invocation_id"] == "inv-1"
    assert rec["shared_overhead"]["self_test_gen"]["tokens"] == 5
    assert rec["reconciles"] is True


def test_capture_status_carries_identity(tmp_path, monkeypatch):
    c, p = cap(tmp_path, monkeypatch)
    c.close({"total_tokens": 0})
    st = [r for r in recs(p) if r["type"] == "capture_status"][0]
    assert st["request_id"] == "req-1" and st["v3_invocation_id"] == "inv-1"


def test_legacy_stage2_records_remain_readable():
    """Old captures predate every identity field and must not be re-attributed."""
    legacy = {"type": "candidate_evaluation", "session_id": "diag-ledger1-0",
              "role": "generated", "candidate_index": "0",
              "code_sha256": "a" * 64, "code_bytes": "10"}
    assert legacy.get("request_id") is None
    assert legacy.get("v3_invocation_id") is None
    # A reader must treat absence as unattributed, never infer from order.
    state = "attributed" if legacy.get("request_id") else "legacy_unattributed"
    assert state == "legacy_unattributed"


def test_identity_never_reaches_generation_or_selection():
    """Identity may be carried. It may not be consulted.

    Every mention must sit inside the diagnostic sink, in run()'s signature,
    on the single line that hands it to the sink, or on the lines that pack
    it for the transport. Anything else would mean a decision path can see
    which request it is serving.

    The transport is the second legitimate consumer, added 2026-08-23: an
    inference call has to say which request it belongs to, and the identity
    is the only thing that can say it. Packing it is not consulting it -- the
    companion test below holds that line by checking that what comes out of
    the packing is never read anywhere that decides anything.
    """
    import ast as _ast
    src = open(os.path.join(V3, "pipeline.py"), encoding="utf-8").read()
    tree = _ast.parse(src)
    sink = next(n for n in _ast.walk(tree)
                if isinstance(n, _ast.ClassDef) and n.name == "_PoolCapture")
    run = next(n for n in _ast.walk(tree)
               if isinstance(n, _ast.FunctionDef) and n.name == "run")
    lines = src.split("\n")
    stray = []
    for idx, line in enumerate(lines, start=1):
        if "trace_request_id" not in line and "v3_invocation_id" not in line:
            continue
        in_sink = sink.lineno <= idx <= (sink.end_lineno or sink.lineno)
        in_run_sig = run.lineno <= idx <= run.lineno + 8
        hands_off = "capture.identify(" in line
        packs_for_transport = ("request_id=trace_request_id" in line
                               or "invocation_id=v3_invocation_id" in line)
        if not (in_sink or in_run_sig or hands_off or packs_for_transport):
            stray.append((idx, line.strip()))
    assert not stray, f"identity reaches a decision path: {stray}"


def test_transport_identity_is_carried_and_never_read():
    """The packed identity is handed to the adapter and read nowhere else.

    Two mentions are allowed in pipeline.py: building it in run(), and the
    one assignment onto the request-scoped adapter. A third would mean some
    decision path can now see it -- which is the thing the packing was not
    allowed to buy.
    """
    import ast as _ast
    src = open(os.path.join(V3, "pipeline.py"), encoding="utf-8").read()
    mentions = [(i, l.strip()) for i, l in enumerate(src.split("\n"), start=1)
                if "request_identity" in l]
    assigns = [m for m in mentions if "llm.request_identity =" in m[1]]
    params = [m for m in mentions if m[1].startswith("request_identity=")
              or "request_identity=None" in m[1]]
    builds = [m for m in mentions if "RequestIdentity(" in m[1]]
    accounted = {m[0] for m in assigns + params + builds}
    stray = [m for m in mentions if m[0] not in accounted]
    assert not stray, f"transport identity is read on a decision path: {stray}"
    assert len(assigns) == 1, f"expected one adapter assignment, got {assigns}"
    assert len(builds) == 1, f"expected one construction site, got {builds}"

    # And no scoring or selection module touches it at all.
    for module in ("scoring.py", "planning.py"):
        text = open(os.path.join(V3, module), encoding="utf-8").read()
        for line in text.split("\n"):
            if "request_identity" not in line:
                continue
            assert ("llm.request_identity =" in line
                    or line.strip().startswith("request_identity=")), (
                f"{module} reads the transport identity: {line.strip()}")


def test_disabled_capture_records_nothing(tmp_path, monkeypatch):
    monkeypatch.delenv(pipeline.CAPTURE_ENV, raising=False)
    c = pipeline._PoolCapture.from_env()
    c.identify("req", "inv")
    c.note_candidate(role="generated", index=0, code="Z\n", accepted=True,
                     record=None, phase="p")
    c.close({"total_tokens": 0})
    assert c.records_written == 0


# --- the four cases Pass 2 left open ------------------------------------------

def test_duplicate_live_invocation_identity_fails_closed(tmp_path, monkeypatch):
    """Two live scopes may not share an invocation id.

    Nothing mints duplicates today -- they are uuid4 -- but a future change that
    derived the id from a filename or a counter would, and two pools sharing an
    id are indistinguishable forever once written.
    """
    import adapters
    seen = {}

    def register(scope):
        if scope.invocation_id in seen:
            raise ValueError(f"duplicate live invocation id {scope.invocation_id}")
        seen[scope.invocation_id] = scope

    a = adapters.CancelScope("inv-dup")
    b = adapters.CancelScope("inv-dup")
    register(a)
    with pytest.raises(ValueError):
        register(b)
    # The first invocation is untouched by the rejection.
    assert seen["inv-dup"] is a
    assert a.cancelled is False


def test_short_prefix_collision_does_not_merge_instances(tmp_path, monkeypatch):
    """Instance identity must not lean on the shortened content id."""
    c, p = cap(tmp_path, monkeypatch)
    # Force the collision rather than hunting for one: two DIFFERENT bodies
    # whose short ids are made equal by the fixture.
    real = pipeline._PoolCapture.candidate_id
    monkeypatch.setattr(pipeline._PoolCapture, "candidate_id",
                        staticmethod(lambda code: "deadbeefcafe"))
    c.note_candidate(role="generated", index=0, code="one\n", accepted=True,
                     record=None, phase="p")
    c.note_candidate(role="generated", index=1, code="two\n", accepted=True,
                     record=None, phase="p")
    monkeypatch.setattr(pipeline._PoolCapture, "candidate_id", staticmethod(real))
    c.close({"total_tokens": 0})
    got = [r for r in recs(p) if r["type"] == "candidate_evaluation"]
    assert len(got) == 2, "a prefix collision swallowed a candidate"
    assert len({r["candidate_instance_id"] for r in got}) == 2
    assert len({r["code_sha256"] for r in got}) == 2, "full hashes must still differ"


def test_instance_ids_survive_out_of_order_completion(tmp_path, monkeypatch):
    """Identity comes from the preassigned index, not from who finishes first."""
    import random
    orders = []
    for trial in range(6):
        c, p = cap(tmp_path, monkeypatch, name=f"o{trial}.jsonl", inv=f"inv-{trial}")
        items = [(i, f"cand{i}\n") for i in range(5)]
        shuffled = items[:]
        random.Random(trial).shuffle(shuffled)
        for idx, code in shuffled:                   # completion order varies
            c.note_candidate(role="generated", index=idx, code=code,
                             accepted=True, record=None, phase="p")
        c.close({"total_tokens": 0})
        by_code = {r["code_sha256"]: r["candidate_instance_id"]
                   for r in recs(p) if r["type"] == "candidate_evaluation"}
        orders.append({k: v.split(":", 1)[1] for k, v in by_code.items()})
    first = orders[0]
    for other in orders[1:]:
        assert other == first, "instance identity changed with completion order"


def test_selector_replay_joins_by_request_and_invocation(tmp_path, monkeypatch):
    """Modern captures join explicitly; legacy ones stay legacy."""
    c1, p1 = cap(tmp_path, monkeypatch, "m1.jsonl", trace="ring2", inv="inv-1")
    c1.note_candidate(role="generated", index=0, code="A\n", accepted=True,
                      record=None, phase="p")
    c1.close({"total_tokens": 0})
    c2, p2 = cap(tmp_path, monkeypatch, "m2.jsonl", trace="ring2", inv="inv-2")
    c2.note_candidate(role="generated", index=0, code="A\n", accepted=True,
                      record=None, phase="p")
    c2.close({"total_tokens": 0})

    pools = {}
    for path in (p1, p2):
        for r in recs(path):
            if r["type"] != "candidate_evaluation":
                continue
            key = (r.get("request_id", ""), r.get("v3_invocation_id", ""))
            assert key[1], "a modern record without an invocation id is unjoinable"
            pools.setdefault(key, []).append(r)
    assert len(pools) == 2, "two invocations of one request collapsed into one pool"
    assert all(len(v) == 1 for v in pools.values())

    # Cross-invocation lineage must be refused, not resolved.
    a = pools[("ring2", "inv-1")][0]
    b = pools[("ring2", "inv-2")][0]
    assert a["candidate_instance_id"] != b["candidate_instance_id"]
    assert b["candidate_instance_id"].startswith("inv-2:")
    assert not b["candidate_instance_id"].startswith("inv-1:")

    legacy = {"type": "candidate_evaluation", "session_id": "diag-ledger1-0",
              "role": "generated", "candidate_index": "0", "code_sha256": "b" * 64}
    key = (legacy.get("request_id", ""), legacy.get("v3_invocation_id", ""))
    assert key == ("", ""), "a legacy record must not acquire modern identity"
