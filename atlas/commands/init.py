"""`atlas init` — first-run install wizard (PC-054).

Composes existing primitives (no new install/probe/registry logic):

  * tier.classify(probe())                  → TierProfile
  * model_registry.for_tier()               → suggested Model
  * model_registry.supported_models()[0]    → fallback when tier default is no-artifacts
  * model.main(["install", ...])            → download + SHA verify (inherits PC-056.1/.2 gates)

Then writes:
  * <atlas_root>/.env                       — Compose configuration
  * <atlas_root>/secrets/api-keys.json      — bearer-token auth (mode 0600, parent 0700)

Flags:
  --yes              non-interactive, accept all defaults
  --skip-download    write .env + api-keys but skip model.install
  --reconfigure      back up existing .env → .env.bak before writing
  --dry-run          print proposed writes; touch nothing
  --json             machine-readable summary
  --models-dir PATH  override default <atlas_root>/models
  --image-tag TAG    non-interactive image-source choice (default: latest)
  --no-color
"""

from __future__ import annotations

import argparse
import contextlib
import json as jsonlib
import os
import secrets as secrets_mod
import shutil
import sys
from typing import List, Optional, Tuple

from atlas.commands import model, model_registry, tier


# ---------------------------------------------------------------------------
# Output helpers (mirror tier.py / model.py — same conventions)
# ---------------------------------------------------------------------------

# Shared ANSI colors + unicode-safe output primitives.
from atlas.display import (
    RESET, BOLD, RED, GREEN, YELLOW,
    safe_print as _safe_print,
)


# ---------------------------------------------------------------------------
# Prompt helpers (PC-054 audit fix — wizard is now actually interactive
# unless --yes is passed or stdin isn't a TTY).
# ---------------------------------------------------------------------------

def _is_interactive(args: argparse.Namespace) -> bool:
    """True when the wizard should prompt the user for confirmations.
    --yes or a non-TTY stdin both force non-interactive mode."""
    if args.yes:
        return False
    try:
        return sys.stdin.isatty()
    except (AttributeError, ValueError):
        return False


def _confirm(prompt: str, default_yes: bool, args: argparse.Namespace) -> bool:
    """Yes/no prompt. Non-interactive (--yes or non-TTY) returns the default."""
    if not _is_interactive(args):
        return default_yes
    suffix = "[Y/n]" if default_yes else "[y/N]"
    while True:
        try:
            ans = input(f"  {prompt} {suffix} ").strip().lower()
        except EOFError:
            return default_yes
        if ans == "":
            return default_yes
        if ans in ("y", "yes"):
            return True
        if ans in ("n", "no"):
            return False
        _safe_print("  Please answer 'y' or 'n'.")


def _choose(prompt: str, choices: List[str], default: str,
             args: argparse.Namespace) -> str:
    """Multiple-choice prompt. Non-interactive returns the default."""
    if not _is_interactive(args):
        return default
    suffix = "/".join(c if c != default else c.upper() for c in choices)
    while True:
        try:
            ans = input(f"  {prompt} [{suffix}] ").strip().lower()
        except EOFError:
            return default
        if ans == "":
            return default
        for c in choices:
            if ans == c.lower() or ans == c.lower()[0]:
                return c
        _safe_print(f"  Please pick one of: {', '.join(choices)}.")


# ---------------------------------------------------------------------------
# Backend mapping — vendor → llama.cpp backend → docker image tag suffix
# ---------------------------------------------------------------------------

# Single source of truth for vendor → backend mapping. Used by the wizard
# (label rendering) and by .env writing (ATLAS_BACKEND). Keep aligned
# with the Dockerfile suffixes in /inference/.
_BACKEND_BY_VENDOR = {
    "nvidia": ("cuda",  "CUDA",          True),   # supported in V3.1.0
    "amd":    ("rocm",  "ROCm",          True),   # supported in V3.1.1
    "apple":  ("metal", "Metal",         True),   # supported (hybrid: native llama-server + docker for the rest, #32)
    "intel":  ("sycl",  "SYCL",          False),  # roadmap
}


def _backend_for(vendor: Optional[str]) -> Tuple[str, str, bool]:
    """Return (backend_id, display_name, supported) for a vendor.
    Unknown vendor falls through to ('unknown', 'unknown', False)."""
    if vendor and vendor in _BACKEND_BY_VENDOR:
        return _BACKEND_BY_VENDOR[vendor]
    return ("unknown", "unknown", False)


def _gpu_label(pos: int, g: "tier.GPUInfo") -> str:
    """One-line human label: '[0] NVIDIA RTX 4090 (24.0 GB) [nvidia/CUDA]'.
    pos is the wizard-local position (1..N) for selection; g.index is the
    vendor-local index passed to {CUDA,HIP}_VISIBLE_DEVICES."""
    _, backend_name, supported = _backend_for(g.vendor)
    tag = f"{g.vendor}/{backend_name}" if supported else \
          f"{g.vendor}/{backend_name} — NOT YET SUPPORTED"
    return f"[{pos}] {g.name} ({g.vram_gb:.1f} GB VRAM) "\
           f"[{tag} | vendor-idx={g.index}]"


