"""Per-candidate cost attribution, repair lineage and exact reconciliation.

The V3 capture already retained candidate bytes, hashes, oracle results and
lens energy, but cost was aggregated per run. A canary that has to separate
"V3 generated a correct candidate" from "the selector chose it" cannot price
either question without knowing what each candidate cost, and a repaired
artifact that does not name its parent looks like a fresh generation.

Attribution is keyed on the candidate's own bytes, which is what makes it safe
under parallel generation: a call site owns the code it just produced and can
never be charged for a sibling's tokens.
"""
import json
import os
import sys
import threading

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "..", "v3-service"))
import pipeline  # noqa: E402


def open_capture(tmp_path, monkeypatch):
    path = tmp_path / "pool.jsonl"
    monkeypatch.setenv(pipeline.CAPTURE_ENV, str(path))
    cap = pipeline._PoolCapture.from_env()
    cap.bind("sess-1")
    assert cap.enabled, cap.write_error
    return cap, path


def records(path):
    return [json.loads(l) for l in open(path) if l.strip()]


def test_disabled_capture_costs_nothing(tmp_path, monkeypatch):
    monkeypatch.delenv(pipeline.CAPTURE_ENV, raising=False)
    cap = pipeline._PoolCapture.from_env()
    assert not cap.enabled
    cap.note_cost(code="x", phase="p", tokens=5, latency_ms=1.0)
    cap.note_candidate(role="generated", index=0, code="x", accepted=True,
                       record=None, phase="p")
    cap.close({"total_tokens": 5})
    assert cap.records_written == 0


def test_candidate_id_is_content_derived_and_stable(tmp_path, monkeypatch):
    cap, _ = open_capture(tmp_path, monkeypatch)
    a = cap.candidate_id("print('a')\n")
    b = cap.candidate_id("print('a')\n")
    c = cap.candidate_id("print('b')\n")
    assert a == b and a != c and len(a) == 12
    assert cap.candidate_id("") == ""
    cap.close(None)


