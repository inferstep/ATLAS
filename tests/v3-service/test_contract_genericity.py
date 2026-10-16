"""Executable genericity + policy proofs for the evidence contract.

A synthetic adapter with arbitrary criteria (alpha/beta/gamma) drives the
REAL policy. If a browser or game assumption were baked in, these fail —
which a grep over identifiers could never establish.
"""

import copy
import json
import os
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters as A
import contract as C

CAPS = ["alpha", "beta", "gamma"]
CTX = "ctx-abc123"
SCOPE = "synthetic.txt"

TASK = C.task_contract("synthetic.v1", "1.2.3",
                       [C.requirement("alpha"), C.requirement("beta"),
                        C.requirement("gamma", required=False, weight=0.5)])


def _rec(obs, task=TASK, strength=C.BEHAVIORAL, adapter="synth", ver="0.9.0",
         ctx=CTX, scope=SCOPE, content="c1", **kw):
    return C.build(task, adapter, ver, obs, CAPS, strength,
                   artifact_scope=scope, evaluation_context_hash=ctx,
                   candidate_content_hash=content, **kw)


D = C.observation(C.DEMONSTRATED)
R = C.observation(C.REFUTED)


def _expected(**kw):
    base = {"contract_id": "synthetic.v1", "contract_version": "1.2.3",
            "artifact_scope": SCOPE, "evaluation_context_hash": CTX,
            "calibration_id": "", "adapter_id": "synth", "adapter_version": "0.9.0"}
    base.update(kw)
    return base


# ---- 1. plurality must not choose the rubric ------------------------------

def test_wrong_contract_plurality_cannot_outvote_the_right_one():
    right = _rec({"alpha": D, "beta": D})
    other_task = C.task_contract("other.v1", "9.9.9", [C.requirement("alpha")])
    wrong = [C.build(other_task, "synth", "0.9.0", {"alpha": D}, CAPS, C.BEHAVIORAL,
                     artifact_scope=SCOPE, evaluation_context_hash=CTX,
                     candidate_content_hash=f"w{i}") for i in range(10)]
    res = C.select(wrong + [right], _expected())
    assert res["best_record"] is right, "ten misrouted candidates must not outvote one correct one"
    assert len(res["incomparable"]) == 10


def test_no_matching_record_yields_no_verified_winner():
    other_task = C.task_contract("other.v1", "1.0.0", [C.requirement("alpha")])
    foreign = C.build(other_task, "synth", "0.9.0", {"alpha": D}, CAPS, C.BEHAVIORAL,
                      artifact_scope=SCOPE, evaluation_context_hash=CTX)
    res = C.select([foreign], _expected())
    assert res["best_record"] is None and res["incomparable"] == [foreign]


# ---- 2. comparison identity ----------------------------------------------

def test_empty_identity_fields_never_make_records_comparable():
    a = _rec({"alpha": D}, ctx="", scope="")
    assert not C.comparable(a, a), "empty identity must not compare, even to itself"
    with pytest.raises(C.ContractError):
        C.select([a], _expected(evaluation_context_hash=""))


def test_candidate_hashes_differ_while_context_matches():
    a = _rec({"alpha": D, "beta": D}, content="cand-a")
    b = _rec({"alpha": D}, content="cand-b")
    assert a["candidate_content_hash"] != b["candidate_content_hash"]
    assert C.comparable(a, b), "candidates must stay comparable within one context"


def test_different_adapter_versions_are_incomparable_without_calibration():
    a = _rec({"alpha": D, "beta": D}, ver="0.9.0")
    b = _rec({"alpha": D, "beta": D}, ver="1.0.0")
    assert not C.comparable(a, b)
    ca = _rec({"alpha": D, "beta": D}, ver="0.9.0", calibration_id="cal-1")
    cb = _rec({"alpha": D, "beta": D}, ver="1.0.0", calibration_id="cal-1")
    assert C.comparable(ca, cb), "an explicit shared calibration permits comparison"


# ---- 3. failed executions -------------------------------------------------

def test_a_timed_out_run_is_never_complete_or_a_winner():
    dead = _rec({"alpha": D, "beta": D, "gamma": D}, execution_status=C.EXEC_TIMEOUT)
    assert dead["requirements_complete"] is False
    assert dead["closure_eligible"] is False
    healthy = _rec({"alpha": D})
    res = C.select([dead, healthy], _expected())
    assert res["best_record"] is healthy, "a dead run must not outrank a healthy one"
    assert dead in res["ineligible"]
    assert dead["observations"]["alpha"]["status"] == C.DEMONSTRATED, \
        "partial observations are preserved for diagnostics"


