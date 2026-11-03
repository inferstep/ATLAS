"""Generic, versioned evidence contract.

Four concepts kept separate, because collapsing any pair produced real
defects:

  1. TASK REQUIREMENTS    what this user asked for      (owned by the task)
  2. ADAPTER CAPABILITIES what a verifier can observe   (owned by the adapter)
  3. OBSERVATIONS         what this execution showed
  4. DECISION POLICY      how 1+3 give coverage, ranking, closure  (generic)

The adapter declares what it CAN measure; it never decides what the user
REQUIRED. Criterion ids are opaque strings to everything here.
"""

from __future__ import annotations

import hashlib
import math
from typing import Any, Callable, Dict, List, Optional, Sequence, Tuple

SCHEMA_VERSION = "1.1.0"

# Observation status
DEMONSTRATED = "demonstrated"
REFUTED = "refuted"
UNOBSERVED = "unobserved"
INCONCLUSIVE = "inconclusive"
NOT_APPLICABLE = "not_applicable"
OBSERVATION_STATUSES = {DEMONSTRATED, REFUTED, UNOBSERVED, INCONCLUSIVE, NOT_APPLICABLE}

# Evidence strength: how strong the VERIFIER was. Independent of coverage.
SYNTAX = "syntax"
RUNTIME = "runtime"
BEHAVIORAL = "behavioral"
ORACLE = "oracle"
STRENGTH_ORDER = [SYNTAX, RUNTIME, BEHAVIORAL, ORACLE]

# Execution outcome. Only OK may support a completeness claim: a run that
# timed out can still have marked criteria demonstrated before dying, and
# ranking put completeness first, so it could outrank a healthy record.
EXEC_OK = "ok"
EXEC_TIMEOUT = "timeout"
EXEC_ERROR = "error"
EXEC_CRASH = "crash"
EXEC_SKIPPED = "skipped"
EXECUTION_STATUSES = {EXEC_OK, EXEC_TIMEOUT, EXEC_ERROR, EXEC_CRASH, EXEC_SKIPPED}


class ContractError(ValueError):
    """Malformed record: a wire schema must reject these before serialising."""


def requirement(criterion_id: str, required: bool = True, weight: float = 1.0) -> Dict:
    return {"id": criterion_id, "required": bool(required), "weight": float(weight)}


def observation(status: str, detail: str = "", confidence: float = 1.0) -> Dict:
    return {"status": status, "detail": detail, "confidence": float(confidence)}


def task_contract(contract_id: str, contract_version: str,
                  requirements: Sequence[Dict],
                  minimum_closure_strength: str = BEHAVIORAL,
                  closure_quality_threshold: float = 1.0) -> Dict:
    """What the TASK demands — including how strong its evidence must be.

    A universal "behavioral or better" floor is not prompt-agnostic: an
    algorithmic contract should demand an oracle, while "produce JSON
    matching this schema" is legitimately closed by schema evidence. The
    floor belongs to the contract, defaulting conservatively.
    """
    if minimum_closure_strength not in STRENGTH_ORDER:
        raise ContractError(f"unknown minimum_closure_strength {minimum_closure_strength!r}")
    return {"contract_id": contract_id, "contract_version": contract_version,
            "requirements": list(requirements),
            "minimum_closure_strength": minimum_closure_strength,
            "closure_quality_threshold": float(closure_quality_threshold)}


def _validate(reqs, obs, caps, strength, execution_status, supported):
    ids = [r.get("id") for r in reqs]
    if any(not isinstance(i, str) or not i.strip() for i in ids):
        raise ContractError("criterion ids must be non-empty strings")
    if len(set(ids)) != len(ids):
        raise ContractError(f"duplicate requirements: {ids}")
    for r in reqs:
        w = r.get("weight")
        if not isinstance(w, (int, float)) or not math.isfinite(w) or w < 0:
            raise ContractError(f"weight for {r.get('id')!r} must be finite and >= 0")
    for cid, o in obs.items():
        if o.get("status") not in OBSERVATION_STATUSES:
            raise ContractError(f"unknown observation status {o.get('status')!r} for {cid!r}")
        c = o.get("confidence", 1.0)
        if not isinstance(c, (int, float)) or not math.isfinite(c) or not 0.0 <= c <= 1.0:
            raise ContractError(f"confidence for {cid!r} must be within [0,1]")
        # An adapter may not report on a criterion it cannot measure, in
        # EITHER direction: claiming refutation it cannot observe is the same
        # overreach as claiming demonstration.
        if cid not in caps and o.get("status") in (DEMONSTRATED, REFUTED):
            raise ContractError(f"adapter reported {o['status']} for {cid!r}, outside its capabilities")
    if strength not in STRENGTH_ORDER:
        raise ContractError(f"unknown evidence strength {strength!r}")
    if execution_status not in EXECUTION_STATUSES:
        raise ContractError(f"unknown execution status {execution_status!r}")
    if supported and execution_status == EXEC_SKIPPED:
        raise ContractError("a skipped execution cannot be reported as supported")


