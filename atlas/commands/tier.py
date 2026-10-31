"""atlas tier — hardware probe + tier classification (PC-055, split PC-055.2).

Classifies the host's GPU/RAM/disk into one of five tiers and emits the
recommended ATLAS *runtime* settings for that tier (context length,
parallel slots, KV cache quantization). The recommended *model* per tier
lives in `model_registry.py` so PC-056's full model registry can
absorb that surface without churning every caller of TierProfile.

Layering:
    tier.py           -> hardware capability + runtime knobs
    model_registry.py -> per-tier default model (full registry schema)
    init wizard       -> consumes both, writes merged .env

Tier breakpoints are based on VRAM, the hardest constraint for LLM
inference. Vendor-agnostic as of V3.1.1 — NVIDIA, AMD (ROCm), Apple
Silicon (Metal), and Intel Arc (SYCL, planned) are all classified into
the same VRAM bands.

  cpu      no GPU detected           — ATLAS can't run llama.cpp;
                                       documented for completeness
  small    8 GB <= VRAM < 12 GB      — RTX 3060 / 4060 / T4 / RX 6700 XT
  medium   12 GB <= VRAM < 20 GB     — RTX 4060 Ti 16GB / 5060 Ti 16GB /
                                       RX 6800 / Arc A770 / M3 Pro 18GB
                                       (default development target)
  large    20 GB <= VRAM < 32 GB     — RTX 3090 / 4090 / RX 7900 XTX /
                                       M3 Max 36GB
  xlarge   VRAM >= 32 GB             — RTX 5090 32GB / RX 7900 XTX OC /
                                       MI250 / A6000 / A100 / H100 /
                                       M3 Max 48GB+

Settings per tier are tuned for "sensible defaults that won't OOM on the
smallest GPU in the band." Users can always override in `.env` — the
tier output is a recommendation, not a lock.

Apple Silicon caveat: unified memory means the reported "VRAM" is total
system RAM. Realistic GPU budget under load is ~70% (OS + apps eat the
rest). Tier classification uses the raw unified-memory figure but the
tier card flags the caveat.

Invoke:
    atlas tier              # classify this host + show recommendations
    atlas tier list         # show all 5 tier definitions
    atlas tier --json       # machine output (for PC-054 wizard, PC-056)
    atlas tier --raw        # just the probe (no classification)
"""

import argparse
import json
import os
import platform
import shutil
import subprocess
import sys
from dataclasses import dataclass, asdict, field
from typing import List, Optional, Tuple

from atlas.commands import model_registry

# Shared ANSI colors + unicode-safe output primitives.
# NOTE: fit.py imports _safe_print from this module — keep the alias.
from atlas.display import (
    RESET, BOLD, DIM, RED, GREEN, YELLOW as YELL, CYAN,
    UNICODE_OK, DASH, safe_print as _safe_print,
)


# ---------------------------------------------------------------------------
# Probe — read host hardware
# ---------------------------------------------------------------------------

@dataclass
class GPUInfo:
    """One GPU discovered on the host.

    Vendor-agnostic record. `compute_target` semantics by vendor:
      nvidia: CUDA compute capability without the dot, e.g. "89" (Ada),
              "120" (Blackwell). Passed to cmake as CMAKE_CUDA_ARCHITECTURES.
      amd:    HIP gfx target, e.g. "gfx1100" (RDNA3) or "gfx906" (Vega20).
              Passed to cmake/hipcc as AMDGPU_TARGETS / GPU_TARGETS.
              May be None if rocm-smi alone can't determine it — operator
              must set GFX_TARGET manually or the Dockerfile uses a fat
              build covering common targets.
      apple:  None — Metal doesn't take a compute-target arg at build time;
              llama.cpp Metal backend is universal across M1/M2/M3/M4.
      intel:  None — SYCL planned, target TBD.
    """
    vendor: str            # 'nvidia' | 'amd' | 'apple' | 'intel' | 'unknown'
    name: str
    vram_gb: float
    compute_target: Optional[str] = None
    index: int = 0         # vendor-local GPU index (for multi-GPU)


@dataclass
class Probe:
    # All fields have defaults so callers and tests can construct with the
    # subset they care about. Real probe() population still sets everything.
    has_gpu: bool = False
    gpu_name: Optional[str] = None
    gpu_vendor: Optional[str] = None  # 'nvidia' | 'amd' | 'apple' | 'intel' | None
    vram_gb: float = 0.0
    gpu_count: int = 0
    gpus: List[GPUInfo] = field(default_factory=list)  # all detected GPUs
    system_ram_gb: float = 0.0
    cpu_cores: int = 1          # logical cores (incl. SMT)
    disk_free_gb: float = 0.0
    platform: str = "other"  # 'linux' | 'darwin' | 'windows' | 'other'
    # CPU architecture — drives backend availability (rocm is x86_64-only,
    # vulkan is multi-arch). Values: 'x86_64' | 'aarch64' | 'other'.
    # 'aarch64' covers DGX Spark, Snapdragon X Elite, Apple Silicon, Jetson,
    # Pi 5. See #115 for the multi-arch build matrix.
    system_arch: str = "x86_64"

    @property
    def description(self) -> str:
        if not self.has_gpu:
            return (f"{self.platform} | no GPU | {self.cpu_cores} cores "
                    f"| {self.system_ram_gb:.0f} GB RAM")
        vendor_tag = f"[{self.gpu_vendor}]" if self.gpu_vendor else ""
        return (f"{self.platform} | {vendor_tag} {self.gpu_name} "
                f"({self.vram_gb:.1f} GB VRAM) "
                f"| {self.cpu_cores} cores "
                f"| {self.system_ram_gb:.0f} GB RAM "
                f"| {self.disk_free_gb:.0f} GB free disk")


