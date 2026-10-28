"""What the V3 pipeline records about a candidate, and how it names and ranks
the pool: the Lens view of a candidate, the task identity every record is
measured under, the closure rule and the evidence delivered with a result,
the incumbent's index and name, the re-typing rule, and contract selection
over the passing candidates' records. The canonical record itself,
_evaluate_candidate, is in pipeline.py."""

from typing import Any, Dict, List, Optional

import adapters
import contract
import scoring

def _lens_view(code: str) -> Dict[str, Any]:
    """Both Lens views of one candidate, as its record carries them.

    `energy` and `energy_norm` are None and `lens_failure` is set when the
    Lens did not score the candidate (its input exceeded the embedding
    server's physical batch, the Lens answered an error or was unreachable).
    `per_step` is empty when the per-step call did not score; that failure
    is recorded under `lens_failure` with `path: per_step` when C(x) did.
    Nothing here substitutes a number for a score that did not happen.
    """
    scored = scoring.score_candidate(code)
    energy, energy_norm, energy_calibrated = scored
    failure = getattr(scored, "failure", None)
    token_assertion = getattr(scored, "token_assertion", None)
    per_step = scoring.score_candidate_per_step(code)  # PC-207
    per_step = dict(per_step) if isinstance(per_step, dict) else {}
    per_step_failure = per_step.pop("failure", None)
    per_step_token_assertion = per_step.pop("token_assertion", None)
    if per_step_failure:
        per_step = {}
    if energy is None:
        failure = failure or per_step_failure or {
            "kind": scoring.LENS_ERROR, "detail": "no C(x) energy returned"}
        energy_norm, energy_calibrated = None, False
    elif per_step_failure and failure is None:
        failure = {**per_step_failure, "path": "per_step"}
    return {"energy": energy, "energy_norm": energy_norm,
            "energy_calibrated": bool(energy_calibrated),
            "token_assertion": token_assertion,
            "per_step_token_assertion": per_step_token_assertion,
            "per_step": per_step, "lens_failure": failure}


def _note_lens(emit, candidate: Dict[str, Any], source: str) -> None:
    """Emit what the Lens said about a freshly generated candidate."""
    index = candidate["index"]
    per_step = candidate.get("per_step") or {}
    if per_step:
        emit("lens_per_step",
             f"cand {index}: gx_min={per_step['gx_score_min']:.2f} "
             f"first_off_rails={per_step['first_off_rails_idx']}",
             index=index,
             source=source,
             first_off_rails_idx=per_step["first_off_rails_idx"],
             gx_score_min=per_step["gx_score_min"],
             gx_score_mean=per_step["gx_score_mean"],
             cx_norm_max=per_step["cx_norm_max"],
             n_tokens=per_step["n_tokens"])
    failure = candidate.get("lens_failure")
    if candidate.get("energy") is None and failure:
        emit("lens_unscored",
             f"cand {index}: unscored ({scoring.describe_lens_failure(failure)})",
             index=index, source=source,
             kind=failure.get("kind"),
             input_tokens=failure.get("input_tokens"),
             capacity_tokens=failure.get("capacity_tokens"),
             failure_detail=failure.get("detail"))


def _candidate_lens_payload(candidate: Dict[str, Any]) -> Dict[str, Any]:
    """The exact Lens fields carried by candidate capture records."""
    return {"energy": candidate.get("energy"),
            "energy_norm": candidate.get("energy_norm"),
            "energy_calibrated": candidate.get("energy_calibrated"),
            "token_assertion": candidate.get("token_assertion"),
            "per_step_token_assertion": candidate.get("per_step_token_assertion"),
            "per_step": candidate.get("per_step"),
            "failure": candidate.get("lens_failure")}


def _candidate_by_index(candidates: List[Dict[str, Any]], index: int) -> Optional[Dict[str, Any]]:
    """Return the candidate dict whose original ``index`` field matches.

    Selection reports the winner by the candidate's original index, but
    the ``passing`` list has been sorted and filtered — positional
    indexing would pick the wrong candidate (or IndexError). Returns None
    when no candidate carries that index.
    """
    return next((c for c in candidates if c.get("index") == index), None)


