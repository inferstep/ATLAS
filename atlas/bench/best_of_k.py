"""Lens candidate scoring for the benchmark runner (v3_runner imports
score_candidate for generated candidates and score_candidate_combined for
the probe the CxGx allocation gate reads)."""

import json
import math
import urllib.request
import urllib.error
from typing import Dict, Optional, Tuple


def _finite(value) -> Optional[float]:
    """`value` as a float when it is a finite number, else None: the rule
    stages.candidate_selection.finite_score applies in the product path
    (json.loads accepts NaN / Infinity and reads 1e999 as an infinity).
    The bench cannot import v3-service here, so the four lines are repeated."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    out = float(value)
    return out if math.isfinite(out) else None


def _nonfinite(field: str, value) -> Dict:
    return {"kind": "nonfinite_score", "field": field,
            "detail": f"{field} is not a finite number: {value!r}"[:200]}


def score_candidate(text: str, lens_url: str) -> Tuple[float, float]:
    """Score candidate text through the Geometric Lens.

    Args:
        text: Full text to score (typically "TASK: {prompt}\\n\\nSOLUTION: {response}").
        lens_url: Base URL for geometric-lens (e.g. "http://localhost:31144").

    Returns:
        Tuple of (raw_energy, normalized_energy), or (None, None) when the
        Lens did not score the text: the input exceeded the embedding
        server's physical batch, the Lens answered an error, or it could
        not be reached. None is not an energy and never sorts as one
        (stages.candidate_selection.energy_rank_key ranks it after every
        scored candidate); a neutral pair here read as the best energy in
        the pool, which is the product-path defect this mirrors.
    """
    body = json.dumps({"text": text}).encode("utf-8")
    req = urllib.request.Request(
        f"{lens_url}/internal/lens/score-text",
        data=body,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, ValueError):
        return (None, None)
    if _unscored_answer(data):
        return (None, None)
    energy = _finite(data.get("energy"))
    normalized = _finite(data.get("normalized", 0.5))
    if energy is None or normalized is None:
        return (None, None)
    return (energy, normalized)


def _unscored_answer(data) -> bool:
    """True when a Lens answer says it did not score: a typed `failure`,
    `scored: false`, an `error`, or the verdict "error" (the shape a Lens
    without the typed boundary gave every failure, numbers attached)."""
    if not isinstance(data, dict):
        return True
    return bool(data.get("failure")) or data.get("scored") is False \
        or bool(data.get("error")) or data.get("verdict") == "error"


# The answer of a Lens with no model loaded: the state a model is in before
# its lens bundle is built, which is what this bench builds it from. The
# allocation gate reads it as "no signal" (k=3 floor).
#
# This neutral path is deliberate and bench-only. The product requires the
# lens and stops the run when it cannot score (docs/adr/0011-the-lens-is-
# required.md); the bench keeps this so it still works for a model with no lens
# bundle yet. It is never part of the product. Why:
# docs/CLI.md, "Why the bench keeps a neutral lens score".
NEUTRAL_COMBINED = {
    "cx_energy": 0.0, "cx_normalized": 0.5, "cx_calibrated": False,
    "gx_score": 0.5, "gx_available": False, "verdict": "unavailable",
}

# The answer for a text the Lens did not score: no number anywhere, and the
# typed failure attached.
UNSCORED_COMBINED = {
    "cx_energy": None, "cx_normalized": None, "cx_calibrated": False,
    "gx_score": None, "gx_available": False, "verdict": "unscored",
}


def score_candidate_combined(text: str, lens_url: str) -> Dict:
    """Score candidate text through the Lens's combined C(x)+G(x) endpoint.

    A single embedding extraction feeds both models: C(x) cost-field energy
    (``cx_energy`` raw, ``cx_normalized`` in [0,1], lower = better) and the
    G(x) XGBoost quality classifier (``gx_score`` = P(correct) in [0,1],
    higher = better), so the pair costs no more than C(x) alone. The CxGx
    allocation gate reads both off the probe.

    A lens with no model loaded yields the neutral dict, which the gate
    reads as "no signal" and answers with its k=3 floor. A Lens that did not score the
    text, or could not be reached, yields ``UNSCORED_COMBINED`` with the
    typed ``failure`` attached: every score field None, verdict
    ``"unscored"``. The gate reads that verdict as no signal as well.
    """
    body = json.dumps({"text": text}).encode("utf-8")
    req = urllib.request.Request(
        f"{lens_url}/internal/lens/gx-score",
        data=body,
        headers={"Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read().decode("utf-8"))
    except (urllib.error.URLError, urllib.error.HTTPError, OSError, ValueError) as exc:
        return {**UNSCORED_COMBINED,
                "failure": {"kind": "lens_unreachable", "detail": type(exc).__name__}}
    if isinstance(data, dict) and not data.get("enabled", False):
        return dict(NEUTRAL_COMBINED)
    if _unscored_answer(data):
        failure = data.get("failure") if isinstance(data, dict) else None
        if not isinstance(failure, dict):
            error = data.get("error") if isinstance(data, dict) else "malformed body"
            failure = {"kind": "lens_error", "detail": str(error or "unscored")[:200]}
        return {**UNSCORED_COMBINED, "failure": failure}
    if "cx_energy" not in data:
        return {**UNSCORED_COMBINED,
                "failure": {"kind": "lens_error", "detail": "no C(x) energy in the answer"}}
    for field in ("cx_energy", "cx_normalized", "gx_score"):
        value = data.get(field, 0.5)
        if _finite(value) is None:
            return {**UNSCORED_COMBINED, "failure": _nonfinite(field, value)}
    return {
        "cx_energy": _finite(data["cx_energy"]),
        "cx_normalized": _finite(data.get("cx_normalized", 0.5)),
        "cx_calibrated": bool(data.get("cx_calibrated", False)),
        "gx_score": _finite(data.get("gx_score", 0.5)),
        "gx_available": bool(data.get("gx_available", False)),
        "verdict": data.get("verdict", "unavailable"),
    }
