"""Extract embeddings from llama-server's /embedding endpoint."""

import base64
import logging
import math
import os
import struct
from typing import Dict, List, Optional, Tuple
from .embed_capacity import (EmbeddingCapacityError, observe_rejection,
                             parse_capacity_rejection)
from .model_transport import ModelServerHTTPError, model_request

logger = logging.getLogger(__name__)


class EmbeddingContractError(RuntimeError):
    """The embed server's response violates the loaded artifact's
    embedding contract (wrong response shape or vector norm). Raised
    instead of silently adapting — the two conventions produce vectors
    from different distributions and only one matches the trained
    weights."""


# Contract of the currently loaded artifacts, installed by
# service._verify_model_identity() at weight load. None = no contract
# declared (legacy artifacts): both response shapes are accepted with a
# warning, matching pre-contract behavior.
_EMBEDDING_CONTRACT: Optional[dict] = None

# One-time-warning latch for a served shape that differs from the shape
# recorded in the contract. A mutable holder rather than a rebound global
# so callers reset it in place.
_shape_warn = {"warned": False}

# llama.cpp's embd_normalize sentinel for "return raw vectors". Pooling
# is applied client-side, and normalizing before pooling would change the
# pooled direction, so every request asks for raw.
_NORMALIZE_NONE = -1


def set_embedding_contract(contract: Optional[dict]) -> None:
    """Install (or clear) the embedding contract enforced by
    extract_embedding(). Called on weight load/reload."""
    global _EMBEDDING_CONTRACT
    _EMBEDDING_CONTRACT = dict(contract) if contract else None
    _shape_warn["warned"] = False


def _l2_norm(vec: List[float]) -> float:
    return math.sqrt(sum(v * v for v in vec))


def _l2_normalize(vec: List[float]) -> List[float]:
    norm = _l2_norm(vec)
    if norm == 0.0:
        raise ValueError("cannot L2-normalize a zero embedding vector")
    return [v / norm for v in vec]


def _reject_prenormalized(per_token: List[List[float]]) -> None:
    """Fail if the server normalized each token before we could pool.

    `embd_normalize: -1` asks for raw vectors. A build that ignores the
    field hands back unit-norm rows, and the mean of those is a different
    direction than the pooled raw vector the artifacts were calibrated
    on — an off-distribution score every health check reports as green
    (the 2026-07-15 bench incident). Raw hidden states are far from unit
    norm, so an all-unit response is unambiguous.
    """
    sample = per_token[: min(8, len(per_token))]
    if sample and all(abs(_l2_norm(t) - 1.0) < 1e-3 for t in sample):
        raise EmbeddingContractError(
            "embed server returned pre-normalized per-token vectors despite "
            "`embd_normalize: -1`; pooling them cannot reproduce the pooled "
            "convention the lens artifacts were calibrated on. Upgrade "
            "llama-server to a build that honors embd_normalize, or serve "
            "with --pooling mean."
        )


def _classify_response(raw) -> str:
    """Classify an /embedding response's shape.

    llama-server encodes pooled embeddings either as a flat list or as a
    single-row nested list ([[...]] — observed on the pinned build with
    --pooling mean), so one nested row still means "pooled". Only a
    multi-row nested response is genuinely per-token. (A one-token input
    under --pooling none is indistinguishable from a pooled row; lens
    inputs are code snippets, never a single token.)
    """
    if bool(raw) and isinstance(raw[0], list) and len(raw) > 1:
        return "per_token"
    return "flat"


def _unwrap(raw) -> List[float]:
    """Return the pooled vector from either encoding (flat or [1][dim])."""
    if bool(raw) and isinstance(raw[0], list):
        return raw[0]
    return raw


def _get_embed_url() -> str:
    """Return the URL for the embedding server.

    Uses LLAMA_EMBED_URL if set, otherwise falls back to LLAMA_URL.
    """
    return os.environ.get(
        "LLAMA_EMBED_URL",
        os.environ.get("LLAMA_URL", "http://llama-server:8080"),
    )