def _ensure_delivered_evidence(result, *, file_path, problem):
    """Every successful exit describes THE BYTES IT RETURNS.

    Not every exit runs the structured evaluation on the artifact it hands
    back: repair, refinement and the budget fallback can return code with no
    record of it, or with the record of a different candidate.

    This evaluates the delivered bytes through the SAME canonical
    adapter->contract path every other candidate goes through. Nothing new
    runs here -- an artifact whose behaviour was never observed reports
    exactly that -- and the verifier's own accept/reject is the observation
    input, never a claim of complete evidence: `accepted` on an interactive
    artifact still yields syntax-level, unsupported evidence, and no adapter
    here reports more than syntax.

    It changes no path's `passed`, selection or return value. Where a legacy
    `passed=true` sits on evidence that is not closure-eligible, the envelope
    says so plainly rather than hiding the contradiction.
    """
    if not result:
        return
    code = result.get("code") or ""
    if not code:
        return
    task = _task_identity(file_path, problem)
    record = result.get("evidence_record")
    if not record or record.get("candidate_content_hash") != contract.content_hash(code):
        record = adapters.contract_record(
            adapter=adapters.select_adapter(file_path, code),
            accepted=bool(result.get("passed")),
            contract_id=task["contract_id"],
            contract_version=task["contract_version"],
            artifact_scope=task["artifact_scope"],
            evaluation_context_hash=task["evaluation_context_hash"],
            candidate_content_hash=contract.content_hash(code))
        result["evidence_record"] = record
        # The selection that produced the old record described a different
        # pool; re-state it over the record actually delivered.
        result["contract_selection"] = None
    selection = result.get("contract_selection")
    if not selection or selection.get("best_record") is not record:
        try:
            result["contract_selection"] = contract.select([record], record)
        except contract.ContractError as exc:
            result["contract_selection"] = {
                "best_record": None, "verified_winner": None, "tied": [],
                "incomparable": [], "ineligible": [],
                "selection_reason": f"identity error: {exc}"}


def _task_identity(file_path, problem):
    """What every record in this run is measured under.

    The rubric is the TASK's, not a candidate's: same contract, same artifact
    scope, same evaluation context for every candidate, so records that
    disagree about any of them are incomparable rather than silently ranked
    against each other.
    """
    ext = (file_path or "").rsplit(".", 1)
    suffix = ext[-1].lower() if len(ext) == 2 else "unknown"
    return {"contract_id": f"generate:{suffix}",
            "contract_version": "1",
            "artifact_scope": file_path or "",
            "evaluation_context_hash": contract.content_hash(problem or "")}


def _record_closes(record, code):
    """Closure is contract.select's verdict, on a record proven to describe
    these exact bytes. A record whose hash does not match the candidate it is
    attached to may neither close nor win -- stale evidence about other bytes
    is the one way a verified claim becomes a false one.
    """
    if not record or record.get("candidate_content_hash") != contract.content_hash(code):
        return False
    try:
        selection = contract.select([record], record)
    except contract.ContractError:
        return False
    return selection.get("verified_winner") is record


# The pool index of the incumbent: the caller's own bytes. Every generated
# candidate is numbered from 0 (the phase-zero probe) up; the incumbent was not
# generated, so it takes no generation slot and no generated number.
INCUMBENT_INDEX = -1


def _who(candidate) -> str:
    """How an event names a pool member."""
    if candidate.get("incumbent"):
        return "The submitted file"
    return f"Candidate {candidate.get('index')}"


def _whitespace_free(text: str) -> str:
    return "".join((text or "").split())


def _without_retypings(passing, emit=None):
    """The submitted bytes, never a copy of them (#259).

    A candidate that equals the passing incumbent once whitespace is ignored
    is the incumbent re-typed: the same program plus whatever drift the
    re-typing added, observed as a leading space inside a printed message. It
    leaves the pool, and the exact bytes stand for both. A copy of an
    incumbent that did NOT pass stays: re-typing is how an indentation slip
    gets repaired.
    """
    incumbent = next((c for c in passing if c.get("incumbent")), None)
    if incumbent is None:
        return passing
    bare = _whitespace_free(incumbent.get("code"))
    kept = [c for c in passing
            if c is incumbent or _whitespace_free(c.get("code")) != bare]
    if emit is not None and len(kept) < len(passing):
        dropped = [c.get("index") for c in passing if c not in kept]
        emit("retyping_dropped",
             f"{len(dropped)} candidate(s) only re-typed the submitted file; "
             f"its exact bytes stand for them", indices=dropped)
    return kept


