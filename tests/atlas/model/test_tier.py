"""Tests for atlas.commands.tier: GPU detection, tier choice and the
per-tier minimums. The GPU vendor tools are faked with monkeypatch, so no
test needs nvidia-smi or rocm-smi.
"""

import dataclasses
import subprocess
import sys
import tempfile
from types import SimpleNamespace

import pytest

from atlas import display
from atlas.commands import tier, model_registry


# A host that meets every medium-tier minimum (16 GB VRAM, 16 GB RAM,
# 4 cores, 25 GB disk) with room to spare.
COMFORTABLE_HOST = tier.Probe(
    has_gpu=True, gpu_name="Test GPU", vram_gb=16.0, gpu_count=1,
    system_ram_gb=32.0, cpu_cores=8, disk_free_gb=100.0, platform="linux")


# --- classify(): the tier a GPU lands in -------------------------------------

@pytest.mark.parametrize("has_gpu, vram_gb, expected", [
    pytest.param(False, 0.0, "cpu", id="no-gpu"),
    # nvidia-smi can list a GPU and still fail the memory query. Small-tier
    # settings must not go to a GPU that does not work (PC-055).
    pytest.param(True, 0.0, "cpu", id="gpu-with-zero-vram"),
    # Each band is half-open [min, max). The exact breakpoints are where an
    # off-by-one would show.
    (True, 8.0, "small"),
    (True, 11.9, "small"),
    (True, 12.0, "medium"),
    (True, 16.0, "medium"),
    (True, 19.9, "medium"),
    (True, 20.0, "large"),
    (True, 24.0, "large"),
    (True, 31.9, "large"),
    (True, 32.0, "xlarge"),
    (True, 48.0, "xlarge"),
    (True, 80.0, "xlarge"),
])
def test_classify_picks_the_tier_for_the_vram(has_gpu, vram_gb, expected):
    probe = tier.Probe(has_gpu=has_gpu, vram_gb=vram_gb)
    assert tier.classify(probe).tier == expected


def test_a_gpu_below_the_small_tier_gets_small_with_a_warning():
    """A 4 GB GPU is below small's 8 GB minimum. It still gets the small
    tier, with a warning in notes, so the user sees what to upgrade to."""
    t = tier.classify(tier.Probe(has_gpu=True, vram_gb=4.0))
    assert t.tier == "small"
    assert "below" in t.notes.lower()
    assert "OOM" in t.notes


# --- GPU detection -----------------------------------------------------------

def test_nvidia_smi_lists_every_gpu_and_the_largest_is_primary(monkeypatch):
    """Hybrid-graphics workstation: an 8 GB iGPU listed first, a 48 GB A6000
    second. Taking the first GPU put this host in the small tier (PC-055)."""
    # Columns: index, name, memory.total (MiB), compute capability
    nvidia_smi_output = "0, Intel iGPU, 8192, 7.5\n1, NVIDIA RTX A6000, 49152, 8.6\n"
    ran = subprocess.CompletedProcess(args=[], returncode=0, stdout=nvidia_smi_output)
    monkeypatch.setattr(tier.shutil, "which", lambda _: "/usr/bin/nvidia-smi")
    monkeypatch.setattr(tier.subprocess, "run", lambda *a, **kw: ran)

    gpus = tier._read_nvidia_smi()
    assert [g.name for g in gpus] == ["Intel iGPU", "NVIDIA RTX A6000"]
    primary = tier.primary_gpu(gpus)
    assert primary.name == "NVIDIA RTX A6000"
    assert primary.vram_gb == pytest.approx(48.0, rel=0.01)
    assert primary.compute_target == "86"


def test_no_vendor_tools_means_no_gpus(monkeypatch):
    monkeypatch.setattr(tier.shutil, "which", lambda _: None)
    monkeypatch.setattr(sys, "platform", "linux")  # skip the Apple probe
    assert tier.detect_gpu() == []


def test_a_vendor_override_picks_that_vendor():
    """With NVIDIA and AMD present, the override picks AMD even though
    NVIDIA has as much VRAM."""
    gpus = [
        tier.GPUInfo(vendor="nvidia", name="RTX 4090", vram_gb=24.0),
        tier.GPUInfo(vendor="amd", name="RX 7900 XTX", vram_gb=24.0),
    ]
    assert tier.primary_gpu(gpus, override_vendor="amd").vendor == "amd"


