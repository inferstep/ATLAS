"""Tests for atlas/commands/asa.py (PC-061).

Coverage strategy mirrors test_lens.py: probe_llama() is monkey-patched
to return synthetic LlamaProbe records (no HTTP, no llama-server
required), and _read_cvector_meta() is exercised against real on-disk
GGUF files when possible. The training run itself is dry-run only —
the real run shells into the lens container which isn't reproducible in
CI.
"""

import json

import pytest

from atlas import publishing
from atlas.commands import asa


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def _probe(reachable=True, embedding_dim=4096, n_layers=32,
           model_name="test-model.gguf", patch=True, error=""):
    return asa.LlamaProbe(
        reachable=reachable,
        url="http://test-llama:8080",
        embedding_dim=embedding_dim,
        n_layers=n_layers,
        model_name=model_name,
        has_hidden_states_patch=patch,
        error=error,
    )


def _write_model_marker(vector_path, model="test-model"):
    (vector_path.parent / (vector_path.name + ".model")).write_text(model + "\n")


# ---------------------------------------------------------------------------
# Path resolution
# ---------------------------------------------------------------------------

def test_host_resolve_translates_container_path(tmp_path, monkeypatch):
    """/models/foo.gguf on container -> <atlas_root>/models/foo.gguf on host."""
    models = tmp_path / "models"
    models.mkdir()
    (models / "ast_edit_steering.gguf").write_bytes(b"GGUF" + b"\x00" * 100)
    resolved = asa._host_resolve_vector_path(
        "/models/ast_edit_steering.gguf", str(tmp_path))
    assert resolved == str(models / "ast_edit_steering.gguf")


def test_host_resolve_honors_atlas_models_dir(tmp_path, monkeypatch):
    """ATLAS_MODELS_DIR overrides the default <atlas_root>/models location."""
    alt = tmp_path / "alt-models"
    alt.mkdir()
    (alt / "ast_edit_steering.gguf").write_bytes(b"GGUF" + b"\x00" * 100)
    monkeypatch.setenv("ATLAS_MODELS_DIR", str(alt))
    resolved = asa._host_resolve_vector_path(
        "/models/ast_edit_steering.gguf", str(tmp_path))
    assert resolved == str(alt / "ast_edit_steering.gguf")


def test_host_resolve_reads_models_dir_from_dotenv(tmp_path, monkeypatch):
    monkeypatch.delenv("ATLAS_MODELS_DIR", raising=False)
    alt = tmp_path / "dotenv-models"
    alt.mkdir()
    vector = alt / "ast_edit_steering.gguf"
    vector.write_bytes(b"GGUF" + b"\x00" * 100)
    (tmp_path / ".env").write_text(f"ATLAS_MODELS_DIR={alt}\n")
    resolved = asa._host_resolve_vector_path(
        "/models/ast_edit_steering.gguf", str(tmp_path))
    assert resolved == str(vector)


def test_host_resolve_passthrough_when_path_resolves(tmp_path):
    """Already-absolute paths that exist on disk shouldn't be munged."""
    f = tmp_path / "foo.gguf"
    f.write_bytes(b"GGUF")
    out = asa._host_resolve_vector_path(str(f), "/whatever")
    assert out == str(f)


def test_host_resolve_returns_original_when_nothing_works(tmp_path):
    """Last-resort: return the input so the caller's error message has
    the originally-configured path (not a fabricated one)."""
    out = asa._host_resolve_vector_path("/nope/missing.gguf", str(tmp_path))
    assert out == "/nope/missing.gguf"


# ---------------------------------------------------------------------------
# GGUF inspection
# ---------------------------------------------------------------------------

def test_read_cvector_meta_missing_file(tmp_path):
    meta = asa._read_cvector_meta(str(tmp_path / "nope.gguf"))
    assert meta["present"] is False
    assert "not found" in meta["error"]


def test_read_cvector_meta_non_gguf_magic(tmp_path):
    p = tmp_path / "fake.gguf"
    p.write_bytes(b"NOPE" + b"\x00" * 100)
    meta = asa._read_cvector_meta(str(p))
    assert meta["present"] is True
    assert "not a GGUF" in meta["error"]
    assert meta["dim"] is None


