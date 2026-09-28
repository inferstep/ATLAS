"""Tests for atlas.commands.init (PC-054) — the first-run install wizard.

The wizard is a thin composer over tier + model_registry + model.install,
so these tests focus on the wizard's own behavior:

  - --yes happy path writes .env + api-keys.json with expected values
  - --reconfigure backs up existing .env before overwriting
  - already-configured guard refuses without --reconfigure
  - --skip-download produces config without calling install
  - --dry-run touches no files
  - --json shape is stable for the bootstrap script
  - api-keys.json permissions are 0600 + parent 0700
  - reconfigure of api-keys.json backs up the existing one
"""

import json
import os
import pathlib
import stat

import pytest

from atlas.commands import init, tier


# The wizard refuses on cpu tier (PC-054) — correct production behavior,
# but it means the happy-path tests below would all return rc=1 on a
# CPU-only host (e.g. GitHub runners). This autouse fixture mocks the
# probe to a GPU-equipped host so tests proceed past the GPU guard.
# The explicit cpu-refusal test (test_refuses_on_cpu_tier) overrides
# this with its own monkeypatch.
@pytest.fixture(autouse=True)
def _mock_gpu_probe(monkeypatch):
    gpu_probe = tier.Probe(
        has_gpu=True, gpu_name="NVIDIA Test GPU", vram_gb=24.0, gpu_count=1,
        system_ram_gb=64.0, cpu_cores=16, disk_free_gb=500.0, platform="linux")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: gpu_probe)


def _make_atlas_root(tmp_path) -> str:
    """Create a fake atlas_root with a docker-compose.yml so _find_atlas_root
    walks up from tmp_path and lands here. Returns absolute path."""
    root = tmp_path / "atlas_root"
    root.mkdir()
    (root / "docker-compose.yml").write_text("# fake compose for tests\n")
    return str(root)


def _run(monkeypatch, atlas_root, argv):
    """Run init.main with CWD set to atlas_root so atlas_root resolution
    finds the fake docker-compose.yml."""
    monkeypatch.chdir(atlas_root)
    return init.main(argv)


# ---------------------------------------------------------------------------
# Happy path
# ---------------------------------------------------------------------------

def test_yes_skip_download_writes_env_and_keys(tmp_path, monkeypatch, capsys):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0

    env_path = os.path.join(root, ".env")
    keys_path = os.path.join(root, "secrets", "api-keys.json")
    assert os.path.isfile(env_path)
    assert os.path.isfile(keys_path)

    body = pathlib.Path(env_path).read_text()
    # Wizard must write every key the compose stack reads at boot.
    for key in ("ATLAS_MODELS_DIR", "ATLAS_MODEL_FILE", "ATLAS_MODEL_NAME",
                "ATLAS_CTX_SIZE", "ATLAS_GHCR_OWNER", "ATLAS_IMAGE_TAG",
                "ATLAS_LLAMA_PORT", "ATLAS_PARALLEL_SLOTS",
                "ATLAS_KV_TYPE_K", "ATLAS_KV_TYPE_V",
                "ATLAS_BACKEND", "ATLAS_GPU_VENDOR", "ATLAS_GPU_INDEX",
                "ATLAS_PROXY_UID", "ATLAS_PROXY_GID", "ATLAS_GRAMMAR_MODE"):
        assert f"{key}=" in body, f"missing {key} in .env"
    # The grammar mode is the chosen model's, from the registry.
    model_file = next(line.split("=", 1)[1] for line in body.splitlines()
                      if line.startswith("ATLAS_MODEL_FILE="))
    from atlas.commands import model_registry
    chosen = model_registry.by_model_file(model_file)
    assert chosen is not None
    assert f"ATLAS_GRAMMAR_MODE={chosen.grammar_mode}" in body

    # Proxy runs as the invoking user so it can write the /workspace bind
    # mount (the image's baked-in uid 1001 can't).
    assert f"ATLAS_PROXY_UID={os.getuid()}" in body
    assert f"ATLAS_PROXY_GID={os.getgid()}" in body

    # Default models_dir is ./models when it equals atlas_root/models.
    assert "ATLAS_MODELS_DIR=./models" in body
    assert "ATLAS_IMAGE_TAG=latest" in body
    assert "ATLAS_GHCR_OWNER=inferstep" in body
    assert "ATLAS_MACOS_PREFIX=~/.atlas/macos" in body


