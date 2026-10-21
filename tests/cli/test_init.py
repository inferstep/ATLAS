"""Tests for atlas.commands.init (PC-054), the first-run install wizard.

The wizard composes tier + model_registry + model.install, so these tests
check the wizard's own behaviour: what it writes into .env and
secrets/api-keys.json, when it refuses, when it prompts, and which backend
it picks for each kind of GPU.
"""

import json
import os
import pathlib
import stat

import pytest

from atlas.commands import init, model_registry, tier


# The wizard refuses on a CPU-only host (PC-054), and CI runners have no GPU.
# So every test starts from this GPU host; tests about other hardware patch
# tier.probe again.
@pytest.fixture(autouse=True)
def _mock_gpu_probe(monkeypatch):
    gpu_probe = tier.Probe(
        has_gpu=True, gpu_name="NVIDIA Test GPU", vram_gb=24.0, gpu_count=1,
        system_ram_gb=64.0, cpu_cores=16, disk_free_gb=500.0, platform="linux")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: gpu_probe)


@pytest.fixture
def atlas_root(tmp_path, monkeypatch):
    """A fake ATLAS checkout (a folder with docker-compose.yml), made the
    current directory so the wizard finds it."""
    root = tmp_path / "atlas_root"
    root.mkdir()
    (root / "docker-compose.yml").write_text("# fake compose for tests\n")
    monkeypatch.chdir(root)
    return root


def _single_gpu_probe(vendor, name, vram_gb, platform="linux", system_arch="x86_64"):
    """A host with one GPU, as tier.probe() reports it."""
    gpu = tier.GPUInfo(vendor=vendor, name=name, vram_gb=vram_gb,
                       compute_target=None, index=0)
    return tier.Probe(
        has_gpu=True, gpu_name=name, gpu_vendor=vendor, vram_gb=vram_gb,
        gpu_count=1, gpus=[gpu], system_ram_gb=64.0, cpu_cores=16,
        disk_free_gb=500.0, platform=platform, system_arch=system_arch)


# ---------------------------------------------------------------------------
# What a run writes
# ---------------------------------------------------------------------------

def test_yes_writes_every_env_key_the_stack_reads(atlas_root):
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    body = (atlas_root / ".env").read_text()
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
    chosen = model_registry.by_model_file(model_file)
    assert chosen is not None
    assert f"ATLAS_GRAMMAR_MODE={chosen.grammar_mode}" in body
    # The proxy runs as the invoking user so it can write the /workspace
    # bind mount (the image's baked-in uid 1001 can't).
    assert f"ATLAS_PROXY_UID={os.getuid()}" in body
    assert f"ATLAS_PROXY_GID={os.getgid()}" in body
    # Defaults. The models dir is written as ./models when it is atlas_root/models.
    assert "ATLAS_MODELS_DIR=./models" in body
    assert "ATLAS_IMAGE_TAG=latest" in body
    assert "ATLAS_GHCR_OWNER=inferstep" in body
    assert "ATLAS_MACOS_PREFIX=~/.atlas/macos" in body


def test_api_keys_file_is_0600_in_a_0700_dir(atlas_root):
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    secrets_dir = atlas_root / "secrets"
    assert oct(stat.S_IMODE(secrets_dir.stat().st_mode)) == "0o700"
    assert oct(stat.S_IMODE((secrets_dir / "api-keys.json").stat().st_mode)) == "0o600"


def test_api_keys_file_holds_one_new_sk_atlas_key(atlas_root):
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    payload = json.loads((atlas_root / "secrets" / "api-keys.json").read_text())
    assert len(payload) == 1
    (key, value), = payload.items()
    assert key.startswith("sk-atlas-")
    assert len(key) > len("sk-atlas-") + 20  # token_urlsafe(32) is well over 20 chars
    assert value == {"user": "local", "created_by": "atlas init"}


def test_skip_download_does_not_create_models_dir(atlas_root, capsys):
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    assert "Skipping download" in capsys.readouterr().out
    # The user places the gguf themselves.
    assert not (atlas_root / "models").exists()


def test_dry_run_touches_no_files(atlas_root, capsys):
    rc = init.main(["--yes", "--skip-download", "--dry-run", "--no-color"])
    assert rc == 0
    assert "(dry-run)" in capsys.readouterr().out
    assert os.listdir(atlas_root) == ["docker-compose.yml"]


