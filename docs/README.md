# ATLAS Documentation

Task-oriented index for everything under `docs/`. The
[repository map](#repository-map) at the end says what each top-level folder owns.

> 翻訳 / 번역 / 翻译: [简体中文](lang/zh-CN/README.md) ·
> [日本語](lang/ja/README.md) · [한국어](lang/ko/README.md)
> (README, SETUP, ARCHITECTURE and TROUBLESHOOTING are translated, and
> GETTING_STARTED in zh-CN only; everything else is English-only.)

---

## What are you trying to do?

### Install ATLAS

- [GETTING_STARTED.md](guides/GETTING_STARTED.md) — the beginner path: reality-check
  your hardware, install, verify, and run a first task.
- [SETUP.md](guides/SETUP.md) — all install paths: one-shot bootstrap, Docker
  Compose, bare metal, K3s. Start at
  [§ Pick your install path](guides/SETUP.md#pick-your-install-path).
- [SETUP_MACOS.md](guides/SETUP_MACOS.md) — Apple Silicon (native Metal
  llama-server + Docker for the rest).
- Sizing questions before you download anything:
  [SETUP.md § Supported GPUs](guides/SETUP.md#supported-gpus) and
  [TROUBLESHOOTING.md § What fits on my GPU?](guides/TROUBLESHOOTING.md#what-fits-on-my-gpu)
- What is and isn't supported, with evidence:
  [SUPPORT_MATRIX.md](guides/SUPPORT_MATRIX.md)

### Use ATLAS day to day

- [CLI.md](reference/CLI.md) — the TUI and every `atlas` subcommand: panes,
  [slash commands](reference/CLI.md#slash-commands), permission modes,
  `atlas model` / `tier fit` / `onboard` / `bench` / `lens` / `asa`.
- [CONFIGURATION.md](guides/CONFIGURATION.md) — every environment variable and
  internal constant, per service; includes
  [adding your own model](guides/CONFIGURATION.md#adding-your-own-model-drop-in--unregistered).
- [OPERATIONS.md](guides/OPERATIONS.md) — health, logs, runbooks, upgrade,
  rollback, backup/restore.
- [PUBLISHING.md](project/PUBLISHING.md) — share trained Lens / ASA artifacts
  (HuggingFace upload + registry PR).

### Integrate with ATLAS

- [API.md](reference/API.md) — HTTP reference for all five services (proxy,
  v3-service, geometric-lens, sandbox, llama-server), and the typed SSE
  event envelope shared by proxy, v3-service, and clients.
- [CANDIDATE_POLICY.md](reference/CANDIDATE_POLICY.md) — the one delivery rule for
  V3 candidates: what can honestly be decided without an oracle. Also typed
  obligations, evidence provenance, the evidence wire between v3-service and
  the proxy, candidate staging, and the observe-only authorization and
  feasibility decisions.
- [schemas/](reference/schemas/) — machine-readable contracts:
  [proxy_openapi.yaml](reference/schemas/proxy_openapi.yaml),
  [error_envelope.schema.json](reference/schemas/error_envelope.schema.json),
  [sse_envelope.schema.json](reference/schemas/sse_envelope.schema.json).

### Fix a problem

1. [TROUBLESHOOTING.md § Quick Diagnostics](guides/TROUBLESHOOTING.md#quick-diagnostics)
   — find which service is unhappy in three commands.
2. [TROUBLESHOOTING.md](guides/TROUBLESHOOTING.md) — issues organized by
   service (Docker/GPU, llama-server, proxy, lens, sandbox, benchmarks),
   with an exact-error index up top.
3. `atlas doctor` for a one-shot health report, and
   `atlas diagnostics collect` for a shareable, redacted support bundle
   (both documented in [CLI.md](reference/CLI.md)).
4. Still stuck? [Open an issue](https://github.com/inferstep/ATLAS/issues)
   — paste the doctor output.

### Understand how it works

- [ARCHITECTURE.md](reference/ARCHITECTURE.md) — the two-layer design: outer agent
  loop, inner V3 pipeline, Geometric Lens, sandbox.
- [ARCHITECTURE.md § Plan Mode](reference/ARCHITECTURE.md#plan-mode-per-turn-pre-flight) — per-turn pre-flight planning.
- [SOURCES.md](reference/SOURCES.md) — the research papers behind each component.
- [reports/ablation/README.md](reports/ablation/README.md) — the raw
  per-task traces behind the V3.0 ablation study (LiveCodeBench, 599 tasks),
  whose result is withdrawn.
- [adr/](adr/README.md) — architecture decision records 0001-0011 (trust
  model, Redis, per-model bundles, fail-soft V3, lens optionality
  (superseded), release strategy, the SQLite store (retired), harness
  mechanisms, embedding convention, lens capacity, the lens is required).

### Contribute

- [../.github/CONTRIBUTING.md](../.github/CONTRIBUTING.md) — from finding an issue and
  `/claim` to review and release; style, tests, and the developer quality
  gate (`scripts/ci/production-readiness.py`).
- [CONTRIBUTING.md § Development workflow](../.github/CONTRIBUTING.md#development-workflow) — dev mode, targeted rebuilds,
  running the proxy on the host against the compose stack.
- [CODE_STYLE.md](project/CODE_STYLE.md) — how ATLAS code should read; what
  `scripts/ci/code_health.py` enforces on new code and refactors.
- [scripts/eval/README.md](../scripts/eval/README.md) — the held-out evaluation
  contract between the evaluation side and the development side.
- [project/gates.md](project/gates.md) — the checks on a pull request,
  which of them are required, how the outside tools are set, and the
  measured baselines.
- [RELEASE.md](project/RELEASE.md) — the release contract, verification levels,
  versioning, and the release and hotfix steps.
- [GOVERNANCE.md § Triage](../.github/GOVERNANCE.md#triage) — how new issues become Ready work.
- [INCIDENT_RESPONSE.md](project/INCIDENT_RESPONSE.md) — what to do when a key
  leaks, a release is bad, or automation misbehaves.
- [CONTAINER_PACKAGING.md](project/CONTAINER_PACKAGING.md) — image accounts,
  writable dirs, dependency pinning.
- [GOVERNANCE.md](../.github/GOVERNANCE.md), [MAINTAINERS.md](project/MAINTAINERS.md),
  [../.github/SECURITY.md](../.github/SECURITY.md),
  [CODE_OF_CONDUCT.md](../.github/CODE_OF_CONDUCT.md), [../LICENSE](../LICENSE)
- [THIRD_PARTY_NOTICES.md](project/THIRD_PARTY_NOTICES.md) — the third-party work
  ATLAS uses and its licenses.
- READMEs beside code are not listed one-by-one here: they live in the tree
  next to what they describe (see the [repository map](#repository-map)).

---

## Shortcuts by situation

**New to ATLAS.** [GETTING_STARTED.md](guides/GETTING_STARTED.md), which walks the
whole first hour; keep [TROUBLESHOOTING.md](guides/TROUBLESHOOTING.md) open in a tab.

**Know what you need.** Use the section index above, or
the [repository map](#repository-map) if you're looking for code rather than docs.

**Something's broken.**
[TROUBLESHOOTING.md § Quick Diagnostics](guides/TROUBLESHOOTING.md#quick-diagnostics)
→ the per-service section for whichever health field is `false` →
`atlas doctor` → open an issue with the output.

**Want to read everything.** Follow the
[start-to-finish reading order](#read-atlas-start-to-finish) below.

---

## Core terms (one line each)

- **Direct agent / outer loop** — the Go proxy's tool-calling loop. A model
  needs a lens bundle first, which `atlas lens build` makes
  ([ARCHITECTURE.md](reference/ARCHITECTURE.md), [ADR 0011](adr/0011-the-lens-is-required.md)).
- **V3 pipeline / inner layer** — multi-candidate generation, scoring,
  sandbox verification, and repair for non-trivial files
  ([ARCHITECTURE.md](reference/ARCHITECTURE.md)).
- **Tiers (T0–T3)** — two separate classifications. The per-file tier gates
  V3: T1 writes directly, T2/T3 use the pipeline. The per-message tier only
  distinguishes T0 (conversational: 12-turn cap, no plan) from everything
  else, since the turn cap and plan gate are all it feeds
  ([ARCHITECTURE.md](reference/ARCHITECTURE.md)).
- **Geometric Lens, C(x) / G(x)** — energy-based candidate scoring over the
  model's own embeddings; per-model trained bundle
  ([ARCHITECTURE.md](reference/ARCHITECTURE.md), [SUPPORT_MATRIX.md](guides/SUPPORT_MATRIX.md)).
- **ASA** — activation-steering control vector nudging tool selection;
  per-model, always on wherever a vector is installed ([CLI.md](reference/CLI.md), [PUBLISHING.md](project/PUBLISHING.md)).
- **Sandbox** — isolated multi-language execution used for verification
  ([ARCHITECTURE.md](reference/ARCHITECTURE.md), [API.md](reference/API.md)).
- **GBNF grammar enforcement** — token-level constrained decoding that strongly
  steers tool calls toward the expected JSON schema, with proxy-side recovery
  for malformed or truncated output ([ARCHITECTURE.md](reference/ARCHITECTURE.md)).

---

## Read ATLAS start to finish

In order, each building on the last:

1. [../README.md](../README.md) — what and why
2. [GETTING_STARTED.md](guides/GETTING_STARTED.md) — the first hour
3. [SETUP.md](guides/SETUP.md) / [SETUP_MACOS.md](guides/SETUP_MACOS.md) — install
4. [CLI.md](reference/CLI.md) — the surface you actually touch
5. [TROUBLESHOOTING.md](guides/TROUBLESHOOTING.md) — failure modes and diagnosis
6. [CONFIGURATION.md](guides/CONFIGURATION.md) — every knob, per service
7. [ARCHITECTURE.md](reference/ARCHITECTURE.md) — how the pieces fit
8. [ARCHITECTURE.md § Plan Mode](reference/ARCHITECTURE.md#plan-mode-per-turn-pre-flight) — pre-flight planning
9. [API.md](reference/API.md) — the full HTTP surface
10. [SOURCES.md](reference/SOURCES.md) — the research it stands on
11. [adr/](adr/README.md) — decisions 0001 through 0011, in order (0007 retired)
12. [SUPPORT_MATRIX.md](guides/SUPPORT_MATRIX.md) — claims and their evidence
13. [OPERATIONS.md](guides/OPERATIONS.md) — running it long-term
14. [../.github/CONTRIBUTING.md](../.github/CONTRIBUTING.md) (its Development workflow),
    [RELEASE.md](project/RELEASE.md), [PUBLISHING.md](project/PUBLISHING.md),
    [CONTAINER_PACKAGING.md](project/CONTAINER_PACKAGING.md) — working on it
16. [../CHANGELOG.md](../CHANGELOG.md) — how it got here

---

## Repository map

Where things live: what each top-level folder owns. For how the pieces work together see
[ARCHITECTURE.md](reference/ARCHITECTURE.md); for the full API surface see [API.md](reference/API.md).

| Directory | Owns | More |
|---|---|---|
| `services/` | The services of the stack, one folder each. `proxy/` is the Go agent loop: `/v1/agent`, grammar-constrained tool calls, gates, V3 routing, OpenAI passthrough; its files are named by subject (`loop_`, `model_`, `tool_`, `gates_`, `candidate_`, `delivery_`, `evidence_` and the like). `inference/` holds the llama-server Docker builds (CUDA / ROCm / Vulkan) and the model-neutral entrypoints. `sandbox/` is isolated multi-language code execution and shell, with workspace containment | [services/proxy/README.md](../services/proxy/README.md) |
| `clients/` | The clients of the stack, one folder each. `tui/` is the Bubbletea terminal client, the canonical chat front-end, which consumes the proxy's SSE streams. `extensions/` holds the IDE clients over the proxy HTTP API: `extensions/vscode/` is the VS Code extension (chat sidebar, permission flow, diff review) and `extensions/jetbrains/` the JetBrains plugin (chat and permission cards in the tool window, diff review for predicted and applied file changes, the workspace-mismatch checks, a proxy status bar, model/V3 progress, notifications, actions and settings with the token in the IDE credential store; verified by Plugin Verifier on IDEA, PyCharm, WebStorm and GoLand) | [clients/extensions/vscode/README.md](../clients/extensions/vscode/README.md), [clients/extensions/jetbrains/README.md](../clients/extensions/jetbrains/README.md) |
| `atlas/` | Python CLI package: `atlas` subcommand dispatch (init, doctor, tier, model, onboard, lens, asa, publish, bench, compose, tui, upgrade, rollback, diagnostics, artifact, config). It is the product's own package and stays at the root, where `pyproject.toml` builds it. `atlas/bench/` is the user-facing `atlas bench` command | [atlas/bench/README.md](../atlas/bench/README.md) |
| `v3-service/` | Python HTTP service for the V3 generation pipeline (`stages/` holds the pipeline stage modules, `graph/` the call-graph layer) and the tree-sitter structural tooling | — |
| `geometric-lens/` | Scoring (C(x)/G(x)), ASA control-vector build | [asa_calibration/README.md](../geometric-lens/asa_calibration/README.md) |
| `benchmark/` | Created at runtime, not tracked — dataset caches and run results. The runner/harness code is `atlas/bench/` | — |
| `deploy/` | How the stack is deployed: `compose/` holds the Docker Compose overlays (rocm, vulkan, cpu, macos) layered on the root `docker-compose.yml`, and the dev-mode override example; `k3s/` holds the K3s manifest templates rendered from `atlas.conf` via envsubst | — |
| `scripts/` | Install, deploy, K3s, and CI/release automation; `scripts/eval/` is the maintainers' evaluation | [scripts/eval/README.md](../scripts/eval/README.md) |
| `.github/` | CI workflows, `CODEOWNERS`, `dependabot.yml`, `allowed_signers` for signed release tags, issue templates, and the community files GitHub reads: the contributor guide (`CONTRIBUTING.md`), the governance page (`GOVERNANCE.md`), the code of conduct and the security policy (`SECURITY.md`) | — |
| `tests/` | The Python tests, by the code they test: `atlas/` (the CLI), `v3-service/`, and by subject: `concurrency/`, `contracts/` (doc-link, version and config gates), `e2e/` (the acceptance suite CI runs), `infrastructure/`, `perf/`. Go tests sit beside their code (`*_test.go` in `services/proxy/` and `clients/tui/`), the extensions' tests in `clients/extensions/vscode/test/` and `clients/extensions/jetbrains/src/test/`, and `geometric-lens/tests/` beside its service, which has its own lock file | — |
| `docs/` | The pages, listed above by what you are trying to do. `guides/` holds installing, configuring, running and fixing ATLAS, and the support matrix; `reference/` holds the API, the CLI, the architecture, the candidate policy and the sources, with the API's machine-readable schemas in `reference/schemas/`; `project/` holds how the project is run, checked and shipped (`gates.md` lists the checks on a pull request), `adr/` the decision records, and `lang/` the ja, ko and zh-CN copies. `reports/` is data: the raw traces of the withdrawn V3.0 ablation result, kept so the withdrawal can be checked. `assets/` holds files the README and the TUI demo read | [adr/README.md](adr/README.md) |
| `diagnostics/` | Where diagnostic captures are written at run time; git holds only a placeholder | [OPERATIONS.md](guides/OPERATIONS.md) |

Root also holds the base Compose file `docker-compose.yml`, `atlas.conf.example` (K3s), `pyproject.toml`
(the `atlas` CLI entry point), and the standard `.env.example`. Dependencies are declared in `pyproject.toml` (the `atlas`
package), `.github/requirements/` (the CI locks), a `requirements*.txt` in each Python service, a `go.mod` in `services/proxy/` and
`clients/tui/`, and the extensions' `package.json` and `build.gradle.kts`.
