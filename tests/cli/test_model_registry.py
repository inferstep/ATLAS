"""Tests for atlas.commands.model_registry (PC-056).

Covers:
  - REGISTRY contents and lens/ASA status truth
  - for_tier / tier_for_model / by_name / by_model_file lookups
  - is_installed / installed_size_gb / verify_installed file probes
  - lens artifact location and presence
  - artifact_download_hint
"""

import pytest

from atlas.commands import model_registry


# ---------------------------------------------------------------------------
# Registry contents: the truth today
# ---------------------------------------------------------------------------

def test_registry_has_known_qwen_entries():
    """PC-056 shipped with 4 tier presets; PC-056.1 added Q4_K_M and
    Q8_0 variants of the 9B; PC-215 added gemma-4-12b for a total of 7.
    Adding more is a deliberate scope change and should be a separate
    ticket — flag it loudly here."""
    assert len(model_registry.REGISTRY) == 7
    names = {m.name for m in model_registry.REGISTRY}
    assert names == {"Qwen3.5-7B-Q4_K_M",
                     "Qwen3.5-9B-Q4_K_M", "Qwen3.5-9B-Q6_K", "Qwen3.5-9B-Q8_0",
                     "Qwen3.5-14B-Q5_K_M", "Qwen3.5-32B-Q5_K_M",
                     "gemma-4-12b-it-Q4_K_M"}


def test_only_published_lens_weight_bundles_are_supported_today():
    """Supported means downloadable weights; calibration is tracked separately."""
    names = {m.name for m in model_registry.supported_models()}
    assert names == {"Qwen3.5-9B-Q6_K", "gemma-4-12b-it-Q4_K_M"}


def test_only_q6k_has_a_lens_bundle_among_the_9b_quants():
    """The lens loads a bundle only for the model it was built for (same
    model name and embedding size: identity_matches in
    geometric_lens/identity.py). The 9B bundle was built for Q6_K, so the
    other quants have no Lens artifacts until they get their own. The
    registry used to call them `unverified` and say they reuse the Q6_K
    files, which the lens rejects (#247)."""
    assert model_registry.by_name("Qwen3.5-9B-Q6_K").lens_status == "supported"
    for name in ("Qwen3.5-9B-Q4_K_M", "Qwen3.5-9B-Q8_0"):
        assert model_registry.by_name(name).lens_status == "no-artifacts", name


def test_a_model_without_a_bundle_says_why_and_how_to_build_one():
    """With the lens required, a model with no bundle stops agent work.
    Its notes must say why, say so, and name the way out, not promise a
    silent no-op."""
    for m in model_registry.REGISTRY:
        if m.lens_status != "no-artifacts":
            continue
        notes = m.notes.lower()
        assert "no lens artifacts" in notes, f"{m.name}: notes do not say why"
        assert "atlas lens build" in notes, f"{m.name}: no build path in notes"
        assert "stops agent work" in notes, f"{m.name}: notes hide the stop"
        assert "silently no-op" not in notes, f"{m.name}: stale no-op claim"


def test_only_9b_quants_are_installable():
    """Only the three 9B quants have a download source."""
    installable = {m.name for m in model_registry.REGISTRY if m.can_install}
    assert installable == {"Qwen3.5-9B-Q4_K_M", "Qwen3.5-9B-Q6_K",
                           "Qwen3.5-9B-Q8_0"}


def test_9b_has_verified_download_metadata():
    """The 9B entry has a Hugging Face download URL, a sensible size,
    and the SHA256 we captured from HF's x-linked-etag."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert m.download_url.startswith("https://huggingface.co/")
    assert 6.0 < m.model_size_gb < 8.0  # 6.94 GB observed
    assert len(m.sha256) == 64


def test_download_urls_are_pinned_to_a_commit():
    """download_urls must NOT include `/main/` (PC-056 mistake): a pinned
    commit keeps the SHA256 valid even if upstream re-uploads a file
    with the same name."""
    for m in model_registry.REGISTRY:
        if m.download_url is None:
            continue
        assert "/main/" not in m.download_url, m.name
        assert "/3885219b" in m.download_url, m.name  # the unsloth Qwen3.5 9B commit


def test_supported_9b_needs_no_hf_token():
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert m.requires_hf_token is False
    assert m.is_gated is False


def test_supported_9b_lists_its_lens_artifact_files():
    """doctor cross-checks the 'supported' claim against this list."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert "cost_field.pt" in m.lens_artifact_files


@pytest.mark.parametrize(
    "name, asa_status",
    [
        # Both lens and ASA artifacts are published to HF.
        ("Qwen3.5-9B-Q6_K", "supported"),
        # Built from prompts that named the tool ast_edit (now
        # structural_edit) and never A/B measured on gemma. The label says
        # only that; it does not switch steering off.
        ("gemma-4-12b-it-Q4_K_M", "unverified"),
    ],
)
def test_asa_status_of_models_with_a_steering_vector(name, asa_status):
    m = model_registry.by_name(name)
    assert m.asa_status == asa_status
    assert "ast_edit_steering.gguf" in m.asa_artifact_files