def build(task: Dict, adapter_id: str, adapter_version: str,
          observations: Dict[str, Dict], capabilities: Sequence[str],
          evidence_strength: str, execution_status: str = EXEC_OK,
          supported: bool = True, artifact_scope: str = "",
          evaluation_context_hash: str = "", candidate_content_hash: str = "",
          calibration_id: str = "") -> Dict:
    """Assemble one evidence record and derive the policy fields."""
    reqs = task["requirements"]
    caps = set(capabilities)
    _validate(reqs, observations, caps, evidence_strength, execution_status, supported)

    # Materialise derived observations so telemetry can explain WHY a record
    # failed to complete; previously they only existed inside this function.
    materialised = dict(observations)
    for r in reqs:
        cid = r["id"]
        if cid not in materialised:
            materialised[cid] = observation(
                UNOBSERVED if cid in caps else NOT_APPLICABLE,
                detail="not measurable by this adapter" if cid not in caps else "")

    req_got = req_tot = opt_got = opt_tot = 0.0
    missing: List[str] = []
    for r in reqs:
        cid, w = r["id"], r["weight"]
        demonstrated = materialised[cid]["status"] == DEMONSTRATED
        if r["required"]:
            req_tot += w
            if demonstrated:
                req_got += w
            else:
                missing.append(cid)
        else:
            opt_tot += w
            if demonstrated:
                opt_got += w

    execution_ok = execution_status == EXEC_OK
    required_coverage = (req_got / req_tot) if req_tot else 1.0
    optional_quality = (opt_got / opt_tot) if opt_tot else 0.0
    total_tot = req_tot + opt_tot
    overall = ((req_got + opt_got) / total_tot) if total_tot else 0.0

    requirements_complete = bool(not missing and supported and execution_ok)
    return {
        "schema_version": SCHEMA_VERSION,
        "contract_id": task["contract_id"],
        "contract_version": task["contract_version"],
        "adapter_id": adapter_id,
        "adapter_version": adapter_version,
        "calibration_id": calibration_id,
        "artifact_scope": artifact_scope,
        "evaluation_context_hash": evaluation_context_hash,
        "candidate_content_hash": candidate_content_hash,
        "requirements": list(reqs),
        "capabilities": sorted(caps),
        "observations": materialised,
        "execution_status": execution_status,
        "execution_ok": execution_ok,
        "supported": bool(supported),
        "evidence_strength": evidence_strength,
        "requirements_complete": requirements_complete,
        "required_coverage_score": round(required_coverage, 4),
        "optional_quality_score": round(optional_quality, 4),
        "overall_quality_score": round(overall, 4),
        "missing_required": missing,
        "closure_eligible": _closure(task, evidence_strength, requirements_complete,
                                     overall, supported, execution_ok),
    }


def _closure(task, strength, requirements_complete, quality, supported, execution_ok) -> bool:
    """Closure is a POLICY decision against the TASK's declared floor."""
    if not supported or not execution_ok or not requirements_complete:
        return False
    floor = task.get("minimum_closure_strength", BEHAVIORAL)
    if STRENGTH_ORDER.index(strength) < STRENGTH_ORDER.index(floor):
        return False
    return quality >= task.get("closure_quality_threshold", 1.0)


def _identity_complete(rec: Dict) -> bool:
    """Every identity component must be present, not just three of them.

    Partial identity is how unrelated records compare through equal empty
    values, and how a malformed record silently becomes incomparable
    instead of raising.
    """
    if not (rec.get("contract_id") and rec.get("contract_version")
            and rec.get("artifact_scope") and rec.get("evaluation_context_hash")):
        return False
    if rec.get("calibration_id"):
        return True
    return bool(rec.get("adapter_id") and rec.get("adapter_version"))


