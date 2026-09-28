# ATLAS Setup Guide

Four deployment methods: **one-shot bootstrap** (recommended for new installs), Docker Compose (manual), bare-metal, or K3s.

---

## Pick your install path

The install steps depend on your hardware + OS. Find the row that matches your setup, then jump to the linked section.

| Your hardware | OS | Recommended path | Support level ([matrix](../SUPPORT_MATRIX.md)) |
|---|---|---|---|
| NVIDIA RTX 50-series / Blackwell (B100, GB10) | Linux | [Method 0: bootstrap](#method-0-one-shot-bootstrap) or [Method 1: Docker](#method-1-docker-compose-recommended) | Supported — published CUDA image targets Blackwell |
| NVIDIA RTX 20/30/40, GTX 10xx, datacenter (V100/A100/H100/T4/L4) | Linux | [Method 1: Docker](#method-1-docker-compose-recommended) + one-time [local rebuild](#cuda-compute-capability-dockerfilev31) | Preview — local rebuild required |
| NVIDIA GPU | Windows (WSL2) | [Method 1: Docker — NVIDIA section](#method-1-docker-compose-recommended) | Unsupported — untested, no claims made; reports welcome |
| AMD GPU (RX 6000/7000, MI200+) | Linux | [Method 1: Docker — AMD ROCm](#amd-rocm--whats-different) | Community-tested ([GH #26](https://github.com/inferstep/ATLAS/issues/26)) |
| **Apple Silicon (M1/M2/M3/M4)** | **macOS** | **[SETUP_MACOS.md](SETUP_MACOS.md)** (dedicated guide — hybrid native Metal + Docker) | Supported (maintainer-verified, M2 Pro) |
| Intel Arc / Iris Xe | Linux | [Method 1: Docker — Vulkan](#vulkan--cross-vendor-fallback) | Preview — Vulkan is smoke-tested on lavapipe only; no real-GPU validation yet |
| Snapdragon X Elite (laptops) | Linux | [Vulkan](#vulkan--cross-vendor-fallback) + [arm64 section](#arm64) | Preview (Linux arm64). Windows on ARM is Unsupported |
| Older AMD GPU (Vega, RDNA1, no ROCm 6.x) | Linux | [Method 1: Docker — Vulkan](#vulkan--cross-vendor-fallback) | Preview |
| NVIDIA on ARM64 (DGX Spark, Jetson) | Linux | [arm64 section](#arm64) (CUDA via sbsa/l4t base swap) | Preview — build recipes provided, no device validated end-to-end yet (#115) |
| Raspberry Pi 5 | Linux | [Vulkan + arm64](#arm64) | Preview — expect CPU-tier perf |
| Intel Mac (pre-2020) | macOS | [Method 1: Docker — Vulkan](#vulkan--cross-vendor-fallback) | Unsupported — requires Docker Desktop (untested); Metal is Apple-Silicon-only |
| CPU only, no GPU | any | [CPU-only install](#cpu-only) | Preview — smoke-test only, very slow |
| Kubernetes cluster | Linux | [Method 3: K3s](#method-3-k3s) | Preview — templates CI-validated; no automated live-cluster test |
| Bare-metal / development | Linux | [Method 2: Bare Metal](#method-2-bare-metal) | Preview — manual validation only |

Don't see your setup? File an issue with `uname -a` output and `lspci | grep -i vga` (Linux) / `system_profiler SPDisplaysDataType` (Mac) and we'll add a row.

---

## Method 0: One-shot bootstrap

Single curl command that detects your distro, installs Docker + nvidia-container-toolkit, downloads model weights, and brings the stack up. Idempotent — safe to re-run.

> **NVIDIA pre-Blackwell GPUs (RTX 20/30/40-series, GTX 10xx, V100/A100/T4/L4/H100): read this first.**
> The published `atlas-llama` CUDA image is compiled for compute capability
> `120;121` (Blackwell — RTX 50xx, B100, GB10) **only**. On older NVIDIA GPUs
> llama-server will fail at startup with
> `no kernel image is available for execution on the device`.
> Rebuild the inference image once for your GPU's architecture:
>
> ```bash
> # find your arch (drop the dot: 8.6 -> 86)
> nvidia-smi --query-gpu=compute_cap --format=csv,noheader
> docker compose build --build-arg CUDA_ARCH=86 llama-server   # example: RTX 30xx
> docker compose up -d --no-deps llama-server
> ```
> Full arch table: [CUDA Compute Capability](#cuda-compute-capability-dockerfilev31).

```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

Or, from a checkout:
```bash
./scripts/atlas-bootstrap.sh
```

**Supported distributions:**

| Family | Distros |
|---|---|
| Debian (apt-get) | Ubuntu 20.04+, Debian 11+ |
| RHEL (dnf) | RHEL 9+, Rocky 9+, AlmaLinux 9+, CentOS Stream 9+, Oracle Linux 9+ |
| Fedora (dnf) | Fedora 38+ |

Other distros with `ID_LIKE` matching one of the above (e.g. Linux Mint, Pop!_OS) are accepted with a warning. Distros not in this list — Arch, openSUSE, Alpine, NixOS — aren't tested and the script will refuse to run on them.

The bootstrap works around EPEL, nouveau driver conflicts, the missing-libnvidia-ml.so.1 case (RHEL minimal installs), and the "user added to docker group but current shell doesn't see it yet" race.

**Model selection:** `.env.example` ships with no model selected. When the bootstrap creates `.env` and `ATLAS_MODEL_FILE` is empty, it writes the registry's default recommended model into `.env` (logged as it happens) so the one-shot flow completes without a wizard. Change the selection any time by editing `.env` or running `atlas init`. An existing non-empty selection is respected.

<a id="cpu-only"></a>
**CPU-only / no-GPU hosts (Preview — smoke-test only).** ATLAS boots without a
GPU via the Vulkan image's lavapipe CPU rasterizer, but inference is very
slow; use this to verify the stack works, not for real coding sessions.

1. **The bootstrap refuses no-GPU hosts unless you opt in:**
   `ATLAS_BOOTSTRAP_SKIP_GPU=1 ./scripts/atlas-bootstrap.sh`
   It layers `docker-compose.vulkan.yml` (+ `docker-compose.cpu.yml` when
   `/dev/dri` is absent), writes the model selection and
   `ATLAS_BACKEND=vulkan|cpu` into `.env` itself, and skips the ASA build.
2. **Do not run `atlas init` on a GPU-less host** — the wizard intentionally
   refuses (exit 1) rather than write a `.env` it can't size. The bootstrap
   handles model selection; change models later via `atlas model install`.

Manual equivalent:

```bash
cp .env.example .env    # set ATLAS_MODEL_FILE / ATLAS_MODEL_NAME
atlas model install Qwen3.5-9B-Q6_K
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml -f docker-compose.cpu.yml up -d
atlas doctor            # gpu check WARNS ("CPU-only mode — very slow"); warns exit 0
```

**Firewall:** the Compose stack publishes every service on `127.0.0.1` only, so local use needs no firewall change and the bootstrap leaves firewalld alone by default. Set `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` to open the service ports (8090, 8099, 8070, 30820) for deployments that rebind services to a routable interface.

**Run modes — both work:**

```bash
# Run as your normal user; sudo elevates as needed (Docker install, etc).
# Install ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash

# Run via sudo. SUDO_USER is detected, install still ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | sudo bash

# Real root login (no sudo) — install owned by root. Only do this if there's
# no human user on the box (CI runner, container, etc).
```

**Cautious-install variants** (same script; for anyone who'd rather not
pipe a moving `main` script into bash):

```bash
# Pinned to a release: fetch the script AT the tag and install that tag.
# The checkout is pinned to the (SSH-signed) tag and ATLAS_IMAGE_TAG is
# pinned to the matching cosign-signed images.
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/v3.1.3/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.3 bash

# Review before running: download, read, then execute the same bytes.
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

**Configuration env vars:**

| Flag | Effect |
|---|---|
| `ATLAS_BOOTSTRAP_SKIP_DOCKER=1` | Don't install Docker (already managed) |
| `ATLAS_BOOTSTRAP_SKIP_GPU=1` | Skip the GPU runtime install (NVIDIA toolkit or ROCm setup). |
| `ATLAS_BOOTSTRAP_SKIP_MODELS=1` | Don't download model weights |
| `ATLAS_BOOTSTRAP_SKIP_COMPOSE=1` | Don't run `docker compose up` |
| `ATLAS_BOOTSTRAP_SKIP_ASA=1` | Skip the ASA steering-vector build (default: built ~5 min after services come up; skipped automatically when no GPU is available) |
| `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` | Open the service ports in firewalld (default: off — services bind loopback) |
| `ATLAS_BOOTSTRAP_NO_SUDO=1` | Fail instead of attempting sudo |
| `ATLAS_BOOTSTRAP_REF=vX.Y.Z` | Pin the install to a git tag/sha instead of tracking `main`; a `vX.Y.Z` value also pins `ATLAS_IMAGE_TAG` to the matching images |
| `ATLAS_INSTALL_DIR=/path` | Where to clone (default `/opt/atlas` — see below) |
| `ATLAS_REPO_URL=https://...` | Alternate repo URL |
| `ATLAS_GO_VERSION=1.26.6` | Go toolchain version installed for the TUI build (the TUI needs 1.26.6+; older installed toolchains auto-fetch it) |

**Why `/opt/atlas`?** It's the standard FHS prefix for system-wide third-party software, survives `$HOME` cleanup, and lets multiple users on the same box share one install. If you'd rather it land in your home dir:

```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh \
  | ATLAS_INSTALL_DIR=$HOME/atlas bash
```

When complete, prints a green "ATLAS ready" banner with quick-start commands. Total time on a fresh VM with a fast connection: ~10-30 minutes (model download dominates).

If you'd rather do each step manually, use Method 1 below.

---

## Prerequisites (All Methods)

| Requirement | Details |
|-------------|---------|
| **GPU** | 16 GB+ VRAM. NVIDIA (CUDA, Supported — the published image targets Blackwell; older cards need a one-time [local rebuild](#cuda-compute-capability-dockerfilev31)); AMD (ROCm, Community-tested); Apple Silicon (Metal, macOS hybrid, Supported — see [SETUP_MACOS.md](SETUP_MACOS.md)); Vulkan (Preview) is the cross-vendor fallback; Intel Arc (SYCL) is Roadmap. See [§ Supported GPUs](#supported-gpus). |
| **GPU drivers** | NVIDIA: proprietary drivers (`nvidia-smi` should show your GPU). AMD: `amdgpu-dkms` kernel driver (`/dev/kfd` must exist; `rocm-smi` should show your GPU). |
| **Python 3.9+** | With pip |
| **curl** | For downloading model weights |
| **Model weights** | A registry or BYO GGUF that fits the host. `atlas init` recommends one and writes the selection to `.env`. |

### Verify GPU

**NVIDIA:**

```bash
nvidia-smi
# Should show your GPU with driver version and VRAM
# If this fails, install NVIDIA proprietary drivers first
```

**AMD:**

```bash
rocm-smi --showproductname --showmeminfo vram
# Should show your GPU model and total VRAM
# If rocm-smi is missing or /dev/kfd doesn't exist, install ROCm:
#   RHEL 9: sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
#           sudo amdgpu-install --usecase=dkms,rocm
#   Ubuntu: Follow https://rocm.docs.amd.com/projects/install-on-linux/
# Then REBOOT.
```

**Autodetect** — let `atlas tier` autodetect across vendors and tell you what it found:

```bash
pip install -e .
atlas tier              # prints detected GPU, tier classification, recommended settings
atlas tier --json       # machine-readable (used by atlas init wizard)
```

---

## Method 1: Docker Compose (Recommended)

This is the most heavily exercised deployment method: CI validates the compose files and drives the full control plane deterministically (fake inference), and releases are smoke-tested under Compose on real hardware. Real GPU inference behavior is validated on the cards listed in the hardware table below, not in GitHub-hosted CI.

### Additional Prerequisites

**NVIDIA (CUDA):**
- **Docker** with [nvidia-container-toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html), **or Podman** with the same toolkit
- ~20 GB disk space (model weights + container images)

**AMD (ROCm):**
- **Docker** alone — ROCm doesn't need a separate container runtime; passthrough via `--device=/dev/kfd --device=/dev/dri` is enough
- Your user must be in the `video` and `render` groups: `sudo usermod -aG video,render $USER` (then re-login)
- ~22 GB disk space (ROCm image is ~2 GB larger than the CUDA equivalent)
- 30-60 min for the first `up`: the ROCm llama-server image is compiled on your machine rather than pulled (see [AMD ROCm — what's different](#amd-rocm--whats-different))

### Setup

```bash
# 1. Clone
git clone https://github.com/inferstep/ATLAS.git
cd ATLAS

# 2. Install the ATLAS CLI (puts `atlas` in ~/.local/bin)
pip install --user -e .

# Make sure ~/.local/bin is on your PATH so `atlas` resolves:
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *)
   echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
   source ~/.bashrc
;; esac

# 3. Select/install a model and write model-aware runtime sizing
atlas init

# 4. Install Go 1.26.6+ — required for the TUI client (atlas tui) and
#    optional for the proxy (proxy builds automatically on first run if Go
#    is present; otherwise it runs in Docker with file access limited to
#    ATLAS_PROJECT_DIR). Quickest path:
mkdir -p /tmp/go-install && cd /tmp/go-install
curl -LO https://go.dev/dl/go1.26.6.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.6.linux-amd64.tar.gz
echo 'export PATH="/usr/local/go/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
cd -

# Then build the TUI:
cd tui && go build -o ~/.local/bin/atlas-tui . && cd ..

# 5. Review the environment generated by `atlas init`
#    ATLAS_MODEL_FILE and ATLAS_MODEL_NAME identify this installation's
#    selected model; they are intentionally not project-wide defaults.
${EDITOR:-vi} .env

# 6. Start all services (first run builds container images — this takes several minutes)
#    NVIDIA hosts (default):
docker compose up -d                                                  # or: podman-compose up -d
#    AMD ROCm hosts:
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
#    `atlas init` writes a marker comment into .env telling you which to use.

# 7. Verify everything is healthy (wait for all services to show "healthy")
docker compose ps

# 8. Start coding (from your project directory)
cd /path/to/your/project
atlas
```

#### AMD ROCm — what's different

The ROCm path is identical to NVIDIA *except* for these four points:

1. **Bring up with both compose files** (or let `atlas init` do it for you):
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
   ```
   The override switches the llama-server image to the ROCm build, swaps the NVIDIA driver request for `/dev/kfd` + `/dev/dri` passthrough, and forces `ATLAS_BACKEND=rocm` so the entrypoint takes the HIP-tuning branch.

2. **The llama-server image is built on your machine, not pulled.** GHCR carries prebuilt CUDA and Vulkan llama images; there is no published `atlas-llama-rocm`, because CI has no AMD GPU to test one on (ROCm is Community-tested — see [SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md)). The override declares `pull_policy: build`, so `docker compose pull` skips llama-server and the `up` above compiles it from `inference/Dockerfile.rocm`: 30-60 min the first time, seconds afterwards from the layer cache. Every other ATLAS service still pulls its prebuilt image as usual. To build it ahead of time:
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
   ```

3. **No `nvidia-container-toolkit`** — ROCm doesn't need a separate container runtime, just kernel-level device access. Confirm your user is in the right groups:
   ```bash
   id -nG | tr ' ' '\n' | grep -E '^(render|video)$'
   # Should print both. If not:
   sudo usermod -aG video,render $USER
   # Then log out + back in (or: newgrp render)
   ```

4. **GPU compute target.** The default `Dockerfile.rocm` build is a "fat" image covering RDNA3 (7000 series), RDNA2 (6000 series), and CDNA2 (MI200) — `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`. For a smaller image targeted at your specific GPU, set `ATLAS_GFX_TARGET` before building:
   ```bash
   # Example: only build for RX 7900 XT/XTX
   ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
   ```
   See [LLVM AMDGPU processor table](https://llvm.org/docs/AMDGPUUsage.html) for the gfx target of your card.

For "I have an unsupported GPU but ROCm sort-of works on it" cases (older Vega, RDNA1), see [TROUBLESHOOTING.md § AMD GPU not detected](TROUBLESHOOTING.md) for the `ATLAS_HSA_OVERRIDE_GFX_VERSION` workaround.

#### Vulkan — cross-vendor fallback

When the native vendor backend isn't packaged for your hardware (Intel Arc, Snapdragon Adreno, older AMD without ROCm 6.x), Vulkan is the fallback. One Dockerfile covers AMD (Mesa RADV), Intel (Mesa ANV), NVIDIA (nvidia-container-toolkit), Apple (MoltenVK via macOS Docker), Snapdragon (Adreno), and CPU (Mesa lavapipe).

Tradeoff: typically 20–40% slower than tuned native backends. Use it when CUDA/ROCm isn't an option, or to smoke-test whether ATLAS boots on unusual hardware.

```bash
# Option A — let atlas init pick it for you
# (the wizard offers Vulkan when your GPU vendor's native backend isn't packaged,
#  or run with --backend vulkan to force it regardless of vendor):
atlas init --backend vulkan
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d

# Option B — already-installed deployment, just switch the override file:
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d
# (the entrypoint dispatches on ATLAS_BACKEND, which the compose overlay
#  sets to vulkan; .env's value is ignored when the overlay is in play)
```

What's different from CUDA/ROCm:

1. **No vendor-specific kernel driver requirement.** Vulkan ICDs live inside the image (`mesa-vulkan-drivers` covers AMD/Intel/CPU; NVIDIA's ICD comes from the nvidia-container-toolkit mount).
2. **`/dev/dri` passthrough only** — no `/dev/kfd`, no `--gpus all` (unless you're routing through the NVIDIA toolkit, in which case both still work).
3. **Per-GPU selection via `ATLAS_VK_DEVICE_SELECT`** instead of `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES`. Format is Mesa-standard: `"vendorID:deviceID"` (hex) or a substring of the device name. `GGML_VK_VISIBLE_DEVICES` (numeric index) also works.
4. **`atlas doctor`** runs a `_check_vulkan_via_docker` probe — but only when `ATLAS_BACKEND=vulkan` is set (otherwise it skips to keep CUDA/ROCm runs fast).

If you hit `vulkaninfo` showing only the `llvmpipe` CPU device when you expected a GPU, the kernel-side device passthrough failed — verify `/dev/dri/renderD*` exists on the host and your user is in the `video` + `render` groups (same as the ROCm requirement above).

<a id="arm64"></a>
#### arm64 hosts (#115)

ATLAS targets two CPU architectures: `x86_64` (default, all backends available) and `aarch64` (a subset of backends). Verify with `atlas doctor` — the `arch` check surfaces your architecture and which backends are available before the GPU check fires.

**Backend availability by architecture:**

| Backend | x86_64 | aarch64 | Notes |
|---|---|---|---|
| CUDA | yes (rockylinux9 base) | yes (sbsa or l4t base, build-arg swap) | DGX Spark = sbsa, Jetson = l4t |
| ROCm | yes | **no** | AMD has no arm64 ROCm release. Use Vulkan instead. |
| Vulkan | yes | yes (Mesa is multi-arch) | Universal fallback for all arm64 GPUs |
| CPU (lavapipe) | yes | yes | Slow but always works |

**Targeted arm64 devices:**

- **NVIDIA DGX Spark** (Grace-Blackwell GB10) — CUDA via sbsa base image, compute cap 12.0/12.1
- **NVIDIA Jetson Orin / AGX / Nano** — CUDA via l4t base image, compute cap 8.7
- **Apple Silicon (M1/M2/M3/M4)** — Vulkan via MoltenVK in Docker Desktop (slow path); the shipped fast path is the native hybrid Metal install — see [SETUP_MACOS.md](SETUP_MACOS.md)
- **Snapdragon X Elite** (Windows on ARM laptops) — Vulkan via the Adreno driver
- **Raspberry Pi 5** — Vulkan via Mesa V3D driver, expect CPU-tier performance
- **Ampere Altra / AWS Graviton workstations** — Vulkan via lavapipe (CPU fallback, since no consumer arm64 dGPU yet)

**Building the Vulkan image for arm64:**

```bash
# Multi-arch build that produces a single image manifest covering both archs:
docker buildx build --platform linux/amd64,linux/arm64 \
  -t atlas-llama-server:vulkan \
  -f inference/Dockerfile.vulkan inference/
```

**Building the CUDA image for arm64** (DGX Spark example):

```bash
# Swap to the sbsa-capable ubuntu base, build with --platform linux/arm64:
docker buildx build --platform linux/arm64 \
  --build-arg BUILDER_IMAGE=nvidia/cuda:12.9.0-devel-ubuntu22.04 \
  --build-arg RUNTIME_IMAGE=nvidia/cuda:12.9.0-runtime-ubuntu22.04 \
  -t atlas-llama-server:cuda-arm64 \
  -f inference/Dockerfile.v31 inference/
```

For Jetson, swap to `nvcr.io/nvidia/l4t-jetpack:r36.3.0` in both build args (l4t ships JetPack + CUDA + cuDNN as one image).

**Known gaps (#115 tracks these):**

- No prebuilt arm64 images on GHCR yet — arm64 users must build locally with the recipes above. Prebuilt multi-arch images will land once at least one arm64 device has been validated end-to-end.
- Bootstrap installer (`scripts/atlas-bootstrap.sh`) hasn't been audited for arm64 paths.
- Hardware testing matrix is empty for all five target devices — early adopters with any of these please drop your `atlas doctor` output and `vulkaninfo --summary` on [#115](https://github.com/inferstep/ATLAS/issues/115).

### What Happens on First Run

1. Docker pulls 5 prebuilt container images from
   `ghcr.io/inferstep/atlas-{proxy,v3,lens,llama,sandbox}` (~3 min on a
   fast connection). To build from source instead (the dev path), run
   `docker compose build` before the `up` step — see "Image source"
   below.
2. llama-server loads the 7GB model into GPU VRAM (~1-2 min)
3. All services start health checks
4. Once all 5 services (llama-server, geometric-lens, v3-service, sandbox, atlas-proxy) report healthy, `atlas` connects and launches the Bubbletea TUI

Subsequent `docker compose up -d` starts are fast (seconds) since images are cached.

### Image source: prebuilt vs from-source

`docker-compose.yml` declares both `image:` (GHCR) and `build:` (local
Dockerfile) for every service. Compose's default behavior:

| Command | What it does |
|---------|--------------|
| `docker compose up -d`            | Pull `image:` if not in local cache, else reuse local |
| `docker compose pull`             | Force pull latest tag from GHCR (overwrite local cache) |
| `docker compose build`            | Build from `Dockerfile` (overrides GHCR image) |
| `docker compose up -d --build`    | Always rebuild from source then start |

**Tag pinning.** The tag defaults to `latest`. To pin to a specific
version (recommended for production), set `ATLAS_IMAGE_TAG` in `.env`:

```env
ATLAS_IMAGE_TAG=3.1.3       # semver tag from a git release
ATLAS_IMAGE_TAG=sha-abc1234  # exact commit
ATLAS_IMAGE_TAG=dev          # bleeding edge from dev branch
```

Available tags are listed at <https://github.com/inferstep/ATLAS/pkgs/container/atlas-proxy>
(swap `atlas-proxy` for the other service names: `atlas-v3`,
`atlas-lens`, `atlas-llama`, `atlas-sandbox`).

Edge cases: `compose pull` fails with `unauthorized` on a package still
private to GHCR — authenticate with a `read:packages` token or build from
source instead. `compose pull` also overwrites a locally-built image sharing
the same tag; while iterating on a service, skip the pull or set
`ATLAS_IMAGE_TAG=dev-local` so local and registry images live under
different tags. To pull a fork's images, set `ATLAS_GHCR_OWNER=<your-username>`
in `.env`.

### Verify Installation

The fastest way is **`atlas doctor`** — checks the host environment (GPU
runtime, model and lens artifacts), the docker stack (containers, health
endpoints, auth, state), and a live model inference, printing each
result as it completes and returning exit 0 (healthy) / 1 (failures).
The exact number of checks varies with backend, stack state, and flags.
This is also what `atlas-bootstrap.sh` runs at the end of install.

```bash
atlas doctor              # full check (~5–10s)
atlas doctor --quick      # skip the e2e model inference (~2s)
atlas doctor --json       # machine output, for scripts/CI (buffered, one JSON document)
atlas doctor -v           # verbose: show detail for each check
```

The checks:

| Group | Check | What it confirms |
|---|---|---|
| Host | docker | daemon reachable |
| Host | compose | docker compose v2 installed |
| Host | arch | CPU architecture (`x86_64` / `aarch64`) and which backends are available on it (#115) — always runs, before the GPU check |
| Host | gpu | vendor-aware GPU runtime: NVIDIA (nvidia-container-toolkit runs nvidia-smi inside Docker) or AMD (`/dev/kfd` passthrough); warns when no GPU is detected |
| Host | vulkan | Vulkan ICDs visible inside Docker — only when `ATLAS_BACKEND=vulkan` |
| Host | metal-native | native llama-server binary present and executable — only when `ATLAS_BACKEND=metal` (macOS hybrid) |
| Host | model_file | The `ATLAS_MODEL_FILE` selected in `.env` exists and is > 100 MB |
| Host | lens_weights | `cost_field.pt` + G(x) artifacts present |
| Host | asa_steering | `ast_edit_steering.gguf` present (BiasBusters #4 — warn-not-fail; ATLAS works without it, just unsteered structural_edit-vs-edit_file bias) |
| Host | tier_match | `.env` model selection matches host hardware (warn on overshoot — OOM risk — pass on match or undershoot) |
| Host | tier_constraints | host CPU/RAM/disk meets the recommended tier minimums (catches "16 GB GPU but 8 GB RAM" mismatches) |
| Stack | container/llama-server, geometric-lens, v3-service, sandbox, atlas-proxy | all 5 running and healthy |
| Stack | health/llama, lens, v3, sandbox, proxy | all 5 `/health` endpoints return ok |
| Stack | internal_auth | internal service auth: token file present with tight permissions, and live enforcement probed both ways (wrong token → 401, valid token accepted); warns when auth is disabled (no `secrets/service-token`) |
| Stack | status_dimensions | informational: the seven lens/ASA status dimensions from the proxy `/v1/calibration/status` (the same source the TUI badge reads); never fails the run |
| Stack | image_skew | all 5 `atlas-*` images on the same tag |
| End-to-end | e2e_smoke | live `/v1/chat/completions` round-trip to llama-server (`--quick` to skip) |

The `vulkan` and `metal-native` rows are conditional on the configured backend; the health, `internal_auth`, and `status_dimensions` rows run only when at least one container is up; `e2e_smoke` is skipped by `--quick`. The remaining checks always run.

If you'd rather check by hand:

```bash
# Hit each health endpoint
curl -s http://localhost:8080/health | python3 -m json.tool   # llama-server
curl -s http://localhost:8099/health | python3 -m json.tool   # geometric-lens
curl -s http://localhost:8070/health | python3 -m json.tool   # v3-service
curl -s http://localhost:30820/health | python3 -m json.tool  # sandbox
curl -s http://localhost:8090/health | python3 -m json.tool   # atlas-proxy

# Functional test: full install diagnostic (services, artifacts, e2e smoke)
atlas doctor
```

All health endpoints should return `{"status": "ok"}` or `{"status": "healthy"}`.

> **Note:** Plain `atlas` in an interactive terminal launches the Bubbletea TUI for the full agent loop (tool calls, V3 pipeline, file read/write). The TUI needs a real terminal — piped stdin/stdout prints a pointer to `atlas doctor` and exits.

### Stopping

```bash
docker compose down          # Stop all services (preserves images)
docker compose down --rmi all  # Stop and remove images (next start rebuilds)
```

### Viewing Logs

```bash
docker compose logs -f llama-server    # Follow llama-server logs
docker compose logs -f geometric-lens  # Follow Lens logs
docker compose logs -f v3-service      # Follow V3 pipeline logs
docker compose logs -f atlas-proxy     # Follow proxy logs
docker compose logs -f sandbox         # Follow sandbox logs
docker compose logs --tail 50          # Last 50 lines from all services
```

### Updating

```bash
git pull
docker compose down
docker compose pull          # grab fresh :latest images from GHCR
docker compose up -d
```

### Uninstalling

```bash
# Stop and remove the containers, network, and named volumes
docker compose down -v

# Remove the published images
docker images "ghcr.io/*/atlas-*" -q | xargs -r docker rmi

# Remove the CLI and TUI binaries
pip uninstall atlas
rm -f ~/.local/bin/atlas-tui
rm -rf ~/.cache/atlas-tui          # TUI session history

# The repo checkout, .env, and downloaded models live wherever you put
# them — delete the checkout and its models/ directory to reclaim the
# disk (models are the multi-GB part).
```

K3s installs use `scripts/uninstall.sh` instead, which tears down the
manifests and (optionally) the K3s node itself.

---

## Method 2: Bare Metal

Run all services as local processes without containers. Useful for development or systems where Docker isn't available.

### Additional Prerequisites

| Requirement | Details |
|-------------|---------|
| **Go 1.26.6+** | For building atlas-proxy and the atlas-tui client (older Go toolchains auto-fetch it) |
| **llama.cpp** | Built from source with CUDA (see [llama.cpp build instructions](https://github.com/ggml-org/llama.cpp?tab=readme-ov-file#build)) |
| **Node.js 20+** | Required by sandbox for JavaScript/TypeScript execution |
| **Rust** | Required by sandbox for Rust execution |

### Build

```bash
# 1. Clone and install Python CLI
git clone https://github.com/inferstep/ATLAS.git
cd ATLAS
pip install -e .

# 2. Select and install a registry model (or place a BYO GGUF in models/)
atlas model recommend
atlas model install <registry-name>

# 3. Build the proxy
cd proxy
go build -o ~/.local/bin/atlas-proxy-v2 .
cd ..

# 4. Install geometric-lens Python dependencies
pip install -r geometric-lens/requirements.txt

# 5. Install V3 service PyTorch (CPU only)
pip install torch --index-url https://download.pytorch.org/whl/cpu

# 6. Install sandbox dependencies
pip install -r sandbox/requirements-runtime.txt -r sandbox/requirements-verify.txt
```

### Start Services

Start each service in a separate terminal (or use `&` and redirect to log files):

```bash
# Terminal 1: llama-server (GPU)
llama-server \
  --model "models/$ATLAS_MODEL_FILE" \
  --host 0.0.0.0 --port 8080 \
  --ctx-size 32768 --n-gpu-layers 99 --no-mmap \
  --embeddings --pooling none --flash-attn on --fit off
# --pooling none: the lens's per-step path needs per-token vectors, and it
# pools and normalizes them itself for the whole-text path (the compose
# default, ATLAS_EMBED_POOLING=none).

# Terminal 2: Geometric Lens (required: the proxy refuses requests while it
# cannot score)
cd geometric-lens
LLAMA_URL=http://localhost:8080 \
LLAMA_EMBED_URL=http://localhost:8080 \
python -m uvicorn main:app --host 0.0.0.0 --port 8099

# Terminal 3: V3 Pipeline
cd v3-service
ATLAS_INFERENCE_URL=http://localhost:8080 \
ATLAS_LENS_URL=http://localhost:8099 \
ATLAS_SANDBOX_URL=http://localhost:8020 \
python main.py

# Terminal 4: Sandbox
cd sandbox
python executor_server.py

# Terminal 5: atlas-proxy
ATLAS_PROXY_PORT=8090 \
ATLAS_INFERENCE_URL=http://localhost:8080 \
ATLAS_LLAMA_URL=http://localhost:8080 \
ATLAS_LENS_URL=http://localhost:8099 \
ATLAS_SANDBOX_URL=http://localhost:8020 \
ATLAS_V3_URL=http://localhost:8070 \
ATLAS_MODEL_NAME="${ATLAS_MODEL_NAME:-local-model}" \
atlas-proxy-v2
```

> **Note:** The sandbox listens on port **8020** in bare-metal mode (no Docker port remapping). The proxy's `ATLAS_SANDBOX_URL` must use port 8020, not 30820.

### Launch the TUI

The `atlas` command is the Python package's console entrypoint, installed by `pip install -e .` in the Build step — no separate launcher script is needed. With the services above running:

```bash
cd /path/to/your/project
atlas    # Checks atlas-proxy is reachable, then launches the TUI
```

`atlas` builds the `atlas-tui` binary from `tui/` automatically if it is missing or older than the checkout (requires Go 1.26.6+ on PATH), and verifies the proxy on localhost:8090 before handing over to the TUI.

---

## Method 3: K3s

Kubernetes deployment with GPU scheduling, health probes, and resource limits. Preview — templates are validated and rendered in CI; there is no automated live-cluster test.

### Additional Prerequisites

| Requirement | Details |
|-------------|---------|
| **K3s** | Single-node or multi-node cluster |
| **NVIDIA GPU Operator** or **device plugin** | GPU must be visible as `nvidia.com/gpu` resource |
| **Helm** | For GPU Operator installation |
| **Podman or Docker** | For building container images |

### Automated Install

The install script handles the complete setup — K3s installation, GPU Operator, container builds, and deployment:

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf: model paths, GPU layers, context size, NodePorts

# 2. Run the installer (requires root)
sudo scripts/install.sh
```

The installer will:
1. Check prerequisites (NVIDIA drivers, GPU VRAM, system RAM)
2. Install K3s if not already running
3. Install NVIDIA GPU Operator via Helm (if GPU not visible to cluster)
4. Build container images and import to K3s containerd
5. Generate manifests from `atlas.conf` via envsubst
6. Deploy to the `atlas` namespace
7. Wait for all services to be healthy

### Manual Deploy

If K3s is already running with GPU support:

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf

# 2. Build and import images
scripts/build-containers.sh

# 3. Generate manifests from atlas.conf
scripts/generate-manifests.sh

# 4. Deploy
kubectl apply -n atlas -f manifests/

# 5. Verify
scripts/verify-install.sh
```

### K3s-Specific Configuration

K3s uses `atlas.conf` (not `.env`) for configuration. The HTTP contracts and pipeline behavior are identical to Docker Compose; only deployment plumbing differs:

| Setting | Docker Compose | K3s |
|---------|---------------|-----|
| Config file | `.env` | `atlas.conf` |
| Service exposure | Host ports (`8090`, `8080`, `8099`, `8070`, `30820`) | NodePorts (`30080`, `32735`, `31144`, `30070`, `30820`) |
| Project workspace | Bind mount (`ATLAS_PROJECT_DIR` → `/workspace`) | `hostPath` (`ATLAS_PROJECTS_DIR` → `/workspace` on every Pod that needs it) |
| Model files | Bind mount (`ATLAS_MODELS_DIR` → `/models:ro`) | `hostPath` on the GPU node (`ATLAS_MODELS_DIR`, `Directory`, ro) |
| Stateful storage | Named volume (`v3-telemetry`) | PVC `lens-projects` (sized by `ATLAS_PVC_PROJECTS_SIZE`; mounted, but unused since the project indexer was removed) |
| GPU allocation | `deploy.resources.reservations.devices` (nvidia) | `resources.limits.nvidia.com/gpu: 1` (requires GPU Operator or device plugin) |
| Sandbox toolchain caches | `tmpfs` mounts per language | `emptyDir` with `sizeLimit` per language (universal pattern, same set) |

Model + runtime parameters (`ATLAS_MAIN_MODEL`, `ATLAS_CONTEXT_LENGTH`, `ATLAS_PARALLEL_SLOTS`, `ATLAS_FLASH_ATTENTION`, KV cache quantization, `--embeddings` for the lens scoring path) all read from the same env vars in both modes — see `atlas.conf.example` and `.env.example`.

See [CONFIGURATION.md](CONFIGURATION.md) for the full `atlas.conf` reference.

### Verify K3s Deployment

```bash
# Check pods
kubectl get pods -n atlas

# Check GPU allocation
kubectl describe nodes | grep nvidia.com/gpu

# Run verification suite
scripts/verify-install.sh
```

> **Note:** Docker Compose is the most heavily-exercised deployment method (CI runs against it; every release is smoke-tested under Compose). K3s manifests are generated from `templates/*.yaml.tmpl` at deploy time via `scripts/generate-manifests.sh` (or `install.sh`'s `process_templates` step). Templates consume the model selected in `atlas.conf`; benchmark numbers in CHANGELOG record their own frozen model/configuration.

---

## Hardware Sizing

ATLAS classifies GPUs into 5 tiers and recommends a registry model + context
size + parallel-slots configuration per tier. These are current registry
recommendations, not hard-coded runtime requirements. Run `atlas tier` to see
which tier your hardware lands in and the exact `.env` values to use.

| Tier | VRAM | Recommended model | Context | Slots | Example GPUs |
|------|------|-------------------|--------:|------:|--------------|
| **cpu** | n/a | [CPU-only install](#cpu-only) — Preview, smoke-test only | n/a | n/a | (no GPU) |
| **small** | 8–12 GB | Qwen3.5 7B Q4_K_M (4.4 GB) | 8K | 1 | RTX 3060/4060 8GB, T4 |
| **medium** | 12–20 GB | Qwen3.5 9B Q6_K (6.9 GB) | 32K | 1 | RTX 4060/5060 Ti 16GB, 3080 Ti, 4070 Ti Super |
| **large** | 20–32 GB | Qwen3.5 14B Q5_K_M (10.5 GB) | 32K | 2 | RTX 3090, 4090, 5090 24GB |
| **xlarge** | 32 GB+ | Qwen3.5 32B Q5_K_M (23 GB) | 64K | 2 | RTX 5090 32GB, A6000, A100, H100 |

```bash
atlas tier              # classify this host + show recommendations
atlas tier list         # show all 5 tier definitions
atlas tier fit          # size the runtime for the CONFIGURED model + GPU
atlas tier --json       # machine output (for scripts)
atlas tier --raw        # just the probe (no classification)
```

The tier table gives per-VRAM-band starting points; **`atlas tier fit`**
refines them for the *specific* model you run — it reads the GGUF's KV
geometry plus your GPU's VRAM and solves for the largest context that
stays fully on-GPU (`atlas tier fit --write` applies the result to
`.env`). Run it whenever you change `ATLAS_MODEL_FILE` or the GPU. See
[CLI.md § atlas tier fit](CLI.md#atlas-tier-fit), and
[TROUBLESHOOTING.md § What fits on my GPU?](TROUBLESHOOTING.md#what-fits-on-my-gpu)
for pre-download sizing guidance.

The medium tier is the ATLAS development target — `atlas-bootstrap.sh`
defaults to its model+context settings. For other tiers, run
**`atlas init`** (the first-run wizard) after the bootstrap
completes. It probes hardware via `atlas tier`, picks the right model
from the registry, downloads it with SHA verification, and rewrites
`.env`. Re-run with `atlas init --reconfigure` whenever your hardware
or model registry default changes; after a wizard run, `atlas tier fit
--write` tightens the wizard's tier-level defaults to the chosen model.

| Resource | Minimum | Recommended | Notes |
|----------|---------|-------------|-------|
| GPU VRAM | 8 GB | 16 GB | See tier table above |
| System RAM | 14 GB | 16 GB+ | PyTorch runtime + container overhead |
| Disk | 15 GB | 25 GB | Model (4.4–23 GB depending on tier) + container images (5–8 GB) + working space |
| CPU | 4 cores | 8+ cores | V3 pipeline is CPU-intensive during repair phases |

### Supported GPUs

Any GPU with 8 GB+ VRAM and a llama.cpp-supported backend:

| Vendor | Backend | Status | Build path | Tested cards |
|---|---|---|---|---|
| NVIDIA (Blackwell — RTX 50xx, B100, GB10) | CUDA | Supported (published image) | `inference/Dockerfile.v31` | RTX 5060 Ti 16GB (primary dev) |
| NVIDIA (pre-Blackwell — RTX 20xx–40xx, GTX 10xx, V100/A100/H100/T4/L4) | CUDA | Preview — one-time [local rebuild required](#cuda-compute-capability-dockerfilev31) | `inference/Dockerfile.v31` + `--build-arg CUDA_ARCH=<cc>` | — (upstream llama.cpp supports these; no maintainer validation on ATLAS) |
| AMD | ROCm / HIP | Community-tested | `inference/Dockerfile.rocm` | RX 7900 XTX (community smoke-test, [GH #26](https://github.com/inferstep/ATLAS/issues/26)) |
| Apple Silicon | Metal | Supported (macOS hybrid: native llama-server + Docker, [#32](https://github.com/inferstep/ATLAS/issues/32)) | `scripts/atlas-setup-macos.sh` + `docker-compose.macos.yml` | M2 Pro 32GB (verified); M3/M4 (target) |
| Any (cross-vendor fallback) | Vulkan | Preview | `inference/Dockerfile.vulkan` | lavapipe (CPU ICD) smoke-tested; no real-GPU validation yet |
| Intel Arc | SYCL | Roadmap — Intel Arc uses Vulkan today | TBD | Arc A770 16GB (target) |

`atlas tier` auto-detects across vendors and picks the largest-VRAM GPU. Override with `ATLAS_GPU_VENDOR=amd` or `ATLAS_GPU_INDEX=1` if you have multiple GPUs and want a specific one.

#### CUDA Compute Capability (Dockerfile.v31)

`inference/Dockerfile.v31` compiles llama.cpp for a specific CUDA compute capability. The default — and what the published `atlas-llama` image on GHCR is built with — is `120;121` (Blackwell: RTX 50xx, B100, GB10) **only**. The published image contains no kernels for earlier GPUs, and its embedded PTX cannot be JIT-compiled downward, so on RTX 20/30/40-series, GTX 10xx, and pre-Blackwell datacenter cards (V100/A100/H100/T4/L4) llama-server fails at startup with `no kernel image is available for execution on the device`. You must rebuild the inference image once for your architecture. (A local build with the wrong arch value fails earlier, with `nvcc fatal: unsupported gpu architecture`.)

Find your GPU's arch, then rebuild with `--build-arg CUDA_ARCH=<value>`:

```bash
# Your GPU's compute capability (drop the dot: 8.9 -> 89)
nvidia-smi --query-gpu=compute_cap --format=csv,noheader

# Compose-native rebuild — only llama-server is rebuilt, the other
# services keep using the GHCR images (~30-75 min, one time):
docker compose build --build-arg CUDA_ARCH=89 llama-server
docker compose up -d --no-deps llama-server

# Or build the image directly:
podman build --build-arg CUDA_ARCH=89 -f inference/Dockerfile.v31 -t llama-server:local inference/

# Multiple archs (semicolon-separated) — build a fat binary for Ampere + Ada + Hopper
docker compose build --build-arg CUDA_ARCH="86;89;90" llama-server
```

Common values:

| Arch | Architecture | Cards |
|------|--------------|-------|
| `60`, `61` | Pascal | GTX 10xx, Tesla P4/P40 |
| `70` | Volta | V100 |
| `75` | Turing | RTX 20xx, T4 |
| `80`, `86` | Ampere | A100, RTX 30xx |
| `89` | Ada Lovelace | RTX 40xx, L4 |
| `90` | Hopper | H100 |
| `100`, `120`, `121` | Blackwell | B100, RTX 50xx |

#### AMD GPU Targets (Dockerfile.rocm)

`inference/Dockerfile.rocm` compiles llama.cpp's HIP backend for one or more `gfx` targets. The default is a fat build covering the most common consumer + datacenter AMD GPUs: `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`. Each additional target adds ~150 MB to the binary.

Override at build time with `--build-arg GFX_TARGET=<value>` (or via `ATLAS_GFX_TARGET` env var, which the compose override forwards):

```bash
# Single target — RX 7900 XT/XTX only (smaller image)
ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server

# Two targets for RDNA3 + RDNA2 mixed-fleet
docker build --build-arg GFX_TARGET="gfx1100;gfx1030" -f inference/Dockerfile.rocm -t atlas-llama-rocm:custom inference/
```

Common values:

| Target | Architecture | Cards |
|--------|--------------|-------|
| `gfx1100` | RDNA3 (Navi 31) | RX 7900 XT, 7900 XTX, 7900 GRE |
| `gfx1101` | RDNA3 (Navi 32) | RX 7800 XT, 7700 XT |
| `gfx1102` | RDNA3 (Navi 33) | RX 7600, 7600 XT |
| `gfx1030` | RDNA2 (Navi 21) | RX 6800, 6800 XT, 6900 XT, 6950 XT |
| `gfx1031` | RDNA2 (Navi 22) | RX 6700 XT, 6750 XT |
| `gfx1032` | RDNA2 (Navi 23) | RX 6600, 6600 XT, 6650 XT |
| `gfx90a` | CDNA2 | MI210, MI250, MI250X |
| `gfx942` | CDNA3 | MI300X |
| `gfx900` | Vega | Vega 56/64 (may need HSA override — see TROUBLESHOOTING.md) |
| `gfx1200` | RDNA4 (Navi 44) | RX 9070 |
| `gfx1201` | RDNA4 (Navi 48) | RX 9070 XT |

> **RDNA4 (gfx1200/gfx1201) users:** set `ATLAS_ROCM_TAG=7.2.3-complete` — the default ROCm 6.2 base image does not include gfx1200/gfx1201 compiler support. ROCm 7.0+ supports these targets natively; do not set `ATLAS_HSA_OVERRIDE_GFX_VERSION`. See [TROUBLESHOOTING.md § RDNA4](TROUBLESHOOTING.md) for details.

Your GPU's gfx target: `rocminfo | grep -i gfx | head -1` (or look it up in the [LLVM AMDGPU processor table](https://llvm.org/docs/AMDGPUUsage.html)).

---

## Geometric Lens Weights (Required)

ATLAS needs the Geometric Lens for every request ([ADR 0011](adr/0011-the-lens-is-required.md)). The lens needs C(x) and G(x) weights for the served model. Without them, the proxy refuses each request with HTTP 503 `dependency_down` and says why, and `atlas doctor` fails. A lens that has weights but no calibration yet is accepted: it scores, and its threshold interventions wait for calibration.

Models listed with lens artifacts in [SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md) get them from `atlas model install-artifacts`. Pre-trained weights and training data are also available on HuggingFace:

**[ATLAS Dataset on HuggingFace](https://huggingface.co/datasets/itigges22/ATLAS)** — includes embeddings, training data, and weight files.

Place weight files in `geometric-lens/geometric_lens/models/` (or mount via `ATLAS_LENS_MODELS` in Docker Compose). The service loads them automatically on startup.

For a model with no weights, build them from your own benchmark data. `atlas bench` runs the V3 stages directly, not through the proxy, so it works before the lens has weights. The whole loop is CLI-driven:

```bash
atlas bench --run-id mymodel_lens --tasks 200    # generate + self-label candidates
atlas lens build --force --from-results benchmark/results/mymodel_lens/v3_lcb/per_task
```

`atlas lens build` trains both lens halves, calibrates the thresholds, and writes a `provenance.json` manifest into the activated bundle. See [CLI.md § atlas lens](CLI.md#atlas-lens).

### Bringing your own llama-server

To run inference yourself — a host-native build, a remote box, or llama.cpp in
router mode — point the four consuming services at it and stub the bundled
container. Create `docker-compose.override.yml`:

```yaml
services:
  llama-server:                      # stub the bundled one out
    command: ["sleep", "infinity"]
    entrypoint: []
    healthcheck:
      test: ["CMD", "true"]
    deploy: {}
  geometric-lens:
    environment:
      LLAMA_URL: http://host.docker.internal:8080
  v3-service:
    environment:
      LLAMA_URL: http://host.docker.internal:8080
  atlas-proxy:
    environment:
      ATLAS_LLAMA_URL: http://host.docker.internal:8080
```

`host.docker.internal` resolves on Linux as well as macOS: the compose file
maps it via `extra_hosts: host.docker.internal:host-gateway` on every service
that needs it. You do not need the docker0 gateway address (`172.17.0.1`)
directly, though it is what that mapping resolves to.

**Router mode.** `llama-server --models-preset` rejects any request without a
`model` field (`model name is missing from the request`). Set `ATLAS_MODEL_NAME`
in `.env` to the served model's name — `atlas doctor`'s smoke test sends it, and
single-model servers ignore it.

**Before the first `docker compose up`,** make sure the secrets directory
exists — the bind mount fails without it:

```bash
mkdir -p secrets     # or set ATLAS_SECRETS_DIR
```

`atlas init` creates it for you; only a manual compose-first flow needs this.

### Bringing your own model

If you want to swap in a non-default GGUF, the `atlas lens` subcommand wraps the probe + train pipeline so you don't have to learn the underlying scripts:

```bash
# 1. Drop your GGUF in models/ and update .env to point at it, restart llama-server.

# 2. Probe whether the existing artifacts can score it (cheap, no training):
atlas lens check
# Reports: compat (artifacts work) | needs-build (different dim) | incompatible

# 3. If 'needs-build', train fresh artifacts at the model's native embedding dim:
atlas lens build --samples path/to/labeled.json
# samples format: [{"text": str, "label": 0|1}, ...] where 1 = passing code
# Canonical training set: huggingface.co/datasets/itigges22/ATLAS

# 4. Re-run check — should now report compat:
atlas lens check
```

Full reference: [CLI.md § atlas lens](CLI.md#atlas-lens).

---

## ASA Steering Vector (Auto-Built)

May 2026 BiasBusters #4. A residual-stream steering vector that biases
the model toward `structural_edit` over `edit_file` for whole-function /
class / element rewrites, applied **before** the grammar gate has a
chance to reject anything. Strictly optional — ATLAS continues to work
without it, just with an unsteered tool-selection bias.

`atlas-bootstrap.sh` builds it automatically after the services come
up. The pipeline is:

1. `build_cvector_prompts.py` turns the committed
   `geometric-lens/asa_calibration/contrast_pairs.jsonl` (1000 pairs)
   into positive / negative prompt files.
2. The bootstrap stops `llama-server` briefly, runs
   `llama-cvector-generator` as a one-shot container with `--method mean
   -ngl 99`, writes `models/ast_edit_steering.gguf` plus a
   `models/ast_edit_steering.gguf.model` sidecar marker recording which
   model the vector was built against, then restarts `llama-server`.
3. `inference/entrypoint-v3.1.sh` sees the file on the next start,
   checks that the `.model` sidecar marker matches the selected model,
   and appends `--control-vector-scaled
   /models/ast_edit_steering.gguf:0.5` to the `llama-server` command
   line. A vector whose marker is missing or names a different model
   stays **disabled** (the startup banner reports why) — vectors are
   residual-space artifacts tied to one model.

Total wall time on a 16GB GPU: ~5 minutes. Build runs on the same
hardware the model lives on; the resulting vector is model-specific
(do not move an `ast_edit_steering.gguf` built against
one model's artifacts to a host running a different base model).

> The `ast_edit_steering` filename is intentional and stable: the registry
> SHA-pins it and the `.model` marker sits beside it, so it keeps the name
> even though the tool it steers is now called `structural_edit`.

**Override behavior** (set in `.env` if you want to tune):

| Env var | Default | Effect |
|---|---|---|
| `ATLAS_CONTROL_VECTOR` | `/models/ast_edit_steering.gguf` | Override path |
| `ATLAS_CONTROL_VECTOR_SCALE` | `0.5` | Conservative. Bump to 1.0–1.5 if the bias is too subtle, drop toward 0.2 if non-tool tasks degrade. |
| `ATLAS_CONTROL_VECTOR_LAYER_RANGE` | (all layers) | Pass two integers, e.g. `"24 30"`, to scope to a layer band. Narrower = safer but weaker. |
| `ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED` | `0` | Set to `1` to apply a vector even when its `.model` sidecar marker is missing or doesn't match the selected model. Only for vectors you built yourself and know match. |

**If the local build fails** (e.g. cvector-generator missing in an
older `atlas-llama` image, GPU OOM, network hiccup pulling the
runtime), the bootstrap falls back to downloading a prebuilt
`ast_edit_steering.gguf` from the
[ATLAS HuggingFace dataset](https://huggingface.co/datasets/itigges22/ATLAS).
If that also fails the install completes with a warning — `atlas
doctor` will flag the gap as `warn`, not `fail`.

To skip the build entirely, set `ATLAS_BOOTSTRAP_SKIP_ASA=1` before
running the installer.

To rebuild manually (re-curated pairs, different `--method`, different
base model), see
[`geometric-lens/asa_calibration/README.md`](../geometric-lens/asa_calibration/README.md).

---

## Next Steps

- [CLI.md](CLI.md) — How to use ATLAS once it's running
- [CONFIGURATION.md](CONFIGURATION.md) — All environment variables and tuning options
- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — Common issues and solutions
- [ARCHITECTURE.md](ARCHITECTURE.md) — How the system works internally