def test_api_keys_file_has_strict_permissions(tmp_path, monkeypatch):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0

    keys_dir = os.path.join(root, "secrets")
    keys_path = os.path.join(keys_dir, "api-keys.json")
    dir_mode = stat.S_IMODE(os.stat(keys_dir).st_mode)
    file_mode = stat.S_IMODE(os.stat(keys_path).st_mode)
    assert dir_mode == 0o700, f"secrets/ mode {oct(dir_mode)} != 0700"
    assert file_mode == 0o600, f"api-keys.json mode {oct(file_mode)} != 0600"


def test_api_keys_payload_shape(tmp_path, monkeypatch):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    payload = json.loads(pathlib.Path(root, "secrets", "api-keys.json").read_text())
    # Exactly one key, of the expected sk-atlas-* prefix, valued correctly.
    assert len(payload) == 1
    (key, value), = payload.items()
    assert key.startswith("sk-atlas-")
    assert len(key) > len("sk-atlas-") + 20  # token_urlsafe(32) is well over 20 chars
    assert value == {"user": "local", "created_by": "atlas init"}


# ---------------------------------------------------------------------------
# Already-configured guard + --reconfigure
# ---------------------------------------------------------------------------

def test_already_configured_refuses_without_reconfigure(tmp_path, monkeypatch, capsys):
    root = _make_atlas_root(tmp_path)
    # Pre-existing .env from an earlier setup.
    existing_env = os.path.join(root, ".env")
    pathlib.Path(existing_env).write_text("ATLAS_MODEL_FILE=hand-edited.gguf\n")

    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "Already configured" in out
    assert "--reconfigure" in out
    # And critically — original .env was NOT modified.
    # Path.read_text() closes the handle; bare open().read() in an
    # assert leaks the descriptor and can leak file state under -O.
    assert pathlib.Path(existing_env).read_text() == \
        "ATLAS_MODEL_FILE=hand-edited.gguf\n"


def test_reconfigure_backs_up_existing_env(tmp_path, monkeypatch, capsys):
    root = _make_atlas_root(tmp_path)
    existing_env = os.path.join(root, ".env")
    pathlib.Path(existing_env).write_text("ATLAS_MODEL_FILE=old.gguf\n")

    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color", "--reconfigure"])
    assert rc == 0
    backup = existing_env + ".bak"
    assert os.path.isfile(backup)
    assert pathlib.Path(backup).read_text() == "ATLAS_MODEL_FILE=old.gguf\n"
    # New .env is the wizard's render — has the structured comment header.
    new_body = pathlib.Path(existing_env).read_text()
    assert "generated by `atlas init`" in new_body


def test_reconfigure_without_existing_env_still_works(tmp_path, monkeypatch):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color", "--reconfigure"])
    assert rc == 0
    assert os.path.isfile(os.path.join(root, ".env"))
    # No backup file when there was nothing to back up.
    assert not os.path.isfile(os.path.join(root, ".env.bak"))


def test_reconfigure_backs_up_existing_api_keys(tmp_path, monkeypatch):
    root = _make_atlas_root(tmp_path)
    secrets_dir = os.path.join(root, "secrets")
    os.makedirs(secrets_dir, mode=0o700)
    keys_path = os.path.join(secrets_dir, "api-keys.json")
    pathlib.Path(keys_path).write_text('{"sk-old-key": {"user": "alice"}}\n')
    os.chmod(keys_path, 0o600)

    # Need .env present too so the reconfigure flag is the actual gate.
    pathlib.Path(root, ".env").write_text("ATLAS_MODEL_FILE=foo.gguf\n")

    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color", "--reconfigure"])
    assert rc == 0
    bak = keys_path + ".bak"
    assert os.path.isfile(bak)
    assert "sk-old-key" in pathlib.Path(bak).read_text()
    # New file has a fresh sk-atlas-* key, not the old one.
    new = json.loads(pathlib.Path(keys_path).read_text())
    assert all(k.startswith("sk-atlas-") for k in new.keys())


