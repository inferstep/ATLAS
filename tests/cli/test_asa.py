"""Tests for atlas/commands/asa.py (PC-061).

probe_llama() is monkey-patched to return synthetic LlamaProbe records (no
HTTP, no llama-server required). Tests that need a real GGUF file skip when
the gguf package is not installed. The training run itself is dry-run only:
the real run shells into the lens container, which isn't reproducible in CI.
"""

import base64
import importlib.util
import json
import subprocess
from pathlib import Path

import pytest

from atlas import publishing
from atlas.commands import asa, model_registry


def _probe(reachable=True, embedding_dim=4096, n_layers=32,
           model_name="test-model.gguf", patch=True, error=""):
    return asa.LlamaProbe(
        reachable=reachable, url="http://test-llama:8080",
        embedding_dim=embedding_dim, n_layers=n_layers, model_name=model_name,
        has_hidden_states_patch=patch, error=error,
    )


def _write_control_vector(path, dim, model_hint=None, layer_count=None):
    """A control-vector GGUF as build_steering_vector.py writes it: one
    direction tensor, whose length is the residual dim, plus metadata."""
    import gguf
    import numpy as np
    writer = gguf.GGUFWriter(str(path), arch="controlvector")
    if model_hint:
        writer.add_string("controlvector.model_hint", model_hint)
    if layer_count:
        writer.add_uint32("controlvector.layer_count", layer_count)
    writer.add_tensor("direction.27", np.zeros(dim, dtype=np.float32))
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()


def _fake_docker_run(cmd, *args, **kwargs):
    """Stands in for subprocess.run in the build tests: `docker inspect`
    reports the lens container as running, and every other command
    (docker cp, docker compose) succeeds with no output."""
    stdout = "true\n" if cmd[:2] == ["docker", "inspect"] else ""
    return subprocess.CompletedProcess(cmd, 0, stdout=stdout, stderr="")


# ---------------------------------------------------------------------------
# Path resolution
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("models_dir_set_in", [None, "environment", ".env"],
                         ids=["default", "env-var", "dotenv"])
def test_host_resolve_maps_container_models_path_to_host(
        tmp_path, monkeypatch, models_dir_set_in):
    """/models/x in the container is <models dir>/x on the host. The models
    dir is <atlas_root>/models unless ATLAS_MODELS_DIR points elsewhere."""
    monkeypatch.delenv("ATLAS_MODELS_DIR", raising=False)
    models_dir = tmp_path / ("models" if models_dir_set_in is None else "alt")
    models_dir.mkdir()
    vector = models_dir / "ast_edit_steering.gguf"
    vector.write_bytes(b"GGUF")
    if models_dir_set_in == "environment":
        monkeypatch.setenv("ATLAS_MODELS_DIR", str(models_dir))
    if models_dir_set_in == ".env":
        (tmp_path / ".env").write_text(f"ATLAS_MODELS_DIR={models_dir}\n")

    resolved = asa._host_resolve_vector_path(
        "/models/ast_edit_steering.gguf", str(tmp_path))

    assert resolved == str(vector)


def test_host_resolve_keeps_a_path_that_exists_on_the_host(tmp_path):
    vector = tmp_path / "foo.gguf"
    vector.write_bytes(b"GGUF")
    out = asa._host_resolve_vector_path(str(vector), "/whatever")
    assert out == str(vector)


def test_host_resolve_returns_the_configured_path_when_nothing_matches(
        tmp_path):
    """So the caller's error message names the path the operator set."""
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
    fake = tmp_path / "fake.gguf"
    fake.write_bytes(b"NOPE" + b"\x00" * 100)
    meta = asa._read_cvector_meta(str(fake))
    assert meta["present"] is True
    assert "not a GGUF" in meta["error"]
    assert meta["dim"] is None


