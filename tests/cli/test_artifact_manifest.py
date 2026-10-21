"""Signed artifact manifest verify + bundle snapshot/rollback.

The hash-verification and snapshot/rollback paths need no signing key,
so they run in CI. Signing round-trips are exercised manually where the
release key is present.
"""

import json

import pytest

from atlas import artifact_manifest as am
from atlas.commands import artifact


def _bundle(tmp_path):
    """Write an unsigned two-file bundle whose manifest lists both hashes."""
    (tmp_path / "cost_field.pt").write_bytes(b"weights-v1")
    (tmp_path / "cx_normalization.json").write_text('{"midpoint": 1.0}')
    manifest = {
        "schema_version": 1, "model": "m", "embedding_dim": 3840,
        "artifact_sha256": {
            "cost_field.pt": am._sha256(str(tmp_path / "cost_field.pt")),
            "cx_normalization.json":
                am._sha256(str(tmp_path / "cx_normalization.json")),
        },
    }
    (tmp_path / "provenance.json").write_text(json.dumps(manifest))
    return str(tmp_path)


def test_verify_unsigned_intact_is_ok_but_noted(tmp_path):
    ok, problems = am.verify_manifest(_bundle(tmp_path))
    # Unsigned but intact: integrity verified (ok), noted as Preview-level.
    assert ok
    assert any("unsigned" in p and "Preview-level" in p for p in problems)
    assert not any("mismatch" in p for p in problems)


@pytest.mark.parametrize(
    "damage, expected_problem",
    [
        (lambda weights: weights.write_bytes(b"weights-TAMPERED"), "hash mismatch"),
        (lambda weights: weights.unlink(), "missing on disk"),
    ],
    ids=["changed-file", "deleted-file"],
)
def test_verify_fails_when_a_listed_file_is_changed_or_gone(
        tmp_path, damage, expected_problem):
    bundle_dir = _bundle(tmp_path)
    damage(tmp_path / "cost_field.pt")

    ok, problems = am.verify_manifest(bundle_dir)

    assert not ok
    assert any(expected_problem in p for p in problems)


def test_rollback_without_snapshot_errors(tmp_path):
    assert artifact._rollback(_bundle(tmp_path)) == 1


def test_snapshot_replaces_previous_generation(tmp_path):
    # Gen-1 snapshot leaves cost_field.pt; gen-2 bundle uses
    # safetensors. The gen-2 snapshot must not inherit gen-1 leftovers.
    bundle_dir = _bundle(tmp_path)
    artifact._snapshot(bundle_dir)
    (tmp_path / "cost_field.pt").unlink()
    (tmp_path / "cost_field.safetensors").write_bytes(b"weights-v2")
    artifact._snapshot(bundle_dir)
    snap = tmp_path / artifact.SNAPSHOT_DIR
    assert (snap / "cost_field.safetensors").is_file()
    assert not (snap / "cost_field.pt").exists()


def test_rollback_restores_the_snapshot_exactly(tmp_path):
    # Old bundle: .pt only. New (bad) bundle: .safetensors. Rollback
    # must bring the .pt back and remove the .safetensors, or a loader
    # preferring it would still load the bad weights.
    bundle_dir = _bundle(tmp_path)
    assert artifact._snapshot(bundle_dir) == 0
    (tmp_path / "cost_field.pt").unlink()
    (tmp_path / "cost_field.safetensors").write_bytes(b"weights-BAD")

    assert artifact._rollback(bundle_dir) == 0

    assert (tmp_path / "cost_field.pt").read_bytes() == b"weights-v1"
    assert not (tmp_path / "cost_field.safetensors").exists()