# ---------------------------------------------------------------------------
# --skip-download + --dry-run
# ---------------------------------------------------------------------------

def test_skip_download_does_not_touch_models_dir(tmp_path, monkeypatch, capsys):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "Skipping download" in out
    # Models dir not auto-created when skipped — the user is responsible
    # for placing the gguf themselves.
    assert not os.path.exists(os.path.join(root, "models"))


def test_dry_run_touches_no_files(tmp_path, monkeypatch, capsys):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--dry-run", "--no-color"])
    assert rc == 0
    out = capsys.readouterr().out
    assert "(dry-run)" in out
    # Nothing on disk except the seed compose file.
    assert os.listdir(root) == ["docker-compose.yml"]


# ---------------------------------------------------------------------------
# --json shape
# ---------------------------------------------------------------------------

def test_json_output_shape_is_stable(tmp_path, monkeypatch, capsys):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color", "--json"])
    assert rc == 0
    out = capsys.readouterr().out
    # JSON object is at the very end of stdout — find its opening brace.
    blob = out[out.rindex("{"):]
    payload = json.loads(blob)
    expected_keys = {"atlas_root", "tier", "model", "models_dir",
                     "env_path", "env_backup", "api_keys_path",
                     "api_keys_backup", "api_key", "image_tag",
                     "ghcr_owner", "dry_run", "backend", "gpu"}
    assert expected_keys.issubset(payload.keys())
    assert payload["atlas_root"] == root
    assert payload["dry_run"] is False
    # api_key in JSON matches the file we wrote.
    file_payload = json.loads(pathlib.Path(payload["api_keys_path"]).read_text())
    assert payload["api_key"] in file_payload


def test_amd_probe_renders_rocm_backend(tmp_path, monkeypatch, capsys):
    """When the probe reports an AMD GPU, .env carries ATLAS_BACKEND=rocm
    and the ROCm compose-override hint appears."""
    amd_gpu = tier.GPUInfo(vendor="amd", name="AMD Radeon RX 7900 XTX",
                            vram_gb=24.0, compute_target="gfx1100", index=0)
    amd_probe = tier.Probe(
        has_gpu=True, gpu_name=amd_gpu.name, gpu_vendor="amd",
        vram_gb=24.0, gpu_count=1, gpus=[amd_gpu],
        system_ram_gb=32.0, cpu_cores=8, disk_free_gb=200.0, platform="linux")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: amd_probe)

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_BACKEND=rocm" in body
    assert "ATLAS_GPU_VENDOR=amd" in body
    assert "ATLAS_GPU_INDEX=0" in body
    # The compose-override hint comment should be present so users know
    # the second compose file is required for ROCm.
    assert "docker-compose.rocm.yml" in body


def _apple_probe(vram_gb: float = 32.0) -> tier.Probe:
    """Helper: build a darwin + Apple Silicon Probe for the macOS tests."""
    apple_gpu = tier.GPUInfo(vendor="apple", name=f"Apple M3 Max {int(vram_gb)}GB",
                              vram_gb=vram_gb, compute_target=None, index=0)
    return tier.Probe(
        has_gpu=True, gpu_name=apple_gpu.name, gpu_vendor="apple",
        vram_gb=vram_gb, gpu_count=1, gpus=[apple_gpu],
        system_ram_gb=vram_gb, cpu_cores=12, disk_free_gb=500.0,
        platform="darwin", system_arch="aarch64")


def test_apple_silicon_probe_recommends_metal_hybrid(tmp_path, monkeypatch, capsys):
    """#32: Apple Silicon hosts get the hybrid Metal path recommended
    (native llama-server + Docker for the rest). The wizard surfaces
    the setup-script prereq + the docs reference, and --yes accepts
    the recommendation."""
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: _apple_probe(32.0))

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    out = capsys.readouterr().out
    # The hybrid Metal recommendation must appear with its prereq pointer.
    assert "Apple Silicon detected" in out
    assert "hybrid Metal" in out or "Metal" in out
    assert "atlas-setup-macos.sh" in out
    assert "SETUP_MACOS.md" in out
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    # The .env should carry ATLAS_BACKEND=metal (not vulkan, not refuse).
    assert "ATLAS_BACKEND=metal" in body
    # And the metal-specific bring-up hint should be in the .env header
    # so users know about the two-step start (native launcher + docker).
    assert "atlas-llama-macos.sh" in body
    assert "docker-compose.macos.yml" in body


