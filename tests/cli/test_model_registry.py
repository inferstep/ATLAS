"""Tests for atlas.commands.model_registry (PC-056).

Covers:
  - REGISTRY shape + lens_status truth (only 9B is supported today)
  - for_tier / tier_for_model / by_name lookups
  - is_installed / installed_size_gb file-system probe
  - can_install reflects download_url
  - supported_models / models_for_tier filters
  - stable public field names on the Model record
"""

from atlas.commands import model_registry


# ---------------------------------------------------------------------------
# Registry shape — locks in the Phase 0 truth
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
    supported = model_registry.supported_models()
    names = {m.name for m in supported}
    assert names == {"Qwen3.5-9B-Q6_K", "gemma-4-12b-it-Q4_K_M"}


def test_only_9b_quants_are_publicly_installable():
    """PC-056.1: gated entries got download_urls populated (so
    HF_TOKEN-authenticated users CAN install them) but they're flagged
    requires_hf_token. The "publicly installable without auth" set is
    just the three 9B quants."""
    public = [m for m in model_registry.REGISTRY
               if m.can_install and not m.requires_hf_token]
    public_names = {m.name for m in public}
    assert public_names == {"Qwen3.5-9B-Q4_K_M", "Qwen3.5-9B-Q6_K",
                              "Qwen3.5-9B-Q8_0"}


def test_9b_has_verified_download_metadata():
    """Sanity: the 9B entry has a working download URL, a sensible size,
    and the SHA256 we captured from HF's x-linked-etag."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert m is not None
    assert m.download_url and m.download_url.startswith("https://huggingface.co/")
    assert 6.0 < m.model_size_gb < 8.0  # 6.94 GB observed
    assert m.sha256 is not None and len(m.sha256) == 64


def test_no_artifacts_models_have_explanatory_notes():
    """If lens_status == 'no-artifacts', the user needs to understand
    why. Blank notes would be a UX failure."""
    for m in model_registry.REGISTRY:
        if m.lens_status == "no-artifacts":
            assert m.notes, f"{m.name} has no notes explaining no-artifacts"
            assert "no-artifacts" in m.notes.lower() or \
                   "no lens artifacts" in m.notes.lower() or \
                   "g(x)" in m.notes.lower() or \
                   "silently no-op" in m.notes.lower()


# ---------------------------------------------------------------------------
# PC-061 round-2: Model accepts asa_* schema fields
# ---------------------------------------------------------------------------

def test_model_accepts_asa_status_and_artifact_files():
    """Regression for the round-2 blocker: `atlas asa publish`'s PR body
    suggests a registry diff using `asa_status=...` and
    `asa_artifact_files=[...]`. If the dataclass doesn't accept those
    kwargs, a maintainer pasting the diff verbatim gets a TypeError.

    This test guards the schema by constructing a Model with exactly
    the kwargs the PR-body template uses."""
    m = model_registry.Model(
        name="Test-9B",
        tier="medium",
        model_file="Test-9B.gguf",
        model_display="Test 9B",
        model_size_gb=6.0,
        lens_status="supported",
        asa_status="supported",
        asa_artifact_files=["ast_edit_steering.gguf"],
    )
    assert m.asa_status == "supported"
    assert m.asa_artifact_files == ["ast_edit_steering.gguf"]


def test_asa_status_defaults_to_no_artifacts():
    """Existing registry entries that predate PC-061 didn't pass
    asa_status. Default should keep them honest as 'no-artifacts' rather
    than silently claiming support."""
    m = model_registry.Model(
        name="Test-Pre-PC061",
        tier="small",
        model_file="x.gguf",
        model_display="x",
        model_size_gb=1.0,
        lens_status="no-artifacts",
    )
    assert m.asa_status == "no-artifacts"
    assert m.asa_artifact_files == []


def test_qwen_9b_q6k_claims_asa_supported():
    """The canonical ATLAS model has both lens + ASA artifacts in the
    repo and published to HF. Lock in that the registry reflects this."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert m is not None
    assert m.asa_status == "supported"
    assert "ast_edit_steering.gguf" in m.asa_artifact_files


