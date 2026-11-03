import logging
import uuid
from typing import Dict, Any, Optional
from fastapi import FastAPI, HTTPException
from pydantic import BaseModel
from contextlib import asynccontextmanager

import httpx
from config import config
from geometric_lens.model_transport import (model_headers as _model_headers,
                                            startup_identity as _startup_identity)
from geometric_lens import embed_capacity as _embed_capacity


# ---------------------------------------------------------------------------
# Logging + HTTP-response sanitization helpers
# ---------------------------------------------------------------------------
#
# Untrusted strings (request bodies, file content, exception messages
# that wrap user data) can contain CR/LF and other control chars that
# fake additional log entries when written verbatim. _safe_log() strips
# those and bounds length so a single log line stays one line.
#
# For HTTP responses, _safe_detail() returns a short generic message
# while logging the real exception internally with a correlation ID.
# Useful for endpoints where leaking exception text would expose
# filesystem paths or internal types to a remote caller.
def _safe_log(value: object, maxlen: int = 200) -> str:
    """Render a value for inclusion in a log line. Strips CR/LF and
    other ASCII control chars, masks credential-shaped values,
    truncates to maxlen."""
    from geometric_lens.private_values import filter_private_values
    s = str(value)
    s = "".join(c for c in s if c == "\t" or 0x20 <= ord(c) < 0x7f or ord(c) > 0x9f)
    s = filter_private_values(s)
    if len(s) > maxlen:
        s = s[:maxlen] + "…"
    return s


def _safe_detail(e: Exception, op: str = "operation") -> str:
    """Log the real exception with a correlation ID; return a generic
    detail string safe to send in an HTTP response. Use for endpoints
    where exposing str(e) would leak internal paths / types."""
    err_id = uuid.uuid4().hex[:12]
    logger.error(f"[err {err_id}] {op} failed: {type(e).__name__}: {_safe_log(e)}",
                 exc_info=True)
    return f"{op} failed (error_id={err_id})"


# Configure logging. The private-value filter sits on the root handler
# so every logger in the process (pipeline, cache) is covered
# before serialization.
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s - %(name)s - %(levelname)s - %(message)s"
)
from geometric_lens.structured_log import (install as _install_logging,
                                            bind_identity as _bind_identity)
_install_logging("geometric-lens")
logger = logging.getLogger(__name__)

# Boot-time self-test cache. Populated in lifespan() and re-populated when
# /ready re-runs a retryable self-test; read by /health and /ready.
# Keys: lens_cost_field_loaded, lens_cost_field_dim, lens_gx_loaded,
#       lens_gx_type, lens_cx_calibrated, lens_gx_calibrated, lens_artifact_model,
#       embed_dim,
#       self_test_pass, self_test_error.
_BOOT_STATE_DEFAULTS: Dict[str, Any] = {
    "lens_cost_field_loaded": False,
    "lens_cost_field_dim": None,
    "lens_gx_loaded": False,
    "lens_gx_type": "none",
    "lens_cx_calibrated": False,
    "lens_gx_calibrated": False,
    "lens_artifact_model": None,
    "embed_dim": None,
    "self_test_pass": False,
    "self_test_error": None,
    # True when the self-test failed for a reason that can resolve on its own
    # (llama-server not up yet). /ready re-runs the test in that case instead
    # of reporting 503 for the life of the container.
    "self_test_retryable": False,
    # Drift fingerprint (drift_fingerprint.json next to the artifacts):
    # present=False → nothing to enforce; ok=None until checked.
    "fingerprint_present": False,
    "fingerprint_ok": None,
    "fingerprint_error": None,
}
_BOOT_STATE: Dict[str, Any] = dict(_BOOT_STATE_DEFAULTS)


def _lens_drifted() -> bool:
    """True when the drift fingerprint check failed — scoring responses
    must not claim calibration in this state."""
    return _BOOT_STATE.get("fingerprint_ok") is False


def _apply_drift_flags(result: Dict[str, Any]) -> Dict[str, Any]:
    """Stamp a scoring response with the drift state. On drift, calibration
    claims and the thresholds are withdrawn, so a caller that ignores /ready
    still cannot read the numbers as trustworthy or act on them: the proxy's
    corrective and V3's veto both need thresholds."""
    drifted = _lens_drifted()
    result["drifted"] = drifted
    if drifted:
        for key in ("calibrated", "cx_calibrated", "gx_calibrated"):
            if key in result:
                result[key] = False
        if "thresholds" in result:
            result["thresholds"] = None
    return result


