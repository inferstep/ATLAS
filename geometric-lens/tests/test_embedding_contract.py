"""Embedding-contract enforcement in extract_embedding().

Regression for the 2026-07-15 bench incident: a rebuilt embed server
returned per-token unnormalized embeddings, extract_embedding() silently
mean-pooled them (‖v‖≈60 vs the trained ~1), and C(x) served ~600
against a calibrated ~20-30 with every health check green. The contract
makes the wrong convention a hard error instead of a silent one.
"""

import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from geometric_lens import embedding_extractor as ee
from geometric_lens.identity import (
    validate_embedding_contract,
    validate_model_identity,
)


@pytest.fixture(autouse=True)
def _clear_contract():
    """Every test starts with no contract installed and restores after."""
    ee.set_embedding_contract(None)
    yield
    ee.set_embedding_contract(None)


def _flat(dim=8, norm=1.0):
    """A flat embedding with a chosen L2 norm."""
    import math
    base = [1.0] * dim
    scale = norm / math.sqrt(dim)
    return [v * scale for v in base]


def _install_response(monkeypatch, embedding):
    monkeypatch.setattr(ee, "_post_embedding",
                        lambda *a, **kw: {"embedding": embedding})


# --- contract validation ---------------------------------------------------

def test_validate_embedding_contract_defaults_and_bounds():
    c = validate_embedding_contract({})
    assert c == {"pooling": "mean", "response_shape": "flat",
                 "normalized": False, "norm_tolerance": 0.05}
    for bad in ({"pooling": "sum"}, {"response_shape": "matrix"},
                {"normalized": "yes"}, {"norm_tolerance": 0},
                {"norm_tolerance": 1.5}):
        with pytest.raises(ValueError):
            validate_embedding_contract(bad)


def test_model_identity_carries_optional_contract():
    ident = validate_model_identity({
        "model": "m", "embedding_dim": 8,
        "embedding_contract": {"normalized": True, "response_shape": "flat"},
    })
    assert ident["embedding_contract"]["normalized"] is True
    # Absent contract stays absent (backward compatible).
    assert "embedding_contract" not in validate_model_identity(
        {"model": "m", "embedding_dim": 8})


# --- enforcement -----------------------------------------------------------

def test_flat_normalized_response_passes_contract(monkeypatch):
    ee.set_embedding_contract({"pooling": "mean", "response_shape": "flat",
                               "normalized": True, "norm_tolerance": 0.05})
    _install_response(monkeypatch, _flat(norm=1.0))
    out = ee.extract_embedding("x")
    assert len(out) == 8


def test_pooling_mode_does_not_change_the_vector(monkeypatch):
    """--pooling is server-global, and the per-step PRM path needs
    `none` while whole-text C(x) was calibrated under `mean`. Pooling
    client-side has to make the served mode invisible: the same text must
    yield the same vector either way, or one of the two paths is scored
    off-distribution."""
    ee.set_embedding_contract({"pooling": "mean", "response_shape": "flat",
                               "normalized": True, "norm_tolerance": 0.05})
    per_token = [[1.0, 2.0, 3.0, 4.0], [3.0, 4.0, 5.0, 8.0]]
    pooled = [2.0, 3.0, 4.0, 6.0]  # what --pooling mean returns for it

    _install_response(monkeypatch, per_token)
    from_none = ee.extract_embedding("x")
    _install_response(monkeypatch, pooled)
    from_mean = ee.extract_embedding("x")

    assert from_none == pytest.approx(from_mean)


def test_vector_scale_is_preserved(monkeypatch):
    """C(x) is fitted on unnormalized pooled vectors (‖v‖≈137) and scores
    them across a 4.8-14.2 band; the same vectors normalized score a flat
    0.78-0.80 with no pass/fail separation. Scale is signal, so an
    undeclared contract must not normalize it away."""
    _install_response(monkeypatch, _flat(norm=60.0))
    assert ee._l2_norm(ee.extract_embedding("x")) == pytest.approx(60.0)

    _install_response(monkeypatch, [[1.0, 2.0], [3.0, 4.0]])
    assert ee.extract_embedding("x") == pytest.approx([2.0, 3.0])


def test_normalized_contract_still_normalizes(monkeypatch):
    """Artifacts that declare they were trained on unit vectors get them."""
    ee.set_embedding_contract({"pooling": "mean", "response_shape": "flat",
                               "normalized": True, "norm_tolerance": 0.05})
    _install_response(monkeypatch, _flat(norm=60.0))
    assert ee._l2_norm(ee.extract_embedding("x")) == pytest.approx(1.0)


