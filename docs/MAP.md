# ATLAS Repository Map

Orientation map for the repository: what each top-level directory owns and where
the documentation lives. For component internals see
[ARCHITECTURE.md](ARCHITECTURE.md); for the full API surface see
[API.md](API.md).

---

## Top-level directories

| Directory | Owns | More |
|---|---|---|
| `proxy/` | Go agent loop: `/v1/agent`, grammar-constrained tool calls, gates, V3 routing, OpenAI passthrough | [proxy/README.md](../proxy/README.md) |
| `tui/` | Bubbletea terminal client — the canonical chat front-end, consumes proxy SSE streams | — |
| `extensions/` | IDE clients over the proxy HTTP API; `extensions/vscode/` is the VS Code extension (chat sidebar, permission flow, diff review) and `extensions/jetbrains/` the JetBrains plugin (Stage 2: the proxy protocol/client layer, not yet wired to the tool window) | [extensions/vscode/README.md](../extensions/vscode/README.md), [extensions/jetbrains/README.md](../extensions/jetbrains/README.md) |
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

---

## Documentation

| Doc | Purpose |
|---|---|
| [README.md](README.md) | Task-oriented index of everything under `docs/` — the entry point |
| [GETTING_STARTED.md](GETTING_STARTED.md) | First-run walkthrough: bootstrap, first session, the slash commands |
| [ARCHITECTURE.md](ARCHITECTURE.md) | Two-layer architecture, component breakdowns, data-flow and sequence diagrams |
| [API.md](API.md) | HTTP API reference for every service |
| [CLI.md](CLI.md) | CLI + TUI usage, subcommands, and workflow examples |
| [CONFIGURATION.md](CONFIGURATION.md) | Environment variables, internal constants, and K3s config |
| [SETUP.md](SETUP.md) | Installation: one-shot bootstrap, Docker Compose, K3s |
| [SETUP_MACOS.md](SETUP_MACOS.md) | macOS hybrid install: native Metal llama-server + Docker stack |
| [OPERATIONS.md](OPERATIONS.md) | Day-2 operations: health, logs, runbooks, upgrades, rollback, backup |
| [CONTAINER_PACKAGING.md](CONTAINER_PACKAGING.md) | How the service images are built: runtime accounts, writable paths, dependency pinning |
| [DEVELOPMENT.md](DEVELOPMENT.md) | Dev workflow: targeted rebuilds, host-side proxy, tests |
| [PUBLISHING.md](PUBLISHING.md) | HuggingFace + GitHub publish flow for Lens / ASA artifacts |
| [RELEASE.md](RELEASE.md) | Release contract: capability status, service contracts, and verification levels |
| [PLAN_MODE.md](PLAN_MODE.md) | Plan mode: per-turn pre-flight planning and adherence constants |
| [PROTOCOL.md](PROTOCOL.md) | Typed event envelope contract shared by proxy, v3-service, and clients |
| [EVIDENCE_WIRE.md](EVIDENCE_WIRE.md) | Versioned evidence envelope between v3-service and the proxy, and the delivery rule |
| [CANDIDATE_AUTHORIZATION.md](CANDIDATE_AUTHORIZATION.md) | Typed obligations, evidence provenance, candidate staging, and the observe-only authorization and feasibility decisions |
| [CANDIDATE_POLICY.md](CANDIDATE_POLICY.md) | The one rule that decides which V3 candidate lands, the proposal-versus-authorization split, hard vetoes, and what the terminal tells the user about delivered bytes |
| [TROUBLESHOOTING.md](TROUBLESHOOTING.md) | Common issues and solutions |
| [SOURCES.md](SOURCES.md) | Research papers bucketed by status relative to the current release |
| [STORY.md](STORY.md) | Project background |
| [MAP.md](MAP.md) | This file |
| [quality/gates.md](quality/gates.md) | The checks on a pull request, which are required, tool settings, and the measured baselines |
| [adr/](adr/) | Architecture Decision Records (trust model, per-model bundles, the retired SQLite state store, harness mechanisms, …) |
| [schemas/](schemas/) | Machine-readable contracts: proxy OpenAPI spec, SSE envelope and error-envelope JSON Schemas |
| [lang/](lang/) | Translated documentation (zh-CN, ja, ko) |
| [reports/](reports/) | Ablation studies and call-graph design notes; [reports/archive/](reports/archive/) holds historical status trackers |

Root docs: [README.md](../README.md), [CHANGELOG.md](../CHANGELOG.md),
[SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md), [SECURITY.md](../SECURITY.md),
[GOVERNANCE.md](../GOVERNANCE.md), [MAINTAINERS.md](../MAINTAINERS.md),
[THIRD_PARTY_NOTICES.md](../THIRD_PARTY_NOTICES.md),
[CONTRIBUTING.md](../CONTRIBUTING.md),
[CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md), and [LICENSE](../LICENSE).