def test_per_candidate_attribution(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    cap.note_cost(code="A\n", phase="divsampling", tokens=100, latency_ms=10.0)
    cap.note_cost(code="B\n", phase="divsampling", tokens=250, latency_ms=25.0)
    cap.note_candidate(role="generated", index=0, code="A\n", accepted=True,
                       record=None, phase="divsampling")
    cap.note_candidate(role="generated", index=1, code="B\n", accepted=False,
                       record=None, phase="divsampling")
    cap.close({"total_tokens": 350, "code": ""})
    by_id = {r["candidate_id"]: r for r in records(path)
             if r["type"] == "candidate_evaluation"}
    assert by_id[cap.candidate_id("A\n")]["cost"]["tokens"] == 100
    assert by_id[cap.candidate_id("B\n")]["cost"]["tokens"] == 250
    assert by_id[cap.candidate_id("A\n")]["cost"]["model_calls"] == 1


def test_shared_overhead_is_named_not_divided(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    cap.note_cost(code=None, phase="self_test_gen", tokens=80, latency_ms=8.0)
    cap.note_cost(code=None, phase="plansearch", tokens=400, latency_ms=40.0)
    cap.note_cost(code="A\n", phase="divsampling", tokens=100, latency_ms=10.0)
    cap.note_candidate(role="generated", index=0, code="A\n", accepted=True,
                       record=None, phase="divsampling")
    cap.close({"total_tokens": 580})
    rec = [r for r in records(path) if r["type"] == "cost_reconciliation"][0]
    assert rec["shared_overhead"]["self_test_gen"]["tokens"] == 80
    assert rec["shared_overhead"]["plansearch"]["tokens"] == 400
    assert rec["candidate_tokens"] == 100
    # The shared cost is not smeared across candidates.
    cand = [r for r in records(path) if r["type"] == "candidate_evaluation"][0]
    assert cand["cost"]["tokens"] == 100


def test_reconciliation_is_exact_and_names_the_remainder(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    cap.note_cost(code="A\n", phase="divsampling", tokens=100, latency_ms=1.0)
    cap.note_cost(code=None, phase="probe", tokens=50, latency_ms=1.0)
    cap.close({"total_tokens": 150})
    rec = [r for r in records(path) if r["type"] == "cost_reconciliation"][0]
    assert rec["attributed_tokens"] == 150
    assert rec["unattributed_tokens"] == 0
    assert rec["reconciles"] is True


def test_reconciliation_reports_an_unattributed_remainder(tmp_path, monkeypatch):
    d = tmp_path / "sub"
    d.mkdir()
    cap, path = open_capture(d, monkeypatch)
    cap.note_cost(code="A\n", phase="divsampling", tokens=100, latency_ms=1.0)
    cap.close({"total_tokens": 175})
    rec = [r for r in records(path) if r["type"] == "cost_reconciliation"][0]
    assert rec["unattributed_tokens"] == 75
    assert rec["reconciles"] is False


def test_repair_names_its_parent(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    parent = "def f(:\n"
    child = "def f():\n    return 1\n"
    cap.note_cost(code=parent, phase="divsampling", tokens=100, latency_ms=1.0)
    cap.note_candidate(role="generated", index=0, code=parent, accepted=False,
                       record=None, phase="divsampling")
    cap.note_cost(code=child, phase="repair_pr_cot", tokens=60, latency_ms=6.0,
                  parent_code=parent)
    cap.note_candidate(role="repair", index=None, code=child, accepted=True,
                       record=None, phase="repair_pr_cot")
    cap.close({"total_tokens": 160, "code": child, "passed": True})
    recs = {r["role"]: r for r in records(path) if r["type"] == "candidate_evaluation"}
    assert recs["repair"]["parent_id"] == cap.candidate_id(parent)
    assert recs["generated"]["parent_id"] == ""
    assert recs["repair"]["candidate_id"] == cap.candidate_id(child)


def test_parallel_attribution_never_crosses_candidates(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    codes = [f"print({i})\n" for i in range(24)]

    def worker(i):
        cap.note_cost(code=codes[i], phase="divsampling",
                      tokens=(i + 1) * 10, latency_ms=float(i))

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(24)]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    for i, c in enumerate(codes):
        cap.note_candidate(role=f"generated-{i}", index=i, code=c,
                           accepted=True, record=None, phase="divsampling")
    total = sum((i + 1) * 10 for i in range(24))
    cap.close({"total_tokens": total})
    by_id = {r["candidate_id"]: r for r in records(path)
             if r["type"] == "candidate_evaluation"}
    for i, c in enumerate(codes):
        assert by_id[cap.candidate_id(c)]["cost"]["tokens"] == (i + 1) * 10
    rec = [r for r in records(path) if r["type"] == "cost_reconciliation"][0]
    assert rec["reconciles"] is True


def test_bytes_and_hash_identity_unchanged(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    import base64
    import hashlib
    code = "print('x')\n"
    cap.note_candidate(role="generated", index=0, code=code, accepted=True,
                       record=None, phase="divsampling")
    cap.close({"total_tokens": 0})
    r = [x for x in records(path) if x["type"] == "candidate_evaluation"][0]
    assert base64.b64decode(r["code_b64"]).decode() == code
    assert r["code_sha256"] == hashlib.sha256(code.encode()).hexdigest()
    assert r["code_bytes"] == len(code.encode())
    assert r["candidate_id"] == r["code_sha256"][:12]


def test_malformed_cost_is_refused_not_guessed(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    cap.note_cost(code="A\n", phase="p", tokens=None, latency_ms=None)
    cap.note_cost(code="A\n", phase="p", tokens="abc", latency_ms="x")
    cap.note_cost(code="A\n", phase="p", tokens=10, latency_ms=1.0)
    cap.close({"total_tokens": 10})
    rec = [r for r in records(path) if r["type"] == "cost_reconciliation"][0]
    assert rec["candidate_tokens"] == 10
    assert rec["reconciles"] is True


def test_capture_shutdown_writes_status_and_releases(tmp_path, monkeypatch):
    cap, path = open_capture(tmp_path, monkeypatch)
    cap.note_cost(code="A\n", phase="p", tokens=1, latency_ms=1.0)
    cap.close({"total_tokens": 1})
    assert not cap.enabled
    kinds = [r["type"] for r in records(path)]
    assert "cost_reconciliation" in kinds
    assert kinds[-1] == "capture_status"
    # A post-close write is refused rather than reopening the sink.
    cap.note_cost(code="B\n", phase="p", tokens=99, latency_ms=1.0)
    assert len([r for r in records(path) if r["type"] == "cost_reconciliation"]) == 1
