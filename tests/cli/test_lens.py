"""Tests for atlas/commands/lens.py (PC-057 + PC-058).

Coverage strategy:
- `_check_model` and `_emit_check` are tested by patching `probe_llama` to
  return synthetic LlamaProbe records (no HTTP, no llama-server required).
- `_inspect_cost_field` is exercised against a real torch.save'd state
  dict so the pickle peek path is real, not mocked.
- `_load_training_samples` is tested against tmp_path-written JSON/JSONL.
- `_emit_build`'s training step is exercised in --dry-run mode to keep
  tests fast (no actual CostField training).
"""

import json
import os
from pathlib import Path

import pytest

from atlas import publishing
from atlas.commands import lens


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _probe(reachable=True, embedding_dim=4096, n_layers=32,
           model_name="test-model.gguf", patch=True, error=""):
    return lens.LlamaProbe(
        reachable=reachable,
        url="http://test-llama:8080",
        embedding_dim=embedding_dim,
        n_layers=n_layers,
        model_name=model_name,
        has_hidden_states_patch=patch,
        error=error,
    )


def _write_complete_runtime_artifacts(path, model="test-model",
                                      embedding_dim=4096):
    (path / "model_identity.json").write_text(json.dumps({
        "model": model,
        "embedding_dim": embedding_dim,
    }))
    (path / "cx_normalization.json").write_text(json.dumps({
        "midpoint": 5.0,
        "steepness": 0.5,
        "pass_energy_mean": 1.0,
        "fail_energy_mean": 9.0,
    }))
    (path / "gx_thresholds.json").write_text(json.dumps({
        "severe": 0.1,
        "off_rails": 0.2,
        "low": 0.3,
    }))
    (path / "gx_xgboost.json").write_text("{}")
    (path / "gx_weights.json").write_text("{}")


# ---------------------------------------------------------------------------
# _inspect_cost_field — real torch round-trip
# ---------------------------------------------------------------------------

def test_inspect_cost_field_dim_inferred(tmp_path):
    """A genuine save_cost_field artifact's input dim is recoverable."""
    torch = pytest.importorskip("torch")
    # Build a minimal state dict matching the CostField layout:
    # net.0 is the first Linear (out=512, in=DIM).
    DIM = 768  # something non-canonical to prove the inference is real
    state = {
        "net.0.weight": torch.zeros(512, DIM),
        "net.0.bias":   torch.zeros(512),
        "net.2.weight": torch.zeros(128, 512),
        "net.2.bias":   torch.zeros(128),
        "net.4.weight": torch.zeros(1, 128),
        "net.4.bias":   torch.zeros(1),
    }
    torch.save(state, tmp_path / "cost_field.pt")
    assert lens._inspect_cost_field(str(tmp_path)).dim == DIM


def test_inspect_cost_field_dim_missing_file(tmp_path):
    """No artifact file → returns None, not an exception."""
    assert lens._inspect_cost_field(str(tmp_path)).dim is None


# ---------------------------------------------------------------------------
# _check_model verdicts
# ---------------------------------------------------------------------------

def test_check_unreachable_server_is_incompatible(monkeypatch, tmp_path):
    monkeypatch.setattr(
        lens, "probe_llama",
        lambda *a, **kw: _probe(reachable=False, error="not reachable"))
    v = lens._check_model(None, str(tmp_path))
    assert v.verdict == "incompatible"
    assert v.exit_code == 2
    assert "not reachable" in v.reason


def test_check_zero_dim_is_incompatible(monkeypatch, tmp_path):
    """Server up but /embedding returned nothing -> incompatible."""
    monkeypatch.setattr(
        lens, "probe_llama",
        lambda *a, **kw: _probe(embedding_dim=0,
                                 error="embedding endpoint silent"))
    v = lens._check_model(None, str(tmp_path))
    assert v.verdict == "incompatible"


def test_check_rejects_requested_model_that_is_not_loaded(monkeypatch,
                                                           tmp_path):
    monkeypatch.setattr(
        lens, "probe_llama",
        lambda *a, **kw: _probe(model_name="loaded-model.gguf"),
    )
    verdict = lens._check_model("requested-model.gguf", str(tmp_path))
    assert verdict.verdict == "incompatible"
    assert "has 'loaded-model.gguf' loaded" in verdict.reason


def test_check_missing_artifact_is_needs_build(monkeypatch, tmp_path):
    """Probe OK, no cost_field.pt exists -> needs-build (exit 1)."""
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    v = lens._check_model(None, str(tmp_path))
    assert v.verdict == "needs-build"
    assert v.exit_code == 1
    assert "no cost_field.pt" in v.reason


