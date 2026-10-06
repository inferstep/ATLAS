# ATLAS Support Matrix

Applies to: **V3.1.6 and the current `dev` branch.** This document is
versioned with the repo — the matrix for a release is the file at that
release's tag.

Support levels used throughout:

| Level | Meaning |
|---|---|
| **Supported** | Validated on real hardware or in CI; regressions are release blockers; bugs are triaged first. |
| **Preview** | Wired and tested in CI, but real-hardware/quality evidence is incomplete; behavior may change. |
| **Experimental** | Complete and intentionally optional; off by default; enable-at-your-own-risk. |
| **Community-tested** | Works per a community report we link to; not on maintainer hardware. |
| **Research-only** | Exists for the benchmark/ablation pipeline; not part of the product runtime. |
| **Unsupported** | Not wired, not tested, or explicitly rejected; failure is expected and should be clear. |
| **Roadmap** | Planned but not wired; tracked in an issue. No support claims until it ships. |

**Internal** marks *audience* (a surface meant for the stack itself,
not end users), not maturity — it composes with a level, e.g.
"Experimental (Internal)".

Nothing below is marked Supported solely because code exists — each
Supported row cites its validation.

## Operating systems

| OS | Arch | Level | Validation |
|---|---|---|---|
| Ubuntu 22.04 / 24.04 | amd64 | Supported | CI install matrix (bootstrap ×2 runs, artifact checks) + maintainer hardware |
| Debian 12 | amd64 | Supported | CI install matrix |
| Rocky Linux 9 (RHEL-like) | amd64 | Supported | CI install matrix; maintainer dev box is EL9 |
| Linux Mint / Pop!_OS (ID_LIKE ubuntu) | amd64 | Preview | Accepted by bootstrap with a warning; not separately tested |
| macOS 14+ (Apple Silicon) | arm64 | Supported | Maintainer-verified on M2 Pro 32GB (hybrid Metal deployment) |
| Linux | arm64 | Preview | llama-vulkan image is built multi-arch on releases; no end-to-end arm64 device validation yet (see § ARM64 scope) |
| Arch / openSUSE / Alpine / NixOS | any | Unsupported | Bootstrap refuses with a clear message |
| Windows (incl. WSL) | any | Unsupported | Untested; no claims made |

## Inference backends