def _run_lens_self_test() -> None:
    """C(x)/G(x) self-test — run at boot, and again by /ready after a retryable failure.

    Loads weights, fetches a dummy embedding from llama-server, checks the
    cost-field input dim matches the embedding dim (the silent killer
    behind PC-018), and runs a single C(x) evaluation. Populates
    _BOOT_STATE so /health and /ready can report what actually works.
    Never raises — failures are recorded and surfaced via /ready 503.
    """
    from geometric_lens import service as lens_service

    _BOOT_STATE.update(_BOOT_STATE_DEFAULTS)

    try:
        loaded = lens_service._ensure_models_loaded()
        info = lens_service.get_model_info()
        _BOOT_STATE["lens_cost_field_loaded"] = bool(info.get("loaded"))
        _BOOT_STATE["lens_gx_loaded"] = bool(info.get("gx_loaded"))
        _BOOT_STATE["lens_gx_type"] = info.get("gx_type", "none")
        _BOOT_STATE["lens_cx_calibrated"] = bool(info.get("cx_calibrated"))
        _BOOT_STATE["lens_gx_calibrated"] = bool(info.get("gx_calibrated"))
        _BOOT_STATE["lens_artifact_model"] = info.get("artifact_model")
        if not loaded:
            _BOOT_STATE["self_test_error"] = info.get("error") or (
                "lens model files missing — run `atlas lens build`"
            )
            return

        cf = lens_service._cost_field
        if cf is not None:
            cf_dim = next(cf.parameters()).shape[1] if hasattr(cf, "parameters") else None
            _BOOT_STATE["lens_cost_field_dim"] = cf_dim

        from geometric_lens.embedding_extractor import extract_embedding
        emb = extract_embedding("def add(a, b): return a + b")
        _BOOT_STATE["embed_dim"] = len(emb)

        cf_dim = _BOOT_STATE["lens_cost_field_dim"]
        if cf_dim is not None and cf_dim != len(emb):
            _BOOT_STATE["self_test_error"] = (
                f"lens/embedding dim mismatch: cost_field expects {cf_dim}, "
                f"llama-server returned {len(emb)} (likely wrong model file — see PC-018)"
            )
            return

        raw, norm = lens_service.evaluate_energy("def add(a, b): return a + b")
        if raw == 0.0 and norm == 0.0:
            _BOOT_STATE["self_test_error"] = "C(x) evaluation returned zeros"
            return

        # Drift fingerprint: re-score the reference texts written at
        # training time. A deviation means the serving stack no longer
        # matches what the artifacts were trained on (wrong pooling /
        # normalization / model) even though every request "works" —
        # the failure mode of the 2026-07-15 bench incident.
        from geometric_lens.drift import check_fingerprint
        fp_dir = lens_service.active_models_dir()
        if fp_dir:
            present, fp_ok, fp_detail = check_fingerprint(
                fp_dir, lambda t: lens_service.evaluate_energy(t)[0])
            _BOOT_STATE["fingerprint_present"] = present
            _BOOT_STATE["fingerprint_ok"] = fp_ok if present else None
            _BOOT_STATE["fingerprint_error"] = fp_detail or None
            if present and not fp_ok:
                _BOOT_STATE["self_test_error"] = fp_detail
                return

        _BOOT_STATE["self_test_pass"] = True
        logger.info(
            "Lens self-test OK: cf_dim=%s embed_dim=%s C(x)_raw=%.2f norm=%.3f gx=%s",
            cf_dim, len(emb), raw, norm, _BOOT_STATE["lens_gx_type"],
        )
    except Exception as e:
        # _safe_detail logs the full exception (with correlation ID); the
        # cached value that /health and /ready expose stays generic.
        _BOOT_STATE["self_test_error"] = (
            f"{type(e).__name__}: {_safe_detail(e, 'lens self-test')}"
        )
        # Reaching llama-server is a race at boot, not a verdict about this
        # service. llama loads several GB before it answers, so on a cold
        # start — or a power cut, which is how this was found — the self-test
        # runs first, 503s, and /ready stayed 503 forever even though the
        # artifacts had loaded fine and llama came up healthy seconds later.
        # Mark connectivity failures retryable so /ready can settle itself.
        _BOOT_STATE["self_test_retryable"] = _self_test_retryable(e)