def require_identity(rec: Dict, what: str = "record") -> None:
    if not _identity_complete(rec):
        raise ContractError(
            f"{what} identity incomplete: needs contract id+version, artifact scope, "
            f"evaluation context hash, and either a calibration id or adapter id+version")


def comparison_identity(rec: Dict) -> Tuple:
    return (rec.get("contract_id"), rec.get("contract_version"),
            rec.get("artifact_scope"), rec.get("evaluation_context_hash"),
            rec.get("calibration_id") or (rec.get("adapter_id"), rec.get("adapter_version")))


def comparable(a: Dict, b: Dict) -> bool:
    """Same rubric, same context, same measuring instrument.

    Two adapters implementing one contract do not automatically produce
    numerically comparable scores, so identity includes adapter/calibration.
    """
    if not (_identity_complete(a) and _identity_complete(b)):
        return False
    return comparison_identity(a) == comparison_identity(b)


def rank_key(rec: Dict) -> Tuple:
    """Required coverage dominates optional quality.

    A candidate demonstrating most required behaviour must not lose to one
    demonstrating none of it because the latter scored heavily weighted
    optional criteria.
    """
    return (
        1 if rec.get("execution_ok") else 0,
        1 if rec.get("requirements_complete") else 0,
        float(rec.get("required_coverage_score", 0.0)),
        STRENGTH_ORDER.index(rec["evidence_strength"])
        if rec.get("evidence_strength") in STRENGTH_ORDER else -1,
        float(rec.get("optional_quality_score", 0.0)),
        float(rec.get("overall_quality_score", 0.0)),
    )


def select(records: Sequence[Dict], expected: Dict,
           tie_break: Optional[Callable[[Dict], Any]] = None) -> Dict:
    """Structured selection under the TASK's expected identity.

    Returns a record, not an overloaded tuple, because the distinctions
    matter downstream: a healthy PARTIAL candidate can be the best
    evidence-backed option without being closure-eligible, and calling it a
    "verified winner" is how a fallback becomes a top-level passed=true.

    Failed executions never enter the selectable pool. Ranking alone was not
    enough: a timeout lost to a healthy record, but when EVERY comparable
    record had died, the best corpse still won.
    """
    require_identity(expected, "expected")

    comparable_recs, incomparable, ineligible = [], [], []
    for r in records:
        if not comparable(r, expected):
            incomparable.append(r)
        elif not r.get("supported") or not r.get("execution_ok"):
            # Kept with observations intact for diagnostics, never selectable.
            ineligible.append(r)
        else:
            comparable_recs.append(r)

    if not comparable_recs:
        reason = ("no comparable record" if not ineligible
                  else "every comparable record failed to execute or was unsupported")
        return {"best_record": None, "closure_eligible": False, "verified_winner": None,
                "incomparable": incomparable, "ineligible": ineligible,
                "tied": [], "selection_reason": reason}

    best = max(rank_key(r) for r in comparable_recs)
    tied = [r for r in comparable_recs if rank_key(r) == best]
    if len(tied) > 1 and tie_break is not None:
        winner = max(tied, key=tie_break)
        reason = "tie broken by the caller's key"
    else:
        winner = tied[0]
        reason = "highest evidence rank" if len(tied) == 1 else "stable order among exact ties"

    closure = bool(winner.get("closure_eligible"))
    return {
        "best_record": winner,
        "closure_eligible": closure,
        # Only a closure-eligible record may be called verified.
        "verified_winner": winner if closure else None,
        "incomparable": incomparable,
        "ineligible": ineligible,
        "tied": tied,
        "selection_reason": reason,
    }


# ---------------------------------------------------------------------------
# Wire envelope
# ---------------------------------------------------------------------------
#
# The serialised form of a record plus the selection it took part in. It lives
# here, with the domain, because the vocabulary it renders IS the domain: a
# second module deciding what "verified" means on the wire is exactly the
# duplicate policy this contract exists to prevent. main.py writes it; the Go
# side validates and carries it; neither interprets it.
#
# Candidate evidence (evaluation, coverage) and selection evidence (selection)
# stay separate objects, because collapsing them is how "the best of a bad
# pool" becomes "verified".
#
# Criterion ids are opaque here as everywhere else.

# Bumped when the WIRE shape changes. SCHEMA_VERSION travels beside it and
# versions the record semantics independently, so a consumer can tell a
# transport change from a domain change.
WIRE_VERSION = "1.0.0"

