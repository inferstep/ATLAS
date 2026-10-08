# ATLAS Troubleshooting Guide

Common issues and solutions, organized by service.

---

## Quick Diagnostics

Start with `atlas doctor`, then use Compose status and logs to identify where
the problem is:

```bash
atlas doctor
docker compose ps
docker compose logs --tail 50
```

`atlas doctor` is the preferred first diagnostic because it checks the host,
configuration, and service health together. `docker compose ps` identifies
services that failed to start, and the logs provide the next level of detail.
Use a hardware-specific check only when the first results point to the
inference backend:

| Backend | Diagnostic command or check |
|---|---|
| NVIDIA CUDA | `nvidia-smi` |
| AMD ROCm | Run `rocm-smi` and verify `/dev/kfd` exists. |
| Apple Silicon / Metal | Run `atlas doctor`; if the native server is not listening, start `./scripts/atlas-llama-macos.sh` and inspect its foreground launcher output as described in [SETUP_MACOS.md](SETUP_MACOS.md#troubleshooting). |
| Vulkan | Verify `/dev/dri` exists, then run `docker compose -f docker-compose.yml -f docker-compose.vulkan.yml exec llama-server vulkaninfo --summary`. |
| CPU-only | Confirm the `docker-compose.vulkan.yml` and `docker-compose.cpu.yml` overlays are active. `atlas doctor` should warn and exit successfully rather than fail solely because no GPU exists. |

For the per-service health-check curls, see [SETUP.md § Verify Installation](SETUP.md#verify-installation). The atlas-proxy health endpoint is the most useful for triage — it reports the status of all upstream services:
```json
{
  "status": "ok",
  "inference": true,
  "lens": true,
  "lens_ready": true,
  "sandbox": true,
  "port": "8090",
  "capabilities": ["demo_raw_completion_v1"]
}
```

If any field is `false`, that service is the problem. `status` flips to `"degraded"` whenever any of `inference`, `lens`, `lens_ready`, or `sandbox` is false. `lens` says whether the lens answers at all. `lens_ready` says whether it can score, by the same check the proxy applies to every request: when it is `false`, `lens_reason` says why, and every request is refused until it is `true` (see [Requests Refused: the Lens Cannot Score](#requests-refused-the-lens-cannot-score)).

---

## Find your error

Exact error strings and symptoms, mapped to their entries.

| You're seeing | Go to |
|---|---|
| `no kernel image is available for execution on the device` — NVIDIA GPU | [`no kernel image is available for execution on the device` (CUDA)](#no-kernel-image-is-available-for-execution-on-the-device-cuda) |
| `no kernel image is available for execution on the device` — AMD GPU | [AMD GPU is "unsupported" by ROCm…](#amd-gpu-is-unsupported-by-rocm-but-you-want-to-try-anyway-no-kernel-image-on-rocm) |
| `invalid device function` or `nvcc fatal: unsupported gpu architecture` | [`no kernel image…` (CUDA)](#no-kernel-image-is-available-for-execution-on-the-device-cuda) |
| `libnvidia-ml.so.1: cannot open shared object file` | [libnvidia-ml.so.1 entry](#libnvidia-mlso1-cannot-open-shared-object-file) |
| Container can't see the GPU; model runs on CPU | [GPU Not Detected in Container](#gpu-not-detected-in-container) |
| `/dev/kfd: no such file or directory` | [AMD GPU not detected (ROCm)](#amd-gpu-not-detected-rocm) |
| `Permission denied` on `/dev/kfd` | [AMD GPU detected but Docker can't reach it](#amd-gpu-detected-but-docker-cant-reach-it) |
| `error: AMDGPU target 'gfx1201' is not supported` | [RDNA4 — ROCm 7.x required](#rdna4-rx-9070--9070-xt-gfx1200--gfx1201--rocm-7x-required) |
| ROCm image pull times out / rate-limited | [ROCm container can't pull `rocm/rocm-terminal`](#rocm-container-cant-pull-rocmrocm-terminal) |
| `docker compose build` fails with CUDA errors | [First Build Fails (CUDA Not Found)](#first-build-fails-cuda-not-found) |
| `RPC failed; curl 56` / `early EOF` / `fetch-pack: invalid index-pack output` | [llama.cpp Clone Times Out](#llamacpp-clone-times-out) |
| `error loading model: unknown (model) architecture '…'` | [Rebuilding llama.cpp](#rebuilding-llamacpp-new-model-architecture-or-patch-drift) |
| `error: patch failed:` / `patch does not apply` | [Rebuilding llama.cpp](#rebuilding-llamacpp-new-model-architecture-or-patch-drift) |
| `open /workspace/....atlas.tmp: permission denied` on every write | [Proxy Can't Write the Workspace](#proxy-cant-write-the-workspace-atlastmp-permission-denied) |
| Agent insists a file "does not exist" that's right there; `read_file` and `run_command` disagree | [Agent Says Files Don't Exist That Are Right There](#agent-says-files-dont-exist-that-are-right-there-workspace-mount-split) |
| Permission denied on mounted volumes / model files (Fedora/RHEL) | [SELinux Blocking Container Access](#selinux-blocking-container-access-fedorarhel) |
| `"sandbox": false` in proxy health (manual container setup) | [Sandbox Unreachable](#sandbox-unreachable) |
| `"sandbox": false` in proxy health (Compose stack) | [Sandbox Unreachable (health check)](#sandbox-unreachable-health-check) |
| `address already in use` on startup | [Port Conflicts](#port-conflicts) |
| CUDA allocation error after "fitting params to device memory" | [Model + KV cache don't fit on the GPU](#model--kv-cache-dont-fit-on-the-gpu-startup-fails-or-generation-is-5-slow) |
| Generation at ~2 tok/s, llama-server burning CPU cores | [Model + KV cache don't fit on the GPU](#model--kv-cache-dont-fit-on-the-gpu-startup-fails-or-generation-is-5-slow) / [Slow Generation](#slow-generation-2-toks) |
| Sizing a model before download | [What fits on my GPU?](#what-fits-on-my-gpu) |
| `failed to load model` at startup | [Model File Not Found](#model-file-not-found) |
| llama-server crashes / OOMKilled, VRAM near 100% | [Out of VRAM](#out-of-vram) |
| Model outputs `<think>` tags or prose instead of JSON tool calls | [Grammar Not Enforced](#grammar-not-enforced-model-outputs-thinking-blocks) |
| Gemma keeps emitting `done` without doing anything | [Grammar Not Enforced](#grammar-not-enforced-model-outputs-thinking-blocks) |
| `unexpected end of JSON` on tool calls | [Context Window Too Small](#context-window-too-small) / [Truncation Errors](#truncation-errors-write_file-fails-repeatedly) |
| No tool calls, no V3 — requests pass straight through | [Agent Loop Not Activating](#agent-loop-not-activating) |
| Writes/edits never trigger V3 stages | [V3 Pipeline Not Firing on Feature Files](#v3-pipeline-not-firing-on-feature-files) |
| `Your output was truncated — the content is too long for a single tool call` | [Truncation Errors](#truncation-errors-write_file-fails-repeatedly) |
| `Tool call was truncated (output too long for context window)` | [Truncation Errors](#truncation-errors-write_file-fails-repeatedly) |
| ~30 s idle between a tool result and the next action | [Long Pause Between Tool Result and Next Action](#long-pause-between-tool-result-and-next-action) |
| Agent keeps editing after the fix was verified | [Model Keeps Editing After V3 Already Confirmed the Fix](#model-keeps-editing-after-v3-already-confirmed-the-fix) |
| First tool call reads a file that doesn't exist here | [Model Hallucinates Filenames From Previous Sessions](#model-hallucinates-filenames-from-previous-sessions) |
| `ModuleNotFoundError` during V3 verification only | [Multi-File Project: Sandbox `ModuleNotFoundError`](#multi-file-project-sandbox-modulenotfounderror) |
| `_curses.error: addwstr() returned ERR` | [Curses Bottom-Row `addwstr() returned ERR`](#curses-bottom-row-addwstr-returned-err) |
| ~5-minute pause writing HTML/CSS/JSON files | [V3 Hangs for Several Minutes on Non-Python Files](#v3-hangs-for-several-minutes-on-non-python-files) |
| Terse follow-up ("ok", "yes") gets chat instead of action | [V3 Pipeline Doesn't Fire on "Fix It Again" Prompts](#v3-pipeline-doesnt-fire-on-fix-it-again-prompts) |
| `file not read yet — use read_file first before editing` | [File Not Read Before Editing](#file-not-read-before-editing) |
| `file modified since last read — read it again before editing` | [File Modified Externally](#file-modified-externally) |
| `You have full project context in the system prompt. Do not read more files.` | [Exploration Budget Warning](#exploration-budget-warning) |
| "ATLAS needs the geometric lens for every request" / run ends `lens_unavailable` / `"lens_ready": false` | [Requests Refused: the Lens Cannot Score](#requests-refused-the-lens-cannot-score) |
| "No gx_thresholds.json — Lens scores are uncalibrated" | [Lens Uncalibrated](#lens-uncalibrated) |
| Scores plausible but off-scale; `fingerprint_ok: false` / `drifted: true` | [Embedding-convention drift](#scores-look-plausible-but-are-wildly-off-scale-embedding-convention-drift) |
| "embedding extraction failed" in lens logs | [Embedding Extraction Fails](#embedding-extraction-fails) |
| Sandbox returns `"error_type": "Timeout"` | [Code Execution Timeout](#code-execution-timeout) |
| Sandbox errors on a specific language | [Language Not Supported](#language-not-supported) |
| `LIMITED MODE: running N tasks` below `--tasks` | [Bench runs fewer tasks than requested](#bench-runs-fewer-tasks-than-requested-limited-mode-running-n-tasks-with-n-below---tasks) |
| System sluggish, services OOMKilled | [High RAM Usage](#high-ram-usage) |

---

## Docker / Podman Issues

### GPU Not Detected in Container

**Symptom:** llama-server container starts but model loads on CPU (very slow, ~2 tok/s). `nvidia-smi` shows the GPU from the host but the container can't see it.

**Fix:** Install NVIDIA Container Toolkit:

```bash
# RHEL/Fedora
sudo dnf install nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=podman
sudo systemctl restart podman

# Ubuntu/Debian
sudo apt install nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

Verify GPU is visible inside containers:
```bash
# Docker
docker run --rm --gpus all nvidia/cuda:12.0.0-base-ubuntu22.04 nvidia-smi

# Podman
podman run --rm --device nvidia.com/gpu=all nvidia/cuda:12.0.0-base-ubuntu22.04 nvidia-smi
```

### `libnvidia-ml.so.1: cannot open shared object file`

**Symptom:** During `docker compose up`, llama-server fails with:

```
nvidia-container-cli: initialization error: load library failed:
libnvidia-ml.so.1: cannot open shared object file: no such file or directory
```

**What it means:** the host has the NVIDIA *kernel module* (so `nvidia-smi` works) but the *userspace driver libraries* aren't where the container toolkit expects. On RHEL/Rocky/Alma minimal installs the `nvidia-driver-cuda-libs` package isn't pulled in by default; on Debian/Ubuntu the issue is usually a stale `ldconfig` cache after a driver upgrade.

**Fix sequence** — try in order, stop when `docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi` works:

1. **Refresh ldconfig + restart docker:**
   ```bash
   sudo ldconfig
   sudo systemctl restart docker
   ```

2. **RHEL 9 — add CUDA repo + install open-dkms module** (verified working on RHEL 9.7 with RTX 5060 Ti):
   ```bash
   # Add NVIDIA's CUDA repo
   sudo dnf config-manager --add-repo \
     https://developer.download.nvidia.com/compute/cuda/repos/rhel9/x86_64/cuda-rhel9.repo

   # Enable CodeReady Builder (provides dkms / kernel-devel)
   sudo subscription-manager repos --enable=codeready-builder-for-rhel-9-x86_64-rpms

   # Make sure EPEL is present
   sudo dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm

   # Install the open driver module (REQUIRED for Blackwell — RTX 50xx)
   sudo dnf module install -y nvidia-driver:open-dkms

   sudo ldconfig && sudo systemctl restart docker
   ```

   **Rocky/Alma/CentOS Stream 9** — same as above, but replace the `subscription-manager` line with:
   ```bash
   sudo dnf config-manager --set-enabled crb
   ```

   > Note: the `nvidia-driver-cuda-libs` package only exists once the NVIDIA CUDA repo is added. RHEL 9's stock `BaseOS`/`AppStream` repos do not ship NVIDIA packages. The `nvidia-driver:open-dkms` module is **required** for Blackwell GPUs (RTX 5060/70/80/90); older GPUs accept either open or proprietary.

3. **Ubuntu/Debian — install matching userspace libs:**
   ```bash
   DRV_MAJOR=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | cut -d. -f1)
   sudo apt install -y libnvidia-compute-${DRV_MAJOR}
   sudo ldconfig && sudo systemctl restart docker
   ```

4. **Generate a CDI spec:**
   ```bash
   sudo mkdir -p /etc/cdi
   sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
   docker run --rm --device=nvidia.com/gpu=all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
   ```

The `atlas-bootstrap.sh` script now runs steps 1, 2 (auto-detects RHEL/Rocky/Alma vs subscription path), and 4 automatically. Step 3 is auto-handled on Debian/Ubuntu via `libnvidia-compute-NN` matched to the running driver version.

### AMD GPU not detected (ROCm)

**Symptom:** `atlas tier` says "no GPU detected" on a host that clearly has an AMD GPU, OR `docker compose up` fails with `/dev/kfd: no such file or directory`.

**What it means:** the `amdgpu` kernel driver isn't loaded with compute support (the `kfd` — Kernel Fusion Driver — submodule). Display-only loads of `amdgpu` don't expose `/dev/kfd`.

**Fix sequence:**

1. **Verify the driver is loaded and `/dev/kfd` exists:**
   ```bash
   lsmod | grep amdgpu       # should print amdgpu + amdkfd
   ls -l /dev/kfd            # should print a character-device entry
   ls -l /dev/dri/render*    # should print one or more render nodes
   ```

2. **Install ROCm + kernel driver (if /dev/kfd is missing):**
   - **RHEL 9 / Rocky / Alma:**
     ```bash
     sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
     sudo amdgpu-install --usecase=dkms,rocm
     sudo reboot   # required — the kernel module needs a fresh boot
     ```
   - **Ubuntu/Debian:** follow [the official AMD install guide](https://rocm.docs.amd.com/projects/install-on-linux/) for your distro. The typical sequence is `amdgpu-install --usecase=dkms,rocm` after adding the AMDGPU repo.

3. **After reboot, confirm `rocm-smi` sees the GPU:**
   ```bash
   rocm-smi --showproductname --showmeminfo vram
   ```

### AMD GPU detected but Docker can't reach it

**Symptom:** `atlas doctor` reports "AMD GPU detected but Docker can't reach `/dev/kfd`" or the ROCm container fails with `Permission denied` on `/dev/kfd`.

**What it means:** the user running Docker isn't in the `render` and/or `video` groups. ROCm uses those groups to gate access to `/dev/kfd` and `/dev/dri/render*`.

**Fix:**

```bash
# 1. Confirm which groups you're currently in
id -nG | tr ' ' '\n' | grep -E '^(render|video)$'
# Expect both. If either is missing:

# 2. Create the groups if they don't exist (rare; default on most distros)
sudo groupadd -f render
sudo groupadd -f video

# 3. Add your user to both
sudo usermod -aG video,render $USER

# 4. Re-login (or use newgrp for the current shell)
newgrp render
newgrp video

# 5. Re-verify, then re-run `atlas doctor`
id -nG | grep -E 'render.*video|video.*render'
atlas doctor
```

### AMD GPU is "unsupported" by ROCm but you want to try anyway (`no kernel image` on ROCm)

**Symptom:** `rocm-smi` reports your GPU, but `rocminfo` doesn't, or HIP kernels fail with "no kernel image is available for execution on the device." (For the same error on NVIDIA, see [the CUDA entry](#no-kernel-image-is-available-for-execution-on-the-device-cuda).)

**What it means:** llama.cpp's HIP kernels were compiled for `gfx` targets that don't include your GPU. ROCm has a long-standing pattern of dropping older consumer GPUs from official support while still letting them work with the right override.

**Fix:** force a compatible gfx version at runtime via `ATLAS_HSA_OVERRIDE_GFX_VERSION`. Common overrides (for the canonical card→gfx table, see [SETUP.md § AMD GPU Targets](SETUP.md#amd-gpu-targets-dockerfilerocm)):

| Your GPU | Set `ATLAS_HSA_OVERRIDE_GFX_VERSION=` |
|---|---|
| RDNA1 (RX 5700 XT / 5500 XT) | `10.3.0` (makes it look like RDNA2 / gfx1030) |
| Vega 56/64 (gfx900) | `9.0.0` (usually already supported, override rarely needed) |
| Polaris (RX 580/590, gfx803) | `8.0.3` (deep override; mileage varies) |

Set the var in `.env` so it propagates through the compose override into the container env:

```bash
echo "ATLAS_HSA_OVERRIDE_GFX_VERSION=10.3.0" >> .env
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d --force-recreate llama-server
```

If this works for you on a previously-unsupported card, please leave a note on [GH #26](https://github.com/inferstep/ATLAS/issues/26) — community-tested overrides feed into the next release's docs.

### RDNA4 (RX 9070 / 9070 XT, gfx1200 / gfx1201) — ROCm 7.x required

**Symptom:** Build fails during `docker compose ... build llama-server` with errors like `error: AMDGPU target 'gfx1201' is not supported`, or the container starts but immediately exits with a HIP initialization error.

**What it means:** The default ROCm base image (`rocm/dev-ubuntu-22.04:6.2-complete`) predates RDNA4. The gfx1200 and gfx1201 compiler targets were added in ROCm 7.0 — see the [ROCm compatibility matrix](https://rocm.docs.amd.com/en/latest/compatibility/compatibility-matrix.html) for the full supported hardware list.

**Fix:** Set `ATLAS_ROCM_TAG` to a ROCm 7.x tag before building:

```env
# Add to your .env
ATLAS_ROCM_TAG=7.2.3-complete
ATLAS_GFX_TARGET=gfx1201   # gfx1200 for RX 9070, gfx1201 for RX 9070 XT
```

Then rebuild and bring up the stack:

```bash
docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
```

**Important: do NOT set `ATLAS_HSA_OVERRIDE_GFX_VERSION` for gfx1200/gfx1201.** ROCm 7.0+ supports these targets natively; overriding the GFX version inside Docker causes a mismatch between the compiled kernels and the runtime target, which results in crashes. Leave `ATLAS_HSA_OVERRIDE_GFX_VERSION` unset (the default).

> Tested on AMD Radeon AI PRO R9700 (gfx1201) with ROCm 7.2, `ATLAS_ROCM_TAG=7.2.3-complete`. The hidden-states patch applies cleanly to the pinned llama.cpp SHA. Inference runs correctly across text generation and embedding generation without any additional flags.

### ROCm container can't pull `rocm/rocm-terminal`

**Symptom:** `atlas doctor` ROCm check times out at the image pull, or `docker compose -f ... -f docker-compose.rocm.yml build llama-server` stalls fetching its `rocm/dev-ubuntu-*` base. (`docker compose pull` never touches llama-server on this overlay — it is a local build, `pull_policy: build`.)

**What it means:** ROCm images are large (~2 GB) and Docker Hub rate-limits anonymous pulls.

**Fix:** authenticate (a free Docker Hub account allows higher rate limits) and pre-pull the doctor's check image, or retry during off-peak hours:

```bash
docker login
docker pull rocm/rocm-terminal:latest
```

The doctor's ROCm check always uses `rocm/rocm-terminal:latest`. `ATLAS_ROCM_TAG` pins the *build base* image for the llama-server ROCm build (`rocm/dev-ubuntu-*`), not the doctor's check image:

```bash
ATLAS_ROCM_TAG=6.2-complete docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
```

### First Build Fails (CUDA Not Found)

**Symptom:** `docker compose build` fails with CUDA-related errors during llama-server compilation.

**Fix:** The llama-server Dockerfile builds llama.cpp inside a `nvidia/cuda:12.9.0-devel` base image (digest-pinned in `inference/Dockerfile.v31`), so CUDA headers are available during build without host GPU access. Common causes of build failure:
1. Insufficient disk space (~5GB needed for build artifacts)
2. Network issues downloading the CUDA base image or cloning llama.cpp
3. Podman rootless builds may fail with permission issues — try `podman-compose build` with `--podman-build-args="--format docker"`

### llama.cpp Clone Times Out

**Symptom:** Build hangs in the `llama-server builder 3/3` stage and eventually fails with:

```
error: RPC failed; curl 56 OpenSSL SSL_read: Connection timed out, errno 110
fatal: early EOF
fatal: fetch-pack: invalid index-pack output
```

**Cause:** The full llama.cpp git history is large (~1 GB) and the fetch is sensitive to flaky/slow connections. A momentary stall causes the SSL read to time out and the whole transfer to abort.

**Fix:** `inference/Dockerfile.v31` uses `git init` + a `--depth 1` fetch of the single pinned revision (`LLAMA_CPP_REV`), which avoids the ~1 GB full-history transfer, with `http.postBuffer=524288000` and `http.lowSpeedLimit/Time` to fail-fast on dead connections. If the issue recurs:

1. Retry the build — transient network blips happen, especially on residential connections.
2. If retries keep failing, pre-fetch the pinned revision on the host and adjust the Dockerfile to COPY it. Quick recipe:
   ```bash
   REV=$(grep -m1 'ARG LLAMA_CPP_REV=' inference/Dockerfile.v31 | cut -d= -f2)
   git init /tmp/llama.cpp && cd /tmp/llama.cpp
   git remote add origin https://github.com/ggml-org/llama.cpp
   git fetch --depth 1 origin "$REV" && git checkout FETCH_HEAD
   # then edit Dockerfile.v31 to COPY from /tmp/llama.cpp instead of fetching
   ```
3. Prebuilt llama-server images on GHCR skip this step entirely — pull instead of building.

### Rebuilding llama.cpp (new model architecture, or patch drift)

Developer-maintenance task. Two triggers land here:

- **A dropped-in model fails to load** with `error loading model: unknown (model) architecture 'gemma4'` — the pinned llama.cpp predates that architecture.
- **A build fails** with `error: patch failed: tools/server/server-context.cpp:NN` / `patch does not apply` — upstream drifted past the pinned SHA.

The `atlas-llama` image pins llama.cpp via `LLAMA_CPP_REV` in all four Dockerfiles (`Dockerfile`, `Dockerfile.v31`, `Dockerfile.rocm`, `Dockerfile.vulkan`), and `Dockerfile.v31`, `Dockerfile.rocm`, and `Dockerfile.vulkan` — the three the compose files build from — apply `inference/patches/expose-hidden-states.patch` (the per-layer `hidden_states` extension the Geometric Lens depends on) during the build. The plain `Dockerfile` pins the rev but does not apply the patch, so a server built from it lacks the lens plumbing. To learn a new architecture, move the pin to a llama.cpp SHA that includes it. Prebuilt GHCR images skip the local build; only rebuild when you need an architecture newer than the published image.

**Preserve the hidden-states patch — rebase it, don't delete it.** Removing the `git apply` step builds a server that has silently lost the lens plumbing (`/embedding` ignores the `layers:` parameter). Bump runbook:

1. **Verify the patch against the target SHA** (fast, no Docker):
   ```bash
   mkdir -p /tmp/llama-check && cd /tmp/llama-check
   git init -q llama.cpp && cd llama.cpp
   git remote add origin https://github.com/ggml-org/llama.cpp
   git fetch --depth 1 origin <NEW_SHA> && git checkout -q FETCH_HEAD
   git apply --check $REPO/inference/patches/expose-hidden-states.patch
   ```
   (Only this patch is `git apply`-ed. The spec-decode embeddings fix is a `sed` in the Dockerfiles, a no-op when its target line is absent.)
2. **If it applies cleanly:** bump `LLAMA_CPP_REV` in all four Dockerfiles to the new SHA. The CI smoke test verifies they agree.
3. **If it fails:** `git apply --reject …` to land the clean hunks, re-insert each `*.rej` hunk at its moved anchor (watch for upstream renames in surrounding code, e.g. `model` → `model_tgt`, and update the patch's added lines), then `git diff > $REPO/inference/patches/expose-hidden-states.patch`. Re-run step 1. Compile just the server target CPU-only to catch member/type errors before the long CUDA build: `cmake -B build-cpu -DGGML_CUDA=OFF && cmake --build build-cpu --target llama-server`.
4. Rebuild and bring up:
   ```bash
   docker compose build --build-arg LLAMA_CPP_REV=<sha> llama-server
   docker compose up -d llama-server --no-deps
   ```

Prefer regenerating the patch over pinning to an older SHA — pinning backward means missing upstream fixes.

After the rebuild loads the model, the Geometric Lens still needs retraining for the new model — see [CONFIGURATION.md § Adding your own model](CONFIGURATION.md#adding-your-own-model-drop-in--unregistered).

### Proxy Can't Write the Workspace (`.atlas.tmp: permission denied`)

**Symptom:** Every `write_file`/`edit_file` fails with `cannot write /workspace/...: open /workspace/....atlas.tmp: permission denied` (the agent then wanders looking for "a writable subdirectory").

**Cause:** The atlas-proxy image runs as a baked-in non-root user (uid 1001, `atlas`), but the host directory bind-mounted at `/workspace` (`ATLAS_PROJECT_DIR`) is owned by the operator's uid. Reads work (mode 755); every write is denied. Installs whose `.env` predates `ATLAS_PROXY_UID` hit this after pulling a hardened proxy image.

**Fix:** run the proxy as the invoking user, the same way the sandbox already does:

```bash
# add your ids to .env (atlas init --reconfigure also writes these now)
echo "ATLAS_PROXY_UID=$(id -u)" >> .env
echo "ATLAS_PROXY_GID=$(id -g)" >> .env
docker compose up -d --no-deps --force-recreate atlas-proxy
```

Verify: `docker exec atlas-atlas-proxy-1 touch /workspace/.write_test` succeeds (then remove the file). K3s deployments render the same ids into the proxy Pod's `securityContext` via `scripts/generate-manifests.sh`.

### Agent Says Files Don't Exist That Are Right There (workspace mount split)

**Symptom:** Agent sessions insist a file "does not exist" even though it's plainly in the project directory — `read_file` fails while `run_command` (`ls`, `cat`) sees the file fine, or vice versa. Sessions give up early ("It seems the file X does not exist"), writes never land in the project, and every `/health` endpoint is green.

**Cause:** The proxy and the sandbox bind-mount **different host directories** as `/workspace`. File tools (`read_file`/`write_file`/`edit_file`) are served by the proxy against *its* mount; `run_command` executes in the *sandbox's* mount. Compose resolves the mount source from `ATLAS_PROJECT_DIR` (default: the compose working directory) **per container at creation time** — so recreating one container from a different directory, or with a different `.env`, silently splits them. Nothing fails at startup; the agent just operates split-brained.

**Diagnose:** `atlas doctor` — the `workspace_mounts` check compares both mounts and fails with the two host paths when they differ. By hand:

```bash
docker inspect atlas-atlas-proxy-1 --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
docker inspect atlas-sandbox-1     --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
```

**Fix:** pin `ATLAS_PROJECT_DIR` in `.env` to your project directory, then recreate both containers together:

```bash
echo "ATLAS_PROJECT_DIR=/path/to/your/project" >> .env
docker compose up -d --force-recreate atlas-proxy sandbox
```

### SELinux Blocking Container Access (Fedora/RHEL)

**Symptom:** Containers can't read mounted volumes, permission denied on model files.

**Fix:**
```bash
# Allow container access to model directory
chcon -Rt svirt_sandbox_file_t ~/models/

# Or add :Z flag to volume mounts (Docker Compose handles this)
```

### Sandbox Unreachable

**Symptom:** Proxy health shows `"sandbox": false`. V3 build verification fails.

**Fix:** The sandbox is reached over `sandbox-net`, not `atlas` — it is deliberately the only service NOT on the `atlas` network, so executed code cannot reach the rest of the stack. The proxy, lens and v3-service are dual-homed on both. If running containers manually:
```bash
docker network create sandbox-net
# sandbox joins ONLY sandbox-net; proxy / lens / v3-service join it too,
# in addition to the atlas network.
```

### Port Conflicts

**Symptom:** `docker compose up` fails with "address already in use" on a port.

**Fix:** Check what's using the port and either stop it or change ATLAS ports in `.env`:
```bash
# Find what's using port 8080
lsof -i :8080

# Change port in .env
ATLAS_LLAMA_PORT=8081    # Different port for llama-server
```

All ports are configurable via `.env`. See [CONFIGURATION.md](CONFIGURATION.md).

---

## llama-server Issues

### `no kernel image is available for execution on the device` (CUDA)

**Applies to:** NVIDIA GPUs older than Blackwell — RTX 40xx (Ada), RTX 30xx
(Ampere), RTX 20xx / T4 (Turing), GTX 10xx (Pascal), V100/A100/H100/L4 —
running the prebuilt `ghcr.io/inferstep/atlas-llama` image. The sibling
errors `invalid device function` (runtime) and
`nvcc fatal: unsupported gpu architecture` (local build) have the same cause.
(For the same error on AMD, see [the ROCm entry](#amd-gpu-is-unsupported-by-rocm-but-you-want-to-try-anyway-no-kernel-image-on-rocm).)

**What it means:** the published CUDA image is compiled for compute
capability `120;121` (Blackwell only). The llama-server binary contains no
GPU kernels for earlier architectures, and its embedded PTX (`compute_121`)
cannot be JIT-compiled downward, so the first CUDA kernel launch fails.
This is an image/GPU mismatch, not a driver or VRAM problem.

**Check first:**
```bash
# Your GPU's compute capability (8.9 = Ada, 8.6 = Ampere, 7.5 = Turing, 12.0 = Blackwell)
nvidia-smi --query-gpu=name,compute_cap --format=csv
# What the image was built for (Blackwell-only image prints sm_120/sm_121)
docker run --rm --entrypoint bash ghcr.io/inferstep/atlas-llama:latest \
  -c 'grep -ao "sm_[0-9]*" /usr/local/bin/llama-server | sort -u'
```
If your compute capability is below 12.0 and the image only lists
`sm_120`/`sm_121`, this entry applies.

**Fix — rebuild the inference image for your architecture** (one-time,
~30-75 min; only llama-server is rebuilt, other services keep using GHCR
images). Drop the dot from your compute capability (`8.6` -> `86`):
```bash
docker compose build --build-arg CUDA_ARCH=86 llama-server
docker compose up -d --no-deps llama-server
```
Multiple GPUs / portability: pass a semicolon list, e.g.
`--build-arg CUDA_ARCH="75;86;89"`. Full arch table:
[SETUP.md § CUDA Compute Capability](SETUP.md#cuda-compute-capability-dockerfilev31).

**Verify:**
```bash
docker compose logs llama-server | tail -20   # model loads, no CUDA errors
curl -s localhost:8080/health                  # {"status":"ok"}
```

**Still failing:** confirm the container is actually running your rebuilt
image (`docker compose images llama-server` — a `docker compose pull` may
have overwritten it; pin `ATLAS_IMAGE_TAG` or re-run the build). Pascal
(`60`/`61`) and older are limited by upstream llama.cpp CUDA support — the
Vulkan image (`docker-compose.vulkan.yml`) is the fallback. Otherwise file
an issue with `nvidia-smi` output and the first 50 llama-server log lines.

### Model Loading on CPU Instead of GPU

**Symptom:** Generation at ~2 tok/s instead of ~50 tok/s. `nvidia-smi` doesn't show llama-server using the GPU.

**Fix:** Ensure `-ngl 99` (`--n-gpu-layers`) is set (offloads all layers to GPU). In Docker Compose the entrypoint sets it by default. For bare metal, check the command:
```bash
ps aux | grep llama-server | grep -e '-ngl' -e 'n-gpu-layers'
```

If using Docker, ensure the NVIDIA container runtime is configured (see GPU section above).

### Model + KV cache don't fit on the GPU (startup fails, or generation is 5× slow)

**Symptom (current entrypoint):** llama-server exits at startup with a CUDA
allocation error right after "fitting params to device memory".

**Symptom (older entrypoints without `--fit off`):** the server *starts* and
`nvidia-smi` shows the model loaded, but generation runs at a fraction of the
expected speed, the llama-server process burns several CPU cores
(`top` shows 400–800%), and its host RSS holds gigabytes of model weights —
llama.cpp's memory auto-fitter silently moved layers to the CPU.

**Cause:** the model's weights plus the KV cache (`ATLAS_CTX_SIZE` ×
`PARALLEL` slots × per-layer KV dims) plus the compute buffer
(~`ATLAS_UBATCH` × hidden-dim × 280 bytes) exceed VRAM. These budgets are
per-model — a config tuned for one model can overflow on another with
different KV geometry.

**Fix:** size the runtime for this model + GPU and recreate the container:
```bash
atlas tier fit --write
docker compose up -d llama-server --no-deps --force-recreate
```
`atlas tier fit` reads the GGUF header and your GPU's VRAM and solves for the
largest fully-on-GPU configuration (see [CLI.md § atlas tier fit](CLI.md#atlas-tier-fit)).
ATLAS runs llama-server with `--fit off` so a config that doesn't fit fails
loudly at startup instead of silently running partly on the CPU.

If `atlas tier fit` reports **DOES NOT FIT**, the model itself is too large
for the card — the output names the largest quant file size that *would* fit.
In order of preference:

1. **Use a smaller quant of the same model** (e.g. Q4_K_M instead of Q6_K —
   usually the best quality-per-GiB trade below 16 GB VRAM).
2. **Reduce parallel slots**: `atlas tier fit --slots 1 --write` frees the
   per-slot KV minimum (drops `/demo` split-pane and V3 parallel candidates,
   single-stream use still works).
3. **Pick a smaller model.** See the sizing table below.

### What fits on my GPU?

Approximate rule before you download anything: on the default 4 slots, a GGUF
fits comfortably when

```
file size  ≤  VRAM − ~4.5 GiB
```

(the ~4.5 GiB covers the minimum KV cache at 4 × 8k context, compute buffers,
and the ~1.9 GiB fixed CUDA overhead). With `--slots 1` the margin shrinks to
roughly `VRAM − 3 GiB`. Sliding-window models (Gemma-style) need less than
this; the rule is sized for full-attention models.

| VRAM | GGUF file size (4 slots) | GGUF file size (1 slot) | Typical models |
|------|--------------------------|--------------------------|----------------|
| 8 GB | ≤ ~3 GiB | ≤ ~4.5 GiB | 3–4B Q4–Q6, 7–8B Q2–Q3 |
| 12 GB | ≤ ~7 GiB | ≤ ~8.5 GiB | 7–9B Q4–Q6, 12B Q3–Q4 |
| 16 GB | ≤ ~11 GiB | ≤ ~12.5 GiB | 9B Q6–Q8, 12–14B Q4–Q6 |
| 24 GB | ≤ ~19 GiB | ≤ ~20.5 GiB | 14B Q8, 27–32B Q4 |

HuggingFace model pages list the file size per quant — check it against this
table before downloading. The table is a pre-download estimate only; once the
file is on disk, `atlas tier fit /path/to/model.gguf` is authoritative (it
reads the model's real KV geometry, which can swing the budget by gigabytes
in either direction), and `atlas onboard` prints the same fit automatically.

### Model File Not Found

**Symptom:** llama-server exits immediately with "failed to load model" or similar.

**Fix:** Check the model path:
```bash
# Docker Compose — model must be in ATLAS_MODELS_DIR (default: ./models/)
ls -la "models/$ATLAS_MODEL_FILE"

# Bare metal — check ATLAS_MODELS_DIR + ATLAS_MODEL_FILE
ls -la "$ATLAS_MODELS_DIR/$ATLAS_MODEL_FILE"
```

The filename must match the required `ATLAS_MODEL_FILE` selection in `.env`.

### Out of VRAM

**Symptom:** llama-server crashes or gets OOMKilled shortly after starting. `nvidia-smi` shows VRAM near 100%.

**Fix:** Ensure:
1. No other GPU processes are running (`nvidia-smi` — check for other CUDA processes)
2. You have 16GB+ VRAM
3. The runtime is sized for your model + GPU: `atlas tier fit --write` (don't raise `ATLAS_CTX_SIZE` past what it recommends)

```bash
# Kill other GPU processes if needed
nvidia-smi --query-compute-apps=pid --format=csv,noheader | xargs -I{} kill {}
```

### Grammar Not Enforced (Model Outputs Thinking Blocks)

**Symptom:** Model outputs `<think>` tags or raw text instead of JSON tool calls.

**Fix:** The proxy sets a `response_format` automatically inside the `/v1/agent` agent-loop handler. `ATLAS_GRAMMAR_MODE` picks the shape: the default `strict` sends `{"type":"json_object","schema":<full tool-call schema>}` so llama-server's GBNF sampler can only emit the tool_call/text/done union; `ATLAS_GRAMMAR_MODE=loose` sends `{"type":"json_object"}` only (valid JSON, shape not enforced) — the escape hatch for models that handle the schema-to-GBNF conversion poorly (Gemma-family models need `loose` — strict mode makes them done-spam). If you're hitting llama-server directly via `/v1/chat/completions` or `/v1/completions`, you have to include the parameter yourself:
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "local-model",
    "messages": [{"role":"user","content":"Say hi"}],
    "max_tokens": 50,
    "response_format": {"type": "json_object"}
  }'
```

If this returns raw text instead of JSON, your llama.cpp build doesn't support `response_format`. Rebuild from the latest source.

### Context Window Too Small

**Symptom:** Tool call arguments get truncated. Tool results carry "Tool call was truncated (output too long for context window)" / "Your output was truncated — the content is too long for a single tool call", or proxy logs show `truncated args detected for <tool> at turn N`.

**Fix:** Per-slot context (`ATLAS_CTX_SIZE` ÷ `ATLAS_PARALLEL_SLOTS`; compose default 131072 ÷ 4 = 32k per slot) may be too small for the task. `atlas tier fit` shows the largest budget your GPU supports. Check:
```bash
# Docker Compose
grep CTX_SIZE .env

# Bare metal
ps aux | grep llama-server | grep ctx-size
```

---

## Proxy Issues

### Agent Loop Not Activating

**Symptom:** Requests go directly to llama-server. No tool calls, no streaming status icons, no V3 pipeline.

**Cause:** You're hitting the wrong endpoint. The agent loop only runs on `POST /v1/agent`. `POST /v1/chat/completions` (and anything else under `/v1/`) is a transparent passthrough to llama-server — no tools, no V3, no streaming chat events.

**Fix:** Point your client at `POST http://localhost:8090/v1/agent`. The Bubbletea TUI (`atlas` / `atlas tui`) does this automatically. If you're writing a third-party client, see [docs/API.md](API.md) for the `/v1/agent` SSE event protocol. There is no longer an `ATLAS_AGENT_LOOP` env-var toggle — the split is endpoint-based, not config-based.

### V3 Pipeline Not Firing on Feature Files

**Symptom:** All `write_file` *or* `edit_file` calls are T1 (direct write). No V3 pipeline stages in output.

V3 fires when **all conditions** are met:
1. File has **10+ lines** of content (under 10 lines is always T1)
2. File has **2+ logic indicators** (function defs, control flow, API patterns) — **or** a recognized code/markup extension (`.py`, `.go`, `.js`, `.html`, …), which goes T2 at 10+ lines even with zero indicators
3. V3 service is reachable at `ATLAS_V3_URL`
4. For `edit_file` only: the resulting file warrants a whole-file re-run — cyclomatic complexity ≥ 8, or ≥ 80 lines when complexity can't be measured

Config, data, style, markdown, and shell files (`package.json`, `.yaml`, `.css`, `.md`, `.sh`, …) are always T1 regardless of size. The request tier is forwarded to V3 but doesn't gate activation — the file's own tier does.

Both `write_file` and `edit_file` route through V3.

**Diagnose:**
```bash
# Check V3 service health
curl -s http://localhost:8070/health

# Check proxy logs for tier classification + V3 activation
docker compose logs atlas-proxy | grep -E "write_file|edit_file|tier="
# Look for:
#   "tier=T2:medium" or higher in classifier output
#   "[edit_file] V3 pipeline activating for X (file_tier=2, req_tier=2)"
#   "[write_file] V3 pipeline activating for X"
# T1 means direct write — no V3.
```

If V3 is unreachable, the proxy logs `V3 failed: ...` and falls back to direct write without breaking the edit.

### Truncation Errors (write_file Fails Repeatedly)

**Symptom:** Repeated errors like "Your output was truncated — the content is too long for a single tool call."

**Cause:** The model is trying to write too much content in one call. The proxy detects truncated JSON and rejects the tool call.

The proxy rejects every `write_file` against an existing path — `write_file` is for creating files — and tells the model to use `edit_file` instead. The only exceptions: files of 5 lines or fewer, files that look corrupted on disk (prose preamble, stray markdown fences), and files this session wrote itself. After 3 consecutive failures the error loop breaker stops the agent and returns a summary.

**Fix:** Rephrase your request to ask for targeted changes rather than full file rewrites — "Add input validation to the login function" instead of "Rewrite auth.py".

The proxy distinguishes real truncation (args payload over 200 bytes) from a tool call sent with empty or missing `args` — the latter gets a per-tool hint like `read_file: no arguments provided. Call with {"path":"<file>"}` instead of the truncation remap. It also normalizes OpenAI-style (`arguments`), Anthropic-style (`parameters`), and top-level-inlined argument shapes into the canonical `args` envelope. If a tool call still arrives empty after normalization, the proxy logs `[agent] turn=N EMPTY ARGS — raw model output: "..."` so you can see the exact shape and rephrase.

### Long Pause Between Tool Result and Next Action

**Symptom:** A tool succeeds, then the agent loop sits idle for ~30 seconds before the next turn fires. No errors, no output — eventually the next tool call appears.

**What's happening:** Under a constrained JSON grammar, some local models emit EOS as their first token after a tool result, returning empty content that the parse-error retry path has to recover from — that's the lost ~30 seconds.

**What to do:** The proxy catches the empty turn inside `callLLMConstrained` and retries inline once with `temperature=0.7` and a continuation nudge. If it recurs consistently, restart the proxy to clear llama.cpp's slot cache:
```bash
docker compose restart atlas-proxy llama-server
```
Check `docker compose logs atlas-proxy | grep -E "empty LLM|raw_len=0"` — `raw_len=0` on both the initial call and the retry means the model is in a worse state than the retry handles.

### Model Keeps Editing After V3 Already Confirmed the Fix

**Symptom:** The agent makes a successful V3-verified edit (the TUI shows V3 progress events ending in `Probe passed`), then re-reads the same file and starts editing unrelated functions. Each follow-on edit triggers another full V3 cycle (~110s).

**What's happening:** Compact local models can have trouble self-assessing "is the user's original problem solved?" and keep planning more work after a verified edit.

**What to do:** The agent loop appends a strong user-role nudge after a V3-verified write toward emitting `{"type":"done"}`. If the model ignores it, be more explicit in your prompt that the single change is all you want. Harder stops (per-file edit cap, auto-done) are tracked as follow-up options.

### Model Hallucinates Filenames From Previous Sessions

**Symptom:** Brand-new session, fresh prompt, and the model's first tool call is a `read_file` on a filename that doesn't exist in this workspace — usually one that exists somewhere else you've worked recently.

**What's happening:** llama.cpp's KV slot persists between chat completions to keep the cache warm. Across sessions, residual attention bias from the previous session's tokens can leak into low-entropy outputs like fabricated filenames.

**What to do:** Every user turn starts by erasing **all** llama KV slots (with `--parallel` > 1 a session can land on any slot) so the next completion re-encodes the system prompt fresh (~1-2s on warm GPU). To disable the per-session erase if you'd rather keep the cache fully warm:
```bash
# .env
ATLAS_FRESH_SLOT_PER_SESSION=0
```
Restart the proxy after changing. If you see hallucinations with the erase disabled, restart `llama-server` to clear all slots.

### Multi-File Project: Sandbox `ModuleNotFoundError`

**Symptom:** Edit on a file that imports another module in the same project. V3 reports verification failure with `ModuleNotFoundError: No module named 'utils'` even though the import works on your machine.

**What's happening:** V3's `SandboxAdapter` ships every file the agent has read into the sandbox workspace alongside `solution.py`. A file that isn't in the read set (`ctx.FilesRead`) won't be there, so its imports fail.

**What to do:** Read the missing file via `read_file` so it lands in the project context. If you're calling the sandbox `/execute` API directly, pass supporting files in the request body:
```bash
curl -X POST http://localhost:30820/execute -d '{
  "code": "from utils import greet\nprint(greet(\"x\"))",
  "language": "python",
  "files": {"utils.py": "def greet(n): return f\"hi {n}\""}
}'
```

### Curses Bottom-Row `addwstr() returned ERR`

**Symptom:** Your curses program crashes at runtime with `_curses.error: addwstr() returned ERR`, but ATLAS reported the edit passed V3 verification.

**What's happening:** Writing to the last cell of a curses window (row=LINES-1 or column=COLS-1) returns ERR by documented curses behavior. `interactive_lint` rejects candidates that write there without a `try/except curses.error` wrap, so V3 has to find a wrapped variant before certifying. The idiomatic fix:
```python
try:
    stdscr.addstr(curses.LINES - 1, 0, border)
except curses.error:
    pass  # writing the bottom-right cell errors; benign
```

**What to do:** If V3 can't synthesize the wrap on its own, tell the model explicitly: *"wrap the addstr call at line N in `try: ... except curses.error: pass`."* Check `docker compose logs v3-service | grep interactive_lint` to confirm the lint gate fired.

### V3 Hangs for Several Minutes on Non-Python Files

**Symptom:** Asking ATLAS to write an HTML/CSS/JSON file causes a ~5-minute pause with PR-CoT repair attempts and LLM timeouts. The file eventually lands via the direct-write fallback.

**What's happening:** The V3 smoke check is language-aware — it derives language from the target file's extension and routes to the right checker (`.py` → Python compile, `.js` → `node --check`, `.ts` → `tsc --noEmit`, `.go` → `gofmt -e`, `.java` → `javac`, `.kt` → `kotlinc`, `.rs` → `rustc`, `.rb` → `ruby`, `.php` → `php`, `.sh` → `bash -n`, `.html` → `html.parser`, `.xml` → `ElementTree`, `.json` → `json.loads`, `.yaml` → `yaml.safe_load`). An unrecognized extension falls back to Python and fails, which cascades into repair. Note `.c`/`.cpp`/`.h` are not in the extension map (`_ext_to_lang` in `v3-service/pipeline.py`), so C/C++ files hit the Python fallback even though the sandbox itself has C/C++ checkers.

If `/v3/generate` receives an approved project build command, V3 emits a `build_verify` event after syntax/self-test verification. The command runs in an ephemeral sandbox workspace with the candidate overlaid onto the project, so failed build evidence blocks `passed=true` without writing the candidate into the real checkout. Overlay snapshots skip dependency caches, secrets, model/data artifacts, symlinks, and large files, and enforce file-count and byte limits. If a project needs heavyweight dependencies to build, install them inside the sandbox workspace as part of the explicit verification workflow.

**What to do:** For an unrecognized extension, add it to `_ext_to_lang` in `v3-service/pipeline.py` and rebuild the `v3-service` image. The proxy falls back to a direct write when V3 errors out, so the file lands regardless — just slowly. Check `docker compose logs v3-service | grep smoke_check` to confirm the right language was routed.

### V3 Pipeline Doesn't Fire on "Fix It Again" Prompts

**Symptom:** First request creates a file and V3 runs. A terse follow-up like "ok" or "yes" gets a conversational reply — no tool calls, no V3 events.

**What's happening:** The agent-loop tier classifier (`proxy/agent.go:classifyAgentTier`) answers one question: is this conversation, or work? Work is the default, and T0 requires positive evidence, because the two mistakes cost very differently. Reading conversation as work wastes one planner call on a message the model closes in a single turn; reading work as conversation caps the turn at 5 and skips planning, which makes the request fail outright.

A message is conversational only when it is under 12 characters (`hi`, `thanks`, `ok`) or shaped as a question — a `?` that ends a clause, or an interrogative opener (`why`, `what`, `how`, `is`, `can`, …) matched as a whole word. Task wording outranks both, so `can you fix the login bug?` is work despite the question mark. A client that declares `task_mode: work` (the TUI does) is never classified as conversational. Everything else is work: `still doesn't work, try again` and `the snake is moving way too fast, slow it down` both get the pipeline, even though neither names a file or matches a task-verb list.

**What to do:** Say what you want, even briefly — "yes, fix it" clears the T0 gate. If a follow-up runs the agent loop but V3 stays silent, the request tier isn't the gate — the file's own tier is. See [V3 Pipeline Not Firing on Feature Files](#v3-pipeline-not-firing-on-feature-files) and check `docker compose logs atlas-proxy | grep -E "write_file|edit_file"` for the file-tier line (e.g. `[write_file] app.py → T1:simple (8 lines)`).

### File Not Read Before Editing

**Symptom:** `edit_file` fails with "file not read yet — use read_file first before editing."

**Cause:** The proxy tracks which files the agent has read. If the model tries to edit a file it hasn't read in this session, the edit is rejected as a staleness protection.

**Fix:** The model should read the file first. If it keeps failing, type `/clear` in the TUI to reset chat history and rephrase.

### File Modified Externally

**Symptom:** `edit_file` fails with "file modified since last read — read it again before editing."

**Cause:** The file was changed on disk (by you or another process) after the model read it. The proxy compares modification timestamps.

**Fix:** The model needs to re-read the file. This usually resolves automatically on the next turn.

### Exploration Budget Warning

**Symptom:** Output shows "You have full project context in the system prompt. Do not read more files."

**Cause:** The model has made 4+ consecutive read-only calls (read_file, search_files, list_directory) without writing anything. At 4 reads, the proxy injects a nudge toward writing; at 5+, an escalated nudge. Reads always execute — the nudge steers the next turn, it never skips the read.

**Fix:** If the model is genuinely stuck exploring, be more specific about what you want changed.

---

## Geometric Lens Issues

### Requests Refused: the Lens Cannot Score

**Symptom:** A request fails at once with HTTP 503 `dependency_down`: "ATLAS needs the geometric lens for every request, and it ... Run `atlas doctor`." Or a run stops with `lens_unavailable`: "Stopped: ATLAS needs the geometric lens for every request, and it stopped answering (...)". Proxy `/ready` shows `"lens_ready": false` with a `lens_reason`; `atlas doctor` fails `status_dimensions` with `direct_agent: blocked`.

**Impact:** No request runs until the lens can score. This is deliberate: the lens is required ([ADR 0011](adr/0011-the-lens-is-required.md)). A run that stopped says whether it changed files before it stopped.

**Fix:** The message names the cause. Check the lens health and logs:
```bash
curl -s http://localhost:8099/health | python3 -m json.tool
docker compose logs geometric-lens
```

| Reason in the message | Fix |
|---|---|
| unreachable | The lens container is down or `ATLAS_LENS_URL` is wrong. `docker compose ps geometric-lens`. |
| no C(x) / no G(x) model loaded | The served model has no lens weights. Run `atlas model install-artifacts <name>` for a registry model, or `atlas lens build` ([SETUP.md](SETUP.md#geometric-lens-weights-required)). |
| cannot reach llama-server | The lens cannot reach the model server. Check `LLAMA_URL` / `LLAMA_EMBED_URL` and llama-server health. |
| self-test failed | See the `self_test_error` in `/health`. |
| drifted from the served model | See [Embedding-convention drift](#scores-look-plausible-but-are-wildly-off-scale-embedding-convention-drift). |

The proxy caches the lens answer for 5 seconds, so a fixed lens is accepted within 5 seconds.

### Lens Uncalibrated

**Symptom:** The lens logs `No gx_thresholds.json — Lens scores are uncalibrated; threshold interventions disabled` at startup, and the status shows `lens_calibration: uncalibrated`.

**Impact:** Requests run. The lens scores raw C(x) energies and G(x) probabilities; the calibrated uses (normalized routing, veto and correction thresholds) stay off until the model's calibration files exist.

**Fix:** `atlas lens build` calibrates the thresholds and writes `cx_normalization.json` and `gx_thresholds.json` into the bundle. See [CLI.md § atlas lens](CLI.md#atlas-lens).

### Scores Look Plausible but Are Wildly Off-Scale (embedding-convention drift)

**Symptom:** Everything reports healthy — pods `Ready`, `/health` 200, `gx-score` returns in-range-looking `gx_score` with a `likely_correct` verdict — but `cx_energy` is orders of magnitude off its calibrated range (e.g. ~600 when the model's pass/fail means are ~20-30). A gated benchmark launched in this state produces a complete, plausible, entirely invalid result.

**Cause:** The embed server is serving a different `/embedding` convention than the one the Geometric Lens `C(x)`/`G(x)` artifacts were trained on — typically per-token instead of pooled, or unnormalized instead of L2-normalized (‖v‖≈60 instead of ~1). Same dimensionality, wrong distribution; the cost-field MLP extrapolates to a huge energy and `cx_normalized` saturates. This happens after rebuilding the serving stack without `--pooling mean` (llama-server has no `--embd-normalize` server flag; the lens requests L2 normalization per-call via `embd_normalize` in the `/embedding` body).

**Verify:** where a `drift_fingerprint.json` sits next to the artifacts (see step 3 below), the lens re-scores it in its self-test (at boot, and again from `/ready` after a retryable failure). Check `/ready` and `/health`:
```bash
curl -s http://localhost:8099/health | python3 -m json.tool | grep -A2 fingerprint
```
`fingerprint_ok: false` with a `fingerprint_error` naming expected-vs-observed energy is the drift signal — `/ready` returns 503, the proxy refuses requests and names the drift, and scored responses carry `"drifted": true` with all `calibrated` flags forced false and no thresholds, so nothing downstream can mistake them for trustworthy or act on them.

**Fix:**
1. Confirm the embed server's convention. A pooled+normalized server returns a flat vector with ‖v‖≈1:
   ```bash
   curl -s -X POST http://localhost:8080/embedding -H 'Content-Type: application/json' \
     -d '{"content":"def add(a, b): return a + b","embd_normalize":-1}' | python3 -c "import sys,json,math; e=json.load(sys.stdin)[0]['embedding']; v=e if not isinstance(e[0],list) else [sum(c)/len(e) for c in zip(*e)]; print('shape', 'per_token' if isinstance(e[0],list) else 'flat', 'norm', round(math.sqrt(sum(x*x for x in v)),3))"
   ```
   The pooled norm should land in the low hundreds (~100-150 for the
   shipped Gemma artifacts). A norm of exactly `1.0` means the server
   normalized the vector despite `embd_normalize: -1`, and C(x) will read
   a flat ~0.8 for every input — scores that look healthy but separate
   nothing. Compare against `pass_energy_mean` in `cx_normalization.json`:
   served energies should span that band, not sit on one value.
2. Set `ATLAS_EMBED_POOLING=none` (the default; see [CONFIGURATION.md](CONFIGURATION.md)) and recreate the llama-server container so the entrypoint pins the flags. `--pooling` is server-global, and only `none` serves both the whole-text and per-step paths; pooling and scale are handled client-side.
3. After the server serves the correct convention, the boot self-test passes and `/ready` returns 200. A rebuild (`atlas lens build`) stamps the `embedding_contract` into `model_identity.json`. It does not write a drift fingerprint: `geometric_lens.drift.write_fingerprint` exists, but no command calls it yet, so the fingerprint check runs only where a `drift_fingerprint.json` was written by hand.

### Embedding Extraction Fails

**Symptom:** Lens logs show errors like "embedding extraction failed" or timeouts.

**Cause:** Lens calls llama-server's native `/embedding` endpoint. If llama-server is overloaded or embeddings aren't enabled, this fails.

**Fix:**
```bash
# Test the native embedding endpoint directly
curl -s http://localhost:8080/embedding \
  -H "Content-Type: application/json" \
  -d '{"content": "test"}' | python3 -m json.tool
```

The `--embeddings` flag is set by the llama-server entrypoint in every deployment mode (Compose, bare metal, K3s) — self-embeddings are always on because the Geometric Lens depends on them. The native `/embedding` path (not `/v1/embeddings`) also carries the per-layer hidden-states extension.

### Candidate Reported `unscored` (input exceeds the physical batch)

**Symptom:** The lens log shows `unscored: the input of 2055 tokens exceeds the /embedding physical batch of 2048 tokens`, v3-service emits `lens_unscored` for a candidate (`kind: embed_capacity`, `input_tokens`, `capacity_tokens`), or `atlas doctor` shows `lens_scoring: partial` naming an embed capacity below `ATLAS_MAX_TOKENS`.

**Cause:** llama-server processes one embedding request in a single physical batch (`-ub`, `ATLAS_UBATCH`) and refuses any longer input with HTTP 500. A Lens score is one forward over the whole sequence, so the lens reports the candidate as unscored rather than truncating or splitting it: a split input embeds later pieces without the context of earlier ones and is not the vector the artifacts were calibrated on. The generation budgets that produce candidates (4,096-token PlanSearch code, `ATLAS_MAX_TOKENS` 8,192 for a write) are larger than the largest micro-batch `atlas tier fit` chooses (2,048), so this is expected for long candidates on a default deployment.

**Impact:** The candidate keeps its sandbox result and is ranked after every scored candidate; it is delivered only when no scored candidate passed, and the `selected` event says so. Nothing is scored 0.0 or 0.5 in its place.

**Verify:**
```bash
curl -s http://localhost:8099/health | python3 -c "import sys,json; l=json.load(sys.stdin)['subsystems']['lens']; print({k:v for k,v in l.items() if k.startswith('embed_capacity')})"
```
`embed_capacity_tokens` is the longest input the lens can score; `embed_capacity_source` is `declared` (from `ATLAS_UBATCH`) or `observed` (from a refusal, authoritative).

**Fix:** Raising `ATLAS_UBATCH` raises the capacity, at a VRAM cost of roughly `ubatch × n_embd × 280` bytes for the compute buffer (about 4.4 GB at 4,096 on a 3,840-dim model): size it with `atlas tier fit`, recreate llama-server, and confirm it starts under `--fit off`. Lowering `ATLAS_MAX_TOKENS` bounds the writes the proxy asks the lens to score. Neither makes a split input scorable; scoring past the physical batch needs the calibration work described in [ADR 0010](adr/0010-lens-capacity-boundary-is-typed.md).

---

## Sandbox Issues

### Sandbox Unreachable (health check)

**Symptom:** Code is never tested. Proxy health shows `"sandbox": false`.

**Fix:** Check sandbox health:
```bash
# Docker Compose (host port 30820 maps to container port 8020)
curl -s http://localhost:30820/health

# Bare metal (direct port 8020)
curl -s http://localhost:8020/health
```

If the sandbox container is running but unhealthy, check logs:
```bash
docker compose logs sandbox
```

### Code Execution Timeout

**Symptom:** Sandbox returns `"error_type": "Timeout"`. Code takes too long to execute.

**Default timeout:** 30 seconds per request, capped at `MAX_EXECUTION_TIME`. The Compose stack sets that cap to 300 seconds (via `ATLAS_SANDBOX_MAX_EXECUTION_TIME` in `.env`), matching the proxy's `run_command` cap so long builds and test suites complete; outside compose the executor's in-code cap is 60 seconds.

**Fix:** If your code legitimately needs more time, set a higher timeout in the request (up to the cap), or raise `ATLAS_SANDBOX_MAX_EXECUTION_TIME`. If the code has an infinite loop, this is expected behavior. On timeout the whole process group is killed, so child processes the command spawned don't linger.

### Language Not Supported

**Symptom:** Sandbox returns an error for a specific language.

**Supported languages (execution):** Python, JavaScript, TypeScript, Go, Java, Kotlin, Rust, C, C++, Ruby, PHP, Bash. Syntax-only checks (`/syntax-check`, V3 smoke check) additionally cover HTML, XML, JSON, and YAML.

Check available runtimes:
```bash
curl -s http://localhost:30820/languages | python3 -m json.tool
```

---

## Benchmark Issues

### Bench runs fewer tasks than requested (`LIMITED MODE: running N tasks` with N below `--tasks`)

**Symptom:** `atlas bench --tasks 200` reports `LIMITED MODE: running 100
tasks` (or any count below what you asked for), or a resumed run prints
`Resuming: {done}/{total} complete, {n} remaining` and exits immediately.

**Cause:** the LiveCodeBench dataset cache
(`benchmark/datasets/.cache/livecodebench_v5.jsonl`) holds a partial
download. The HuggingFace rows API can fail mid-pagination; older versions
cached whatever they had and trusted the file forever. The full release_v5
set is ~880 tasks.

**Fix:** flag the cache as partial and re-run — the loader retries the full
fetch (falling back to the existing copy only if every source fails):
```bash
touch benchmark/datasets/.cache/livecodebench_v5.jsonl.partial
atlas bench --run-id <your-run-id> --tasks 200
```
Completed tasks are never lost: results live one-JSON-per-task under
`benchmark/results/<run-id>/v3_lcb/per_task/` and the runner resumes by
skipping any task whose result file exists. A run interrupted for any
reason (OOM, reboot, closed session) resumes the same way — just re-run
the identical `atlas bench` command.

## Performance

### Slow Generation (~2 tok/s)

The model is running on CPU instead of GPU. Check:
1. `nvidia-smi` — is llama-server listed as a GPU process?
2. `-ngl 99` (`--n-gpu-layers`) — are all layers offloaded?
3. NVIDIA Container Toolkit — is the container runtime configured for GPU access?

**Expected performance:** there is no current reference figure; throughput depends on the model, quantization and grammar mode. An earlier ~51 tok/s figure came from a configuration that no longer exists.

### V3 Pipeline Takes Several Minutes

This is normal for T2 files. The V3 pipeline makes multiple LLM calls:
- **Probe only (best case):** ~10-15 seconds (1 generation + 1 score + 1 test)
- **Phase 1 generation:** ~1-2 minutes (PlanSearch + DivSampling + scoring)
- **Phase 3 repair:** ~2-5 minutes (PR-CoT + Refinement, if needed)

To get faster (but lower quality) results:
- Keep files under 10 lines (stays T1, no V3) — recognized code extensions at 10+ lines go T2 regardless of complexity
- Reduce logic complexity (fewer functions, control flow)
- V3 only fires when truly needed — simple files are written instantly

### High RAM Usage

**Symptom:** System becomes sluggish or services get OOMKilled.

**Expected RAM usage:**
- llama-server: ~8 GB (model in VRAM, minimal RAM)
- geometric-lens: ~200 MB (PyTorch runtime + models)
- v3-service: ~150 MB (PyTorch runtime)
- sandbox: ~100 MB (base, spikes during compilation)
- atlas-proxy: ~30 MB (Go binary)

**Total:** ~500 MB RAM + 8.2 GB VRAM. If you have less than 14 GB system RAM, other services may compete for memory.

---

## Getting Help

If your issue isn't listed here:
1. Check service logs: `docker compose logs <service-name>`
2. Check the proxy health endpoint: `curl http://localhost:8090/health`
3. See [CONFIGURATION.md](CONFIGURATION.md) for all environment variables
4. Open an issue on [GitHub](https://github.com/inferstep/ATLAS/issues)

<!-- One line for a proof of the checks. The pull request that carries it is closed without a merge. -->
