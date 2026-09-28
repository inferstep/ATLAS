"""Where a piece of evidence came from, and what it is about.

`contract.py` records how STRONG a verifier was. It does not record who the
verifier was, so a model-generated test that runs and a repository test that
runs produce the same `behavioral` record. Strength is not trust: the same
model wrote the code and the test, from the problem statement alone, and on
the captured pool 21 of 36 valid generated keys disagreed with the task's own
reference.

Two things live here, and in this build neither authorizes anything. Nothing
in production imports this module yet; wiring it into a delivery decision is a
separate change, because doing it here would alter what lands on disk.

  source   a closed, source-SPECIFIC vocabulary. Deliberately not a
           `trusted=true` boolean: a boolean cannot say that the proxy's own
           syntax gate may close a syntax obligation and may not close a
           behavioural one, which is exactly the distinction that decides
           whether a candidate may be delivered.

  binding  the identities the evidence is about -- candidate, request,
           invocation, command, obligation, and the workspace generation it
           was observed against. Evidence that cannot name them cannot be
           shown to be about them, and evidence about a workspace two
           mutations ago is about a workspace that no longer exists.

The hidden benchmark evaluator is absent by construction: there is no constant
for it and no constructor that can produce one, so production cannot name it
even by mistake. A test fails if such a name appears.

Nothing here holds candidate bytes. A binding carries hashes and identities;
the bytes stay where they already are.
"""

from __future__ import annotations

from typing import Any, Dict, Optional, Tuple

import contract

# --- sources ---------------------------------------------------------------

# A check that was in the repository BEFORE generation, pinned by hash.
SOURCE_REPO_OWNED_CHECK = "repo_owned_check"
# Examples the client stated in its structured contract.
SOURCE_CLIENT_DECLARED_EXAMPLE = "client_declared_example"
# Commands the client required be run, as the client wrote them.
SOURCE_CLIENT_DECLARED_VERIFICATION = "client_declared_verification"
# ATLAS's own syntax and structural gates.
SOURCE_PROXY_OWNED_VALIDATION = "proxy_owned_validation"
# Anything the model wrote: tests, examples, assertions, prose.
SOURCE_MODEL_GENERATED = "model_generated"
# A record produced before provenance existed.
SOURCE_LEGACY = "legacy"
# Provenance that cannot be established. Never a default that grants anything.
SOURCE_UNKNOWN = "unknown"

SOURCES = (
    SOURCE_REPO_OWNED_CHECK,
    SOURCE_CLIENT_DECLARED_EXAMPLE,
    SOURCE_CLIENT_DECLARED_VERIFICATION,
    SOURCE_PROXY_OWNED_VALIDATION,
    SOURCE_MODEL_GENERATED,
    SOURCE_LEGACY,
    SOURCE_UNKNOWN,
)

# The strongest obligation each source may ever close. A source absent from
# this map may close nothing, which is why model_generated, legacy and unknown
# are absent rather than mapped to a low value: "weak authority" and "no
# authority" are different, and only one of them may be raised later by a
# stronger observation.
_MAX_AUTHORIZED_STRENGTH = {
    SOURCE_REPO_OWNED_CHECK: contract.ORACLE,
    SOURCE_CLIENT_DECLARED_EXAMPLE: contract.ORACLE,
    SOURCE_CLIENT_DECLARED_VERIFICATION: contract.ORACLE,
    SOURCE_PROXY_OWNED_VALIDATION: contract.SYNTAX,
}

# Identities that must be present and must match exactly. command_identity and
# baseline_identity are handled separately: a syntax obligation runs no
# command, and a candidate that replaces nothing has no baseline, so absence is
# a real answer for both -- but two bindings must still agree on them.
_REQUIRED_IDENTITY = (
    "request_id", "invocation_id", "candidate_instance_id", "candidate_hash",
    "workspace_state_hash", "obligation_id",
)
_BOUND_FIELDS = _REQUIRED_IDENTITY + ("workspace_generation", "command_identity",
                                      "baseline_identity")


