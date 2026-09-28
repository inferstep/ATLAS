"""Drift fingerprint: reference texts with expected C(x) energies.

Catches embedding-stack changes that shift every energy while leaving all
health checks green — the 2026-07-15 bench incident: a rebuilt embed
server switched to per-token unnormalized responses, C(x) served ~600
against a calibrated range of ~20-30, and pods stayed Ready throughout.
A fingerprint written at training time makes that state detectable: the
lens self-test (at boot, and again from /ready after a retryable failure)
re-scores the references and fails /ready when any deviates beyond
tolerance.

File format (drift_fingerprint.json, next to cost_field.pt):

    {
      "tolerance_pct": 15,
      "note": "free-form stack description",
      "references": [
        {"text": "def add(a, b): ...", "expected_energy": 20.923},
        ...
      ]
    }
"""

import json
import logging
import os
from typing import Callable, Optional, Tuple

logger = logging.getLogger(__name__)

FINGERPRINT_FILE = "drift_fingerprint.json"
DEFAULT_TOLERANCE_PCT = 15.0
# Floor so a near-zero expected energy doesn't demand exact reproduction.
MIN_ABS_TOLERANCE = 0.1

# Fixed probe texts. Deliberately plain code — the point is reproducible
# energies, not coverage of the input space.
REFERENCE_TEXTS = [
    "def add(a, b):\n    return a + b",
    "class Stack:\n    def __init__(self):\n        self.items = []\n\n" +
    "    def push(self, item):\n        self.items.append(item)\n\n" +
    "    def pop(self):\n        return self.items.pop()",
    "for i in range(10):\n    print(i * i)",
]


def validate_fingerprint(value) -> dict:
    """Validate and normalize a deserialized fingerprint object."""
    if not isinstance(value, dict):
        raise ValueError("expected a JSON object")
    tol = value.get("tolerance_pct", DEFAULT_TOLERANCE_PCT)
    if (not isinstance(tol, (int, float)) or isinstance(tol, bool)
            or not 0 < tol < 100):
        raise ValueError("tolerance_pct must be a number in (0, 100)")
    refs = value.get("references")
    if not isinstance(refs, list) or not refs:
        raise ValueError("references must be a non-empty list")
    normalized = []
    for i, ref in enumerate(refs):
        if not isinstance(ref, dict):
            raise ValueError(f"references[{i}] must be an object")
        text = ref.get("text")
        if not isinstance(text, str) or not text.strip():
            raise ValueError(f"references[{i}].text must be a non-empty string")
        energy = ref.get("expected_energy")
        if not isinstance(energy, (int, float)) or isinstance(energy, bool):
            raise ValueError(f"references[{i}].expected_energy must be a number")
        normalized.append({"text": text, "expected_energy": float(energy)})
    return {"tolerance_pct": float(tol),
            "note": str(value.get("note", "")),
            "references": normalized}


def load_fingerprint(models_dir: str) -> Optional[dict]:
    """Load and validate the fingerprint. None when the file is absent;
    raises ValueError when present but invalid."""
    path = os.path.join(models_dir, FINGERPRINT_FILE)
    if not os.path.exists(path):
        return None
    with open(path) as fh:
        return validate_fingerprint(json.load(fh))


def check_fingerprint(models_dir: str,
                      score_fn: Callable[[str], float]
                      ) -> Tuple[bool, bool, str]:
    """Re-score the fingerprint references against the live stack.

    score_fn maps a reference text to its raw C(x) energy.

    Returns (present, ok, detail):
      (False, True,  "")      — no fingerprint file; nothing to enforce
      (True,  True,  "")      — every reference within tolerance
      (True,  False, reason)  — drift or an unreadable/invalid file; the
                                reason names expected vs observed
    """
    try:
        fp = load_fingerprint(models_dir)
    except (ValueError, OSError) as exc:
        return True, False, f"{FINGERPRINT_FILE} unreadable: {exc}"
    if fp is None:
        return False, True, ""
    for ref in fp["references"]:
        # A reference that cannot be scored raises: that is a failed
        # measurement, not drift. Reading it as drift made a passing
        # model-server hiccup at boot a lasting "drifted" verdict.
        got = float(score_fn(ref["text"]))
        expected = ref["expected_energy"]
        tol = max(abs(expected) * fp["tolerance_pct"] / 100.0,
                  MIN_ABS_TOLERANCE)
        if abs(got - expected) > tol:
            return True, False, (
                f"embedding stack drift: reference scored {got:.2f}, "
                f"expected {expected:.2f} ±{fp['tolerance_pct']:g}%. The "
                f"serving stack no longer matches what the lens artifacts "
                f"were trained on — check the embed server's `--pooling` "
                f"flag (ATLAS_EMBED_POOLING=none) and the loaded model."
            )
    return True, True, ""


def write_fingerprint(models_dir: str,
                      score_fn: Callable[[str], float],
                      texts=None,
                      tolerance_pct: float = DEFAULT_TOLERANCE_PCT,
                      note: str = "") -> str:
    """Score the reference texts through the live stack and persist the
    fingerprint next to the artifacts. Write it right after training, so
    the expectations describe the current weights + embedding
    convention."""
    fp = validate_fingerprint({
        "tolerance_pct": tolerance_pct,
        "note": note,
        "references": [
            {"text": t, "expected_energy": float(score_fn(t))}
            for t in (texts or REFERENCE_TEXTS)
        ],
    })
    path = os.path.join(models_dir, FINGERPRINT_FILE)
    tmp_path = path + ".tmp"
    with open(tmp_path, "w") as fh:
        json.dump(fp, fh, indent=2)
        fh.write("\n")
    os.replace(tmp_path, path)
    logger.info("Wrote %s (%d references, ±%g%%)",
                path, len(fp["references"]), fp["tolerance_pct"])
    return path