def test_check_dim_mismatch_is_needs_build(monkeypatch, tmp_path):
    """Artifact exists but its input dim != model's embedding dim."""
    torch = pytest.importorskip("torch")
    state = {"net.0.weight": torch.zeros(512, 2048)}  # 2048-dim artifact
    torch.save(state, tmp_path / "cost_field.pt")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama",
                        lambda *a, **kw: _probe(embedding_dim=4096))
    v = lens._check_model(None, str(tmp_path))
    assert v.verdict == "needs-build"
    assert "Dim mismatch" in v.reason
    assert v.artifact_dim == 2048
    assert v.probe.embedding_dim == 4096


def test_check_dim_match_is_compat(monkeypatch, tmp_path):
    """Artifact dim matches model embedding dim -> compat (exit 0)."""
    torch = pytest.importorskip("torch")
    state = {"net.0.weight": torch.zeros(512, 4096)}
    torch.save(state, tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path)
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama",
                        lambda *a, **kw: _probe(embedding_dim=4096))
    v = lens._check_model(None, str(tmp_path))
    assert v.verdict == "compat"
    assert v.exit_code == 0
    assert v.artifact_dim == 4096


def test_check_dim_match_without_calibration_needs_build(monkeypatch, tmp_path):
    torch = pytest.importorskip("torch")
    torch.save({"net.0.weight": torch.zeros(512, 4096)},
               tmp_path / "cost_field.pt")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    verdict = lens._check_model(None, str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "cx_normalization.json" in verdict.reason


def test_check_rejects_present_but_invalid_calibration(monkeypatch, tmp_path):
    torch = pytest.importorskip("torch")
    torch.save({"net.0.weight": torch.zeros(512, 4096)},
               tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path)
    (tmp_path / "gx_thresholds.json").write_text(json.dumps({
        "severe": 0.4, "off_rails": 0.3, "low": 0.2,
    }))
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    verdict = lens._check_model(None, str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "gx_thresholds.json" in verdict.reason
    assert "invalid" in verdict.reason


def test_check_rejects_artifacts_for_same_dim_different_model(monkeypatch,
                                                               tmp_path):
    torch = pytest.importorskip("torch")
    torch.save({"net.0.weight": torch.zeros(512, 4096)},
               tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path, "other-model")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    verdict = lens._check_model(None, str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "model_identity.json" in verdict.reason
    assert "other-model" in verdict.reason


def test_check_rejects_the_q6k_bundle_for_another_quant(monkeypatch, tmp_path):
    """#247: a quant of the same model has the same embedding size, but a
    bundle built for Q6_K does not load for Q4_K_M. `atlas lens check`
    must say so, as the registry notes now do."""
    torch = pytest.importorskip("torch")
    torch.save({"net.0.weight": torch.zeros(512, 4096)},
               tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe(
        model_name="Qwen3.5-9B-Q4_K_M.gguf"))
    verdict = lens._check_model(None, str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "Qwen3.5-9B-Q6_K" in verdict.reason


def test_check_compat_warns_when_pc202_patch_missing(monkeypatch, tmp_path):
    """Compat verdict but no PC-202 patch -> reason mentions G(x) limitation."""
    torch = pytest.importorskip("torch")
    state = {"net.0.weight": torch.zeros(512, 4096)}
    torch.save(state, tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path)
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama",
                        lambda *a, **kw: _probe(patch=False))
    v = lens._check_model(None, str(tmp_path))
    assert v.verdict == "compat"
    assert "PC-202" in v.reason
    assert "G(x)" in v.reason


def test_activate_bundle_replaces_complete_set_and_removes_legacy(tmp_path):
    live = tmp_path / "live"
    staging = tmp_path / "staging"
    live.mkdir()
    staging.mkdir()
    (live / "cost_field.pt").write_bytes(b"old-cost")
    (live / "metric_tensor.pt").write_bytes(b"stale-metric")
    (live / "gx_xgboost.pkl").write_bytes(b"stale-pickle")
    (staging / "cost_field.pt").write_bytes(b"new-cost")
    _write_complete_runtime_artifacts(staging)

    lens._activate_lens_bundle(str(staging), str(live))

    assert (live / "cost_field.pt").read_bytes() == b"new-cost"
    assert (live / "model_identity.json").is_file()
    assert not (live / "metric_tensor.pt").exists()
    assert not (live / "gx_xgboost.pkl").exists()


def test_activate_incomplete_bundle_preserves_live_artifacts(tmp_path):
    live = tmp_path / "live"
    staging = tmp_path / "staging"
    live.mkdir()
    staging.mkdir()
    (live / "cost_field.pt").write_bytes(b"old-cost")
    (staging / "cost_field.pt").write_bytes(b"new-cost")

    with pytest.raises(ValueError, match="incomplete"):
        lens._activate_lens_bundle(str(staging), str(live))

    assert (live / "cost_field.pt").read_bytes() == b"old-cost"


# ---------------------------------------------------------------------------
# CLI JSON output shape — what scripts will key off
# ---------------------------------------------------------------------------

def test_check_json_output_has_stable_shape(monkeypatch, tmp_path, capsys):
    monkeypatch.setattr(lens, "probe_llama",
                        lambda *a, **kw: _probe(reachable=False, error="oops"))
    rc = lens.main(["check", "--json"])
    assert rc == 2
    payload = json.loads(capsys.readouterr().out)
    for key in ("verdict", "reason", "probe", "exit_code"):
        assert key in payload
    assert payload["exit_code"] == 2
    assert payload["verdict"] == "incompatible"
    # Probe nested object preserves the dataclass fields scripts may want
    for key in ("reachable", "url", "embedding_dim", "n_layers"):
        assert key in payload["probe"]


# ---------------------------------------------------------------------------
# Training sample loader — JSON + JSONL
# ---------------------------------------------------------------------------

def test_load_training_samples_json_array(tmp_path):
    path = tmp_path / "samples.json"
    path.write_text(json.dumps([
        {"text": "ok", "label": 1},
        {"text": "bad", "label": 0},
    ]))
    samples = lens._load_training_samples(str(path))
    assert len(samples) == 2
    assert samples[0]["label"] == 1


def test_load_training_samples_jsonl(tmp_path):
    path = tmp_path / "samples.jsonl"
    path.write_text(
        '{"text": "a", "label": 1}\n'
        '{"text": "b", "label": 0}\n'
        '\n'  # blank line tolerated
        '{"text": "c", "label": 1}\n'
    )
    samples = lens._load_training_samples(str(path))
    assert len(samples) == 3
    assert [s["text"] for s in samples] == ["a", "b", "c"]


def test_load_training_samples_missing_file(tmp_path):
    """Missing file returns empty list, not an exception."""
    assert lens._load_training_samples(str(tmp_path / "nope.json")) == []
    assert lens._load_training_samples(None) == []


# ---------------------------------------------------------------------------
# Build subcommand — early-exit + dry-run paths
# ---------------------------------------------------------------------------

def test_build_refuses_without_samples_flag(monkeypatch, tmp_path, capsys):
    """Build with no --samples and no artifacts -> usage error (rc=1)."""
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    rc = lens.main(["build", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "--samples" in out
    assert "huggingface" in out.lower()


def test_build_refuses_on_unreachable_server(monkeypatch, tmp_path, capsys):
    monkeypatch.setattr(
        lens, "probe_llama",
        lambda *a, **kw: _probe(reachable=False, error="server down"))
    samples_path = tmp_path / "samples.json"
    samples_path.write_text(json.dumps([
        {"text": "x", "label": 1}, {"text": "y", "label": 0},
    ]))
    rc = lens.main(["build", "--samples", str(samples_path), "--no-color"])
    assert rc == 2  # incompatible -> hard fail


def test_build_refuses_when_too_few_samples(monkeypatch, tmp_path, capsys):
    """<50 samples -> refuse (would produce a bad C(x))."""
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    samples_path = tmp_path / "samples.json"
    samples_path.write_text(json.dumps([
        {"text": f"sample {i}", "label": i % 2} for i in range(20)
    ]))
    rc = lens.main(["build", "--samples", str(samples_path),
                    "--force", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "samples loaded" in out.lower() or "samples." in out.lower() or "20" in out


def test_build_refuses_when_one_class_missing(monkeypatch, tmp_path, capsys):
    """All-pass or all-fail samples -> refuse (contrastive needs both)."""
    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    samples_path = tmp_path / "samples.json"
    # 60 PASS, 0 FAIL — passes count threshold, fails class-balance
    samples_path.write_text(json.dumps([
        {"text": f"sample {i}", "label": 1} for i in range(60)
    ]))
    # Stub the embedding extractor so we don't try to hit a real server
    monkeypatch.setattr(lens, "_extract_training_embeddings",
                        lambda *a, **kw: {"embeddings": [], "labels": []})
    rc = lens.main(["build", "--samples", str(samples_path),
                    "--force", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "both" in out.lower() or "contrastive" in out.lower()


def test_build_compat_with_no_force_is_noop(monkeypatch, tmp_path, capsys):
    """If artifacts already exist for the current dim, build exits 0 without
    requiring --samples. Forcing a retrain is opt-in via --force."""
    torch = pytest.importorskip("torch")
    state = {"net.0.weight": torch.zeros(512, 4096)}
    torch.save(state, tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path)
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(lens, "probe_llama",
                        lambda *a, **kw: _probe(embedding_dim=4096))
    rc = lens.main(["build", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "--force" in out


# ---------------------------------------------------------------------------
# Main dispatcher
# ---------------------------------------------------------------------------

def test_main_with_no_subcommand_shows_help(capsys):
    rc = lens.main([])
    out = capsys.readouterr().out
    assert rc == 1
    assert "check" in out and "build" in out and "publish" in out


# ---------------------------------------------------------------------------
# Publish subcommand (PC-059) — paths that don't require an HF token
# ---------------------------------------------------------------------------

def test_publish_refuses_when_no_artifact(monkeypatch, tmp_path, capsys):
    """No cost_field.pt in the artifact dir -> usage error (rc=1)."""
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "No cost_field.pt" in out
    assert "atlas lens build" in out


def test_publish_refuses_uncalibrated_partial_artifact(monkeypatch, tmp_path,
                                                       capsys):
    (tmp_path / "cost_field.pt").write_bytes(b"weights")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    rc = lens.main(["publish", "test-model", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "incomplete or uncalibrated" in out
    assert "cx_normalization.json" in out


def test_publish_refuses_invalid_calibration(monkeypatch, tmp_path, capsys):
    (tmp_path / "cost_field.pt").write_bytes(b"weights")
    _write_complete_runtime_artifacts(tmp_path)
    (tmp_path / "cx_normalization.json").write_text(json.dumps({
        "midpoint": 1.0,
        "steepness": False,
    }))
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    rc = lens.main(["publish", "test-model", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "calibration is invalid" in out
    assert "cx_normalization.json" in out


def test_publish_dry_run_prints_pr_body(monkeypatch, tmp_path, capsys):
    """Dry-run hashes the artifact, renders the PR body, prints it."""
    torch = pytest.importorskip("torch")
    state = {"net.0.weight": torch.zeros(512, 4096),
             "net.0.bias": torch.zeros(512)}
    torch.save(state, tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 0
    # PR body markdown should be present
    assert "Verification checklist" in out
    assert "Suggested registry diff" in out
    # SHA256 should appear (computed from the fake .pt above)
    assert "SHA256" in out or "sha256" in out
    # Should mention `atlas lens publish` (the tool that auto-generated it)
    assert "auto-generated" in out


def test_publish_dry_run_works_without_torch(monkeypatch, tmp_path, capsys):
    """Even without torch on the host, publish --dry-run should still
    print a usable PR body — just with dim shown as 'unverified'."""
    # Create a non-empty cost_field.pt that isn't a valid torch state dict.
    # The SHA-256 + size + path inspection paths don't need torch; only
    # the dim introspection does.
    (tmp_path / "cost_field.pt").write_bytes(b"fake pt content for sha256")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    # Force the dim-inspection to fail by stubbing _inspect_cost_field.
    monkeypatch.setattr(
        lens, "_inspect_cost_field",
        lambda d: lens.ArtifactInspection(present=True, dim=None,
                                           torch_available=False,
                                           error="torch missing"))
    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "unverified" in out  # dim_label fallback


def test_publish_requires_repo_unless_dry_run(monkeypatch, tmp_path, capsys):
    """No --repo + no --dry-run -> usage error explaining the requirement.

    We set HF_TOKEN here so the publish_preflight passes — we want to
    isolate the test to the --repo check, not the auth gate (covered by
    test_preflight_blocks_non_dryrun_when_token_missing)."""
    if not publishing.huggingface_hub_available():
        pytest.skip("huggingface_hub not installed on this host")
    (tmp_path / "cost_field.pt").write_bytes(b"fake")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setenv("HF_TOKEN", "hf_dummy_for_test")
    monkeypatch.setattr(
        lens, "_inspect_cost_field",
        lambda d: lens.ArtifactInspection(present=True, dim=4096))
    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "--repo" in out


def test_publish_requires_hf_token_when_uploading(monkeypatch, tmp_path, capsys):
    """--repo given but HF_TOKEN missing -> clean error pointing at the
    token settings page."""
    (tmp_path / "cost_field.pt").write_bytes(b"fake")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(
        lens, "_inspect_cost_field",
        lambda d: lens.ArtifactInspection(present=True, dim=4096))
    for k in ("HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "HUGGING_FACE_HUB_TOKEN"):
        monkeypatch.delenv(k, raising=False)
    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K",
                    "--repo", "alice/atlas-lens-test", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "HF_TOKEN" in out
    assert "huggingface.co/settings/tokens" in out


def test_publish_skip_pr_writes_body_without_gh(monkeypatch, tmp_path, capsys):
    """--skip-pr in dry-run mode should print the PR body unconditionally
    (no `gh` invocation, no upload). Sanity-check the exit code is 0."""
    (tmp_path / "cost_field.pt").write_bytes(b"fake")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setattr(
        lens, "_inspect_cost_field",
        lambda d: lens.ArtifactInspection(present=True, dim=4096))
    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K",
                    "--dry-run", "--skip-pr", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "Verification checklist" in out


def test_render_registry_pr_body_includes_required_fields():
    """Direct unit test on the renderer — guards against accidental
    field drops in the markdown template."""
    body = lens._render_registry_pr_body(
        model_name="TestModel-9B",
        hf_repo="alice/atlas-lens-test",
        base_model="TestModel 9B (Q6_K)",
        dim=4096,
        sha256="a" * 64,
        license_id="apache-2.0",
    )
    assert "TestModel-9B" in body
    assert "alice/atlas-lens-test" in body
    assert "apache-2.0" in body
    assert "a" * 64 in body
    # The maintainer needs the Python diff with `lens_status="supported"`
    assert 'lens_status="supported"' in body
    assert "lens_calibrated=True" in body


def test_registry_set_lens_upgrades_existing_entry():
    content = '''
@dataclass
class Model:
    lens_calibrated: bool = False
    lens_hf_repo: Optional[str] = None

REGISTRY: List[Model] = [
    Model(
        name="ExampleModel",
        tier="medium",
        lens_status="unverified",
        lens_calibrated=False,
        lens_artifact_files=["old.pt"],
        lens_hf_repo="owner/old-bundle",
    ),
]
'''
    updated = publishing.registry_set_lens(
        content,
        "ExampleModel",
        "owner/current-bundle",
        ["cost_field.pt", "cx_normalization.json", "gx_thresholds.json"],
    )
    assert updated is not None
    assert updated.count('lens_status="supported"') == 1
    assert updated.count("lens_calibrated=True") == 1
    assert 'lens_hf_repo="owner/current-bundle"' in updated
    assert "old.pt" not in updated
    assert "owner/old-bundle" not in updated


def test_registry_set_lens_returns_none_for_unknown_entry():
    assert publishing.registry_set_lens(
        "REGISTRY: List[Model] = [\n]\n",
        "MissingModel",
        "owner/bundle",
        ["cost_field.pt"],
    ) is None


def test_sha256_file_is_deterministic_and_correct(tmp_path):
    """_sha256_file should match what `sha256sum` would compute."""
    import hashlib
    content = b"deterministic content for sha test" * 100
    (tmp_path / "f.bin").write_bytes(content)
    expected = hashlib.sha256(content).hexdigest()
    assert publishing.sha256_file(str(tmp_path / "f.bin")) == expected


# ---------------------------------------------------------------------------
# publish pre-flight panel (PC-059.1 / publishing UX polish)
# ---------------------------------------------------------------------------

def test_preflight_dry_run_skips_auth_gates(monkeypatch, capsys):
    """--dry-run should always return True regardless of missing creds —
    the whole point is letting users preview without setting anything up."""
    for k in ("HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "HUGGING_FACE_HUB_TOKEN"):
        monkeypatch.delenv(k, raising=False)
    ok = publishing.publish_preflight("lens", dry_run=True, color=False)
    out = capsys.readouterr().out
    assert ok is True, "dry-run preflight should always pass"
    assert "submission pre-flight" in out
    assert "PUBLISHING.md" in out, "should reference the docs walkthrough"
    assert "--dry-run" in out, "should call out that auth gates are skipped"


def test_preflight_blocks_non_dryrun_when_token_missing(monkeypatch, capsys):
    for k in ("HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "HUGGING_FACE_HUB_TOKEN"):
        monkeypatch.delenv(k, raising=False)
    ok = publishing.publish_preflight("lens", dry_run=False, color=False)
    out = capsys.readouterr().out
    assert ok is False, "missing HF_TOKEN in real run should block"
    assert "HF_TOKEN" in out
    assert "huggingface.co/settings/tokens" in out


def test_preflight_passes_when_token_and_pkg_present(monkeypatch, capsys):
    monkeypatch.setenv("HF_TOKEN", "hf_dummy_for_test")
    # huggingface_hub may or may not be installed in CI; only assert pass
    # when the pkg is genuinely available (otherwise the test would be
    # flaky depending on host env).
    if not publishing.huggingface_hub_available():
        pytest.skip("huggingface_hub not installed on this host")
    ok = publishing.publish_preflight("lens", dry_run=False, color=False)
    out = capsys.readouterr().out
    assert ok is True
    assert "HF_TOKEN env var" in out


def test_preflight_mentions_both_lens_and_asa_kinds(monkeypatch, capsys):
    """The panel header should reflect whichever publish flow invoked it
    so the user isn't confused about what they're shipping."""
    monkeypatch.setenv("HF_TOKEN", "hf_dummy")
    publishing.publish_preflight("asa", dry_run=True, color=False)
    out = capsys.readouterr().out
    assert "atlas asa publish" in out
    publishing.publish_preflight("lens", dry_run=True, color=False)
    out = capsys.readouterr().out
    assert "atlas lens publish" in out


def test_preflight_gh_missing_is_optional_warning(monkeypatch, capsys):
    """Without gh installed the preflight should still pass — it just
    notes the user will paste the PR body manually."""
    monkeypatch.setenv("HF_TOKEN", "hf_dummy")
    monkeypatch.setattr(publishing, "gh_available", lambda: False)
    if not publishing.huggingface_hub_available():
        pytest.skip("huggingface_hub not installed on this host")
    ok = publishing.publish_preflight("lens", dry_run=False, color=False)
    out = capsys.readouterr().out
    assert ok is True, "missing gh should not block — paste fallback is valid"
    assert "gh CLI" in out
    assert "paste" in out.lower() or "compare" in out.lower()


def test_preflight_dry_run_doesnt_flag_missing_token_as_failure(
    monkeypatch, capsys
):
    """Regression: with --dry-run, missing HF_TOKEN must NOT render with
    a red ✗ or print the alarming 'required — get a token at ...' hint.
    Dry-run is the path users take BEFORE setting up creds; making it
    look like they did something wrong is the opposite of what we want."""
    for k in ("HF_TOKEN", "HUGGINGFACE_HUB_TOKEN", "HUGGING_FACE_HUB_TOKEN"):
        monkeypatch.delenv(k, raising=False)
    ok = publishing.publish_preflight("lens", dry_run=True, color=False)
    out = capsys.readouterr().out
    assert ok is True
    # The red ✗ marker must not appear next to HF_TOKEN in dry-run mode.
    # (We render the row with a neutral ○ instead.)
    assert "✗ HF_TOKEN" not in out, (
        "dry-run preflight should not show ✗ for missing token; "
        "got:\n" + out
    )
    # The alarming "Cannot continue" footer must not fire either.
    assert "Cannot continue" not in out
    # The dry-run footer should still be present and tell the user
    # nothing is being enforced.
    assert "nothing will leave the host" in out


def test_publish_opens_pr_via_github_api(monkeypatch, tmp_path, capsys):
    """The registry PR is built through `gh api` (branch + commit + PR) —
    never `gh pr create`, which needs a local git checkout most installs
    don't have (PC-214). A failing API call falls back to printing the
    PR body rather than failing the publish."""
    if not publishing.huggingface_hub_available():
        pytest.skip("huggingface_hub not installed on this host")
    (tmp_path / "cost_field.pt").write_bytes(b"fake")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    monkeypatch.setenv("HF_TOKEN", "hf_dummy_for_test")
    monkeypatch.setattr(
        lens, "_inspect_cost_field",
        lambda d: lens.ArtifactInspection(present=True, dim=4096))

    captured = []

    class _FakeResult:
        returncode = 1   # every gh api call "fails" -> body fallback
        stdout = ""
        stderr = "simulated offline"

    def _fake_run(cmd, *a, **kw):
        captured.append(cmd)
        return _FakeResult()

    import subprocess as _sp
    monkeypatch.setattr(_sp, "run", _fake_run)
    from huggingface_hub import HfApi
    monkeypatch.setattr(HfApi, "create_repo", lambda self, **kw: None)
    monkeypatch.setattr(HfApi, "upload_file", lambda self, **kw: None)

    rc = lens.main(["publish", "Qwen3.5-9B-Q6_K",
                    "--repo", "alice/atlas-lens-test", "--no-color"])
    assert rc == 0
    gh_calls = [c for c in captured if c and c[0] == "gh"]
    assert gh_calls, "expected gh api invocations"
    assert all(c[1] == "api" for c in gh_calls), \
        f"publish must use `gh api`, not gh pr create: {gh_calls}"
    out = capsys.readouterr().out
    # API path failed -> the PR body is printed for manual paste.
    assert "Add Lens artifacts" in out


# ---------------------------------------------------------------------------
# Drift fingerprint (#230): every built bundle carries one
# ---------------------------------------------------------------------------
#
# The lens re-scores fixed reference texts at boot and fails /ready when an
# energy drifts, but nothing wrote the fingerprint it compares against.

_REPO = Path(__file__).resolve().parents[2]


@pytest.fixture
def lens_drift(monkeypatch):
    """geometric_lens, importable the way the build imports it on a host."""
    monkeypatch.syspath_prepend(str(_REPO / "geometric-lens"))
    import geometric_lens.drift as drift
    return drift


def _tiny_cost_field(torch):
    torch.manual_seed(0)
    return torch.nn.Sequential(torch.nn.Linear(4, 1))


def _fake_embed(text):
    return [len(text) / 100.0, text.count("\n") / 10.0, 0.5, float("def" in text)]


def test_a_built_bundle_carries_a_fingerprint_scored_the_service_way(lens_drift, tmp_path):
    torch = pytest.importorskip("torch")
    model = _tiny_cost_field(torch)
    assert lens._write_bundle_fingerprint(str(tmp_path), model, embed=_fake_embed) is None

    def score(text):
        return float(model(torch.tensor(_fake_embed(text)).unsqueeze(0)).item())

    fp = lens_drift.load_fingerprint(str(tmp_path))
    assert [r["text"] for r in fp["references"]] == lens_drift.REFERENCE_TEXTS
    for ref in fp["references"]:
        assert abs(ref["expected_energy"] - score(ref["text"])) < 1e-6
    # The boot check accepts the same stack and reports a drifted one.
    assert lens_drift.check_fingerprint(str(tmp_path), score) == (True, True, "")
    present, ok, detail = lens_drift.check_fingerprint(str(tmp_path), lambda t: score(t) * 3 + 50)
    assert present and not ok and "drift" in detail


def test_a_reference_that_cannot_be_scored_leaves_no_fingerprint(lens_drift, tmp_path):
    torch = pytest.importorskip("torch")

    def unreachable(_text):
        raise ConnectionError("llama-server is not answering")

    warning = lens._write_bundle_fingerprint(str(tmp_path), _tiny_cost_field(torch),
                                             embed=unreachable)
    assert warning and "not written" in warning
    assert not (tmp_path / "drift_fingerprint.json").exists()
    # With no fingerprint the boot check enforces nothing: never a drift verdict.
    assert lens_drift.check_fingerprint(str(tmp_path), lambda t: 1.0) == (False, True, "")


def test_the_build_points_the_service_embedder_at_the_server_it_reached(
        lens_drift, tmp_path, monkeypatch):
    torch = pytest.importorskip("torch")
    import geometric_lens.embedding_extractor as ee
    seen = []

    def fake_extract(text):
        seen.append(ee._get_embed_url())
        return _fake_embed(text)

    monkeypatch.setattr(ee, "extract_embedding", fake_extract)
    monkeypatch.delenv("LLAMA_EMBED_URL", raising=False)
    monkeypatch.delenv("LLAMA_URL", raising=False)
    assert lens._write_bundle_fingerprint(str(tmp_path), _tiny_cost_field(torch),
                                          llama_url="http://localhost:8080") is None
    # Not the compose service name, which a host cannot resolve.
    assert seen and set(seen) == {"http://localhost:8080"}
    assert "LLAMA_EMBED_URL" not in os.environ


def test_the_fingerprint_moves_with_its_bundle(tmp_path):
    live, staging = tmp_path / "live", tmp_path / "staging"
    live.mkdir()
    staging.mkdir()
    (live / "cost_field.pt").write_bytes(b"old-cost")
    (live / "drift_fingerprint.json").write_text("old")
    (staging / "cost_field.pt").write_bytes(b"new-cost")
    _write_complete_runtime_artifacts(staging)
    (staging / "drift_fingerprint.json").write_text("new")

    lens._activate_lens_bundle(str(staging), str(live))

    assert (live / "drift_fingerprint.json").read_text() == "new"


def test_a_bundle_without_a_fingerprint_does_not_keep_the_old_one(tmp_path):
    # An old fingerprint describes the old weights: kept, it would read as drift.
    live, staging = tmp_path / "live", tmp_path / "staging"
    live.mkdir()
    staging.mkdir()
    (live / "cost_field.pt").write_bytes(b"old-cost")
    (live / "drift_fingerprint.json").write_text("old")
    (staging / "cost_field.pt").write_bytes(b"new-cost")
    _write_complete_runtime_artifacts(staging)

    lens._activate_lens_bundle(str(staging), str(live))

    assert not (live / "drift_fingerprint.json").exists()


def test_lens_build_activates_a_bundle_that_carries_its_fingerprint(
        lens_drift, tmp_path, monkeypatch):
    """`atlas lens build`, end to end with C(x) trained for real: the
    activated bundle has a fingerprint, and the provenance manifest hashes
    it. G(x) training (XGBoost + scikit-learn) and the model server are
    stand-ins."""
    torch = pytest.importorskip("torch")
    import geometric_lens.embedding_extractor as ee
    import geometric_lens.training as training

    samples = ([{"text": f"def ok_{i}(): return {i}", "label": 1} for i in range(30)]
               + [{"text": f"def bad_{i}(: return {i}", "label": 0} for i in range(30)])
    samples_path = tmp_path / "samples.json"
    samples_path.write_text(json.dumps(samples))

    def fake_extract(samples, llama_url, color, cache_path=None, expect_dim=0):
        return {"embeddings": [[0.1, 0.2, 0.1, 0.2] if s["label"] else [0.9, 0.8, 0.9, 0.8]
                               for s in samples],
                "labels": [int(s["label"]) for s in samples]}

    def fake_save_gx(_result, save_dir=None):
        (Path(save_dir) / "gx_xgboost.json").write_text("{}")
        (Path(save_dir) / "gx_weights.json").write_text("{}")
        (Path(save_dir) / "gx_thresholds.json").write_text(
            json.dumps({"severe": 0.1, "off_rails": 0.2, "low": 0.3}))

    monkeypatch.setattr(lens, "probe_llama", lambda *a, **kw: _probe(embedding_dim=4))
    monkeypatch.setattr(lens, "_extract_training_embeddings", fake_extract)
    monkeypatch.setattr(training, "train_gx", lambda data: {"cv_auc_mean": 0.9})
    monkeypatch.setattr(training, "save_gx", fake_save_gx)
    monkeypatch.setattr(ee, "observe_embedding_convention", lambda: None)
    monkeypatch.setattr(ee, "extract_embedding", lambda text: [0.1, 0.2, 0.3, 0.4])

    live = tmp_path / "live"
    rc = lens.main(["build", "--samples", str(samples_path), "--artifact-dir", str(live),
                    "--force", "--epochs", "3", "--no-color"])
    assert rc == 0
    fp = lens_drift.load_fingerprint(str(live))
    assert fp is not None and len(fp["references"]) == len(lens_drift.REFERENCE_TEXTS)
    manifest = (live / "provenance.json").read_text()
    assert "drift_fingerprint.json" in manifest


def test_publish_ships_the_drift_fingerprint(monkeypatch, tmp_path, capsys):
    """A registry consumer gets the fingerprint with the weights, so the
    drift check runs on their stack too."""
    torch = pytest.importorskip("torch")
    torch.save({"net.0.weight": torch.zeros(512, 4096), "net.0.bias": torch.zeros(512)},
               tmp_path / "cost_field.pt")
    _write_complete_runtime_artifacts(tmp_path, "Qwen3.5-9B-Q6_K")
    (tmp_path / "drift_fingerprint.json").write_text(json.dumps({
        "tolerance_pct": 15, "references": [{"text": "x", "expected_energy": 1.0}]}))
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(tmp_path))
    assert lens.main(["publish", "Qwen3.5-9B-Q6_K", "--dry-run", "--no-color"]) == 0
    assert "drift_fingerprint.json" in capsys.readouterr().out


def test_the_fingerprint_is_scored_under_the_bundles_embedding_contract(
        lens_drift, tmp_path, monkeypatch):
    """The service installs the bundle's embedding contract when it loads it,
    and a normalized contract changes every vector: the fingerprint must be
    scored the same way, or a fresh bundle reads as drifted at boot."""
    torch = pytest.importorskip("torch")
    import geometric_lens.embedding_extractor as ee
    from geometric_lens.identity import save_model_identity
    monkeypatch.setattr(ee, "_post_embedding",
                        lambda text, **kw: {"embedding": [3.0, 4.0, 0.0, 0.0]})
    save_model_identity(str(tmp_path), "test-model", 4, embedding_contract={
        "pooling": "mean", "response_shape": "flat", "normalized": True,
        "norm_tolerance": 0.05})
    model = _tiny_cost_field(torch)
    assert lens._write_bundle_fingerprint(str(tmp_path), model,
                                          llama_url="http://localhost:8080") is None
    # The service's vector: [3, 4, 0, 0] L2-normalized.
    want = float(model(torch.tensor([0.6, 0.8, 0.0, 0.0]).unsqueeze(0)).item())
    for ref in lens_drift.load_fingerprint(str(tmp_path))["references"]:
        assert abs(ref["expected_energy"] - want) < 1e-5
    assert ee._EMBEDDING_CONTRACT is None


def test_model_card_provenance_names_the_loss_the_bundle_was_trained_with(tmp_path):
    """A bundle from `atlas lens build` records the ranking margin; a bundle
    without that record must not be described as contrastive-trained."""
    built = tmp_path / "built"
    built.mkdir()
    (built / "provenance.json").write_text(json.dumps(
        {"hyperparameters": {"epochs": 100, "lr": 1e-3, "margin": 5.0}}))
    text = lens._model_card_provenance(str(built), "test-model", 4096)
    assert "atlas lens build" in text
    assert "Contrastive ranking loss" in text
    assert "MSE" not in text

    legacy = tmp_path / "legacy"
    legacy.mkdir()
    for d in (legacy, tmp_path / "missing"):
        text = lens._model_card_provenance(str(d), "test-model", 4096)
        assert "class-weighted" in text and "MSE" in text
        assert "Trained locally via `atlas lens build`" not in text


def test_model_card_embeds_the_provenance_paragraph():
    card = lens._render_model_card_md(
        "test-model", "test-model", 4096, "0" * 64, 1024, "apache-2.0",
        ["cost_field.pt"], provenance="PROVENANCE PARAGRAPH")
    assert "## Provenance\n\nPROVENANCE PARAGRAPH\n\n## License" in card
