"""The one outbound transport for model-bound Lens HTTP calls.

Every request the Lens makes to the model server (embeddings, the served-model
identity probe) goes through here, so the attribution headers cannot drift
between call sites. The headers are the two the rest of ATLAS already uses:

    X-ATLAS-Request-ID          the caller's correlation id
    X-ATLAS-V3-Invocation-ID    the V3 invocation the call belongs to

Their values come only from the current request context bound by the Lens
middleware (geometric_lens.structured_log, the same ContextVars every ATLAS
Python service uses). Nothing here generates, guesses or remembers an
identity: with no bound identity the headers are simply absent, which is the
ordinary non-acquisition case. The pair travels as received: a caller that
supplied both gets both forwarded, one gets one. ContextVars are per task
and per thread, so concurrent requests cannot exchange identities, and a
worker thread that was never bound forwards nothing.

Attribution only. Nothing in scoring, selection, authorization or completion
reads these headers, and no candidate bytes or user content enter them.
"""
import contextlib
import json
import logging
import os
import urllib.error
from typing import Any, Dict, Iterator, Optional
from urllib.request import Request, urlopen

from .auth_token import auth_headers
from .structured_log import bind_identity, current_identity

logger = logging.getLogger(__name__)

REQUEST_ID_HEADER = "X-ATLAS-Request-ID"
INVOCATION_ID_HEADER = "X-ATLAS-V3-Invocation-ID"

# A declared identity for startup and readiness work (the boot self-test, the
# drift fingerprint, a /ready re-run). An acquisition that requires every
# model-bound call to be attributed registers this pair with its relay and
# sets both variables on the Lens container; ordinary deployments set neither
# and startup work carries no identity, as before. A partial pair is ignored:
# half an identity is not one.
STARTUP_REQUEST_ID_ENV = "ATLAS_LENS_STARTUP_REQUEST_ID"
STARTUP_INVOCATION_ID_ENV = "ATLAS_LENS_STARTUP_INVOCATION_ID"

# How much of an error body is read for its message. llama-server's error
# envelope is a few hundred bytes; anything larger is not one.
_ERROR_BODY_LIMIT = 4096


class ModelServerHTTPError(RuntimeError):
    """The model server answered a model-bound call with an HTTP error.

    Carries the status and the server's own message (llama-server wraps it
    in {"error": {"code", "message", "type"}}), so a caller can tell a
    physical-batch refusal from a crash without matching on exception text.
    """

    def __init__(self, status: int, message: str, url: str = ""):
        self.status = int(status)
        self.message = message
        self.url = url
        super().__init__(f"HTTP {self.status} from {url or 'model server'}: {message}")


def _error_message(exc: urllib.error.HTTPError) -> str:
    """The server's message for an HTTP error: the envelope's `error.message`
    when the body is one, else the status reason. One bounded line."""
    try:
        body = exc.read(_ERROR_BODY_LIMIT)
    except Exception:  # noqa: BLE001 - a body that cannot be read has no message
        body = b""
    message = ""
    try:
        parsed = json.loads(body.decode("utf-8", "replace")) if body else None
    except ValueError:
        parsed = None
    if isinstance(parsed, dict):
        err = parsed.get("error")
        if isinstance(err, dict) and isinstance(err.get("message"), str):
            message = err["message"]
        elif isinstance(err, str):
            message = err
    if not message:
        message = str(getattr(exc, "reason", "") or "").strip() or f"status {exc.code}"
    return " ".join(message.split())[:512]


def identity_headers() -> Dict[str, str]:
    """Attribution headers for the current bound identity; empty when none."""
    request_id, invocation_id = current_identity()
    headers: Dict[str, str] = {}
    if request_id:
        headers[REQUEST_ID_HEADER] = request_id
    if invocation_id:
        headers[INVOCATION_ID_HEADER] = invocation_id
    return headers


def model_headers(content_type: Optional[str] = None) -> Dict[str, str]:
    """Every header a model-bound call carries: auth, attribution, content type."""
    headers = dict(auth_headers())
    headers.update(identity_headers())
    if content_type:
        headers["Content-Type"] = content_type
    return headers


def model_request(url: str, payload: Optional[Dict[str, Any]] = None,
                  timeout: float = 120) -> Any:
    """POST `payload` (or GET when None) to a model-server URL; parsed JSON back.

    One call, no retry: a caller that retries does so under the same bound
    identity, so a retry carries the same pair as the attempt it repeats.
    """
    data = json.dumps(payload).encode() if payload is not None else None
    req = Request(url, data=data,
                  headers=model_headers("application/json" if data is not None else None))
    try:
        with urlopen(req, timeout=timeout) as resp:
            return json.loads(resp.read())
    except urllib.error.HTTPError as exc:
        raise ModelServerHTTPError(exc.code, _error_message(exc), url) from exc


def startup_identity_pair() -> Optional[tuple]:
    """The declared startup pair from the environment, or None."""
    rid = os.environ.get(STARTUP_REQUEST_ID_ENV, "").strip()
    inv = os.environ.get(STARTUP_INVOCATION_ID_ENV, "").strip()
    if rid and inv:
        return rid, inv
    if rid or inv:
        logger.warning("startup identity ignored: %s and %s must both be set",
                       STARTUP_REQUEST_ID_ENV, STARTUP_INVOCATION_ID_ENV)
    return None


@contextlib.contextmanager
def startup_identity() -> Iterator[Optional[tuple]]:
    """Bind the declared startup pair for the duration of startup or readiness
    work, then restore whatever identity was bound before. With no declared
    pair nothing is bound and nothing changes. Never attaches a task's
    identity: the pair comes from configuration, not from any request."""
    declared = startup_identity_pair()
    if declared is None:
        yield None
        return
    previous = current_identity()
    bind_identity(*declared)
    try:
        yield declared
    finally:
        bind_identity(*previous)
