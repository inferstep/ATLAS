"""Lens provenance manifest (§9.5)."""

import json
import os
import sys

sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from geometric_lens import provenance


def test_build_and_roundtrip(tmp_path):
    # a couple of fake artifact files to hash
    (tmp_path / "cost_field.pt").write_bytes(b"fake-weights")
    (tmp_path / "cx_normalization.json").write_text("{}")

    m = provenance.build_manifest(
        model="gemma-4-12b-it-Q4_K_M", embedding_dim=3840,
        created_at="2026-07-05T00:00:00",
        dataset="gemma_lens", n_samples=287, n_pass=196, n_fail=91,
        metrics={"val_auc": 0.73},
        normalization={"midpoint": 10.5},
        hyperparameters={"epochs": 100}, seed=42, save_dir=str(tmp_path))

    assert m["schema_version"] == provenance.SCHEMA_VERSION
    assert m["model"] == "gemma-4-12b-it-Q4_K_M"
    assert m["embedding_dim"] == 3840
    # artifact hashes computed for present files
    assert "cost_field.pt" in m["artifact_sha256"]
    assert len(m["artifact_sha256"]["cost_field.pt"]) == 64

    path = provenance.save_provenance(str(tmp_path), m)
    assert os.path.basename(path) == "provenance.json"
    with open(path) as f:
        assert json.load(f) == m