def test_apple_silicon_refuses_when_metal_hybrid_declined(
    tmp_path, monkeypatch, capsys
):
    """When the user says no to the hybrid Metal prompt, the wizard
    refuses (writes nothing). The user can re-run with --backend vulkan
    for the Docker-only path, or run setup script first then retry."""
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: _apple_probe(18.0))
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    monkeypatch.setattr("builtins.input", lambda *a, **kw: "n")

    root = _make_atlas_root(tmp_path)
    # No --yes so the prompt actually fires and gets the "n".
    rc = _run(monkeypatch, root,
              ["--skip-download", "--no-color"])
    out = capsys.readouterr().out
    assert rc == 1
    # The decline path should mention both fallback options (vulkan +
    # re-run setup) so the user knows how to recover.
    assert "vulkan" in out or "Vulkan" in out
    # .env must NOT be written when declined.
    assert not os.path.isfile(os.path.join(root, ".env"))


def test_apple_silicon_backend_vulkan_override_skips_metal_prompt(
    tmp_path, monkeypatch, capsys
):
    """Mac users who explicitly want the slow Docker-only path (e.g.
    CI runs, or they don't want to install brew) can pass --backend
    vulkan. The metal-hybrid prompt should NOT fire and .env should
    land on vulkan."""
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: _apple_probe(18.0))
    monkeypatch.setattr(tier, "vulkan_available", lambda: True)

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color",
               "--backend", "vulkan"])
    out = capsys.readouterr().out
    # The hybrid Metal banner must NOT appear when --backend is forced.
    assert "Apple Silicon detected" not in out
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_BACKEND=vulkan" in body
    assert "ATLAS_BACKEND=metal" not in body


def test_apple_silicon_backend_metal_override_explicit(tmp_path, monkeypatch):
    """Mac users who want the metal hybrid path without going through
    the prompt can pass --backend metal directly. Should write the same
    .env as the auto-detected path."""
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: _apple_probe(32.0))

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color",
               "--backend", "metal"])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_BACKEND=metal" in body
    assert "atlas-llama-macos.sh" in body
    assert "docker-compose.macos.yml" in body


# ---------------------------------------------------------------------------
# --image-tag / --ghcr-owner overrides
# ---------------------------------------------------------------------------

def test_image_tag_override_lands_in_env(tmp_path, monkeypatch):
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color",
               "--image-tag", "v1.2.3", "--ghcr-owner", "myfork"])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_IMAGE_TAG=v1.2.3" in body
    assert "ATLAS_GHCR_OWNER=myfork" in body


# ---------------------------------------------------------------------------
# --models-dir override
# ---------------------------------------------------------------------------

def test_models_dir_override_lands_in_env_as_absolute(tmp_path, monkeypatch):
    root = _make_atlas_root(tmp_path)
    custom = tmp_path / "custom_models"
    custom.mkdir()
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color",
               "--models-dir", str(custom)])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    # Non-default path is written verbatim (absolute), not as ./models.
    assert f"ATLAS_MODELS_DIR={custom}" in body
    assert "ATLAS_MODELS_DIR=./models" not in body


# ---------------------------------------------------------------------------
# PC-054 audit fixes
# ---------------------------------------------------------------------------

def test_refuses_when_no_atlas_checkout_in_cwd(tmp_path, monkeypatch, capsys):
    """Running outside an ATLAS checkout (no docker-compose.yml in CWD or
    any parent) refuses up-front rather than silently writing .env into
    a random directory."""
    # tmp_path has no docker-compose.yml — and pytest's tmp_path is under
    # /tmp/pytest-of-*/, none of whose ancestors have one either.
    monkeypatch.chdir(tmp_path)
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "no docker-compose.yml found" in out
    # And critically — nothing got written.
    assert os.listdir(tmp_path) == []