def test_read_cvector_meta_real_gguf(tmp_path):
    pytest.importorskip("gguf")
    vector = tmp_path / "v.gguf"
    _write_control_vector(vector, dim=4096, model_hint="testarch",
                          layer_count=36)

    meta = asa._read_cvector_meta(str(vector))

    assert meta["present"] is True
    assert meta["dim"] == 4096
    assert meta["layer_count"] == 36
    assert meta["model_hint"] == "testarch"
    assert meta["error"] == ""


# ---------------------------------------------------------------------------
# atlas asa check verdicts
# ---------------------------------------------------------------------------

def test_check_missing_vector_is_needs_build(monkeypatch, tmp_path):
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(tmp_path / "nope.gguf"))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    verdict = asa._check_asa(str(tmp_path))
    assert verdict.verdict == "needs-build"
    assert verdict.exit_code == 1


@pytest.mark.parametrize("vector_dim, verdict, exit_code, reason_part", [
    (4096, "compat", 0, "matches model"),
    (2048, "needs-build", 1, "Dim mismatch"),
], ids=["same-dim-as-model", "other-dim-than-model"])
def test_check_compares_vector_dim_with_model_dim(
        monkeypatch, tmp_path, vector_dim, verdict, exit_code, reason_part):
    pytest.importorskip("gguf")
    vector = tmp_path / "v.gguf"
    _write_control_vector(vector, dim=vector_dim, layer_count=36)
    (tmp_path / "v.gguf.model").write_text("test-model\n")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vector))
    monkeypatch.setattr(asa, "probe_llama",
                        lambda *a, **kw: _probe(embedding_dim=4096))

    result = asa._check_asa(str(tmp_path))

    assert result.verdict == verdict
    assert result.exit_code == exit_code
    assert reason_part in result.reason
    assert result.vector_dim == vector_dim
    assert result.vector_layer_count == 36


def test_check_unverified_when_gguf_pkg_missing(monkeypatch, tmp_path):
    """Without the gguf package the host can't read the dim. The verdict
    stays compat (unverified): a host-tooling gap must not push users to
    rebuild."""
    vector = tmp_path / "v.gguf"
    vector.write_bytes(b"GGUF" + b"\x00" * 100)
    (tmp_path / "v.gguf.model").write_text("test-model\n")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vector))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setattr(asa, "_read_cvector_meta", lambda path: {
        "present": True, "size_bytes": 104, "dim": None,
        "layer_count": None, "model_hint": None,
        "error": "gguf python pkg not installed; dim unverified",
    })

    verdict = asa._check_asa(str(tmp_path))

    assert verdict.verdict == "compat"
    assert verdict.unverified is True
    assert "pip install gguf" in verdict.reason


@pytest.mark.parametrize("marker, reason_part", [
    (None, "entrypoint will keep it disabled"),
    ("other-model", "marked for 'other-model'"),
], ids=["no-model-marker", "marker-for-another-model"])
def test_check_needs_build_when_model_marker_does_not_match(
        monkeypatch, tmp_path, marker, reason_part):
    """The entrypoint loads a vector only when its <vector>.model marker
    names the loaded model (here test-model.gguf)."""
    vector = tmp_path / "v.gguf"
    vector.write_bytes(b"GGUF" + b"\x00" * 100)
    if marker:
        (tmp_path / "v.gguf.model").write_text(marker + "\n")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vector))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())

    verdict = asa._check_asa(str(tmp_path))

    assert verdict.verdict == "needs-build"
    assert reason_part in verdict.reason
    # test-model has no registry entry, so there is nothing to download.
    assert "install-artifacts" not in verdict.reason


def test_check_needs_build_points_at_published_artifacts(monkeypatch,
                                                         tmp_path):
    """When the registry has downloadable ASA artifacts for the loaded
    model, the needs-build reason offers the download, not only a local
    retrain."""
    vector = tmp_path / "v.gguf"
    vector.write_bytes(b"GGUF" + b"\x00" * 100)
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vector))
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe(
        model_name="/models/gemma-4-12b-it-Q4_K_M.gguf"))

    verdict = asa._check_asa(str(tmp_path))

    assert verdict.verdict == "needs-build"
    assert ("atlas model install-artifacts gemma-4-12b-it-Q4_K_M"
            in verdict.reason)


