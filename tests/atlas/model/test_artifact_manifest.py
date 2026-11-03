"""Signed artifact manifest verify + bundle snapshot/rollback.

The hash-verification and snapshot/rollback paths need no signing key.
The signature check signs with throwaway keys made by ssh-keygen; it skips
where ssh-keygen cannot run.
"""

import json
import os
import pwd
import shutil
import subprocess

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


def _ssh_keygen_unusable():
    """ssh-keygen must be on PATH, and it refuses to run for a uid that has
    no account (a container started with a bare --user has none)."""
    try:
        pwd.getpwuid(os.getuid())
    except KeyError:
        return True
    return shutil.which("ssh-keygen") is None


@pytest.mark.skipif(_ssh_keygen_unusable(),
                    reason="signing needs ssh-keygen and a user account")
@pytest.mark.parametrize(
    ("signing_key", "expected_ok"),
    [("release", True), ("stranger", False)],
    ids=["signed-by-the-allowed-key", "signed-by-an-unknown-key"],
)
def test_verify_trusts_only_a_signature_by_an_allowed_signer(
        tmp_path, monkeypatch, signing_key, expected_ok):
    for name in ("release", "stranger"):
        subprocess.run(["ssh-keygen", "-q", "-t", "ed25519", "-N", "",
                        "-C", name, "-f", str(tmp_path / name)], check=True)
    # The repo's allowed_signers lists only the release key.
    key_type, key = (tmp_path / "release.pub").read_text().split()[:2]
    (tmp_path / ".github").mkdir()
    (tmp_path / ".github" / "allowed_signers").write_text(
        f"release@example.org {key_type} {key}\n")
    monkeypatch.setattr(am, "atlas_root", lambda: str(tmp_path))
    bundle = tmp_path / "bundle"
    bundle.mkdir()
    bundle_dir = _bundle(bundle)
    subprocess.run(["ssh-keygen", "-Y", "sign", "-f", str(tmp_path / signing_key),
                    "-n", am.NAMESPACE, str(bundle / "provenance.json")],
                   check=True, capture_output=True)

    ok, _problems = am.verify_manifest(bundle_dir)

    assert ok is expected_ok


@pytest.mark.parametrize(
    ("damage", "expected_exit_code"),
    [
        (lambda weights: None, 0),
        (lambda weights: weights.write_bytes(b"weights-TAMPERED"), 1),
    ],
    ids=["intact-unsigned-bundle", "changed-file"],
)
def test_artifact_verify_exit_code_follows_the_verdict(
        tmp_path, damage, expected_exit_code):
    bundle_dir = _bundle(tmp_path)
    damage(tmp_path / "cost_field.pt")

    assert artifact.main(["verify", bundle_dir]) == expected_exit_code


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
