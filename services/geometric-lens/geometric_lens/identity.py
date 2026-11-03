"""Identity metadata for model-coupled Geometric Lens artifacts."""

import json
import os

_VALID_POOLING = ("mean", "none")
_VALID_SHAPES = ("flat", "per_token")


def canonical_model_identity(value: str) -> str:
    """Return a path-insensitive, extension-insensitive model identity."""
    text = str(value or "").strip().replace("\\", "/")
    name = text.rsplit("/", 1)[-1]
    if name.lower().endswith(".gguf"):
        name = name[:-5]
    return name.casefold()


def validate_embedding_contract(value) -> dict:
    """Validate and normalize an embedding_contract object.

    The contract records the embedding convention the artifacts were
    trained against — pooled vs per-token response shape and whether
    vectors arrive L2-normalized. extract_embedding() enforces it at
    request time; without it a convention change shifts every energy
    silently (the 2026-07-15 bench incident: per-token responses were
    mean-pooled unnormalized, ‖v‖≈60, C(x) ~600 instead of ~26 with
    every health check green).
    """
    if not isinstance(value, dict):
        raise ValueError("embedding_contract must be a JSON object")
    pooling = value.get("pooling", "mean")
    if pooling not in _VALID_POOLING:
        raise ValueError(f"embedding_contract.pooling must be one of "
                         f"{_VALID_POOLING}, got {pooling!r}")
    shape = value.get("response_shape", "flat")
    if shape not in _VALID_SHAPES:
        raise ValueError(f"embedding_contract.response_shape must be one of "
                         f"{_VALID_SHAPES}, got {shape!r}")
    normalized = value.get("normalized", False)
    if not isinstance(normalized, bool):
        raise ValueError("embedding_contract.normalized must be a boolean")
    tol = value.get("norm_tolerance", 0.05)
    if (not isinstance(tol, (int, float)) or isinstance(tol, bool)
            or not 0 < tol < 1):
        raise ValueError("embedding_contract.norm_tolerance must be a "
                         "number in (0, 1)")
    return {"pooling": pooling, "response_shape": shape,
            "normalized": normalized, "norm_tolerance": float(tol)}


def validate_model_identity(value) -> dict:
    """Validate and normalize a deserialized artifact identity object."""
    if not isinstance(value, dict):
        raise ValueError("expected a JSON object")
    model = value.get("model")
    if not isinstance(model, str) or not canonical_model_identity(model):
        raise ValueError("expected a non-empty model identity")
    dim = value.get("embedding_dim")
    if not isinstance(dim, int) or isinstance(dim, bool) or dim <= 0:
        raise ValueError("expected a positive integer embedding_dim")
    out = {"model": model.strip(), "embedding_dim": dim}
    contract = value.get("embedding_contract")
    if contract is not None:
        out["embedding_contract"] = validate_embedding_contract(contract)
    return out


def identity_matches(artifact_identity: dict, selected_model: str,
                     embedding_dim: int = 0) -> bool:
    """Return whether an artifact identity belongs to the selected model."""
    identity = validate_model_identity(artifact_identity)
    if canonical_model_identity(identity["model"]) != canonical_model_identity(
            selected_model):
        return False
    return not embedding_dim or identity["embedding_dim"] == int(embedding_dim)


def load_model_identity(models_dir: str) -> dict:
    path = os.path.join(models_dir, "model_identity.json")
    with open(path) as fh:
        return validate_model_identity(json.load(fh))


def save_model_identity(save_dir: str, model: str, embedding_dim: int,
                        embedding_contract: dict = None) -> str:
    identity = validate_model_identity({
        "model": model,
        "embedding_dim": embedding_dim,
        "embedding_contract": embedding_contract,
    })
    os.makedirs(save_dir, exist_ok=True)
    path = os.path.join(save_dir, "model_identity.json")
    tmp_path = path + ".tmp"
    with open(tmp_path, "w") as fh:
        json.dump(identity, fh, indent=2)
        fh.write("\n")
    os.replace(tmp_path, path)
    return path