def _select_by_contract(passing, selected, result, capture):
    """Contract selection over the passing candidates' own records, kept in
    the result as the evidence selection and in the capture as the pool."""
    # Contract selection over the candidates' own records. It does
    # not choose the delivered bytes -- the lens choice above stands
    # -- but it is the selection the evidence envelope reports, and
    # the proxy authorizes a candidate only on a verified winner
    # from it. The choice is contract.select over the candidates' own
    # records, under the rubric the BASELINE was measured with --
    # records that disagree about contract, artifact or context are
    # incomparable rather than silently ranked, so a foreign
    # majority cannot outvote one matching record.
    pool = [c for c in passing if c.get("contract_record")]
    expected = result.get("evidence_record") or (
        pool[0]["contract_record"] if pool else None)
    contract_pick = None
    if pool and expected:
        picked, contract_pick, verified = _contract_pick(pool, expected)
        status = contract.selection_status(picked)
        result["contract_selection"] = picked
        result["evidence_selection"] = {
            "lens_index": getattr(selected, "index", None),
            "evidence_index": (contract_pick or {}).get("index"),
            "verified_index": (verified or {}).get("index"),
            "agree": getattr(selected, "index", None)
                     == (contract_pick or {}).get("index"),
            "status": status,
            "reason": picked.get("selection_reason", ""),
            "tied": len(picked.get("tied") or []),
            "incomparable": len(picked.get("incomparable") or []),
            "ineligible": len(picked.get("ineligible") or []),
            "candidates": [
                {"index": c.get("index"),
                 "strength": c["contract_record"]["evidence_strength"],
                 "adapter": c["contract_record"]["adapter_id"],
                 "supported": c["contract_record"]["supported"],
                 "closure_eligible": c["contract_record"]["closure_eligible"],
                 "quality": c["contract_record"]["overall_quality_score"],
                 "missing_required": c["contract_record"]["missing_required"],
                 "energy": c.get("energy")}
                for c in pool],
        }
        capture.note_pool(
            phase="phase1", pool=pool,
            lens_index=getattr(selected, "index", None),
            evidence_index=(contract_pick or {}).get("index"),
            verified_index=(verified or {}).get("index"),
            status=status, reason=picked.get("selection_reason", ""),
            tied=len(picked.get("tied") or []),
            incomparable=len(picked.get("incomparable") or []),
            ineligible=len(picked.get("ineligible") or []))


def _contract_pick(pool, expected):
    """contract.select over the pool's records. Returns (picked, the pick
    the evidence reports, the verified winner or None)."""
    try:
        picked = contract.select(
            [c["contract_record"] for c in pool], expected)
    except contract.ContractError as exc:
        picked = {"best_record": None, "verified_winner": None,
                  "tied": [], "incomparable": [], "ineligible": [],
                  "selection_reason": f"identity error: {exc}"}
    by_record = {id(c["contract_record"]): c for c in pool}
    best = by_record.get(id(picked.get("best_record")))
    verified = by_record.get(id(picked.get("verified_winner")))
    # A record must describe the bytes it is attached to before
    # it may win anything.
    if verified and not _record_closes(
            verified["contract_record"], verified["code"]):
        verified = None
    contract_pick = verified or best
    return picked, contract_pick, verified


def _announce_selection(emit, passing, selected):
    """Emit which candidate the Lens selected and on what grounds. Returns
    the selected candidate's dict (None if no candidate has its index)."""
    winner = _candidate_by_index(passing, selected.index)
    lens_scored = getattr(selected, "energy", None) is not None
    lens_failure = (winner or {}).get("lens_failure")
    named = ("the submitted file" if (winner or {}).get("incumbent")
             else f"candidate {selected.index}")
    if (winner or {}).get("consensus"):
        detail = (f"Lens selected {named} from candidates "
                  f"that agree — no candidate passed verification")
    elif lens_scored:
        detail = f"Lens selected {named}"
    else:
        # The only verified candidate carries no score. It
        # is delivered on its sandbox evidence and says so;
        # a scored candidate would have outranked it.
        detail = (f"Selected {named}: verified, "
                  f"unscored by the lens "
                  f"({scoring.describe_lens_failure(lens_failure or {})})")
    emit("selected", detail,
         index=selected.index,
         energy=getattr(selected, "energy", None),
         lens_scored=lens_scored, lens_failure=lens_failure)
    return winner