def _self_test_retryable(exc: BaseException) -> bool:
    """Whether a self-test failure is the model server not answering yet.

    Classified the way scoring failures are (embed_capacity.failure_from_
    exception): no answer at all, or a 5xx while llama-server loads. The
    transport raises ModelServerHTTPError for an HTTP error, a name the
    earlier list of type names did not contain, so a 503 at boot had become
    a failure that never retried. A 4xx is a real fault and does not retry.
    """
    from geometric_lens.embed_capacity import (
        KIND_SERVER_ERROR, KIND_UNREACHABLE, failure_from_exception)
    failure = failure_from_exception(exc)
    if failure["kind"] == KIND_UNREACHABLE:
        return True
    if failure["kind"] == KIND_SERVER_ERROR:
        return int(failure.get("status") or 0) >= 500
    # httpx's own timeouts and resets are not OSError subclasses.
    return type(exc).__name__ in {"ConnectError", "ConnectTimeout", "ReadTimeout",
                                  "RemoteProtocolError", "RemoteDisconnected"}


def _llama_state() -> Dict[str, Any]:
    url = config.llama.base_url.rstrip("/") + "/health"
    try:
        with httpx.Client(timeout=2.0, headers=_model_headers()) as client:
            r = client.get(url)
        return {"reachable": r.status_code == 200, "status_code": r.status_code}
    except Exception as e:
        # Routine while llama-server is down — log at warning without a
        # traceback, and keep exception text out of the response.
        logger.warning("llama-server health probe failed: %s: %s",
                       type(e).__name__, _safe_log(e))
        return {"reachable": False,
                "error": f"{type(e).__name__}: llama-server unreachable"}


@asynccontextmanager
async def lifespan(app: FastAPI):
    """Startup and shutdown events."""
    logger.info("Geometric Lens API starting up")
    logger.info(f"Llama server: {config.llama.base_url}")

    # Boot-time C(x)/G(x) self-test. Records state; never raises. Startup work
    # carries the declared startup identity when one is configured (attribution
    # only), and none otherwise.
    with _startup_identity():
        _run_lens_self_test()
    if not _BOOT_STATE["self_test_pass"]:
        logger.error(
            "Geometric Lens self-test FAILED: %s. /ready will return 503.",
            _BOOT_STATE["self_test_error"],
        )

    yield

    logger.info("Geometric Lens API shutting down")


app = FastAPI(
    title="Geometric Lens API",
    description="C(x)/G(x) scoring and sandbox analysis for the ATLAS stack",
    version="3.0.1",
    lifespan=lifespan
)

# --- Internal service auth (per-installation token) ---
# /internal/* is enforced by this middleware when a token is configured.
# /health, /ready and / stay open (compose/K8s probes are headerless).
from geometric_lens.auth_token import (SERVICE_TOKEN as _SERVICE_TOKEN,
                                       install_urllib_opener as
                                       _install_urllib_opener)
import hmac as _hmac

_install_urllib_opener()  # outbound: embedding extractor, identity probe

@app.middleware("http")
async def _require_service_token(request, call_next):
    # Enforce only on /internal/* — /health, /ready, / stay open for
    # probes.
    if _SERVICE_TOKEN and request.url.path.startswith("/internal/"):
        got = request.headers.get("authorization", "")
        if not _hmac.compare_digest(got, f"Bearer {_SERVICE_TOKEN}"):
            from fastapi.responses import JSONResponse
            return JSONResponse(status_code=401, content={
                "error": "unauthorized",
                "detail": "internal service auth is enabled; send "
                          "Authorization: Bearer <service-token> "
                          "(secrets/service-token)"})
    return await call_next(request)


# Registered AFTER the token middleware: Starlette wraps in reverse
# registration order (last = outermost), and the correlation ID must be
# set/echoed even on requests the auth middleware rejects with 401.
@app.middleware("http")
async def _correlation_id(request, call_next):
    # Adopt the caller's correlation ID and V3 invocation ID (or none); echo
    # the correlation ID back so the whole turn shares one id across services.
    # Both are bound on the ContextVars every ATLAS service uses, so every
    # model-bound call this request makes (geometric_lens.model_transport)
    # carries the same pair the caller supplied, and nothing else. They are
    # cleared when the request ends, whether it returned or raised, so no
    # later work in this context can inherit them.
    rid = request.headers.get("x-atlas-request-id", "")
    inv = request.headers.get("x-atlas-v3-invocation-id", "")
    _bind_identity(rid, inv)
    try:
        response = await call_next(request)
    finally:
        _bind_identity("", "")
    if rid:
        response.headers["X-ATLAS-Request-ID"] = rid
    return response