# ---- 4. required coverage vs optional quality -----------------------------

def test_required_coverage_outranks_heavily_weighted_optional_quality():
    task = C.task_contract("synthetic.v1", "1.2.3",
                           [C.requirement("alpha"), C.requirement("beta"),
                            C.requirement("gamma", required=False, weight=9.0)])
    mostly_required = _rec({"alpha": D, "beta": D, "gamma": R}, task=task)
    all_optional = _rec({"alpha": R, "beta": R, "gamma": D}, task=task)
    assert all_optional["overall_quality_score"] > mostly_required["overall_quality_score"]
    assert C.rank_key(mostly_required) > C.rank_key(all_optional)


# ---- 5. closure floor belongs to the contract -----------------------------

def test_syntax_cannot_close_a_behavioural_contract():
    rec = _rec({"alpha": D, "beta": D, "gamma": D}, strength=C.SYNTAX)
    assert rec["requirements_complete"] is True
    assert rec["closure_eligible"] is False


def test_syntax_closes_a_syntax_scoped_contract():
    schema_task = C.task_contract("json_schema.v1", "1.0.0",
                                  [C.requirement("alpha")],
                                  minimum_closure_strength=C.SYNTAX)
    rec = _rec({"alpha": D}, task=schema_task, strength=C.SYNTAX)
    assert rec["closure_eligible"] is True, \
        "a universal behavioural floor is not prompt-agnostic"


def test_an_oracle_floor_rejects_merely_behavioural_evidence():
    algo = C.task_contract("algorithmic.v1", "1.0.0", [C.requirement("alpha")],
                           minimum_closure_strength=C.ORACLE)
    assert _rec({"alpha": D}, task=algo, strength=C.BEHAVIORAL)["closure_eligible"] is False
    assert _rec({"alpha": D}, task=algo, strength=C.ORACLE)["closure_eligible"] is True


# ---- 6. validation + materialisation --------------------------------------

@pytest.mark.parametrize("bad", [
    {"reqs": [C.requirement("a"), C.requirement("a")]},
    {"reqs": [C.requirement("")]},
    {"reqs": [C.requirement("a", weight=float("inf"))]},
    {"reqs": [C.requirement("a", weight=-1)]},
])
def test_malformed_requirements_are_rejected(bad):
    task = C.task_contract("x.v1", "1", bad["reqs"])
    with pytest.raises(C.ContractError):
        C.build(task, "synth", "0.9", {}, CAPS, C.BEHAVIORAL,
                artifact_scope=SCOPE, evaluation_context_hash=CTX)


@pytest.mark.parametrize("obs", [
    {"alpha": {"status": "made_up", "confidence": 1.0}},
    {"alpha": C.observation(C.DEMONSTRATED, confidence=1.5)},
    {"omega": C.observation(C.DEMONSTRATED)},
    {"omega": C.observation(C.REFUTED)},
])
def test_malformed_or_overreaching_observations_are_rejected(obs):
    with pytest.raises(C.ContractError):
        _rec(obs)


def test_unknown_statuses_are_rejected():
    with pytest.raises(C.ContractError):
        _rec({"alpha": D}, execution_status="exploded")
    with pytest.raises(C.ContractError):
        _rec({"alpha": D}, strength="vibes")


def test_derived_observations_are_materialised_for_telemetry():
    task = C.task_contract("synthetic.v1", "1.2.3",
                           [C.requirement("alpha"), C.requirement("delta")])
    rec = _rec({"alpha": D}, task=task)
    back = json.loads(json.dumps(rec))
    assert back["observations"]["delta"]["status"] == C.NOT_APPLICABLE, \
        "telemetry must be able to explain why completion failed"
    assert "delta" in back["missing_required"]


def test_unobserved_survives_serialization():
    task = C.task_contract("synthetic.v1", "1.2.3",
                           [C.requirement("alpha"), C.requirement("beta")])
    rec = json.loads(json.dumps(_rec({"alpha": D}, task=task)))
    assert rec["observations"]["beta"]["status"] == C.UNOBSERVED
    assert rec["schema_version"] == C.SCHEMA_VERSION


# ---- 7. tie-break ---------------------------------------------------------

def test_exact_evidence_ties_reach_the_supplied_tie_break():
    a = _rec({"alpha": D, "beta": D}, content="a")
    b = _rec({"alpha": D, "beta": D}, content="b")
    res = C.select([a, b], _expected(),
                   tie_break=lambda r: -{"a": 1, "b": 0}[r["candidate_content_hash"]])
    assert len(res["tied"]) == 2, "the pipeline must see the tie"
    assert res["best_record"] is b, "the supplied lens tie-break decides, not list order"


