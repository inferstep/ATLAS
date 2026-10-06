# ATLAS API Reference

API endpoints for each ATLAS service. All services communicate over HTTP/JSON. Streaming endpoints use Server-Sent Events (SSE).

> **Ports listed are defaults.** All are configurable via environment variables (see [CONFIGURATION.md](CONFIGURATION.md)). The HTTP contracts below are deployment-mode-agnostic — only the host:port differs. Quick reference:
>
> | Service | Docker Compose (host=container) | Bare metal (env-configured) | K3s NodePort (`atlas.conf.example`) |
> |---|---|---|---|
> | atlas-proxy | `8090` | `ATLAS_PROXY_PORT` (default 8090) | `ATLAS_PROXY_NODEPORT` (default 30080) |
> | v3-service | `8070` | `ATLAS_V3_PORT` (default 8070) | `ATLAS_V3_NODEPORT` (default 30070) |
> | geometric-lens | `8099` | `ATLAS_LENS_PORT` (default 8099) | `ATLAS_LENS_NODEPORT` (default 31144) |
> | sandbox | host `30820` → container `8020` | `ATLAS_SANDBOX_PORT` (default 8020) | `ATLAS_SANDBOX_NODEPORT` (default 30820) |
> | llama-server | `8080` | `ATLAS_LLAMA_PORT` (default 8080) | `ATLAS_LLAMA_NODEPORT` (default 32735) |
>
> The K3s manifests are rendered from `templates/*.yaml.tmpl` into `$K8S_DIR/manifests/` by `scripts/install.sh` at install time. See [SETUP.md Method 3](SETUP.md) for the full K3s deployment.

---

## atlas-proxy (Port 8090)

The main entry point. Wraps llama-server with an agent loop, grammar-constrained tool calls, Lens scoring, and sandbox verification.

**This is the public client surface.** The canonical client is [atlas-tui](CLI.md), but the contract below is stable and other front-ends (web UIs, editor plugins, CI bots, custom CLIs) can use it directly.

**Public client API** — the endpoints a front-end drives. The three a minimal client must implement are `/v1/agent`, `/cancel`, and `/v1/permission`; the rest are optional enrichments.

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/v1/agent` | POST | Send a user message, stream back a turn (tool calls, results, tokens, completion) as SSE |
| `/cancel` | POST | Abort an in-flight `/v1/agent` turn by `session_id` |
| `/v1/permission` | POST | Answer a `permission_request` (approve/deny a destructive tool call mid-turn) |
| `/events` | GET | Subscribe to a global typed-envelope event broker — same events the TUI's pipeline pane uses |
| `/v1/calibration/status` | GET | Lens + ASA compat verdict for the loaded model — what the TUI's Pipeline pane badge reads on startup |

**OpenAI compatibility:**

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/v1/chat/completions` | POST | OpenAI-compatible chat completions — a direct passthrough to llama-server for SDK compatibility |
| `/v1/models` | GET | List available models (OpenAI-compatible). `/models` (no `/v1/` prefix) is an alias. |

**Diagnostics:**