# Endpoints
# Note: probe/scoring endpoints below are deliberately plain `def` — they do
# synchronous work (httpx sync client, urlopen to llama-server,
# torch), so FastAPI runs them in its threadpool instead of blocking the
# event loop.
@app.get("/health")
def health():
    """Structured per-subsystem health.

    Always returns 200 — this endpoint is for *information*, not gating.
    Use /ready for liveness/scoring-functional gating.
    """
    llama_st = _llama_state()
    lens_ok = _BOOT_STATE["self_test_pass"]
    overall = llama_st["reachable"] and lens_ok
    return {
        "service": "geometric-lens",
        "status": "healthy" if overall else "degraded",
        "subsystems": {
            "llama_server": llama_st,
            "lens": {
                "cost_field_loaded": _BOOT_STATE["lens_cost_field_loaded"],
                "cost_field_dim": _BOOT_STATE["lens_cost_field_dim"],
                "embed_dim": _BOOT_STATE["embed_dim"],
                "gx_loaded": _BOOT_STATE["lens_gx_loaded"],
                "gx_type": _BOOT_STATE["lens_gx_type"],
                "cx_calibrated": _BOOT_STATE["lens_cx_calibrated"],
                "gx_calibrated": _BOOT_STATE["lens_gx_calibrated"],
                "artifact_model": _BOOT_STATE["lens_artifact_model"],
                "self_test_pass": _BOOT_STATE["self_test_pass"],
                "self_test_error": _BOOT_STATE["self_test_error"],
                "fingerprint_present": _BOOT_STATE["fingerprint_present"],
                "fingerprint_ok": _BOOT_STATE["fingerprint_ok"],
                "fingerprint_error": _BOOT_STATE["fingerprint_error"],
                # The /embedding physical batch: the longest input one score
                # can be computed from. Declared by the deployment
                # (LLAMA_EMBED_CAPACITY_TOKENS) or observed from a refusal.
                # Information, not a gate: a deployment whose capacity is
                # below what its callers generate still scores everything
                # shorter, and reports each longer input as unscored.
                **_embed_capacity.snapshot(),
            },
        },
    }


@app.get("/ready")
def ready():
    """Readiness gate. 200 only when scoring is functional, 503 otherwise.

    Use this for orchestrator probes that should pull traffic away when
    lens scoring degrades (the silent-failure mode PC-019 was filed for).
    """
    llama_st = _llama_state()
    # Settle a boot-order race rather than latching it. Only retried when the
    # failure was connectivity-shaped AND llama is reachable now, so a real
    # fault (dim mismatch, missing artifacts, fingerprint drift) still fails
    # fast and does not re-embed on every poll.
    if (not _BOOT_STATE["self_test_pass"]
            and _BOOT_STATE.get("self_test_retryable")
            and llama_st["reachable"]):
        logger.info("llama-server is reachable now — re-running the lens self-test")
        with _startup_identity():
            _run_lens_self_test()

    lens_ok = _BOOT_STATE["self_test_pass"]

    ok = llama_st["reachable"] and lens_ok
    payload = {
        "ready": ok,
        "llama_server": llama_st["reachable"],
        "lens_self_test": _BOOT_STATE["self_test_pass"],
        "fingerprint_ok": _BOOT_STATE["fingerprint_ok"],
        "embed_capacity_tokens": _embed_capacity.snapshot()["embed_capacity_tokens"],
        "reason": _BOOT_STATE["self_test_error"] if not lens_ok else None,
    }
    if not ok:
        raise HTTPException(status_code=503, detail=payload)
    return payload


# ──────────────────────────────────────────────────────────────
# Geometric Lens: Internal Monitoring Endpoints
# ──────────────────────────────────────────────────────────────

class LensScoreTextRequest(BaseModel):
    text: str


class LensScorePerStepRequest(BaseModel):
    text: str
    # Optional transformer-block index. None => last-layer (vanilla /embedding,
    # no PC-202 patch needed). Set to use the PC-202 layers extension and score
    # at the residual stream of a specific intermediate layer (PC-204 fusion).
    layer: Optional[int] = None