def test_json_output_shape_is_stable(atlas_root, capsys):
    """The bootstrap script reads this JSON."""
    rc = init.main(["--yes", "--skip-download", "--no-color", "--json"])
    assert rc == 0
    out = capsys.readouterr().out
    # The JSON object is the last thing on stdout.
    payload = json.loads(out[out.rindex("{"):])
    assert {"atlas_root", "tier", "model", "models_dir",
            "env_path", "env_backup", "api_keys_path",
            "api_keys_backup", "api_key", "image_tag",
            "ghcr_owner", "dry_run", "backend", "gpu"}.issubset(payload.keys())
    assert payload["atlas_root"] == str(atlas_root)
    assert payload["dry_run"] is False
    # The api_key in the JSON is the one written to the file.
    file_payload = json.loads(pathlib.Path(payload["api_keys_path"]).read_text())
    assert payload["api_key"] in file_payload


def test_image_tag_and_ghcr_owner_flags_land_in_env(atlas_root):
    rc = init.main(["--yes", "--skip-download", "--no-color",
                    "--image-tag", "v1.2.3", "--ghcr-owner", "myfork"])
    assert rc == 0
    body = (atlas_root / ".env").read_text()
    assert "ATLAS_IMAGE_TAG=v1.2.3" in body
    assert "ATLAS_GHCR_OWNER=myfork" in body


def test_models_dir_flag_lands_in_env_as_absolute_path(atlas_root, tmp_path):
    custom = tmp_path / "custom_models"
    custom.mkdir()
    rc = init.main(["--yes", "--skip-download", "--no-color",
                    "--models-dir", str(custom)])
    assert rc == 0
    body = (atlas_root / ".env").read_text()
    assert f"ATLAS_MODELS_DIR={custom}" in body
    assert "ATLAS_MODELS_DIR=./models" not in body


# ---------------------------------------------------------------------------
# An existing setup, and --reconfigure
# ---------------------------------------------------------------------------

def test_existing_env_is_kept_without_reconfigure(atlas_root, capsys):
    (atlas_root / ".env").write_text("ATLAS_MODEL_FILE=hand-edited.gguf\n")
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "Already configured" in out
    assert "--reconfigure" in out
    assert (atlas_root / ".env").read_text() == "ATLAS_MODEL_FILE=hand-edited.gguf\n"


def test_reconfigure_backs_up_env_and_api_keys(atlas_root):
    env = atlas_root / ".env"
    env.write_text("ATLAS_MODEL_FILE=old.gguf\n")
    keys = atlas_root / "secrets" / "api-keys.json"
    keys.parent.mkdir(mode=0o700)
    keys.write_text('{"sk-old-key": {"user": "alice"}}\n')

    rc = init.main(["--yes", "--skip-download", "--no-color", "--reconfigure"])
    assert rc == 0
    assert (atlas_root / ".env.bak").read_text() == "ATLAS_MODEL_FILE=old.gguf\n"
    assert "sk-old-key" in (atlas_root / "secrets" / "api-keys.json.bak").read_text()
    # The new files are the wizard's own: a generated .env and a fresh key.
    assert "generated by `atlas init`" in env.read_text()
    assert all(k.startswith("sk-atlas-") for k in json.loads(keys.read_text()))


def test_reconfigure_with_nothing_to_back_up_writes_no_backup(atlas_root):
    rc = init.main(["--yes", "--skip-download", "--no-color", "--reconfigure"])
    assert rc == 0
    assert (atlas_root / ".env").is_file()
    assert not (atlas_root / ".env.bak").exists()


# ---------------------------------------------------------------------------
# Refusals
# ---------------------------------------------------------------------------

def test_refuses_outside_an_atlas_checkout(tmp_path, monkeypatch, capsys):
    """No docker-compose.yml in the current directory or any parent: refuse
    up front instead of writing .env into a random folder."""
    # pytest's tmp_path and its parents have no docker-compose.yml.
    monkeypatch.chdir(tmp_path)
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 1
    assert "no docker-compose.yml found" in capsys.readouterr().out
    assert os.listdir(tmp_path) == []


def test_refuses_on_a_cpu_only_host(atlas_root, monkeypatch, capsys):
    """No GPU: refuse rather than recommend a model the host can't run."""
    cpu_probe = tier.Probe(
        has_gpu=False, gpu_name=None, vram_gb=0.0, gpu_count=0,
        system_ram_gb=8.0, cpu_cores=4, disk_free_gb=100.0, platform="linux")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: cpu_probe)
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "No GPU detected" in out
    assert "requires a CUDA, ROCm, or Metal-capable GPU" in out
    # The refusal comes before anything is written.
    assert not (atlas_root / ".env").exists()
    assert not (atlas_root / "secrets").exists()


