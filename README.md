<p align="center">
  <img src="docs/images/herodemo.gif" alt="ATLAS TUI in action"/><br/>
  <sub><i>The ATLAS TUI live, 10× sped up, running the V3 pipeline on a file creation.</i></sub>
</p>

<h1 align="center">A.T.L.A.S.</h1>
<p align="center"><b>Adaptive Test-time Learning and Autonomous Specialization</b></p>

<p align="center">
  <img src="https://img.shields.io/badge/version-V3.1.7-blue" alt="Version"/>
  <img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="License"/>
  <img src="https://img.shields.io/badge/model-agnostic-green" alt="Model-agnostic"/>
</p>

<p align="center">
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/test.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/test.yml?branch=main&label=tests" alt="Tests"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/install-test.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/install-test.yml?branch=main&label=install%20matrix" alt="Install matrix"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/codeql.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/codeql.yml?branch=main&label=codeql" alt="CodeQL"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/container-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/container-scan.yml?label=container%20scan" alt="Container scan"/></a>
  <a href="https://github.com/inferstep/ATLAS/actions/workflows/verify-tags.yml"><img src="https://img.shields.io/github/actions/workflow/status/inferstep/ATLAS/verify-tags.yml?label=release%20signature" alt="Release signature"/></a>
  <a href="https://scorecard.dev/viewer/?uri=github.com/inferstep/ATLAS"><img src="https://api.scorecard.dev/projects/github.com/inferstep/ATLAS/badge" alt="OpenSSF Scorecard"/></a>
</p>

<p align="center">
  <a href="docs/lang/zh-CN/README.md"><img src="https://img.shields.io/badge/文档-简体中文-orange" alt="简体中文"/></a>
  <a href="docs/lang/ja/README.md"><img src="https://img.shields.io/badge/ドキュメント-日本語-orange" alt="日本語"/></a>
  <a href="docs/lang/ko/README.md"><img src="https://img.shields.io/badge/문서-한국어-orange" alt="한국어"/></a>
</p>


## 🌎 What is ATLAS?

**ATLAS is a local coding agent that brings frontier-style reasoning and verification to compact open models.** It puts more intelligence in the system around the model (planning, candidate generation, quality scoring, sandboxed testing, and repair) so smaller models can tackle real software work entirely on your own hardware, without a hosted API or per-token fees.

## 💡 Why ATLAS?

* **Get more from smaller models.** ATLAS adds planning, candidate selection, verification, and repair around the model instead of depending on a single generation.
* **Verify before accepting.** Generated code can be compiled, tested, and corrected inside an isolated execution environment.
* **Spend compute where it matters.** Straightforward edits take a shorter path, while harder tasks receive more candidates, reasoning, and validation.
* **Run your own model.** Use a compatible GGUF model on NVIDIA, AMD, Apple Silicon, Vulkan, or CPU-supported hardware. A model needs a lens bundle; `atlas lens build` makes one for a model that has none.
* **Keep control local.** ATLAS does not intentionally upload your repository or prompts to a hosted model or ATLAS-operated service. Sandbox commands have outbound network access by default; set `ATLAS_SANDBOX_NET_INTERNAL=true` to disable it.
* **Own the full stack.** ATLAS is open source and self-hosted. It requires no hosted model or third-party model-provider API key; a local per-installation service token authenticates ATLAS services.

---

## 📰 Latest release