def _pick_gpu(probe: "tier.Probe", args: argparse.Namespace,
              color: bool) -> Optional["tier.GPUInfo"]:
    """Pick which GPU ATLAS will use. Honors ATLAS_GPU_VENDOR /
    ATLAS_GPU_INDEX env if set; otherwise auto-picks largest VRAM.
    When multiple GPUs are detected AND interactive, prompts the user
    with the auto-pick as the default.

    Wizard prompt uses *list position* (1..N) rather than vendor-local
    index, because a multi-vendor host (e.g. NVIDIA + AMD) would have
    colliding indices otherwise — each vendor enumerates from 0.
    """
    if not probe.gpus:
        return None
    override_vendor = os.environ.get("ATLAS_GPU_VENDOR") or None
    override_index_str = os.environ.get("ATLAS_GPU_INDEX")
    try:
        override_index = int(override_index_str) if override_index_str else None
    except ValueError:
        override_index = None

    auto = tier.primary_gpu(probe.gpus, override_vendor=override_vendor,
                            override_index=override_index)
    if len(probe.gpus) == 1 or not _is_interactive(args):
        return auto

    _safe_print(f"  Multiple GPUs detected ({len(probe.gpus)}):")
    auto_pos = None
    for pos, g in enumerate(probe.gpus, start=1):
        marker = "*" if auto is not None and g is auto else " "
        _safe_print(f"    {marker} {_gpu_label(pos, g)}")
        if auto is not None and g is auto:
            auto_pos = pos
    _safe_print(f"  Default: position {auto_pos} ({auto.name if auto else 'none'}) "
                f"— largest VRAM")

    choices = [str(i + 1) for i in range(len(probe.gpus))]
    default_choice = str(auto_pos) if auto_pos else choices[0]
    pick_str = _choose("Pick a GPU position", choices, default_choice, args)
    try:
        pos = int(pick_str)
    except ValueError:
        return auto
    if 1 <= pos <= len(probe.gpus):
        return probe.gpus[pos - 1]
    return auto


# ---------------------------------------------------------------------------
# Path resolution
# ---------------------------------------------------------------------------

def _find_atlas_root() -> Optional[str]:
    """Walk up from CWD looking for docker-compose.yml.

    Returns None if no compose file is found in any ancestor — the wizard
    refuses to write .env / secrets/ into a non-checkout directory rather
    than silently dumping config wherever the user happens to be. That
    refusal is why this is not `env.atlas_root()`: that resolver searches
    from its own file first and falls back to the cwd, so it always
    answers with *some* directory.
    """
    cur = os.path.abspath(os.getcwd())
    while True:
        if os.path.isfile(os.path.join(cur, "docker-compose.yml")):
            return cur
        parent = os.path.dirname(cur)
        if parent == cur:
            return None
        cur = parent


def _resolve_models_dir(arg_models_dir: Optional[str], atlas_root: str) -> str:
    if arg_models_dir:
        return os.path.abspath(arg_models_dir)
    env = os.environ.get("ATLAS_MODELS_DIR")
    if env:
        return os.path.abspath(env)
    return os.path.join(atlas_root, "models")


# ---------------------------------------------------------------------------
# Step 1 — hardware probe
# ---------------------------------------------------------------------------

def _step_probe(args: argparse.Namespace, color: bool
                 ) -> Tuple[tier.Probe, tier.TierProfile, Optional[tier.GPUInfo]]:
    probe = tier.probe()
    profile = tier.classify(probe)
    _safe_print(f"  Detected tier: {BOLD if color else ''}{profile.tier}{RESET if color else ''} "
                f"({profile.label})")
    if probe.has_gpu:
        _safe_print(f"    Primary GPU: [{probe.gpu_vendor}] "
                    f"{probe.gpu_name or 'unknown'} "
                    f"({probe.vram_gb:.1f} GB VRAM)")
        if probe.gpu_count > 1:
            _safe_print(f"    ({probe.gpu_count} GPUs total — wizard will "
                        f"prompt for selection)")
    else:
        _safe_print("    GPU: none detected")
    _safe_print(f"    System: {probe.system_ram_gb:.1f} GB RAM, "
                f"{probe.cpu_cores} cores, "
                f"{probe.disk_free_gb:.1f} GB free")
    # #115: surface arch when it's not the default. arm64 hosts (DGX Spark,
    # Snapdragon X Elite, Apple Silicon, Jetson, Pi 5) have a different
    # backend matrix — no rocm, vulkan as universal fallback, cuda needs
    # the sbsa/l4t base image swap.
    if probe.system_arch != "x86_64":
        _safe_print(f"    Architecture: {probe.system_arch} (see "
                    f"docs/SETUP.md#arm64 for backend availability)")
    selected = _pick_gpu(probe, args, color)
    if selected is not None and probe.gpu_count > 1:
        _safe_print(f"    Selected: {selected.name} "
                    f"(vendor-idx={selected.index})")
    return probe, profile, selected


# ---------------------------------------------------------------------------
# Step 2 — model selection
# ---------------------------------------------------------------------------

