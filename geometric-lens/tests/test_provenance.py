"""Lens provenance manifest (§9.5)."""

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
    loaded = provenance.load_provenance(str(tmp_path))
    assert loaded == m


def test_completeness_gate(tmp_path):
    (tmp_path / "cost_field.pt").write_bytes(b"x")
    complete = provenance.build_manifest(
        model="m", embedding_dim=3840, created_at="2026-07-05T00:00:00",
        dataset="ds", metrics={"val_auc": 0.7},
        normalization={"midpoint": 1.0}, save_dir=str(tmp_path))
    # training_commit may be "(unknown)" off a git tree; force a value
    complete["training_commit"] = "abc1234"
    assert provenance.is_complete(complete), provenance.missing_fields(complete)

    incomplete = dict(complete)
    incomplete["dataset"] = ""
    assert not provenance.is_complete(incomplete)
    assert "dataset" in provenance.missing_fields(incomplete)


def test_missing_manifest_is_incomplete():
    assert not provenance.is_complete(None)
    assert set(provenance.missing_fields(None)) == set(
        provenance.REQUIRED_FOR_COMPLETE)


def test_is_complete_rejects_zero_dim():
    m = {
        "model": "m", "embedding_dim": 0, "created_at": "t",
        "training_commit": "abc", "dataset": "ds",
        "metrics": {"val_auc": 0.7}, "normalization": {"midpoint": 1.0},
        "artifact_sha256": {"cost_field.pt": "x" * 64},
    }
    assert not provenance.is_complete(m)
    assert "embedding_dim" in provenance.missing_fields(m)
    # a valid positive dim passes
    m["embedding_dim"] = 3840
    assert provenance.is_complete(m)
    assert "embedding_dim" not in provenance.missing_fields(m)