def test_check_json_output_for_unreachable_server(monkeypatch, capsys):
    monkeypatch.setattr(asa, "probe_llama",
                        lambda *a, **kw: _probe(reachable=False, error="oops"))

    rc = asa.main(["check", "--json"])

    payload = json.loads(capsys.readouterr().out)
    assert rc == 2
    for key in ("verdict", "reason", "probe", "vector_path",
                "vector_present", "exit_code"):
        assert key in payload, f"missing {key}"
    assert payload["verdict"] == "incompatible"
    assert payload["exit_code"] == 2


# ---------------------------------------------------------------------------
# atlas asa build
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("docker_on_path, probe, exit_code, message", [
    (False, _probe(), 1, "docker not on PATH"),
    (True, _probe(reachable=False, error="server down"), 2,
     "llama-server unreachable: server down"),
    # Without the PC-202 patch, build can't extract per-layer residuals.
    (True, _probe(patch=False), 2, "PC-202"),
], ids=["docker-missing", "server-unreachable", "pc202-patch-missing"])
def test_build_refuses_when_preflight_fails(
        monkeypatch, capsys, docker_on_path, probe, exit_code, message):
    monkeypatch.setattr(asa, "_docker_available", lambda: docker_on_path)
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: probe)

    rc = asa.main(["build", "--no-color"])

    assert rc == exit_code
    assert message in capsys.readouterr().out


def test_build_dry_run_removes_stale_output_and_staged_files(
        monkeypatch, tmp_path, capsys):
    """Before staging, build deletes any /tmp/ast_edit_steering.gguf left
    by an earlier failed run, so a new crash can't hand back the old vector
    as a fresh build. A dry run then removes the staged script and pairs
    from the container's /tmp."""
    # The default pairs file is generated, not checked in: pass --pairs.
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text(
        '{"prompt": "edit the file", "label": "edit_file"}\n'
        '{"prompt": "rewrite the function", "label": "structural_edit"}\n')
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe())
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    monkeypatch.setattr(subprocess, "run", _fake_docker_run)
    docker_exec_commands = []
    monkeypatch.setattr(
        asa, "_docker_exec",
        lambda container, cmd, capture=False: docker_exec_commands.append(cmd))

    rc = asa.main(["build", "--dry-run", "--no-color", "--pairs", str(pairs)])

    assert rc == 0
    assert "--layer 24" in capsys.readouterr().out  # 75% of 32 layers
    assert docker_exec_commands == [
        ["rm", "-f", "/tmp/ast_edit_steering.gguf"],
        ["rm", "-f", "/tmp/build_steering_vector.py"],
        ["rm", "-f", "/tmp/contrast_pairs.jsonl"],
    ]


def test_gguf_block_count_zero_on_non_gguf(tmp_path):
    junk = tmp_path / "junk.gguf"
    junk.write_bytes(b"NOPE" + b"\x00" * 64)
    assert asa._gguf_block_count(str(junk)) == 0
    assert asa._gguf_block_count(str(tmp_path / "missing.gguf")) == 0


