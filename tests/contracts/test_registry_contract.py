"""Model-registry artifact contract check.

Every registry entry must be installable or say in its notes why not;
every downloadable artifact must be hash-pinned; artifact
filenames must be ones the runtime actually consumes.
"""

import re

from atlas.commands import model_registry

# Artifact filenames with a runtime consumer (lens loader / doctor /
# llama entrypoint). A registry entry referencing anything else is
# shipping bytes nothing reads.
CONSUMED_LENS_FILES = {
    "cost_field.pt", "cost_field.safetensors",
    "gx_xgboost.json", "gx_weights.json", "gx_thresholds.json",
    "cx_normalization.json", "model_identity.json",
}
CONSUMED_ASA_FILES = {"ast_edit_steering.gguf"}

LENS_STATUSES = {"supported", "no-artifacts", "unverified"}
ASA_STATUSES = {"supported", "no-artifacts", "unverified"}


def test_entries_have_identity_fields():
    for m in model_registry.REGISTRY:
        assert m.name and m.tier and m.model_file and m.model_display, m
        assert m.lens_status in LENS_STATUSES, (m.name, m.lens_status)
        assert m.asa_status in ASA_STATUSES, (m.name, m.asa_status)


def test_every_download_is_hash_pinned():
    """An entry with a download URL carries its SHA-256, which the
    installer checks; a download without one would go unchecked."""
    for m in model_registry.REGISTRY:
        if m.download_url:
            assert m.sha256 and re.fullmatch(r"[0-9a-f]{64}", m.sha256), (
                f"{m.name}: download_url without a sha256 of 64 hex characters")


def test_an_entry_without_a_download_says_why():
    for m in model_registry.REGISTRY:
        if m.download_url is None:
            assert m.notes, f"{m.name}: uninstallable with no explanation"


def test_artifact_url_bases_pin_every_file():
    for m in model_registry.REGISTRY:
        if m.lens_artifact_url_base:
            missing = [f for f in m.lens_artifact_files
                       if f not in m.lens_artifact_sha256]
            assert not missing, (
                f"{m.name}: lens artifacts downloadable but unpinned: "
                f"{missing}")
        if m.asa_artifact_url_base:
            missing = [f for f in m.asa_artifact_files
                       if f not in m.asa_artifact_sha256]
            assert not missing, (
                f"{m.name}: ASA artifacts downloadable but unpinned: "
                f"{missing}")


def test_artifact_files_are_consumed_by_runtime():
    for m in model_registry.REGISTRY:
        stray = set(m.lens_artifact_files) - CONSUMED_LENS_FILES
        assert not stray, (
            f"{m.name}: lens_artifact_files names files the runtime "
            f"never reads: {sorted(stray)}")
        stray = set(m.asa_artifact_files) - CONSUMED_ASA_FILES
        assert not stray, (
            f"{m.name}: asa_artifact_files names files the runtime "
            f"never reads: {sorted(stray)}")