def test_without_a_tie_break_selection_is_stable():
    a = _rec({"alpha": D, "beta": D}, content="a")
    b = _rec({"alpha": D, "beta": D}, content="b")
    assert C.select([a, b], _expected())["best_record"] is a


# ---- genericity -----------------------------------------------------------

def test_the_policy_functions_carry_no_domain_vocabulary():
    import ast as _ast
    tree = _ast.parse(Path(C.__file__).read_text())
    for node in _ast.walk(tree):
        if isinstance(node, (_ast.FunctionDef, _ast.AsyncFunctionDef)):
            body = [n for n in node.body
                    if not (isinstance(n, _ast.Expr) and isinstance(n.value, _ast.Constant)
                            and isinstance(n.value.value, str))]
            code = "\n".join(_ast.dump(n) for n in body).lower()
            for word in ("canvas", "snake", "collision", "food", "keydown", "browser"):
                assert word not in code, f"'{word}' leaked into {node.name}()"


# ---- all-failed pools -----------------------------------------------------

def _dead(status, content="d"):
    return _rec({"alpha": D, "beta": D, "gamma": D},
                execution_status=status, content=content)


@pytest.mark.parametrize("status", [C.EXEC_TIMEOUT, C.EXEC_CRASH, C.EXEC_ERROR])
def test_an_all_failed_pool_has_no_winner(status):
    """Ranking alone let the best corpse win when every record had died."""
    res = C.select([_dead(status, "a"), _dead(status, "b")], _expected())
    assert res["best_record"] is None
    assert res["verified_winner"] is None
    assert len(res["ineligible"]) == 2
    assert "failed to execute" in res["selection_reason"]


def test_unsupported_plus_timeout_yields_no_winner():
    unsup = _rec({"alpha": D}, supported=False, content="u")
    res = C.select([unsup, _dead(C.EXEC_TIMEOUT, "t")], _expected())
    assert res["best_record"] is None and len(res["ineligible"]) == 2


def test_a_healthy_partial_beats_a_failed_complete_looking_record():
    partial = _rec({"alpha": D}, content="p")          # healthy, incomplete
    dead = _dead(C.EXEC_TIMEOUT, "d")                  # "complete" but dead
    res = C.select([dead, partial], _expected())
    assert res["best_record"] is partial
    assert res["closure_eligible"] is False, "best evidence is not the same as verified"
    assert res["verified_winner"] is None


def test_a_healthy_partial_is_never_called_a_verified_winner():
    res = C.select([_rec({"alpha": D}, content="p")], _expected())
    assert res["best_record"] is not None
    assert res["verified_winner"] is None, \
        "an incomplete record must not become a top-level verified pass"


def test_a_complete_healthy_record_is_a_verified_winner():
    res = C.select([_rec({"alpha": D, "beta": D, "gamma": D})], _expected())
    assert res["verified_winner"] is res["best_record"]
    assert res["closure_eligible"] is True


def test_failed_records_keep_their_observations_for_diagnostics():
    dead = _dead(C.EXEC_CRASH)
    res = C.select([dead], _expected())
    assert res["ineligible"][0]["observations"]["alpha"]["status"] == C.DEMONSTRATED


# ---- complete identity validation -----------------------------------------

@pytest.mark.parametrize("missing", ["contract_id", "contract_version",
                                     "artifact_scope", "evaluation_context_hash"])
def test_expected_identity_must_be_complete(missing):
    with pytest.raises(C.ContractError):
        C.select([], _expected(**{missing: ""}))


def test_calibration_without_adapter_identity_is_still_valid():
    exp = _expected(adapter_id="", adapter_version="", calibration_id="cal-9")
    C.select([], exp)          # must not raise


def test_no_adapter_identity_and_no_calibration_is_rejected():
    with pytest.raises(C.ContractError):
        C.select([], _expected(adapter_id="", adapter_version="", calibration_id=""))


@pytest.mark.parametrize("field", ["contract_version", "adapter_id", "adapter_version"])
def test_records_with_incomplete_identity_are_incomparable(field):
    rec = _rec({"alpha": D, "beta": D})
    rec[field] = ""
    res = C.select([rec], _expected())
    assert res["best_record"] is None
    assert rec in res["incomparable"]