# ---------------------------------------------------------------------------
# Per-vendor GPU detection
# ---------------------------------------------------------------------------

def _read_nvidia_smi() -> List[GPUInfo]:
    """Detect NVIDIA GPUs via nvidia-smi. Returns empty list if not present.

    Queries `index,name,memory.total,compute_cap` so we get the compute
    capability for build-time cmake. Compute cap formatted as "8.9" by
    nvidia-smi; we strip the dot for CMAKE_CUDA_ARCHITECTURES which wants
    "89".
    """
    if not shutil.which("nvidia-smi"):
        return []
    try:
        p = subprocess.run(
            ["nvidia-smi", "--query-gpu=index,name,memory.total,compute_cap",
             "--format=csv,noheader,nounits"],
            capture_output=True, text=True, timeout=10,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return []
    if p.returncode != 0:
        return []
    gpus: List[GPUInfo] = []
    for ln in (line.strip() for line in p.stdout.strip().splitlines()
               if line.strip()):
        parts = [x.strip() for x in ln.split(",")]
        if len(parts) < 3:
            continue
        try:
            idx = int(parts[0])
            name = parts[1]
            vram_gb = float(parts[2]) / 1024.0
            compute = parts[3].replace(".", "") if len(parts) > 3 else None
        except (ValueError, IndexError):
            continue
        gpus.append(GPUInfo(vendor="nvidia", name=name, vram_gb=vram_gb,
                            compute_target=compute, index=idx))
    return gpus


# rocm-smi product-name → gfx target mapping for common consumer/datacenter
# AMD GPUs. Used as a best-effort when rocminfo isn't available. Source:
# https://llvm.org/docs/AMDGPUUsage.html and https://www.llvm.org/docs/AMDGPUUsage.html#processors
_AMD_GFX_BY_NAME: List[Tuple[str, str]] = [
    # RDNA3 (Navi 31/32/33) — RX 7000 series
    ("7900 XTX", "gfx1100"), ("7900 XT", "gfx1100"), ("7900 GRE", "gfx1100"),
    ("7800 XT", "gfx1101"), ("7700 XT", "gfx1101"),
    ("7600",    "gfx1102"),
    # RDNA2 (Navi 21/22/23) — RX 6000 series
    ("6900 XT", "gfx1030"), ("6950 XT", "gfx1030"), ("6800 XT", "gfx1030"),
    ("6800",    "gfx1030"),
    ("6700 XT", "gfx1031"), ("6750 XT", "gfx1031"),
    ("6600 XT", "gfx1032"), ("6650 XT", "gfx1032"), ("6600", "gfx1032"),
    # CDNA2/3 datacenter
    ("MI300X", "gfx942"), ("MI300A", "gfx940"),
    ("MI250X", "gfx90a"), ("MI250",  "gfx90a"),
    ("MI210",  "gfx90a"), ("MI100",  "gfx908"),
    # Vega
    ("Vega 64", "gfx900"), ("Vega 56", "gfx900"),
]


def _amd_gfx_from_name(name: str) -> Optional[str]:
    """Best-effort gfx-target lookup from product name. None if no match."""
    for needle, gfx in _AMD_GFX_BY_NAME:
        if needle.lower() in name.lower():
            return gfx
    return None


def _read_rocm_smi() -> List[GPUInfo]:
    """Detect AMD GPUs via rocm-smi. Returns empty list if not present.

    rocm-smi has shifted JSON schema across versions; we tolerate both the
    pre-6.x "card0" / "Card Series" keys and the 6.x "GPU[0]" / "Card model"
    keys. If --json fails entirely, fall back to text parsing of the
    default rocm-smi table.
    """
    if not shutil.which("rocm-smi"):
        return []
    # Preferred: JSON output covering product name + VRAM
    try:
        p = subprocess.run(
            ["rocm-smi", "--showproductname", "--showmeminfo", "vram", "--json"],
            capture_output=True, text=True, timeout=10,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return []
    if p.returncode != 0 or not p.stdout.strip():
        return []
    try:
        data = json.loads(p.stdout)
    except json.JSONDecodeError:
        return []
    gpus: List[GPUInfo] = []
    for key, info in data.items():
        # Tolerate 'card0', 'GPU[0]', 'GPU0', etc.
        idx_str = ''.join(c for c in key if c.isdigit())
        if not idx_str:
            continue
        try:
            idx = int(idx_str)
        except ValueError:
            continue
        name = (info.get("Card Series") or info.get("Card Model")
                or info.get("Card model") or info.get("Card SKU")
                or info.get("GPU Model") or "AMD GPU")
        # VRAM key varies: "VRAM Total Memory (B)", "VRAM Total (B)",
        # "vram_total_bytes". Try each.
        vram_bytes = 0.0
        for vkey in ("VRAM Total Memory (B)", "VRAM Total (B)",
                     "vram_total_bytes", "VRAM Total"):
            if vkey in info:
                try:
                    vram_bytes = float(str(info[vkey]).replace(",", ""))
                    break
                except ValueError:
                    continue
        vram_gb = vram_bytes / (1024 ** 3) if vram_bytes else 0.0
        gpus.append(GPUInfo(vendor="amd", name=name, vram_gb=vram_gb,
                            compute_target=_amd_gfx_from_name(name),
                            index=idx))
    return gpus


def _read_apple_metal() -> List[GPUInfo]:
    """Detect Apple Silicon GPU via system_profiler. macOS-only.

    Apple Silicon has unified memory — the GPU shares the system RAM
    pool. Reported "VRAM" is total system RAM. Realistic GPU budget
    under load is ~70% (OS + browser + IDE eat the rest). The tier card
    surfaces this caveat in its notes; this detector reports the raw
    figure so downstream code makes the same tier classification
    decision as for dedicated-VRAM platforms.

    Intel Macs (pre-2020) have AMD or Intel GPUs but llama.cpp Metal
    only supports Apple Silicon. We filter on chip family to avoid
    false-positiving an Intel Mac as Metal-capable.
    """
    if sys.platform != "darwin":
        return []
    if not shutil.which("system_profiler"):
        return []
    try:
        p = subprocess.run(
            ["system_profiler", "SPDisplaysDataType", "-json"],
            capture_output=True, text=True, timeout=10,
        )
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return []
    if p.returncode != 0:
        return []
    try:
        data = json.loads(p.stdout)
    except json.JSONDecodeError:
        return []
    unified_ram_gb = _read_system_ram_gb()
    gpus: List[GPUInfo] = []
    for idx, gpu in enumerate(data.get("SPDisplaysDataType", []) or []):
        name = (gpu.get("sppci_model") or gpu.get("_name") or "Apple GPU")
        # Filter: only Apple Silicon GPUs get Metal acceleration for llama.cpp
        # at performant speeds. Intel Macs' AMD/Intel GPUs are not in scope.
        if not any(tag in name for tag in ("Apple M", "Apple GPU")):
            continue
        gpus.append(GPUInfo(vendor="apple", name=name,
                            vram_gb=unified_ram_gb,
                            compute_target=None, index=idx))
    return gpus


def detect_gpu() -> List[GPUInfo]:
    """Detect all GPUs across all vendors. Returns empty list if none found.

    Probes nvidia-smi, rocm-smi, and system_profiler (macOS) in sequence.
    Each helper returns [] silently if its vendor's CLI is absent, so
    this is safe on any host. Multi-vendor hosts (e.g., NVIDIA dGPU +
    Intel iGPU on a workstation) will return entries from each detected
    vendor; `primary_gpu()` picks one.
    """
    gpus: List[GPUInfo] = []
    gpus.extend(_read_nvidia_smi())
    gpus.extend(_read_rocm_smi())
    gpus.extend(_read_apple_metal())
    return gpus


def arch_detect() -> str:
    """Normalize the host CPU architecture string.

    Returns one of:
      'x86_64'  — Intel/AMD 64-bit (covers x86_64, amd64, x64)
      'aarch64' — ARM 64-bit (covers aarch64, arm64; DGX Spark, Apple
                  Silicon, Snapdragon X Elite, Jetson, Pi 5)
      'other'   — anything else (armv7l, ppc64le, riscv64, etc.)

    Drives backend availability:
      x86_64   — all backends (cuda, rocm, vulkan, cpu fallback)
      aarch64  — cuda (NVIDIA sbsa/l4t base images required), vulkan,
                 cpu fallback. NO rocm (AMD ROCm has no arm64 release).
      other    — vulkan only at best; arch is too niche for ATLAS to
                 promise anything.

    Used by `atlas init` to filter the offered backend list so users
    on aarch64 don't get rocm suggested at them, and to surface a
    warning when the host arch + vendor combo has no native path.
    """
    raw = platform.machine().lower()
    if raw in ("x86_64", "amd64", "x64"):
        return "x86_64"
    if raw in ("aarch64", "arm64"):
        return "aarch64"
    return "other"


def vulkan_available() -> bool:
    """Probe whether the Vulkan universal backend (PC-114) can run on
    this host. Used by the `atlas init` wizard to decide whether to
    offer `vulkan` as a fallback when no recognized vendor's native
    backend is available.

    Lenient on purpose: we want the wizard to be able to suggest Vulkan
    even on hosts without vulkaninfo (the runtime stack lives inside the
    docker image — mesa-vulkan-drivers covers Mesa RADV/ANV/lavapipe).
    True when ANY of these is true:
      1. vulkaninfo is on PATH (most reliable signal)
      2. /dev/dri exists (Linux GPU rendering nodes — covers AMD/Intel
         and NVIDIA-via-toolkit cases for Vulkan in the container)
      3. host is macOS (MoltenVK path via QEMU + Docker Desktop)
      4. there's already a detected GPU (worst-case, Vulkan via lavapipe
         CPU fallback inside the container will at least boot)

    False when none of those hold — typically a non-Linux/non-macOS host
    with no GPU at all, where the container couldn't even fall back to
    lavapipe sensibly.
    """
    if shutil.which("vulkaninfo"):
        return True
    if os.path.exists("/dev/dri"):
        return True
    if sys.platform == "darwin":
        return True
    # Last-resort: if the regular GPU probes found ANYTHING, Vulkan-in-
    # docker will probably work even without host-side vulkaninfo.
    if detect_gpu():
        return True
    return False


def primary_gpu(gpus: List[GPUInfo],
                override_vendor: Optional[str] = None,
                override_index: Optional[int] = None) -> Optional[GPUInfo]:
    """Pick the GPU ATLAS will use for inference.

    Default policy: largest VRAM wins. On hybrid-graphics systems (iGPU +
    dGPU, or NVIDIA + Intel), the smaller/display GPU is filtered out by
    this rule.

    Overrides (typically from ATLAS_GPU_VENDOR / ATLAS_GPU_INDEX env vars
    set by the installer or operator):
      override_vendor — constrain selection to this vendor (e.g. force AMD
                        when both NVIDIA and AMD are present).
      override_index  — within the chosen vendor pool, pick this index.

    If overrides don't match any detected GPU, fall back to the auto-pick
    rule rather than returning None — better to run on *something* than
    fail at startup.
    """
    if not gpus:
        return None
    candidates = gpus
    if override_vendor:
        filtered = [g for g in gpus if g.vendor == override_vendor.lower()]
        if filtered:
            candidates = filtered
    if override_index is not None:
        for g in candidates:
            if g.index == override_index:
                return g
    return max(candidates, key=lambda g: g.vram_gb)


def _read_system_ram_gb() -> float:
    """Cross-platform best-effort system RAM read."""
    # Linux/most-Unix: /proc/meminfo
    try:
        with open("/proc/meminfo") as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    kb = int(line.split()[1])
                    return kb / (1024 * 1024)
    except (FileNotFoundError, ValueError, IndexError):
        # best-effort: swallow on failure (caller continues)
        pass
    # Fallback for macOS (sysctl) or other
    try:
        p = subprocess.run(["sysctl", "-n", "hw.memsize"],
                           capture_output=True, text=True, timeout=5)
        if p.returncode == 0:
            return int(p.stdout.strip()) / (1024 ** 3)
    except (subprocess.TimeoutExpired, FileNotFoundError):
        # best-effort: swallow on failure (caller continues)
        pass
    return 0.0


def _read_disk_free_gb(path: str = "/") -> float:
    try:
        st = shutil.disk_usage(path)
        return st.free / (1024 ** 3)
    except OSError:
        return 0.0


def _read_cpu_cores() -> int:
    """Logical CPU count (includes SMT/hyperthreading).

    os.cpu_count() returns None on rare platforms; fall back to 1
    rather than crashing.
    """
    return os.cpu_count() or 1


def probe(install_dir: Optional[str] = None) -> Probe:
    """Run all hardware probes and return a Probe.

    GPU selection: detects across NVIDIA/AMD/Apple, then honors
    ATLAS_GPU_VENDOR + ATLAS_GPU_INDEX env-var overrides if set; otherwise
    auto-picks largest VRAM. The full GPU list is preserved on the Probe
    so wizard / multi-GPU UI can offer the operator a choice.
    """
    gpus = detect_gpu()
    override_vendor = os.environ.get("ATLAS_GPU_VENDOR") or None
    override_index_str = os.environ.get("ATLAS_GPU_INDEX")
    try:
        override_index = int(override_index_str) if override_index_str else None
    except ValueError:
        override_index = None
    primary = primary_gpu(gpus, override_vendor=override_vendor,
                          override_index=override_index)
    sys_ram = _read_system_ram_gb()
    cpu_cores = _read_cpu_cores()
    # Probe disk against where ATLAS will live (model files are large).
    disk_path = install_dir if install_dir and os.path.isdir(install_dir) else "/"
    disk_free = _read_disk_free_gb(disk_path)
    plat = sys.platform if sys.platform in ("linux", "darwin", "win32") else "other"
    plat = "windows" if plat == "win32" else plat
    arch = arch_detect()
    return Probe(
        has_gpu=primary is not None,
        gpu_name=primary.name if primary else None,
        gpu_vendor=primary.vendor if primary else None,
        vram_gb=primary.vram_gb if primary else 0.0,
        gpu_count=len(gpus),
        gpus=gpus,
        system_ram_gb=sys_ram,
        cpu_cores=cpu_cores,
        disk_free_gb=disk_free,
        platform=plat,
        system_arch=arch,
    )


# ---------------------------------------------------------------------------
# Tier definitions
# ---------------------------------------------------------------------------

@dataclass
class TierProfile:
    """Recommended ATLAS *runtime* settings for a hardware tier.

    PC-055.2 split: this record is pure *hardware capability* + the
    llama-server runtime knobs that derive from it. Model selection
    (which gguf to load) lives in `model_registry.py` so PC-056's
    full model registry can absorb that surface without touching tiers.

    Runtime fields map directly to docker-compose.yml / .env knobs:
      context_length   -> ATLAS_CTX_SIZE / CONTEXT_LENGTH
      parallel_slots   -> ATLAS_PARALLEL_SLOTS  (llama-server --parallel)
      kv_cache_k       -> ATLAS_KV_TYPE_K (llama-server -ctk)
      kv_cache_v       -> ATLAS_KV_TYPE_V (llama-server -ctv)

    The min_* fields are constraint floors used by `evaluate_constraints`.
    They reflect "what you actually need to run ATLAS at this tier without
    host-side OOMs," not just the GPU dimension. ATLAS is heavy on CPU
    (V3 pipeline PR-CoT repair, sandbox compiles, lens scoring) and RAM
    (5 containers each with their own RSS, plus sandbox tmpfs), so a
    16 GB GPU paired with 8 GB host RAM is a real OOM risk.
    """
    tier: str            # cpu | small | medium | large | xlarge
    label: str           # short human name
    description: str
    min_vram_gb: float
    max_vram_gb: Optional[float]  # None = unbounded above
    example_gpus: List[str]
    # Recommended runtime settings (model-independent — same gguf
    # quant choice doesn't change context/slots/KV caching strategy).
    context_length: int
    parallel_slots: int
    kv_cache_k: str
    kv_cache_v: str
    # Per-tier system minimums (PC-055.1)
    min_system_ram_gb: float
    min_cpu_cores: int
    min_disk_gb: float
    notes: str

    def env_vars(self) -> dict:
        """Render the recommended *runtime* settings as a dict suitable
        for .env writing.

        Model-related env vars (ATLAS_MODEL_FILE, ATLAS_MODEL_NAME) are
        rendered by `model_registry.Model.env_vars()`.
        Wizard / installer code merges the two dicts before writing .env.
        """
        return {
            "ATLAS_CTX_SIZE": str(self.context_length),
            "ATLAS_PARALLEL_SLOTS": str(self.parallel_slots),
            "ATLAS_KV_TYPE_K": self.kv_cache_k,
            "ATLAS_KV_TYPE_V": self.kv_cache_v,
        }


# Tier breakpoints. Order matters — classify() walks top-down picking the
# first whose VRAM range matches.
TIERS: List[TierProfile] = [
    TierProfile(
        tier="cpu",
        label="CPU-only (no GPU)",
        description="No GPU detected. ATLAS requires a CUDA, ROCm, or "
                    "Metal-capable GPU for llama.cpp inference. CPU-only "
                    "is documented for completeness but not supported in v1.",
        min_vram_gb=0.0, max_vram_gb=0.0,
        example_gpus=[],
        context_length=0,
        parallel_slots=0,
        kv_cache_k="N/A", kv_cache_v="N/A",
        min_system_ram_gb=0, min_cpu_cores=0, min_disk_gb=0,
        notes="Supported backends: NVIDIA (CUDA), AMD (ROCm — V3.1.1), "
              "Apple Silicon (Metal — macOS hybrid, #32), Intel Arc (SYCL — roadmap).",
    ),
    TierProfile(
        tier="small",
        label="Small (entry-level GPU)",
        description="Conservative settings sized for 8 GB cards. "
                    "Choose a registry model that leaves room for KV cache "
                    "and compute buffers.",
        min_vram_gb=8.0, max_vram_gb=12.0,
        example_gpus=["RTX 3060 8GB", "RTX 4060 8GB",
                      "RX 6600 XT 8GB", "RX 7600 8GB",
                      "Arc A580 8GB",
                      "T4 16GB (datacenter)"],
        context_length=8192,
        parallel_slots=1,
        kv_cache_k="q4_0", kv_cache_v="q4_0",
        # 5 containers (~7 GB combined RSS) + host OS + sandbox tmpfs
        # + V3 pipeline burst memory ~= 12 GB minimum.
        min_system_ram_gb=12.0,
        # 4 cores: 1 for proxy/lens idle, 1 for sandbox compiles,
        # 1 for v3 PR-CoT repair, 1 for llama prompt processing.
        min_cpu_cores=4,
        # Model (4.4 GB) + container images (8 GB) + ~7 GB working space.
        min_disk_gb=20.0,
        notes="Q4 KV cache trades ~5% quality for ~50% memory. "
              "Increase to q8_0 if you have 12 GB and find quality lacking.",
    ),
    TierProfile(
        tier="medium",
        label="Medium (mid-range GPU)",
        description="ATLAS development target. Registry-selected model with "
                    "32K context and q8/q4 KV cache.",
        min_vram_gb=12.0, max_vram_gb=20.0,
        example_gpus=["RTX 4060 Ti 16GB", "RTX 5060 Ti 16GB",
                      "RTX 3080 Ti 12GB", "RTX 4070 Ti Super 16GB",
                      "RX 6800 16GB", "RX 7700 XT 12GB",
                      "Arc A770 16GB",
                      "Apple M3 Pro 18GB (unified)"],
        context_length=32768,
        parallel_slots=1,
        kv_cache_k="q8_0", kv_cache_v="q4_0",
        # Larger model + 4× context vs small means more KV cache + more
        # prompt processing memory in v3 PR-CoT.
        min_system_ram_gb=16.0,
        min_cpu_cores=4,
        # Model (6.9 GB) + images (8 GB) + ~10 GB working.
        min_disk_gb=25.0,
        notes="Default ATLAS configuration. Verified on RTX 5060 Ti 16GB "
              "with ~3 GB headroom remaining. Apple Silicon: unified memory "
              "means realistic GPU budget is ~70% of system RAM.",
    ),
    TierProfile(
        tier="large",
        label="Large (high-end consumer GPU)",
        description="Headroom for a larger registry model with 32K context and "
                    "2 parallel slots for multi-conversation.",
        min_vram_gb=20.0, max_vram_gb=32.0,
        example_gpus=["RTX 3090 24GB", "RTX 4090 24GB", "RTX 5090 24GB",
                      "RX 7900 XT 20GB", "RX 7900 XTX 24GB",
                      "Apple M3 Max 36GB (unified)"],
        context_length=32768,
        parallel_slots=2,
        kv_cache_k="q8_0", kv_cache_v="q8_0",
        # 2 parallel slots = doubled prompt-processing CPU + memory.
        min_system_ram_gb=24.0,
        min_cpu_cores=8,
        # Model (10.5 GB) + images (8 GB) + ~16 GB working / scratch
        # for sandbox compiles + V3 ablation results during dev.
        min_disk_gb=35.0,
        notes="2 parallel slots lets ATLAS handle a coding session + "
              "background V3 verification without queueing.",
    ),
    TierProfile(
        tier="xlarge",
        label="X-Large (datacenter GPU)",
        description="Large registry model with 64K context, 2-4 parallel slots, "
                    "and full F16 KV cache for maximum quality.",
        min_vram_gb=32.0, max_vram_gb=None,
        example_gpus=["RTX 5090 32GB", "RTX A6000 48GB",
                      "A100 40/80GB", "H100 80GB",
                      "MI210 64GB", "MI250 128GB", "MI300X 192GB",
                      "Apple M3 Max 48GB+ (unified)"],
        context_length=65536,
        parallel_slots=2,
        kv_cache_k="f16", kv_cache_v="f16",
        # 32B + 64K context + F16 KV is sustained-throughput territory;
        # CPU bottleneck on prompt processing becomes real with long contexts.
        min_system_ram_gb=32.0,
        min_cpu_cores=16,
        # Model (23 GB) + images (8 GB) + 30 GB headroom for benchmark
        # outputs / multi-model A/B testing typical of datacenter use.
        min_disk_gb=60.0,
        notes="F16 KV cache + 64K context costs ~10 GB of cache. "
              "If you have 80 GB+ VRAM, bump parallel_slots to 4 and "
              "context to 131072 manually in .env.",
    ),
]


# ---------------------------------------------------------------------------
# Constraint evaluation (PC-055.1)
# ---------------------------------------------------------------------------

@dataclass
class ConstraintCheck:
    """One axis of host-vs-tier constraint comparison."""
    name: str        # 'gpu_vram' | 'system_ram' | 'cpu_cores' | 'disk_free'
    actual: float
    required: float
    unit: str        # 'GB' | 'cores'
    status: str      # 'pass' | 'warn' | 'fail'
    message: str     # human-readable, includes shortfall if any


def evaluate_constraints(p: Probe, t: TierProfile) -> List[ConstraintCheck]:
    """Check each per-tier minimum against the probe.

    Returns a list of ConstraintCheck — one per axis. Status semantics:
      pass — comfortably above minimum
      warn — below minimum but within ~15% (RAM/disk) or any shortage (CPU)
      fail — meaningfully below minimum, will OOM or fail to install

    CPU shortage is always warn-level (not fail) because it makes ATLAS
    slow but doesn't OOM. RAM/disk shortage past the warn threshold is
    fail because it actually breaks runtime.
    """
    checks: List[ConstraintCheck] = []

    # GPU VRAM — only meaningful for GPU tiers
    if t.tier != "cpu":
        if p.vram_gb >= t.min_vram_gb:
            checks.append(ConstraintCheck(
                "gpu_vram", p.vram_gb, t.min_vram_gb, "GB", "pass",
                f"{p.vram_gb:.1f} GB >= {t.min_vram_gb:.0f} GB minimum"))
        else:
            shortfall = t.min_vram_gb - p.vram_gb
            checks.append(ConstraintCheck(
                "gpu_vram", p.vram_gb, t.min_vram_gb, "GB", "fail",
                f"{p.vram_gb:.1f} GB / {t.min_vram_gb:.0f} GB minimum "
                f"({shortfall:.1f} GB short — llama-server will OOM)"))

    # System RAM — 5 containers + V3 pipeline + host OS
    if p.system_ram_gb >= t.min_system_ram_gb:
        checks.append(ConstraintCheck(
            "system_ram", p.system_ram_gb, t.min_system_ram_gb, "GB", "pass",
            f"{p.system_ram_gb:.1f} GB >= {t.min_system_ram_gb:.0f} GB minimum"))
    elif p.system_ram_gb >= t.min_system_ram_gb * 0.85:
        shortfall = t.min_system_ram_gb - p.system_ram_gb
        checks.append(ConstraintCheck(
            "system_ram", p.system_ram_gb, t.min_system_ram_gb, "GB", "warn",
            f"{p.system_ram_gb:.1f} GB / {t.min_system_ram_gb:.0f} GB minimum "
            f"({shortfall:.1f} GB short — V3 pipeline may OOM under load)"))
    else:
        shortfall = t.min_system_ram_gb - p.system_ram_gb
        checks.append(ConstraintCheck(
            "system_ram", p.system_ram_gb, t.min_system_ram_gb, "GB", "fail",
            f"{p.system_ram_gb:.1f} GB / {t.min_system_ram_gb:.0f} GB minimum "
            f"({shortfall:.1f} GB short — host will OOM during V3 + sandbox compiles)"))

    # CPU cores — affects throughput (prompt processing, V3 PR-CoT, sandbox)
    if p.cpu_cores >= t.min_cpu_cores:
        checks.append(ConstraintCheck(
            "cpu_cores", float(p.cpu_cores), float(t.min_cpu_cores), "cores", "pass",
            f"{p.cpu_cores} cores >= {t.min_cpu_cores} minimum"))
    else:
        # CPU shortage = slow, not OOM. Always warn (never fail).
        checks.append(ConstraintCheck(
            "cpu_cores", float(p.cpu_cores), float(t.min_cpu_cores), "cores", "warn",
            f"{p.cpu_cores} cores / {t.min_cpu_cores} minimum "
            f"(V3 pipeline + prompt processing will be slow)"))

    # Disk free — for model + images + working space
    if p.disk_free_gb >= t.min_disk_gb:
        checks.append(ConstraintCheck(
            "disk_free", p.disk_free_gb, t.min_disk_gb, "GB", "pass",
            f"{p.disk_free_gb:.0f} GB free >= {t.min_disk_gb:.0f} GB minimum"))
    elif p.disk_free_gb >= t.min_disk_gb * 0.7:
        shortfall = t.min_disk_gb - p.disk_free_gb
        checks.append(ConstraintCheck(
            "disk_free", p.disk_free_gb, t.min_disk_gb, "GB", "warn",
            f"{p.disk_free_gb:.0f} GB free / {t.min_disk_gb:.0f} GB minimum "
            f"({shortfall:.0f} GB short — clean cache before model download)"))
    else:
        shortfall = t.min_disk_gb - p.disk_free_gb
        checks.append(ConstraintCheck(
            "disk_free", p.disk_free_gb, t.min_disk_gb, "GB", "fail",
            f"{p.disk_free_gb:.0f} GB free / {t.min_disk_gb:.0f} GB minimum "
            f"({shortfall:.0f} GB short — model download or container pull will fail)"))

    return checks


def overall_status(checks: List[ConstraintCheck]) -> str:
    """Reduce a list of constraints to the worst-case status."""
    if any(c.status == "fail" for c in checks):
        return "fail"
    if any(c.status == "warn" for c in checks):
        return "warn"
    return "pass"


def _round_floats(d: dict, ndigits: int = 1) -> dict:
    """Round float values in a dict for clean JSON output.

    Vendor SMI tools report memory in MiB/bytes; converting to GB produces
    noisy decimals like 15.9287109375. Downstream consumers (PC-054 wizard,
    PC-056 model registry) want clean numbers — they never need 4-decimal
    VRAM precision since tier breakpoints are integer-aligned. Nested
    lists/dicts (e.g. Probe.gpus) are passed through unchanged; JSON's
    own float serializer handles those.
    """
    out = {}
    for k, v in d.items():
        if isinstance(v, float):
            out[k] = round(v, ndigits)
        else:
            out[k] = v
    return out


def classify(p: Probe) -> TierProfile:
    """Pick the tier whose VRAM range contains this probe's VRAM.

    GPU present but VRAM below the smallest tier (e.g., 4 GB) returns
    `small` with a notes-level warning rather than `cpu`, so users with a
    too-small GPU at least see what to upgrade to. Pure no-GPU OR
    has_gpu-but-zero-VRAM (rare vendor-SMI failure mode where the tool
    returns the GPU but reports zero memory) returns `cpu` since neither
    can run llama.cpp.

    Apple Silicon unified memory: VRAM = total system RAM. A 16 GB MBP
    lands in `medium` tier on raw numbers, but the tier card's notes
    field warns that realistic GPU budget is ~70% under load.
    """
    if not p.has_gpu or p.vram_gb <= 0:
        return TIERS[0]  # cpu
    for t in TIERS[1:]:
        if t.max_vram_gb is None:
            if p.vram_gb >= t.min_vram_gb:
                return t
        elif t.min_vram_gb <= p.vram_gb < t.max_vram_gb:
            return t
    # GPU present but below smallest tier breakpoint. Return small with
    # a runtime note about insufficient VRAM (caller can render the
    # tier.notes field, which already explains the trade-offs).
    small = TIERS[1]
    return TierProfile(**{**asdict(small),
        "notes": (f"Your GPU has only {p.vram_gb:.1f} GB VRAM, below the "
                  f"{small.min_vram_gb:.0f} GB minimum for the small tier. "
                  f"ATLAS will likely OOM. " + small.notes)})


def by_name(name: str) -> Optional[TierProfile]:
    """Look up a tier by its short name."""
    for t in TIERS:
        if t.tier == name:
            return t
    return None


# ---------------------------------------------------------------------------
# Output
# ---------------------------------------------------------------------------

def _print_tier_card(t: TierProfile, p: Optional[Probe], color: bool) -> None:
    bar = (CYAN + "─" * 60 + RESET) if color and UNICODE_OK else ("-" * 60)
    _safe_print(bar)
    title = f"{BOLD}Tier: {t.tier}{RESET}" if color else f"Tier: {t.tier}"
    _safe_print(f"  {title}  {DASH}  {t.label}")
    _safe_print(bar)
    _safe_print(f"  {DIM if color else ''}{t.description}{RESET if color else ''}")
    _safe_print()
    if t.tier == "cpu":
        _safe_print("  VRAM range:    n/a (no GPU detected)")
    elif t.max_vram_gb is None:
        _safe_print(f"  VRAM range:    {t.min_vram_gb:.0f} GB and up")
    else:
        _safe_print(f"  VRAM range:    {t.min_vram_gb:.0f} GB {DASH} "
                    f"{t.max_vram_gb:.0f} GB")
    if t.example_gpus:
        _safe_print(f"  Example GPUs:  {', '.join(t.example_gpus)}")
    _safe_print()
    _safe_print(f"  {BOLD}Recommended ATLAS settings:{RESET}" if color
                else "  Recommended ATLAS settings:")
    # Surfaced here so users see the lens warning before committing to install.
    rec = model_registry.for_tier(t.tier)
    if rec is not None:
        lens = rec.lens_status
        if lens == "supported":
            lens_marker = (f"{GREEN}[Lens supported]{RESET}" if color
                           else "[Lens supported]")
        elif lens == "no-artifacts":
            lens_marker = (f"{YELL}[Lens NO-ARTIFACTS — G(x) will no-op]{RESET}"
                           if color else "[Lens NO-ARTIFACTS — G(x) will no-op]")
        else:
            lens_marker = ""
        _safe_print(f"    Model:           {rec.model_display} "
                    f"({rec.model_size_gb:.1f} GB on disk)  {lens_marker}")
        _safe_print(f"    File:            {rec.model_file}")
    else:
        _safe_print("    Model:           (no default recommendation)")
    _safe_print(f"    Context length:  {t.context_length:,} tokens")
    _safe_print(f"    Parallel slots:  {t.parallel_slots}")
    _safe_print(f"    KV cache K / V:  {t.kv_cache_k} / {t.kv_cache_v}")
    _safe_print()
    _safe_print(f"  {BOLD}System minimums for this tier:{RESET}" if color
                else "  System minimums for this tier:")
    _safe_print(f"    System RAM:      {t.min_system_ram_gb:.0f} GB")
    _safe_print(f"    CPU cores:       {t.min_cpu_cores}")
    _safe_print(f"    Disk free:       {t.min_disk_gb:.0f} GB")
    _safe_print()
    _safe_print(f"  Notes: {t.notes}")
    if p is not None:
        _print_constraints(p, t, color)


def _constraint_icon(status: str, color: bool) -> str:
    if not color or not UNICODE_OK:
        return {"pass": "[OK]  ", "warn": "[WARN]", "fail": "[FAIL]"}[status]
    return {"pass": f"{GREEN}✓{RESET}", "warn": f"{YELL}⚠{RESET}",
            "fail": f"{RED}✗{RESET}"}[status]


def _print_constraints(p: Probe, t: TierProfile, color: bool) -> None:
    """PC-055.1: render the host-vs-tier constraint table."""
    if t.tier == "cpu":
        # No GPU = no useful constraint check at this tier.
        return
    checks = evaluate_constraints(p, t)
    _safe_print()
    _safe_print(f"  {BOLD}Constraint check (your host vs this tier):{RESET}"
                if color else "  Constraint check (your host vs this tier):")
    for c in checks:
        icon = _constraint_icon(c.status, color)
        label = {"gpu_vram": "GPU VRAM", "system_ram": "System RAM",
                 "cpu_cores": "CPU cores", "disk_free": "Disk free"}[c.name]
        _safe_print(f"    {icon}  {label:14s}  {c.message}")
    overall = overall_status(checks)
    _safe_print()
    if overall == "pass":
        verdict = (f"  {GREEN}OK{RESET}: this tier is a comfortable fit "
                   f"for your hardware." if color else
                   "  OK: this tier is a comfortable fit for your hardware.")
    elif overall == "warn":
        verdict = (f"  {YELL}Warning{RESET}: this tier is borderline. "
                   f"ATLAS will run but may struggle under load."
                   if color else
                   "  Warning: this tier is borderline. ATLAS will run "
                   "but may struggle under load.")
    else:
        verdict = (f"  {RED}Fail{RESET}: at least one constraint is short "
                   f"of the minimum. ATLAS will OOM or fail to install."
                   if color else
                   "  Fail: at least one constraint is short of the minimum. "
                   "ATLAS will OOM or fail to install.")
    _safe_print(verdict)


def _emit_classify(p: Probe, t: TierProfile, args: argparse.Namespace,
                   color: bool) -> int:
    if args.json:
        # Include constraints + overall status so PC-054 wizard and PC-056
        # model registry can render the same evaluation without re-implementing
        # the rules. Existing keys (probe, tier, env, constraints, overall)
        # preserved across PC-055.1 -> PC-055.2; the only schema change is
        # that `tier` no longer carries model_*. The model fields move under
        # the new `recommendation` key, and `env` is the merged dict (model
        # vars + tier runtime vars) so consumers writing .env get one bag.
        constraints = (evaluate_constraints(p, t) if t.tier != "cpu" else [])
        rec = model_registry.for_tier(t.tier)
        env = dict(t.env_vars())
        if rec is not None:
            env.update(rec.env_vars())
        out = {
            "probe": _round_floats(asdict(p)),
            "tier": asdict(t),
            "recommendation": (model_registry.as_dict(rec)
                               if rec is not None else None),
            "env": env,
            "constraints": [_round_floats(asdict(c)) for c in constraints],
            "overall": overall_status(constraints) if constraints else "skip",
        }
        print(json.dumps(out, indent=2, ensure_ascii=not UNICODE_OK))
        return 0
    if args.raw:
        for k, v in _round_floats(asdict(p)).items():
            _safe_print(f"  {k:18s} {v}")
        return 0
    hdr = f"{BOLD}ATLAS tier{RESET}" if color else "ATLAS tier"
    _safe_print(f"{hdr} {DASH} probing host hardware")
    _safe_print()
    _safe_print(f"  Detected: {p.description}")
    _safe_print()
    _print_tier_card(t, p, color)
    _safe_print()
    if t.tier == "cpu":
        _safe_print(f"  {YELL if color else ''}Warning: ATLAS requires "
                    f"a GPU (NVIDIA CUDA, AMD ROCm, or Apple Silicon "
                    f"Metal). See SETUP.md.{RESET if color else ''}")
        return 1
    _safe_print("  Apply these settings: edit .env to set the values "
                "shown above.")
    _safe_print(f"  Or run: {CYAN if color else ''}atlas init{RESET if color else ''}.")
    # Exit non-zero on constraint failure so scripts (bootstrap, CI) can
    # gate on it. Warnings stay exit 0 — they're actionable, not fatal.
    overall = overall_status(evaluate_constraints(p, t))
    return 1 if overall == "fail" else 0


def _emit_list(args: argparse.Namespace, color: bool) -> int:
    if args.json:
        print(json.dumps([asdict(t) for t in TIERS], indent=2,
                         ensure_ascii=not UNICODE_OK))
        return 0
    hdr = f"{BOLD}ATLAS tier definitions (PC-055){RESET}" if color else (
        "ATLAS tier definitions (PC-055)")
    _safe_print(hdr)
    _safe_print()
    for t in TIERS:
        _print_tier_card(t, None, color)
        _safe_print()
    return 0


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="atlas tier",
        description="Hardware tier classification (PC-055)")
    parser.add_argument("subcommand", nargs="?", default="classify",
        choices=["classify", "list", "fit"],
        help="`classify` (default) probes this host. `list` shows all tiers. "
             "`fit` sizes llama-server runtime knobs for the configured model "
             "on this GPU (PC-208).")
    parser.add_argument("model", nargs="?", default=None,
        help="for `fit`: GGUF path (default: the model configured in .env)")
    parser.add_argument("--write", action="store_true",
        help="for `fit`: write the result into .env")
    parser.add_argument("--slots", type=int, default=None,
        help="for `fit`: parallel slot count (default: 4)")
    parser.add_argument("--json", action="store_true",
        help="emit JSON output (for PC-054 wizard, PC-056 model registry)")
    parser.add_argument("--raw", action="store_true",
        help="print probe output only, no classification")
    parser.add_argument("--no-color", action="store_true",
        help="disable ANSI color")
    parser.add_argument("--install-dir", default=None,
        help="probe disk free against this path (defaults to /)")
    args = parser.parse_args(argv)

    color = sys.stdout.isatty() and not args.no_color and not args.json

    if args.subcommand == "list":
        return _emit_list(args, color)

    if args.subcommand == "fit":
        from atlas.commands import fit as fit_module
        return fit_module.emit_fit(args, color)

    p = probe(install_dir=args.install_dir)
    t = classify(p)
    return _emit_classify(p, t, args, color)


if __name__ == "__main__":
    sys.exit(main())