def test_rocm_smi_json_gives_the_amd_gpu(monkeypatch):
    rocm_smi_json = ('{"card0": {"Card Series": "Radeon RX 7900 XTX", '
                     '"VRAM Total Memory (B)": "25753026560"}}')
    ran = subprocess.CompletedProcess(args=[], returncode=0, stdout=rocm_smi_json)
    monkeypatch.setattr(tier.shutil, "which",
                        lambda x: "/opt/rocm/bin/rocm-smi" if x == "rocm-smi" else None)
    monkeypatch.setattr(tier.subprocess, "run", lambda *a, **kw: ran)

    gpus = tier._read_rocm_smi()
    assert len(gpus) == 1
    assert gpus[0].vendor == "amd"
    assert "7900 XTX" in gpus[0].name
    assert gpus[0].vram_gb == pytest.approx(24.0, rel=0.05)
    assert gpus[0].compute_target == "gfx1100"


# --- evaluate_constraints(): pass, warn or fail ------------------------------

def test_a_comfortable_host_passes_every_medium_check():
    checks = tier.evaluate_constraints(COMFORTABLE_HOST, tier.by_name("medium"))
    assert all(c.status == "pass" for c in checks)
    assert tier.overall_status(checks) == "pass"


@pytest.mark.parametrize("change, tier_name, check_name, expected", [
    # medium needs 16 GB RAM: 14 GB is within 15% of that, 8 GB is not.
    pytest.param({"system_ram_gb": 14.0}, "medium", "system_ram", "warn",
                 id="ram-within-15pct-warns"),
    pytest.param({"system_ram_gb": 8.0}, "medium", "system_ram", "fail",
                 id="ram-past-15pct-fails"),
    # Too few cores makes ATLAS slow, not out of memory: always warn, never
    # fail, even with 1 core where xlarge needs 16 (PC-055.1).
    pytest.param({"cpu_cores": 2}, "medium", "cpu_cores", "warn",
                 id="cpu-shortage-warns"),
    pytest.param({"cpu_cores": 1}, "xlarge", "cpu_cores", "warn",
                 id="large-cpu-shortage-still-warns"),
])
def test_a_shortage_warns_or_fails(change, tier_name, check_name, expected):
    host = dataclasses.replace(COMFORTABLE_HOST, **change)
    checks = tier.evaluate_constraints(host, tier.by_name(tier_name))
    status_by_name = {c.name: c.status for c in checks}
    assert status_by_name[check_name] == expected


def test_overall_status_is_the_worst_check():
    """RAM warns and disk fails: the overall status is fail, not warn."""
    host = dataclasses.replace(COMFORTABLE_HOST, system_ram_gb=14.0, disk_free_gb=5.0)
    checks = tier.evaluate_constraints(host, tier.by_name("medium"))
    assert tier.overall_status(checks) == "fail"


# --- `atlas tier`: the exit code and the printed card ------------------------

@pytest.mark.parametrize("host, expected_rc", [
    # Scripts (bootstrap, CI) gate on the exit code: a warning is no failure.
    pytest.param(dataclasses.replace(COMFORTABLE_HOST, system_ram_gb=14.0), 0,
                 id="borderline-ram-warns-and-exits-0"),
    pytest.param(dataclasses.replace(COMFORTABLE_HOST, disk_free_gb=5.0), 1,
                 id="disk-short-of-the-minimum-exits-1"),
    pytest.param(tier.Probe(has_gpu=False), 1, id="no-gpu-exits-1"),
])
def test_atlas_tier_exit_code(monkeypatch, host, expected_rc):
    monkeypatch.setattr(tier, "probe", lambda install_dir=None: host)
    assert tier.main(["--no-color"]) == expected_rc


def test_tier_card_prints_with_color_and_unicode(monkeypatch, capsys):
    # A terminal that takes color and Unicode.
    monkeypatch.setattr(tier, "UNICODE_OK", True)
    monkeypatch.setattr(display, "UNICODE_OK", True)

    tier._print_tier_card(tier.by_name("medium"), COMFORTABLE_HOST, color=True)

    assert "─" * 60 in capsys.readouterr().out  # the card's bar line