# ---------------------------------------------------------------------------
# The wire envelope: transport, not policy
# ---------------------------------------------------------------------------
#
# The same generic contract, serialised. These fix the semantics the wire has
# to answer before anything consumes it, and deliberately assert that NO
# decision changed: this phase moves evidence across the boundary losslessly.

V3DIR = Path(__file__).resolve().parents[2] / "v3-service"
FIXTURES = V3DIR / "testdata" / "evidence_wire_cases.json"

C1, C2, OPT = "criterion_a", "criterion_b", "criterion_c"
CODE = "const board = [];\n"


def _task(minimum=C.BEHAVIORAL):
    return C.task_contract(
        "generate:js", "1",
        [C.requirement(C1), C.requirement(C2),
         C.requirement(OPT, required=False)],
        minimum_closure_strength=minimum)


def _record(observations, strength, *, execution=C.EXEC_OK, supported=True,
            code=CODE, minimum=C.BEHAVIORAL, scope="static/game.js"):
    return C.build(
        _task(minimum), "probe_adapter", "1.0.0", observations, [C1, C2, OPT],
        strength, execution_status=execution, supported=supported,
        artifact_scope=scope, evaluation_context_hash=C.content_hash("ctx"),
        candidate_content_hash=C.content_hash(code))


def _demonstrated(*ids):
    return {i: C.observation(C.DEMONSTRATED) for i in ids}


# --- what the envelope IS --------------------------------------------------

def test_envelope_carries_candidate_and_selection_evidence_separately():
    """Both, in separate objects. Collapsing them is how the best of a bad
    pool becomes 'verified'."""
    rec = _record(_demonstrated(C1, C2, OPT), C.BEHAVIORAL)
    env = C.envelope(rec, C.select([rec], rec), C.content_hash(CODE))

    assert set(env) == {"wire_version", "record_schema_version", "identity",
                        "evaluation", "coverage", "selection", "delivery"}
    # Candidate evidence names the candidate; selection evidence names the pool.
    assert env["evaluation"]["evidence_strength"] == C.BEHAVIORAL
    assert env["identity"]["candidate_content_hash"] == C.content_hash(CODE)
    assert env["selection"]["status"] == C.SELECTION_VERIFIED_WINNER
    # Versions travel separately: a transport change is not a domain change.
    assert env["wire_version"] == C.WIRE_VERSION
    assert env["record_schema_version"] == C.SCHEMA_VERSION


def test_best_record_without_a_verified_winner_is_representable():
    """A ranked best record below the contract's strength floor. Nothing here
    sets `passed`, and the envelope says plainly that closure was not earned."""
    rec = _record(_demonstrated(C1, C2, OPT), C.RUNTIME,
                  minimum=C.BEHAVIORAL)
    env = C.envelope(rec, C.select([rec], rec), C.content_hash(CODE))

    assert env["selection"]["status"] == C.SELECTION_BEST_NOT_ELIGIBLE
    assert env["evaluation"]["closure_eligible"] is False
    assert env["evaluation"]["requirements_complete"] is True


def test_no_evidence_is_none_and_malformed_evidence_raises():
    """Two different facts, two different outcomes. A malformed record is never
    serialised into a half-valid envelope."""
    assert A.evidence_envelope({"evidence_record": None, "code": CODE},
                               delivered_code=CODE) is None

    incomplete = _record(_demonstrated(C1), C.SYNTAX)
    incomplete["evaluation_context_hash"] = ""
    try:
        C.envelope(incomplete, None, C.content_hash(CODE))
    except C.ContractError:
        return
    raise AssertionError("a record with blank identity must not serialise")


def test_delivery_section_names_the_bytes_actually_delivered():
    rec = _record(_demonstrated(C1, C2, OPT), C.BEHAVIORAL, code=CODE)
    same = C.envelope(rec, None, C.content_hash(CODE))
    other = C.envelope(rec, None, C.content_hash("different bytes\n"))

    assert same["delivery"]["describes_delivered_candidate"] is True
    assert other["delivery"]["describes_delivered_candidate"] is False
    assert other["delivery"]["delivered_content_hash"] != \
        other["identity"]["candidate_content_hash"]


def test_unmeasurable_is_distinct_from_missing():
    """'We could not look' and 'we looked and it was absent' are different
    facts, and the coverage section keeps them apart."""
    task = C.task_contract(
        "generate:js", "1",
        [C.requirement(C1), C.requirement(C2)])
    rec = C.build(
        task, "probe_adapter", "1.0.0", _demonstrated(C1), [C1],
        C.BEHAVIORAL, artifact_scope="static/game.js",
        evaluation_context_hash=C.content_hash("ctx"),
        candidate_content_hash=C.content_hash(CODE))
    env = C.envelope(rec, None, C.content_hash(CODE))

    assert env["coverage"]["demonstrated"] == [C1]
    assert env["coverage"]["unmeasurable"] == [C2]
    assert env["coverage"]["missing"] == []


