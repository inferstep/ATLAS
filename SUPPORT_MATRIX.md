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

The published model-server images for amd64 need a processor with AVX2 (and with it FMA, F16C and BMI2). A virtual machine must pass AVX2 on to its guest; some default processor models do not.

## Models (registry)

`lens=supported` means published weights exist; **calibrated** requires
per-model `cx_normalization.json` + `gx_thresholds.json` (see
`atlas lens check`). Quality-validation status is what `atlas doctor`
and `atlas lens check` report against the installed bundle.

| Registry ID | Level | Lens | ASA | Notes |
|---|---|---|---|---|
| Qwen3.5-9B-Q6_K | Supported | supported (uncalibrated legacy bundle) | supported (A/B-validated May 2026) | Reference model; hash-pinned public download |
| gemma-4-12b-it-Q4_K_M | Preview | supported; calibration **derived + verified** on maintainer hardware (val AUC 0.73, 287 LCB samples) — live lens reports `cx_calibrated: true`. The published HF bundle is still the uncalibrated one; re-publishing the calibrated bundle is a maintainer decision (moderate AUC, shared artifact) | Unverified — vector built, published, hash-pinned, and **on**: steering is always on, so `atlas model install-artifacts` writes its `.model` marker. Built from prompts that named the tool `ast_edit` (now `structural_edit`). A/B on 2026-09-27 (120 held-out probes, 2 samples per arm, scale 0.5): no measurable effect on the first file-writing tool, because unsteered gemma already picks `structural_edit` for 95% of whole-function rewrites and never used `edit_file` for one. A vector rebuilt for the current tool names did no better, so the shipped vector stays. Not Supported: no benefit is measured (see § Feature paths — ASA steering) | Manual GGUF download (Gemma ToU); artifacts hash-pinned |
| Qwen3.5-9B-Q4_K_M / Q8_0 | Preview | no-artifacts: the lens loads a bundle only for the exact model, so each quant needs its own (`atlas bench`, then `atlas lens build --from-results`) | unverified (the Q6_K vector) | Hash-pinned public downloads |
| Qwen3.5-7B / 14B / 32B | Preview | no-artifacts | no-artifacts | HF-gated upstream (HF_TOKEN required; no anonymous hash). Requests are refused until a lens bundle exists (`atlas lens build`): the lens is required (ADR 0011) |
| Bring-your-own GGUF | Preview | Requires `atlas lens build` (per-model bundle) | Requires `atlas asa build` | Requests are refused until the model has its lens bundle (ADR 0011); `atlas bench` runs without one, to build it — see § Model contract |
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
| `model_runtime` | Is the model served and reachable, as the lens sees it | supported / unreachable / unknown (the lens is unreachable) |
| `direct_agent` | The agent loop (tools, permissions, sandbox verify) | supported / **blocked** — blocked while the lens cannot score, because every request needs it (ADR 0011). An uncalibrated lens can score |
| `lens_identity` | Cost field matches the served model (identity + dimension) | supported / no-artifacts / dim-mismatch |
| `lens_scoring` | Raw C(x)+G(x) scoring available | supported / partial (G(x) missing, which blocks `direct_agent`; or the embed capacity is below the generation ceiling so the longest writes come back unscored) / disabled |
| `lens_calibration` | Per-model normalization + thresholds loaded | calibrated / uncalibrated / disabled |
| `lens_intervention` | Automatic corrective behavior | active *(only when calibrated)* / neutral / disabled |
| `asa` | Activation-steering vector for the served model | active (marked for the served model; whether its effect was measured is the registry's `asa_status`) / unverified (no matching marker) / incompatible / missing |

**Automatic intervention stays neutral or disabled whenever calibration
is absent** — this is enforced in the runtime, not just displayed: the
agent applies thresholds only when `calibratedThresholds()` succeeds
(`proxy/agent.go`), so an uncalibrated or mismatched lens produces
telemetry but never steers with another model's cutoffs.

Reference model (Qwen3.5-9B-Q6_K), current: `model_runtime` supported,
`direct_agent` supported, `lens_identity` supported, `lens_scoring`
supported, `lens_calibration` **uncalibrated** (legacy bundle predates
the calibration files), `lens_intervention` **neutral**, `asa`
active (A/B-validated in May 2026, before the `ast_edit` → `structural_edit`
rename; not re-measured since). The gemma reference install additionally
has `lens_calibration` calibrated (derived + verified locally) with
`lens_intervention` active and `asa` active (registry status unverified:
a 2026-09-27 A/B found no measurable effect on tool choice).

### Lens bundle provenance

Every bundle activated by `atlas lens build` auto-writes `provenance.json`:
backbone + dim + quant + layer, dataset, training commit,
hyperparameters, seed, train/val split, validation metrics,
normalization + thresholds, creation time, and SHA-256 of every artifact
file. `geometric_lens.provenance.is_complete()` gates Supported
eligibility — a bundle missing required fields stays Preview/Legacy
rather than silently claiming Supported. The gemma reference bundle
carries a complete manifest (val AUC 0.73, all seven files hashed).

### Model contract

ATLAS is **model-agnostic in its agent loop, per-model-bundle for
Lens/ASA**: the agent loop (grammar constraints, tools, sandbox
verification) makes no model-family assumptions — behavior keys off
GGUF metadata and stream shape, never model names. Every request also
needs the model's own Lens bundle (identity-checked, dimension-checked
at load; mismatched bundles are rejected and the lens reports itself
disabled), because the lens is required (ADR 0011): without it the
proxy refuses the request and says why. The ASA vector is
marker-gated at llama-server startup. A model is therefore usable once
it has a registry entry with published artifacts or a locally built
bundle (`atlas bench`, then `atlas lens build`).

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
| ASA steering | Always on wherever a vector is installed for the served model. Supported on Qwen3.5-9B-Q6_K; unverified on gemma | A/B-validated (May 2026) on Qwen, before the tool rename, and not re-measured. On gemma, a 2026-09-27 A/B found no measurable effect on the first file-writing tool (no headroom: unsteered gemma already chooses the tool the vector targets); whole-task outcomes were not measured. Every recorded dev-server measurement ran gemma steered |
| Call-graph reasoning (#39) | Preview | Always on, Python files only (the veto, repair context and read/outline edges); hermetic tests; effect on task outcomes not yet measured |
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
  security fixes and critical-bug fixes for 90 days after N ships.
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