def test_prenormalized_per_token_response_raises(monkeypatch):
    """A build that ignores `embd_normalize: -1` hands back unit-norm
    rows. Their mean points somewhere else than the pooled raw vector, so
    this must fail loudly rather than score off-distribution."""
    _install_response(monkeypatch, [_flat(norm=1.0), _flat(dim=8, norm=1.0)])
    with pytest.raises(ee.EmbeddingContractError) as exc:
        ee.extract_embedding("x")
    assert "embd_normalize" in str(exc.value)


def test_single_row_nested_is_pooled_not_per_token(monkeypatch):
    """The pinned llama-server encodes pooled embeddings as [[...]] (one
    nested row) under --pooling mean. That must satisfy a flat contract
    and unwrap to the vector — not raise as per-token."""
    ee.set_embedding_contract({"pooling": "mean", "response_shape": "flat",
                               "normalized": True, "norm_tolerance": 0.05})
    _install_response(monkeypatch, [_flat(norm=1.0)])
    out = ee.extract_embedding("x")
    assert len(out) == 8 and not isinstance(out[0], list)


def test_requests_raw_vectors_regardless_of_contract(monkeypatch):
    """Normalization does not commute with pooling, so every request asks
    for raw vectors and normalizes after pooling. Asking the server to
    normalize first would leave the per-token path pooling unit vectors."""
    seen = {}

    def _spy(text, layers=None, timeout=120, embd_normalize=None):
        seen["embd_normalize"] = embd_normalize
        return {"embedding": _flat(norm=3.0)}

    monkeypatch.setattr(ee, "_post_embedding", _spy)
    ee.set_embedding_contract({"pooling": "mean", "response_shape": "flat",
                               "normalized": True, "norm_tolerance": 0.05})
    ee.extract_embedding("x")
    assert seen["embd_normalize"] == -1

    ee.set_embedding_contract(None)
    ee.extract_embedding("x")
    assert seen["embd_normalize"] == -1


def test_extract_per_token_rejects_pooled_encodings(monkeypatch):
    _install_response(monkeypatch, _flat(norm=1.0))
    with pytest.raises(ValueError):
        ee.extract_per_token("x")
    _install_response(monkeypatch, [_flat(norm=1.0)])  # single nested row
    with pytest.raises(ValueError):
        ee.extract_per_token("x")
    _install_response(monkeypatch, [[1.0, 2.0], [3.0, 4.0]])
    vecs, dim = ee.extract_per_token("x")
    assert len(vecs) == 2 and dim == 2
    # Native scale, matching the unnormalized vectors C(x) is fitted on.
    assert [list(v) for v in vecs] == [[1.0, 2.0], [3.0, 4.0]]


def test_observe_convention_reports_flat_normalized(monkeypatch):
    _install_response(monkeypatch, _flat(norm=1.0))
    c = ee.observe_embedding_convention("x")
    assert c["response_shape"] == "flat" and c["normalized"] is True

    _install_response(monkeypatch, [_flat(norm=5.0), _flat(norm=5.0)])
    c = ee.observe_embedding_convention("x")
    assert c["response_shape"] == "per_token" and c["pooling"] == "none"


def test_observed_convention_round_trips_into_an_identity(monkeypatch):
    """`atlas lens build` records what it observed so the artifacts declare
    the convention they were fitted on.

    save_model_identity has always accepted an embedding_contract and the
    builder never passed one, so shipped artifacts declare none — which is
    what lets a server-side pooling or normalization change shift every
    score with health checks green. Measured 2026-08-03: unit-norm vectors
    against a cost field fitted on |v|~137 scored 0.76-0.80 for a clean
    function, a repetition loop and truncated junk alike.
    """
    from geometric_lens.identity import (
        save_model_identity, load_model_identity,
    )
    import tempfile

    _install_response(monkeypatch, _flat(norm=137.0))
    observed = ee.observe_embedding_convention("def f(): pass")
    assert observed["normalized"] is False, "|v|=137 is not unit-norm"

    with tempfile.TemporaryDirectory() as d:
        save_model_identity(d, "gemma-4-12b-it-Q4_K_M", 3840,
                            embedding_contract=observed)
        back = load_model_identity(d)
    assert back["embedding_contract"]["normalized"] is False
    assert back["embedding_contract"]["response_shape"] == "flat"
