# ATLAS Proxy

Local inference proxy that hosts the structured agent endpoint
(`/v1/agent`), the typed event broker (`/events`, PC-061), and the
cancel hook (`/cancel`, PC-062) the TUI drives. Plain OpenAI traffic
on `/v1/chat/completions` and unmatched paths pass through to
llama-server unchanged.

## Architecture

```
TUI ─┐
     ├─→ ATLAS Proxy (:8090) ──┬─→ llama-server  (:8080)
CLI ─┘                          ├─→ V3 service    (:8070)
                                ├─→ Lens          (:8099)
                                └─→ Sandbox       (:30820)
```

## Agent loop (every turn)

Each user message drives an agent loop that runs until the model emits
`{"type":"done"}` or hits the turn cap:

1. **Pre-flight plan** (PC-179 / PC-206) — `v3-service` `/v3/plan` is
   called to seed an explicit step list. 3 candidates sampled, scored
   heuristically, best plan pinned. The active step is injected into
   the system prompt every turn (`gates.go`).
2. **Grammar-constrained generation** — `llama-server` is strongly steered
   toward a JSON envelope: `tool_call`, `text`, or `done`. GBNF +
   `response_format: json_object` constrains decoding, while the proxy recovers
   malformed or truncated output and treats parsing as fallible.
3. **Tool dispatch + validation** — 14 tools (`read_file`,
   `outline_file`, `search_files`, `list_directory`, `find_file`,
   `write_file`, `edit_file`, `structural_edit`, `delete_file`, `move_file`,
   `run_command`, `run_background`, `tail_background`,
   `stop_background`). Per-tool
   guardrails: read-tracking, mtime checks, default-deny patterns,
   suspicious-shrinkage guard (`guardrails.go`).
4. **V3 routing for T2+ writes** — when the file being written
   qualifies (≥ 10 lines with logic indicators, or a recognized
   code/markup extension — `classifyFileTier` in `tools.go`), the
   write is offloaded to `v3-service` and the bridge re-emits each
   pipeline stage onto `/events` as a `v3:<stage>` envelope.
5. **Adherence + stuck-pattern gates** — every turn is scored against
   the active plan step (`gates.go`) and three stuck-pattern
   detectors (tool/reasoning repetition in `detectors.go`, claim
   checking in `gates.go`). Auto-revise the plan after a configurable
   threshold; bail if the loop is genuinely stuck.

## Tier Classification

| Tier | Description | Treatment |
|------|-------------|-----------|
| T0 | Conversational (hi, thanks) | Agent loop capped at 5 turns; no plan mode; text response only |
| T1 | Config/data/style/prose files, or under 10 lines | Tool calls executed directly. No V3 offload. |
| T2 | ≥ 10 lines with logic indicators, or a recognized code/markup extension | `write_file` / `edit_file` may route through V3 (PlanSearch / DivSampling / Budget Forcing / PR-CoT / Refinement, lens candidate selection). |
| T3 | Escalated from cyclomatic complexity (CC ≥ 16) | Same V3 treatment as T2. |

## Usage

```bash
# Start all services and launch the TUI
atlas

# Or run the proxy standalone
atlas-proxy-v2                          # listens on :8090
```

## Configuration

| Env Var | Default | Description |
|---------|---------|-------------|
| ATLAS_INFERENCE_URL | http://localhost:8080 | llama-server inference URL |
| ATLAS_LLAMA_URL | (= ATLAS_INFERENCE_URL) | Override for llama-server target |
| ATLAS_LENS_URL | http://localhost:8099 | Geometric Lens API (C(x)+G(x) scoring) |
| ATLAS_SANDBOX_URL | http://localhost:30820 | Code execution sandbox |
| ATLAS_V3_URL | http://localhost:8070 | V3 pipeline service |
| ATLAS_PROXY_PORT | 8090 | Proxy listen port |
| ATLAS_MODEL_NAME | local-model | Neutral fallback request identifier; normal installs set the selected model |
| ATLAS_WORKSPACE_DIR | (cwd) | Workspace root for read/write tools |

## Build

```bash
cd proxy && go build -o ~/.local/bin/atlas-proxy-v2 .
```