def _step_select_model(profile: tier.TierProfile,
                        selected_gpu: Optional[tier.GPUInfo],
                        probe: tier.Probe,
                        args: argparse.Namespace,
                        color: bool) -> Optional[model_registry.Model]:
    """Pick a model for the user. Tier default if `supported`, otherwise
    surface the supported-fallback so wizard never recommends a model
    that the lens cannot score (ATLAS stops agent work on such a model).

    PC-054 audit fix: refuse on cpu tier — the user has no GPU and
    `docker compose up -d` would fail at llama-server load. Better to
    refuse here than write a broken .env.

    V3.1.1 addition: refuse on unsupported backends (SYCL, and metal on
    non-macOS hosts) — the Dockerfile for that backend hasn't shipped, so
    `docker compose up` would fail at image pull. Better to refuse with a
    clear message. Apple Silicon metal is handled by the hybrid branch below.

    #115 addition: arch-aware backend filtering. AMD ROCm has no aarch64
    release, so on arm64 hosts with an AMD GPU we treat rocm as
    not-supported and offer the Vulkan fallback (Mesa RADV is multi-arch)."""
    if profile.tier == "cpu":
        _safe_print(f"  {RED if color else ''}No GPU detected. "
                    f"ATLAS requires a CUDA, ROCm, or Metal-capable GPU for "
                    f"llama.cpp inference.{RESET if color else ''}")
        _safe_print("  Supported: NVIDIA (CUDA), AMD (ROCm — V3.1.1), Apple "
                    "Silicon (Metal — macOS hybrid, #32). See SETUP.md. The "
                    "wizard refuses here rather than write a .env that won't boot.")
        return None

    if selected_gpu is not None:
        backend_id, backend_name, supported = _backend_for(selected_gpu.vendor)
        # #115: rocm has no aarch64 release. Strip support on non-x86_64
        # so the wizard falls through to the Vulkan fallback rather than
        # writing ATLAS_BACKEND=rocm + failing at image pull. Mesa RADV
        # under vulkan covers AMD GPUs on arm64 (slower than rocm would
        # be, but it actually exists).
        if backend_id == "rocm" and probe.system_arch != "x86_64":
            supported = False
            _safe_print(f"  {YELLOW if color else ''}Note: ROCm has no "
                        f"{probe.system_arch} release.{RESET if color else ''} "
                        f"Vulkan-via-Mesa-RADV is the path for AMD GPUs on "
                        f"arm64 hosts (see #115).")
        # #32: Apple Silicon hybrid path — native llama-server (Metal) +
        # Docker for the rest of the stack. This is the FAST path; the
        # docker-only Vulkan-via-MoltenVK path is the slow fallback (kept
        # available via --backend vulkan). Surface the setup-script
        # prerequisite so users know they need to run it before bringing
        # the stack up.
        if (backend_id == "metal" and probe.platform == "darwin"
                and not args.backend):
            _safe_print(f"  {GREEN if color else ''}Apple Silicon detected. "
                        f"Recommended setup: native Metal inference + Docker "
                        f"for the supporting services.{RESET if color else ''}")
            _safe_print("")
            _safe_print(f"  {BOLD if color else ''}Before you continue:{RESET if color else ''} "
                        f"run ./scripts/atlas-setup-macos.sh if you haven't. "
                        f"It installs the build tools and compiles llama.cpp "
                        f"with Metal. Full instructions in docs/SETUP_MACOS.md.")
            _safe_print("")
            _safe_print("  Other options:")
            _safe_print("    --backend vulkan   Docker-only (no native build, "
                        "slower)")
            if not _confirm("Continue with the recommended setup?",
                            default_yes=True, args=args):
                _safe_print("  Refusing rather than writing a .env that won't "
                            "boot. Re-run with --backend vulkan for the "
                            "Docker-only path, or run the setup script + retry.")
                return None
        # Operator-forced backend (--backend vulkan) short-circuits the
        # vendor probe entirely. Useful for users who want the universal
        # fallback even when their card has native support, or to test
        # the Vulkan path on a CUDA box.
        if args.backend:
            _safe_print(f"  --backend {args.backend} requested — overriding "
                        f"the vendor-default ({backend_name}).")
        elif not supported:
            # PC-114 (#114): when the native backend isn't packaged, offer
            # Vulkan as the universal fallback before refusing. Vulkan
            # works on Apple Silicon (via MoltenVK), Intel Arc (Mesa
            # ANV), Snapdragon Adreno, and CPU lavapipe — covering the
            # gap left by the cuda/rocm-only matrix.
            _safe_print(f"  {YELLOW if color else ''}Selected GPU vendor "
                        f"'{selected_gpu.vendor}' uses the {backend_name} backend, "
                        f"which is not yet packaged.{RESET if color else ''}")
            roadmap = {
                # 'metal' no longer in this dict — apple+darwin is handled
                # by the hybrid-metal branch above. 'metal' only lands here
                # if the vendor is apple but the host is NOT darwin (very
                # unusual, e.g. someone forced ATLAS_GPU_VENDOR=apple on
                # Linux), in which case Vulkan is the right fallback.
                "sycl":  "Roadmap (Intel Arc / oneAPI SYCL backend).",
                "unknown": "Vendor not recognized — file an issue with "
                           "your GPU details.",
            }
            _safe_print(f"  {roadmap.get(backend_id, '')}")
            if tier.vulkan_available():
                _safe_print("")
                _safe_print(f"  {GREEN if color else ''}Vulkan universal "
                            f"backend is available as a fallback.{RESET if color else ''}")
                _safe_print("  Vulkan covers this GPU (and the CPU lavapipe "
                            "fallback) but runs ~20–40% slower than a tuned "
                            "native backend would.")
                if _confirm("Use Vulkan?", default_yes=True, args=args):
                    args.backend = "vulkan"
                else:
                    _safe_print("  Refusing rather than writing a .env that "
                                "won't boot. Re-run with --backend vulkan to "
                                "skip this prompt next time.")
                    return None
            else:
                _safe_print("  Vulkan fallback isn't available on this host "
                            "either (no vulkaninfo, no /dev/dri, no detected "
                            "GPU). Refusing.")
                return None

    tier_default = model_registry.for_tier(profile.tier)
    supported = model_registry.supported_models()
    fallback = supported[0] if supported else None

    if tier_default and tier_default.lens_status == "supported":
        _safe_print(f"  Recommended: {BOLD if color else ''}{tier_default.name}{RESET if color else ''} "
                    f"({tier_default.model_size_gb:.1f} GB, Lens supported)")
        if not _confirm(f"Use {tier_default.name}?", default_yes=True, args=args):
            _safe_print("  User declined — re-run with `atlas model list` to pick "
                        "a different model, then `atlas init --reconfigure --skip-download` "
                        "to wire the .env without re-downloading.")
            return None
        return tier_default

    # Tier default is missing or no-artifacts — fall back.
    if tier_default and tier_default.lens_status != "supported":
        _safe_print(f"  Tier default ({tier_default.name}) has lens_status="
                    f"{tier_default.lens_status}: the lens could not score it, and "
                    f"ATLAS would stop agent work on it.")
    if fallback is None:
        _safe_print(f"  {RED if color else ''}No Lens-supported model in registry; "
                    f"cannot recommend.{RESET if color else ''}")
        return None
    _safe_print(f"  Falling back to: {BOLD if color else ''}{fallback.name}{RESET if color else ''} "
                f"({fallback.model_size_gb:.1f} GB, Lens supported)")
    if not _confirm(f"Use {fallback.name}?", default_yes=True, args=args):
        _safe_print("  User declined — see `atlas model list` for alternatives.")
        return None
    return fallback