**[V3.1.7 "Maia"](https://github.com/inferstep/ATLAS/releases/tag/v3.1.7)** is the current release. Its notes are on that release page and in the [CHANGELOG](CHANGELOG.md).

## ⭐ Star History

<!-- Self-hosted chart: rendered weekly by .github/workflows/star-chart.yml
     onto the `star-history` asset branch (scripts/star-history-chart.py).
     Replaces the star-history.com embed, whose shared token pool
     rate-limits unpredictably. -->
<a href="https://github.com/inferstep/ATLAS/stargazers">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/inferstep/ATLAS/star-history/star-history-dark.svg" />
   <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/inferstep/ATLAS/star-history/star-history-light.svg" />
   <img alt="Star history chart" src="https://raw.githubusercontent.com/inferstep/ATLAS/star-history/star-history-light.svg" width="100%" />
 </picture>
</a>

<sub>Updated weekly (Mondays, via GitHub Actions).</sub>

---

## 🧱 What ATLAS Does

1. **[atlas-tui](docs/CLI.md)** - native Bubbletea terminal UI; the canonical chat client. Type `atlas` in any project directory to launch it.
   - [Live pipeline view](docs/CLI.md#panes) - watch V3 stages stream in a side pane
   - [Slash commands](docs/CLI.md#slash-commands) - `/add`, `/diff`, `/commit`, `/run` for local file context and shell-out
   - [Input modes](docs/CLI.md#input-modes) - chat, `!bash`, and `/slash` with a hint dropdown

2. **[atlas-proxy](docs/ARCHITECTURE.md#3-atlas-proxy-outer-layer)** - Go agent loop that orchestrates the system.
   - [Tool-call routing](docs/ARCHITECTURE.md#tools) - classifies file operations by complexity tier
   - [Grammar enforcement](docs/ARCHITECTURE.md#grammar-enforcement) - GBNF schemas strongly steer output toward the expected JSON shapes, with proxy-side recovery for malformed or truncated output
   - [BiasBusters](docs/ARCHITECTURE.md#tool-selection-bias-mitigations) - four composed mitigations (descriptions, grammar bans, system notes, ASA steering) that push the model toward `structural_edit` for structural code edits
   - [Safety limits](docs/ARCHITECTURE.md#safety-limits) - turn caps, token budgets, timeouts

3. **[V3 Pipeline](docs/ARCHITECTURE.md#4-v3-pipeline-inner-layer)** - multi-phase code generation; turns a single prompt into a verified candidate.
   - [PlanSearch](docs/ARCHITECTURE.md#pipeline-flow) - constraint-driven structured planning
   - [DivSampling](docs/ARCHITECTURE.md#pipeline-flow) - diverse candidates across temperature and strategy
   - [Budget tiers](docs/ARCHITECTURE.md#pipeline-flow) - the allocator picks a compute tier per task; the tier sets how many candidates are generated, and the prompt and token budget for each
   - [PR-CoT Repair](docs/ARCHITECTURE.md#pipeline-flow) - repairs failing candidates from the sandbox's error output
   - [Refinement Loops](docs/ARCHITECTURE.md#pipeline-flow) - sandbox verify and correct, then repeat

4. **[Geometric Lens](docs/ARCHITECTURE.md#5-geometric-lens)** - energy-based scoring over the model's own embeddings, no external oracle. ([What is a "Geometric Lens"?](docs/ARCHITECTURE.md#why-geometric-lens))
   - [C(x) Cost Field](docs/ARCHITECTURE.md#scoring-models) - model-hidden-dim→512→128→1 MLP that scores candidate quality
   - [G(x) Quality Prediction](docs/ARCHITECTURE.md#scoring-models) - XGBoost ensemble whose score helps set how many candidates are generated; selection itself takes the lowest C(x)
   - [Per-step scoring](docs/API.md#geometric-lens-port-8099) - per-token C(x)/G(x) scoring of writes; per-model thresholds drive interventions only when the model's bundle is calibrated

5. **[Sandbox](docs/ARCHITECTURE.md#6-sandbox)** - isolated execution for build verification.
   - Multi-language execution: Python, Rust, Go, C, Shell, others. Rust, Go and C run as commands in the project; V3 checks their syntax, and the sandbox's `/execute` route compiles them but cannot run them yet ([#163](https://github.com/inferstep/ATLAS/issues/163))
   - Compilation and linting before scoring
   - Runs both generated and existing test suites

6. **[llama-server](docs/CONFIGURATION.md#6-llama-server)** - local LLM inference on one consumer GPU.
   - GPU-accelerated quantized inference (Q6_K / Q4_K_M) - NVIDIA CUDA, AMD ROCm, Apple Metal (macOS hybrid), and Vulkan; Intel SYCL on the roadmap
   - Grammar-constrained decoding at the token level
   - Self-embeddings, so the lens doesn't need a second model

Full documentation (setup, architecture, configuration, troubleshooting, and the [research behind each component](docs/SOURCES.md)) lives in the [docs/](docs/) directory.

---

## 🚀 Get Started

One-shot install:
```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

Prefer not to pipe a moving script into bash? Same installer, two more careful ways to run it:
```bash
# Pinned to a release: script, checkout, and images all at the signed tag
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/v3.1.7/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.7 bash

# Review before running
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

The script detects your distro (Ubuntu, Debian, RHEL, Fedora, Rocky, Alma) and your GPU vendor (NVIDIA → nvidia-container-toolkit; AMD → ROCm device passthrough), installs the appropriate runtime, downloads the model weights, builds the ASA steering vector, and starts the stack. Expect 10-30 minutes; the model download is the bottleneck.

Then in any project directory, run `atlas`.

**Requirements**

| | |
|---|---|
| GPU | 16 GB+ VRAM. NVIDIA (CUDA, Supported), AMD (ROCm, Community-tested), or Apple Silicon (Metal, macOS hybrid, Supported); Vulkan (Preview) covers most other GPUs. The prebuilt CUDA image targets Blackwell (RTX 50xx); older NVIDIA GPUs need a one-time local rebuild (see [SETUP.md § CUDA Compute Capability](docs/SETUP.md#cuda-compute-capability-dockerfilev31)). Levels: [SUPPORT_MATRIX.md](docs/SUPPORT_MATRIX.md); GPU list: [SETUP.md § Supported GPUs](docs/SETUP.md#supported-gpus). To size a specific model to your card, see [What fits on my GPU?](docs/TROUBLESHOOTING.md#what-fits-on-my-gpu). |
| Runtime | Docker (NVIDIA: + nvidia-container-toolkit; AMD: standalone Docker is enough) |
| Python | 3.9+ |
| Disk | ~20 GB CUDA / ~22 GB ROCm (model weights + container images) |

Apple Silicon runs natively through the macOS hybrid Metal path (native llama-server + Docker for the rest - see **[SETUP_MACOS.md](docs/SETUP_MACOS.md)**); Intel Arc (SYCL) is on the roadmap. For the manual install path (Docker Compose, bare-metal, K3s) and the full set of bootstrap flags, see **[SETUP.md](docs/SETUP.md)**.

---

## ⚠️ Known Limitations

- **Linux Docker stack, plus a native macOS path.** NVIDIA (Supported), AMD ROCm (Community-tested), and Vulkan (Preview) Docker paths exist today; Apple Silicon (Supported) runs via the native macOS hybrid Metal path ([#32](https://github.com/inferstep/ATLAS/issues/32)). Intel Arc / SYCL is Roadmap. Level definitions: [SUPPORT_MATRIX.md](docs/SUPPORT_MATRIX.md).
- **ATLAS has no current benchmark result.** The V3.0 LiveCodeBench figure is withdrawn (see the [CHANGELOG](CHANGELOG.md#withdrawn-the-v30-livecodebench-result-746)), and no capability or reliability figure has been measured for the current release. Numbers will be published only after a re-measurement on held-out tasks; model-specific results are tracked in [#28](https://github.com/inferstep/ATLAS/issues/28).
- **The lens's signal is not proven yet.** On the reference model it scores but stays uncalibrated, so its interventions are neutral. On 3.1.6 it gave no usable signal, and that is not yet measured on `dev` ([#281](https://github.com/inferstep/ATLAS/issues/281)).
- **Complex feature additions can be inconsistent.** Compact models sometimes spend agent turns exploring an unfamiliar codebase before writing code.
- **Grammar-constrained decoding is slower than unconstrained decoding.**

---

## 🗺️ Roadmap

**3.2.0** ([milestone](https://github.com/inferstep/ATLAS/milestone/2)) - make "done" mean what it says, and ship the `dev` line.
- Verification: close the gaps between "completed" and what was verified ([#330](https://github.com/inferstep/ATLAS/issues/330)), and count a guard's refusal as a failure ([#327](https://github.com/inferstep/ATLAS/issues/327)).
- Safety: harden the sandbox and the first install ([#384](https://github.com/inferstep/ATLAS/issues/384), [#410](https://github.com/inferstep/ATLAS/issues/410), [#483](https://github.com/inferstep/ATLAS/issues/483)), and test the service token end to end ([#212](https://github.com/inferstep/ATLAS/issues/212)), on K3s too ([#697](https://github.com/inferstep/ATLAS/issues/697)).
- IDEs: the VS Code extension, in Preview.
- Release: a release candidate tested for at least three days ([#196](https://github.com/inferstep/ATLAS/issues/196)), with signatures and build attestations ([#252](https://github.com/inferstep/ATLAS/issues/252), [#361](https://github.com/inferstep/ATLAS/issues/361)).

**Backlog / help wanted**
- Hardware: ARM64 multi-arch builds ([#115](https://github.com/inferstep/ATLAS/issues/115)), multi-GPU for larger models ([#34](https://github.com/inferstep/ATLAS/issues/34)), Intel oneAPI / SYCL ([#27](https://github.com/inferstep/ATLAS/issues/27)).
- Tooling: JetBrains plugin ([#35](https://github.com/inferstep/ATLAS/issues/35)).
- Deployment: ROCm on K3s / Kubernetes.
- Research: reasoning with sampling ([#9](https://github.com/inferstep/ATLAS/issues/9)); rerun LiveCodeBench with its hidden tests ([#28](https://github.com/inferstep/ATLAS/issues/28)).
- Models: automated HuggingFace submission ([#102](https://github.com/inferstep/ATLAS/issues/102)); model-agnostic backends ([#66](https://github.com/inferstep/ATLAS/issues/66)).

---

## ❤️ Support ATLAS

ATLAS is built by a single college student in his free time on a single consumer GPU. If the project has been useful to you and you want to help keep it sustainable, please consider **[sponsoring on GitHub](https://github.com/sponsors/itigges22)**.

Sponsorship directly funds:

- **Compute & hardware** - more GPUs for faster benchmark iteration, access to architectures the maintainer can't afford (AMD ROCm, higher VRAM cards, cloud rentals for larger-model experiments).
- **Contributor bounties** - meaningful compensation for external contributors who put real time into substantive PRs, so ATLAS can grow faster than a single-person pace allows.
- **Research** - continued academic engagement around the architecture, from future workshop and conference submissions to paper writing and collaborations that validate and extend the approach.
- **Community** - continued support for the community and platforms ATLAS runs on, including documentation, user-facing channels, and educational content that help ATLAS reach more developers and better serve the ones already using it.

Every sponsor is credited in the release notes of the version they helped fund.

---

## 🤝 Contributing

ATLAS is developed in the open and welcomes contributors and core maintainers. Bug fixes, accelerator support, and larger subsystem work are all welcome.

Found a bug or hit a wall? **[Open an issue](https://github.com/inferstep/ATLAS/issues)** - you don't need to submit a fix. Bug reports and feedback help just as much as code.

See **[CONTRIBUTING.md](CONTRIBUTING.md)** for guidelines and the [repository map](docs/README.md#repository-map) for an overview of the codebase layout.

---

## 📄 License

Licensed under the [GNU Affero General Public License v3.0 (AGPL-3.0)](LICENSE).