def test_read_cvector_meta_real_gguf(tmp_path):
    """Use the gguf package to write a minimal control-vector GGUF that
    matches what build_steering_vector.py produces, then assert we can
    introspect dim + model_hint + layer_count."""
    pytest.importorskip("gguf")
    pytest.importorskip("numpy")
    import gguf
    import numpy as np

    out_path = tmp_path / "v.gguf"
    writer = gguf.GGUFWriter(str(out_path), arch="controlvector")
    writer.add_string("controlvector.model_hint", "testarch")
    writer.add_uint32("controlvector.layer_count", 36)
    vec = np.zeros(4096, dtype=np.float32)
    writer.add_tensor("direction.27", vec)
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()

    meta = asa._read_cvector_meta(str(out_path))
    assert meta["present"] is True
    assert meta["dim"] == 4096
    assert meta["layer_count"] == 36
    assert meta["model_hint"] == "testarch"
    assert meta["error"] == ""


# ---------------------------------------------------------------------------
# atlas asa check verdicts
# ---------------------------------------------------------------------------

def test_check_unreachable_is_incompatible(monkeypatch, tmp_path):
    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(reachable=False, error="not reachable"))
    v = asa._check_asa(str(tmp_path))
    assert v.verdict == "incompatible"
    assert v.exit_code == 2


def test_check_missing_vector_is_needs_build(monkeypatch, tmp_path):
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(tmp_path / "nope.gguf"))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    v = asa._check_asa(str(tmp_path))
    assert v.verdict == "needs-build"
    assert v.exit_code == 1


def test_check_vector_present_dim_match_is_compat(monkeypatch, tmp_path):
    pytest.importorskip("gguf")
    pytest.importorskip("numpy")
    import gguf
    import numpy as np
    vp = tmp_path / "v.gguf"
    writer = gguf.GGUFWriter(str(vp), arch="controlvector")
    writer.add_string("controlvector.model_hint", "qwen3")
    writer.add_uint32("controlvector.layer_count", 36)
    writer.add_tensor("direction.27", np.zeros(4096, dtype=np.float32))
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()
    _write_model_marker(vp)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(asa, "probe_llama",
                        lambda *a, **kw: _probe(embedding_dim=4096))
    v = asa._check_asa(str(tmp_path))
    assert v.verdict == "compat"
    assert v.exit_code == 0
    assert v.vector_dim == 4096
    assert v.vector_layer_count == 36


def test_check_dim_mismatch_is_needs_build(monkeypatch, tmp_path):
    pytest.importorskip("gguf")
    pytest.importorskip("numpy")
    import gguf
    import numpy as np
    vp = tmp_path / "v.gguf"
    writer = gguf.GGUFWriter(str(vp), arch="controlvector")
    writer.add_tensor("direction.27", np.zeros(2048, dtype=np.float32))
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()
    _write_model_marker(vp)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(asa, "probe_llama",
                        lambda *a, **kw: _probe(embedding_dim=4096))
    v = asa._check_asa(str(tmp_path))
    assert v.verdict == "needs-build"
    assert "Dim mismatch" in v.reason
    assert v.vector_dim == 2048


def test_check_unverified_when_gguf_pkg_missing(monkeypatch, tmp_path):
    """File exists, magic bytes valid, but no gguf pkg -> unverified=True
    but verdict stays compat (don't push users to needs-build over a
    host-tooling gap)."""
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    _write_model_marker(vp)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    # Stub _read_cvector_meta to simulate gguf-missing case
    monkeypatch.setattr(asa, "_read_cvector_meta", lambda p: {
        "present": True, "size_bytes": 104, "dim": None,
        "layer_count": None, "model_hint": None,
        "error": "gguf python pkg not installed; dim unverified",
    })
    v = asa._check_asa(str(tmp_path))
    assert v.verdict == "compat"
    assert v.unverified is True
    assert "gguf" in v.reason.lower() or "verification" in v.reason.lower()