@pytest.mark.parametrize(
    "asa_fields, expected_status, expected_files",
    [
        # The fields that `atlas asa publish` puts in its suggested registry
        # diff: a maintainer pasting that diff must not get a TypeError.
        ({"asa_status": "supported",
          "asa_artifact_files": ["ast_edit_steering.gguf"]},
         "supported", ["ast_edit_steering.gguf"]),
        # Entries older than PC-061 must not silently claim ASA support.
        ({}, "no-artifacts", []),
    ],
    ids=["asa-fields-given", "asa-fields-left-out"],
)
def test_model_takes_asa_fields_and_defaults_to_no_artifacts(
        asa_fields, expected_status, expected_files):
    # The other keywords are the stable public field names callers use.
    m = model_registry.Model(
        name="Test-9B", tier="medium", model_file="Test-9B.gguf",
        model_display="Test 9B", model_size_gb=6.0, lens_status="supported",
        **asa_fields,
    )
    assert m.asa_status == expected_status
    assert m.asa_artifact_files == expected_files


def test_grammar_mode_is_loose_for_gemma_and_strict_for_the_rest():
    """Gemma under the strict schema grammar emits `done` instead of
    calling tools, and every recorded gemma measurement ran loose. The
    registry says so, and env_vars carries it so an install writes it."""
    for m in model_registry.REGISTRY:
        expected = "loose" if m.name.startswith("gemma") else "strict"
        assert m.grammar_mode == expected, m.name
        assert m.env_vars()["ATLAS_GRAMMAR_MODE"] == expected, m.name


# ---------------------------------------------------------------------------
# Lookups
# ---------------------------------------------------------------------------

@pytest.mark.parametrize(
    "tier, expected_name, expected_lens_status",
    [
        # Q4_K_M is listed first, but Q6_K has published lens weights.
        ("medium", "Qwen3.5-9B-Q6_K", "supported"),
        # No supported entry: the first match (the caller shows a warning).
        ("xlarge", "Qwen3.5-32B-Q5_K_M", "no-artifacts"),
    ],
)
def test_for_tier_prefers_a_supported_model(tier, expected_name,
                                            expected_lens_status):
    m = model_registry.for_tier(tier)
    assert m.name == expected_name
    assert m.lens_status == expected_lens_status


def test_models_for_tier_returns_only_that_tier():
    """Medium tier: the three 9B quants plus gemma-4-12b (PC-215)."""
    medium = model_registry.models_for_tier("medium")
    assert sorted(m.name for m in medium) == [
        "Qwen3.5-9B-Q4_K_M", "Qwen3.5-9B-Q6_K", "Qwen3.5-9B-Q8_0",
        "gemma-4-12b-it-Q4_K_M"]


def test_every_model_is_found_by_its_name_and_its_file():
    for m in model_registry.REGISTRY:
        assert model_registry.by_name(m.name) is m
        assert model_registry.tier_for_model(m.model_file) == m.tier


def test_by_model_file_ignores_the_folder_and_the_case():
    gemma = model_registry.by_name("gemma-4-12b-it-Q4_K_M")
    assert model_registry.by_model_file("/models/gemma-4-12b-it-Q4_K_M.gguf") is gemma
    assert model_registry.by_model_file("GEMMA-4-12B-IT-q4_k_m.GGUF") is gemma


@pytest.mark.parametrize(
    "lookup, unknown",
    [
        (model_registry.for_tier, "colossal"),
        (model_registry.for_tier, "cpu"),  # no model is registered for cpu
        (model_registry.tier_for_model, "not-a-real-file.gguf"),
        (model_registry.by_name, "Llama-Made-Up"),
        (model_registry.by_model_file, "not-a-registered-model.gguf"),
        (model_registry.by_model_file, ""),
    ],
    ids=["for_tier-unknown", "for_tier-cpu", "tier_for_model", "by_name",
         "by_model_file", "by_model_file-empty"],
)
def test_lookup_of_an_unknown_value_returns_none(lookup, unknown):
    assert lookup(unknown) is None


# ---------------------------------------------------------------------------
# Install-state probes
# ---------------------------------------------------------------------------