def test_build_derives_layer_from_gguf_when_props_lacks_depth(
        monkeypatch, tmp_path, capsys):
    """Some llama-server builds omit n_layer from /props. Build then reads
    <arch>.block_count from the model GGUF on the host and derives
    layer = round(depth * 0.75), instead of demanding --layer."""
    gguf = pytest.importorskip("gguf")
    models = tmp_path / "models"
    models.mkdir()
    writer = gguf.GGUFWriter(str(models / "m.gguf"), arch="newarch")
    writer.add_uint32("newarch.block_count", 48)
    writer.write_header_to_file()
    writer.write_kv_data_to_file()
    writer.write_tensors_to_file()
    writer.close()
    pairs = tmp_path / "pairs.jsonl"
    pairs.write_text(
        '{"prompt": "edit the file", "label": "edit_file"}\n'
        '{"prompt": "rewrite the function", "label": "structural_edit"}\n')
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe(
        n_layers=0, model_name="/models/m.gguf"))
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    monkeypatch.setattr(asa, "_docker_exec", lambda *a, **kw: None)
    monkeypatch.setattr(subprocess, "run", _fake_docker_run)
    # ATLAS_MODELS_DIR maps /models/m.gguf to tmp_path; _atlas_root stays
    # real so the staged build script is found.
    monkeypatch.setenv("ATLAS_MODELS_DIR", str(models))

    rc = asa.main(["build", "--dry-run", "--no-color", "--pairs", str(pairs)])

    out = capsys.readouterr().out
    assert rc == 0
    assert "model depth 48 read from GGUF header" in out
    assert "--layer 36" in out


def test_build_still_asks_for_layer_when_gguf_unreadable(
        monkeypatch, tmp_path, capsys):
    """No depth in /props and no readable model GGUF on the host: refuse
    and ask for --layer instead of guessing."""
    monkeypatch.setattr(asa, "probe_llama", lambda *a, **kw: _probe(
        n_layers=0, model_name="/models/not-here.gguf"))
    monkeypatch.setattr(asa, "_docker_available", lambda: True)
    monkeypatch.setattr(asa, "_atlas_root", lambda: str(tmp_path))
    monkeypatch.setattr(subprocess, "run", _fake_docker_run)

    rc = asa.main(["build", "--no-color"])

    assert rc == 2
    assert "--layer" in capsys.readouterr().out


# ---------------------------------------------------------------------------
# atlas asa publish
# ---------------------------------------------------------------------------

def test_publish_requires_repo_unless_dry_run(monkeypatch, capsys):
    """HF_TOKEN is set so the auth check passes and only --repo is
    missing."""
    if not publishing.huggingface_hub_available():
        pytest.skip("huggingface_hub not installed on this host")
    monkeypatch.setenv("HF_TOKEN", "hf_dummy_for_test")
    rc = asa.main(["publish", "--no-color"])
    assert rc == 1
    assert "--repo" in capsys.readouterr().out


def test_publish_refuses_when_no_vector(monkeypatch, tmp_path, capsys):
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(tmp_path / "nope.gguf"))
    rc = asa.main(["publish", "--dry-run", "--no-color"])
    assert rc == 1
    assert "No control vector" in capsys.readouterr().out


def test_publish_dry_run_prints_pr_body(monkeypatch, tmp_path, capsys):
    vector = tmp_path / "v.gguf"
    vector.write_bytes(b"GGUF" + b"\x00" * 100)
    (tmp_path / "v.gguf.model").write_text("TestModel-9B\n")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vector))

    rc = asa.main(["publish", "TestModel-9B", "--dry-run", "--no-color"])

    out = capsys.readouterr().out
    assert rc == 0
    assert "Verification checklist" in out
    assert "Provenance" in out
    assert "asa_artifact_files" in out


def test_publish_refuses_vector_marked_for_another_model(monkeypatch, tmp_path,
                                                          capsys):
    vector = tmp_path / "v.gguf"
    vector.write_bytes(b"GGUF" + b"\x00" * 100)
    (tmp_path / "v.gguf.model").write_text("other-model\n")
    monkeypatch.setenv("ATLAS_CONTROL_VECTOR", str(vector))

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


def test_publishing_records_a_vector_as_unverified():
    """Publishing shows a vector exists, not that it helps. The registry
    edit writes "unverified"; "supported" is promoted by hand with an A/B
    result."""
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


# A registry file from before Model had lens_calibrated and lens_hf_repo.
REGISTRY_BEFORE_LENS_FIELDS = """\
from dataclasses import dataclass, field
from typing import List, Optional


@dataclass(frozen=True)
class Model:
    name: str
    tier: str
    model_file: str
    model_display: str
    model_size_gb: float
    lens_status: str
    download_url: Optional[str] = None
    sha256: Optional[str] = None
    license: Optional[str] = None
    lens_artifact_files: List[str] = field(default_factory=list)
    notes: str = ""


REGISTRY: List[Model] = [
]
"""


