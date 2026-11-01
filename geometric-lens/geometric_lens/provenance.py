"""Per-bundle lens provenance manifest (SUPPORT_MATRIX §9.5).

A newly-trained lens bundle records how it was produced so it is
reproducible and its Supported status is auditable: backbone + dim +
quant + layer, dataset, training commit, hyperparameters, seed,
train/val split, validation metrics, normalization + thresholds,
creation time, and SHA-256 of every artifact file. `atlas lens check`
and `atlas doctor` surface it; a bundle without a complete manifest
stays Preview/Legacy rather than silently claiming Supported.
"""

import hashlib
import json
import os
import subprocess
from typing import Dict, Optional

MANIFEST_NAME = "provenance.json"
SCHEMA_VERSION = 1

# Files that make up a lens bundle; hashed into the manifest when present.
BUNDLE_FILES = [
    "cost_field.pt",
    "cost_field.safetensors",
    "cx_normalization.json",
    "gx_xgboost.json",
    "gx_weights.json",
    "gx_thresholds.json",
    "drift_fingerprint.json",
    "model_identity.json",
]


def _sha256(path: str) -> Optional[str]:
    try:
        h = hashlib.sha256()
        with open(path, "rb") as fh:
            for chunk in iter(lambda: fh.read(65536), b""):
                h.update(chunk)
        return h.hexdigest()
    except OSError:
        return None


def _artifact_hashes(save_dir: str) -> Dict[str, str]:
    out: Dict[str, str] = {}
    for name in BUNDLE_FILES:
        digest = _sha256(os.path.join(save_dir, name))
        if digest:
            out[name] = digest
    return out


def _git_commit(repo_hint: Optional[str] = None) -> str:
    try:
        return subprocess.check_output(
            ["git", "rev-parse", "--short", "HEAD"],
            cwd=repo_hint or os.path.dirname(os.path.abspath(__file__)),
            text=True, stderr=subprocess.DEVNULL).strip()
    except (subprocess.SubprocessError, OSError):
        return "(unknown)"


def build_manifest(*, model: str, embedding_dim: int, created_at: str,
                   quantization: str = "", layer: str = "",
                   dataset: str = "", n_samples: int = 0,
                   n_pass: int = 0, n_fail: int = 0,
                   train_val_split: str = "", metrics: Optional[dict] = None,
                   normalization: Optional[dict] = None,
                   thresholds: Optional[dict] = None,
                   hyperparameters: Optional[dict] = None,
                   seed: Optional[int] = None,
                   llama_cpp_rev: str = "",
                   save_dir: str = "") -> dict:
    """Assemble the manifest. Artifact hashes are filled from save_dir."""
    return {
        "schema_version": SCHEMA_VERSION,
        "artifact_type": "lens",
        "model": model,
        "embedding_dim": embedding_dim,
        "quantization": quantization,
        "hidden_state_layer": layer,
        "created_at": created_at,
        "training_commit": _git_commit(),
        "llama_cpp_rev": llama_cpp_rev,
        "dataset": dataset,
        "samples": {"total": n_samples, "pass": n_pass, "fail": n_fail},
        "train_val_split": train_val_split,
        "hyperparameters": hyperparameters or {},
        "seed": seed,
        "metrics": metrics or {},
        "normalization": normalization or {},
        "thresholds": thresholds or {},
        "artifact_sha256": _artifact_hashes(save_dir) if save_dir else {},
    }


def save_provenance(save_dir: str, manifest: dict) -> str:
    os.makedirs(save_dir, exist_ok=True)
    path = os.path.join(save_dir, MANIFEST_NAME)
    tmp = path + ".tmp"
    with open(tmp, "w") as fh:
        json.dump(manifest, fh, indent=2)
        fh.write("\n")
    os.replace(tmp, path)
    return path