# ---------------------------------------------------------------------------
# Step 3 — download (delegate to atlas model install)
# ---------------------------------------------------------------------------

def _step_download(m: model_registry.Model, models_dir: str,
                    args: argparse.Namespace, color: bool) -> int:
    """Returns 0 on success (or already-installed), non-zero on failure.
    Skips when --skip-download or --dry-run."""
    if args.skip_download:
        _safe_print(f"  Skipping download (--skip-download). "
                    f"Place {m.model_file} in {models_dir} before bringing the stack up.")
        return 0
    if model_registry.is_installed(m, models_dir):
        _safe_print(f"  Already installed: {os.path.join(models_dir, m.model_file)}")
        return 0
    if args.dry_run:
        _safe_print(f"  (dry-run) would install {m.name} into {models_dir}")
        return 0

    # Compose `atlas model install` — inherits all of PC-056.1/.2's gates,
    # SHA verification, lock, oversized-part guard, HF_TOKEN handling.
    install_argv = ["install", m.name, "--models-dir", models_dir]
    if args.no_color:
        install_argv.append("--no-color")
    if args.yes:
        install_argv.append("--yes")
    rc = model.main(install_argv)
    if rc != 0:
        _safe_print(f"  {RED if color else ''}Model install failed (rc={rc}). "
                    f"Re-run `atlas model install {m.name}` after resolving the issue, "
                    f"then re-run `atlas init --reconfigure` to finish wiring .env."
                    f"{RESET if color else ''}")
    return rc


# ---------------------------------------------------------------------------
# Step 4 — write .env
# ---------------------------------------------------------------------------

def _detect_total_ram_gib() -> float:
    """Best-effort total physical RAM in GiB, portable across the platforms
    ATLAS installs on. Returns 0.0 when it can't tell (caller treats that as
    "leave the sandbox memory uncapped")."""
    # Linux — authoritative and dependency-free.
    try:
        with open("/proc/meminfo") as fh:
            for line in fh:
                if line.startswith("MemTotal:"):
                    return int(line.split()[1]) / (1024 ** 2)  # kB → GiB
    except OSError:
        # /proc is Linux-specific; continue to macOS and psutil probes.
        pass
    # macOS / BSD.
    try:
        import subprocess
        out = subprocess.run(["sysctl", "-n", "hw.memsize"],
                             capture_output=True, text=True, timeout=3)
        if out.returncode == 0 and out.stdout.strip():
            return int(out.stdout.strip()) / (1024 ** 3)
    except Exception:  # noqa: S110
        # sysctl is a best-effort platform probe; psutil remains available as
        # the portable fallback.
        pass
    # Portable fallback if psutil happens to be installed.
    try:
        import psutil
        return psutil.virtual_memory().total / (1024 ** 3)
    except Exception:
        return 0.0