# --- live record bridging --------------------------------------------------

def test_oracle_strength_is_claimed_only_where_an_oracle_ran():
    """No verifier here runs an oracle, so no live record claims its
    strength: an accepted artifact demonstrated syntax and nothing above it."""
    for adapter in A.ALL_ADAPTERS:
        rec = A.contract_record(
            adapter=adapter, accepted=True,
            contract_id="c", contract_version="1", artifact_scope="s",
            evaluation_context_hash=C.content_hash("ctx"),
            candidate_content_hash=C.content_hash(CODE))
        assert rec["evidence_strength"] == C.SYNTAX, adapter


# --- ownership boundaries --------------------------------------------------

def test_generic_contract_stays_prompt_agnostic():
    """The generic owner carries no task vocabulary. The ids it moves are
    opaque, including through the wire envelope that now lives here."""
    src = (V3DIR / "contract.py").read_text().lower()
    for word in ("snake", "canvas", "food", "collision", "player", "keystroke"):
        assert word not in src, f"domain vocabulary {word!r} leaked into the contract"


def test_main_serialises_but_decides_nothing():
    """The handler must not grow a second policy: no ranking, no closure, no
    strength inference beside the owner that has them."""
    src = (V3DIR / "main.py").read_text()
    handler = src[src.index("def _handle_generate"):]
    handler = handler[:handler.index("def _handle_plan")]
    for banned in ("closure_eligible", "rank_key", "STRENGTH_ORDER",
                   "evidence_strength", "C.build", "contract.build",
                   "contract.select"):
        assert banned not in handler, f"main.py decides policy: {banned}"
    assert "adapters.evidence_envelope" in handler


def test_one_canonical_contract_one_serialiser_no_duplicate_policy():
    """Consolidation sentinel. One module defines the contract, one renders the
    envelope, and closure/ranking exist in exactly one place."""
    modules = {p.name: p.read_text() for p in V3DIR.glob("*.py")}
    assert "evidence_wire.py" not in modules, \
        "the wire module was folded into contract.py and adapters.py"

    defines_envelope = [n for n, s in modules.items() if "\ndef envelope(" in s]
    assert defines_envelope == ["contract.py"], defines_envelope

    defines_closure = [n for n, s in modules.items() if "\ndef _closure(" in s]
    assert defines_closure == ["contract.py"], defines_closure

    # evidence.py still carries the prototype's own ranking; nothing else may.
    ranks = sorted(n for n, s in modules.items() if "\ndef rank_key(" in s)
    assert ranks == ["contract.py"], ranks

    # Exactly one bridge from the prototype record to a contract record.
    bridges = [n for n, s in modules.items() if "\ndef contract_record(" in s]
    assert bridges == ["adapters.py"], bridges


def test_the_retiring_prototype_is_gone():
    """Retirement complete. evidence.py held the prototype's strength scale,
    early-return predicate, ranking and adapter mechanics; each moved to the
    layer that owns it, and the file itself is deleted. No compatibility
    module, alias or shim replaced it."""
    assert not (V3DIR / "evidence.py").exists(), "evidence.py is still present"
    for path in list(V3DIR.rglob("*.py")) + list(
            (V3DIR.parent / "tests").rglob("*.py")):
        text = path.read_text()
        for banned in ("import evidence\n", "import evidence as ",
                       "from evidence import"):
            # This sentinel names the strings it forbids; skip its own source.
            if path.name == Path(__file__).name:
                continue
            assert banned not in text, f"{path.name} still imports evidence"


def test_no_compatibility_aliases_or_duplicate_implementations_remain():
    modules = {p.name: p.read_text() for p in V3DIR.glob("*.py")}
    # Each moved symbol has exactly one definition, in its new owner.
    for fn, owner in (("def select_adapter(", "adapters.py"),
                      ("def contract_record(", "adapters.py"),
                      ("def envelope(", "contract.py"),
                      ("def select(", "contract.py"),
                      ("def _closure(", "contract.py"),
                      ("def rank_key(", "contract.py")):
        owners = sorted(n for n, s in modules.items() if fn in s)
        assert owners == [owner], f"{fn.strip()} owners: {owners}"
    # And the superseded policy has no definition anywhere.
    for dead in ("def may_return_early(", "def may_return_early_result(",
                 "def at_least(", "def grade_interactive(",
                 "def result_from_adapter(", "STRENGTH_ORDER = [NONE"):
        owners = sorted(n for n, s in modules.items() if dead in s)
        assert owners == [], f"superseded {dead.strip()} survives in {owners}"