@app.post("/internal/lens/score-text")
def lens_score_text(request: LensScoreTextRequest):
    """Score a text string through the Geometric Lens. Returns raw and normalized energy."""
    try:
        from geometric_lens import service as lens_service
        from geometric_lens.embedding_extractor import extract_embedding

        if not lens_service._ensure_models_loaded():
            return {"energy": None, "normalized": None, "calibrated": False,
                    "enabled": True, "scored": False,
                    "failure": {"kind": "models_not_loaded"},
                    "error": "models_not_loaded"}

        import torch
        from geometric_lens.embed_capacity import finite

        emb = extract_embedding(request.text)
        x = torch.tensor(emb, dtype=torch.float32).unsqueeze(0)

        with torch.no_grad():
            energy = finite(lens_service._cost_field(x).item(), "energy")

        normalized = finite(lens_service._normalize_cx_energy(energy), "normalized")

        return _apply_drift_flags({
            "scored": True,
            "energy": energy,
            "normalized": normalized,
            "calibrated": lens_service._cx_normalization is not None,
            "enabled": True,
        })
    except Exception as e:
        from geometric_lens.service import failure_record
        return {
            "energy": None, "normalized": None, "calibrated": False,
            "enabled": True, "scored": False,
            "failure": failure_record(e, "score-text"),
            "error": _safe_detail(e, "lens score-text"),
        }


@app.post("/internal/lens/gx-score")
def lens_gx_score(request: LensScoreTextRequest):
    """Combined C(x) + G(x) scoring in a single call.

    Returns C(x) energy, normalized energy, G(x) XGBoost quality prediction,
    and a human-readable verdict. Uses one embedding extraction for both models.
    """
    try:
        from geometric_lens.service import evaluate_combined

        result = evaluate_combined(request.text)
        if isinstance(result, dict):
            result = _apply_drift_flags(result)
        return result
    except Exception as e:
        from geometric_lens.service import failure_record, unscored_combined
        return unscored_combined(failure_record(e, "gx-score"),
                                 _safe_detail(e, "lens gx-score"))


@app.post("/internal/lens/score-per-step")
def lens_score_per_step(request: LensScorePerStepRequest):
    """PC-207 lens-as-PRM: score every token in the text instead of pooling.

    Returns C(x) and (when XGBoost is loaded) G(x) per generation step,
    plus aggregates across the whole sequence. Used by V3 candidate
    generation to abort off-rails candidates early instead of paying the
    full decode cost — the lens stops being ORM-by-timing (scores
    completed text) and becomes PRM-by-timing.

    Set `layer` to use the PC-202 hidden-states extension and score the
    residual stream at a specific intermediate layer (PC-204). Leave
    `layer` null to use the model's last-layer hidden state via vanilla
    /embedding (works on unpatched llama-server).
    """
    try:
        from geometric_lens.service import evaluate_per_step

        result = evaluate_per_step(request.text, layer=request.layer)
        agg = result.get("aggregate") or {}
        failure = result.get("failure") or {}
        # _safe_log on the request.layer value strips CRLF + truncates
        # so user input can't fake a separate log entry. The other args
        # are floats/ints from result — structurally safe; the failure
        # kind is one of the service's own constants.
        logger.info(
            "lens score-per-step: in_chars=%d n_tok=%d scored=%s failure=%s "
            "gx_min=%.3f gx_mean=%.3f off_rails=%d layer=%s lat=%.0fms",
            len(request.text or ""),
            int(result.get("n_tokens", 0)),
            bool(result.get("scored", bool(result.get("n_tokens")))),
            _safe_log(failure.get("kind")) if failure else "-",
            float(agg.get("gx_score_min", 0.0)),
            float(agg.get("gx_score_mean", 0.0)),
            int(agg.get("first_off_rails_idx", -1)),
            _safe_log(request.layer) if request.layer is not None else "last",
            float(result.get("latency_ms", 0.0)),
        )
        # Per-step scores carry thresholds too, and drift must withdraw them
        # here as it does on the other scoring endpoints.
        return _apply_drift_flags(result)
    except Exception as e:
        from geometric_lens.service import failure_record, unscored_per_step
        return unscored_per_step(failure_record(e, "score-per-step"),
                                 _safe_detail(e, "lens score-per-step"))


if __name__ == "__main__":
    import uvicorn
    uvicorn.run(
        app,
        host=config.server.host,
        port=config.server.port
    )