def test_refuses_on_cpu_tier(tmp_path, monkeypatch, capsys):
    """When tier.classify returns 'cpu' (no GPU), refuse rather than
    silently recommend a 16GB-VRAM model the user can't run."""
    root = _make_atlas_root(tmp_path)

    # Force a cpu probe regardless of the actual host.
    from atlas.commands import tier
    cpu_probe = tier.Probe(
        has_gpu=False, gpu_name=None, vram_gb=0.0, gpu_count=0,
        system_ram_gb=8.0, cpu_cores=4, disk_free_gb=100.0, platform="linux")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: cpu_probe)

    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "No GPU detected" in out
    assert "requires a CUDA, ROCm, or Metal-capable GPU" in out
    # Nothing got written — the refusal happens before step 4.
    assert not os.path.isfile(os.path.join(root, ".env"))
    assert not os.path.isdir(os.path.join(root, "secrets"))


def test_yes_skips_prompts(tmp_path, monkeypatch, capsys):
    """--yes makes the wizard non-interactive — input() is never called.
    Patch input() to raise so the test fails loudly if the wizard
    accidentally tries to prompt."""
    root = _make_atlas_root(tmp_path)

    def _no_prompt(*args, **kwargs):
        raise AssertionError("wizard called input() despite --yes")
    monkeypatch.setattr("builtins.input", _no_prompt)

    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0


def test_interactive_decline_aborts_wizard(tmp_path, monkeypatch, capsys):
    """When the user answers 'n' at the model-confirmation prompt,
    the wizard exits 1 cleanly and writes nothing."""
    root = _make_atlas_root(tmp_path)

    # Force interactive mode: claim stdin is a TTY, no --yes.
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    # First prompt: decline.
    monkeypatch.setattr("builtins.input", lambda *a, **kw: "n")

    rc = _run(monkeypatch, root,
              ["--skip-download", "--no-color"])
    assert rc == 1
    # No .env, no secrets/.
    assert not os.path.isfile(os.path.join(root, ".env"))
    assert not os.path.isdir(os.path.join(root, "secrets"))


def test_interactive_default_yes_on_empty_input(tmp_path, monkeypatch):
    """Pressing Enter at a [Y/n] prompt accepts the default (yes)."""
    root = _make_atlas_root(tmp_path)
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    # Empty string = pressed Enter without typing anything.
    monkeypatch.setattr("builtins.input", lambda *a, **kw: "")

    rc = _run(monkeypatch, root,
              ["--skip-download", "--no-color"])
    assert rc == 0
    assert os.path.isfile(os.path.join(root, ".env"))


def test_non_tty_stdin_is_treated_as_yes(tmp_path, monkeypatch):
    """When stdin isn't a TTY (piped input, CI), the wizard never
    prompts — it uses defaults the same way --yes does. Otherwise CI
    runs would hang waiting on input that will never come."""
    root = _make_atlas_root(tmp_path)
    monkeypatch.setattr("sys.stdin.isatty", lambda: False)
    # input() being callable would still be a bug — verify it isn't called.
    def _no_prompt(*args, **kwargs):
        raise AssertionError("wizard called input() with non-TTY stdin")
    monkeypatch.setattr("builtins.input", _no_prompt)

    rc = _run(monkeypatch, root,
              ["--skip-download", "--no-color"])
    assert rc == 0
    assert os.path.isfile(os.path.join(root, ".env"))


# ---------------------------------------------------------------------------
# Vulkan universal backend (PC-114, #114)
# ---------------------------------------------------------------------------

def test_backend_vulkan_override_writes_vulkan_into_env(tmp_path, monkeypatch):
    """--backend vulkan forces ATLAS_BACKEND=vulkan in .env regardless of
    detected GPU vendor. Lets a user opt into the universal fallback
    even on a CUDA-capable box (e.g. to smoke-test the Vulkan path)."""
    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color",
               "--backend", "vulkan"])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_BACKEND=vulkan" in body
    # The vulkan-specific compose hint should appear in the header so
    # operators don't accidentally bring the stack up with only the base
    # file (which would route llama-server to the CUDA image).
    assert "docker-compose.vulkan.yml" in body


