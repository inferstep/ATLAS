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

- [GETTING_STARTED.md](GETTING_STARTED.md) — the beginner path: reality-check
  your hardware, install, verify, and run a first task.
- [SETUP.md](SETUP.md) — all install paths: one-shot bootstrap, Docker
  Compose, bare metal, K3s. Start at
  [§ Pick your install path](SETUP.md#pick-your-install-path).
- [SETUP_MACOS.md](SETUP_MACOS.md) — Apple Silicon (native Metal
  llama-server + Docker for the rest).
- Sizing questions before you download anything:
  [SETUP.md § Supported GPUs](SETUP.md#supported-gpus) and
  [TROUBLESHOOTING.md § What fits on my GPU?](TROUBLESHOOTING.md#what-fits-on-my-gpu)
- What is and isn't supported, with evidence:
  [SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md)

### Use ATLAS day to day

- [CLI.md](CLI.md) — the TUI and every `atlas` subcommand: panes,
  [slash commands](CLI.md#slash-commands), permission modes,
  `atlas model` / `tier fit` / `onboard` / `bench` / `lens` / `asa`.
- [CONFIGURATION.md](CONFIGURATION.md) — every environment variable and
  internal constant, per service; includes
  [adding your own model](CONFIGURATION.md#adding-your-own-model-drop-in--unregistered).
- [OPERATIONS.md](OPERATIONS.md) — health, logs, runbooks, upgrade,
  rollback, backup/restore.
- [PUBLISHING.md](PUBLISHING.md) — share trained Lens / ASA artifacts
  (HuggingFace upload + registry PR).

### Integrate with ATLAS

- [API.md](API.md) — HTTP reference for all five services (proxy,
  v3-service, geometric-lens, sandbox, llama-server).
- [PROTOCOL.md](PROTOCOL.md) — the typed SSE event envelope shared by
  proxy, v3-service, and clients.
- [CANDIDATE_AUTHORIZATION.md](CANDIDATE_AUTHORIZATION.md) — typed
  obligations, evidence provenance, the evidence wire between v3-service and
  the proxy, candidate staging, and the observe-only authorization and
  feasibility decisions.
- [CANDIDATE_POLICY.md](CANDIDATE_POLICY.md) — the one delivery rule for
  V3 candidates: what can honestly be decided without an oracle.
- [schemas/](schemas/) — machine-readable contracts:
  [proxy_openapi.yaml](schemas/proxy_openapi.yaml),
  [error_envelope.schema.json](schemas/error_envelope.schema.json),
  [sse_envelope.schema.json](schemas/sse_envelope.schema.json).

### Fix a problem

1. [TROUBLESHOOTING.md § Quick Diagnostics](TROUBLESHOOTING.md#quick-diagnostics)
   — find which service is unhappy in three commands.
2. [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — issues organized by
   service (Docker/GPU, llama-server, proxy, lens, sandbox, benchmarks),
   with an exact-error index up top.
3. `atlas doctor` for a one-shot health report, and
   `atlas diagnostics collect` for a shareable, redacted support bundle
   (both documented in [CLI.md](CLI.md)).
4. Still stuck? [Open an issue](https://github.com/inferstep/ATLAS/issues)
   — paste the doctor output.

### Understand how it works

- [ARCHITECTURE.md](ARCHITECTURE.md) — the two-layer design: outer agent
  loop, inner V3 pipeline, Geometric Lens, sandbox.
- [PLAN_MODE.md](PLAN_MODE.md) — per-turn pre-flight planning.
- [SOURCES.md](SOURCES.md) — the research papers behind each component.
- [reports/ablation/README.md](reports/ablation/README.md) — the raw
  per-task traces behind the V3.0 ablation study (LiveCodeBench, 599 tasks),
  whose result is withdrawn.
- [adr/](adr/README.md) — architecture decision records 0001-0011 (trust
  model, Redis, per-model bundles, fail-soft V3, lens optionality
  (superseded), release strategy, the SQLite store (retired), harness
  mechanisms, embedding convention, lens capacity, the lens is required).

### Contribute

- [../CONTRIBUTING.md](../CONTRIBUTING.md) — from finding an issue and
  `/claim` to review and release; style, tests, and the developer quality
  gate (`scripts/production-readiness.py`).
- [CONTRIBUTING.md § Development workflow](../CONTRIBUTING.md#development-workflow) — dev mode, targeted rebuilds,
  running the proxy on the host against the compose stack.
- [CODE_STYLE.md](CODE_STYLE.md) — how ATLAS code should read; what
  `scripts/code_health.py` enforces on new code and refactors.
- [EVAL_INTERFACE.md](EVAL_INTERFACE.md) — the held-out evaluation
  contract between the evaluation session and the development side.
- [quality/gates.md](quality/gates.md) — the checks on a pull request,
  which of them are required, how the outside tools are set, and the
  measured baselines.
- [RELEASE.md](RELEASE.md) — the release contract, verification levels,
  versioning, and the release and hotfix steps.
- [GOVERNANCE.md § Triage](../GOVERNANCE.md#triage) — how new issues become Ready work.
- [INCIDENT_RESPONSE.md](INCIDENT_RESPONSE.md) — what to do when a key
  leaks, a release is bad, or automation misbehaves.
- [CONTAINER_PACKAGING.md](CONTAINER_PACKAGING.md) — image accounts,
  writable dirs, dependency pinning.
- [../GOVERNANCE.md](../GOVERNANCE.md), [../MAINTAINERS.md](../MAINTAINERS.md),
  [../SECURITY.md](../SECURITY.md),
  [../CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md), [../LICENSE](../LICENSE)
- READMEs beside code and [../THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md)
  are not listed one-by-one here: they live in the tree next to what they
  describe (see the [repository map](#repository-map)), and
  third-party notices live at the repository root.

---

## Shortcuts by situation

**New to ATLAS.** [GETTING_STARTED.md](GETTING_STARTED.md), which walks the
whole first hour; keep [TROUBLESHOOTING.md](TROUBLESHOOTING.md) open in a tab.

**Know what you need.** Use the section index above, or
the [repository map](#repository-map) if you're looking for code rather than docs.

**Something's broken.**
[TROUBLESHOOTING.md § Quick Diagnostics](TROUBLESHOOTING.md#quick-diagnostics)
→ the per-service section for whichever health field is `false` →
`atlas doctor` → open an issue with the output.

**Want to read everything.** Follow the
[start-to-finish reading order](#read-atlas-start-to-finish) below.

---

## Core terms (one line each)

- **Direct agent / outer loop** — the Go proxy's tool-calling loop; works
  with any GGUF, no per-model training ([ARCHITECTURE.md](ARCHITECTURE.md)).
- **V3 pipeline / inner layer** — multi-candidate generation, scoring,
  sandbox verification, and repair for non-trivial files
  ([ARCHITECTURE.md](ARCHITECTURE.md)).
- **Tiers (T0–T3)** — two separate classifications. The per-file tier gates
  V3: T1 writes directly, T2/T3 use the pipeline. The per-message tier only
  distinguishes T0 (conversational: 5-turn cap, no plan) from everything
  else, since the turn cap and plan gate are all it feeds
  ([ARCHITECTURE.md](ARCHITECTURE.md)).
- **Geometric Lens, C(x) / G(x)** — energy-based candidate scoring over the
  model's own embeddings; per-model trained bundle
  ([ARCHITECTURE.md](ARCHITECTURE.md), [../SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md)).
- **ASA** — activation-steering control vector nudging tool selection;
  per-model, opt-in until validated ([CLI.md](CLI.md), [PUBLISHING.md](PUBLISHING.md)).
- **Sandbox** — isolated multi-language execution used for verification
  ([ARCHITECTURE.md](ARCHITECTURE.md), [API.md](API.md)).
- **GBNF grammar enforcement** — token-level constrained decoding that strongly
  steers tool calls toward the expected JSON schema, with proxy-side recovery
  for malformed or truncated output ([ARCHITECTURE.md](ARCHITECTURE.md)).

---

## Read ATLAS start to finish

In order, each building on the last:

1. [../README.md](../README.md) — what and why
2. [GETTING_STARTED.md](GETTING_STARTED.md) — the first hour
3. [SETUP.md](SETUP.md) / [SETUP_MACOS.md](SETUP_MACOS.md) — install
4. [CLI.md](CLI.md) — the surface you actually touch
5. [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — failure modes and diagnosis
6. [CONFIGURATION.md](CONFIGURATION.md) — every knob, per service
7. [ARCHITECTURE.md](ARCHITECTURE.md) — how the pieces fit
8. [PLAN_MODE.md](PLAN_MODE.md) — pre-flight planning
9. [PROTOCOL.md](PROTOCOL.md) — the event contract
10. [API.md](API.md) — the full HTTP surface
11. [SOURCES.md](SOURCES.md) — the research it stands on
12. [adr/](adr/README.md) — decisions 0001 through 0011, in order (0007 retired)
13. [../SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md) — claims and their evidence
14. [OPERATIONS.md](OPERATIONS.md) — running it long-term
15. [../CONTRIBUTING.md](../CONTRIBUTING.md) (its Development workflow),
    [RELEASE.md](RELEASE.md), [PUBLISHING.md](PUBLISHING.md),
    [CONTAINER_PACKAGING.md](CONTAINER_PACKAGING.md) — working on it
16. [../CHANGELOG.md](../CHANGELOG.md) — how it got here

---

## Repository map

What each top-level folder owns. For how the pieces work together see
[ARCHITECTURE.md](ARCHITECTURE.md); for the full API surface see [API.md](API.md).

| Directory | Owns | More |
|---|---|---|
| `proxy/` | Go agent loop: `/v1/agent`, grammar-constrained tool calls, gates, V3 routing, OpenAI passthrough | [proxy/README.md](../proxy/README.md) |
| `tui/` | Bubbletea terminal client — the canonical chat front-end, consumes proxy SSE streams | — |
| `extensions/` | IDE clients over the proxy HTTP API; `extensions/vscode/` is the VS Code extension (chat sidebar, permission flow, diff review) and `extensions/jetbrains/` the JetBrains plugin (chat and permission cards in the tool window, diff review for predicted and applied file changes, the workspace-mismatch checks, a proxy status bar, model/V3 progress, notifications, actions and settings with the token in the IDE credential store; verified by Plugin Verifier on IDEA, PyCharm, WebStorm and GoLand) | [extensions/vscode/README.md](../extensions/vscode/README.md), [extensions/jetbrains/README.md](../extensions/jetbrains/README.md) |
| `atlas/` | Python CLI package: `atlas` subcommand dispatch (init, doctor, tier, model, onboard, lens, asa, publish, bench, compose, tui, upgrade, rollback, diagnostics, artifact, config); `atlas/bench/` is the benchmark harness | [atlas/bench/README.md](../atlas/bench/README.md) |
| `v3-service/` | Python HTTP service for the V3 generation pipeline (`stages/` holds the pipeline stage modules, `graph/` the call-graph layer) and the tree-sitter structural tooling | — |
| `geometric-lens/` | Scoring (C(x)/G(x)), ASA control-vector build | [asa_calibration/README.md](../geometric-lens/asa_calibration/README.md) |
| `sandbox/` | Isolated multi-language code execution and shell, with workspace containment | — |
| `inference/` | llama-server Docker builds (CUDA / ROCm / Vulkan) and model-neutral entrypoints | — |
| `benchmark/` | Created at runtime, not tracked — dataset caches and run results. The runner/harness code is `atlas/bench/` | — |
| `scripts/` | Install, deploy, K3s, and CI/release automation | — |
| `.github/` | CI workflows, `CODEOWNERS`, `dependabot.yml`, `allowed_signers` for signed release tags, issue templates | — |
| `templates/` | K3s manifest templates rendered from `atlas.conf` via envsubst | — |
| `tests/` | Cross-cutting test suite: `cli/`, `concurrency/`, `contracts/` (doc-link, version and config gates), `e2e/` (the acceptance suite CI runs), `infrastructure/`, `integration/`, `perf/`, `v3/`, `v3-service/` | — |

Root also holds the Docker Compose stack (`docker-compose.yml` plus the rocm /
vulkan / cpu / macos overlays), `atlas.conf.example` (K3s), `pyproject.toml`
(the `atlas` CLI entry point), and the standard `.env.example`.