def _sandbox_limits() -> Tuple[str, str, str, str, str]:
    """Compute (MEM, PIDS, CPUS, UID, GID) sandbox settings for THIS host.

    The sandbox runs untrusted model-authored shell, so the container is the
    real backstop against resource exhaustion. Memory caps at ~75% of host
    RAM (a ceiling, not a reservation) with a 2 GiB floor; CPUs cap at ~75%
    of cores (1-core floor) so a runaway build can't starve the
    latency-sensitive llama-server; pids_limit is a constant fork-bomb stop.
    UID/GID are the invoking user's, so the non-root sandbox writes
    /workspace files with the right owner (no CAP_DAC_OVERRIDE needed —
    the container drops all capabilities). "0" for mem/cpus means Docker
    leaves them uncapped, so detection failure degrades to the prior
    behavior rather than a bad cap.
    """
    ram = _detect_total_ram_gib()
    if ram <= 0:
        mem = "0"  # undetectable → no cap (no regression vs. before)
    else:
        mem = f"{max(2, int(ram * 0.75))}g"
    cores = os.cpu_count() or 0
    cpus = str(max(1, int(cores * 0.75))) if cores else "0"
    try:
        uid, gid = str(os.getuid()), str(os.getgid())
    except AttributeError:  # non-POSIX host
        uid, gid = "1000", "1000"
    return mem, "1024", cpus, uid, gid


def _render_env(m: model_registry.Model, profile: tier.TierProfile,
                 selected_gpu: Optional[tier.GPUInfo],
                 models_dir: str, atlas_root: str, image_tag: str,
                 ghcr_owner: str,
                 backend_override: Optional[str] = None) -> str:
    """Compose the .env body. Order is stable for diff-friendliness.

    `backend_override` (PC-114): when set, takes precedence over the
    vendor-derived backend. Set by `atlas init --backend vulkan` (or
    the wizard's Vulkan-fallback path when the native backend isn't
    packaged).
    """
    # models_dir written as a relative path when it's the default
    # <atlas_root>/models, absolute otherwise — keeps `.env` portable
    # across cloned checkouts that follow the same layout.
    default_models = os.path.join(atlas_root, "models")
    models_value = "./models" if os.path.abspath(models_dir) == os.path.abspath(default_models) \
        else models_dir

    # Backend selection (V3.1.1) — drives which Dockerfile / image is used
    # and which docker-compose override files apply.
    vendor = selected_gpu.vendor if selected_gpu else "nvidia"
    if backend_override:
        backend_id = backend_override
        # Pretty name for the .env header — keep aligned with the
        # backend-id values the entrypoint dispatches on.
        backend_name = {"cuda": "CUDA", "rocm": "ROCm",
                        "vulkan": "Vulkan",
                        "metal": "Metal (native + hybrid Docker)"}.get(
                            backend_override, backend_override)
    else:
        backend_id, backend_name, _ = _backend_for(vendor)
    gpu_index = str(selected_gpu.index) if selected_gpu else "0"

    sandbox_mem, sandbox_pids, sandbox_cpus, sandbox_uid, sandbox_gid = _sandbox_limits()

    keys = {
        "ATLAS_MODELS_DIR": models_value,
        "ATLAS_MODEL_FILE": m.model_file,
        "ATLAS_MODEL_NAME": m.model_file.rsplit(".", 1)[0],
        # The model's grammar mode from the registry: an install runs the
        # configuration the model was measured with.
        "ATLAS_GRAMMAR_MODE": m.grammar_mode,
        "ATLAS_CTX_SIZE": str(profile.context_length),
        "ATLAS_PARALLEL_SLOTS": str(profile.parallel_slots),
        "ATLAS_KV_TYPE_K": profile.kv_cache_k,
        "ATLAS_KV_TYPE_V": profile.kv_cache_v,
        "ATLAS_BACKEND": backend_id,
        "ATLAS_GPU_VENDOR": vendor,
        "ATLAS_GPU_INDEX": gpu_index,
        "ATLAS_GHCR_OWNER": ghcr_owner,
        "ATLAS_IMAGE_TAG": image_tag,
        "ATLAS_MACOS_PREFIX": "~/.atlas/macos",
        "ATLAS_LLAMA_PORT": "8080",
        "ATLAS_LENS_PORT": "8099",
        "ATLAS_V3_PORT": "8070",
        "ATLAS_SANDBOX_PORT": "30820",
        "ATLAS_PROXY_PORT": "8090",
        # Sandbox resource caps, sized to this host (see _sandbox_limits).
        # mem ~75% of detected RAM; pids a constant fork-bomb stop.
        "ATLAS_SANDBOX_MEM": sandbox_mem,
        "ATLAS_SANDBOX_PIDS": sandbox_pids,
        "ATLAS_SANDBOX_CPUS": sandbox_cpus,
        "ATLAS_SANDBOX_UID": sandbox_uid,
        "ATLAS_SANDBOX_GID": sandbox_gid,
        # The proxy writes the /workspace host bind mount; run it as the
        # invoking user for the same reason as the sandbox — the image's
        # baked-in uid 1001 can't write operator-owned host dirs.
        "ATLAS_PROXY_UID": sandbox_uid,
        "ATLAS_PROXY_GID": sandbox_gid,
    }

    gpu_descr = (f"{selected_gpu.name} ({selected_gpu.vram_gb:.1f} GB VRAM)"
                 if selected_gpu else "none")
    lines = [
        "# ATLAS Compose configuration — generated by `atlas init` (PC-054).",
        f"# Tier: {profile.tier} ({profile.label})",
        f"# Model: {m.name} (lens_status={m.lens_status})",
        f"# Backend: {backend_name} ({backend_id})  |  GPU: {gpu_descr}",
        "# Re-run `atlas init --reconfigure` to regenerate from new defaults.",
        "",
    ]
    if backend_id == "rocm":
        lines.append(
            "# NOTE (ROCm): bring the stack up with the ROCm override:")
        lines.append(
            "#   docker compose -f docker-compose.yml "
            "-f docker-compose.rocm.yml up -d")
        lines.append("")
    elif backend_id == "vulkan":
        lines.append(
            "# NOTE (Vulkan, PC-114): bring the stack up with the Vulkan override:")
        lines.append(
            "#   docker compose -f docker-compose.yml "
            "-f docker-compose.vulkan.yml up -d")
        lines.append(
            "# Vulkan is the universal fallback (~20-40% slower than tuned "
            "native backends) — works on AMD/Intel/Snapdragon/Apple-via-MoltenVK/CPU.")
        lines.append("")
    elif backend_id == "metal":
        lines.append(
            "# NOTE (Metal, #32): hybrid Mac path — native llama-server "
            "+ Docker for everything else.")
        lines.append(
            "# Bring the stack up in TWO steps:")
        lines.append(
            "#   1. ./scripts/atlas-llama-macos.sh        # native llama-server "
            "(uses Metal, 5-10x faster than MoltenVK)")
        lines.append(
            "#   2. docker compose -f docker-compose.yml "
            "-f docker-compose.macos.yml up -d   # proxy + v3 + lens + sandbox")
        lines.append(
            "# Run ./scripts/atlas-setup-macos.sh first if you haven't already "
            "(installs brew deps + builds llama.cpp with Metal). See "
            "docs/SETUP_MACOS.md.")
        lines.append("")
    for k, v in keys.items():
        lines.append(f"{k}={v}")
    lines.append("")
    return "\n".join(lines)