| Backend | Level | Tested device | Validation |
|---|---|---|---|
| CUDA (NVIDIA, Blackwell — RTX 50xx / B100 / GB10) | Supported | RTX 5060 Ti 16GB | Primary dev hardware; release smoke tests. Published image is compiled for compute capability 12.0/12.1 only |
| CUDA (NVIDIA, pre-Blackwell — RTX 20xx–40xx, GTX 10xx, T4/L4/V100/A100/H100) | Preview (local rebuild required) | — | Published image fails with `no kernel image`; rebuild with `--build-arg CUDA_ARCH=<cc>` per SETUP.md. Upstream llama.cpp supports these; no maintainer validation on ATLAS |
| ROCm (AMD, x86_64) | Community-tested | RX 7900 XTX | [GH #26](https://github.com/inferstep/ATLAS/issues/26) |
| Metal (Apple Silicon hybrid) | Supported | M2 Pro 32GB | Maintainer-verified; native llama-server + Docker services |
| Vulkan (universal) | Preview | lavapipe (CPU ICD) boot path | Smoke-tested; the designated fallback for Intel/others |
| CPU (lavapipe via Vulkan image) | Preview | CI-adjacent smoke | Functional but slow by design |
| Intel SYCL | Roadmap | — | Vulkan is the Intel path today |
| Multi-GPU | Unsupported | — | `ATLAS_GPU_INDEX` selects ONE GPU; splitting across GPUs is untested (GH #34 is roadmap) |

## Models (registry)

`lens=supported` means published weights exist; **calibrated** requires
per-model `cx_normalization.json` + `gx_thresholds.json` (see
`atlas lens check`). Quality-validation status is what `atlas doctor`
and `atlas lens check` report against the installed bundle.

| Registry ID | Level | Lens | ASA | Notes |
|---|---|---|---|---|
| Qwen3.5-9B-Q6_K | Supported | supported (uncalibrated legacy bundle) | supported (A/B-validated May 2026) | Reference model; hash-pinned public download |
| gemma-4-12b-it-Q4_K_M | Preview | supported, but with no usable signal in 3.1.x: an install reports `cx_calibrated: true` from calibration files fit to a different cost field than the published one, and per-token scoring cannot run under the default pooling ([#281](https://github.com/inferstep/ATLAS/issues/281)). Fixes planned for 3.2.0 | Preview — vector built, published, hash-pinned; **on by default**: the installer builds the vector (or `atlas model install` downloads the hash-pinned one) and writes its `.model` marker, so llama-server applies it at scale 0.5 on every start. Not A/B-measured on gemma; to run without it, install with `ATLAS_BOOTSTRAP_SKIP_ASA=1`. Not Supported until an A/B effect measurement + quality-regression bounds exist (see § Feature paths — ASA steering) | Manual GGUF download (Gemma ToU); artifacts hash-pinned |
| Qwen3.5-9B-Q4_K_M / Q8_0 | Preview | unverified (same-family artifacts, combo unvalidated) | unverified | Hash-pinned public downloads |
| Qwen3.5-7B / 14B / 32B | Preview | no-artifacts | no-artifacts | HF-gated upstream (HF_TOKEN required; no anonymous hash) |
| Bring-your-own GGUF | Preview | Requires `atlas lens build` (per-model bundle) | Requires `atlas asa build` | Direct agent mode works model-agnostically; V3 scoring/steering need the per-model bundle — see § Model contract |
| Frozen Qwen3-14B (V3.0 benchmark model; its 74.6% LCB result is withdrawn) | Research-only | frozen reference | — | Historical benchmark reference only; not a runtime registry entry |

### Reference-model status dimensions

"The lens works" is ambiguous — it can mean the model is served, or raw
scoring is available, or per-model calibration is loaded, or automatic
interventions are firing. These are **separate** dimensions and every
status surface (the proxy `/v1/calibration/status` endpoint, the TUI
badge, `atlas doctor`, `atlas lens check`) reports the same seven so
they cannot disagree — the CLI/TUI/doctor all read the endpoint, which
computes them in one place (`proxy/lens.go`).

| Dimension | Meaning | Statuses |
|---|---|---|
| `model_runtime` | Is the model served and reachable | supported / unreachable |
| `direct_agent` | The agent loop (tools, permissions, sandbox verify) | **supported** always — model-agnostic, independent of lens/ASA |
| `lens_identity` | Cost field matches the served model (identity + dimension) | supported / no-artifacts / dim-mismatch |
| `lens_scoring` | Raw C(x)+G(x) scoring available | supported / partial / disabled |
| `lens_calibration` | Per-model normalization + thresholds loaded | calibrated / uncalibrated / disabled |
| `lens_intervention` | Automatic corrective behavior | active *(only when calibrated)* / neutral / disabled |
| `asa` | Activation-steering vector | supported / unverified / missing |

**Automatic intervention stays neutral or disabled whenever calibration
is absent** — this is enforced in the runtime, not just displayed: the
agent applies thresholds only when `calibratedThresholds()` succeeds
(`proxy/agent.go`), so an uncalibrated or mismatched lens produces
telemetry but never steers with another model's cutoffs.

Reference model (Qwen3.5-9B-Q6_K), current: `model_runtime` supported,
`direct_agent` supported, `lens_identity` supported, `lens_scoring`
supported, `lens_calibration` **uncalibrated** (legacy bundle predates
the calibration files), `lens_intervention` **neutral**, `asa`
supported (A/B-validated). A default gemma install reports
`lens_calibration` **calibrated** and `lens_intervention` **active**,
because the calibration files tracked in this repository are loaded
next to the published cost field. That report is misleading in 3.1.x:
the calibration was fit to a different cost field, per-token scoring
cannot run under the default pooling, and the scores barely change
between inputs, so the lens neither vetoes nor alerts
([#281](https://github.com/inferstep/ATLAS/issues/281)). `asa` is
supported: the installer writes the steering vector's marker by
default (not A/B-measured on gemma).

### Lens bundle provenance

Every bundle activated by `atlas lens build`/`retrain` auto-writes `provenance.json`:
backbone + dim + quant + layer, dataset, training commit,
hyperparameters, seed, train/val split, validation metrics,
normalization + thresholds, creation time, and SHA-256 of every artifact
file. `geometric_lens.provenance.is_complete()` gates Supported
eligibility — a bundle missing required fields stays Preview/Legacy
rather than silently claiming Supported. The gemma reference bundle
carries a complete manifest (val AUC 0.73, all seven files hashed).

### Model contract

ATLAS is **direct-mode model-agnostic, per-model-bundle for Lens/ASA**:
any llama.cpp-loadable GGUF drives the direct agent loop (grammar
constraints, tools, sandbox verification) with no model-family
assumptions — behavior keys off GGUF metadata and stream shape, never
model names. The differentiating V3 scoring/steering stack requires the
model's own Lens bundle (identity-checked, dimension-checked at load;
mismatched bundles are rejected and the lens reports itself disabled)
and ASA vector (marker-gated at llama-server startup). "Any model, full
stack" is therefore not claimed: full-stack support = registry entry or
locally-built bundle.

## Deployment modes

| Mode | Level | Validation |
|---|---|---|
| Docker Compose (base + backend overlay) | Supported | CI compose validation on every overlay; releases smoke-tested; the deterministic E2E drives the control plane |
| macOS hybrid (native llama + compose) | Supported | Maintainer-verified (M2 Pro) |
| K3s (generated manifests) | Preview | Templates validated + rendered in CI; no automated live-cluster test |
| Bare metal | Preview | Documented (SETUP.md Method 2); manual validation only |
| Offline / air-gapped | Unsupported | Model + artifact downloads require network; no offline bundle exists |
| Rootless Docker / Docker Desktop (Linux) | Unsupported | Untested; no claims made. (Docker Desktop **on macOS** is part of the Supported hybrid path — it hosts the four non-inference services only) |

## Sandbox languages

Verification depth: **executed** = code runs via `/execute` with
timeouts/output caps; **syntax** = compile/parse check only.

| Language | Depth | Level |
|---|---|---|
| Python 3 | executed + self-tests | Supported |
| JavaScript / TypeScript (node, tsx) | executed | Supported |
| Go | executed | Supported |
| Rust | executed | Supported |
| C / C++ | executed (gcc/g++) | Supported |
| Bash / sh | executed | Supported |
| HTML / XML / JSON / YAML | syntax | Supported |
| Java | executed | Preview (installed in default sandbox; CI smoke test containerized; host-runner tests skipif-gated) |
| Kotlin | executed | Preview (installed in default sandbox; CI smoke test containerized; host-runner tests skipif-gated) |
| Ruby | executed | Preview (installed in default sandbox; CI smoke test containerized; host-runner tests skipif-gated) |
| PHP | executed | Preview (installed in default sandbox; CI smoke test containerized; host-runner tests skipif-gated) |

## Feature paths

| Path | Level | Validation |
|---|---|---|
| Direct agent (tools, permissions, sandbox verify) | Supported | Deterministic E2E in CI + unit/contract suites |
| V3 pipeline (probe → candidates → selection) | Supported (control plane) / Preview (per-model quality) | Deterministic V3/Lens E2E in CI; real-model quality validated on the reference model only |
| Lens C(x)/G(x) scoring | Supported (contract) / per-model calibration required for interventions | Identity + dim checks enforced; calibration status surfaced everywhere |
| ASA steering | Supported on Qwen3.5-9B-Q6_K; Preview on gemma-4-12b-it-Q4_K_M, on by default (registry `supported`) | A/B-validated (May 2026) on Qwen; not A/B-measured on gemma. The installer builds the vector, or `atlas model install` downloads the hash-pinned one, and writes its `.model` marker, so llama-server applies it at scale 0.5 on every start. To run without it, install with `ATLAS_BOOTSTRAP_SKIP_ASA=1` |
| Call-graph reasoning (#39) | Experimental | `ATLAS_CALL_GRAPH=1`; hermetic tests |
| Host verification (`ATLAS_VERIFY_IN=host`) | Experimental | Explicit opt-in; removes the container backstop |
| Benchmark/ablation stack (`ATLAS_V3_*`, lens feedback) | Research-only | Never read by the product runtime (contract-tested) |
| IDE integration | Unsupported | No extension exists |

## Context lengths

Sized per model + hardware by `atlas tier fit` (KV-cache-aware). The
compose default is 131072 total across 4 slots on a 16 GB card;
macOS-native defaults are smaller (documented in SETUP_MACOS). Any
context a model + VRAM combination can hold is in scope; exceeding VRAM
fails fast at llama-server startup (fit is off).

## Installation methods

| Method | Level |
|---|---|
| `curl \| bash` bootstrap (`scripts/atlas-bootstrap.sh`) | Supported (CI-tested on 4 distros, idempotent, sudo/non-root paths) |
| Manual compose (`cp .env.example .env` + `atlas init`) | Supported |
| `pip install -e .` CLI from checkout | Supported (the only packaged distribution today; no PyPI release) |
| K3s `scripts/install.sh` | Preview |

## Version compatibility policy

- **Supported versions:** the latest release (N) fully; N−1 receives
  security fixes and critical-bug fixes for 90 days after N ships. This
  applies from 3.2.0 on. Until then no further 3.1.x release is planned;
  see [SECURITY.md](SECURITY.md).
- **Registry / artifact bundle schemas:** additive changes only within
  a minor release; consumers ignore unknown fields; identity
  (`model_identity.json`) is mandatory in every bundle from V3.1.2 on.
- **HTTP/SSE protocol:** additive event types are non-breaking (clients
  drop unknown types — the TUI does); field removals or renames require
  a major version and one release of deprecation notice.
- **Python:** ≥3.9 (CLI), 3.13 (sandbox), 3.11 (proxy/lens/v3 containers), CI runs 3.12.
  **Go:** proxy 1.24+, TUI 1.26+ (GOTOOLCHAIN auto-fetch).
- **Docker:** Engine 24+ with Compose v2. **llama.cpp:** pinned by
  revision in all inference Dockerfiles; bumps go through the CI
  patch-apply gate and a hardware smoke before release.
- **Deprecations:** announced in the changelog one minor release before
  removal; removed config keys are listed in CONFIGURATION.md § removed
  variables and ignored (never fatal) when present in old configs.