def test_loose_secrets_dir_without_yes_reports_setup_incomplete(atlas_root, capsys):
    """secrets/ with loose permissions, no --yes and no TTY: the wizard must
    not report success while it skips api-keys.json."""
    secrets_dir = atlas_root / "secrets"
    secrets_dir.mkdir()
    secrets_dir.chmod(0o755)  # whatever the umask is
    rc = init.main(["--skip-download", "--no-color"])
    assert rc == 1
    out = capsys.readouterr().out
    assert "loose permissions" in out
    assert "Setup incomplete" in out
    assert "Setup complete" not in out
    assert not (secrets_dir / "api-keys.json").exists()


# ---------------------------------------------------------------------------
# Prompts
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("flags, stdin_is_tty", [
    (["--yes"], True),
    ([], False),
], ids=["yes-flag-on-a-tty", "stdin-not-a-tty"])
def test_no_prompt_with_yes_or_without_a_tty(atlas_root, monkeypatch, flags, stdin_is_tty):
    """--yes, or a stdin that is not a TTY (CI, piped input), takes the
    defaults without calling input(); a prompt there would hang CI."""
    monkeypatch.setattr("sys.stdin.isatty", lambda: stdin_is_tty)

    def fail_on_prompt(*args, **kwargs):
        raise AssertionError("the wizard called input()")
    monkeypatch.setattr("builtins.input", fail_on_prompt)

    rc = init.main([*flags, "--skip-download", "--no-color"])
    assert rc == 0
    assert (atlas_root / ".env").is_file()


@pytest.mark.parametrize("answer, expected_rc, files_written", [
    ("n", 1, False),
    ("", 0, True),
], ids=["n-declines-and-writes-nothing", "enter-accepts-the-default"])
def test_answer_at_the_model_prompt(atlas_root, monkeypatch, answer, expected_rc, files_written):
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    monkeypatch.setattr("builtins.input", lambda *a, **kw: answer)
    rc = init.main(["--skip-download", "--no-color"])
    assert rc == expected_rc
    assert (atlas_root / ".env").is_file() is files_written
    assert (atlas_root / "secrets").is_dir() is files_written


# ---------------------------------------------------------------------------
# Backend choice per GPU (ROCm, Metal #32, Vulkan PC-114, arm64 #115)
# ---------------------------------------------------------------------------

@pytest.mark.parametrize("system_arch, expected_backend, rocm_note_shown", [
    ("x86_64", "rocm", False),
    ("aarch64", "vulkan", True),
], ids=["x86_64-gets-rocm", "aarch64-falls-back-to-vulkan"])
def test_amd_gpu_backend_depends_on_the_cpu_architecture(
        atlas_root, monkeypatch, capsys, system_arch, expected_backend, rocm_note_shown):
    """ROCm has no arm64 release, so an AMD GPU on aarch64 gets Vulkan
    (Mesa RADV is multi-arch) and a note that says why."""
    amd_probe = _single_gpu_probe("amd", "AMD Radeon RX 7900 XTX", 24.0,
                                  system_arch=system_arch)
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: amd_probe)
    monkeypatch.setattr(tier, "vulkan_available", lambda: True)
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 0
    assert ("ROCm has no aarch64 release" in capsys.readouterr().out) is rocm_note_shown
    body = (atlas_root / ".env").read_text()
    assert f"ATLAS_BACKEND={expected_backend}" in body
    assert "ATLAS_GPU_VENDOR=amd" in body
    assert "ATLAS_GPU_INDEX=0" in body
    # The .env header names the compose override file the backend needs.
    assert f"docker-compose.{expected_backend}.yml" in body


@pytest.mark.parametrize("flags, banner_shown", [
    ([], True),
    (["--backend", "metal"], False),
], ids=["auto-detected", "forced-with-backend-metal"])
def test_apple_silicon_gets_the_hybrid_metal_env(
        atlas_root, monkeypatch, capsys, flags, banner_shown):
    """Apple Silicon gets native llama-server (Metal) + Docker for the rest.
    Auto-detection shows a recommendation banner that --yes accepts;
    --backend metal skips the banner and writes the same .env."""
    apple = _single_gpu_probe("apple", "Apple M3 Max 32GB", 32.0,
                              platform="darwin", system_arch="aarch64")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: apple)
    rc = init.main(["--yes", "--skip-download", "--no-color", *flags])
    assert rc == 0
    out = capsys.readouterr().out
    assert ("Apple Silicon detected" in out) is banner_shown
    # The setup-script prerequisite and its docs are pointed to.
    assert "atlas-setup-macos.sh" in out
    assert "SETUP_MACOS.md" in out
    body = (atlas_root / ".env").read_text()
    assert "ATLAS_BACKEND=metal" in body
    # The .env header explains the two-step start: native launcher, then Docker.
    assert "atlas-llama-macos.sh" in body
    assert "docker-compose.macos.yml" in body