def _backup_if_exists(path: str) -> Optional[str]:
    if not os.path.exists(path):
        return None
    bak = path + ".bak"
    shutil.copy2(path, bak)
    return bak


def _step_write_env(m: model_registry.Model, profile: tier.TierProfile,
                     selected_gpu: Optional[tier.GPUInfo],
                     models_dir: str, atlas_root: str, args: argparse.Namespace,
                     color: bool) -> Tuple[str, Optional[str]]:
    """Returns (env_path, backup_path_or_None). On --dry-run, no writes."""
    env_path = os.path.join(atlas_root, ".env")
    body = _render_env(m, profile, selected_gpu, models_dir, atlas_root,
                       image_tag=args.image_tag,
                       ghcr_owner=args.ghcr_owner,
                       backend_override=args.backend)
    if args.dry_run:
        _safe_print(f"  (dry-run) would write {env_path} ({len(body)} bytes)")
        return env_path, None

    backup = _backup_if_exists(env_path) if args.reconfigure else None
    if backup:
        _safe_print(f"  Backed up existing .env → {backup}")

    with open(env_path, "w") as fh:
        fh.write(body)
    _safe_print(f"  Wrote {env_path}")
    return env_path, backup


# ---------------------------------------------------------------------------
# Step 5 — generate api-keys.json
# ---------------------------------------------------------------------------

