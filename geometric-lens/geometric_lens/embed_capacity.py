"""The /embedding capacity boundary: a transport limit, kept apart from scores.

llama-server processes one embedding request in a single physical batch
(`-ub`, n_ubatch) and refuses a longer input with HTTP 500:

    input (2055 tokens) is too large to process. increase the physical
    batch size (current batch size: 2048)

Every Lens score is one forward over the whole sequence. An input past the
limit cannot be scored on the calibrated convention: splitting it would
embed later pieces without the context of earlier ones and pool a vector the
artifacts were never fitted on. So the refusal is reported as a typed
failure carrying the two numbers the server gave, and is never turned into
a number that reads like a verdict (energy 0.0, gx 0.5, an empty aggregate).

This module owns the parsing of that refusal, the exception type, the
classification of every other reason a score did not happen, and the
capacity the service reports on /health and /ready: declared by the
deployment through LLAMA_EMBED_CAPACITY_TOKENS (compose passes ATLAS_UBATCH,
the value llama-server itself runs with), or observed from a refusal, which
is authoritative and replaces the declaration.
"""
import math
import numbers
import os
import re
import threading
import urllib.error
from typing import Optional, Tuple

DECLARED_ENV = "LLAMA_EMBED_CAPACITY_TOKENS"

KIND_CAPACITY = "embed_capacity"
KIND_SERVER_ERROR = "model_server_error"
KIND_UNREACHABLE = "model_server_unreachable"
KIND_CONTRACT = "embedding_contract"
KIND_EMPTY = "empty_input"
KIND_NONFINITE = "nonfinite_score"
KIND_INTERNAL = "internal"

_TOO_LARGE = re.compile(r"too large to process", re.IGNORECASE)
_INPUT_TOKENS = re.compile(r"input \((\d+) tokens\)")
_BATCH_SIZE = re.compile(r"batch size: (\d+)")
_DETAIL_LIMIT = 200


def parse_capacity_rejection(message) -> Optional[Tuple[Optional[int], Optional[int]]]:
    """(input_tokens, capacity_tokens) when `message` is llama-server's
    physical-batch refusal, None when it is any other message. Older
    builds word the refusal without numbers; both fields are then None."""
    if not isinstance(message, str) or not _TOO_LARGE.search(message):
        return None
    m_in = _INPUT_TOKENS.search(message)
    m_cap = _BATCH_SIZE.search(message)
    return (int(m_in.group(1)) if m_in else None,
            int(m_cap.group(1)) if m_cap else None)


def _one_line(text, limit: int = _DETAIL_LIMIT) -> str:
    """Server text for a failure record: whitespace collapsed, control
    characters dropped, bounded. Candidate bytes never enter here."""
    s = " ".join(str(text or "").split())
    s = "".join(c for c in s if 0x20 <= ord(c) < 0x7f or ord(c) > 0x9f)
    return s[:limit]


class EmbeddingCapacityError(RuntimeError):
    """The model server refused the embedding: the input exceeds the physical
    batch. `input_tokens` and `capacity_tokens` are the server's own counts
    (None on builds that do not report them)."""

    def __init__(self, input_tokens: Optional[int], capacity_tokens: Optional[int],
                 message: str = ""):
        self.input_tokens = input_tokens
        self.capacity_tokens = capacity_tokens
        self.message = _one_line(message)
        super().__init__(
            f"input of {input_tokens if input_tokens is not None else 'unknown'} "
            f"tokens exceeds the /embedding physical batch of "
            f"{capacity_tokens if capacity_tokens is not None else 'unknown'} tokens")


class NonFiniteScoreError(ValueError):
    """A score came out NaN or infinite. Such a value is not a judgment: it
    sorts arbitrarily, ties with everything, and json.dumps emits it as a
    token standard JSON does not have. `field` names which score."""

    def __init__(self, field: str, value):
        self.field = field
        self.value = value
        super().__init__(f"{field} is not a finite number: {value!r}")