def test_a_missing_model_file_is_not_installed(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.is_installed(m, str(tmp_path)) is False
    assert model_registry.installed_size_gb(m, str(tmp_path)) is None
    r = model_registry.verify_installed(m, str(tmp_path))
    assert r["match"] == "missing"
    assert r["installed"] is False
    assert r["actual_sha256"] is None


@pytest.mark.parametrize(
    "size, installed",
    [(1024, False), (101 * 1024 * 1024, True)],
    ids=["1-KB-aborted-download", "just-over-100-MB"],
)
def test_is_installed_needs_a_file_over_100_mb(tmp_path, size, installed):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    with open(tmp_path / m.model_file, "wb") as f:
        f.truncate(size)  # a sparse file: this size, without the disk use
    assert model_registry.is_installed(m, str(tmp_path)) is installed


def test_installed_size_gb_reads_the_file_size(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    with open(tmp_path / m.model_file, "wb") as f:
        f.truncate(2 * 1024 ** 3)  # sparse 2 GB
    assert model_registry.installed_size_gb(m, str(tmp_path)) == 2.0


def test_compute_sha256_matches_known_value(tmp_path):
    p = tmp_path / "h.txt"
    p.write_bytes(b"hello world")
    assert model_registry.compute_sha256(str(p)) == (
        "b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7ace2efcde9")


def test_verify_installed_without_an_expected_hash_says_no_expected(tmp_path):
    """Entries with sha256=None (e.g., gated ones we couldn't HEAD)
    can't be verified end-to-end."""
    m = model_registry.by_name("Qwen3.5-7B-Q4_K_M")
    with open(tmp_path / m.model_file, "wb") as f:
        f.truncate(101 * 1024 * 1024)
    r = model_registry.verify_installed(m, str(tmp_path))
    assert r["match"] == "no-expected"
    assert r["installed"] is True


def test_verify_installed_detects_a_corrupt_file(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    with open(tmp_path / m.model_file, "wb") as f:
        f.truncate(101 * 1024 * 1024)  # zeros, not the real model
    r = model_registry.verify_installed(m, str(tmp_path))
    assert r["match"] == "mismatch"
    assert r["actual_sha256"] is not None
    assert r["actual_sha256"] != r["expected_sha256"]


# ---------------------------------------------------------------------------
# Lens artifact location and presence
# ---------------------------------------------------------------------------

def test_lens_artifact_dir_defaults_to_the_checkout(tmp_path, monkeypatch):
    monkeypatch.delenv("ATLAS_LENS_MODELS", raising=False)
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.lens_artifact_dir_for(m, str(tmp_path)) == str(
        tmp_path / "geometric-lens" / "geometric_lens" / "models")


def test_lens_artifact_dir_honors_atlas_lens_models_env(monkeypatch, tmp_path):
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.lens_artifact_dir_for(m, "/unused") == str(tmp_path)


def test_lens_artifact_dir_reads_compose_dotenv(monkeypatch, tmp_path):
    monkeypatch.delenv("ATLAS_LENS_MODELS", raising=False)
    lens_dir = tmp_path / "external-lens"
    (tmp_path / ".env").write_text(f"ATLAS_LENS_MODELS={lens_dir}\n")
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.lens_artifact_dir_for(m, str(tmp_path)) == str(lens_dir)


def test_a_model_without_lens_support_expects_no_lens_files(tmp_path):
    """Nothing was claimed, so there is nothing to check."""
    m = model_registry.by_name("Qwen3.5-14B-Q5_K_M")  # no-artifacts
    assert model_registry.lens_artifact_dir_for(m, str(tmp_path)) is None
    state = model_registry.lens_artifacts_present(m, str(tmp_path))
    assert state["ok"] is True
    assert state["expected_files"] == []


@pytest.mark.parametrize(
    "files_on_disk, ok, missing",
    [
        (["cost_field.pt", "model_identity.json"], True, []),
        ([], False, ["cost_field.pt", "model_identity.json"]),
    ],
    ids=["all-files-present", "no-files"],
)
def test_lens_artifacts_present_lists_missing_files(
        tmp_path, monkeypatch, files_on_disk, ok, missing):
    lens_dir = tmp_path / "lens-models"
    lens_dir.mkdir()
    for name in files_on_disk:
        (lens_dir / name).write_bytes(b"x")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(lens_dir))
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")

    state = model_registry.lens_artifacts_present(m, str(tmp_path))

    assert state["ok"] is ok
    assert state["missing_files"] == missing


# ---------------------------------------------------------------------------
# Download hint
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("kind", ["lens", "asa"])
def test_artifact_download_hint_names_the_install_command(kind):
    hint = model_registry.artifact_download_hint(
        "/models/gemma-4-12b-it-Q4_K_M.gguf", kind)
    assert "atlas model install-artifacts gemma-4-12b-it-Q4_K_M" in hint


@pytest.mark.parametrize(
    "model_file",
    [
        "unknown.gguf",
        "Qwen3.5-7B-Q4_K_M.gguf",  # no steering vector and no URL to fetch one
    ],
    ids=["unregistered-model", "nothing-published"],
)
def test_artifact_download_hint_is_empty_when_nothing_is_published(model_file):
    assert model_registry.artifact_download_hint(model_file, "asa") == ""
