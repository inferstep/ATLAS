# ATLAS Documentation

Task-oriented index for everything under `docs/`. For a directory-by-directory
map of the repository itself, see [MAP.md](MAP.md).

> 翻訳 / 번역 / 翻译: [简体中文](lang/zh-CN/README.md) ·
> [日本語](lang/ja/README.md) · [한국어](lang/ko/README.md)
> (README, SETUP, ARCHITECTURE, TROUBLESHOOTING are translated; everything
> else is English-only.)

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
- [EVIDENCE_WIRE.md](EVIDENCE_WIRE.md) — the versioned evidence envelope
  between v3-service and the proxy, and the rule that authorizes delivery.
- [CANDIDATE_AUTHORIZATION.md](CANDIDATE_AUTHORIZATION.md) — typed
  obligations, evidence provenance, candidate staging, and the observe-only
  authorization and feasibility decisions.
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
- [reports/V3_ABLATION_STUDY.md](reports/V3_ABLATION_STUDY.md) — the
  V3.0 ablation report; its 74.6% result is withdrawn (notice at the top).
- [reports/CALL_GRAPH_REASONING_V3.md](reports/CALL_GRAPH_REASONING_V3.md)
  — structural call-graph reasoning design notes.
- [adr/](adr/README.md) — architecture decision records 0001-0011 (trust
  model, Redis, per-model bundles, fail-soft V3, lens optionality
  (superseded), release strategy, the SQLite store (retired), harness
  mechanisms, embedding convention, lens capacity, the lens is required).
- [STORY.md](STORY.md) — why this project exists.

### Contribute

- [../CONTRIBUTING.md](../CONTRIBUTING.md) — from finding an issue and
  `/claim` to review and release; style, tests, and the developer quality
  gate (`scripts/production-readiness.py`).
- [DEVELOPMENT.md](DEVELOPMENT.md) — dev mode, targeted rebuilds,
  running the proxy on the host against the compose stack.
- [quality/gates.md](quality/gates.md) — the checks on a pull request,
  which of them are required, how the outside tools are set, and the
  measured baselines.
- [RELEASE.md](RELEASE.md) — the release contract, verification levels,
  versioning, and the release and hotfix steps.
- [TRIAGE.md](TRIAGE.md) — how new issues become Ready work.
- [INCIDENT_RESPONSE.md](INCIDENT_RESPONSE.md) — what to do when a key
  leaks, a release is bad, or automation misbehaves.
- [CONTAINER_PACKAGING.md](CONTAINER_PACKAGING.md) — image accounts,
  writable dirs, dependency pinning.
- [../GOVERNANCE.md](../GOVERNANCE.md), [../MAINTAINERS.md](../MAINTAINERS.md),
  [../SECURITY.md](../SECURITY.md),
  [../CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md)

---

## Shortcuts by situation

**New to ATLAS.** [GETTING_STARTED.md](GETTING_STARTED.md), which walks the
whole first hour; keep [TROUBLESHOOTING.md](TROUBLESHOOTING.md) open in a tab.

**Know what you need.** Use the section index above, or
[MAP.md](MAP.md) if you're looking for code rather than docs.

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
2. [STORY.md](STORY.md) — background
3. [GETTING_STARTED.md](GETTING_STARTED.md) — the first hour
4. [SETUP.md](SETUP.md) / [SETUP_MACOS.md](SETUP_MACOS.md) — install
5. [CLI.md](CLI.md) — the surface you actually touch
6. [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — failure modes and diagnosis
7. [CONFIGURATION.md](CONFIGURATION.md) — every knob, per service
8. [ARCHITECTURE.md](ARCHITECTURE.md) — how the pieces fit
9. [PLAN_MODE.md](PLAN_MODE.md) — pre-flight planning
10. [PROTOCOL.md](PROTOCOL.md) — the event contract
11. [API.md](API.md) — the full HTTP surface
12. [reports/V3_ABLATION_STUDY.md](reports/V3_ABLATION_STUDY.md) — historical V3.0 ablation report (result withdrawn)
13. [reports/CALL_GRAPH_REASONING_V3.md](reports/CALL_GRAPH_REASONING_V3.md)
14. [SOURCES.md](SOURCES.md) — the research it stands on
15. [adr/](adr/README.md) — decisions 0001 through 0010, in order (0007 retired)
16. [../SUPPORT_MATRIX.md](../SUPPORT_MATRIX.md) — claims and their evidence
17. [OPERATIONS.md](OPERATIONS.md) — running it long-term
18. [DEVELOPMENT.md](DEVELOPMENT.md), [../CONTRIBUTING.md](../CONTRIBUTING.md),
    [RELEASE.md](RELEASE.md), [PUBLISHING.md](PUBLISHING.md),
    [CONTAINER_PACKAGING.md](CONTAINER_PACKAGING.md) — working on it
19. [../CHANGELOG.md](../CHANGELOG.md) — how it got here