def _step_write_api_keys(atlas_root: str, args: argparse.Namespace,
                          color: bool) -> Tuple[str, Optional[str], str]:
    """Returns (path, backup_path_or_None, generated_key_or_existing).

    Permissions: parent dir 0700, file 0600. Refuses to fix loose perms
    on existing parent dir without --yes (security guardrail — the user
    might have intentionally chmod'd it for a multi-user setup)."""
    secrets_dir = os.path.join(atlas_root, "secrets")
    keys_path = os.path.join(secrets_dir, "api-keys.json")

    # Parent dir handling
    if os.path.isdir(secrets_dir):
        mode = os.stat(secrets_dir).st_mode & 0o777
        if mode & 0o077 and not args.yes:
            _safe_print(f"  {YELLOW if color else ''}secrets/ exists with "
                        f"loose permissions ({oct(mode)})."
                        f"{RESET if color else ''}")
            # Interactive: ask before tightening (the user might have
            # chmod'd it intentionally for a multi-user setup).
            # Non-interactive without --yes: refuse — the caller reports
            # failure instead of a silent skip masquerading as success.
            if not _confirm("chmod secrets/ to 0700 and continue?",
                            default_yes=False, args=args):
                _safe_print("  Not writing api-keys.json into a loose "
                            "secrets/ dir. Re-run with --yes to chmod to "
                            "0700, or chmod manually.")
                return keys_path, None, ""

    if args.dry_run:
        _safe_print(f"  (dry-run) would write {keys_path}")
        return keys_path, None, ""

    os.makedirs(secrets_dir, mode=0o700, exist_ok=True)
    try:
        os.chmod(secrets_dir, 0o700)
    except PermissionError:
        pass  # not fatal — dir already exists, perms already strict enough

    backup = _backup_if_exists(keys_path) if args.reconfigure else None
    if backup:
        _safe_print(f"  Backed up existing api-keys.json → {backup}")

    key = "sk-atlas-" + secrets_mod.token_urlsafe(32)
    payload = {key: {"user": "local", "created_by": "atlas init"}}
    body = jsonlib.dumps(payload, indent=2) + "\n"

    # Write with explicit mode via O_CREAT to avoid a brief 0644 window.
    fd = os.open(keys_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w") as fh:
            fh.write(body)
    except Exception:
        try:
            os.close(fd)
        except OSError:
            # best-effort: swallow on failure (caller continues)
            pass
        raise
    try:
        os.chmod(keys_path, 0o600)
    except PermissionError:
        # best-effort: swallow on failure (caller continues)
        pass

    _safe_print(f"  Wrote {keys_path} (mode 0600)")
    _safe_print(f"  API key: {key}")
    _safe_print("    Set this in your client: Authorization: Bearer <key>")
    return keys_path, backup, key


def _step_write_service_token(atlas_root: str, args: argparse.Namespace,
                               color: bool) -> str:
    """Generate <root>/secrets/service-token (0600) unless present.

    The token authenticates internal service-to-service and CLI-to-
    service requests (Authorization: Bearer). Existing tokens are kept
    so re-running init doesn't break a live stack; --rotate-token
    regenerates (restart services afterwards so they reload it). The
    value itself is never printed.
    """
    secrets_dir = os.path.join(atlas_root, "secrets")
    tok_path = os.path.join(secrets_dir, "service-token")

    if args.dry_run:
        _safe_print(f"  (dry-run) would write {tok_path}")
        return tok_path

    if os.path.isfile(tok_path) and not getattr(args, "rotate_token", False):
        _safe_print(f"  Service token already present at {tok_path} (kept; "
                    "use --rotate-token to regenerate)")
        return tok_path

    os.makedirs(secrets_dir, mode=0o700, exist_ok=True)
    token = "atlas-st-" + secrets_mod.token_urlsafe(32)
    fd = os.open(tok_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
    try:
        with os.fdopen(fd, "w") as fh:
            fh.write(token + "\n")
    except Exception:
        # fdopen failing before it takes ownership leaves fd open; a
        # double-close after fdopen succeeded raises OSError — ignore it.
        with contextlib.suppress(OSError):
            os.close(fd)
        raise
    # Re-assert the mode in case a pre-existing file kept looser bits;
    # some filesystems (e.g. certain network mounts) refuse chmod.
    with contextlib.suppress(PermissionError):
        os.chmod(tok_path, 0o600)
    action = "Rotated" if getattr(args, "rotate_token", False) else "Wrote"
    _safe_print(f"  {action} {tok_path} (mode 0600) — internal service auth")
    if getattr(args, "rotate_token", False):
        _safe_print("    Restart the stack so services reload it: "
                    "docker compose restart")
    return tok_path


# ---------------------------------------------------------------------------
# Already-configured guard
# ---------------------------------------------------------------------------

def _refuse_if_already_configured(atlas_root: str, args: argparse.Namespace,
                                    color: bool) -> bool:
    """True = refused (caller should exit). False = proceed."""
    env_path = os.path.join(atlas_root, ".env")
    if not os.path.isfile(env_path):
        return False
    if args.reconfigure:
        return False  # explicit reconfigure — proceed (will back up)
    _safe_print(f"  {RED if color else ''}Already configured: "
                f"{env_path} exists.{RESET if color else ''}")
    _safe_print("  Pass --reconfigure to back up + regenerate, or edit .env directly.")
    return True


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="atlas init",
        description="First-run install wizard: probe hardware, pick a "
                    "model, write .env + api-keys.json. (PC-054)")
    parser.add_argument("--yes", action="store_true",
        help="non-interactive: accept all defaults; required for "
             "scripted bootstrap")
    parser.add_argument("--skip-download", action="store_true",
        help="write config but don't download the model "
             "(bring-your-own gguf)")
    parser.add_argument("--rotate-token", action="store_true",
        help="regenerate secrets/service-token (internal service auth); "
             "restart the stack afterwards so services reload it")
    parser.add_argument("--reconfigure", action="store_true",
        help="back up existing .env and api-keys.json (.bak suffix) "
             "before writing new ones")
    parser.add_argument("--dry-run", action="store_true",
        help="print proposed writes, touch no files, no network")
    parser.add_argument("--json", action="store_true",
        help="machine-readable summary on stdout")
    parser.add_argument("--models-dir", default=None,
        help="override default <atlas_root>/models")
    parser.add_argument("--image-tag", default="latest",
        help="ATLAS_IMAGE_TAG to write into .env (default: latest)")
    parser.add_argument("--ghcr-owner", default="inferstep",
        help="ATLAS_GHCR_OWNER to write into .env (default: inferstep)")
    parser.add_argument("--backend", default=None,
        choices=["cuda", "rocm", "vulkan", "metal"],
        help="force a specific llama-server backend instead of "
             "auto-detecting from GPU vendor. `vulkan` is the universal "
             "fallback (PC-114, #114) — works on basically any GPU + "
             "CPU lavapipe, ~30%% slower than the native backends. "
             "`metal` is the macOS hybrid path (#32) — native llama-server "
             "+ Docker for the rest. Requires running ./scripts/"
             "atlas-setup-macos.sh first. See docs/SETUP_MACOS.md.")
    parser.add_argument("--no-color", action="store_true")
    args = parser.parse_args(argv)

    color = (not args.no_color) and sys.stdout.isatty()
    atlas_root = _find_atlas_root()
    if atlas_root is None:
        _safe_print(f"{RED if color else ''}atlas init: "
                    f"no docker-compose.yml found in {os.getcwd()} or any "
                    f"parent directory.{RESET if color else ''}")
        _safe_print("  The wizard writes .env and secrets/ relative to your "
                    "ATLAS checkout. cd into the repo (or clone it: "
                    "git clone https://github.com/inferstep/ATLAS.git) "
                    "before re-running.")
        return 1
    models_dir = _resolve_models_dir(args.models_dir, atlas_root)

    # Header
    _safe_print(f"{BOLD if color else ''}atlas init{RESET if color else ''} — "
                f"first-run wizard (atlas_root: {atlas_root})")
    _safe_print("")

    if _refuse_if_already_configured(atlas_root, args, color):
        return 1

    # Step 1
    _safe_print("[1/5] Probing hardware…")
    probe, profile, selected_gpu = _step_probe(args, color)
    _safe_print("")

    # Step 2
    _safe_print("[2/5] Selecting model…")
    chosen = _step_select_model(profile, selected_gpu, probe, args, color)
    if chosen is None:
        _safe_print(f"  {RED if color else ''}No installable model found "
                    f"for tier={profile.tier}.{RESET if color else ''}")
        return 1
    _safe_print("")

    # Step 3
    _safe_print("[3/5] Downloading model…")
    rc = _step_download(chosen, models_dir, args, color)
    if rc != 0:
        return rc
    _safe_print("")

    # Step 4
    _safe_print("[4/5] Writing .env…")
    env_path, env_backup = _step_write_env(chosen, profile, selected_gpu,
                                            models_dir, atlas_root, args, color)
    _safe_print("")

    # Step 5
    _safe_print("[5/5] Generating api-keys.json…")
    keys_path, keys_backup, api_key = _step_write_api_keys(atlas_root, args, color)
    if api_key or args.dry_run:
        # Same secured secrets/ dir; skipped when api-keys was refused
        # (loose perms) so we never write a token into a loose dir.
        _step_write_service_token(atlas_root, args, color)
    _safe_print("")
    if not args.dry_run and not api_key:
        # api-keys.json was skipped (loose secrets/ perms, declined or
        # non-interactive) — the install isn't complete, so don't report
        # success.
        _safe_print(f"  {RED if color else ''}Setup incomplete: "
                    f"api-keys.json was not written.{RESET if color else ''}")
        return 1

    # Next steps. args.backend wins over vendor-derived since the wizard
    # writes that into ATLAS_BACKEND above.
    if args.backend:
        backend_id = args.backend
    else:
        backend_id, _, _ = _backend_for(selected_gpu.vendor if selected_gpu else None)
    if not args.dry_run:
        _safe_print(f"{GREEN if color else ''}Setup complete.{RESET if color else ''}")
        _safe_print("Next:")
        if backend_id == "metal":
            # #32 hybrid path — four steps because llama-server runs
            # natively on macOS while the rest stay in Docker. Setup
            # script is idempotent so re-runs are cheap.
            _safe_print("  1. ./scripts/atlas-setup-macos.sh    "
                        "# one-time: brew + build llama.cpp with Metal "
                        "(skip if already done)")
            _safe_print("  2. ./scripts/atlas-llama-macos.sh    "
                        "# starts native llama-server (run in its own terminal)")
            _safe_print("  3. docker compose -f docker-compose.yml "
                        "-f docker-compose.macos.yml up -d   "
                        "# proxy + v3 + lens + sandbox")
            _safe_print("  4. atlas doctor               # verify install health")
            _safe_print("  5. atlas                      # start using ATLAS")
            _safe_print("")
            _safe_print("  See docs/SETUP_MACOS.md for the full walkthrough.")
        else:
            _safe_print("  1. atlas tier fit --write     # size the runtime for "
                        "this model + GPU (tier presets are VRAM-band defaults)")
            if backend_id == "rocm":
                _safe_print("  2. docker compose -f docker-compose.yml "
                            "-f docker-compose.rocm.yml up -d   # bring up the ROCm stack")
            elif backend_id == "vulkan":
                _safe_print("  2. docker compose -f docker-compose.yml "
                            "-f docker-compose.vulkan.yml up -d   # bring up the Vulkan stack")
            else:
                _safe_print("  2. docker compose up -d        # bring up the stack")
            _safe_print("  3. atlas doctor               # verify install health")
            _safe_print("  4. atlas                      # start using ATLAS")

    if args.json:
        out = {
            "atlas_root": atlas_root,
            "tier": profile.tier,
            "model": chosen.name,
            "models_dir": models_dir,
            "env_path": env_path,
            "env_backup": env_backup,
            "api_keys_path": keys_path,
            "api_keys_backup": keys_backup,
            "api_key": api_key,
            "image_tag": args.image_tag,
            "ghcr_owner": args.ghcr_owner,
            "dry_run": args.dry_run,
            "backend": backend_id,
            "gpu": ({"vendor": selected_gpu.vendor, "name": selected_gpu.name,
                     "vram_gb": round(selected_gpu.vram_gb, 1),
                     "index": selected_gpu.index,
                     "compute_target": selected_gpu.compute_target}
                    if selected_gpu else None),
        }
        _safe_print(jsonlib.dumps(out, indent=2))

    return 0


if __name__ == "__main__":
    sys.exit(main())
