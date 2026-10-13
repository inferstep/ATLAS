"""Model card text for `atlas lens publish`.

Kept out of lens.py, which has no room left under the file size limit.
"""

from __future__ import annotations

import json as jsonlib
import os


def model_card_provenance(artifact_dir: str, base_model: str, dim: int) -> str:
    """The model card's Provenance paragraph for the bundle in *artifact_dir*.

    `atlas lens build` writes a provenance.json whose hyperparameters carry
    the ranking `margin`; only those bundles trained with the contrastive
    ranking loss. A bundle without that record may come from the removed
    scripts/retrain_lens_from_results.py (class-weighted MSE to energy
    targets), so the card must not claim the contrastive loss for it.
    """
    arch = f"Architecture: {dim} -> 512 -> 128 -> 1 (SiLU, SiLU, Softplus)."
    try:
        with open(os.path.join(artifact_dir, "provenance.json")) as fh:
            hyper = (jsonlib.load(fh).get("hyperparameters") or {})
    except (OSError, ValueError, AttributeError):
        hyper = {}
    if "margin" in hyper:
        return (f"Trained locally via `atlas lens build` against {base_model}'s\n"
                f"self-embeddings. {arch} Contrastive ranking loss on labeled\n"
                f"pass/fail code samples.")
    return (f"Trained against {base_model}'s self-embeddings. {arch}\n"
            f"No `atlas lens build` provenance record accompanies these artifacts,\n"
            f"so the training loss is not recorded here. `atlas lens build` uses a\n"
            f"contrastive ranking loss; bundles from the removed\n"
            f"`scripts/retrain_lens_from_results.py` trained C(x) with class-weighted\n"
            f"MSE to energy targets instead.")