def _post_embedding(text: str, layers: Optional[List[int]] = None,
                    timeout: float = 120,
                    embd_normalize: Optional[int] = None) -> dict:
    """POST to /embedding and return the parsed first item.

    Sends the optional PC-202 `layers` extension when provided, and the
    per-request `embd_normalize` mode when set (llama-server has no
    `--embd-normalize` server flag; normalization is a request field on
    the native endpoint). Returns the raw response dict so callers can
    read both `embedding` and (if layers were requested) `hidden_states`
    + the shape metadata.
    """
    url = f"{_get_embed_url()}/embedding"
    body: Dict = {"content": text}
    if layers:
        body["layers"] = layers
    if embd_normalize is not None:
        body["embd_normalize"] = embd_normalize
    # One transport for every model-bound call: auth and the bound request /
    # invocation identity travel with the embedding request (attribution only).
    try:
        data = model_request(url, body, timeout=timeout)
    except ModelServerHTTPError as exc:
        # llama-server embeds a request in one physical batch and refuses a
        # longer input. That is a transport limit on this deployment, not a
        # property of the text: it is raised as its own type, with the
        # server's counts, and recorded as the capacity the service knows.
        rejection = parse_capacity_rejection(exc.message)
        if rejection is not None:
            input_tokens, capacity_tokens = rejection
            observe_rejection(input_tokens, capacity_tokens)
            raise EmbeddingCapacityError(input_tokens, capacity_tokens,
                                         exc.message) from exc
        raise
    if not isinstance(data, list) or not data:
        raise ValueError(f"unexpected /embedding response shape: {type(data).__name__}")
    return data[0]


def extract_embedding(text: str) -> List[float]:
    """Extract an embedding vector from llama-server.

    Pooling and normalization are both performed here rather than taken
    from the server, so the vector is identical whether llama-server runs
    `--pooling mean` or `--pooling none`. `--pooling` is server-global in
    llama.cpp and the per-step PRM path (extract_per_token) requires
    `none`; deriving the pooled vector client-side lets both paths share
    one server without either changing convention.

    The request asks for unnormalized vectors (`embd_normalize: -1`)
    because normalization does not commute with pooling: the mean of
    per-token unit vectors is not the unit-normalized mean of the raw
    vectors. Pooling raw and normalizing after reproduces the pooled +
    L2 convention the artifacts were calibrated on.

    Returns:
        List of floats with model-native dimensionality.
    """
    contract = _EMBEDDING_CONTRACT
    item = _post_embedding(text, embd_normalize=_NORMALIZE_NONE)
    raw = item["embedding"]
    got = _classify_response(raw)
    is_per_token = got == "per_token"

    if contract:
        expected = contract.get("response_shape", "flat")
        if got != expected and not _shape_warn["warned"]:
            _shape_warn["warned"] = True
            logger.info(
                "embed server returned a %s /embedding response; the loaded "
                "artifacts declare %s. Pooling and normalization are applied "
                "client-side, so both shapes yield the same vector.",
                got, expected,
            )

    if is_per_token:
        # Per-token: mean-pool across tokens
        per_token = raw
        n_tokens = len(per_token)

        if n_tokens == 0:
            raise ValueError("No token embeddings returned")

        _reject_prenormalized(per_token)

        dim = len(per_token[0])

        pooled = [0.0] * dim
        for tok_emb in per_token:
            for i, v in enumerate(tok_emb):
                pooled[i] += v
        for i in range(dim):
            pooled[i] /= n_tokens
        vec = pooled
    else:
        vec = _unwrap(raw)

    # Scale is load-bearing, so it is not normalized away by default: the
    # shipped cost field is fitted on unnormalized mean-pooled vectors
    # (‖v‖≈137 in training_embeddings_3840d.json) and scores them across
    # the calibrated 4.8-14.2 band. The same vectors L2-normalized score
    # 0.78-0.80 — one flat value with no separation between pass and
    # fail. A contract may declare artifacts that were trained normalized.
    if contract and contract.get("normalized"):
        vec = _l2_normalize(vec)

    return vec