# --- probe() -----------------------------------------------------------------

@pytest.mark.parametrize("install_dir, measured_path", [
    pytest.param(tempfile.gettempdir(), tempfile.gettempdir(),
                 id="existing-install-dir"),
    pytest.param("/nonexistent/path/does/not/exist", "/",
                 id="missing-install-dir-falls-back-to-root"),
])
def test_probe_measures_free_disk_where_atlas_is_installed(
        monkeypatch, install_dir, measured_path):
    """doctor once measured / for an install on another mount, such as
    /opt/atlas on /data (PC-055.1)."""
    measured = []

    def fake_disk_usage(path):
        measured.append(path)
        return SimpleNamespace(free=50 * 1024 ** 3)

    monkeypatch.setattr(tier.shutil, "disk_usage", fake_disk_usage)
    tier.probe(install_dir=install_dir)
    assert measured == [measured_path]


def test_ram_from_sysctl_is_the_byte_count_in_gib(monkeypatch):
    """macOS has no /proc/meminfo: `sysctl -n hw.memsize` prints the RAM
    size in bytes."""
    def no_proc_meminfo(path, *args, **kwargs):
        raise FileNotFoundError(path)
    monkeypatch.setattr(tier, "open", no_proc_meminfo, raising=False)
    sysctl = subprocess.CompletedProcess(args=[], returncode=0, stdout="34359738368\n")
    monkeypatch.setattr(tier.subprocess, "run", lambda *a, **kw: sysctl)

    assert tier._read_system_ram_gb() == 32.0


@pytest.mark.parametrize("machine", ["aarch64", "x86_64"])
def test_probe_reports_the_system_arch(monkeypatch, machine):
    """init and doctor filter backends on Probe.system_arch, so probe must
    take it from the machine string and not fall back to a default."""
    monkeypatch.setattr(tier, "detect_gpu", lambda: [])
    monkeypatch.setattr(tier.platform, "machine", lambda: machine)
    assert tier.probe().system_arch == machine


@pytest.mark.parametrize("machine, expected", [
    ("x86_64", "x86_64"),
    ("amd64", "x86_64"),
    ("x64", "x86_64"),
    ("X86_64", "x86_64"),   # case-insensitive
    ("aarch64", "aarch64"),
    ("arm64", "aarch64"),
    ("ARM64", "aarch64"),
    ("armv7l", "other"),
    ("ppc64le", "other"),
    ("riscv64", "other"),
    ("", "other"),
])
def test_arch_detect_normalizes_the_machine_string(monkeypatch, machine, expected):
    """The platform.machine() forms seen on NVIDIA sbsa, Mac arm64,
    Windows amd64 and the long tail of niche arches."""
    monkeypatch.setattr(tier.platform, "machine", lambda: machine)
    assert tier.arch_detect() == expected


# --- tiers hold hardware settings; models live in model_registry (PC-055.2) --

def test_tier_profile_has_no_model_fields():
    t = tier.by_name("medium")
    for field in ("model_file", "model_display", "model_size_gb"):
        assert not hasattr(t, field), f"TierProfile.{field} belongs in model_registry"


def test_tier_env_vars_hold_runtime_settings_and_no_model():
    env = tier.by_name("medium").env_vars()
    assert "ATLAS_MODEL_FILE" not in env
    assert "ATLAS_MODEL_NAME" not in env
    for key in ("ATLAS_CTX_SIZE", "ATLAS_PARALLEL_SLOTS",
                "ATLAS_KV_TYPE_K", "ATLAS_KV_TYPE_V"):
        assert key in env


def test_each_gpu_tier_has_a_model_that_maps_back_to_it():
    """doctor.check_tier_match needs the forward and reverse lookups to
    agree. The cpu tier has no model: ATLAS needs a GPU (PC-056)."""
    for t in tier.TIERS:
        rec = model_registry.for_tier(t.tier)
        if t.tier == "cpu":
            assert rec is None
        else:
            assert rec is not None, f"no model for tier {t.tier!r}"
            assert rec.tier == t.tier
            assert model_registry.tier_for_model(rec.model_file) == t.tier


def test_unknown_names_have_no_model():
    assert model_registry.for_tier("nonexistent") is None
    assert model_registry.tier_for_model("not-a-real.gguf") is None