def test_selection_and_closure_live_only_in_the_contract():
    modules = {p.name: p.read_text() for p in V3DIR.glob("*.py")}
    for fn in ("\ndef select(", "\ndef _closure("):
        owners = [n for n, s in modules.items() if fn in s]
        assert owners == ["contract.py"], f"{fn.strip()} owners: {owners}"
    ranks = sorted(n for n, s in modules.items() if "\ndef rank_key(" in s)
    assert ranks == ["contract.py"], ranks


def test_candidates_carry_one_canonical_record():
    """No parallel authoritative fields beside the contract record."""
    src = (V3DIR / "pipeline.py").read_text()
    for banned in ('c["evidence_strength"]', 'c["behavior_score"]',
                   'c["missing_required"]', 'c["evidence"]',
                   '"evidence_strength": probe_result'):
        assert banned not in src, f"parallel candidate field survives: {banned}"
    assert 'c["contract_record"]' in src


# --- golden fixtures -------------------------------------------------------
#
# Complete /v3/generate response bodies, produced by the REAL serialiser and
# committed so the Go tests decode these exact bytes. Regenerate with
# ATLAS_WRITE_EVIDENCE_FIXTURES=1 pytest tests/v3-service/test_contract_genericity.py

FIX_C1, FIX_C2, FIX_OPT = "criterion_a", "criterion_b", "criterion_c"
FIX_DELIVERED = "const board = [];\nfor (const c of board) { console.log(c); }\n"
FIX_OTHER = "const board = [];\n"


def _fix_task(minimum=C.BEHAVIORAL, threshold=1.0):
    return C.task_contract(
        "generate:js", "1",
        [C.requirement(FIX_C1), C.requirement(FIX_C2),
         C.requirement(FIX_OPT, required=False)],
        minimum_closure_strength=minimum, closure_quality_threshold=threshold)


def _fix_record(*, observations, strength, execution=C.EXEC_OK, supported=True,
                code=FIX_DELIVERED, scope="static/game.js", adapter="probe_adapter",
                minimum=C.BEHAVIORAL):
    return C.build(
        _fix_task(minimum), adapter, "1.0.0", observations,
        [FIX_C1, FIX_C2, FIX_OPT], strength, execution_status=execution,
        supported=supported, artifact_scope=scope,
        evaluation_context_hash=C.content_hash("build a playable board"),
        candidate_content_hash=C.content_hash(code))


def _fix_demonstrated(*ids):
    return {i: C.observation(C.DEMONSTRATED) for i in ids}


def _fix_response(env, *, passed, code=FIX_DELIVERED):
    return {"code": code, "passed": passed, "phase_solved": "phase1",
            "candidates_tested": 3, "winning_score": 0.87, "total_tokens": 1234,
            "total_time_ms": 4567.0,
            "verification_evidence": [{"verifier": "sandbox", "status": "passed"}],
            "evidence": env, "evidence_unavailable_reason": ""}


def _case(case_id, description, response, *, availability, strength="",
          selection="", describes_delivered=False, reason_contains=""):
    """One cross-language case: the bytes, and what BOTH sides must conclude.

    The expectations are declared here, by hand, and verified independently on
    the Go side. Deriving them from the same code that validates them would
    make the pair agree with itself rather than with the contract.
    """
    return {"id": case_id, "description": description, "response": response,
            "expect": {"availability": availability,
                       "evidence_strength": strength,
                       "selection_status": selection,
                       "describes_delivered_candidate": describes_delivered,
                       "reason_contains": reason_contains}}


