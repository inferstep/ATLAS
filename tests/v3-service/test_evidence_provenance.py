"""Where evidence came from, and what it is bound to.

The contract already records how STRONG a verifier was. It does not record
who the verifier was, so a model-generated test that runs and a repository
test that runs produce the same `behavioral` record. Strength is not trust:
the model that wrote the code also wrote the test, and on the captured pool 21
of 36 generated keys disagreed with the task's own reference.

Two things are added here and neither authorizes anything yet:

  source    a closed, source-SPECIFIC vocabulary. Not `trusted=true`: a
            boolean cannot say that a proxy syntax gate may close a syntax
            obligation and may not close a behavioural one.
  binding   the identities a piece of evidence is about. Evidence that cannot
            name its candidate, request, invocation, command, obligation and
            workspace generation cannot be shown to be about them.

The hidden benchmark evaluator is absent by construction. There is no constant
for it, no constructor that can produce one, and a test below fails if one
appears.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import provenance as P  # noqa: E402
import contract as C  # noqa: E402


def binding(**over):
    base = dict(
        request_id="req-1", invocation_id="inv-1",
        candidate_instance_id="inv-1:generated:0",
        candidate_hash="c" * 64,
        workspace_generation=7, workspace_state_hash="w" * 64,
        command_identity="python3 solve.py",
        obligation_id="oracle_cases_pass", required_strength=C.ORACLE,
        source=P.SOURCE_REPO_OWNED_CHECK, observed_strength=C.ORACLE,
    )
    base.update(over)
    return P.binding(**base)


# --- the vocabulary is closed and source-specific ---------------------------

def test_sources_are_source_specific_not_a_trusted_boolean():
    assert P.SOURCES == (
        P.SOURCE_REPO_OWNED_CHECK,
        P.SOURCE_CLIENT_DECLARED_EXAMPLE,
        P.SOURCE_CLIENT_DECLARED_VERIFICATION,
        P.SOURCE_PROXY_OWNED_VALIDATION,
        P.SOURCE_MODEL_GENERATED,
        P.SOURCE_LEGACY,
        P.SOURCE_UNKNOWN,
    )
    assert not hasattr(P, "TRUSTED"), "a boolean cannot carry source-specific authority"


def test_the_hidden_evaluator_is_structurally_absent():
    """Not merely unused -- unrepresentable."""
    for name in dir(P):
        low = name.lower()
        for banned in ("hidden", "holdout", "benchmark", "oracle_answer", "evaluator"):
            assert banned not in low, f"{name} names an evaluator source"
    with pytest.raises(P.ProvenanceError):
        binding(source="hidden_evaluator")


def test_an_unknown_source_fails_closed():
    for bad in ("", None, "trusted", "repo", 7, "REPO_OWNED_CHECK"):
        with pytest.raises(P.ProvenanceError):
            binding(source=bad)


def test_an_unknown_strength_fails_closed():
    for bad in ("", None, "strong", "ORACLE", 3):
        with pytest.raises(P.ProvenanceError):
            binding(required_strength=bad)
        with pytest.raises(P.ProvenanceError):
            binding(observed_strength=bad)


# --- what may authorize what ------------------------------------------------

def test_model_generated_is_recorded_but_never_authorizing():
    b = binding(source=P.SOURCE_MODEL_GENERATED, observed_strength=C.ORACLE,
                required_strength=C.ORACLE)
    assert b["source"] == P.SOURCE_MODEL_GENERATED     # recorded
    ok, why = P.may_authorize(b)
    assert not ok and "model_generated" in why


def test_legacy_is_readable_but_never_authorizing():
    b = binding(source=P.SOURCE_LEGACY, observed_strength=C.ORACLE)
    assert b["source"] == P.SOURCE_LEGACY
    ok, why = P.may_authorize(b)
    assert not ok and "legacy" in why


def test_unknown_never_authorizes():
    ok, why = P.may_authorize(dict(binding(), source=P.SOURCE_UNKNOWN))
    assert not ok and "unknown" in why


def test_proxy_validation_may_close_syntax_but_not_behaviour():
    ok, _ = P.may_authorize(binding(
        source=P.SOURCE_PROXY_OWNED_VALIDATION,
        obligation_id="parses", required_strength=C.SYNTAX,
        observed_strength=C.SYNTAX))
    assert ok
    ok, why = P.may_authorize(binding(
        source=P.SOURCE_PROXY_OWNED_VALIDATION,
        obligation_id="behaves", required_strength=C.BEHAVIORAL,
        observed_strength=C.SYNTAX))
    assert not ok and "strength" in why


def test_a_capable_source_still_needs_the_observed_strength():
    """Capability is an upper bound. A repo test that only compiled did not
    demonstrate behaviour merely because a repo test could have."""
    ok, why = P.may_authorize(binding(
        source=P.SOURCE_REPO_OWNED_CHECK,
        required_strength=C.ORACLE, observed_strength=C.SYNTAX))
    assert not ok and "strength" in why


def test_client_declared_sources_may_reach_behavioural_closure():
    for src in (P.SOURCE_CLIENT_DECLARED_EXAMPLE,
                P.SOURCE_CLIENT_DECLARED_VERIFICATION,
                P.SOURCE_REPO_OWNED_CHECK):
        ok, why = P.may_authorize(binding(source=src,
                                          required_strength=C.BEHAVIORAL,
                                          observed_strength=C.BEHAVIORAL))
        assert ok, (src, why)


# --- binding: evidence cannot cross an identity -----------------------------

@pytest.mark.parametrize("field,other", [
    ("request_id", "req-2"),
    ("invocation_id", "inv-2"),
    ("candidate_instance_id", "inv-1:generated:1"),
    ("candidate_hash", "d" * 64),
    ("command_identity", "python3 other.py"),
    ("baseline_identity", "syntax:some-other-baseline"),
    ("obligation_id", "something_else"),
    ("workspace_generation", 8),
    ("workspace_state_hash", "z" * 64),
])
def test_evidence_cannot_cross_an_identity(field, other):
    held = binding()
    asked = dict(held, **{field: other})
    ok, why = P.binds_to(held, asked)
    assert not ok, f"{field} was allowed to differ"
    assert field in why


def test_identical_identity_binds():
    ok, why = P.binds_to(binding(), binding())
    assert ok, why


def test_a_stale_generation_fails_closed():
    held = binding(workspace_generation=7)
    asked = dict(held, workspace_generation=9)   # a mutation happened since
    ok, why = P.binds_to(held, asked)
    assert not ok and "workspace_generation" in why


def test_a_missing_identity_component_fails_closed():
    for field in ("request_id", "invocation_id", "candidate_instance_id",
                  "candidate_hash", "workspace_state_hash", "obligation_id"):
        with pytest.raises(P.ProvenanceError):
            binding(**{field: ""})


def test_command_identity_is_optional_but_binding_when_present():
    """A syntax obligation runs no command; a verification obligation does."""
    b = binding(command_identity=None)
    ok, _ = P.binds_to(b, dict(b))
    assert ok
    ok, why = P.binds_to(b, dict(b, command_identity="python3 solve.py"))
    assert not ok and "command_identity" in why


def test_baseline_identity_is_optional_but_binding_when_present():
    """A candidate that replaces nothing has no baseline. One that replaces a
    validated artifact is about THAT artifact, and evidence earned against a
    different baseline is about a different replacement."""
    b = binding(baseline_identity=None)
    ok, _ = P.binds_to(b, dict(b))
    assert ok
    ok, why = P.binds_to(b, dict(b, baseline_identity="syntax:abc"))
    assert not ok and "baseline_identity" in why
    held = binding(baseline_identity="syntax:abc")
    ok, why = P.binds_to(held, dict(held, baseline_identity="syntax:def"))
    assert not ok and "baseline_identity" in why


def test_an_empty_baseline_identity_is_refused():
    """Absent and blank are different: a blank one would compare equal to
    another blank one and let two unrelated replacements share evidence."""
    with pytest.raises(P.ProvenanceError):
        binding(baseline_identity="   ")


def test_workspace_generation_must_be_a_non_negative_integer():
    for bad in (-1, "7", None, 1.5, True):
        with pytest.raises(P.ProvenanceError):
            binding(workspace_generation=bad)


# --- no bytes anywhere ------------------------------------------------------

def test_a_binding_carries_no_candidate_bytes():
    b = binding()
    for k, v in b.items():
        assert "def " not in str(v) and "import " not in str(v), k
    assert "candidate_code" not in b and "code" not in b


# --- inert: nothing consumes this yet ---------------------------------------

def test_nothing_in_production_consumes_provenance_yet():
    """This commit adds the vocabulary and the checks. Wiring them into
    authorization is the next slice, and doing it early would change delivery."""
    import ast
    root = Path(__file__).resolve().parents[2] / "v3-service"
    consumers = []
    for path in sorted(root.rglob("*.py")):
        if path.name in ("provenance.py",) or "test" in path.name:
            continue
        tree = ast.parse(path.read_text(encoding="utf-8"))
        for node in ast.walk(tree):
            if isinstance(node, (ast.Import, ast.ImportFrom)):
                names = [a.name for a in node.names] + [getattr(node, "module", "") or ""]
                if any("provenance" == n for n in names):
                    consumers.append(path.name)
    assert not consumers, (
        f"provenance is consumed by {consumers}; this commit must not change "
        "any live authorization or delivery decision")