def test_backend_vulkan_override_skips_vendor_compat_check(tmp_path, monkeypatch):
    """The --backend override short-circuits the vendor-isn't-packaged
    refusal path. Without this, --backend vulkan would still bail on a
    host with an Intel iGPU (since intel maps to the SYCL backend that
    isn't packaged yet)."""
    root = _make_atlas_root(tmp_path)
    # Simulate an Intel iGPU (intel → sycl → not packaged → would normally refuse).
    intel_probe = tier.Probe(
        has_gpu=True, gpu_name="Intel Arc A770", vram_gb=16.0, gpu_count=1,
        system_ram_gb=64.0, cpu_cores=16, disk_free_gb=500.0, platform="linux",
        gpu_vendor="intel")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: intel_probe)

    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color",
               "--backend", "vulkan"])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_BACKEND=vulkan" in body


def test_vulkan_available_returns_true_with_vulkaninfo_on_path(monkeypatch):
    """tier.vulkan_available() should return True when vulkaninfo is
    found on PATH — the cheapest reliable signal that the host can
    run Vulkan."""
    monkeypatch.setattr("shutil.which",
                        lambda name: "/usr/bin/vulkaninfo" if name == "vulkaninfo" else None)
    assert tier.vulkan_available() is True


def test_vulkan_available_returns_true_with_dev_dri(monkeypatch):
    """Even without vulkaninfo on the host, /dev/dri presence means the
    Vulkan-in-container path will work (Mesa ICDs inside the image
    handle the rest)."""
    monkeypatch.setattr("shutil.which", lambda name: None)
    monkeypatch.setattr("os.path.exists",
                        lambda p: True if p == "/dev/dri" else False)
    assert tier.vulkan_available() is True


def test_vulkan_available_returns_true_on_darwin(monkeypatch):
    """macOS hosts get True regardless (MoltenVK + Docker Desktop on Mac
    provides the Vulkan path even with no native vulkaninfo)."""
    monkeypatch.setattr("shutil.which", lambda name: None)
    monkeypatch.setattr("os.path.exists", lambda p: False)
    monkeypatch.setattr(tier, "detect_gpu", lambda: [])
    monkeypatch.setattr("sys.platform", "darwin")
    assert tier.vulkan_available() is True


def test_vulkan_available_false_when_no_signal(monkeypatch):
    """No vulkaninfo, no /dev/dri, not macOS, no detected GPU →
    Vulkan probably won't work even via lavapipe. Return False so the
    wizard refuses cleanly instead of writing a doomed .env."""
    monkeypatch.setattr("shutil.which", lambda name: None)
    monkeypatch.setattr("os.path.exists", lambda p: False)
    monkeypatch.setattr(tier, "detect_gpu", lambda: [])
    monkeypatch.setattr("sys.platform", "linux")
    assert tier.vulkan_available() is False


# ---------------------------------------------------------------------------
# arm64 multi-arch (#115)
# ---------------------------------------------------------------------------

def test_amd_on_aarch64_falls_through_to_vulkan(tmp_path, monkeypatch, capsys):
    """AMD GPU + aarch64 host: ROCm has no arm64 release, so the wizard
    must NOT write ATLAS_BACKEND=rocm. Instead it should fall through to
    the Vulkan fallback (Mesa RADV is multi-arch) — same code path as
    the Apple Silicon offer."""
    amd_gpu = tier.GPUInfo(vendor="amd", name="AMD Radeon RX 7900 XTX",
                            vram_gb=24.0, compute_target="gfx1100", index=0)
    amd_probe = tier.Probe(
        has_gpu=True, gpu_name=amd_gpu.name, gpu_vendor="amd",
        vram_gb=24.0, gpu_count=1, gpus=[amd_gpu],
        system_ram_gb=32.0, cpu_cores=8, disk_free_gb=200.0,
        platform="linux", system_arch="aarch64")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: amd_probe)
    monkeypatch.setattr(tier, "vulkan_available", lambda: True)

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    out = capsys.readouterr().out
    # The aarch64-rocm message must surface so the user understands why
    # they're being steered to vulkan instead of rocm.
    assert "aarch64" in out
    assert "ROCm" in out or "rocm" in out
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    # Should land on vulkan, not rocm.
    assert "ATLAS_BACKEND=vulkan" in body
    assert "ATLAS_BACKEND=rocm" not in body