def test_check_present_vector_without_marker_is_not_compatible(monkeypatch,
                                                                tmp_path):
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setattr(asa, "_read_cvector_meta", lambda p: {
        "present": True, "size_bytes": 104, "dim": 4096,
        "layer_count": None, "model_hint": None, "error": "",
    })
    verdict = asa._check_asa(str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "entrypoint will keep it disabled" in verdict.reason


def test_check_rejects_marker_for_another_model(monkeypatch, tmp_path):
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    _write_model_marker(vp, "other-model")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setattr(asa, "_read_cvector_meta", lambda p: {
        "present": True, "size_bytes": 104, "dim": 4096,
        "layer_count": None, "model_hint": None, "error": "",
    })
    verdict = asa._check_asa(str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "other-model" in verdict.reason


def test_model_marker_value_strips_loaded_model_path():
    assert (publishing.model_marker_value("/models/Example-Model.gguf")
            == "Example-Model")


def test_check_needs_build_points_at_published_artifacts(monkeypatch, tmp_path):
    """When the loaded model has a registry entry with downloadable ASA
    artifacts, the needs-build reason must offer the download path — not
    only the local retrain."""
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(
            model_name="/models/gemma-4-12b-it-Q4_K_M.gguf"))
    monkeypatch.setattr(asa, "_read_cvector_meta", lambda p: {
        "present": True, "size_bytes": 104, "dim": 4096,
        "layer_count": None, "model_hint": None, "error": "",
    })
    verdict = asa._check_asa(str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "atlas model install-artifacts gemma-4-12b-it-Q4_K_M" \
        in verdict.reason


def test_check_needs_build_no_hint_for_unregistered_model(monkeypatch,
                                                          tmp_path):
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setattr(asa, "_read_cvector_meta", lambda p: {
        "present": True, "size_bytes": 104, "dim": 4096,
        "layer_count": None, "model_hint": None, "error": "",
    })
    verdict = asa._check_asa(str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert "install-artifacts" not in verdict.reason


def test_check_json_output_shape(monkeypatch, tmp_path, capsys):
    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(reachable=False, error="oops"))
    rc = asa.main(["check", "--json"])
    payload = json.loads(capsys.readouterr().out)
    assert rc == 2
    for key in ("verdict", "reason", "probe", "vector_path",
                "vector_present", "exit_code"):
        assert key in payload, f"missing {key}"
    assert payload["exit_code"] == 2


# ---------------------------------------------------------------------------
# atlas asa build — pre-flight guards
# ---------------------------------------------------------------------------

def test_build_refuses_on_unreachable_server(monkeypatch, capsys):
    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(reachable=False, error="server down"))
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    rc = asa.main(["build", "--no-color"])
    assert rc == 2


def test_build_refuses_when_pc202_patch_missing(monkeypatch, capsys):
    """No PC-202 patch -> can't extract per-layer residuals -> refuse."""
    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(patch=False))
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    rc = asa.main(["build", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 2
    assert "PC-202" in out


def test_build_refuses_when_docker_missing(monkeypatch, capsys):
    monkeypatch.setattr(asa, "_docker_available", lambda: False)
    rc = asa.main(["build", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "docker not on PATH" in out or "docker" in out.lower()


# ---------------------------------------------------------------------------
# GGUF-header depth fallback (llama-server builds without n_layer in /props)
# ---------------------------------------------------------------------------

def _write_minimal_model_gguf(path, arch="testarch", block_count=48):
    """A model GGUF carrying only general.architecture and
    <arch>.block_count — the two keys the depth fallback reads."""
    pytest.importorskip("gguf")
    import gguf
    writer = gguf.GGUFWriter(str(path), arch=arch)
    writer.add_uint32(f"{arch}.block_count", block_count)
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()


def test_gguf_block_count_reads_arch_prefixed_depth(tmp_path):
    p = tmp_path / "m.gguf"
    _write_minimal_model_gguf(p, arch="newarch", block_count=48)
    assert asa._gguf_block_count(str(p)) == 48


def test_gguf_block_count_zero_on_non_gguf(tmp_path):
    p = tmp_path / "junk.gguf"
    p.write_bytes(b"NOPE" + b"\x00" * 64)
    assert asa._gguf_block_count(str(p)) == 0
    assert asa._gguf_block_count(str(tmp_path / "missing.gguf")) == 0


def test_build_derives_layer_from_gguf_when_props_lacks_depth(
        monkeypatch, tmp_path, capsys):
    """Some llama-server builds omit n_layer from /props. Build must fall
    back to <arch>.block_count from the model GGUF on the host and derive
    layer = round(depth * 0.75) instead of demanding --layer."""
    models = tmp_path / "models"
    models.mkdir()
    _write_minimal_model_gguf(models / "m.gguf", arch="newarch",
                              block_count=48)

    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(n_layers=0, model_name="/models/m.gguf"))
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    # ATLAS_MODELS_DIR steers /models/* host resolution to tmp_path;
    # _atlas_root stays real so the staged build script resolves.
    monkeypatch.setenv("ATLAS_MODELS_DIR", str(models))
    import subprocess as _sp
    monkeypatch.setattr(_sp, "run", _fake_subprocess_run_for_build())
    monkeypatch.setattr(
        asa, "_docker_exec",
        lambda *a, **kw: type("R", (), {"returncode": 0, "stdout": "",
                                        "stderr": ""})())
    pairs = _make_fixture_pairs(tmp_path)

    rc = asa.main(["build", "--dry-run", "--no-color",
                   "--pairs", str(pairs)])
    out = capsys.readouterr().out
    assert rc == 0
    assert "model depth 48 read from GGUF header" in out
    assert "--layer 36" in out


def test_build_still_asks_for_layer_when_gguf_unreadable(
        monkeypatch, tmp_path, capsys):
    """No /props depth AND no readable model GGUF on the host: refuse
    with the explicit --layer instruction rather than guessing."""
    monkeypatch.setattr(
        asa, "probe_llama",
        lambda *a, **kw: _probe(n_layers=0,
                                model_name="/models/not-here.gguf"))
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    monkeypatch.setattr(asa, "_atlas_root", lambda: str(tmp_path))
    import subprocess as _sp
    monkeypatch.setattr(_sp, "run", _fake_subprocess_run_for_build())
    rc = asa.main(["build", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 2
    assert "--layer" in out


# ---------------------------------------------------------------------------
# PC-061 round-2: stale-output + cleanup behavior in _emit_build
# ---------------------------------------------------------------------------

def _fake_subprocess_run_for_build():
    """Return a subprocess.run substitute that satisfies asa build's
    pre-flight checks (docker inspect must report "true" for container
    running) and succeeds for docker cp. Used by the cleanup-behavior
    tests below."""
    def _run(cmd, *a, **kw):
        # docker inspect for container state — must return "true" to
        # pass the container-running pre-flight.
        if isinstance(cmd, list) and len(cmd) >= 2 \
                and cmd[0] == "docker" and cmd[1] == "inspect":
            return type("R", (), {"returncode": 0,
                                  "stdout": "true\n", "stderr": ""})()
        # Everything else (docker cp) just succeeds with empty stdout.
        return type("R", (), {"returncode": 0,
                              "stdout": "", "stderr": ""})()
    return _run


def _make_fixture_pairs(tmp_path):
    """Write a minimal contrast-pairs JSONL to tmp_path and return its
    path. The default pairs file (geometric-lens/asa_calibration/
    contrast_pairs.jsonl) is generated by generate_pairs.py and is NOT
    checked into the repo — CI runners don't have it. Tests that
    exercise `atlas asa build` must pass --pairs explicitly."""
    p = tmp_path / "pairs.jsonl"
    p.write_text(
        '{"prompt": "edit the file", "label": "edit_file"}\n'
        '{"prompt": "rewrite the function", "label": "structural_edit"}\n'
    )
    return p


def test_build_dry_run_cleans_up_staged_files(monkeypatch, tmp_path, capsys):
    """--dry-run shouldn't leave the staged script + pairs in the
    container's /tmp. Verify _docker_exec gets called with `rm -f` for
    both staged paths before we return."""
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    import subprocess as _sp
    monkeypatch.setattr(_sp, "run", _fake_subprocess_run_for_build())
    pairs = _make_fixture_pairs(tmp_path)

    docker_exec_calls = []

    def _spy_docker_exec(container, cmd, capture=False):
        docker_exec_calls.append((container, cmd))
        return type("R", (), {"returncode": 0,
                              "stdout": "", "stderr": ""})()

    monkeypatch.setattr(asa, "_docker_exec", _spy_docker_exec)
    rc = asa.main(["build", "--dry-run", "--no-color",
                   "--pairs", str(pairs)])
    assert rc == 0
    assert "--layer 24" in capsys.readouterr().out
    rm_targets = [cmd[-1] for _, cmd in docker_exec_calls
                  if cmd[:2] == ["rm", "-f"]]
    assert "/tmp/build_steering_vector.py" in rm_targets, (
        "dry-run must clean up the staged script — saw rm calls for: "
        f"{rm_targets}"
    )
    assert "/tmp/contrast_pairs.jsonl" in rm_targets, (
        "dry-run must clean up staged pairs — saw rm calls for: "
        f"{rm_targets}"
    )


def test_build_pre_run_nukes_stale_output(monkeypatch, tmp_path, capsys):
    """Regression for the round-2 blocker: a stale
    /tmp/ast_edit_steering.gguf from a prior failed run must be deleted
    BEFORE the new training fires. Otherwise a fresh crash that writes
    nothing would let `docker cp` return the stale vector as if it were
    a new build."""
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    import subprocess as _sp
    monkeypatch.setattr(_sp, "run", _fake_subprocess_run_for_build())
    pairs = _make_fixture_pairs(tmp_path)

    docker_exec_calls = []

    def _spy(container, cmd, capture=False):
        docker_exec_calls.append((container, cmd))
        return type("R", (), {"returncode": 0,
                              "stdout": "", "stderr": ""})()

    monkeypatch.setattr(asa, "_docker_exec", _spy)
    asa.main(["build", "--dry-run", "--no-color",
              "--pairs", str(pairs)])
    gguf_rm_calls = [cmd for _, cmd in docker_exec_calls
                     if cmd == ["rm", "-f", "/tmp/ast_edit_steering.gguf"]]
    assert len(gguf_rm_calls) >= 1, (
        "expected `rm -f /tmp/ast_edit_steering.gguf` to fire before "
        "training to prevent stale-output corruption; "
        f"saw docker_exec calls: {docker_exec_calls}"
    )


# ---------------------------------------------------------------------------
# atlas asa publish — early-exit paths
# ---------------------------------------------------------------------------

def test_publish_requires_repo_unless_dry_run(monkeypatch, tmp_path, capsys):
    """--repo required when actually uploading. Set HF_TOKEN so the
    publish_preflight passes — we want to isolate this test to the --repo
    enforcement, not the auth gate."""
    if not publishing.huggingface_hub_available():
        import pytest
        pytest.skip("huggingface_hub not installed on this host")
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    monkeypatch.setenv("HF_TOKEN", "hf_dummy_for_test")
    rc = asa.main(["publish", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "--repo" in out


def test_publish_refuses_when_no_vector(monkeypatch, tmp_path, capsys):
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(tmp_path / "nope.gguf"))
    rc = asa.main(["publish", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    assert "No control vector" in out


def test_publish_dry_run_prints_pr_body(monkeypatch, tmp_path, capsys):
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    _write_model_marker(vp, "TestModel-9B")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    rc = asa.main(["publish", "TestModel-9B", "--dry-run", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 0
    assert "Verification checklist" in out
    assert "Provenance" in out
    assert "asa_artifact_files" in out


def test_publish_refuses_vector_marked_for_another_model(monkeypatch, tmp_path,
                                                          capsys):
    vp = tmp_path / "v.gguf"
    vp.write_bytes(b"GGUF" + b"\x00" * 100)
    _write_model_marker(vp, "other-model")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vp))
    rc = asa.main(["publish", "TestModel-9B", "--dry-run", "--no-color"])
    assert rc == 1
    assert "Refusing to mislabel" in capsys.readouterr().out


def test_render_asa_pr_body_includes_required_fields():
    body = asa._render_asa_pr_body(
        model_name="TestModel-9B",
        hf_repo="alice/atlas-asa-test",
        base_model="TestModel 9B (Q6_K)",
        dim=4096, layer=27,
        sha256="a" * 64,
        license_id="apache-2.0",
    )
    assert "TestModel-9B" in body
    assert "alice/atlas-asa-test" in body
    assert "apache-2.0" in body
    assert "a" * 64 in body
    assert 'asa_status="unverified"' in body
    assert "layer 27" in body
    assert "4096-dim" in body


def test_main_with_no_subcommand_shows_help(capsys):
    rc = asa.main([])
    out = capsys.readouterr().out
    assert rc == 1
    assert "check" in out and "build" in out and "publish" in out


def test_publishing_records_a_vector_as_unverified():
    """Publishing shows a vector exists, not that it helps. The registry
    edit writes "unverified"; "supported" is promoted by hand with an A/B
    result."""
    from atlas import publishing
    content = (
        "REGISTRY = [\n"
        "    Model(\n"
        '        name="TestModel-9B",\n'
        '        asa_status="no-artifacts",\n'
        "    ),\n"
        "]\n"
    )
    out = publishing.registry_set_asa(content, "TestModel-9B",
                                      "alice/atlas-asa-test",
                                      ["ast_edit_steering.gguf"])
    assert out is not None
    assert 'asa_status="unverified"' in out
    assert 'asa_status="supported"' not in out
