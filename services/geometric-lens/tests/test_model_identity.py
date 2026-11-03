import json
import os
import sys

import pytest

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from geometric_lens.identity import (
    canonical_model_identity,
    identity_matches,
    load_model_identity,
    save_model_identity,
    validate_model_identity,
)
from geometric_lens import service


def test_identity_is_path_and_extension_insensitive():
    assert canonical_model_identity("/models/Example-Model.gguf") == "example-model"
    assert canonical_model_identity("example-model") == "example-model"


def test_identity_requires_model_and_positive_integer_dim():
    for value in (
        {},
        {"model": "", "embedding_dim": 4096},
        {"model": "example", "embedding_dim": True},
        {"model": "example", "embedding_dim": 0},
    ):
        with pytest.raises(ValueError):
            validate_model_identity(value)


def test_identity_rejects_same_dim_different_model():
    identity = {"model": "model-a", "embedding_dim": 4096}
    assert identity_matches(identity, "/models/model-a.gguf", 4096)
    assert not identity_matches(identity, "/models/model-b.gguf", 4096)


def test_identity_save_round_trip_is_atomic_shape(tmp_path):
    path = save_model_identity(str(tmp_path), "/models/Example.gguf", 3840)
    with open(path, encoding="utf-8") as identity_file:
        saved = json.load(identity_file)
    assert saved == {
        "model": "/models/Example.gguf",
        "embedding_dim": 3840,
    }
    assert load_model_identity(str(tmp_path))["embedding_dim"] == 3840
    assert not os.path.exists(path + ".tmp")


def test_service_requires_identity_for_selected_model(tmp_path, monkeypatch):
    monkeypatch.setenv("ATLAS_MODEL_NAME", "selected-model")
    assert not service._verify_model_identity(str(tmp_path))
    save_model_identity(str(tmp_path), "other-model", 4096)
    assert not service._verify_model_identity(str(tmp_path))
    save_model_identity(str(tmp_path), "/models/selected-model.gguf", 4096)
    assert service._verify_model_identity(str(tmp_path))