def _build_cases():
    """Every golden case, from the real serialiser, in one document."""
    h = C.content_hash(FIX_DELIVERED)
    cases = []

    winner = _fix_record(observations=_fix_demonstrated(FIX_C1, FIX_C2, FIX_OPT),
                         strength=C.BEHAVIORAL)
    sel = C.select([winner], winner)
    cases.append(_case(
        "01_verified_winner",
        "behavioural, requirements complete, closure eligible, delivered",
        _fix_response(C.envelope(winner, sel, h), passed=True),
        availability="available", strength=C.BEHAVIORAL,
        selection="verified_winner", describes_delivered=True))

    partial = _fix_record(observations=_fix_demonstrated(FIX_C1), strength=C.BEHAVIORAL)
    cases.append(_case(
        "02_behavioral_incomplete_requirements",
        "behavioural evidence with one required criterion never demonstrated",
        _fix_response(C.envelope(partial, C.select([partial], partial), h), passed=False),
        availability="available", strength=C.BEHAVIORAL,
        selection="best_not_closure_eligible", describes_delivered=True))

    syn = _fix_record(observations={}, strength=C.SYNTAX)
    cases.append(_case(
        "03_syntax_only",
        "the verifier could observe nothing beyond parsing",
        _fix_response(C.envelope(syn, C.select([syn], syn), h), passed=False),
        availability="available", strength=C.SYNTAX,
        selection="best_not_closure_eligible", describes_delivered=True))

    best = _fix_record(observations=_fix_demonstrated(FIX_C1, FIX_C2, FIX_OPT),
                       strength=C.RUNTIME, minimum=C.BEHAVIORAL)
    cases.append(_case(
        "04_best_not_closure_eligible",
        "complete and healthy, but below the contract's strength floor",
        _fix_response(C.envelope(best, C.select([best], best), h), passed=False),
        availability="available", strength=C.RUNTIME,
        selection="best_not_closure_eligible", describes_delivered=True))

    unsup = _fix_record(observations={}, strength=C.SYNTAX,
                        execution=C.EXEC_SKIPPED, supported=False)
    cases.append(_case(
        "05_unsupported_candidate",
        "unsupported artifact: unverified, never failed",
        _fix_response(C.envelope(unsup, C.select([unsup], unsup), h), passed=False),
        availability="available", strength=C.SYNTAX,
        selection="ineligible", describes_delivered=True))

    dead = _fix_record(observations={}, strength=C.SYNTAX, execution=C.EXEC_CRASH)
    cases.append(_case(
        "06_no_verified_winner",
        "every comparable record died: a record exists, a winner does not",
        _fix_response(C.envelope(dead, C.select([dead], winner), h), passed=False),
        availability="available", strength=C.SYNTAX,
        selection="ineligible", describes_delivered=True))

    foreign = _fix_record(observations=_fix_demonstrated(FIX_C1, FIX_C2, FIX_OPT),
                          strength=C.BEHAVIORAL, scope="static/other.js")
    cases.append(_case(
        "07_incomparable_records",
        "measured under a different rubric than the expected one",
        _fix_response(C.envelope(foreign, C.select([foreign], winner), h), passed=False),
        availability="available", strength=C.BEHAVIORAL,
        selection="incomparable", describes_delivered=True))

    twin = _fix_record(observations=_fix_demonstrated(FIX_C1, FIX_C2, FIX_OPT),
                       strength=C.BEHAVIORAL, code=FIX_OTHER)
    cases.append(_case(
        "08_tied_records",
        "two records of identical rank",
        _fix_response(C.envelope(winner, C.select([winner, twin], winner), h), passed=True),
        availability="available", strength=C.BEHAVIORAL,
        selection="tied", describes_delivered=True))

    stale = _fix_record(observations=_fix_demonstrated(FIX_C1, FIX_C2, FIX_OPT),
                        strength=C.BEHAVIORAL, code=FIX_OTHER)
    cases.append(_case(
        "09_evidence_for_other_candidate",
        "well formed, and about bytes other than the delivered ones",
        _fix_response(C.envelope(stale, C.select([stale], stale), h), passed=True),
        availability="available", strength=C.BEHAVIORAL,
        selection="verified_winner", describes_delivered=False))

    legacy = _fix_response(None, passed=True)
    del legacy["evidence"]
    del legacy["evidence_unavailable_reason"]
    cases.append(_case(
        "10_legacy_no_envelope",
        "a producer that predates the envelope entirely",
        legacy, availability="absent", reason_contains="no evidence envelope"))

    # Deliberately damaged AFTER serialisation. No producer emits these; a
    # consumer that cannot survive a buggy or future one fails in the field.
    good = C.envelope(winner, sel, h)
    future = copy.deepcopy(good)
    future["wire_version"] = "99.0.0"
    cases.append(_case(
        "11_unknown_wire_version",
        "a future producer's wire major: not interpreted at all",
        _fix_response(future, passed=True),
        availability="unavailable", reason_contains="unsupported wire version"))

    broken = copy.deepcopy(good)
    broken["identity"]["evaluation_context_hash"] = ""
    cases.append(_case(
        "12_malformed_identity",
        "identity with a blank required component",
        _fix_response(broken, passed=True),
        availability="unavailable", reason_contains="identity incomplete"))

    contradiction = copy.deepcopy(good)
    contradiction["evaluation"]["execution_status"] = C.EXEC_TIMEOUT
    cases.append(_case(
        "13_closure_contradicts_execution",
        "closure claimed over an execution that did not complete",
        _fix_response(contradiction, passed=True),
        availability="unavailable",
        reason_contains="closure claimed over execution status"))

    return {"schema": "atlas.evidence_wire.cases/1", "cases": cases}