def test_apple_silicon_declining_the_metal_setup_writes_nothing(
        atlas_root, monkeypatch, capsys):
    apple = _single_gpu_probe("apple", "Apple M3 Max 18GB", 18.0,
                              platform="darwin", system_arch="aarch64")
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: apple)
    monkeypatch.setattr("sys.stdin.isatty", lambda: True)
    monkeypatch.setattr("builtins.input", lambda *a, **kw: "n")
    rc = init.main(["--skip-download", "--no-color"])  # no --yes, so it asks
    assert rc == 1
    # The refusal names the Docker-only way out.
    assert "Re-run with --backend vulkan" in capsys.readouterr().out
    assert not (atlas_root / ".env").exists()


@pytest.mark.parametrize("probe", [
    _single_gpu_probe("nvidia", "NVIDIA RTX 4090", 24.0),
    _single_gpu_probe("intel", "Intel Arc A770", 16.0),
    _single_gpu_probe("apple", "Apple M3 Max 18GB", 18.0,
                      platform="darwin", system_arch="aarch64"),
], ids=["nvidia-has-cuda", "intel-sycl-not-packaged", "apple-skips-metal-prompt"])
def test_backend_vulkan_flag_wins_over_the_gpu_vendor(atlas_root, monkeypatch, capsys, probe):
    """--backend vulkan writes the Vulkan .env on any GPU. It skips the
    vendor's own backend, the not-yet-packaged refusal (Intel's SYCL) and
    the Apple Metal prompt."""
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: probe)
    rc = init.main(["--yes", "--skip-download", "--no-color", "--backend", "vulkan"])
    assert rc == 0
    assert "Apple Silicon detected" not in capsys.readouterr().out
    body = (atlas_root / ".env").read_text()
    assert "ATLAS_BACKEND=vulkan" in body
    # Without the override file in the header, an operator could start the
    # stack with the base file alone, which runs the CUDA image.
    assert "docker-compose.vulkan.yml" in body


@pytest.mark.parametrize("vulkaninfo_on_path, has_dev_dri, platform, expected", [
    (True, False, "linux", True),
    (False, True, "linux", True),
    (False, False, "darwin", True),
    (False, False, "linux", False),
], ids=["vulkaninfo-on-path", "dev-dri-exists", "macos", "no-signal"])
def test_vulkan_available(monkeypatch, vulkaninfo_on_path, has_dev_dri, platform, expected):
    """True on any sign that Vulkan can run: vulkaninfo on PATH, /dev/dri
    (the Mesa drivers in the image do the rest) or macOS (MoltenVK). With
    no sign and no detected GPU it is False, so the wizard refuses instead
    of writing a .env that can't boot."""
    monkeypatch.setattr("shutil.which",
                        lambda name: "/usr/bin/vulkaninfo" if vulkaninfo_on_path else None)
    monkeypatch.setattr("os.path.exists", lambda path: has_dev_dri and path == "/dev/dri")
    monkeypatch.setattr("sys.platform", platform)
    monkeypatch.setattr(tier, "detect_gpu", lambda: [])
    assert tier.vulkan_available() is expected


@pytest.mark.parametrize("probe, arch_line_shown", [
    (_single_gpu_probe("nvidia", "NVIDIA GB10", 128.0, system_arch="aarch64"), True),
    (_single_gpu_probe("nvidia", "NVIDIA RTX 5060 Ti", 16.0), False),
], ids=["dgx-spark-aarch64", "rtx-on-x86_64"])
def test_probe_names_the_architecture_only_off_x86_64(
        atlas_root, monkeypatch, capsys, probe, arch_line_shown):
    """arm64 users get a line that points to SETUP.md#arm64; on the common
    x86_64 host that line would be noise."""
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: probe)
    rc = init.main(["--yes", "--skip-download", "--no-color"])
    assert rc == 0  # CUDA on DGX Spark (aarch64) is supported
    out = capsys.readouterr().out
    assert ("Architecture:" in out) is arch_line_shown
    assert ("Architecture: aarch64 (see docs/SETUP.md#arm64" in out) is arch_line_shown