def test_gemma_asa_is_unverified_until_measured():
    """The gemma vector was built from prompts that named the tool ast_edit
    (now structural_edit) and was never A/B measured on gemma. The label
    says only that; it does not switch steering off."""
    m = model_registry.by_name("gemma-4-12b-it-Q4_K_M")
    assert m is not None
    assert m.asa_status == "unverified"
    assert "ast_edit_steering.gguf" in m.asa_artifact_files


def test_grammar_mode_is_a_property_of_the_model():
    """Gemma under the strict schema grammar emits `done` instead of
    calling tools, and every recorded gemma measurement ran loose. The
    registry says so, and env_vars carries it so an install writes it."""
    gemma = model_registry.by_name("gemma-4-12b-it-Q4_K_M")
    assert gemma.grammar_mode == "loose"
    assert gemma.env_vars()["ATLAS_GRAMMAR_MODE"] == "loose"
    for m in model_registry.REGISTRY:
        assert m.grammar_mode in ("strict", "loose"), m.name
        if not m.name.startswith("gemma"):
            assert m.grammar_mode == "strict", m.name
            assert m.env_vars()["ATLAS_GRAMMAR_MODE"] == "strict", m.name


# ---------------------------------------------------------------------------
# Lookups
# ---------------------------------------------------------------------------

def test_for_tier_prefers_supported():
    """When several tier entries exist, prefer one with published weights."""
    m = model_registry.for_tier("medium")
    assert m is not None
    assert m.lens_status == "supported"


def test_for_tier_falls_back_to_first_when_no_supported():
    """When no tier-matched entry is supported, return the first match
    (caller renders the warning)."""
    m = model_registry.for_tier("xlarge")
    assert m is not None
    assert m.tier == "xlarge"
    assert m.lens_status == "no-artifacts"


def test_for_tier_unknown_returns_none():
    assert model_registry.for_tier("colossal") is None
    assert model_registry.for_tier("cpu") is None  # no model registered for cpu


def test_tier_for_model_roundtrips():
    """Every registered model's file resolves back to its tier."""
    for m in model_registry.REGISTRY:
        assert model_registry.tier_for_model(m.model_file) == m.tier


def test_tier_for_model_unknown_returns_none():
    assert model_registry.tier_for_model("not-a-real-file.gguf") is None


def test_by_name_roundtrips():
    for m in model_registry.REGISTRY:
        assert model_registry.by_name(m.name) is m


def test_by_name_unknown_returns_none():
    assert model_registry.by_name("Llama-Made-Up") is None


def test_models_for_tier_returns_only_matches():
    """Medium tier: the three 9B quants plus gemma-4-12b (PC-215)."""
    medium = model_registry.models_for_tier("medium")
    assert all(m.tier == "medium" for m in medium)
    assert len(medium) == 4


# ---------------------------------------------------------------------------
# Install-state probe
# ---------------------------------------------------------------------------