| Endpoint | Method | Purpose |
|----------|--------|---------|
| `/health` | GET | Liveness — always 200, `status` reports `"ok"` or `"degraded"` |
| `/ready` | GET | Readiness probe — 200 only when inference, lens scoring (the lens can score: the check `/v1/agent` applies), the sandbox, and v3-service are all healthy; 503 otherwise. Use this for load-balancer / orchestrator health checks; use `/health` for informational status. |
| `/version` | GET | API version, SSE protocol version, and the full error-code set — see [Versioning and error codes](#versioning-and-error-codes) |
| `/workspace` | GET | Host and container workspace paths for the connected proxy — see [GET /workspace](#workspace-path) |

**Catch-all:** any unmatched path is proxied directly to llama-server.

---

### POST /v1/agent

Tool-based agent endpoint. Sends a user message, runs the agent loop (LLM → tool call → tool result → repeat) until the model emits `done` or hits the turn cap, and streams every step back as SSE.

**Request:**
```json
{
  "message": "Add a snake game in Python and verify it runs",
  "working_dir": "/home/me/projects/snake",
  "mode": "default",
  "session_id": "tui-7f3a2c1b"
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `message` | string | (required) | The user's request |
| `working_dir` | string | `"."` | Host-side working directory. Inside the proxy container this is overridden to `ATLAS_WORKSPACE_DIR` (the bind-mount target — `/workspace` by default). The startup wrapper aligns the bind mount to the user's cwd, so writes land in the right place. |
| `mode` | string | `"default"` | Permission mode: `"default"` (asks before `write_file`, `delete_file` and every command: `run_command`, `run_background`, `stop_background`; in-place edits and `move_file` run without asking), `"accept-edits"` (also runs `write_file` without asking; still asks before deletes and commands), `"yolo"` (asks for nothing except `delete_file`, which is always asked about per file) |
| `session_id` | string | `""` | Required for `/cancel` and for the interactive permission prompt (`/v1/permission`). The proxy keys the cancel handle and pending permission requests by this id while the turn is running. **Without a session_id, destructive tool calls in `default`/`accept-edits` mode are denied** (there is no channel to answer the prompt) — unattended clients use `mode:"yolo"` or pre-approve tools via `session_allowed_tools`. |
| `history` | array | `[]` | Optional. Prior-turn `{role, content}` messages (`"user"` / `"assistant"`) the client wants replayed into the conversation before the new message. Capped at the most recent 40 entries. Omit for a single-turn request. |
| `session_allowed_tools` | array | `[]` | Optional. Tool names the user has approved for the whole session (e.g. from an "allow for session" choice). The proxy skips the interactive permission prompt for these. The client re-sends the current list on each turn. `delete_file` is never covered: each deletion is its own `permission_request` with `one_time_only: true`, and a client must not answer one on its own or offer a session-wide answer for it. |
| `task_contract` | object | absent | Optional. What the client knows about the request. `task_mode` (required when the object is sent): `"work"` (the workspace should change) or `"question"` (nothing should change). `expected_outputs`: exact workspace paths the request must produce. `verification`: exact commands that must be run and pass. `candidate_policy`: accepted and ignored, so older clients keep working; one rule decides whether a V3 candidate replaces the model's own write ([CANDIDATE_POLICY.md](CANDIDATE_POLICY.md)). A contract that does not validate (an unknown mode, a path outside the workspace, a `question` that declares outputs or commands) is refused with HTTP 400 rather than dropped. Without a contract, the proxy decides from the message whether a change is required, and no V3 candidate is generated, because no target is grounded. See [ARCHITECTURE.md § Client task mode](ARCHITECTURE.md#client-task-mode). |
| `bypass_v3`, `v3_mode`, `feasibility_mode` | — | — | Removed. V3 runs on every request and feasibility is recorded, never enforced. A request that asks for V3 off (`bypass_v3: true`, `v3_mode` other than `full`) or `feasibility_mode: enforce` is refused with 400; `false`, `full` and `observe` are accepted and change nothing. |
| `disable_fresh_slot` | bool | `false` | Optional. Keeps the pre-warmed KV-cache prefix instead of requesting a fresh slot. Used by `/demo`. |
| `sandbox_subdir` | string | `""` | Optional. Confines the turn to a subdirectory of the workspace (a bare directory name — anything with path separators or traversal is ignored). `/demo` uses one per pane so concurrent sessions don't clobber each other's files. |

**Response:** `text/event-stream` of `data: {...}\n\n` lines. The proxy flushes a `: connected\n\n` SSE comment on connect so clients see HTTP/200 immediately, then emits typed events for the duration of the turn, terminated by `data: [DONE]\n\n`.

**The lens is required** ([ADR 0011](adr/0011-the-lens-is-required.md)). Before any work, the proxy checks that the Geometric Lens can score (the same check as `/ready`; the answer is cached for 5 s). If it cannot, the request gets a plain HTTP 503 error envelope, not a stream: `error` is `dependency_down`, and `detail` says why and to run `atlas doctor`. If the lens stops scoring during the run, the run ends with `done` `status: "failed"`, `reason: "lens_unavailable"`; the tool call that needed the score is answered with `success: false`, and the summary says whether earlier changes are on disk.

#### Event types on `/v1/agent`

Every event has the shape `{"type":"<name>","data":{...}}`. Types in emission order for a typical turn:

| Type | When | Payload |
|------|------|---------|
| `turn_start` | At the start of every agent loop iteration | `turn` (int), `messages` (int), `trimmed` (bool, true if conversation history was trimmed for context window) |
| `llm_call_start` | Before each LLM round-trip | `turn`, `messages`, `prompt_tokens` (estimated, chars/4) |
| `llm_prompt_progress` | Every ~100 ms while llama-server is in prompt-eval (before any decoded token). Real prompt-eval counters come from llama-server's `/slots` endpoint; when `/slots` returns 404/501 the poller keeps emitting with `processed=0` and the chars/4 estimate as `total` — the elapsed timer is still the useful signal. | `processed` (int), `total` (int), `pct` (0–1 float), `elapsed_ms` (int). Stops as soon as `llm_first_token` fires. |
| `llm_first_token` | First streamed delta from llama-server | `prompt_ms` (time-to-first-token in milliseconds) |
| `llm_token` | Each streamed delta | `text` (the delta string — typically a token or two) |
| `llm_call_end` | LLM call finished | `turn`, `tokens` (this call), `total_tokens` (cumulative for the turn), `ms`, `chars`. On error: `error` is added, `chars` is absent, and `tokens` is `0`. |
| `tool_call` | Model emitted a `{"type":"tool_call",...}` JSON | `name` (string), `args` (raw JSON), `turn` |
| `permission_request` | A destructive tool call is awaiting approval in `default`/`accept-edits` mode. The turn pauses until the client answers via `POST /v1/permission` (or the client disconnects/cancels, or the fail-safe timeout denies). | `tool_name` (string), `args` (raw JSON), `message` (human-readable description), `tool_call_id` (string — echo back on `/v1/permission`) |
| `permission_denied` | The pending tool call was not allowed: denied by the client, ended by a disconnect or cancel, timed out, not askable (no `session_id`), or refused by the proxy before it asked | `tool` (the tool name); `reason` whenever no user denied the call: why the proxy refused a `delete_file` before asking, that nobody could be asked, that the request ended, or that nobody answered in time. A denial by the user has no `reason` |
| `tool_result` | Tool finished executing | `tool`, `success` (bool), `data` (raw JSON), `error` (string), `elapsed` (Go duration string, e.g. `"245ms"`) |
| `text` | Model emitted a `{"type":"text","content":"..."}` JSON (conversational reply) | `content` (string) |
| `v3_progress` | V3 pipeline stage that doesn't have a dedicated typed event yet (fallback) | `message` (string) — humanized stage label |
| `v3_llm_start` / `v3_llm_end` | V3's internal LLMAdapter started / finished a call (planner, candidate generation, repair, etc.) | `detail` (string), `call` (int), `tokens` (int, on `llm_end`), `elapsed_ms` (int, on `llm_end`), `max_tokens`, `temperature` (on `llm_start`) |
| `v3_token` | V3's internal LLM streamed a token | `text` (delta string) |
| `v3_reasoning_token` | V3's internal LLM streamed a `reasoning_content` delta. Separate from `reasoning_token` so it targets the V3 streaming row rather than the agent's LLM row. | `text` (delta string) |
| `v3_phase` | V3 phase transition (`phase1`, `phase2`, `phase2_allocated`) | `stage`, `detail`, plus the CxGx allocation on `phase2_allocated`: `k` (candidate count, never below 3), `tier`, `base_tier` (the tier C(x) picked before escalation), `gx_escalation` (int, 0/1/2 tiers added by G(x)), `capped_from` (string, the tier before the wall-clock cap lowered it; empty when uncapped), `reason` (`gated`\|`budget_capped`\|`uncalibrated`) |
| `v3_plansearch` | PlanSearch step (`plansearch`, `plansearch_done`, `plansearch_error`) | `stage`, `detail`, `plans` (int), `candidates` (int, on `_done`), `tokens` (int, on `_done`) |
| `v3_divsampling` | DivSampling step (`divsampling`, `divsampling_done`, `divsampling_error`) | `stage`, `detail`, `slots` (int), `total` (int, on `_done`) |
| `v3_sandbox` | Per-candidate sandbox test (`sandbox_test`, `sandbox_pass`, `sandbox_fail`, `sandbox_done`) | `stage`, `detail`, `index` (int), `elapsed_ms` (int), `energy` (float, on `_pass`), `stderr` (string, first 120 chars on `_fail`), `passed` / `total` (on `_done`) |
| `v3_select` | Candidate selection (`selected`) | `stage`, `detail`, `index` (int), `energy` (float) |
| `v3_lens_per_step` | Per-token lens scoring of a generated candidate. Fires once per candidate. | `stage`, `detail`, `index` (int, candidate index), `source` (`plansearch`\|`divsampling`), `first_off_rails_idx` (int, -1 if none), `gx_score_min` (float), `gx_score_mean` (float), `cx_norm_max` (float), `n_tokens` (int) |
| `v3_lens_veto` | A sandbox-passing candidate was rejected because its `gx_min` fell below the model's severe-quality threshold. The candidate is marked failed and re-enters the Phase-3 repair pool; the energy fallback never returns it. Absent when the Lens is uncalibrated. | `stage`, `detail`, `index` (int, candidate index), `gx_score_min` (float), `first_off_rails_idx` (int, -1 if none) |
| `v3_structural_veto` | A sandbox-passing candidate was rejected because tree-sitter found direct-identifier calls resolving to no local def, import, builtin, or project symbol. The candidate is marked failed and re-enters the Phase-3 repair pool; the energy fallback never returns it. | `stage`, `detail`, `index` (int, candidate index), `n_unresolved` (int), `unresolved_calls` (string[], up to 5), `n_calls_total` (int) |
| `v3_call_chain_context` | Phase-3 repair injected a call-chain context block for the failing function. Informational. | `stage`, `detail`, `function` (string — the failing function name) |
| `symbol_index_injected` | Turn-zero auto-injection of function/class snippets for symbols named in the user message. | `matched` (string[] — matched symbol names), `n_files` (int — project files scanned), `skipped` (int — symbols that didn't resolve) |
| `agent_lens_score` | Lens scored a `write_file` or `edit_file` tool call's content. Fires per write/edit before tool execution. | `tool` (`write_file`\|`edit_file`), `turn` (int), `n_tokens` (int), `first_off_rails_idx` (int, -1 if none), `gx_score_min` (float), `gx_score_mean` (float), `latency_ms` (float) |
| `agent_lens_intervention` | Lens detected consecutive low-quality writes against the model's `low`/`severe` thresholds and queued a corrective for the next LLM call. Absent when calibration is missing. | `turn` (int), `tool` (string), `reason` (string — the corrective injected into ctx.Messages) |
| `agent_repeat_intervention` | Proxy saw the same `(tool_name, args)` signature ≥3× in the last 8 turns and queued a corrective. | `turn` (int), `tool` (string), `reason` (string — the corrective injected into ctx.Messages) |
| `agent_reasoning_intervention` | Proxy saw the model's `reasoning_content` open with the same prefix for ≥3 consecutive turns and queued a corrective. | `turn` (int), `consecutive` (int — how many turns the snippet repeated), `snippet` (string — the normalized opening that triggered), `reason` (string — corrective injected into ctx.Messages) |
| `reasoning_token` | One delta from a model's optional `reasoning_content` stream during SSE chat completion. These tokens are forwarded to the TUI for live display. Distinct from `llm_token` (which carries the JSON tool-call content destined for parse). | `text` (string — single delta of reasoning prose) |
| `reasoning_budget_cut` | The `reasoning_content` stream exceeded the reasoning budget with no content emitted — the proxy cuts the stream and re-prompts. | `reasoning_chars` (int — reasoning chars accumulated when cut) |
| `content_loop_cut` | The proxy detected a verbatim repeating tail in the content stream (the model restating itself in a loop) and cut the stream. | `chars` (int — content chars accumulated when cut) |
| `old_str_cut` | An `edit_file` old_str had stopped matching its target file and kept arriving, so the proxy cut the stream: the call could not succeed. The model is told the file and the line where old_str left it. | `path` (string), `chars` (int — content chars when cut), `old_str_chars` (int), `matched_lines` (int — old_str lines that still matched) |
| `v3_repair` | Phase 3 repair strategy (`phase3`, `pr_cot*`, `refinement*`, `fallback`, `fallback_all_vetoed`) | `stage`, `detail`, `strategy` (string: `pr_cot` / `refinement`), `failing` (int), `iterations` (int, on `refinement_pass`), `tokens` (int, on `_pass`) |
| `v3_probe` | Probe phase events (`probe`, `probe_light`, `probe_error`, `probe_retry`, `probe_failed`, `probe_scored`, `probe_sandbox`, `probe_pass`) | `stage`, `detail` |
| `v3_self_test` | Self-test generation/verify events (`self_test_gen`, `self_test_done`, `self_test_error`, `self_test_skip`, `self_test_verify`) | `stage`, `detail` |
| `v3_plan` | Plan-pipeline progress (`plan_start`, `plan_candidate`, `plan_candidate_scored`, `plan_candidate_unparseable`, `plan_candidate_error`, `plan_selected`, `plan_failed`). Per-token `token`/`llm_start`/`llm_end` events are filtered out at the proxy. | `stage`, `detail`, `index` (int, per-candidate), `score` (float, on `_scored`/`_selected`), `revision` (int, set when fired during a revise) |
| `plan_loaded` | A winning plan has been generated. Fires once after initial generation and again after each revision. Carries the full step list. | `steps` (array of `{id, action, target, why}`), `verify_step` (string id), `rationale` (string), `winning_score` (float), `revision` (int — 0 for initial plan, 1+ for revisions) |
| `plan_adherence` | Emitted after each tool call, indicating whether the call satisfied an outstanding plan step. Off-plan calls (`matched=false`, no `neutral`) accumulate into the off-streak counter that drives auto-revise. | On match: `matched=true`, `step_index`, `step_id`, `step_action`, `satisfied` (steps satisfied so far), `total`. On miss: `matched=false`, `tool`, `off_streak` (consecutive off-plan calls), `satisfied`, `total`. Recon tools (`read_file`, `list_directory`, `find_file`, `search_files`) emit the miss shape plus `neutral=true` — they don't satisfy steps but leave `off_streak` unchanged. |
| `plan_revise` | The off-streak crossed `planAutoReviseThreshold` (5) — a fresh plan is being generated. The next `plan_loaded` (with `revision>0`) supersedes the prior plan; `Satisfied` flags reset. | `reason` (string), `revision` (int, 1-indexed) |
| `done` | The session ended, once per request, whatever the outcome. Only `status: "completed"` means the work was finished; read an absent or unknown `status` as `incomplete`. See [ARCHITECTURE.md § Terminal contract](ARCHITECTURE.md#terminal-contract-and-the-session-budget) | `summary` (string — the server's account; for a `text`-shaped turn it may be empty), `status` (`completed`, `incomplete`, `stopped`, `failed` or `timed_out`; the summary ends with "V3 did not check these files: …" when a file's bytes on disk were written after V3 ran out of time or was unavailable), `reason` (string — why, for example `deliverables_demonstrated` (the deliverables that can run were run), `deliverables_parse_only` (some were only checked to parse), `verification_demanded_unmet`, `claim_check_unresolved`, `lens_unavailable` (the lens stopped scoring; the run stopped)), `unresolved` (comma-separated exit gates whose bounces were spent with their finding still true; present only when there are some — a `completed` run with `unresolved` completed with the caveats its summary names) |
| `error` | LLM/parse/turn-cap error | `error` (string) |

After the final event the server writes the SSE sentinel `data: [DONE]\n\n` and closes the response.

> A minimal chat client needs only `text`, `tool_call`, `tool_result`, `done`, `error`. The `llm_*` and `v3_*` events report model activity during the turn; ignore them if your UI doesn't render progress.

#### Stream parsing example (Python)

```python
import json, requests

with requests.post(
    "http://localhost:8090/v1/agent",
    json={"message": "fix the bug in app.py", "session_id": "client-1"},
    stream=True,
    timeout=(10, None),  # connect timeout 10s, no read timeout — turns can be long
) as r:
    r.raise_for_status()
    for line in r.iter_lines(decode_unicode=True):
        if not line or not line.startswith("data: "):
            continue
        body = line[6:]
        if body == "[DONE]":
            break
        evt = json.loads(body)
        t, d = evt["type"], evt["data"]
        if t == "tool_call":
            print(f"→ {d['name']}({d.get('args', {})})")
        elif t == "tool_result":
            print(f"  {'OK' if d['success'] else 'FAIL'} {d.get('elapsed', '')}")
        elif t == "text":
            print(d["content"])
        elif t == "done":
            # Only "completed" is a finished task; absent or unknown is not.
            if d.get("status") == "completed":
                print(f"✓ {d.get('summary', '')}")
            else:
                print(f"not complete ({d.get('status', 'incomplete')}: "
                      f"{d.get('reason', '')}) {d.get('summary', '')}")
        elif t == "error":
            print(f"✗ {d['error']}")
```

Full streaming chat with token-level rendering is roughly +20 lines (buffer `llm_token` deltas, flush on `llm_call_end`).

---

### POST /cancel

Abort an in-flight `/v1/agent` turn. Idempotent — repeated calls for the same session return 404.

**Request:**
```json
{"session_id": "tui-7f3a2c1b"}
```

**Response (200):**
```json
{"cancelled": true}
```

**Response (404):**
```json
{"cancelled": false}
```

When cancelled, the agent loop exits via `context.Canceled`, the SSE stream emits its trailing `[DONE]`, and the connection closes cleanly. Any in-flight LLM call to llama-server is also aborted via the cascading request context.

The TUI uses this on `Esc` mid-turn — see [CLI.md → Cancelling a turn](CLI.md).

---

### POST /v1/permission

Answer a `permission_request` event. In `default` and `accept-edits` mode the agent loop pauses on a destructive tool call and emits `permission_request`; the turn stays blocked until this endpoint delivers a decision, the client disconnects/cancels, or a fail-safe timeout denies (`ATLAS_PERMISSION_TIMEOUT_SEC`, default 600s). Idempotent — a decision for an unknown or already-resolved request returns 404.

**Request:**
```json
{"session_id": "tui-7f3a2c1b", "tool_call_id": "call_3", "decision": "allow", "scope": "once"}
```

| Field | Type | Description |
|-------|------|-------------|
| `session_id` | string | The `session_id` of the paused `/v1/agent` turn |
| `tool_call_id` | string | The `tool_call_id` from the `permission_request` event |
| `decision` | string | `"allow"` or `"deny"` (anything other than `"allow"` denies) |
| `scope` | string | `"once"` (this call only) or `"session"`. `"session"` additionally skips re-prompting for the same tool for the rest of the turn; the client typically also adds the tool to `session_allowed_tools` on subsequent turns. |

**Deletions are approved once, against the object inspected.** For `delete_file` the prompt describes the exact target (canonical path, type, and for a regular file its content hash), `scope: "session"` is downgraded to this one call, and the approval is spent by the next attempt whatever its outcome. While the prompt is open the proxy holds a kernel reference to the inspected object (Linux `O_PATH`, following nothing, so a symlink is held as the link), and just before removal it checks that the object it holds still has a name and is what the path now refers to, then that the bytes, type, link text and size are unchanged. A file removed and recreated with identical bytes, a link recreated with the same text, an empty directory replaced by another, an in-place edit, a retarget, a type change or a directory that gained a child all make the approval stale: nothing is deleted and the model is told to ask again. On a platform without a held reference the proxy refuses to ask at all. What remains is the window between that final check and the `unlink`: Linux offers no unlink conditional on the object behind the name, so a replacement landing inside that window is not ruled out.

**Response (200):** `{"delivered": true}` — the blocked turn was signaled.
**Response (404):** `{"delivered": false}` — no matching pending request (already resolved, cancelled, or timed out).

The correlation key is `session_id` + `tool_call_id`, so multiple destructive calls within one turn (`call_0`, `call_1`, …) are answered independently. See [CLI.md → Permission modes](CLI.md).

---

### GET /events

Subscribe to the **global typed-envelope broker**. Unlike `/v1/agent` (per-request stream of one turn), `/events` is a long-lived pub/sub feed of structured envelopes from across the proxy: agent loop boundaries, tool calls, V3 stage transitions, metrics. Multiple clients can subscribe simultaneously; slow consumers drop events rather than blocking producers.

**Envelope wire format** (matches `atlas/events.py` exactly). A `stage_start` example (`duration_ms` is only set on `stage_end` / `tool_result` and the final `done`):
```json
{
  "event_id": "evt_a1b2c3d4",
  "timestamp": 1714617823.412,
  "type": "stage_start",
  "stage": "llm",
  "payload": {"turn": 1, "messages": 3}
}
```

**Event types:** `stage_start`, `stage_end`, `tool_call`, `tool_result`, `metric`, `error`, `done`. See [PROTOCOL.md](PROTOCOL.md) for the full per-type payload contracts and the `{stage, detail}` → envelope translation atlas-proxy applies to v3-service SSE.

**Transport:** SSE. Each line is `data: <json>\n\n`. The server sends `: connected\n\n` immediately on subscribe and a `: heartbeat\n\n` comment every 15 s during quiet stretches to keep proxies/load-balancers from idling out the connection.

**Example:**
```bash
curl -N http://localhost:8090/events
```

Use `/events` when you want a global observability feed (a TUI pipeline pane, a metrics scraper, a debug log viewer). Use `/v1/agent` when you want to drive a specific user turn.

---

### GET /v1/calibration/status

Returns the proxy's view of whether the loaded model has compatible Geometric Lens artifacts (the verdict distilled from the lens service's `/health` payload) and whether an ASA control vector is in play. The TUI hits this on startup to render the badge next to the Pipeline pane title.

**Response shape:**

```json
{
  "lens": {
    "verdict": "supported",
    "can_score": true,
    "model_server_reachable": true,
    "cost_field_loaded": true,
    "cost_field_dim": 4096,
    "embed_dim": 4096,
    "gx_loaded": true,
    "cx_calibrated": true,
    "gx_calibrated": true,
    "embed_capacity_tokens": 2048,
    "embed_capacity_source": "declared",
    "hint": "ready"
  },
  "asa": {
    "verdict": "missing",
    "vector_path": "/models/ast_edit_steering.gguf",
    "vector_present": false,
    "hint": "no control vector at /models/ast_edit_steering.gguf — build one via `atlas asa build`"
  }
}
```

`cost_field_dim` / `embed_dim` reflect the loaded model's hidden dimension — the values differ per model. `embed_capacity_tokens` is the longest input the lens can score (llama-server's physical batch, `ATLAS_UBATCH`) as the lens reports it, `0` when unknown; `embed_capacity_source` is `declared` or `observed`. When it is below the proxy's per-turn generation ceiling (`ATLAS_MAX_TOKENS`), the `lens_scoring` dimension reads `partial` and its detail names both numbers: writes longer than the capacity come back unscored.

The payload also carries a `dimensions` array — the seven status dimensions the TUI and `atlas doctor` render, each `{name, status, detail}`:

```json
{
  "dimensions": [
    {"name": "model_runtime", "status": "supported", "detail": "model served and reachable"},
    {"name": "direct_agent", "status": "supported", "detail": "tools, permissions and sandbox verify; the lens can score"},
    {"name": "lens_identity", "status": "supported", "detail": "cost field matches the served model's dimension"},
    {"name": "lens_scoring", "status": "supported", "detail": "C(x) + G(x) scoring available"},
    {"name": "lens_calibration", "status": "calibrated", "detail": "per-model normalization + thresholds loaded"},
    {"name": "lens_intervention", "status": "active", "detail": "threshold interventions enabled"},
    {"name": "asa", "status": "unverified", "detail": "control vector present without a matching model marker; run `atlas asa build`"}
  ]
}
```

**Verdict values:**

- **Lens:** `supported` | `uncalibrated` | `no-artifacts` | `incomplete-artifacts` | `dim-mismatch` | `drifted` | `self-test-failed` | `model-server-unreachable` | `unreachable`. `incomplete-artifacts` means C(x) loaded but G(x) artifacts are missing; `uncalibrated` means weights loaded without the model's calibration files (`cx_normalization.json` / `gx_thresholds.json`) — both point at `atlas lens build`. `can_score` is `true` only for `supported` and `uncalibrated`; for every other verdict the `direct_agent` dimension is `blocked` and requests are refused ([ADR 0011](adr/0011-the-lens-is-required.md)). `model_server_reachable` is llama-server's reachability as the lens reports it.
- **ASA:** `active` | `missing` | `unverified` | `incompatible`. `active` means the control vector is marked for the served model; whether its effect was measured is the registry's `asa_status`. `incompatible` means the control vector on disk is marked for a different model than the one selected.

**Use:**

```bash
curl http://localhost:8090/v1/calibration/status | jq .
```

**Cache:** none — every call re-probes the lens service. Cost is ~50–200 ms (one HTTP round-trip to `lens/health`). TUI calls once at startup; CI / monitoring should poll no faster than every few seconds.

---

### POST /v1/chat/completions (passthrough)

OpenAI-compatible chat completions, kept for SDK compatibility. The proxy forwards these requests to llama-server otherwise unmodified, but guarantees an explicit completion bound: `max_tokens` (or `n_predict` on `/completion`, `/completions`, `/infill`) is clamped to `ATLAS_MAX_COMPLETION_TOKENS` (default 8192) when missing, non-positive, or above that ceiling. A client that omits `max_tokens` therefore gets 8192, not the server default — **no agent loop, no tool calls, no V3 pipeline runs on this endpoint**. The response shape and streaming format is whatever llama-server returns natively.

**For agent turns and tool calls, use `/v1/agent`.** It carries the full structured event stream (tool calls, V3 progress, permission requests).

**Request:**
```json
{
  "model": "local-model",
  "messages": [
    {"role": "user", "content": "Create a Python hello world script"}
  ],
  "max_tokens": 4096,
  "temperature": 0.3,
  "stream": true
}
```

**Response:** llama-server's native OpenAI-compatible shape — `chat.completion.chunk` SSE deltas when `stream: true`, a single `chat.completion` object otherwise. The proxy adds nothing else to the payload.

> **Note:** `/models` (no `/v1/` prefix) is an alias for `/v1/models`. Any unmatched path is proxied directly to llama-server.

---

### Tools available to the agent loop

Defined in `proxy/tools.go`. Used by the model when responding `{"type":"tool_call","name":"<tool>","args":{...}}`.

| Tool | Purpose |
|------|---------|
| `read_file` | Read a file and return its contents with line numbers |
| `outline_file` | Symbol outline of a file (functions/classes with line ranges and call edges, via tree-sitter). Orients inside a file too large to read whole; returns no bodies, so it locates code without showing it. |
| `write_file` | Create a new file. **Rejected for any existing file >5 lines** (`proxy/agent.go`) — use `structural_edit` (whole function/class/element rewrite) or `edit_file` (≤10-line surgical change). Two exemptions: corrupted-looking files (prose preamble, stray markdown fences), so a self-heal full-replace is allowed there; and files the session itself created, so the agent can rewrite its own drafts. |
| `edit_file` | Apply targeted `old_str`/`new_str` edits to an existing file. `old_str` and `new_str` are applied exactly as sent. A match that needed quote-style or per-line whitespace tolerance is applied and reported in the result (`content_note`). An `old_str` that matches only once `read_file`'s `N<tab>` display prefixes are removed is refused, with the matched line range and a `replace_lines` call that makes the change; before 2026-09-17 such edits were applied with the prefixes silently stripped from both arguments. Routes through V3 verification at tier 2+. A `.py` edit that introduces an unresolved direct call (would-be `NameError`) is refused by the structural gate — the error names the call and the file is not modified. The wrong tool for >10 lines of change — switch to `structural_edit`. |
| `insert_after` | Insert lines into a file after line `line` (1-based, as printed by `read_file`; `0` inserts at the top). Only the new text is supplied — there is no anchor to reproduce, which is what makes it the right tool for ADDING code rather than changing it. Requires the file to have been read, so the line numbers are current. Same syntax, structural and embedded-script gates as `edit_file`. |
| `replace_lines` | Replace lines `start_line`..`end_line` (1-based, inclusive, as printed by `read_file`) with new content. Instead of reproducing the whole span as an anchor, the model asserts only `expected_first_line` and `expected_last_line` — two lines, compared ignoring leading/trailing whitespace, so a stale range is caught before anything is written. A mismatch returns the actual lines with a ±3-line numbered window. Capped at `replaceLinesMaxSpan` (60) lines and one hunk per call; requires the file to have been read. Same structural and embedded-script gates as `edit_file`. |
| `structural_edit` | Surgical replacement of one whole named block (function, class, or HTML element). Selectors v1: Python `function:NAME` / `class:NAME` (decorator-aware), HTML `<tag>` (top-level; `<style>` inside `<head>` is NOT reachable in v1). REQUIRED for whole-function / whole-class / whole-element rewrites in existing files. Same structural gate as `edit_file` on the composed post-edit `.py` file. |
| `delete_file` | Remove a file (or an empty directory) from the workspace |
| `move_file` | Rename/move a file within the workspace (`source` → `destination`) |
| `search_files` | Regex search inside file **contents**. Returns matching lines with file paths and line numbers |
| `find_file` | Regex search by file **name** or relative path. Use to check whether a file exists. |
| `list_directory` | List files and subdirectories at a given path |
| `run_command` | Execute a shell command via bash inside the **sandbox container**. Sees `/workspace` (your project, bind-mounted rw, same path as the proxy). Has python3 + pip, node + npm, go, rust, gcc/g++, bash, pytest, tsx pre-installed. When the sandbox is unreachable the tool fails with `sandbox unavailable: ...` (exit code 1) — it never falls back to executing on the proxy host. Host execution happens only when the operator explicitly selects it via `ATLAS_VERIFY_IN=host` or `target = "host"` under `[execution]` in `.atlas/config.toml` **and** `ATLAS_TRUST_MODE=fully-trusted`. The default `trusted` silently downgrades a host request back to the sandbox, and `untrusted` refuses `run_command`/`run_background` outright. The proxy still runs `validateShellCommand` upstream as the destructive-verb gate — this entry just picks the executor. |
| `run_background` | Start a long-running process (e.g. `python app.py`, `npm run dev`) in the sandbox and return immediately with a `job_id`. The proxy detects shell `&` backgrounding through `run_command` and routes it here. |
| `tail_background` | Fetch new stdout/stderr lines from a backgrounded job by `job_id`. |
| `stop_background` | Terminate a backgrounded job by `job_id`. |

**Workspace containment.** Before any tool handler touches the filesystem, the proxy validates every path-taking argument (`path`, `source`, `destination`, `cwd`) against the workspace root (`proxy/context.go`). Paths that resolve outside the workspace — via `..`, absolute paths, or symlink components — are rejected with an error before execution, in every permission mode.

**Write deny-list.** A safety deny-list applies in every permission mode, including `yolo` (`proxy/permissions.go`): `write_file`/`edit_file` targets matching `.env`, `*.pem`, `*.key`, or `*credentials*` are refused, as are `run_command` invocations matching destructive patterns (`rm -rf /`, `mkfs*`, `dd if=... of=/dev/...`).

---

### GET /v1/models

OpenAI-compatible model list.

```bash
curl http://localhost:8090/v1/models
```

### GET /health

```bash
curl http://localhost:8090/health
```

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

Always returns 200. `status` is `"ok"` when inference, the lens, and the sandbox all respond healthy and the lens can score, `"degraded"` otherwise. `lens` reflects the lens service's informational `/health`; `lens_ready` says whether the lens can score, by the check `/v1/agent` applies to every request (lens `/ready`, then its `/health`: C(x) and G(x) loaded, self-test passed, not drifted, llama-server reachable). When `lens_ready` is `false`, `lens_reason` says why, and every request is refused. `capabilities` advertises optional proxy features clients can probe for.

### GET /ready

```bash
curl http://localhost:8090/ready
```

```json
{
  "ready": true,
  "inference": true,
  "lens_ready": true,
  "sandbox": true,
  "v3": true
}
```

Returns 200 only when **all** gates pass: llama-server `/health`, the lens can score (the check `/v1/agent` applies; `lens_reason` says why when it cannot), sandbox `/health`, and v3-service `/health` (checked whenever a V3 URL is configured). 503 with the same body otherwise.

---

## V3 Pipeline Service (Port 8070)

Runs the full V3 code generation pipeline: probe, PlanSearch, DivSampling, Budget Forcing, Lens scoring, sandbox testing, and Phase 3 repair. Normally invoked indirectly through the proxy's `edit_file`/`write_file` tools, but the HTTP surface is stable for direct use.

### POST /v3/generate

Run the V3 pipeline for a file generation task. Streams progress events as SSE.

**Request:**
```json
{
  "file_path": "app/page.tsx",
  "baseline_code": "export default function Page() { ... }",
  "project_context": {"package.json": "{...}", "tsconfig.json": "{...}"},
  "framework": "nextjs",
  "build_command": "npx next build",
  "constraints": ["Must use Tailwind CSS", "Must be a client component"],
  "tier": 2,
  "working_dir": "/path/to/project",
  "budget_ms": 150000
}
```

All fields are optional except the task itself. `tier` defaults to 2. `budget_ms` is the wall-clock cap the caller applies to this call; when it is a positive number the pipeline plans every phase against it, and otherwise it reads `ATLAS_V3_TIMEOUT`. The proxy always sends the cap it applies.

**Response (SSE stream):**
```
data: {"stage": "probe", "detail": "Generating probe candidate..."}
data: {"stage": "probe_scored", "detail": "C(x)=0.72 norm=0.68 G(x)=0.81 (likely_correct)", "data": {"gx_score": 0.81, "gx_available": true, "verdict": "likely_correct"}}
data: {"stage": "phase2_allocated", "detail": "k=3 tier=standard", "data": {"k": 3, "tier": "standard", "base_tier": "standard", "gx_escalation": 0, "capped_from": "", "reason": "gated"}}
data: {"stage": "plansearch", "detail": "Generating 2 plans...", "data": {"plans": 2}}
data: {"stage": "sandbox_test", "detail": "Testing 3 candidates...", "data": {"candidates": 3}}
data: {"stage": "sandbox_pass", "detail": "Candidate 1 passed", "data": {"index": 1, "elapsed_ms": 420, "energy": 0.34}}
data: {"stage": "sandbox_done", "detail": "1/3 passed", "data": {"passed": 1, "total": 3}}
data: {"stage": "llm_start", "detail": "call #4", "data": {"call": 4, "max_tokens": 4096, "temperature": 0.7}}
data: {"stage": "token", "detail": "def "}
data: {"stage": "token", "detail": "merge_sort("}
data: {"stage": "llm_end", "detail": "245 tok · 1820ms", "data": {"call": 4, "tokens": 245, "elapsed_ms": 1820}}

event: result
data: {"code": "...", "passed": true, "phase_solved": "phase1", "candidates_tested": 3, "winning_score": 0.85, "total_tokens": 12500, "total_time_ms": 4200.0, "verification_evidence": []}

data: [DONE]
```

`phase_solved` says how `code` was chosen: `probe`, `phase1`, `pr_cot`, `refinement` or `budget` after a check it passed; `consensus` by agreement alone; `incumbent` when the caller's own `baseline_code` held (`code` is then those exact bytes); `baseline` when nothing was chosen and the caller's bytes are sent back; `none` otherwise.

Each progress event has the shape `{"stage": "<name>", "detail": "<human-readable>", "data": {...}}`. The `data` object carries structured fields specific to that stage (counts, indices, timings, strategy labels) — the proxy bridge fans these out into the dedicated `v3_*` events on `/v1/agent`. Older stages without `data` enrichment still emit `stage` + `detail` only and continue to flow through `v3_progress`.

The proxy's `tools.go` bridge translates these into `v3_progress` / `v3_token` / `v3_llm_start` / `v3_llm_end` events on the `/v1/agent` stream — direct callers see the raw V3 stages.

<details>
<summary><b>All SSE stage values</b></summary>

The pipeline emits stages as it progresses. Not all stages appear in every run — the pipeline exits early when a candidate passes.

| Phase | Stages |
|-------|--------|
| **Setup / classification** | `task_type` (interactive/batch classification of the request) |
| **Probe (Phase 0)** | `probe`, `probe_light`, `probe_error`, `probe_retry`, `probe_failed`, `self_test_gen`, `self_test_done`, `self_test_error`, `self_test_skip` (interactive task — compile smoke-test instead), `self_test_verify`, `smoke_check`, `interactive_lint`, `probe_scored`, `probe_sandbox`, `probe_pass`, `incumbent` (the caller's `baseline_code` checked as a candidate), `incumbent_holds` (it passed and its record closes, so it is returned unchanged) |
| **Generation (Phase 1)** | `phase1`, `phase2` (allocation), `phase2_allocated`, `plansearch`, `plansearch_done`, `plansearch_error`, `divsampling`, `divsampling_done`, `divsampling_error` |
| **Testing / selection (Phase 2)** | `sandbox_test`, `sandbox_pass`, `sandbox_fail`, `sandbox_done`, `role_check` (a candidate dropped a top-level name of the caller's file; it fails), `lens_per_step`, `lens_veto`, `structural_veto`, `call_graph_veto`, `retyping_dropped` (candidates that only re-typed the caller's file leave the pool), `selected` |
| **Repair (Phase 3)** | `phase3`, `pr_cot`, `pr_cot_pass`, `pr_cot_failed`, `pr_cot_error`, `refinement`, `refinement_pass`, `refinement_failed`, `refinement_error`, `refinement_verify_failed`, `refinement_skip` (remaining `ATLAS_V3_TIMEOUT` budget cannot afford one iteration — straight to fallback), `call_chain_context`, `fallback`, `fallback_all_vetoed` (every candidate was vetoed — no code returned) |
| **Verification** | `build_verify_unavailable` (build-command verification skipped — runner unreachable or command not allowed by policy) |
| **LLM streaming** | `llm_start`, `token`, `llm_end` (one bracketed group per internal LLM call — planner, candidate generation, repair, etc.) |

</details>

### POST /v3/plan

Generates a step-by-step plan for a coding task using diverse LLM sampling and heuristic scoring. Used by the proxy's agent loop to seed each turn with explicit step guidance — see [ARCHITECTURE.md § Plan Mode](ARCHITECTURE.md#plan-mode-per-turn-pre-flight) for the consumer side.

**Request:**
```json
{
  "user_message": "fix the broken index.html template",
  "working_dir": "/workspace",
  "project_context": {
    "app.py": "from flask import Flask...",
    "templates/index.html": "<!DOCTYPE html>..."
  },
  "n_candidates": 3
}
```

| Field | Required | Notes |
|---|---|---|
| `user_message` | yes | The original user request the planner must address. |
| `working_dir` | optional | Used in the planner prompt for path-context. Defaults to `""` (empty). |
| `project_context` | optional | Map of `relative_path → file_content`. Files are truncated to ~200 chars in the planner prompt. |
| `n_candidates` | optional | How many candidate plans to sample. Defaults to 3. Each is sampled at a different temperature (0.3 / 0.5 / 0.7) for diversity. |

**Response:** SSE stream of `{stage, detail, data}` events ending with `event: result\ndata: <plan-json>` and `data: [DONE]`.

Plan-pipeline stages: `plan_start`, `plan_candidate`, `plan_candidate_unparseable`, `plan_candidate_error`, `plan_candidate_scored`, `plan_selected`, `plan_failed`. Per-candidate token streaming flows under `token` / `llm_start` / `llm_end` (filtered out by the proxy bridge before reaching `/v1/agent`).

The final `event: result` payload has the shape:

```json
{
  "steps": [
    {"id": "s1", "action": "read_file", "target": "templates/index.html", "why": "inspect"},
    {"id": "s2", "action": "edit_file", "target": "templates/index.html", "why": "fix structural HTML"},
    {"id": "s3", "action": "run_command", "target": "curl http://localhost:5000/", "why": "verify"}
  ],
  "verify_step": "s3",
  "rationale": "investigate, change, verify.",
  "candidates_tested": 3,
  "winning_score": 1.0,
  "winning_index": 0,
  "reasons": ["step count 3 in range", "verify_step=s3", ...]
}
```

If all candidates fail to parse, the endpoint returns a single-step fallback (`{steps:[{action:"investigate the request and act"}], verify_step:null, winning_index:-1}`) rather than 5xx — callers can detect the fallback by `winning_index<0` or `reasons` containing "all candidates failed".

### POST /internal/structural_edit

Friendly-selector named-node replacement. Stateless transform: caller provides the file's source bytes, server parses with tree-sitter, finds the named node, returns the new full file content. The proxy reads + writes; this endpoint never touches the filesystem.

**Request:**
```json
{
  "path": "app.py",
  "source": "<full current file content>",
  "selector": "function:dashboard",
  "content": "@app.route('/dashboard')\ndef dashboard():\n    return render_template('dashboard.html')"
}
```

**Selectors v1:**
- Python: `function:NAME`, `class:NAME` — decorator-aware (replaces the `decorated_definition` wrapper when present so `@app.route(...)` lines get included in the swap)
- HTML: `<tag>` — top-level tag-name match (e.g. `<body>`, `<head>`, `<h1>`)

**Response (success):**
```json
{
  "success": true,
  "language": "python",
  "selector": "function:dashboard",
  "new_content": "<full new file>",
  "byte_range": [662, 881],
  "old_size": 960,
  "new_size": 852
}
```

**Response (failure):**
```json
{"success": false, "error": "selector 'function:foo' matched 0 nodes in app.py — that symbol does not exist in this file. This file defines: function:dashboard, class:UserModel. Use one of these exact selectors, or read the file to confirm."}
```

Hard rule: selector must match exactly one node. Ambiguous selectors fail with a clear error so the caller can be more specific instead of silently rewriting the wrong function.

### POST /internal/symbol_index

Resolve user-message symbol references against project source. The proxy extracts candidate symbols from the user message via regex (backticked identifiers, "the X function" patterns, dotted-path leaves), walks the working directory for `.py` files (capped at 50 files / 500 KB total), and POSTs to this endpoint. Server tree-sitter-walks each file for `function_definition` and `class_definition` nodes, returns snippets for the symbols defined in the project. Matched snippets are auto-injected into the agent's first turn so the model doesn't burn turns on `read_file`-spelunking.

**Request:**
```json
{
  "file_map": {"app.py": "<file source>", "utils.py": "..."},
  "symbols": ["dashboard", "UserModel", "validate"],
  "max_snippets": 3,
  "max_lines_per_snippet": 200
}
```

**Response:**
```json
{
  "matched": [
    {"name": "dashboard", "kind": "function", "file": "app.py", "snippet": "@app.route('/dashboard')\ndef dashboard():\n    return ...", "n_lines": 5, "truncated": false}
  ],
  "skipped": [
    {"name": "UserModel", "reason": "not defined in scanned project files"},
    {"name": "validate", "reason": "ambiguous (3 definitions)"}
  ]
}
```

Decorator-aware (matches `structural_edit`'s behavior): a function with `@app.route(...)` returns the byte range of the wrapping `decorated_definition`, so the snippet includes the decorator line. Skips nested functions and methods inside classes — top-level definitions only in v1.

Stateless: each call rebuilds the index from the file_map. No caching.

### POST /internal/cyclomatic_complexity

McCabe cyclomatic complexity from tree-sitter syntax-tree traversal. Used by the proxy's `classifyFileTier` to *escalate* (never downgrade) the regex-based tier verdict when real branching complexity warrants the V3 pipeline.

**Request:**
```json
{"path": "app.py", "source": "<full file content>"}
```

**Response (success):**
```json
{"ok": true, "language": "python", "cyclomatic_complexity": 12}
```

**Response (unsupported / parse failed):**
```json
{"ok": false, "error": "cyclomatic_complexity v1 supports .py only (got index.html)"}
```

v1 supports Python only. Decision points counted: `if`/`elif`, `for`, `while`, `except`, `and`/`or` (short-circuit), ternary `x if cond else y`, `case` (match), and `if` filter clauses inside comprehensions.

### POST /internal/outline

Top-level function/class listing for a file — the backend of the proxy's `outline_file` tool. Uses the same decorator-aware tree-sitter walk as `structural_edit`, so any symbol the outline names is selectable by `structural_edit` with the same name. Bodies are not returned. `.py` only (`supported: false` otherwise — the proxy falls back to a regex outline).

**Request:**
```json
{"path": "app.py", "source": "<full file content>"}
```

**Response:**
```json
{
  "symbols": [
    {"name": "dashboard", "kind": "function", "start_line": 12, "end_line": 24}
  ],
  "supported": true
}
```

Each symbol also carries its intra-file call-graph neighborhood (`calls` / `called_by` string arrays).

An `embedded_regions` array is added when the file holds code in another language — `<script>` / `<style>` blocks in HTML, and the same inside Python string literals (the `render_template_string` shape). The host grammar cannot see into a string literal, so without it the outline of a Flask app whose whole UI is one template reports `function:index` and nothing else, and a model looking for the game loop reaches for `structural_edit selector="function:draw"` — a symbol no selector can reach. Each entry carries `where`, `kind`, `start_line`, `end_line`, and for JavaScript the `symbols` declared inside it.

```json
{"embedded_regions": [
  {"where": "the <script> block inside the Python string HTML_TEMPLATE",
   "kind": "javascript", "start_line": 77, "end_line": 202,
   "symbols": ["draw", "spawnFood", "collision", "gameOver"]}
]}
```

### POST /internal/structural_check

Resolve every direct-identifier call in a Python source against its local defs, imports, bound names, builtins, and (optionally) supplied project symbols — without executing it. Used by the proxy's structural gate (#147) on the `.py` write paths (`edit_file`, `structural_edit`, and the `write_file` branches; under BypassV3 only the non-iterating T0/T1 direct `write_file` is excepted) to refuse content that introduces a `NameError`: a call to a name the file never binds parses fine but 500s at request time.

**Request:**
```json
{
  "path": "app.py",
  "source": "<composed post-edit file text>",
  "project_context": {"other.py": "..."}
}
```

`project_context` is optional additive leniency: top-level defs from those files are credited so a cross-file symbol is not flagged. The caller excludes the edited file's own pre-edit body (its current symbols come from `source`).

**Response:**
```json
{"ok": true, "unresolved": ["render_template"], "n_unresolved": 1, "wildcard_imports": false}
```

`ok: false` means the check could not run (tree-sitter unavailable, non-UTF-8 source) and the caller fails open. Malformed Python does not produce `ok: false` — tree-sitter parses tolerantly. `unresolved` is the complete list (no cap): the caller diffs original-vs-edited lists, and a truncated list would make that comparison unsound. `wildcard_imports: true` reports that the source contains `from x import *`; unresolved reporting is suppressed in that case (the wildcard may supply any name), so it always accompanies an empty `unresolved`.

### POST /internal/embedded_script_check

Syntax-check the JavaScript (and brace-balance the CSS) *embedded* in a file — what the sandbox's `/syntax-check` is blind to, because it sees the host language only. Two carriers are handled: `<script>`/`<style>` blocks in `.html`/`.htm`/`.jinja`/`.jinja2` files, and HTML held in a **Python string literal** (the `render_template_string` shape). Backs the proxy's embedded-script gate on `edit_file`, `insert_after`, `replace_lines`, `structural_edit` and the `write_file` branches.

Given the optional `previous` (the pre-edit file) it also reports a **stopped render loop**: a function a repeating timer used to drive that the edit left scheduled exactly once, never re-arming. That finding carries `"defect": "stopped_loop"` and needs both versions, because one version alone cannot tell a dead loop from a deliberate delayed one-shot. A finding for a missing closing token also carries `opened_line` / `opened_text`, the block that was left unclosed: tree-sitter reports the absence at the point the parser gave up, which is generally a line the edit never touched. A `let`/`const` declaring the same name twice in one scope is reported from the edited file alone as `"defect": "redeclaration"` — the spec makes that an unconditional early error, so the browser refuses the whole script. Same blind spot as the syntax check, one level up — the JavaScript parses, the server starts and the page returns 200, and the page freezes after one frame.

**Request:**
```json
{"path": "app.py", "source": "<full file content>", "previous": "<pre-edit content, optional>"}
```

**Response:**
```json
{
  "ok": true,
  "findings": [
    {
      "line": 121,
      "column": 86,
      "kind": "javascript",
      "where": "the <script> block inside the Python string HTML_TEMPLATE",
      "message": "unexpected `)`",
      "hint": "Nothing opened a `(` for it to close — delete the stray `)`, or add the `(` it was meant to close.",
      "text": "else if(key === 'ArrowDown' && direction !== 'UP') nextDirection = 'DOWN');"
    }
  ]
}
```

`line`/`column` are positions in the **file**, not in the extracted block, so the model can go straight to them. `kind` is `javascript` or `css`. At most 3 findings are returned, sorted by line; the caller reports the first.

`ok: false` means the check could not run (tree-sitter-javascript missing from the build, non-UTF-8 source) and the caller fails open. An unsupported extension is `ok: true` with no findings — nothing was wrong with it.

It backs a write-blocking gate, so it is conservative by construction: every ambiguous block reports nothing rather than guessing. No finding is produced when the `<script>`/`<style>` tag counts don't balance (a `</script>` inside a JS string truncates the raw text), for `<script src=...>` with no body, for a non-JS `type` (`text/template`, `application/json`, `importmap`, …), for a block still carrying template tags after `{{ }}` placeholder masking (`{% %}`, `<% %>`, multi-line placeholders), when masking those placeholders makes the block parse (the error was the template syntax), or when the Python string literal is an f-string, part of a concatenation, or contains a backslash escape (what Python renders then differs from the source bytes being parsed). Placeholder masking is length- and newline-preserving and runs only *after* the raw parse already failed, so it can only ever remove findings.

### GET /health

```bash
curl http://localhost:8070/health
# {"status": "ok", "service": "v3-pipeline"}
```

---

## Geometric Lens (Port 8099)

Energy-based code scoring using C(x) cost field and G(x) quality prediction. Every route is internal to the stack — the proxy and v3-service are the only callers.

> **Internal port:** The container binds uvicorn to **8099** (`geometric-lens/Dockerfile`, `EXPOSE 8099`). Docker Compose maps host 8099 → container 8099. Bare-metal launches with the same `--port 8099` default. K3s deployments expose `ATLAS_LENS_NODEPORT` (default 31144) externally.

### POST /internal/lens/gx-score

Score code using combined C(x) + G(x) energy. Single embedding extraction serves both models.

**Request:**
```json
{"text": "def merge_sort(arr):\n    if len(arr) <= 1:\n        return arr\n    ..."}
```

**Response:**
```json
{
  "scored": true,
  "cx_energy": 5.2,
  "cx_normalized": 0.32,
  "cx_calibrated": true,
  "gx_score": 0.85,
  "verdict": "likely_correct",
  "gx_available": true,
  "enabled": true,
  "latency_ms": 26.4
}
```

| Field | Type | Description |
|-------|------|-------------|
| `scored` | bool | Whether a score was computed. `false` means every score field is `null` and `failure` says why |
| `cx_energy` | float or null | Raw cost field energy (lower = more likely correct) |
| `cx_normalized` | float or null | 0–1 normalized energy |
| `cx_calibrated` | bool | Whether the model's C(x) normalization calibration (`cx_normalization.json`) is loaded |
| `gx_score` | float or null | 0–1 probability of correctness (G(x) model) |
| `verdict` | string | `"likely_correct"`, `"uncertain"`, or `"likely_incorrect"` when thresholds are calibrated; `"uncalibrated"` when the model's `gx_thresholds.json` is missing; `"unavailable"` when the Lens is disabled or the G(x) model isn't loaded; `"unscored"` when no score was computed (payload carries `failure` and `error`) |
| `gx_available` | bool | Whether the G(x) model was loaded |
| `enabled` | bool | Whether Geometric Lens is enabled |
| `latency_ms` | float | Execution time in milliseconds |
| `failure` | object | Present only when `scored` is `false`: `kind` plus its detail (below) |

When Lens is disabled, returns `enabled: false` with neutral defaults (`cx_energy: 0.0`, `gx_score: 0.5`): a configuration state, not a failed score.

When the Lens could not score the text, the response is not a score: `scored: false`, `verdict: "unscored"`, `null` in every score field, and a `failure` object. `kind` is one of:

| `failure.kind` | Meaning | Extra fields |
|---|---|---|
| `embed_capacity` | The input exceeds llama-server's physical batch (`ATLAS_UBATCH`), which processes one embedding request whole. The counts are the server's own. | `input_tokens`, `capacity_tokens`, `detail` |
| `model_server_error` | llama-server answered an HTTP error that was not the capacity refusal | `status`, `detail` |
| `model_server_unreachable` | No answer from llama-server (refused, reset, timed out) | `detail` (exception type) |
| `embedding_contract` | The answer violated the artifact's embedding convention | `detail` |
| `nonfinite_score` | A score came out NaN or infinite (a degenerate forward or calibration); reported as unscored, never serialized as a number | `field` (`cx_energy`, `cx_normalized`, `gx_score`), `detail` |
| `internal` | Anything else; the service log has the traceback | `detail` |

Nothing in such a response is truncated, split or substituted: the whole sequence is what the calibrated score is computed from ([ADR 0010](adr/0010-lens-capacity-boundary-is-typed.md)). v3-service records the failure on the candidate, ranks it after every scored candidate, and delivers it only as the last verified candidate standing; the proxy applies no threshold to an unscored write.

**Example:**
```bash
curl http://localhost:8099/internal/lens/gx-score \
  -H "Content-Type: application/json" \
  -d '{"text": "print(\"hello world\")"}'
```

### GET /health

```bash
curl http://localhost:8099/health
# {"service": "geometric-lens", "status": "healthy", "subsystems": {"llama_server": {...}, "lens": {...}}}
```

Always returns 200 — the endpoint is informational. `status` is `"healthy"` or `"degraded"`; the `subsystems.lens` block carries `cost_field_loaded`, `cost_field_dim`, `embed_dim`, `gx_loaded`, `cx_calibrated`, `gx_calibrated`, and the self-test result the proxy's `/v1/calibration/status` verdict is derived from.

It also carries the embedding capacity contract: `embed_capacity_tokens` (the longest input one score can be computed from, llama-server's physical batch; `null` until known), `embed_capacity_source` (`"declared"` from `LLAMA_EMBED_CAPACITY_TOKENS`, `"observed"` once the server has refused an input, which is authoritative), `embed_capacity_rejections` and `embed_capacity_max_rejected_tokens`. Information only: an input past the capacity is reported `unscored` per request, never gated here.

### GET /ready

```bash
curl http://localhost:8099/ready
```

Readiness probe (`geometric-lens/main.py`). Flips to 503 when scoring is degraded (lens weights missing, embedding-dim mismatch). It answers 200 for a lens with no G(x) model, so the atlas-proxy asks this and then the lens `/health` before it accepts a request ([ADR 0011](adr/0011-the-lens-is-required.md)); its `/health` and `/ready` report the result as `lens_ready`. The payload repeats `embed_capacity_tokens`; the capacity never changes the verdict.

### Additional endpoints

These are not part of the public API — every row is consumed by other ATLAS services in-stack. They require the service token when one is configured (`secrets/service-token`, see CONFIGURATION.md `ATLAS_SERVICE_TOKEN_FILE`) and are open otherwise.

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/internal/lens/score-text` | POST | Score text (C(x) only). `scored: false` with `energy`/`normalized` `null` and a `failure` when no score was computed (same kinds as `gx-score`, plus `models_not_loaded`) |
| `/internal/lens/score-per-step` | POST | Per-token C(x)+G(x) scoring (one forward pass over the prompt; returns per-step verdicts plus `first_off_rails_idx` and aggregates). Pass `layer: int` to score a specific intermediate residual layer (requires the per-layer hidden-states extension on llama-server). `scored: false` with an empty `per_step`/`aggregate`, `n_tokens: 0` and a `failure` when no score was computed (same kinds as `gx-score`). |

---

## Sandbox (Port 30820 → container 8020)

Isolated code execution with compilation, testing, and linting support. The container is read-only with `/workspace` bind-mounted (rw) from `ATLAS_PROJECT_DIR` — the same path the proxy sees, so paths the agent learned via `read_file`/`list_directory` work verbatim in `/shell` calls.

### POST /jobs/start

Spawn a background process and return a `job_id` immediately. Used by the proxy's `run_background` tool for long-running things like `python app.py` or `npm run dev`. The process runs in a new session group so `/jobs/{id}/stop` can kill the whole tree.

**Request:**
```json
{
  "command": "python app.py",
  "cwd": "/workspace",
  "env": {"FLASK_ENV": "development"}
}
```

**Response:**
```json
{"job_id": "a1b2c3d4e5f6", "pid": 4711, "started_at": 1714617823.4}
```

**Errors:** 400 on empty command; 429 when active-job count exceeds `BG_MAX_JOBS`.

### GET /jobs

Every job the sandbox is holding, whoever started it. The registry is process-wide and has no session concept, so a server left running by an earlier session keeps its port while `/jobs/{job_id}` needs an id the new session never saw — the bind failure's own advice ("identify and stop that program") was unfollowable without this. The proxy's port-conflict hint falls back to this list and names the offending job so the model can `stop_background` it.

**Response:**
```json
{"jobs": [{"job_id": "a1b2c3d4e5f6", "command": "python app.py", "started_at": 1714617823.4, "running": true}]}
```

Ordered oldest first. Exited jobs stay listed with `"running": false` until the reaper clears them, so a just-finished job is distinguishable from one still holding a port.

### GET /jobs/{job_id}/output

Snapshot of recent stdout/stderr plus run state. Used by the proxy's `tail_background` tool. Each stream is a ring buffer capped at `BG_MAX_LINES`; pass `?lines=N` (default 50) for the tail length.

**Response:**
```json
{
  "job_id": "a1b2c3d4e5f6",
  "running": true,
  "exit_code": null,
  "stdout": ["Listening on :5000\n", "..."],
  "stderr": [],
  "elapsed_sec": 12.4,
  "command": "python app.py"
}
```

`exit_code` is null while running, integer once exited. **404** when `job_id` is unknown.

### POST /jobs/{job_id}/stop

SIGTERM the process group, wait briefly, SIGKILL if still alive. Used by the proxy's `stop_background` tool. Returns the final stdout/stderr buffer. **404** when `job_id` is unknown.

### POST /shell

Run a shell command against the bind-mounted workspace. The proxy's `run_command` tool routes here so the agent's verification commands (`pytest`, `python app.py`, `npm test`, `curl -sf`, etc.) execute against the user's actual files with the full language matrix the proxy lacks.

**Request:**
```json
{
  "command": "cd flask_app && pip install -q -r requirements.txt && python app.py",
  "cwd": "/workspace",
  "timeout": 30,
  "env": {"FLASK_ENV": "development"}
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `command` | string | (required) | Shell command run via `bash -c`. |
| `cwd` | string | `/workspace` | Absolute path inside the container. **Must be under `/workspace`** — `/etc`, `/`, etc. are rejected with HTTP 400. The path must already exist (no auto-mkdir). |
| `timeout` | int | 30 | Max execution time in seconds. Capped at `MAX_EXECUTION_TIME` (60s in-code default; the Compose stack sets it to 300 via `ATLAS_SANDBOX_MAX_EXECUTION_TIME`, matching the proxy's `run_command` cap). |
| `env` | object | null | Extra env vars merged on top of the container's environment. |
| `files` | object | null | Optional map of `relative-path → content`. When present, `/shell` copies a bounded snapshot of the workspace into `/tmp`, overlays these files, runs the command there, then deletes the snapshot. Used by V3 build verification to test a candidate without writing it into the real bind-mounted project. |
| `observe_paths` | array | null | Optional list of workspace-relative paths to hash before and after the command. When present, the response carries an `observation`. Opt-in: existing callers pay nothing. An empty list still reports the workspace digest. |

**Response:**
```json
{
  "success": true,
  "stdout": "Hello World\n",
  "stderr": "",
  "exit_code": 0,
  "elapsed_ms": 78,
  "timed_out": false
}
```

`success` is `exit_code == 0`. `timed_out` states the timeout structurally so a
caller does not have to recognise it from prose.

**Observation** (present only when `observe_paths` was sent):

```json
{
  "observation": {
    "target_before": {"solve.py": "e3b0c442…"},
    "target_after":  {"solve.py": "e3b0c442…"},
    "workspace_before": "9f86d081…",
    "workspace_after":  "9f86d081…",
    "workspace_files": 42,
    "digest_truncated": false
  }
}
```

The snapshot is deleted before the response returns, so nothing outside the
executor can look at it afterwards — this is the only way a caller can learn
whether a command changed what it was testing. It reports **facts and no
conclusions**: an absent path is `""` rather than omitted, and
`digest_truncated` is `true` when the tree exceeded
`ATLAS_SHELL_OBSERVE_MAX_FILES` (5000) rather than a partial digest being
passed off as a complete one. Whether any observed change was *permitted* is
the caller's question; the executor has no way to know.

The proxy uses this for candidate staging — see
[CANDIDATE_AUTHORIZATION.md](CANDIDATE_AUTHORIZATION.md). Stdout is truncated to its last 4000 chars and stderr to its last 2000 server-side; the proxy's `run_command` bridge applies its own caps (stdout 8000 / stderr 4000) on top. State is **not** persistent between calls — each call is its own subprocess. To preserve state (e.g. an installed pip package) chain commands with `&&` in a single call, or rely on a project venv that survives across calls because it lives on the bind-mounted workspace.

The proxy's destructive-verb gate (`validateShellCommand`) blocks catastrophic commands — fork bombs, `rm -rf /`-class whole-project wipes, `find … -delete` / `-exec rm` from a search root, `dd`/`mkfs`/`wipefs` against block devices — and unwraps one `bash -c "…"` / `eval "…"` layer so the inner command is checked too, *before* the call ever reaches `/shell`. Ordinary `mv`, `cp`, and targeted `rm` of specific files are allowed. This endpoint is the executor, not the gate.

### POST /execute

Execute code in an isolated environment.

**Request:**
```json
{
  "code": "from utils import greet\nprint(greet('world'))",
  "language": "python",
  "test_code": null,
  "requirements": null,
  "timeout": 30,
  "files": {"utils.py": "def greet(name): return f'hi {name}'"}
}
```

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `code` | string | (required) | Code to execute |
| `language` | string | `"python"` | Target language (see supported list below) |
| `test_code` | string | null | Optional test code (e.g. pytest assertions) |
| `requirements` | string[] | null | Python packages to pip install before execution |
| `timeout` | int | 30 | Max execution time in seconds (capped at `MAX_EXECUTION_TIME`) |
| `files` | object | null | Map of `relative-path → file-content` written into the workspace before execution. Use to ship multi-file project context (e.g. modules the candidate imports). Path traversal (`..`, absolute paths, symlink components) is rejected. |
| `stdin` | string | null | Optional standard input piped to the run step. When null the process inherits the server's stdin. |

**Response:**
```json
{
  "success": true,
  "compile_success": true,
  "tests_run": 1,
  "tests_passed": 1,
  "lint_score": 8.5,
  "stdout": "hello from sandbox\n",
  "stderr": "",
  "error_type": null,
  "error_message": null,
  "execution_time_ms": 45
}
```

| Field | Type | Description |
|-------|------|-------------|
| `success` | bool | Overall pass/fail |
| `compile_success` | bool | Whether compilation succeeded (always true for interpreted languages) |
| `tests_run` | int | Number of tests executed |
| `tests_passed` | int | Number of tests that passed |
| `lint_score` | float? | Pylint score 0–10 (Python only, null for other languages) |
| `stdout` | string | Stdout output (truncated to last 4000 chars) |
| `stderr` | string | Stderr output (truncated to last 2000 chars) |
| `error_type` | string? | Error classification (e.g. `SyntaxError`, `CompileError`, `Timeout`, `ImportError`) |
| `error_message` | string? | First 500 chars of error details |
| `execution_time_ms` | int | Execution time in milliseconds |

### POST /syntax-check

Check syntax without executing code.

**Request:**
```json
{
  "code": "def foo(:\n    pass",
  "language": "python",
  "filename": "main.py"
}
```

**Response:**
```json
{
  "valid": false,
  "errors": ["SyntaxError: invalid syntax (line 1)"],
  "language": "python",
  "check_time_ms": 12,
  "status": "checked",
  "outcome": "completed"
}
```

The verdict is about syntax only, for the one file sent. `status` is `checked` when the checker ran to its own conclusion, and `not_run` when the resource contract stopped it or it never started; then `valid` is `false`, `outcome` says how it ended (`timed_out`, `memory_exhausted`, `spawn_failed`, ...), and `errors` holds one `syntax verification unavailable` line rather than a syntax error.

### GET /languages

List supported languages with installed runtime versions.

```bash
curl http://localhost:30820/languages
```

```json
{
  "languages": {
    "python": "Python 3.11.2",
    "javascript": "v20.11.0",
    "typescript": "5.3.3",
    "go": "go1.22.0",
    "rust": "rustc 1.77.0",
    "c": "gcc 13.2.0",
    "cpp": "g++ 13.2.0",
    "bash": "GNU bash 5.2.21"
  }
}
```

**Supported languages:** `python` (aliases: `py`, `python3`), `javascript` (`js`, `node`), `typescript` (`ts`), `go` (`golang`), `java`, `kotlin` (`kt`, `kts`), `rust` (`rs`), `c`, `cpp` (`c++`), `ruby` (`rb`), `php`, `bash` (`sh`, `shell`)

### GET /health

```bash
curl http://localhost:30820/health
# {"status": "healthy"}
```

---

## llama-server (Port 8080)

Standard llama.cpp server API. See [llama.cpp documentation](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md).

### POST /v1/chat/completions

OpenAI-compatible chat completions with `response_format` support for grammar-constrained JSON output.

**Example:**
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "local-model",
    "messages": [{"role":"user","content":"Say hello"}],
    "max_tokens": 50,
    "response_format": {"type": "json_object"}
  }'
```

### POST /completion

Raw completion endpoint (no chat template). Used internally by the benchmark runner.

### POST /embedding

Generate embeddings for input text. Used by Geometric Lens for C(x)/G(x) scoring.

#### ATLAS extension: per-layer residual hidden states

`/embedding` and `/embeddings` (the native paths — *not* `/v1/embeddings`)
accept an optional `layers` parameter that returns the post-block residual
stream at the requested transformer layers, in addition to the standard
final-layer embedding. Used by the Geometric Lens (lens-as-PRM scoring)
and the Qwen-Scope SAE service.

**Request:**

```json
{
  "content": "...",
  "layers": [8, 16, 24]
}
```

- `layers` (optional): array of transformer block indices. Each must be
  in `[0, n_layer)`. Maximum 8 entries per request (memory bound).
  Omit for back-compat (response shape is unchanged).
- Rejected on `/v1/embeddings` — the OAI-compat path doesn't expose this
  extension. Use `/embedding` or `/embeddings` instead.

**Response (when `layers` is set):**

```json
[{
  "index": 0,
  "embedding": [...],
  "hidden_states": {
    "8":  "<base64 float32, row-major n_tokens × hidden_dim>",
    "16": "...",
    "24": "..."
  },
  "hidden_states_n_tokens": 84,
  "hidden_states_dim":      4096,
  "hidden_states_dtype":    "float32",
  "hidden_states_encoding": "base64"
}]
```

The base64-decoded buffer is float32 little-endian, row-major
`[n_tokens][hidden_dim]`. A client decodes with
`np.frombuffer(b64decode(s), dtype='<f4').reshape(n_tokens, hidden_dim)`.

**Decode example (Python):**

```python
import base64, numpy as np, requests
r = requests.post("http://llama-server:8080/embedding", json={
    "content": "def fib(n): return n if n<2 else fib(n-1)+fib(n-2)",
    "layers":  [8, 16, 24],
}).json()[0]
n, d = r["hidden_states_n_tokens"], r["hidden_states_dim"]
hs = {int(k): np.frombuffer(base64.b64decode(v), dtype="<f4").reshape(n, d)
      for k, v in r["hidden_states"].items()}
# hs[16].shape == (n_tokens, 4096)
```

**Tap point:** post-block residual stream (`l_out-{N}` in llama.cpp's
ggml graph). Verified against Qwen3.5-9B-Q6_K (architecture `qwen35`)
and matches the hook point Qwen-Scope SAEs are trained on. Same tensor
name regardless of whether the block is attention-based or
DeltaNet/SSM-based, so all 32 layers of Qwen3.5-9B are accessible.

**Wire format rationale:** base64 over JSON arrays. Empirically a single
layer of 84 tokens × 4096 dims is 1.4 MiB raw float32, 1.8 MiB as base64,
or 7 MiB as a JSON array of floats — and lens-as-PRM scoring fires this every N tokens
during generation. JSON would push 50+ MiB per multi-layer scoring call;
base64 keeps it manageable.

### GET /health

```bash
curl http://localhost:8080/health
# {"status":"ok"}
```

> llama-server exposes many more endpoints. See the [llama.cpp server docs](https://github.com/ggml-org/llama.cpp/blob/master/tools/server/README.md) for the full API reference.

---

## Versioning and error codes

`GET /version` returns the API version, the SSE protocol version, the
full error-code set, the tool-call grammar mode this proxy applies
(`strict` or `loose`, from `ATLAS_GRAMMAR_MODE`), and the session limit in
seconds (`ATLAS_AGENT_SESSION_TIMEOUT_SEC`, default 600), so a measurement can
record the configuration it ran against:

```json
{"api_version": "1.0.0", "protocol_version": 1, "error_codes": [...], "grammar_mode": "strict", "session_timeout_s": 600}
```

`api_version` follows semver (minor = additive, major = breaking).
Error responses use a stable envelope — **switch on `error` (a closed
code set), never on `detail`** (the human message may change):

```json
{"error": "unauthorized", "detail": "...", "api_version": "1.0.0"}
```

Codes: `unauthorized`, `invalid_input`, `unsupported_operation`,
`dependency_unavailable`, `resource_limit`, `internal_error` — the
closed set is exactly what live `writeError` calls emit
(`AllErrorCodes` in `proxy/main.go`). Machine-readable schemas live in `docs/schemas/`: the full
OpenAPI 3.1 spec for this surface (`proxy_openapi.yaml`, parity-checked
against the registered routes in CI) plus JSON Schemas for the error and
SSE envelopes.

---

## Workspace path

`GET /workspace` returns the host and container paths the connected
proxy has mounted, so a client can detect whether its own workspace
folder matches what the proxy is actually editing:

```json
{"project_dir": "/home/anuj/atlas-proxy", "working_dir": "/workspace", "containerized": true}
```

`project_dir` is the **host** path (from `ATLAS_PROJECT_DIR`) — empty
string when the proxy can't report an absolute path (unset, or a
relative default like `.`), never a raw relative value. `working_dir` is the path the proxy writes to inside its own process
(from `ATLAS_WORKSPACE_DIR` — `/workspace` under Compose, the launch
cwd for a locally-spawned proxy). `containerized` is `true` when
a container runtime's marker file exists (`/.dockerenv` for Docker,
`/run/.containerenv` for Podman) or `KUBERNETES_SERVICE_HOST` is set (a
Kubernetes or K3s pod); `working_dir` is not a container signal, since
the local launcher sets it too.

This endpoint requires the service token like any other route — it
discloses a host filesystem path, so it is deliberately **not** in the
open-path exemption list (`/health`, `/ready`, `/version`).

## Building a non-TUI client

A minimal client needs four things (plus one header):

0. **Send the service token** on every request when the installation
   has one (`secrets/service-token`, generated by `atlas init`):
   `Authorization: Bearer <token>`. Without it, all proxy routes except
   `/health`, `/ready`, and `/version` return 401 on token-bearing
   installs. Reading the file at startup is enough — rotation restarts
   the stack.

1. **POST `/v1/agent`** with `{message, working_dir, mode, session_id}` and parse the SSE stream. See the Python example above.
2. **Answer `permission_request` events.** In `default`/`accept-edits` mode the turn pauses on destructive tools until you **POST `/v1/permission`** with `{session_id, tool_call_id, decision:"allow"|"deny", scope:"once"|"session"}` (echo `tool_call_id` from the event). Unanswered requests deny after `ATLAS_PERMISSION_TIMEOUT_SEC` (default 600s). Unattended clients skip this by using `mode:"yolo"` or pre-approving tools via `session_allowed_tools`.
3. **POST `/cancel`** with `{session_id}` when the user wants to abort.
4. *(Optional)* **GET `/events`** in a background goroutine/thread for the global typed-envelope feed if you want a pipeline-progress sidebar.
5. *(Optional)* **GET `/workspace`** to compare the proxy's mounted
   host path against your own workspace root, and warn the user if
   they differ (a "wrong folder open" check). Treat a missing or
   unreachable response as "can't tell" — older proxies won't have
   this route yet.

The TUI ([atlas tui](CLI.md)) is a Go reference implementation — its `model.go`/`events.go` show how to handle every event type, and `panes.go` shows one approach to rendering them. Browse `tui/` in the repo for a complete worked example. The VS Code extension (`extensions/vscode/`) is a TypeScript client built exactly to this section's surface, with unit-tested SSE parsing and permission handling.

This document and the TUI source are the canonical reference for building a client.