def observe_embedding_convention(
        text: str = "def add(a, b):\n    return a + b") -> dict:
    """Probe the embed server once and describe the convention it serves.

    Used at training time to record what the training embeddings were
    actually built from, so the resulting artifacts carry an accurate
    embedding_contract.
    """
    item = _post_embedding(text)
    raw = item["embedding"]
    if _classify_response(raw) == "per_token":
        return {"pooling": "none", "response_shape": "per_token",
                "normalized": False, "norm_tolerance": 0.05}
    norm = _l2_norm(_unwrap(raw))
    return {"pooling": "mean", "response_shape": "flat",
            "normalized": abs(norm - 1.0) <= 0.05,
            "norm_tolerance": 0.05}


def extract_per_token(text: str) -> Tuple[List[List[float]], int]:
    """Extract per-token last-layer hidden states from /embedding.

    Used by PC-207 lens-as-PRM to score each generation step instead of only
    pooled completed text. Works against vanilla llama-server (no PC-202 patch
    required) when the server uses `--pooling none`; if it is configured with
    another pooling mode we detect the pooled-flat shape and raise rather than
    silently degrade.

    Returns:
        (per_token_vectors, hidden_dim) — outer list is one entry per input
        token, inner list is the hidden_dim float vector at the last layer.
    """
    item = _post_embedding(text, embd_normalize=_NORMALIZE_NONE)
    raw = item["embedding"]
    # A flat response is pooled; so is a single nested row (the pooled
    # encoding under --pooling mean). Genuine per-token output for a code
    # snippet always has multiple rows.
    if _classify_response(raw) != "per_token":
        raise ValueError(
            "extract_per_token needs per-token embeddings; "
            "llama-server appears to be pooling. Start it with --pooling none."
        )
    # Returned at native scale. C(x) is fitted on unnormalized pooled
    # vectors, and a pooled vector carries the scale of the tokens it
    # averages, so raw per-token vectors are the closest match to the
    # calibrated distribution.
    return raw, len(raw[0])


def extract_per_layer_per_token(text: str, layers: List[int]) -> Tuple[Dict[int, List[List[float]]], int, int]:
    """Extract per-token residual hidden states at the requested layers.

    Uses the PC-202 `/embedding` extension (`layers: [int]`). The server
    must have been built with `inference/patches/expose-hidden-states.patch`
    applied; on an unpatched server the `layers` field is silently ignored
    and only the standard `embedding` field comes back, which we detect
    and raise on.

    Returns:
        (per_layer_dict, n_tokens, hidden_dim). Each layer's value is a
        list of per-token vectors (length n_tokens, each of len hidden_dim).
    """
    if not layers:
        raise ValueError("layers must be a non-empty list of layer indices")
    item = _post_embedding(text, layers=layers)
    if "hidden_states" not in item:
        raise RuntimeError(
            "/embedding response missing `hidden_states`; "
            "llama-server is likely missing the PC-202 hidden-states patch. "
            "Rebuild atlas-llama-server with inference/Dockerfile.v31."
        )
    n_tokens = int(item["hidden_states_n_tokens"])
    hidden_dim = int(item["hidden_states_dim"])
    out: Dict[int, List[List[float]]] = {}
    for layer_str, b64 in item["hidden_states"].items():
        raw = base64.b64decode(b64)
        n_floats = len(raw) // 4
        if n_floats != n_tokens * hidden_dim:
            raise ValueError(
                f"layer {layer_str}: decoded {n_floats} floats, "
                f"expected {n_tokens}*{hidden_dim}={n_tokens*hidden_dim}"
            )
        flat = struct.unpack(f"<{n_floats}f", raw)
        # reshape [n_tokens, hidden_dim]
        rows: List[List[float]] = [
            list(flat[i * hidden_dim : (i + 1) * hidden_dim]) for i in range(n_tokens)
        ]
        out[int(layer_str)] = rows
    return out, n_tokens, hidden_dim