def test_amd_on_x86_64_still_picks_rocm(tmp_path, monkeypatch, capsys):
    """Regression guard: the aarch64 carve-out must NOT affect the
    normal x86_64 AMD path. RX 7900 XTX on a normal AMD desktop should
    still write ATLAS_BACKEND=rocm."""
    amd_gpu = tier.GPUInfo(vendor="amd", name="AMD Radeon RX 7900 XTX",
                            vram_gb=24.0, compute_target="gfx1100", index=0)
    amd_probe = tier.Probe(
        has_gpu=True, gpu_name=amd_gpu.name, gpu_vendor="amd",
        vram_gb=24.0, gpu_count=1, gpus=[amd_gpu],
        system_ram_gb=32.0, cpu_cores=8, disk_free_gb=200.0,
        platform="linux", system_arch="x86_64")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: amd_probe)

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    body = pathlib.Path(root, ".env").read_text()
    assert "ATLAS_BACKEND=rocm" in body


def test_aarch64_arch_surfaces_in_probe_output(tmp_path, monkeypatch, capsys):
    """The Step 1 probe banner should print the architecture line when
    it's not the default x86_64. This is the breadcrumb that tells arm64
    users to read the SETUP.md#arm64 section."""
    nvidia_gpu = tier.GPUInfo(vendor="nvidia", name="NVIDIA GB10",
                               vram_gb=128.0, compute_target="120", index=0)
    spark_probe = tier.Probe(
        has_gpu=True, gpu_name=nvidia_gpu.name, gpu_vendor="nvidia",
        vram_gb=128.0, gpu_count=1, gpus=[nvidia_gpu],
        system_ram_gb=128.0, cpu_cores=20, disk_free_gb=2000.0,
        platform="linux", system_arch="aarch64")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: spark_probe)

    root = _make_atlas_root(tmp_path)
    rc = _run(monkeypatch, root,
              ["--yes", "--skip-download", "--no-color"])
    out = capsys.readouterr().out
    assert "Architecture: aarch64" in out
    assert "SETUP.md#arm64" in out
    # DGX Spark CUDA path is supported, so this should succeed.
    assert rc == 0


def test_x86_64_does_not_surface_arch_line(tmp_path, monkeypatch, capsys):
    """Negative: on the default x86_64 host, the architecture line must
    NOT appear (it would be noise for the 99% case). This is the
    counterpoint to test_aarch64_arch_surfaces_in_probe_output."""
    nvidia_gpu = tier.GPUInfo(vendor="nvidia", name="NVIDIA RTX 5060 Ti",
                               vram_gb=16.0, compute_target="120", index=0)
    normal_probe = tier.Probe(
        has_gpu=True, gpu_name=nvidia_gpu.name, gpu_vendor="nvidia",
        vram_gb=16.0, gpu_count=1, gpus=[nvidia_gpu],
        system_ram_gb=32.0, cpu_cores=8, disk_free_gb=500.0,
        platform="linux", system_arch="x86_64")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: normal_probe)

    root = _make_atlas_root(tmp_path)
    _run(monkeypatch, root,
         ["--yes", "--skip-download", "--no-color"])
    out = capsys.readouterr().out
    assert "Architecture:" not in out


def test_loose_secrets_perms_without_yes_fails_noninteractive(tmp_path,
                                                                monkeypatch,
                                                                capsys):
    """secrets/ with loose perms + no --yes + non-interactive stdin: the
    wizard must not report success while silently skipping api-keys.json."""
    root = _make_atlas_root(tmp_path)
    secrets_dir = os.path.join(root, "secrets")
    os.makedirs(secrets_dir, mode=0o755)
    os.chmod(secrets_dir, 0o755)  # umask-proof

    rc = _run(monkeypatch, root, ["--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "loose permissions" in out
    assert "Setup incomplete" in out
    assert "Setup complete" not in out
    assert not os.path.exists(os.path.join(secrets_dir, "api-keys.json"))