def finite(value, field: str) -> float:
    """`value` as a float when it is a finite number, else NonFiniteScoreError.
    Every score the service reports passes through here, so the guard has
    one owner and a degenerate forward is typed, never numbered."""
    if isinstance(value, bool) or not isinstance(value, numbers.Real):
        raise NonFiniteScoreError(field, value)
    out = float(value)
    if not math.isfinite(out):
        raise NonFiniteScoreError(field, value)
    return out


def finite_array(values, field: str):
    """`values` (a numpy array) when every element is finite, else
    NonFiniteScoreError naming the field: one bad token unscores the text,
    since an aggregate over it would carry the bad value."""
    import numpy as np
    arr = np.asarray(values, dtype=float)
    if not np.all(np.isfinite(arr)):
        bad = arr[~np.isfinite(arr)]
        raise NonFiniteScoreError(field, float(bad[0]))
    return arr


# --- the capacity the service knows ------------------------------------------------

_lock = threading.Lock()
_observed = {"capacity": None, "rejections": 0, "max_rejected": None}


def declared_capacity() -> Optional[int]:
    """The physical batch the deployment declares, or None. Anything that is
    not a positive integer is ignored: a wrong declaration is worse than none."""
    raw = os.environ.get(DECLARED_ENV, "").strip()
    if not raw.isdigit():
        return None
    value = int(raw)
    return value if value > 0 else None


def observe_rejection(input_tokens: Optional[int], capacity_tokens: Optional[int]) -> None:
    """Record one physical-batch refusal. The server's stated capacity is the
    ground truth and replaces any declaration."""
    with _lock:
        _observed["rejections"] += 1
        if capacity_tokens is not None:
            _observed["capacity"] = int(capacity_tokens)
        if input_tokens is not None:
            prev = _observed["max_rejected"]
            _observed["max_rejected"] = (int(input_tokens) if prev is None
                                         else max(prev, int(input_tokens)))


def reset() -> None:
    with _lock:
        _observed.update({"capacity": None, "rejections": 0, "max_rejected": None})


def snapshot() -> dict:
    """The capacity contract as /health and /ready report it."""
    with _lock:
        observed = dict(_observed)
    if observed["capacity"] is not None:
        capacity, source = observed["capacity"], "observed"
    else:
        capacity = declared_capacity()
        source = "declared" if capacity is not None else None
    return {
        "embed_capacity_tokens": capacity,
        "embed_capacity_source": source,
        "embed_capacity_rejections": observed["rejections"],
        "embed_capacity_max_rejected_tokens": observed["max_rejected"],
    }


# --- why a score did not happen ------------------------------------------------------

def failure_from_exception(exc: BaseException) -> dict:
    """A typed, number-free record of why an embedding was not scored.

    Kinds:
      embed_capacity           the input exceeds the physical batch (with counts)
      model_server_error       the model server answered an HTTP error
      model_server_unreachable no answer at all (refused, timed out, reset)
      embedding_contract       the answer violated the artifact's convention
      nonfinite_score          a score came out NaN or infinite (with the field)
      internal                 anything else; the service log has the traceback
    """
    from .embedding_extractor import EmbeddingContractError
    from .model_transport import ModelServerHTTPError

    if isinstance(exc, EmbeddingCapacityError):
        return {"kind": KIND_CAPACITY,
                "input_tokens": exc.input_tokens,
                "capacity_tokens": exc.capacity_tokens,
                "detail": exc.message}
    if isinstance(exc, ModelServerHTTPError):
        return {"kind": KIND_SERVER_ERROR, "status": exc.status,
                "detail": _one_line(exc.message)}
    if isinstance(exc, EmbeddingContractError):
        return {"kind": KIND_CONTRACT, "detail": type(exc).__name__}
    if isinstance(exc, NonFiniteScoreError):
        return {"kind": KIND_NONFINITE, "field": exc.field,
                "detail": _one_line(repr(exc.value))}
    if isinstance(exc, (urllib.error.URLError, OSError, TimeoutError, ConnectionError)):
        return {"kind": KIND_UNREACHABLE, "detail": type(exc).__name__}
    return {"kind": KIND_INTERNAL, "detail": type(exc).__name__}