# What the selection concluded, as a closed vocabulary -- never a bare boolean,
# because "a best record exists" and "a winner is verified" are different facts.
SELECTION_VERIFIED_WINNER = "verified_winner"
SELECTION_BEST_NOT_ELIGIBLE = "best_not_closure_eligible"
SELECTION_TIED = "tied"
SELECTION_INCOMPARABLE = "incomparable"
SELECTION_INELIGIBLE = "ineligible"
SELECTION_NO_VERIFIED_WINNER = "no_verified_winner"
SELECTION_STATUSES = {
    SELECTION_VERIFIED_WINNER, SELECTION_BEST_NOT_ELIGIBLE, SELECTION_TIED,
    SELECTION_INCOMPARABLE, SELECTION_INELIGIBLE, SELECTION_NO_VERIFIED_WINNER,
}


def content_hash(text: str) -> str:
    """The one hash function both sides use to name bytes."""
    return hashlib.sha256((text or "").encode("utf-8")).hexdigest()


def selection_status(selection: Optional[Dict]) -> str:
    """The closed-vocabulary reading of a select() result."""
    if not selection or selection.get("best_record") is None:
        if selection and selection.get("ineligible"):
            return SELECTION_INELIGIBLE
        if selection and selection.get("incomparable"):
            return SELECTION_INCOMPARABLE
        return SELECTION_NO_VERIFIED_WINNER
    if selection.get("verified_winner") is not None:
        if len(selection.get("tied") or []) > 1:
            return SELECTION_TIED
        return SELECTION_VERIFIED_WINNER
    return SELECTION_BEST_NOT_ELIGIBLE


def _coverage(record: Dict) -> Dict:
    required, demonstrated, missing, unmeasurable = [], [], [], []
    optional = []
    obs = record.get("observations") or {}
    for r in record.get("requirements") or []:
        cid = r["id"]
        status = (obs.get(cid) or {}).get("status", UNOBSERVED)
        if r.get("required"):
            required.append(cid)
            if status == DEMONSTRATED:
                demonstrated.append(cid)
            elif status == NOT_APPLICABLE:
                unmeasurable.append(cid)
            else:
                missing.append(cid)
        else:
            optional.append({"id": cid, "status": status})
    return {"required": required, "demonstrated": demonstrated,
            "missing": missing, "unmeasurable": unmeasurable,
            "optional": optional}


def envelope(record: Optional[Dict], selection: Optional[Dict],
             delivered_content_hash: str) -> Dict:
    """Render the versioned envelope. Raises ContractError on a bad record."""
    if record is None:
        raise ContractError("no record to serialise")
    require_identity(record, "record")

    status = selection_status(selection)
    if status not in SELECTION_STATUSES:
        raise ContractError(f"unknown selection status {status!r}")

    return {
        "wire_version": WIRE_VERSION,
        "record_schema_version": record.get("schema_version", SCHEMA_VERSION),
        "identity": {
            "contract_id": record["contract_id"],
            "contract_version": record["contract_version"],
            "adapter_id": record.get("adapter_id", ""),
            "adapter_version": record.get("adapter_version", ""),
            "calibration_id": record.get("calibration_id", ""),
            "artifact_scope": record["artifact_scope"],
            "evaluation_context_hash": record["evaluation_context_hash"],
            "candidate_content_hash": record.get("candidate_content_hash", ""),
        },
        "evaluation": {
            "execution_status": record["execution_status"],
            "supported": bool(record["supported"]),
            "evidence_strength": record["evidence_strength"],
            "requirements_complete": bool(record["requirements_complete"]),
            "closure_eligible": bool(record["closure_eligible"]),
            "quality": {
                "required_coverage": record["required_coverage_score"],
                "optional_quality": record["optional_quality_score"],
                "overall": record["overall_quality_score"],
            },
        },
        "coverage": _coverage(record),
        "selection": {
            "status": status,
            "reason": (selection or {}).get("selection_reason", ""),
            "tied_count": len((selection or {}).get("tied") or []),
            "incomparable_count": len((selection or {}).get("incomparable") or []),
            "ineligible_count": len((selection or {}).get("ineligible") or []),
        },
        "delivery": {
            "delivered_content_hash": delivered_content_hash,
            # A convenience for readers; the consumer still recomputes the hash
            # of what it is about to deliver rather than trusting this flag.
            "describes_delivered_candidate":
                bool(delivered_content_hash)
                and record.get("candidate_content_hash") == delivered_content_hash,
        },
    }