def test_golden_cases_match_the_serialiser():
    """The committed bytes are exactly what the serialiser produces today, in
    ONE document both languages read. Go decodes these, so a silent drift here
    is a cross-language drift."""
    rendered = json.dumps(_build_cases(), indent=2, sort_keys=True) + "\n"
    if os.environ.get("ATLAS_WRITE_EVIDENCE_FIXTURES") == "1":
        FIXTURES.write_text(rendered)
        return
    assert FIXTURES.exists(), f"missing golden case file {FIXTURES}"
    assert FIXTURES.read_text() == rendered, (
        "evidence_wire_cases.json is stale; regenerate with "
        "ATLAS_WRITE_EVIDENCE_FIXTURES=1 pytest tests/v3-service/test_contract_genericity.py")


def test_every_required_case_exists_exactly_once():
    doc = json.loads(FIXTURES.read_text())
    ids = [c["id"] for c in doc["cases"]]
    assert len(ids) == len(set(ids)) == 13, ids
    for expected in ("01_verified_winner", "02_behavioral_incomplete_requirements",
                     "03_syntax_only", "04_best_not_closure_eligible",
                     "05_unsupported_candidate", "06_no_verified_winner",
                     "07_incomparable_records", "08_tied_records",
                     "09_evidence_for_other_candidate", "10_legacy_no_envelope",
                     "11_unknown_wire_version", "12_malformed_identity",
                     "13_closure_contradicts_execution"):
        assert expected in ids, f"missing golden case {expected}"
    # Every case declares what both sides must conclude from it.
    for c in doc["cases"]:
        assert c["description"]
        assert c["expect"]["availability"] in ("available", "unavailable", "absent")
        if c["expect"]["availability"] == "available":
            assert c["expect"]["evidence_strength"]
            assert c["expect"]["selection_status"]
        else:
            assert c["expect"]["reason_contains"]


def _case_by_id(case_id):
    for c in json.loads(FIXTURES.read_text())["cases"]:
        if c["id"] == case_id:
            return c
    raise AssertionError(f"no case {case_id}")


def test_legacy_case_has_no_envelope_at_all():
    payload = _case_by_id("10_legacy_no_envelope")["response"]
    assert "evidence" not in payload
    assert payload["passed"] is True


def test_no_generated_case_observation_reaches_the_wire_or_authorization():
    """Consensus and generated-case scores are diagnostics.

    Go authorizes on the envelope alone — availability, verified_winner,
    closure eligibility and a content hash. Nothing derived from a
    model-generated case may appear there, and nothing may reach the
    response the proxy reads: the handler writes an explicit whitelist, and
    `consensus` is deliberately not in it.
    """
    main_src = (V3DIR / "main.py").read_text()
    response_block = main_src.split("response = {", 1)[1].split("}", 1)[0]
    for leaked in ("consensus", "self_test", "cases_passed", "provenance"):
        assert leaked not in response_block, leaked

    envelope_src = (V3DIR / "contract.py").read_text()
    for leaked in ("consensus", "agreement", "cluster", "provenance"):
        assert leaked not in envelope_src, leaked

    # The envelope's own shape is unchanged by this work.
    record = _record(_demonstrated(), C.SYNTAX)
    envelope = C.envelope(record, C.select([record], record),
                          delivered_content_hash=C.content_hash(CODE))
    assert set(envelope) == {"wire_version", "record_schema_version", "identity",
                             "evaluation", "coverage", "selection", "delivery"}
    assert "consensus" not in json.dumps(envelope)


def test_consensus_defines_no_second_strength_scale():
    """One strength vocabulary, in the contract. A ranking signal that grew
    its own scale is how correlated agreement becomes 'behavioural'."""
    pipeline_src = (V3DIR / "pipeline.py").read_text()
    consensus_block = pipeline_src.split("def _consensus_winners(", 1)[1]
    consensus_block = consensus_block.split("\nclass ", 1)[0].split("\ndef ", 1)[0]
    for forbidden in ("STRENGTH_", "closure_eligible", "verified_winner",
                      "requirements_complete", "evidence_strength"):
        assert forbidden not in consensus_block, forbidden