class ProvenanceError(ValueError):
    """A binding that cannot be established. Raised at construction: a
    malformed binding must never exist to be compared against."""


def binding(*, request_id: str, invocation_id: str, candidate_instance_id: str,
            candidate_hash: str, workspace_generation: int,
            workspace_state_hash: str, obligation_id: str,
            required_strength: str, source: str, observed_strength: str,
            command_identity: Optional[str] = None,
            baseline_identity: Optional[str] = None) -> Dict[str, Any]:
    """One piece of evidence, with everything needed to say what it is about.

    Every authority-critical value is checked here rather than at use: an
    unknown source or strength that survives construction is one that some
    later reader has to remember to reject.
    """
    if source not in SOURCES:
        raise ProvenanceError(f"unknown evidence source {source!r}")
    for name, value in (("required_strength", required_strength),
                        ("observed_strength", observed_strength)):
        if value not in contract.STRENGTH_ORDER:
            raise ProvenanceError(f"unknown {name} {value!r}")
    values = {
        "request_id": request_id, "invocation_id": invocation_id,
        "candidate_instance_id": candidate_instance_id,
        "candidate_hash": candidate_hash,
        "workspace_state_hash": workspace_state_hash,
        "obligation_id": obligation_id,
    }
    for name in _REQUIRED_IDENTITY:
        v = values[name]
        if not isinstance(v, str) or not v.strip():
            raise ProvenanceError(f"{name} is required and must be a non-empty string")
    # bool is an int subclass; a True generation is a malformed one.
    if isinstance(workspace_generation, bool) or \
            not isinstance(workspace_generation, int) or workspace_generation < 0:
        raise ProvenanceError(
            f"workspace_generation must be a non-negative integer, got "
            f"{workspace_generation!r}")
    for name, value in (("command_identity", command_identity),
                        ("baseline_identity", baseline_identity)):
        if value is not None and (not isinstance(value, str) or not value.strip()):
            raise ProvenanceError(f"{name} must be absent or a non-empty string")
    out = dict(values)
    out["workspace_generation"] = workspace_generation
    out["command_identity"] = command_identity
    out["baseline_identity"] = baseline_identity
    out["required_strength"] = required_strength
    out["observed_strength"] = observed_strength
    out["source"] = source
    return out


def may_authorize(b: Dict[str, Any]) -> Tuple[bool, str]:
    """Whether this evidence COULD authorize its obligation, on source and
    strength alone. Says nothing about which candidate it is about -- that is
    binds_to, and both must hold.

    Not consulted by any delivery decision in this build.
    """
    source = b.get("source")
    if source not in SOURCES:
        return False, f"unknown evidence source {source!r}"
    ceiling = _MAX_AUTHORIZED_STRENGTH.get(source)
    if ceiling is None:
        return False, f"{source} evidence never authorizes an obligation"
    required = b.get("required_strength")
    observed = b.get("observed_strength")
    if required not in contract.STRENGTH_ORDER:
        return False, f"unknown required_strength {required!r}"
    if observed not in contract.STRENGTH_ORDER:
        return False, f"unknown observed_strength {observed!r}"
    order = contract.STRENGTH_ORDER
    if order.index(ceiling) < order.index(required):
        return False, (f"{source} may not establish {required} strength "
                       f"(its ceiling is {ceiling})")
    if order.index(observed) < order.index(required):
        return False, (f"observed strength {observed} is below the required "
                       f"{required}")
    return True, ""


def binds_to(held: Dict[str, Any], asked: Dict[str, Any]) -> Tuple[bool, str]:
    """Whether evidence `held` is about the thing `asked` describes.

    Every identity must match. One candidate may not borrow another's
    evidence, one invocation may not borrow another's, and evidence observed
    against an earlier workspace generation is about a workspace that no
    longer exists -- a later mutation, move or recreation bumps the generation
    and invalidates it.
    """
    for field in _BOUND_FIELDS:
        if held.get(field) != asked.get(field):
            return False, (f"{field} differs: evidence is about "
                           f"{held.get(field)!r}, not {asked.get(field)!r}")
    return True, ""