def test_is_installed_false_for_missing_file(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.is_installed(m, str(tmp_path)) is False


def test_is_installed_false_for_too_small_file(tmp_path):
    """100 MB sanity threshold guards against aborted-download zero-byte
    files showing as installed."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    p = tmp_path / m.model_file
    p.write_bytes(b"x" * 1024)  # 1 KB — way under the 100 MB threshold
    assert model_registry.is_installed(m, str(tmp_path)) is False


def test_is_installed_true_for_large_enough_file(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    p = tmp_path / m.model_file
    # Use sparse-file trick: seek + write 1 byte → file appears as
    # whatever offset we picked, without actually consuming disk.
    with open(p, "wb") as f:
        f.seek(101 * 1024 * 1024)  # just past 100 MB
        f.write(b"\0")
    assert model_registry.is_installed(m, str(tmp_path)) is True


def test_installed_size_gb_returns_size_when_present(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    p = tmp_path / m.model_file
    with open(p, "wb") as f:
        f.seek(2 * 1024 ** 3 - 1)  # 2 GB - 1 byte
        f.write(b"\0")
    sz = model_registry.installed_size_gb(m, str(tmp_path))
    assert sz is not None
    assert 1.9 < sz < 2.1


def test_installed_size_gb_returns_none_when_absent(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.installed_size_gb(m, str(tmp_path)) is None


# ---------------------------------------------------------------------------
# Stable public API
# ---------------------------------------------------------------------------

def test_tier_for_model_known_file():
    assert model_registry.tier_for_model("Qwen3.5-9B-Q6_K.gguf") == "medium"


def test_model_exposes_stable_field_names():
    """Callers do `rec.model_file`, `rec.model_display`,
    `rec.model_size_gb` — the Model record keeps those exact field
    names stable."""
    rec = model_registry.for_tier("medium")
    assert hasattr(rec, "model_file")
    assert hasattr(rec, "model_display")
    assert hasattr(rec, "model_size_gb")
    assert hasattr(rec, "tier")
    # And new fields are also visible (callers can opt in)
    assert hasattr(rec, "lens_status")
    assert hasattr(rec, "download_url")


# ---------------------------------------------------------------------------
# PC-056.1 schema additions: 9B variants, commit-pinned URLs, requires_hf_token
# ---------------------------------------------------------------------------

def test_pc0561_registry_now_has_seven_entries():
    """PC-056.1 added Q4_K_M and Q8_0 variants of the 9B; PC-215 added
    gemma-4-12b via the publish flow. Adding more is a deliberate scope
    change — flag it loudly here."""
    assert len(model_registry.REGISTRY) == 7


def test_pc0561_three_quants_for_9b():
    """All three 9B quants present — Q4_K_M (smaller), Q6_K (default
    supported), Q8_0 (higher quality)."""
    nine_quants = {m.name for m in model_registry.REGISTRY
                    if m.tier == "medium" and m.name.startswith("Qwen3.5-9B")}
    assert nine_quants == {"Qwen3.5-9B-Q4_K_M", "Qwen3.5-9B-Q6_K",
                            "Qwen3.5-9B-Q8_0"}


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


def test_a_model_without_a_bundle_says_how_to_build_one():
    """With the lens required, a model with no bundle stops agent work.
    Its notes must say so and name the way out, not promise a silent
    no-op."""
    for m in model_registry.REGISTRY:
        if m.lens_status != "no-artifacts":
            continue
        notes = m.notes.lower()
        assert "atlas lens build" in notes, f"{m.name}: no build path in notes"
        assert "stops agent work" in notes, f"{m.name}: notes hide the stop"
        assert "silently no-op" not in notes, f"{m.name}: stale no-op claim"


def test_pc0561_for_tier_medium_still_picks_supported_q6k():
    """With multiple medium-tier entries (Q4 no-artifacts, Q6 supported,
    Q8 no-artifacts), for_tier must pick the SUPPORTED one — that's why
    the 'prefer supported' rule was important."""
    assert model_registry.for_tier("medium").name == "Qwen3.5-9B-Q6_K"


def test_pc0561_urls_pinned_to_commit_hash():
    """download_urls must NOT include `/main/` (PC-056 mistake) — they
    must be pinned to a specific commit so SHA256 stays valid even if
    upstream re-uploads with the same filename."""
    for m in model_registry.REGISTRY:
        if m.download_url is None:
            continue
        assert "/main/" not in m.download_url, (
            f"{m.name} URL not commit-pinned: {m.download_url}")
        # Should contain the unsloth Qwen3.5 commit hash
        assert "/3885219b" in m.download_url, (
            f"{m.name} not pinned to expected commit: {m.download_url}")


def test_pc0561_gated_entries_have_url_and_flag():
    """PC-056.1: gated entries got download_url populated (so HF_TOKEN
    users can install) AND requires_hf_token=True (so anonymous users
    get the helpful message before even trying)."""
    for name in ("Qwen3.5-7B-Q4_K_M", "Qwen3.5-14B-Q5_K_M",
                  "Qwen3.5-32B-Q5_K_M"):
        m = model_registry.by_name(name)
        assert m.download_url is not None, (
            f"{name} should now have a populated URL (PC-056.1)")
        assert m.requires_hf_token is True, (
            f"{name} should be flagged requires_hf_token=True")
        assert m.is_gated is True
        # gated still means can_install is True (URL is present); the
        # gate is at install-time, not in the registry property.
        assert m.can_install is True


def test_pc0561_supported_entry_does_not_require_hf_token():
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert m.requires_hf_token is False
    assert m.is_gated is False


def test_pc0561_supported_model_has_lens_artifact_files():
    """The supported 9B-Q6_K must declare which files prove its
    'supported' claim — doctor cross-checks against this list."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert "cost_field.pt" in m.lens_artifact_files


# ---------------------------------------------------------------------------
# PC-056.1 SHA verification helpers
# ---------------------------------------------------------------------------

def test_compute_sha256_matches_known_value(tmp_path):
    """Computed hash must equal the known SHA of 'hello world'."""
    p = tmp_path / "h.txt"
    p.write_bytes(b"hello world")
    expected = ("b94d27b9934d3e08a52e52d7da7dabfac484efe37a5380ee9088f7a"
                "ce2efcde9")
    assert model_registry.compute_sha256(str(p)) == expected


def test_verify_installed_missing_returns_missing(tmp_path):
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    r = model_registry.verify_installed(m, str(tmp_path))
    assert r["match"] == "missing"
    assert r["installed"] is False
    assert r["actual_sha256"] is None


def test_verify_installed_no_expected_returns_no_expected(tmp_path):
    """Models with sha256=None (e.g., gated entries we couldn't HEAD)
    can't be verified end-to-end. Status should be 'no-expected'."""
    m = model_registry.by_name("Qwen3.5-7B-Q4_K_M")
    p = tmp_path / m.model_file
    with open(p, "wb") as f:
        f.seek(101 * 1024 * 1024)
        f.write(b"\0")
    r = model_registry.verify_installed(m, str(tmp_path))
    assert r["match"] == "no-expected"
    assert r["installed"] is True


def test_verify_installed_mismatch_detects_corruption(tmp_path):
    """A file with unexpected contents must report mismatch."""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    p = tmp_path / m.model_file
    with open(p, "wb") as f:
        f.seek(101 * 1024 * 1024)
        f.write(b"\0")
    r = model_registry.verify_installed(m, str(tmp_path))
    assert r["match"] == "mismatch"
    assert r["actual_sha256"] is not None
    assert r["actual_sha256"] != r["expected_sha256"]


# ---------------------------------------------------------------------------
# PC-056.1 Lens artifact resolution + presence
# ---------------------------------------------------------------------------

def test_lens_artifact_dir_for_uses_default_when_unset(tmp_path):
    """Default resolution: <atlas_root>/geometric-lens/geometric_lens/models"""
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    d = model_registry.lens_artifact_dir_for(m, str(tmp_path))
    assert d.endswith("geometric-lens/geometric_lens/models")


def test_lens_artifact_dir_for_returns_none_for_unsupported_model():
    """No expectation = no path — caller should treat None as 'don't check'."""
    m = model_registry.by_name("Qwen3.5-7B-Q4_K_M")  # no-artifacts
    assert model_registry.lens_artifact_dir_for(m, "/some/root") is None


def test_lens_artifact_dir_for_honors_atlas_lens_models_env(monkeypatch, tmp_path):
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    d = model_registry.lens_artifact_dir_for(m, "/unused")
    assert d == str(tmp_path)


def test_lens_artifact_dir_for_reads_compose_dotenv(monkeypatch, tmp_path):
    monkeypatch.delenv("ATLAS_LENS_MODELS", raising=False)
    artifact_dir = tmp_path / "external-lens"
    (tmp_path / ".env").write_text(
        f"ATLAS_LENS_MODELS={artifact_dir}\n")
    m = model_registry.by_name("Qwen3.5-9B-Q6_K")
    assert model_registry.lens_artifact_dir_for(m, str(tmp_path)) == str(
        artifact_dir)


def test_lens_artifacts_present_ok_when_files_exist(tmp_path):
    """Set up a fake artifact dir with the required .pt files, point
    ATLAS_LENS_MODELS at it, expect ok=True."""
    art_dir = tmp_path / "lens-models"
    art_dir.mkdir()
    (art_dir / "cost_field.pt").write_bytes(b"x")
    (art_dir / "model_identity.json").write_text('{"model": "Qwen3.5-9B-Q6_K", "embedding_dim": 4096}')
    import os
    os.environ["ATLAS_LENS_MODELS"] = str(art_dir)
    try:
        m = model_registry.by_name("Qwen3.5-9B-Q6_K")
        state = model_registry.lens_artifacts_present(m, str(tmp_path))
        assert state["ok"] is True
        assert state["missing_files"] == []
    finally:
        del os.environ["ATLAS_LENS_MODELS"]


def test_lens_artifacts_present_missing_files_listed(tmp_path):
    """No .pt files at the expected path → both should be in missing_files."""
    import os
    os.environ["ATLAS_LENS_MODELS"] = str(tmp_path / "nonexistent")
    try:
        m = model_registry.by_name("Qwen3.5-9B-Q6_K")
        state = model_registry.lens_artifacts_present(m, str(tmp_path))
        assert state["ok"] is False
        assert "cost_field.pt" in state["missing_files"]
    finally:
        del os.environ["ATLAS_LENS_MODELS"]


def test_lens_artifacts_present_skips_unsupported_models(tmp_path):
    """Models without lens_status='supported' should always return ok=True
    (nothing was claimed; nothing to verify)."""
    m = model_registry.by_name("Qwen3.5-14B-Q5_K_M")  # no-artifacts
    state = model_registry.lens_artifacts_present(m, str(tmp_path))
    assert state["ok"] is True
    assert state["expected_files"] == []


def test_by_model_file_matches_basename_and_case():
    m = model_registry.by_model_file("/models/gemma-4-12b-it-Q4_K_M.gguf")
    assert m is not None and m.name == "gemma-4-12b-it-Q4_K_M"
    assert model_registry.by_model_file("GEMMA-4-12B-IT-q4_k_m.GGUF") is m \
        or model_registry.by_model_file("GEMMA-4-12B-IT-q4_k_m.GGUF").name \
        == m.name
    assert model_registry.by_model_file("not-a-registered-model.gguf") is None
    assert model_registry.by_model_file("") is None


def test_artifact_download_hint_for_supported_kinds():
    hint = model_registry.artifact_download_hint(
        "/models/gemma-4-12b-it-Q4_K_M.gguf", "asa")
    assert "atlas model install-artifacts gemma-4-12b-it-Q4_K_M" in hint
    assert model_registry.artifact_download_hint(
        "gemma-4-12b-it-Q4_K_M.gguf", "lens") != ""
    assert model_registry.artifact_download_hint("unknown.gguf", "asa") == ""


def test_artifact_download_hint_empty_without_urls(monkeypatch):
    """A registry entry whose kind isn't 'supported' with a URL base must
    produce no hint — nothing downloadable was claimed."""
    from dataclasses import replace
    entry = model_registry.by_name("gemma-4-12b-it-Q4_K_M")
    stripped = replace(entry, asa_status="no-artifacts",
                       asa_artifact_url_base=None)
    monkeypatch.setattr(model_registry, "REGISTRY", [stripped])
    assert model_registry.artifact_download_hint(
        "gemma-4-12b-it-Q4_K_M.gguf", "asa") == ""