@pytest.mark.parametrize("registry_source", [
    pytest.param(Path(model_registry.__file__).read_text(), id="current-registry"),
    pytest.param(REGISTRY_BEFORE_LENS_FIELDS, id="registry-before-lens-fields"),
])
def test_publish_adds_an_importable_registry_entry(tmp_path, registry_source):
    """`atlas publish` and `atlas lens publish` commit this edit upstream, so
    the file must still import, with the new model inside REGISTRY, even
    when the upstream Model is older than the publisher's install."""
    entry = publishing.render_registry_entry(
        "New-Model-Q4", "New-Model-Q4.gguf", 5.2, "medium", 4096,
        "alice/new-model-lens", "apache-2.0", ["cost_field.safetensors"])

    edited = publishing.registry_insert_entry(registry_source, "New-Model-Q4",
                                              entry)

    edited_file = tmp_path / "edited_registry.py"
    edited_file.write_text(edited)
    spec = importlib.util.spec_from_file_location("edited_registry", edited_file)
    edited_registry = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(edited_registry)
    added = [m for m in edited_registry.REGISTRY if m.name == "New-Model-Q4"]
    assert [m.model_file for m in added] == ["New-Model-Q4.gguf"]


def test_registry_pr_from_a_contributor_goes_through_their_fork(monkeypatch):
    """A contributor cannot push to inferstep/ATLAS: the edit is committed to
    a branch of their fork, and the PR asks to merge <login>:<branch> into
    dev."""
    monkeypatch.delenv("ATLAS_UPSTREAM_REPO", raising=False)
    registry_file = publishing.REGISTRY_PATH
    replies = {
        "user": "alice",
        "repos/inferstep/ATLAS/branches/dev": {"name": "dev"},
        "repos/inferstep/ATLAS/forks": {"full_name": "alice/ATLAS"},
        "repos/alice/ATLAS/merge-upstream": {},
        "repos/alice/ATLAS/branches/dev": {"commit": {"sha": "base-sha"}},
        f"repos/alice/ATLAS/contents/{registry_file}?ref=dev": {
            "content": base64.b64encode(b"REGISTRY = []\n").decode(),
            "sha": "old-blob"},
        "repos/alice/ATLAS/git/refs": {},
        f"repos/alice/ATLAS/contents/{registry_file}": {},
        "repos/inferstep/ATLAS/pulls": {
            "html_url": "https://github.com/inferstep/ATLAS/pull/9"},
    }
    sent = {}

    def fake_gh_api(api_args, payload=None):
        sent[api_args[0]] = payload
        return True, replies[api_args[0]], ""

    monkeypatch.setattr(publishing, "gh_api", fake_gh_api)

    url = publishing.open_registry_pr_via_api(
        "New-Model", "Registry: add New-Model", "body",
        edit_fn=lambda content: content + "# new entry\n")

    assert url == "https://github.com/inferstep/ATLAS/pull/9"
    commit = sent[f"repos/alice/ATLAS/contents/{registry_file}"]
    assert (base64.b64decode(commit["content"]).decode()
            == "REGISTRY = []\n# new entry\n")
    assert commit["sha"] == "old-blob"
    pull_request = sent["repos/inferstep/ATLAS/pulls"]
    assert pull_request["head"] == "alice:" + commit["branch"]
    assert pull_request["base"] == "dev"


def test_model_marker_value_strips_loaded_model_path():
    assert (publishing.model_marker_value("/models/Example-Model.gguf")
            == "Example-Model")


def test_main_with_no_subcommand_shows_help(capsys):
    rc = asa.main([])
    out = capsys.readouterr().out
    assert rc == 1
    assert "check" in out and "build" in out and "publish" in out
