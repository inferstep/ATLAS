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

import provenance as P
import contract as C


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


@pytest.mark.parametrize("field, bad", [
    pytest.param("source", "", id="empty source"),
    pytest.param("source", None, id="no source"),
    pytest.param("source", "trusted", id="source trusted"),
    pytest.param("source", "repo", id="source repo"),
    pytest.param("source", 7, id="source 7"),
    pytest.param("source", "REPO_OWNED_CHECK", id="source in capitals"),
    pytest.param("required_strength", "", id="empty required strength"),
    pytest.param("required_strength", None, id="no required strength"),
    pytest.param("required_strength", "strong", id="required strength strong"),
    pytest.param("required_strength", "ORACLE", id="required strength in capitals"),
    pytest.param("required_strength", 3, id="required strength 3"),
    pytest.param("observed_strength", "", id="empty observed strength"),
    pytest.param("observed_strength", None, id="no observed strength"),
    pytest.param("observed_strength", "strong", id="observed strength strong"),
    pytest.param("observed_strength", "ORACLE", id="observed strength in capitals"),
    pytest.param("observed_strength", 3, id="observed strength 3"),
    pytest.param("request_id", "", id="empty request_id"),
    pytest.param("invocation_id", "", id="empty invocation_id"),
    pytest.param("candidate_instance_id", "", id="empty candidate_instance_id"),
    pytest.param("candidate_hash", "", id="empty candidate_hash"),
    pytest.param("workspace_state_hash", "", id="empty workspace_state_hash"),
    pytest.param("obligation_id", "", id="empty obligation_id"),
    # The workspace generation must be a non-negative integer.
    pytest.param("workspace_generation", -1, id="generation -1"),
    pytest.param("workspace_generation", "7", id="generation as a string"),
    pytest.param("workspace_generation", None, id="no generation"),
    pytest.param("workspace_generation", 1.5, id="generation 1.5"),
    pytest.param("workspace_generation", True, id="generation True"),
    # Absent and blank are different: a blank baseline would compare equal to
    # another blank one and let two unrelated replacements share evidence.
    pytest.param("baseline_identity", "   ", id="blank baseline_identity"),
])
def test_a_malformed_binding_is_refused(field, bad):
    with pytest.raises(P.ProvenanceError):
        binding(**{field: bad})


# --- what may authorize what ------------------------------------------------

@pytest.mark.parametrize("source, required, observed", [
    pytest.param(P.SOURCE_PROXY_OWNED_VALIDATION, C.SYNTAX, C.SYNTAX,
                 id="proxy validation closes syntax"),
    pytest.param(P.SOURCE_CLIENT_DECLARED_EXAMPLE, C.BEHAVIORAL, C.BEHAVIORAL,
                 id="client example closes behaviour"),
    pytest.param(P.SOURCE_CLIENT_DECLARED_VERIFICATION, C.BEHAVIORAL, C.BEHAVIORAL,
                 id="client verification closes behaviour"),
    pytest.param(P.SOURCE_REPO_OWNED_CHECK, C.BEHAVIORAL, C.BEHAVIORAL,
                 id="repo check closes behaviour"),
])
def test_evidence_may_authorize_its_obligation(source, required, observed):
    ok, why = P.may_authorize(binding(source=source, required_strength=required,
                                      observed_strength=observed))
    assert ok, why


@pytest.mark.parametrize("source, required, observed, reason", [
    pytest.param(P.SOURCE_MODEL_GENERATED, C.ORACLE, C.ORACLE, "model_generated",
                 id="model generated never authorizes"),
    pytest.param(P.SOURCE_LEGACY, C.ORACLE, C.ORACLE, "legacy",
                 id="legacy never authorizes"),
    pytest.param(P.SOURCE_UNKNOWN, C.ORACLE, C.ORACLE, "unknown",
                 id="unknown never authorizes"),
    pytest.param(P.SOURCE_PROXY_OWNED_VALIDATION, C.BEHAVIORAL, C.SYNTAX, "strength",
                 id="proxy validation cannot close behaviour"),
    # Capability is an upper bound. A repo test that only compiled did not
    # demonstrate behaviour merely because a repo test could have.
    pytest.param(P.SOURCE_REPO_OWNED_CHECK, C.ORACLE, C.SYNTAX, "strength",
                 id="repo check that observed only syntax"),
])
def test_evidence_may_not_authorize_its_obligation(source, required, observed, reason):
    b = binding(source=source, required_strength=required, observed_strength=observed)
    assert b["source"] == source    # recorded all the same
    ok, why = P.may_authorize(b)
    assert not ok
    assert reason in why


# --- binding: evidence cannot cross an identity -----------------------------

@pytest.mark.parametrize("field, held, asked", [
    pytest.param("request_id", "req-1", "req-2", id="request"),
    pytest.param("invocation_id", "inv-1", "inv-2", id="invocation"),
    pytest.param("candidate_instance_id", "inv-1:generated:0", "inv-1:generated:1",
                 id="candidate instance"),
    pytest.param("candidate_hash", "c" * 64, "d" * 64, id="candidate hash"),
    pytest.param("obligation_id", "oracle_cases_pass", "something_else", id="obligation"),
    # A mutation since the evidence was observed bumps the generation.
    pytest.param("workspace_generation", 7, 8, id="stale workspace generation"),
    pytest.param("workspace_state_hash", "w" * 64, "z" * 64, id="workspace state"),
    # A syntax obligation runs no command; a verification obligation does.
    pytest.param("command_identity", "python3 solve.py", "python3 other.py", id="command"),
    pytest.param("command_identity", None, "python3 solve.py", id="command absent, then present"),
    # A candidate that replaces nothing has no baseline. One that replaces a
    # validated artifact is about THAT artifact, and evidence earned against a
    # different baseline is about a different replacement.
    pytest.param("baseline_identity", None, "syntax:abc", id="baseline absent, then present"),
    pytest.param("baseline_identity", "syntax:abc", "syntax:def", id="baseline"),
])
def test_evidence_cannot_cross_an_identity(field, held, asked):
    evidence = binding(**{field: held})
    ok, why = P.binds_to(evidence, dict(evidence, **{field: asked}))
    assert not ok, f"{field} was allowed to differ"
    assert field in why


def test_identical_identity_binds():
    ok, why = P.binds_to(binding(), binding())
    assert ok, why
    # Command and baseline identities may be absent; absent on both sides binds.
    absent = binding(command_identity=None, baseline_identity=None)
    ok, why = P.binds_to(absent, dict(absent))
    assert ok, why


# --- no bytes anywhere ------------------------------------------------------

def test_a_binding_carries_no_candidate_bytes():
    b = binding()
    for k, v in b.items():
        assert "def " not in str(v), k
        assert "import " not in str(v), k
    assert "candidate_code" not in b
    assert "code" not in b


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
