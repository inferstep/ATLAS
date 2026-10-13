> **English** | **[简体中文](lang/zh-CN/ARCHITECTURE.md)** | **[日本語](lang/ja/ARCHITECTURE.md)** | **[한국어](lang/ko/ARCHITECTURE.md)**

# ATLAS Architecture

System architecture for ATLAS V3.1.6. Two-layer design: an outer agent loop handles tool-call orchestration, and an inner V3 pipeline generates diverse code candidates with build verification and energy-based selection.

---

## 1. System Overview

```mermaid
graph LR
    User["User"] --> TUI["atlas-tui\n(Bubbletea)"]
    TUI --> Proxy["atlas-proxy\n:8090"]

    subgraph outer["Outer Layer"]
        Proxy -->|"grammar JSON"| LLM["llama-server\n:8080"]
        Proxy -->|"T2 files"| V3Service["v3-service\n:8070"]
    end

    subgraph inner["Inner Layer"]
        V3Service --> LLM
        V3Service --> Lens["geometric-lens\n:8099"]
        V3Service --> Sandbox["sandbox\n:30820"]
        Lens --> LLM
    end

    style User fill:#333,color:#fff
    style TUI fill:#1a3a5c,color:#fff
    style Proxy fill:#1a3a5c,color:#fff
    style LLM fill:#5c1a1a,color:#fff
    style V3Service fill:#2d5016,color:#fff
    style Lens fill:#2d5016,color:#fff
    style Sandbox fill:#2d5016,color:#fff
```

Services run as containers via Docker Compose (recommended) or as local processes via the `atlas` launcher. Only llama-server uses the GPU. Everything else runs on CPU.

The chat front-end is the **atlas-tui** (Bubbletea): a native Go terminal UI consuming `/v1/agent` (per-turn chat SSE) and `/events` (global typed-envelope feed for the pipeline pane). Launch with `atlas` (interactive default) or `atlas tui` (explicit). Pipeline pane shows V3 stages live; chat pane renders assistant markdown via glamour; slash commands `/add /diff /commit /run` etc. handle local file context and shell-out. Mode-aware input (chat / `!bash` / `/slash`) with a hint dropdown.

Third-party clients that want tool calls + V3 pipeline target `/v1/agent` directly; `/v1/chat/completions` is a passthrough to llama-server (see §3). The contract is documented in [API.md](API.md).

### 1.1 Supported Accelerators

llama-server is the only GPU-using service; every other ATLAS service runs on CPU (proxy is Go, v3-service / geometric-lens / sandbox are Python). That keeps the multi-backend surface small — adding a new accelerator means a new Dockerfile + an entrypoint env-var branch, not changes to the pipeline.

| Backend | Status (V3.1.x) | Image / build path | Compose override | Tested cards |
|---|---|---|---|---|
| **CUDA** (NVIDIA) | Supported (since V3.1.0) | `inference/Dockerfile.v31` → `atlas-llama` | (default) | RTX 5060 Ti 16GB (canonical). The published image is compiled for Blackwell (compute capability 12.0/12.1) only; earlier generations need a local rebuild — see [SETUP.md](SETUP.md) |
| **ROCm / HIP** (AMD) | Community-tested (since V3.1.1) | `inference/Dockerfile.rocm` → `atlas-llama-rocm`, built on the host (`pull_policy: build`; no GHCR image) | `docker-compose.rocm.yml` | RX 7900 XTX (community smoke-test, GH #26) |
| **Metal** (Apple Silicon) | Supported ([#32](https://github.com/inferstep/ATLAS/issues/32)) | Hybrid: native llama-server (Metal) + Docker for the rest (macOS can't passthrough GPU to containers) | `docker-compose.macos.yml` | M-series; Q4_K_M on ≤16 GB, Q6_K on ≥24 GB unified |
| **Vulkan** (cross-vendor fallback) | Preview | `inference/Dockerfile.vulkan` → `atlas-llama-vulkan` | `docker-compose.vulkan.yml` | lavapipe CPU boot path (smoke-tested); no real-GPU validation yet |
| **SYCL** (Intel Arc) | Roadmap — Intel Arc uses `vulkan` today | TBD | TBD | — |

**Backend selection happens at install time, not runtime.** `atlas init` runs `tier.detect_gpu()` (see `atlas/commands/tier.py`), picks the largest-VRAM GPU across all detected vendors (override with `ATLAS_GPU_VENDOR` / `ATLAS_GPU_INDEX`), and writes `ATLAS_BACKEND={cuda|rocm|metal|vulkan}` into `.env`. Detection resolves to the packaged native backend when one exists: CUDA for NVIDIA, ROCm for AMD on x86_64, the hybrid Metal path on macOS. When no native backend is packaged for the host (Intel Arc, AMD on arm64, unrecognized vendors), the wizard offers the Vulkan universal fallback (default-yes): one image covers AMD, Intel, Adreno, MoltenVK, and the lavapipe CPU rasterizer, at roughly 20–40% below a tuned native backend. It refuses — rather than writing a `.env` that won't boot — only when nothing usable exists. Each backend has its own image — CUDA and Vulkan prebuilt on GHCR, ROCm compiled on the AMD host at first `up` — so users don't run a fat image that ships every backend's libraries.

**Bring-your-own-model surface (V3.1.1).** `atlas lens check` is a cheap pre-flight against a running llama-server that reports whether the loaded model is Lens-compatible. `atlas lens build --samples <path>` wraps `geometric-lens/geometric_lens/training.py` to train fresh C(x) (`cost_field.pt`) **and** G(x) (XGBoost) artifacts at the model's native embedding dim. Together they let users swap in non-default GGUFs without forking the lens code — the C(x) constructor accepts arbitrary `input_dim`, so the only thing that changes per-model is the trained weights. See [CLI.md § atlas lens](CLI.md#atlas-lens) for the user-facing flow; `atlas lens publish` (or the combined `atlas publish`) uploads the artifacts to HuggingFace and opens the registry PR that pins their hashes.

**What's vendor-agnostic** (works on every backend): grammar-constrained JSON, self-embeddings (`/embedding`), per-layer hidden states, ASA control vectors (loaded by llama.cpp's `control_vector_load` regardless of backend), KV cache quantization, the entire outer agent loop, V3 pipeline, Geometric Lens, and sandbox.

**What differs per backend:**
- **Flash attention.** CUDA + ROCm: full support. Metal: limited (llama.cpp Metal backend supports flash-attn for some head sizes; defaults to off if unsupported). Vulkan: driver-dependent.
- **Pinned host memory.** `GGML_CUDA_NO_PINNED` applies to CUDA + ROCm (HIP mirrors the CUDA path at the GGML compat layer). Metal/Vulkan don't use the CUDA/HIP pinning path.
- **Multi-GPU + tensor parallelism.** V1 supports single-GPU only on every backend; multi-GPU is GH #34, not bound to a specific vendor.
- **Apple unified memory.** macOS shares GPU+system memory; "VRAM" math is actually "16 GB total minus OS + apps." See §7.

The K3s deployment path (`scripts/install.sh`, manifests in `templates/`) is CUDA-only as of V3.1.1 — ROCm K8s recipe is deferred to the V3.2 infra list (needs `/dev/kfd` + `/dev/dri` hostPath mounts and `render`/`video` group membership, the cluster-level equivalents of `docker-compose.rocm.yml`).

---

## 2. Services

| Service | Port | Language | Purpose |
|---------|------|----------|---------|
| **llama-server** | 8080 | C++ (llama.cpp) | LLM inference (CUDA / ROCm / Metal / Vulkan; SYCL on roadmap — see §1.1), grammar-constrained JSON, self-embeddings, per-layer residual hidden states |
| **atlas-proxy** | 8090 | Go | Agent loop, tool-call routing, tier classification, `/v1/agent` SSE, `/events` typed SSE, `/cancel`. `/v1/chat/completions` forwards to llama-server with only a `max_tokens` clamp applied (see API.md). |
| **atlas-tui** | (client) | Go | Bubbletea TUI; consumes `/events` and `/v1/agent` SSE streams. |
| **v3-service** | 8070 | Python | V3 pipeline HTTP wrapper (PlanSearch, DivSampling, PR-CoT, etc.) |
| **geometric-lens** | 8099 | Python (FastAPI) | Internal `/internal/*` scoring service: C(x) energy scoring, G(x) XGBoost quality prediction, per-step scoring |
| **sandbox** | 30820 (host) / 8020 (container) | Python (FastAPI) | Isolated code execution, compilation, linting, test running |

---

## 3. atlas-proxy (Outer Layer)

The proxy is the entry point for chat front-ends. It accepts user messages on `/v1/agent` (typed event stream — what the TUI uses) and runs an internal agent loop that calls llama-server, parses tool calls, executes them, and streams events back. The `/v1/chat/completions` endpoint is a transparent passthrough to llama-server; it is kept for SDK compatibility and does not run the agent loop. See [API.md](API.md) for the full event-type catalogue.

Every non-test Go file directly under `proxy/`, with its area of ownership:

| File | Owns |
|---|---|
| `main.go` | HTTP server, routes, auth, passthrough, error envelope, private-value log filter |
| `agent.go` | The agent loop: turn state, LLM calls, plan generation, stuck-loop breakers |
| `tools.go` | The 16 tool definitions + executors, tier classification, tool-call grammar |
| `gates.go` | Honesty/plan gates: claim-check, structural, syntax, embedded-script, plan-adherence, plan-reminder, asset lint |
| `detectors.go` | Stuck-pattern detectors: tool repetition, reasoning repetition, traceback localization |
| `context.go` | Context enrichment: symbol index, project scan, workspace containment, session file manifest |
| `permissions.go` | Permission gate (`/v1/permission`), trust mode, hard-blocked patterns |
| `lens.go` | Lens scoring calls, the `VerificationRecord` the loop keeps for each verification run (passing or failed, with its evidence kind), calibration status |
| `lens_identity.go` | Creates and attaches the proxy-owned invocation identity used for direct Lens service calls |
| `lens_required.go` | Enforces that the geometric Lens is available, tracks Lens failures, and stops runs when scoring is unavailable |
| `logsafe.go` | Builds privacy-safe tool argument summaries for logs, allowing only approved fields and redacting sensitive content |
| `memory_envelope.go` | Validates declared deployment memory budgets against the operator-stated host capacity and reports unenforced budget overruns |
| `object_handle_linux.go` | Holds Linux filesystem objects open during deletion approval so the approved object cannot be replaced before removal |
| `object_handle_other.go` | Refuses deletion approval on non-Linux platforms because a filesystem object cannot be safely pinned and verified |
| `mutation_scope.go` | Derives the exact file and edit scope from model tool calls so candidates cannot modify anything outside the requested target |
| `obligation_kinds.go` | Defines the closed obligation types, required evidence strength, and how each obligation is satisfied |
| `obligations.go` | Chooses whether run obligations come from the caller’s declared contract or ATLAS’s legacy inference, and keeps that decision bound to the request |
| `command_evidence.go` | What a shell command demonstrates: execution, probe, static check or nothing, and whether the command line reports that part's exit status |
| `guardrails.go` | Per-tool steering guards (shrinkage, missing-command/module steers, doctype strip) |
| `events.go` | Typed-envelope broker (`/events`) and SSE plumbing |
| `v3_bridge.go` | SSE client for v3-service `/v3/generate` + `/v3/plan` |
| `types.go` | Shared types, tiers, turn caps |
| `advisory_policy.go` | Decides whether to keep the model’s proposal or deliver a candidate, applying hard vetoes before verification and selection rules |
| `authorization_decision.go` | Checks whether candidate evidence satisfies the task’s authorization requirements, including target scope and preservation of the existing file’s verified behavior |
| `authorization_grant.go` | Creates, consumes, and retires one-use permissions bound to an exact candidate, target, and workspace state |
| `automatic_attribution.go` | Records why automatic candidate delivery succeeded or was refused, using the decisions already made by the route, authorization, policy, and delivery owners |
| `automatic_delivery.go` | Checks whether the exact V3-selected candidate meets the safety and identity requirements for automatic delivery |
| `candidate_capture_only.go` | Prevents candidate delivery during capture-only measurement runs and records the suppression |
| `candidate_delivery.go` | Authorizes candidate delivery, consumes its one-use permission, rechecks current workspace state, and writes the approved bytes |
| `candidate_policy.go` | Defines the fixed candidate policy, its possible outcomes, and the private record of each decision |
| `candidate_provenance.go` | Reports where delivered content came from and which policy allowed it to reach disk |
| `candidate_reachability.go` | Determines and records why a mutation bypassed candidate generation, including eligibility and remaining time |
| `candidate_staging.go` | Runs client-declared verification commands against candidate files in an isolated sandbox copy and checks the returned observations |
| `cut_call.go` | Builds recovery guidance from complete fields in a truncated tool call and the current file, without executing the incomplete call |
| `delivered_handoff.go` | Updates the model’s file context and tool feedback when the delivered content differs from what it submitted |
| `delivery_settlement.go` | Records completed candidate deliveries and checks that the authorized artifact still exists with matching bytes and ledger state |
| `edit_route_delivery.go` | Carries edit-tool candidates through authorization and delivery, preserving the originating tool’s result format and fallback content |
| `evidence_wiring.go` | Connects syntax and declared-command evidence producers to candidate routes, assigns candidate identities, and records available or missing evidence |
| `execution_outcome.go` | Classifies command completion, interruption, and resource exhaustion so an incomplete run cannot count as successful verification |
| `exploration_budget.go` | Prompts a run to act or answer after repeated read-only calls, according to what the request requires |
| `feasibility_decision.go` | Assesses whether available evidence producers could satisfy the task before generation and records the assessment without controlling generation |
| `fenced_framing.go` | Recognizes complete fenced file payloads, extracts their contents, and supplies framing grammar and retry guidance |
| `old_str_watch.go` | Stops runaway edit text while it streams when it can no longer match the target file, and supplies recovery guidance |
| `repair.go` | Tracks syntax repairs left open by session writes, blocks premature completion, and reports unresolved repairs at handoff |
| `reply_lead_ins.go` | Normalizes introductory phrases so completion checks can recognize replies that merely announce future work |
| `route_disposition.go` | Tracks how each candidate route ended and whether its delivery permission resulted in content reaching disk |
| `route_entry.go` | Creates a unique identity for each candidate-generation attempt so its evidence, authorization, and delivery records can be linked |
| `staged_command_evidence.go` | Credits staged verification toward completion only while its candidate bytes, workspace, and delivery remain current |
| `staging_contract.go` | Defines and validates sandbox staging requests, observations, execution budgets, and their identity bindings |
| `structured_mutation_target.go` | Checks whether a parsed mutation call grounds candidate delivery to its exact target when the client has not declared output paths |
| `syntax_evidence.go` | Turns proxy syntax-check observations into evidence bound to exact candidate bytes and structural obligations |
| `v3_delivery_registry.go` | Lists the production routes and originating tools allowed to deliver service-authored candidate content |
| `v3_fallback_note.go` | Tracks files written after V3 failed or timed out and discloses still-current unchecked content in the final summary |
| `verification_evidence.go` | Produces evidence from client-declared command observations and tracks which individual verification obligations they satisfy |
| `verification_requirements.go` | Validates typed client verification requirements and limits evidence strength according to the declared check and the origin of its test assets |
| `workspace_observation.go` | Observes file changes made by foreground and background commands and records deliverables and deletions in the session ledger |

### Agent Loop Flow

```mermaid
flowchart LR
    Start["User msg"] --> Build["Build prompt"] --> Call["llama-server"] --> Parse["Parse JSON"]
    Parse --> Route{Type?}

    Route -->|"tool_call"| Tier{"T2?"}
    Tier -->|"Yes"| V3["V3 Pipeline"] --> Result["Append result"]
    Tier -->|"No"| Exec["Execute tool"] --> Result
    Result --> Budget{"Budget?"}
    Budget -->|"< 4"| Call
    Budget -->|"4"| Warn["Nudge: write now"] --> Call
    Budget -->|"5+"| Esc["Escalated nudge"] --> Call

    Route -->|"text"| Stream["Stream"] --> Call
    Route -->|"done"| Done["End"]

    style Start fill:#1a3a5c,color:#fff
    style Done fill:#333,color:#fff
    style V3 fill:#2d5016,color:#fff
```

### Grammar Enforcement

Every model output is constrained toward one of three valid JSON shapes:

```json
{"type": "tool_call", "name": "<tool_name>", "args": {...}}
{"type": "text", "content": "<message>"}
{"type": "done", "summary": "<summary>"}
```

In the default `strict` mode the proxy sends a full JSON schema — `oneOf` with `additionalProperties: false`, tool names enumerated from the registry — which llama-server enforces as a grammar during token generation. Grammar constraints make malformed output rare, not impossible: `ATLAS_GRAMMAR_MODE=loose` sends `{"type":"json_object"}` only (valid JSON, no shape enforcement — some models require it), and the response token cap can truncate mid-JSON. The proxy treats parsing as fallible — it recovers JSON from prose/`reasoning_content`, detects truncated tool args before execution, feeds targeted parse-failure descriptions back, and breaks the loop after three consecutive failures.

**The fenced-content channel (`@fenced`).** Whole-file bodies do not ride the JSON channel. Code inside a JSON string pays escaping pressure on every dense line, and a served 12B measurably cannot sustain it: the same solution parses 6/6 emitted in a fenced markdown block and 0/6 emitted as a JSON string (lost close-parens, literal `\n` fusing statements into comments, joins losing their spaces). The system prompt instructs the model to set `write_file`'s `content` to exactly `@fenced`; the proxy then makes one *unconstrained* sub-call (no grammar, no `response_format`) whose reply is the file in a single fenced block, tagged for the target extension (` ```python `, ` ```html `, …). The envelope stays under the JSON constraint; only the file body moves to plain text. Details of the mechanism (`proxy/agent.go::fetchFencedContent`): the sub-call is ephemeral (never appended to the conversation — from the main thread's view the model wrote `@fenced` and the write happened); a reply whose file itself contains ` ``` ` is extracted by a trailing-fence anchor so interior fences don't cut it; a model that puts the sentinel *and* the file in one string has the sentinel stripped and the inline content used; two attempts, then a bounce asking for inline content. Every attempt is a real generation and is accounted in the run totals (`fenced_calls` / `fenced_tokens` on the final `done` payload, one `fenced_fetch` stage event per attempt).

The repetition detector fingerprints the call the **model** sent, snapshotted at the top of the tool-call branch before fenced resolution rewrites `parsed.Args` with the fetched body. Without that snapshot the detector reads bytes that differ on every attempt while the call itself is byte-identical, which is why the seven-turn `@fenced` loops in the frozen run reached the harness cap with the detector silent. The fetched bytes are unaffected: they still drive the mutation, the gates, the ledger and the write. Target paths are cleaned for the signature so `solve.py` and `./solve.py` cannot buy separate budgets.

When a path's fenced allowance is spent and the model asks for that channel again, the refusal is made once into something it can act on: the canonical path, the current source when the file is readable (bounded, or an explicit note that it is not on disk), and the alternatives that still work — the whole file inline, a targeted edit, a read, or a model-issued `run_command`. Offered once per canonical path; an alias, a new turn, or an unrelated success cannot re-open it. It starts no generation, runs no command, and forces no tool. A model that keeps asking meets the ordinary failed-call accounting and stops.

When the run-first gate has already demanded that a warned file be run and the model answers with the same raw `@fenced` write, repeating the demand is not a mechanism. On that recurrence the model is handed the file as it actually is — canonical path, up to 120 numbered lines, and the current failed verdict if the ledger holds one for those exact bytes — and the useless call is held back. It runs before fenced resolution, so a blocked repeat costs no generation; it reads and never writes, runs no command, forces no tool, fires at most once per canonical path, and is skipped when less than 90 s of work budget remains. If the model repeats the blocked intent anyway, the existing ban and repeat detector end the run honestly.

The run-first gate itself quotes one instruction, `runFirstInstruction`: the command that runs a file of that extension (`runCommandFor`: `python3`, `node`, `go run`, `java`, or a `kotlinc … && java -jar` pair for Kotlin), sent through `run_command` for a script that exits and through `run_background` for a script that serves (the same `runsAServerLoop` check the foreground-server redirect uses). Both tools discharge the warned mark when their command executes the file, so a server script started in the background and read through its settle-window traceback counts as run. `python3 -m <module> file.py` and `python3 -c ...` hand the file to another program and are never treated as starting its server loop. Measured 2026-09-14 before these three rules: the gate demanded `run_command python3 app.py`, the redirect refused it, `run_background` never discharged the mark, `python3 -m py_compile app.py` was refused as a server start, and the session ended with the SyntaxError still on disk.

Because each attempt costs a generation, the channel only opens for a call that can execute. `fencedCallIsExecutable` runs the checks the call has to survive anyway — argument shape, a nonblank path, `validateToolWorkspacePaths`, `shouldDenyToolCall` — by calling them, so its refusals are a subset of `executeToolCall`'s. An unusable call is simply not resolved: the sentinel stays in `content`, the tool refuses it with its own message and `MutationNone`, and the session spends one turn instead of a generation. Measured before the check existed: a 300 s canary reached turn 36 having written nothing, every turn a `write_file` with `content: "@fenced"` and no path, each one opening the channel and each failure recorded against an allowance key for the empty path.

The fenced sub-call can stall: the stream opens and then sends nothing while the server generates out of sight. A watchdog cuts it (`fencedStalledTimeout`, ~25 s). The stall is a property of the session, not the file, so once one file's fetch stalls the next file's fetch stalls the same way. After the first stalled fetch the channel is turned off for the rest of the run (`ctx.FencedStalls` past `fencedSessionStallLimit`, tested by `fencedChannelDisabledForSession`): the next `@fenced` returns immediately with no sub-call, and the model is steered to write the file inline. Inline is the safe fallback because the write-integrity checks below now catch the truncation that `@fenced` first existed to avoid.

### Write-integrity checks

Three checks keep a broken file from landing as a clean write. Each names the real cause so the model's re-send can fix it.

**A write cut short by an unescaped quote is refused.** A `write_file` whose `content` holds an unescaped `"` ends its JSON string early; a lenient decoder then reads the rest of the file into a discarded key and lands a truncated file that reports success. The loop checks a parsed edit-tool call for a key outside the tool's real signature (read by reflection off the input schema) that carries the shape of leaked file content — long, or holding code punctuation — and refuses it (`swallowedContentFeedback`), telling the model the string ended early and to escape the inner quotes or use `structural_edit`.

**A Jinja template that would 500 on every render is caught at write time.** A template can parse as HTML and still fail on render: `{% for x in xs %)` closes the tag with `)`. `html.parser` passes it and the server starts, but every render raises `TemplateSyntaxError`. The sandbox syntax check parses the Jinja too, scoped to files that are actually templates (a `templates/` directory or a `.jinja`/`.jinja2` name) and only when a `{%` statement tag is present, so Vue/Angular HTML that shares `{{ }}` is never handed to a Jinja parser; unknown-tag/filter errors (a third-party extension) are dropped. The proxy sends the file path so the sandbox can apply that scope, on both the direct-write gate and V3's compile smoke check.

**Interactive tasks skip V3 repair when the baseline already compiles.** V3's repair phase is reached only when no generated candidate passed. For an interactive task the only signal is "does it compile" (a server cannot be run to completion in the sandbox), so the sole compiling code at that point is the model's own write, and a repaired-to-compiling candidate is no better verified than a compiling baseline. When the baseline compiles, repair is skipped and the baseline is handed back, returning the budget to the agent loop instead of spending ~50% of the session re-deriving it. Algorithmic tasks keep repair: they can be executed, and their model-generated self-test results are recorded as diagnostics.

### Tools

16 tools registered in `proxy/tools.go`:

| Tool | Purpose | Read-only |
|------|---------|-----------|
| `read_file` | Read file contents (with optional offset/limit) | Yes |
| `outline_file` | List a file's top-level functions/classes with line ranges, no bodies (tree-sitter for `.py`, best-effort scan otherwise). Locates a target inside a file too large to read whole: outline, then `read_file` with offset/limit. Returns no code, so it cannot ground a diagnosis — `read_file` is the default entry point | Yes |
| `write_file` | Create a NEW file (rejected for existing files >5 lines — see safety limits) | No |
| `edit_file` | Surgical inline string replacement (old_str/new_str) for ≤10-line changes | No |
| `insert_after` | Insert new lines after a given line number — the line numbers `read_file` prints. For ADDING code (a branch, function, import) where nothing existing changes: there is no `old_str` to reproduce, which is the step that fails on long spans | No |
| `replace_lines` | Replace a line range (`start_line`..`end_line`, as `read_file` prints them) with new content. For CHANGING code without reproducing it: the anchor is two asserted lines (first and last of the range, whitespace-insensitive) rather than the whole span, so the verbatim burden is 2 lines instead of N. Capped at 60 lines per call — a whole JavaScript function inside a template is the unit of work, and it does not fit in less | No |
| `structural_edit` | Whole-function/class/HTML-element rewrite via tree-sitter selector (`function:NAME`, `class:NAME`, `<tag>`); REQUIRED over edit_file for whole-node swaps. GH #39, .py/.html/.htm only in v1 | No |
| `delete_file` | Delete file or empty directory (forces loop exit after) | No |
| `move_file` | Move or rename a file within the workspace (e.g. `index.html` → `templates/`). Pure relocation — bypasses the V3/surgical-edit gate, refuses to clobber an existing destination. The supported path for "reorganize the files" since shell `mv`/`cp` are refused | No |
| `find_file` | Regex search by file **name** / path (cheap existence + locate). Distinct from `search_files` which greps inside file contents. | Yes |
| `search_files` | Regex search across file contents (max 200 matches, skips .git/node_modules) | Yes |
| `list_directory` | List directory contents with type and size | Yes |
| `run_command` | Execute shell command via sandbox container; 5 min timeout cap. A command the shell cannot parse at all returns why it could not run: when the cause is a program quoted inline after `-c`/`-e`, the rejection redirects to writing the snippet to a file, since a raw `syntax error near unexpected token` gives the model nothing to change and it re-sends the identical line | No |
| `run_background` | Start a long-running process (e.g. `python app.py`) in the sandbox; returns a `job_id` immediately | No |
| `tail_background` | Fetch new stdout/stderr from a backgrounded job by `job_id` | Yes |
| `stop_background` | SIGTERM/SIGKILL a backgrounded job by `job_id` | No |

### Deliverable ledger (observational)

`executeToolCall` records what each call did to the session's deliverables, in
`AgentContext.Ledger` keyed on the resolved workspace path. It observes and
decides nothing: no restoration, no refusal, no event. `ToolEffect` picks the
treatment.

| Effect | What is recorded |
|--------|------------------|
| direct mutation (`write_file`, `edit_file`, `structural_edit`, `insert_after`, `replace_lines`) | the file is re-read and hashed after the call, so the recorded hash names the bytes on disk rather than the bytes the tool proposed |
| `delete_file`, `move_file` | a tombstone once the path is actually gone, retaining any checkpoint bytes and prohibiting automatic restoration; a move observes its destination fresh and transfers no verdict |
| `run_command` | every tracked path is rehashed: unchanged files keep their verdict, changed ones lose it. The workspace is also walked before and after (`applyShellChanges`, stat-only, skipping dependency, cache and build directories): a file of a kind completion can judge that the command created or changed enters the ledger as the session's work; a file that was there when the request started and that the command removed is tombstoned `deleted:shell`, so completion reads it as a deletion nobody approved; a file the run created and a later command removed leaves the ledger. A walk that hits its cap leaves a caveat in the summary |
| `run_background`, `stop_background` | a workspace hazard is raised on start and lowered only on a reaped exit code. Each call is walked before and after like `run_command`, and the walk after the call that started the first live job is kept as a baseline. Once no job can still be writing (completion reaps an exited job, `stop_background` confirms an exit, or the session reaps its jobs at the end), the workspace is compared with that baseline under the same rules; a removal the ledger already records (`delete_file`, `move_file`, an earlier walk) is not charged to the job |

A branch that proves it mutated nothing (`MutationNone`) records nothing, so a
refused write to a path the session never owned does not enter the ledger.

`ValidationKind`/`ValidationStatus` are copied from the tool's own result and
bound to the hash they describe; `CurrentValidation()` returns unknown as soon
as the file's hash moves away from the validated one. Bytes are checkpointed
only on an explicit `passed` for that exact hash, under a 256 KiB per-file and
2 MiB per-session ceiling. Over the ceiling the observation is kept, the bytes
are not, and `checkpoint_unavailable` records why.

Which mutators can leave a checkpoint follows from whether they produce an
explicit pass for the bytes they wrote, measured by
`TestWhichMutatorsCanEverPromoteACheckpoint`:

| Tool | Reaches `passed` | Note |
|------|------------------|------|
| `write_file`, `edit_file`, `insert_after`, `replace_lines` | yes | via the syntax gate, when the sandbox is reachable |
| `structural_edit` | no | the tree-sitter splice is v3-service's; this tool runs no check of its own on the bytes it writes, so it reports `syntax`/`not_run` |
| `delete_file`, `move_file` | no | neither changes content, so neither has a verdict to give |

Every branch of the seven mutators carries its own `MutationStatus`: the
boundary is structurally forbidden from classifying a direct mutator, so an
unclassified branch would be indistinguishable from a deliberate no-op.
`noMutation` / `errNoMutation` mark a branch that formed no bytes to write,
`refusedNoCheck` a guard that declined bytes that were ready, and
`errFailedMutation` a write that was attempted and left the target
indeterminate. `TestEvery*OutcomeIsClassified` covers the families.

#### What the model sees

Classification is a server-side fact. `ToolResult.ModelFacing()` projects a
result down to `success` / `data` / `error` plus the V3 provenance fields, and
`MarshalText` — the only path from a result into `ctx.Messages` — goes through
it. The full struct still marshals with every field for internal use; the
projection is an allowlist, so a field added to `ToolResult` stays out of the
conversation until someone decides otherwise.

Three tests hold the boundary in `proxy/result_contract_test.go` and
`proxy/tool_effect_test.go`: `TestEveryModelFacingSerializationSiteIsInventoried`
fails on a new direct marshal or a tool message built by an uninventoried
route, `TestModelFacingTextCarriesNoClassification` pins the projected bytes
per outcome, and `TestModelPromptBytesAreUnchangedByClassification` runs the
agent loop over two branches whose classification changed and compares the
request bodies against the parent commit.

Two limitations are carried, not inferred around. `structural_edit` reports
`syntax`/`not_run` on success and therefore cannot promote a checkpoint;
`move_file`'s cross-filesystem copy path is production-reachable but has no
production-path test, since one temp directory cannot straddle two
filesystems.

#### Restoration

One terminal has recovery wired to it: the repeat detector's stop, which is
where a run ends holding a deliverable it has itself shown to be broken. The
other twelve `done` emitters are untouched — a terminal that demonstrated
nothing has nothing to recover from — and
`TestRestorationIsWiredToExactlyOneTerminal` fails if that spreads.

At that terminal each deliverable is re-read, re-checked through the same
syntax contract the write path uses, and decided on its own. Every clause is a
reason not to act:

| Required | Declines when |
|----------|---------------|
| this session wrote the path | it was never a deliverable here |
| current bytes freshly re-read and re-checked | the checker could not run, so the verdict is unknown |
| that exact hash carries a demonstrated failure | the current contents parse, or were never checked |
| held bytes exist, in bounds, hashing to their own record | evicted, over-ceiling, or self-inconsistent |
| the held version passed the *same kind* of check | a structural pass is not evidence about syntax |
| the two versions differ | the file already holds the safer version |
| not tombstoned, not restoration-prohibited | the model deleted or moved it on purpose |
| no live background hazard | a background job may still be writing |

The write goes through `atomicReplaceFile`, the same write-then-rename the
mutating tools use, and the result is re-read and hashed: a restore that
cannot prove it landed exactly is a failure that preserves the real error and
leaves the current bytes alone. Recovery sets no progress hint, registers no
session write, emits no tool event, claims no V3 provenance, and never turns a
stopped run into a completed one — the summary discloses per path which files
were put back, which were left alone and why, and which could not be restored,
with no implication that they moved together.

#### Terminal contract and the session budget

Every session ends in exactly one terminal event, from one emitter. It carries
the legacy `summary` unchanged plus an additive `status` — `completed`,
`incomplete`, `stopped`, `timed_out`, `failed` — and a stable machine-readable
`reason`. Consumers read absent, malformed or unrecognised as `incomplete`,
never as completion (`NormalizeTerminalStatus` in Go, `terminal_status` in
`atlas/events.py`). The broker's `done`/`stage_end` envelopes report the same
outcome instead of asserting `success: true`.

For any status other than `completed`, the server owns the sentence as well as
the field. The model's own account is passed through only where the completion
gate authorised it; a non-completed terminal with no summary gets a
server-composed one naming the outcome, whether anything is on disk and whether
it was shown to be valid; and a completion claim that reaches the emitter on a
non-completed status is replaced outright rather than edited around. This
closes the gap Phase 2B left: a client that reads only `summary` — every client
written before `status` existed — now reads the same truth as one that reads
both.

A call refused by the pre-execution workspace-boundary check counts as the failed call it is, so a session whose workspace root cannot be opened stops in three turns instead of repeating one refusal until its budget runs out. `workspacePathFields` is the single registry of which arguments each tool treats as a path; the tests enumerate it rather than keeping a second copy.

The workspace hazard is a set keyed by job identity, not a count of start attempts. A `run_background` the tool refused before dispatch owns nothing; a live job owns exactly one hazard that a duplicate id cannot double-raise; a job already gone by the settle window is settled immediately, with the same tracked-path rehash any exit requires; and a start that may have dispatched but returned no usable id raises an unidentified hazard nothing can reap, because a session cannot call the workspace quiet when it does not know what is running in it. Settling one job clears only that job, is idempotent, and cannot underflow. At a `done` or `text` exit with the run's own jobs still live, the exit gate names each job and its `stop_background` call before the completion decision (bounded like the other exit gates), so a server started to verify the work is stopped by the model and its own summary stands; a job the model leaves running still makes the completion `background_work_unresolved`, with the job named in the summary.

A deliverable with no applicable checker can still demonstrate completion, but only as existence-and-currency evidence and never relabelled as a syntax pass: the path must be prose (`isDocumentAsset` — the set `stripOneFenceLayer` has always used), the ledger must already own it, its record must read exactly `none`/`not_applicable`, and that record must describe the bytes on disk right now. An unsupported language, an unknown extension, or a template with embedded content all fall outside it, because "no checker ran" means something different for those than for a text file.

A path the run asked to change and never demonstrably changed is tracked as unresolved work, separately from the deliverable ledger: the ledger records what the session owns on disk, and an intent that never landed owns nothing. It opens where the intent is presented — arguments parsed, path inside the workspace, permission granted, not deny-listed — which is before dispatch, so a fenced resolution that fails is still owed. It closes only on what the ledger can prove about that same canonical path: applied bytes whose current validation passes, or `not_applicable` where no checker applies; a delete closes on confirmed absence; a move needs the source gone and the destination's own bytes validated. Bounded at 64 paths, failing closed past that. When work is still outstanding, the model gets one bounded chance per generation to finish it or explicitly abandon the path with `delete_file`; nothing is written, deleted or run on its behalf, and prose settles nothing.

Both exits — a model-issued `done` and a text reply — reach that decision through one finalizer, in one order: an existing deliverable failure keeps its own more specific reason; otherwise a run that was asked to change something on disk and changed nothing is `incomplete` / `action_demanded_unmet`; otherwise a run that was asked to verify and never did is `incomplete` / `verification_demanded_unmet`; otherwise the completion stands. The predicates are the ones that already rewrite the summary, so the machine-readable half and the prose can no longer contradict each other.

Every exit gate is bounded to three bounces, and a gate whose bounces are spent stops sending the run back, but it no longer stops counting. If its finding still holds at the exit, the gate records it. The findings that are facts about the delivered work end the run `incomplete`, each with its own reason unless an earlier check already refused the exit: `claim_check_unresolved` (a template the code renders does not exist), `warned_file_never_run` (a file written with a parse warning was never run) and `unread_citation` (the reply cites files the run never read). The heuristic gates, which have known false positives, let the run complete, and the summary names what they still found: a form or request target with no matching route, added code nothing calls, unfinished plan steps, a program only ever run with a file piped into its stdin, and bytes that changed after the run that verified them. Where verification was demanded, drift and a probe that reached a server started before the last change leave the run unverified, so it ends `verification_demanded_unmet`. The `done` event carries `unresolved`, the spent gates by name, whenever there are any.

A model-issued `done` is `completed` only when the run's file obligation —
what it declared plus what it wrote — is demonstrably satisfied right now,
through the same syntax contract the write path uses. If anything was deleted
or moved, completion is refused with `delete_intent_unestablished`: whether
removal was the task is not knowable here. The reason names what the
completion rests on: `deliverables_demonstrated` when every deliverable that
can be run (code in an executable language, and HTML pages) was shown working
by a current run, or there is nothing to run; `deliverables_parse_only` when
some of them are only current and parse. Without a work contract, and for
pages, nothing demands the run, so `completed` can rest on a parse; the reason
says so, and the summary says which files nothing ran. A model account that
claims more than that ("all tests pass", "everything works") is shown after
the server's sentence and labelled as unchecked.

The session budget is server-owned: 600 s total, 30 s reserve, so work stops
at 570 s. `ATLAS_AGENT_SESSION_TIMEOUT_SEC` overrides the total within
[120, 3600]; anything malformed, zero, negative or out of range falls back to
the default with a log. Two lifetimes are kept separate — the response context
survives for finalisation while the work context (LLM, tools, gates, V3,
sandbox) ends one reserve early — so the deadline that stops the work does not
kill the channel that explains it. On the deadline the server cancels work,
reaps only this session's background jobs, rehashes tracked paths, runs the
Phase 3B restoration decision unchanged, and emits one `timed_out` terminal
inside the reserve. A client disconnect is not a timeout: work stops and jobs
are reaped, but nothing is claimed into a closed response. Phase 2 fenced
admission observes the work deadline automatically, since it already reserves
against `ctx.Ctx`'s deadline.

For the later reproducibility pass, not fixed here: the system prompt renders
tool descriptions in Go map order, so two runs of identical code produce
different prompt bytes. It affects prompt reproducibility only, not behaviour;
`conversationBytes` in `proxy/tool_effect_test.go` elides the system message
for exactly this reason.

`SessionWrites` is a separate, older map keyed on the raw model-supplied path;
`proxy/types.go` documents the aliasing that follows from that. Its readers are
the write_file overwrite guard's notion of the model's own draft, the active
debug fast path, the session manifest note, V3 project context, the artifact
gate's drift check and the edit-tool leniency gates; none of that changes here.

The work contract's verification demand (`decideVerificationDemand`) takes the
code deliverables it asks about from the ledger, where every mutation tool's
landing is recorded canonically by `recordLedgerEffect`. The coverage that
answers it -- which paths a passing run named, and the bytes they held -- is
computed by `changedPathsForCoverage` in `proxy/guardrails.go`: the session
writes plus the ledger's canonical code deliverables, so the demand and its
coverage read one identity. Before that, coverage read only the session-write
map, which `edit_file` and `structural_edit` never wrote to and no edit tool
wrote to for a delivered candidate, so every edit task under `task_mode: work`
ended `verification_demanded_unmet` whatever the model ran. Coverage states
only that a run named a changed path over its current bytes; completion still
needs that run, a later mutation still re-arms the demand, and the ledger's own
validation still settles mutation debt. Only a segment that ran the program or
its tests, or fetched a page, binds coverage, and only when the command line
reports that segment's exit status (`classifyCommandEvidence`): a parse, a
lint or a build binds nothing, and neither does a test piped into `tail`. A
runner that names no file binds the session's files it discovers
(`runnerEntries`): a bare `pytest` or `pytest tests/` its test files,
`python -m unittest` its `test*.py`, `go run .` its package, `go test ./...`
the packages that contain tests (without tests it only compiles), and `npm
test`, `jest` or `vitest` the files their default patterns match; imports are
followed from there as from a named entry point. A
failed run is recorded as well, and a failure over the same bytes takes back an
earlier pass, so the latest result decides. When the demand is unmet at the
exit, the verification gate says so, with the command that would run the file,
before the finalizer refuses the exit. A cancelled edit reports no mutation
and the ledger observes nothing for it. `structural_edit` records its
validation as not run (it performs no check of its own on the bytes it
splices), so a task whose only mutation is a structural edit ends
`unresolved_mutation_debt` unless something else validates the file: a known,
separate limit.

### Tool-selection bias mitigations

A measured reference deployment showed a bias toward `edit_file` over
`structural_edit` even when `structural_edit` was correct (BiasBusters arxiv 2510.00307 —
embeddings of nearby tool names compete; descriptions matter more than names).
Four model-independent defenses compose in the proxy:

1. **Description rewrite** (`proxy/tools.go`). edit_file's description
   warns against whole-file/whole-function use; structural_edit's description
   says REQUIRED for >10-line / whole-node swaps; write_file's says
   NEW files only.
2. **Conditional GBNF grammar** (`proxy/tools.go`,
   `proxy/agent.go:stepExclusions`). When a write_file is rejected on
   an existing .py/.html/.htm file >5 lines, the next LLM call is
   constrained by a GBNF grammar that bans edit_file and write_file
   from the tool-name production. The model physically cannot emit
   them. Restriction expires after one decision.
3. **Per-step tool-list filter** (same trigger). An ephemeral
   `[system note]` user message is injected reminding the model that
   structural_edit is the only structural-edit tool for this step.
4. **ASA steering vectors** (`geometric-lens/asa_calibration/`).
   Activation steering shifts the residual-stream distribution upstream
   so structural_edit is preferred even on first-attempt decisions before any
   rejection has fired. Auto-loaded by `inference/entrypoint-v3.1.sh`
   from `/models/ast_edit_steering.gguf` only when its `.model` sidecar
   matches the selected model—always-on after a compatible build via the workflow
   in `geometric-lens/asa_calibration/README.md`. Override path/scale/
   layer-range via `ATLAS_CONTROL_VECTOR*` env vars.

   **Per-model coupling.** Each ASA vector is trained
   against a specific model's residual-stream geometry. No cross-model
   fallback is safe. `atlas asa check` verifies the `.model` sidecar, probes
   the loaded embedding dimension, parses GGUF layer metadata, and reports
   `compat` / `needs-build` / `incompatible`. `atlas asa build` derives the
   extraction layer from the loaded model, writes the vector and marker, and
   runs inside the lens container. `atlas asa publish` refuses missing or
   mismatched markers before upload. See [CLI.md § atlas asa](CLI.md#atlas-asa).

### Per-File Tier Classification

Each `write_file`/`edit_file` call is classified independently:

| Tier | Max Turns | Action |
|------|-----------|--------|
| T0 (Conversational) | 12 | Text response only |
| T1 (Simple) | 0 (uncapped) | Direct write — no V3 overhead |
| T2 (Feature) | 0 (uncapped) | V3 pipeline fires |
| T3 (Hard) | 0 (uncapped) | V3 pipeline fires |

The two columns above belong to two different classifiers, and the table reads as one only by coincidence. **Turns** comes from the message tier (`proxy/agent.go:classifyAgentTier`), which scores what the user typed. **Behavior** comes from the file tier (`proxy/tools.go:classifyFileTier`), which scores the file being edited and is what actually gates V3 — the message tier is forwarded to v3-service but only lands in a log line.

Because the turn cap is the same for T1/T2/T3, the message tier has exactly one decision to make: conversational or not. T0 caps the run and skips Plan Mode; every other value behaves identically. It therefore requires positive evidence to call something conversational — a sub-12-character greeting or a question shape — and treats everything else as work. The asymmetry is deliberate: misreading conversation as work costs one wasted planner call, while misreading work as conversation caps a real request and skips its plan, which fails it.

A client that declares `task_mode: work` is never tiered as conversational (`declaredTier`): only the declaration can tell a request phrased as a question ("Can the page also show today's date") from a question. The classifier is the fallback for callers that declare nothing.

That asymmetry also governs the question-shape test (`isQuestionMessage`). A `?` that ends a clause (followed by the end, a space or a quote, and outside brackets and backticks) is a question; a `?` in a URL query or a regex is not. An interrogative opener is a question when the wh-word is a whole word — "Whole-number…", "Whenever…" and "However…" are not — and "when"/"where" openers must be inverted ("when does"), because "When the timer hits zero, …" opens a request. An opening "do" needs a pronoun ("do you"), so "Do the same for the /users route" is work, and a mid-message auxiliary needs a subject, so ". Do not hardcode the answer." is not a question. A wh-word opening a mid-message clause is a question only when the clause is INVERTED — the auxiliary follows the wh-word, as in "what DOES x do". A relative clause keeps subject-verb order ("what it is", "where it was found") and is descriptive, so a field list like "post an item (what it is, where it was found)" is not read as a question. Past-tense auxiliaries are left out of the inverted set on the same asymmetry: a build request must not be capped, and missing a rare past-tense mid-clause question with no `?` costs only a planner call. A build request that reads as a question was the failure this closed: a lost-and-found web app was classified conversational, capped, and unplanned, on the words "where it was found".

Tier caps are 0 (uncapped); the detector stack inside the loop decides when to break: lens regression (`agent_lens_intervention`), reasoning repetition (`agent_reasoning_intervention`), tool-call repetition (`agent_repeat_intervention`), path-aware error breaker, done-without-action gate, claim-check gate, evidence gate, plan adherence threshold, and the empty-response fallback. Operators can override with `ATLAS_MAX_TURNS=<n>` for one-off "fix the entire app" prompts — see `proxy/types.go::envOverrideMaxTurns`.

Three of those gates decide whether to fire from what the run observed rather than from how the request was worded, because request wording is an open vocabulary that no list completes:

- **Verification gate** — blocks `done` when the user asked for a repair, *or* when a run, test or check actually exited non-zero and nothing has passed since. The second condition catches a failing test the model introduced on its own, which no reading of the user's message could have predicted. A later failure takes back an earlier pass; a failed lint or parse is cleared by that same check passing again, or by a passing run, and a lint never clears a failed test. Only a run that executed the program or its tests, or fetched a page in a way an HTTP error would fail (`curl -f`, or the body piped into `grep`), counts as verification, and only when the command line reports that part's exit status (`proxy/command_evidence.go`). A parse, a lint, a build or a `--version` is not verification, and neither is `pytest | tail`, `pytest || true`, `app.py; echo` or `app.py &`; the gate names such a command back to the model with the reason. Its ledger is artifact-bound, not boolean. Every verification run appends a `VerificationRecord` — the command line, its evidence kind, whether it failed, its stdin contract, and the sha256 of each session-written file its verifying segments named — and the exit re-hashes those files, bouncing `done` when the bytes on disk are no longer the bytes a run vouched for. A green run that printed nothing on a task whose prompt demands printed output *latches as a failed verification* rather than counting. A program only ever run as `prog < file` (the redirect is recognized anywhere in a segment, as is `cat file | prog`) is verified under a contract the caller may not use, and the exit demands one standalone run. A write that landed with a parse warning marks its path pending-execution: further writes and edits to that path bounce until a command actually *attempts to execute* it — an interpreter invocation or `./file`; `cat`/`grep`/`ls` naming the file prove nothing and discharge nothing, and neither does a parse such as `python -m py_compile`. And when the red-run streak crosses two, the loop injects fresh-rewrite advice immediately (stop patching, rewrite from a clean sheet) instead of waiting for the model to attempt `done` into the same text.
- **Done-without-action gate** — blocks `done` when the request demands a state change and nothing landed on disk. **When the client sends a validated `task_contract`, its `task_mode` decides that question**: `work` means a state change is required, `question` means it is not, and the wording of the message no longer overrides either. When no contract is sent, the legacy heuristic decides exactly as before — it blocks on explicit action wording, *or* when the model opened the project on a non-conversational message and nothing changed. One helper owns this choice (`proxy/guardrails.go::decideActionDemand`) and both action-demand call sites consume it; the heuristic is evaluated once, there, and is recorded for comparison even when a contract decides. That covers verbs absent from the intent list (`remove the debug logging` matches none of them), while questions stay exempt: they are conversational, and answering one by reading files and writing nothing is correct.
- **Evidence gate** — blocks an exit whose reply names a workspace file the run was never shown the contents of. The write tools already refuse to edit a path that was not read first; this applies the same rule to answers, where the reply itself is the deliverable. It keys on `AgentContext.BodySeen`, which records the files actually put in front of the model — deliberately narrower than `FilesRead`, because `outline_file` caches a file's whole source for staleness tracking while showing the model only signatures and line ranges. Reading the file, or having authored it, clears the gate.

#### Client task mode

A request may carry `task_contract.task_mode`, validated at the request boundary (`work` or
`question`; absent stays distinguishable from present-and-empty). What it does and does not do:

- **Authoritative when present** for one question only: does this request require a state change.
- **Fallback when absent** — the legacy natural-language heuristic decides, unchanged. Legacy
  clients are accepted as they are; no mode is inferred or fabricated for them.
- **Establishes an obligation, never a completion.** A `work` mode says action is required; the run
  still has to demonstrate it through the existing evidence — current-hash deliverable validity,
  applicable verification, no unresolved mutation debt, no live background hazard, no stale
  validation, no blocking tombstone or permission defect.
- **Authorises nothing destructive.** A `question` mode does not permit mutation and does not erase
  mutation debt, broken deliverables, failed validation, background hazards or deletion
  obligations; if the model mutates anyway, the same safety and completion rules govern those bytes.
  Deletion still requires its own path-and-identity-bound confirmation.
- **Never reaches the model.** Task mode is not in any prompt, and no model, V3 or lens output can
  set it. An internally malformed mode fails closed to requiring work rather than reading as a
  question.

Shadow diagnostic records (private, observational, off by default — see
[OPERATIONS.md](OPERATIONS.md#private-diagnostics-task-contract-shadow-capture)) version **per
record kind**, because adding a field to one must not silently redefine another:

| Record | Version | Contract |
| --- | --- | --- |
| `task_contract_shadow_gate` v1 | 1 | legacy observation only: what the heuristic said. Sealed captures taken before the task-mode migration are v1 and stay readable by the analyser written for them. |
| `task_contract_shadow_gate` v2 | 2 | v1 plus `live_action_demand` (the decision that actually governed) and `action_demand_source` (`legacy`, `contract_work`, `contract_question`, `contract_invalid_failed_closed`). |
| `task_contract_shadow_request` | 1 | unchanged |
| `task_contract_shadow_footer` | 1 | unchanged |

`comparison` still describes contract-versus-legacy; `influences_live_decision` still describes
whether the observer and its sink can influence policy, and stays false. A schema change means a
new version with a closed field set and a reader for it — never a redefinition of an existing
version, and never a parser that shrugs at unknown fields.

Clients: the TUI sends `work` for an ordinary message and `question` for a one-shot `/ask <message>`;
the e2e and reliability harnesses send `work`. The VS Code extension sends no contract yet, so its
requests get no V3 candidate ([CANDIDATE_POLICY.md](CANDIDATE_POLICY.md)). `expected_outputs` and `verification` are carried and
validated but **not yet migrated** — deliverable and verification obligations are still derived the
old way.

The evidence gate exists because the exemption in the done-without-action gate above — questions are conversational, so they are never gated — left the answer path with no completion check at all. Measured on a diagnostic question spanning three modules: across 12 sessions the model ran `list_directory`, outlined exactly one file, and answered, never once reading a body. The outcome tracked which filename it guessed rather than anything it inspected (`scoring.py` wrong 11/11, `planning.py` right 1/1), because the prompt contained the word "scored". One reply cited "lines 134-142" of a file it had never seen.

Classifier in `proxy/tools.go` (`classifyFileTier`); logic-pattern matcher in the same file (`hasLogicIndicators`).

**Always T1 (direct write):**
- Config files matched by name (e.g. `package.json`, `go.mod`, `pyproject.toml`, `dockerfile`, `docker-compose.*`)
- Data files by extension (`.json`, `.yaml`, `.yml`, `.toml`, `.csv`, `.xml`, `.env`)
- Style files (`.css`, `.scss`, `.less`)
- Documentation (`.md`, `.txt`, `.rst`) and shell scripts (`.sh`, `.bash`)
- Trivially-tiny files under **10 lines** (V3 has nothing to meaningfully diversify on at that size)
- Unknown extensions with no logic indicators

The exact config-file list and extension sets live in `proxy/tools.go:classifyFileTier`.

Every content-edit tool enters the pipeline through `runEditPipeline` (`proxy/tools.go`) — `edit_file`, `insert_after`, `replace_lines`, and `structural_edit` and `write_file` via their own V3 paths. Tier classification uses `max(oldTier, newTier)` so a destructive edit that shrinks a T2+ file into a T1 stub still qualifies. A tool that skips this produces one greedy sample with no candidates and no lens scoring regardless of tier, which is what the tier system exists to prevent; `tests/contracts/test_write_gate_coverage.py` asserts every write path reaches it.

Every content-edit tool enters the pipeline through `runEditPipeline` (`proxy/tools.go`) — `edit_file`, `insert_after` and `replace_lines` directly, `structural_edit` and `write_file` via their own V3 paths. Tier classification uses `max(oldTier, newTier)`, so a destructive edit that shrinks a T2+ file into a T1 stub still qualifies. A tool that skips this produces one greedy sample with no candidate generation and no lens scoring regardless of tier, which is the thing the tier system exists to prevent; `tests/contracts/test_write_gate_coverage.py` asserts every write path reaches it.

**T2 (V3 pipeline)** — file qualifies if it's ≥10 lines AND either:
- `hasLogicIndicators(content)` returns true — **2+ matches** across pattern families covering function/method definitions, control flow, error handling, Flask/FastAPI/Django routing, Express/Node API, React state/data, validation, database calls, JSX/React component patterns, and imports (the literal token list is in `proxy/tools.go:hasLogicIndicators`)
- OR the file has a recognized source-code / markup extension (`.py`, `.go`, `.rs`, `.ts`, `.tsx`, `.js`, `.jsx`, `.html`, `.htm`, …) and no logic indicators fired — gets the benefit of the doubt at T2 (covers minimal-but-real files like a 12-line component shell)

**T3 (Hard)** — currently classifier never emits T3 by itself; the cyclomatic-complexity refiner (`refineTierWithCC` via GH #39 point 2's `/internal/cyclomatic_complexity`) *escalates* on McCabe CC: to T2 at CC ≥ 8 (including from T1) and to T3 at CC ≥ 16. Never downgrades.

### Plan Mode (per-turn pre-flight)

Plan mode is a pre-flight planning step that runs once per agent turn before the first tool call: the planner samples candidate plans, scores them heuristically, and renders the winner into the system prompt, where an adherence gate auto-revises when the model thrashes off-plan. It cuts discovery thrashing and blocks no-evidence `done` by guarding on the plan's verify step.

See [PLAN_MODE.md](PLAN_MODE.md) for the full flow, components, tunables, skip conditions, cost, and testing matrix.

### Safety Limits

Operator-facing limits and the knobs that tune them. Internal steering guards (traceback localization, missing-module/missing-command/broken-inline-script/case-mismatch steers, symbol grounding, no-op/empty-content/syntax gates, doctype strip) live in `proxy/guardrails.go` and `proxy/agent.go`; the structural gate (refuses a `.py` write that introduces an unresolved direct call — a would-be `NameError` — on `edit_file`, `structural_edit`, and every `write_file` branch; it runs on every request; no request field turns it off) lives in `proxy/gates.go`. The embedded-script gate lives there too: it parses the JavaScript (and brace-balances the CSS) inside `<script>`/`<style>` blocks — in `.html`/`.htm`/`.jinja`/`.jinja2` files **and inside Python string literals**, the `render_template_string` shape — through v3-service `POST /internal/embedded_script_check`, and refuses a change that newly breaks it. It exists because every other gate is structurally blind to that code: the 2026-08-01 dogfooding session left a stray `)` in a Flask app's inline `<script>`, and the Python compiled, the server started and `curl /` returned 200, so the verification gate passed and `done` was accepted while the page was dead in the browser. Same healthy→broken rule as the syntax gate (a still-broken repair-in-progress is allowed) and the same fail-soft posture: an unreachable service, a missing grammar or an ambiguous block (template statement tags, `<script src>`, a non-JS `type`, an escaped Python string) yields no finding, never a blocked write. The missing-command steer fires on `command not found` shell errors: the sandbox is non-root on a read-only base, so absent binaries can never be apt-installed at runtime — the steer says so and points at pip-installable equivalents or the preinstalled toolchains instead of letting the model re-run into the repetition breaker. The broken-inline-script steer fires when a `python -c` verification one-liner fails with a SyntaxError in the `-c` argument itself (a multi-statement `def`/`for` body jammed onto one line): the solution file may be correct while only the verify command is malformed, so it directs the model to move the test into a `.py` file rather than re-run the unparseable one-liner.

**Fast-path writes during active iteration.** V3 fires on the *first* write of a T2+ file (baseline generation). But once the model has written a file and just saw it fail a run, the next write is a targeted fix in an edit-test-fix loop — it skips V3 (still syntax- and structural-gated) and writes directly. V3's full pipeline is multi-minute per call and, on a file mid-debug, frequently completes without a usable result and falls back anyway; paying that latency per iteration throttles the loop to a handful of cycles. The fast-path keys off `SessionWrites[path]` plus a failed most-recent run referencing the file.

**Iteration vs. repetition.** The loop breakers distinguish a model *iterating toward a fix* from one *spinning*. `write_file` repetition is keyed on the target path **plus a whitespace-stripped content fingerprint**: reasserting the same draft collides and counts toward the threshold, but rewriting a file with materially different content (fixing successive compiler errors) produces distinct signatures and is not counted. When repetition *is* detected, the breaker **steers before it kills** — the first detection injects a corrective `[system note]` and the loop continues; only a second detection (the model repeated after seeing the nudge) ends the session. This replaced an immediate hard-stop that terminated legitimate iteration with the solution on disk but unverified.

| Limit | Value | Purpose |
|-------|-------|---------|
| Conversation trim | Sliding window sized to the slot: keep system + most-recent-user-instruction + the active file's content + as many trailing messages as fit `per-slot context − ATLAS_MAX_TOKENS − 2048 − slot/8` (the `slot/8` term is tokenizer slack: the chars/4 estimate under-counts dense code/JSON). The pinned instruction and file content are counted against the budget, not just re-injected. Floor: keep 8; hard ceiling via `ATLAS_AGENT_HISTORY_BUDGET`. If llama-server still rejects the prompt as over-context, the loop force-trims to the minimum window and retries once instead of killing the session | Prevent context overflow without dropping the file under edit |
| Redundant-read short-circuit | Whole-file re-read of an unchanged file returns an "already in context" pointer only while the content is still live; otherwise the full file is re-served (`ATLAS_DEDUP_READS=0` disables) | Avoid re-encoding an unchanged file every turn without the model editing blind |
| V3 interactive wall-clock cap | Single V3 pipeline call capped at `ATLAS_V3_TIMEOUT` (default 300s); on timeout the proxy falls back to the model's syntax- and structural-gated content, and the run's final summary names the file (`0` disables) | Keep an interactive session responsive under a long repair stall |
| Per-turn reasoning budget | Cut the stream after ~6144 reasoning tokens (`ATLAS_REASONING_BUDGET`, 0 disables); recovery extracts an embedded tool_call or re-prompts | Bound reasoning spirals |
| write_file for existing files | Reject if file > 5 lines; on .py/.html/.htm the per-step grammar gate steers to `structural_edit` | Force surgical (`edit_file`) or whole-node (`structural_edit`) edits |
| Suspicious-shrinkage guard | Reject `structural_edit`/`edit_file` when `oldSize >= 100B` and `newSize < 64B` (`proxy/guardrails.go::validateNotSuspiciouslyShrunk`) | Catch destructive stub rewrites before they hit disk |
| structural_edit runaway-content guard | Reject when `content` > 8 KB AND > 4× the file size | Catch reasoning-leak blobs emitted as the replacement node |
| structural_edit node-size precondition | Reject a replacement ≥5× the node and ≥30 lines larger, when it either duplicates content already in the file or the file holds a ≥20-line module-level string constant (`v3-service` `_replacement_dwarfs_node`) | A blob spliced through a small selector deletes whatever the node held; an observed session left a Flask app with no `@app.route` at all, still parsing |
| Duplicate-entrypoint guard | Reject a `.py` write that leaves more than one top-level `if __name__ == "__main__":` when it had at most one before (`proxy/gates.go::duplicateMainGuard`) | The signature of a whole-file blob spliced through a node selector; it parses and runs, with everything after the first blocking `app.run()` dead |
| Duplicate-lexical-binding guard | Reject an edit that leaves `let`/`const` declaring the same name twice in one scope (`v3-service` `embedded_script_check`) | An early SyntaxError: the browser refuses the whole script, so every handler on the page dies while the server still returns 200. tree-sitter parses it, so the syntax check cannot see it |
| Stopped-render-loop guard | Reject an edit that leaves a function a repeating timer used to drive scheduled exactly once and never re-armed (`v3-service` `embedded_script_check` with the pre-edit file; `proxy/gates.go::embeddedScriptGate`) | The JavaScript parses, the server starts and the page returns 200, so every other check passes while the page freezes after one frame |
| V3 out-of-scope-rewrite guard | Discard a V3 candidate that drops a line the caller's edit had left alone, keeping the caller's content (`proxy/gates.go::v3RewroteBeyondTheEdit`) | V3 improves a whole *file*, so on a small file it retypes everything the edit never touched; a live session came back with `#e94562` as `#e94162` and `id="msg"` as `id=" msg"`, neither of which is a syntax error |
| Error loop breaker | 3 consecutive failures **of the same kind** (message skeleton: digits, quoted spans and paths stripped); a rejection that differs resets the streak, since three distinct refusals is a model converging, not looping | Stop runaway failure cycles without killing legitimate iteration |
| Failed-call ceiling | 12 failed tool calls per run (`maxTotalFailures`) | Bounds a run that cycles through failure modes, now that the streak resets on a changed rejection |
| Plan-completion gate | Refuse `done` while a planned step has no matching tool call, when the planner scored the plan ≥0.6, it has ≥2 steps, and at least one matched (`proxy/guardrails.go::planIncompleteMessage`) | A multi-part task delivered in part and declared done; the per-turn progress note was already there and was ignored |
| Exploration budget | Nudge at 4 consecutive read-only calls; escalated nudge at 5+. Reads always execute — the nudge steers the *next* turn toward a write | Push the model to write instead of exploring indefinitely |
| Command output truncation | stdout 8,000 chars, stderr 4,000 chars | Prevent context flooding |
| Search results | 200 matches max; file search skips files > 1 MB | Bound search cost |
| Truncation detection | JSON parse check on tool args | Catch truncated model output |

---

## 4. V3 Pipeline (Inner Layer)

Activates inside `write_file`/`edit_file` executors for T2+ files. The pipeline has four phases with early exits at every stage.

### Pipeline Flow

```mermaid
flowchart LR
    Entry["T2 detected"] --> Probe["Probe"] --> Score1["C(x)/G(x)"] --> SB1["Sandbox"]
    SB1 --> Pass1{"Pass?"}
    Pass1 -->|"Yes"| Done["Done"]

    Pass1 -->|"No"| Alloc["CxGx Gate\nk >= 3"] --> PS["PlanSearch"] --> DS["DivSampling"] --> BF["BudgetForcing"] --> Build["Build Check"] --> Score2["Score K"] --> SB2["Test K"]

    SB2 --> AnyPass{"Passed?"}
    AnyPass -->|"1+"| Select["Lens Select"] --> Done

    AnyPass -->|"0"| FA["Failure Analysis"] --> PRCOT["PR-CoT"]
    PRCOT --> PRPass{"Pass?"}
    PRPass -->|"Yes"| Done
    PRPass -->|"No"| Refine["Refinement"] --> Done

    style Entry fill:#1a3a5c,color:#fff
    style Done fill:#333,color:#fff
    style Probe fill:#1a3a5c,color:#fff
    style Alloc fill:#1a3a5c,color:#fff
    style PS fill:#1a3a5c,color:#fff
    style DS fill:#1a3a5c,color:#fff
    style BF fill:#1a3a5c,color:#fff
    style Select fill:#2d5016,color:#fff
    style Score1 fill:#2d5016,color:#fff
    style Score2 fill:#2d5016,color:#fff
    style SB1 fill:#2d5016,color:#fff
    style SB2 fill:#2d5016,color:#fff
    style Build fill:#2d5016,color:#fff
    style PRCOT fill:#5c3a1a,color:#fff
    style Refine fill:#5c3a1a,color:#fff
    style FA fill:#5c3a1a,color:#fff
```

Legend: blue = generation, green = verification/selection, brown = repair.

### Phase Details

**Phase 0: Probe** generates a single baseline candidate with progressive budget retry (light → standard → nothink). It is scored with the selected model's C(x)/G(x) artifacts and tested in the sandbox. If it passes and its evidence record closes, the pipeline exits early; otherwise generation continues.

**The incumbent** (#259). The proxy's write and edit routes always send the caller's own file as `baseline_code`. Its exact bytes are a candidate too:
- It gets the same sandbox verification, contract record and vetoes, and a lens score. It is ranked with the generated candidates at pool index -1, and it takes no generation slot. If its own record closes, it is returned at once.
- A generated candidate replaces it only by passing the same checks and ranking higher. The candidate must also keep the file's role: every top-level def, class and assignment name, and every name imported from another project file.
- A copy that differs from it only in whitespace never replaces it.

When the incumbent holds, `phase_solved` is `incumbent`, and the proxy writes the model's own bytes.

**Candidate Allocation: the CxGx gate** (emitted as `phase2` / `phase2_allocated`) decides how many candidates the failed probe earns. The probe's combined C(x)+G(x) score (one embedding extraction, both models) drives a two-step rule: the calibrated C(x) normalized energy picks a base tier on the same ladder Budget Forcing uses, and the G(x) quality score escalates that tier by +1 when it falls below the model's calibrated severe boundary and +2 when it falls well below (0.75x it) — the case where the probe looks cheap to C(x) but wrong to G(x). The tier sets k (`nothink` 1, `standard` 3, `hard` 5, `extreme` 8) under a hard **k >= 3 floor**, so the gate can only add candidates to the previously pinned k=3, never remove them; its worst case is the old behavior. Both signals require this model's calibration files (`cx_normalization.json`, `gx_thresholds.json`): an uncalibrated lens, or a probe the lens declines (too long for the embedding batch, empty), allocates exactly k=3 at `standard`, so an uncalibrated bundle runs the pipeline it ran before rather than routing on a scale that means nothing for it. A lens that cannot score at all stops the run instead ([ADR 0011](adr/0011-the-lens-is-required.md)).

The floor is the difference between this and the C(x)-only allocator removed earlier: that one had no floor, so it handed k=1 to tasks whose probe had *just failed*. Whether lens-driven allocation beats fixed or randomly assigned tiers is unmeasured. An earlier four-arm comparison (gated, fixed k=3, the same tier mix shuffled across tasks, and k=8, at n=175 per arm) is not evidence either way: it ran on Qwen3.5-9B with a patched runner that is not in this repository, with thinking enabled on escalation, which the live gate cannot apply, on LiveCodeBench tasks the G(x) head was trained on, and at that sample size its arms are within noise of each other.

Live-path difference: the proxy's V3 bridge abandons a pipeline call after `ATLAS_V3_TIMEOUT` (default 300s), a cap the bench never had, so an unbounded escalation to k=8 would spend the budget on generation and return a timeout fallback instead of the k=3 answer the clock could have produced. The live orchestrator therefore passes its remaining wall-clock and the per-call latency observed on that task, and the gate lowers the tier to what the budget can actually generate — reserving one refinement iteration so the escalation cannot starve Phase 3 — never below the floor. The bench runner passes no budget and allocates exactly what was measured. `v3-service/stages/cxgx_gate.py`, shared by both orchestrators.

**Phase 1: Constraint-Driven Generation**

**V3 always runs.** Planning and candidate generation run on every request the routing rules send to them: a file of Tier 2 or above, a V3 service configured, and a session not iterating on a file it just watched fail. No request field turns either off. `bypass_v3`, `v3_mode` and `feasibility_mode` were removed, and a request that asks for V3 off, planner-only or `enforce` is a 400, never a quiet run under another system. A measurement without V3 takes a research build.

- **PlanSearch** generates structurally different implementation plans by extracting distinct constraint sets — one per allocated candidate slot the probe did not already fill (k-1). Its plan-construction and code-generation steps fan out across worker threads (`_fan_out` in `v3-service/stages/plan_search.py`), which makes it the only V3 stage that dispatches inference off the request thread. A worker inherits no ContextVar and no local from its parent, so request-scoped state reaches it on the request-scoped `LLMAdapter` — the same carrier as the cancellation scope. That is where the identity each inference call is sent under lives (`LLMAdapter.request_identity`); the header resolver takes no ContextVar fallback, because in a worker a fallback reads as "no request" and silently strips attribution. A PlanSearch call that cannot reach the model at all raises `PlanSearchInfrastructureError` rather than returning a short candidate list: DivSampling still backfills the slots below, and an empty batch and a refused one are otherwise indistinguishable downstream.
- **DivSampling** applies perturbation diversity: 4 roles (competitive_programmer, systems_engineer, mathematician, pragmatist) + 4 instructions (step_by_step, edge_case_first, complexity_aware, constraint_driven) + 4 styles (functional, pythonic, optimize_iteratively, structured)
- **Budget Forcing** controls thinking token allocation:

| Tier | Thinking Tokens |
|------|----------------|
| nothink | 0 (template-level thinking disabled) |
| light | 1,024 |
| standard | 2,048 |
| hard | 4,096 |
| extreme | 8,192 |

Each tier maps to a system prompt (direct vs. think-step-by-step) and a max-token budget. Tier selection uses the selected model's calibrated C(x) energy; absent calibration, ATLAS uses the configured default budget rather than another model's constants.

**Phase 2: Verification and Selection**

- **Build Verification**: Python (`py_compile`), TypeScript (`tsc --noEmit`), JavaScript (`node --check`), Go (`go build`), Java (`javac`), Kotlin (`kotlinc`), Rust (`rustc` on the sandbox `/execute` path; `Cargo.toml` projects are detected with `cargo build`, and `cargo check` is accepted only via the build-command allowlist), C/C++ (full `gcc`/`g++` compile with `-Wall` on `/execute`; `-fsyntax-only` applies only to the `/syntax-check` route), Ruby (`ruby -c`, no compile step — interpreted), PHP (`php -l`, no compile step — interpreted), Shell (`bash -n`). Framework overrides for Next.js, React, Flask, Django, Express.
- **The `/syntax-check` verdict**: syntax only, one file at a time, so a reference to a sibling file or a package the lone file cannot see is not an error. Java is parsed without being compiled (`javac -XDshould-stop.ifNoError=PARSE`); Kotlin and TypeScript count only syntax diagnostics (kotlinc's `syntax error`, tsc's TS1xxx); JavaScript is checked as a `.cjs` file and, when that fails only on module syntax, as a `.mjs` file, so Node's module-type detection cannot decide the verdict; HTML must contain markup and must not end inside a tag, a comment, a `<script>` or a `<style>`. A checker the resource contract stopped, or that never started, returns `status: "not_run"` with its `outcome`, never `valid: true`; the proxy records that as not run, not as a pass or a syntax error.
- **Vetoes**: three checks can reject a sandbox-passing candidate — the lens veto (per-step `gx_min` below the model's calibrated severe threshold: the code executes but the generation pattern collapsed toward a stub), the structural veto (Python targets only: tree-sitter finds a direct-identifier call resolving to no local def, import, builtin, or project symbol — a `NameError` in waiting; the Python grammar parses any other language tolerantly and reported an HTML template's `Time (mins)` as a call), and the call-graph veto (Python targets only, for the same reason: cross-file calls with no in-scope definition). A vetoed candidate is marked failed (`passed=false`, `vetoed_by`, veto reason as its error output) and joins the Phase-3 repair pool like any failing candidate; the final energy fallback never returns it. If every candidate is vetoed and repair fails, the pipeline returns no code and the caller substitutes its baseline
- **Input-only consensus (CodeT)**: when 0/K passed and a generated suite exists, candidates run on the generated *inputs* and cluster by agreement on what they printed — the signal is convergence between independently generated programs, not the model's guessed answer key. Hard rules: a candidate joins clustering only with a real answer on *every* probe case (a crash prints a `CRASH` marker the clustering refuses, empty output clusters with nothing, and partial validity is not agreement material), and a cluster needs at least two members — one program agreeing with itself is not evidence
- **Lens Selection** (1+ passing): sort by C(x) energy, lowest wins

**Phase 3: Repair** (if 0/K pass, or every passer was vetoed) — two strategies, sequential with early exit:

- **Failure Analysis**: categorize failures (wrong_algorithm, implementation_bug, edge_case_miss, time_limit, format_error, partial_correct)
- **PR-CoT**: 4 perspectives (logical_consistency, information_completeness, biases, alternative_solutions) x (analysis + repair) = ~8 LLM calls, up to 3 rounds
- **Refinement Loop**: Failure Analysis → Constraint Refinement → Code Gen → Test → Learn. 2 iterations, 120s budget, ~5+ LLM calls each. Cosine distance filtering (>= 0.15) prevents hypothesis repetition. Budget-gated: the orchestrator enters the loop only when the remaining wall-clock (the `ATLAS_V3_TIMEOUT` cap live; the loop's own 120s budget in bench) can afford one iteration (~3 sequential LLM calls at the observed per-call latency) — otherwise it emits `refinement_skip` and goes straight to the fallback

### Module Map

The pipeline stages are 12 Python modules in `v3-service/stages/`. `v3-service/pipeline.py` orchestrates 11 of them (10 directly; `constraint_refinement` via the refinement loop); `embedding_store` runs only under the offline bench runner (`atlas/bench/v3_runner.py`, which puts the checkout's `v3-service/` on its path so both callers share one stage implementation):

```mermaid
graph LR
    Main["pipeline.py"] --> CG["CxGx Gate"]
    Main --> PS["PlanSearch 1A"]
    Main --> DS["DivSampling 1B"]
    Main --> BF["BudgetForcing 1C"]
    Main --> CS["CandidateSelection"]
    Main --> FA["FailureAnalysis 3A"]
    Main --> PRCOT["PR-CoT 3C"]
    Main --> RL["RefinementLoop 3E"]
    Main --> STG["SelfTestGen"]
    Main --> LLM["LLMClient"]
    Bench["v3_runner.py\n(bench only)"] --> ES["EmbeddingStore"]

    RL --> FA
    RL --> CR["ConstraintRefiner 3B"]
    CG -->|"tier table"| BF
    CG -->|"budget helpers"| RL

    style Main fill:#333,color:#fff
    style Bench fill:#333,color:#fff
    style CG fill:#1a3a5c,color:#fff
    style PS fill:#1a3a5c,color:#fff
    style DS fill:#1a3a5c,color:#fff
    style BF fill:#1a3a5c,color:#fff
    style CS fill:#2d5016,color:#fff
    style FA fill:#5c3a1a,color:#fff
    style CR fill:#5c3a1a,color:#fff
    style PRCOT fill:#5c3a1a,color:#fff
    style RL fill:#5c3a1a,color:#fff
    style STG fill:#333,color:#fff
    style LLM fill:#333,color:#fff
    style ES fill:#333,color:#fff
```

Legend: blue = Phase 1 (generation), green = Phase 2 (selection), brown = Phase 3 (repair), gray = utilities. Modules fed by `v3_runner.py` are bench-runner-only; the service does not call them. The service itself is `main.py` (HTTP handler) → `pipeline.py` (orchestrator) → `planning.py` / `scoring.py` / `symbols.py` / `adapters.py` flat siblings.

---

## 5. Geometric Lens

Neural scoring system that evaluates code quality without executing it by analyzing the geometric structure of model embeddings. Runs entirely on CPU. The service surface is internal-only (`/internal/*`): C(x)/G(x) scoring (single-shot and per-step).

#### Why "Geometric Lens"?

The core idea behind the Geometric Lens comes from a simple premise: stop scaling models and start wrapping them in supporting infrastructure. Jose Crespo's ["Everyone's Wrong About AI Programming"](https://www.josecrespophd.org/p/everyones-wrong-about-ai-programming) argues that AI-generated code drifts toward errors because current LLMs operate in flat embedding spaces where correct and incorrect code paths cost the same. The solution is to build an energy landscape around the model where correct code is "downhill" and incorrect code is "uphill."

Anthropic's [Manipulating Manifolds](https://transformer-circuits.pub/2025/linebreaks/index.html) research provides evidence that transformers already create manipulable geometric structures in their embedding space - the raw material is already there. Bar et al.'s [Geometric Unification of Generative AI](https://arxiv.org/html/2510.00666v1) formalizes how distance functions on data manifolds can be learned and used for scoring.

ATLAS implements this with two complementary models. C(x) is a learned energy function (`hidden_dim`→512→128→1 MLP) over the selected model's own embeddings. Each code candidate gets embedded by llama-server, and C(x) scores where it sits in that geometry. Low energy means the candidate clusters with known-correct code. High energy means it clusters with known-incorrect code. No external oracle, no execution required—just the geometry of the selected model's representations.

G(x) is the quality predictor - an XGBoost classifier over PCA-reduced embeddings that predicts pass/fail from where a candidate sits in the reduced space. Where C(x) answers "how good is this candidate?", G(x) answers "is this candidate likely to pass?" It is the only G(x) implementation: the earlier metric-tensor formulation and its correctability endpoint were removed once XGBoost became the deployed path (see git history for the geometry-aware variant).

### Scoring Models

```mermaid
graph LR
    EE["Embedding Extractor\nllama-server /embedding\nmodel hidden dim"] --> CX["C(x) Cost Field\nd→512→128→1\nSiLU + Softplus"]
    EE --> GX["G(x) XGBoost\nPCA(128) + classifier"]
    CX --> SVC["Service Layer\nevaluate_combined()"]
    GX --> SVC
    SVC --> V{"Verdict"}
    V -->|"at/above artifact low"| LC["likely_correct"]
    V -->|"between severe and low"| UN["uncertain"]
    V -->|"below artifact severe"| LI["likely_incorrect"]

    TR["Training Pipeline\ncontrastive ranking loss"] --> CX

    style EE fill:#333,color:#fff
    style CX fill:#2d5016,color:#fff
    style GX fill:#2d5016,color:#fff
    style SVC fill:#333,color:#fff
    style TR fill:#1a3a5c,color:#fff
```

The following figures describe the frozen reference artifacts used for the
published V3 study; they are provenance, not runtime dimensions or defaults:

| Model | Reference architecture | Training Data | Performance |
|-------|-------------|---------------|-------------|
| **C(x)** | 4096→512→128→1 MLP (SiLU, Softplus) | 597 LCB embeddings (504 PASS, 93 FAIL) | Val AUC 0.9467, sep 2.04x |
| **G(x)** | PCA(4096→128) + XGBoost | 13,398 embeddings (4,835 PASS, 8,563 FAIL) | PCA 80.8% variance |

C(x) normalization is `sigmoid(steepness × (energy - midpoint))`. The
selected model's `cx_normalization.json` supplies both values; `atlas lens
build` derives them from that model's labeled PASS/FAIL candidates. G(x)
verdict thresholds likewise come from `gx_thresholds.json`. Without either
calibration, normalized decisions stay neutral/uncalibrated rather than
borrowing the reference artifact's scale.

**Attribution on model-bound calls.** Every request the Lens makes to the
model server (`/embedding`, the `/v1/models` identity probe) goes through one
transport, `geometric_lens/model_transport.py`, which forwards the two
correlation headers the rest of ATLAS already uses: `X-ATLAS-Request-ID` and
`X-ATLAS-V3-Invocation-ID`. Their values come only from the identity the Lens
middleware bound for the current request (the same ContextVars every ATLAS
Python service uses); V3 sends both on its scoring calls. With no bound
identity the headers are absent, a partial pair stays partial, and the
binding is cleared when the request ends, so concurrent requests and
background work cannot exchange or inherit identities. Startup and readiness
work (the boot self-test, a `/ready` re-run) carries an identity only when
`ATLAS_LENS_STARTUP_REQUEST_ID` and `ATLAS_LENS_STARTUP_INVOCATION_ID` are
both set; an acquisition that requires attributed embedding traffic declares
that pair and registers it with its relay, ordinary deployments set neither.
Attribution only: no scoring, selection, authorization or completion logic
reads these headers, and no candidate bytes or user content enter them.

**The proxy's direct Lens calls carry their own invocation.** The proxy talks
to the Lens directly on one model-bound path, per-write scoring
(`/internal/lens/score-per-step`), which is not a V3 candidate invocation. One owner,
`proxy/lens_identity.go`, builds those requests and stamps the bound
`X-ATLAS-Request-ID` together with a proxy-owned Lens invocation derived from
that request id alone: `proxy-lens:` followed by the first 32 hex digits of
`sha256("atlas/proxy-lens-invocation/v1\n" + request_id)`. It is deterministic
(a relay can register the pair before any model-bound traffic), the same for
every direct Lens call within one request, distinct across requests and from
V3's UUID invocations, and derived from nothing but the typed request id: never
from prose, paths, candidate bytes, tool arguments or model output, and never
settable by the model. It travels in the existing `X-ATLAS-V3-Invocation-ID`
channel; the header name is historical and the value is a general model-bound
invocation identity. It is a scope label, not a credential: nothing reads it to
authorise a mutation, permission, candidate or completion, and it never appears
in SSE events, tool results, prompts or logs. A request id that is absent or
outside the closed format (`[A-Za-z0-9._:-]{1,128}`) derives no invocation. The
closed specification and vectors are in
`proxy/testdata/lens_invocation_vectors.json`.

**The embedding capacity boundary.** llama-server processes one `/embedding`
request in a single physical batch (`-ub`, `ATLAS_UBATCH`) and refuses a
longer input; every Lens score is one forward over the whole sequence. The
refusal is a transport limit of the deployment, not a judgment about the
text, and it is kept apart from every score: the answer says `scored:
false` with `null` in every score field and a typed `failure`
(`embed_capacity` with the server's `input_tokens` and `capacity_tokens`,
`model_server_error`, `model_server_unreachable`, `embedding_contract`,
`nonfinite_score` for a NaN or infinite value, `internal`). Nothing is truncated or split in the serving path, because a
split input is not the vector the artifacts were fitted on. v3-service
records the failure on the candidate, ranks it after every scored one and
delivers it only as the last verified candidate standing; the proxy applies
no threshold to an unscored write. The capacity the lens knows (declared
through `LLAMA_EMBED_CAPACITY_TOKENS`, or observed from a refusal) is
reported on `/health` and `/ready` and, when it is below the proxy's
generation ceiling, as `lens_scoring: partial` in the status dimensions.
Decision record: [ADR 0010](adr/0010-lens-capacity-boundary-is-typed.md).

Every current Lens bundle also contains `model_identity.json`. The service
requires its model name to match the served-model id reported by
llama-server's `/v1/models` (with `ATLAS_MODEL_NAME` as the fallback when the
probe fails); embedding-width equality alone cannot establish compatibility
between two different models.

> **Note:** Model weights (.pt, .pkl files) are not committed to the repository — they are built during training and baked into the container image or mounted at runtime. When model files are absent, its scoring endpoints answer `enabled: false` (with placeholder numbers: C(x) 0.0, G(x) `gx_score: 0.5`, `verdict: "unavailable"`). The proxy and V3 treat that answer as a lens that cannot score, and ATLAS refuses requests until the files exist ([ADR 0011](adr/0011-the-lens-is-required.md)). A score the Lens attempted and could not compute is answered `scored: false` with a typed `failure` and no number: a failure of the lens or its model server stops the run, and an input the lens declines (too long, empty, non-finite) is reported unscored. Training data and weights are available on [HuggingFace](https://huggingface.co/datasets/itigges22/ATLAS).

<a id="rag--pageindex-v2"></a><a id="confidence-router--pattern-cache"></a>

> **Removed subsystems.** Earlier releases shipped a RAG/PageIndex project indexer, a BM25 pattern matcher, and a Thompson-Sampling confidence router inside the lens. They were reachable only through lens endpoints nothing in the product called, and were removed in the 2026-08 simplification campaign (see CHANGELOG). The pattern cache, the last of that stack, was removed in 2026-09: it stored the solution of every successful session, evaluation runs included, and injected "lessons" into every later run, which made it a channel from the test set into the product.

---

## 6. Sandbox

Isolated code execution with compilation, testing, and linting.

```mermaid
graph LR
    subgraph executors["Language Executors"]
        Py["Python\npylint (0-10) + pytest"]
        JS["JavaScript\nNode.js 20"]
        TS["TypeScript\ntsc --noEmit + tsx"]
        Go["Go 1.22\ngo build + run"]
        Java["Java 21\njavac + java -cp"]
        Kotlin["Kotlin 2.4.0\nkotlinc + java -jar"]
        Rust["Rust stable\nrustc + run"]
        C["C / C++\ngcc/g++ -Wall"]
        Ruby["Ruby\nruby -c + run"]
        PHP["PHP\nphp -l + run"]
        Bash["Bash\nbash -n + run"]
    end

    subgraph support["Support"]
        Syn["Syntax Checker\nper-language AST validation"]
        Err["Error Classifier\n15 types: SyntaxError, NameError\nTypeError, CompileError, Timeout..."]
        Trunc["Output Truncation\nstdout: 4000 chars\nstderr: 2000 chars"]
    end

    style executors fill:#2d5016,color:#fff
    style support fill:#333,color:#fff
```

Language aliases accepted: `py`/`python3` (Python), `js`/`node` (JavaScript), `ts` (TypeScript), `golang` (Go), `java` (Java), `kt`/`kts` (Kotlin), `rs` (Rust), `c++` (C++), `rb` (Ruby), `php` (PHP), `sh`/`shell` (Bash). Common CLI tools are baked into the image (`git`, `sqlite3`, `jq`, `patch`, `zip`/`unzip`, `xz`, `curl`) plus binary-inspection tools (`strings`, `objdump`, `readelf`, `nm` via binutils, and `file`, `xxd`) — the container is non-root on a read-only base, so anything a task shells out to must be preinstalled; nothing can be apt-installed at runtime. `read_file` on a binary returns a pointer to these tools rather than raw bytes. Max execution time: 300s in the Docker deployment (compose sets `MAX_EXECUTION_TIME=${ATLAS_SANDBOX_MAX_EXECUTION_TIME:-300}` to match the proxy's 5-min `run_command` cap; the bare code default is 60s). Memory, CPU, and process caps are container-level: compose sets `mem_limit ${ATLAS_SANDBOX_MEM:-4g}`, `cpus ${ATLAS_SANDBOX_CPUS:-2}`, and `pids_limit ${ATLAS_SANDBOX_PIDS:-1024}`; `atlas init` writes host-appropriate values (~75% of RAM and cores) into `.env`. Writable mounts are tmpfs, one per language ecosystem, and Docker mounts them `noexec` by default. `/home/sandbox/gobuild` is the exception and carries `exec`, because `go run` links a native binary into `GOTMPDIR` and then executes it; with the default `GOTMPDIR=/tmp` every `go run` failed with `fork/exec ...: permission denied`. Adding a compiled language means checking whether its toolchain executes from a scratch directory or from the project tree, since `/workspace` already permits exec and needs nothing. Two workspace paths: **`/execute`** (V3 candidate-test path) uses an ephemeral scratch dir under `/tmp/sandbox` (tmpfs); **`/shell`** (the agent's `run_command` route, plus `/jobs/*` for background processes) runs against `/workspace` — the bind-mounted project root from `ATLAS_PROJECT_DIR` (Docker) or hostPath `${ATLAS_PROJECTS_DIR}` (K3s), the same path the proxy sees.

---

## 7. Example VRAM Budget

One measured RTX 5060 Ti 16GB deployment using a 9B Q6 model and 32K context:

| Component | VRAM |
|-----------|------|
| Qwen3.5-9B-Q6_K model weights | ~6.9 GB |
| KV cache (32K context) | ~1.3 GB |
| **Total llama-server** | **~8.2 GB** |
| Geometric Lens | 0 (CPU-only, ~12 MB RAM for models, ~128 MB for PyTorch runtime) |
| v3-service | 0 (CPU-only) |
| sandbox | 0 (CPU-only) |
| atlas-proxy | 0 (Go binary, ~30 MB RAM) |
| **Free VRAM** | **~7.8 GB** |

All computation outside of llama-server runs on CPU. The GPU is used exclusively for LLM inference and embedding extraction.

### 7.1 VRAM Budget per Backend

The 8.2 GB / 7.8 GB-free split above is an example, not an ATLAS model default. Actual usage follows the model, quantization, context, and parallel-slot settings selected by `atlas init`. Other backends differ structurally:

| Backend | Reported "VRAM" | Realistic budget under load | Notes |
|---|---|---|---|
| **CUDA** (dedicated VRAM) | Hardware spec (16 GB on the canonical 5060 Ti) | ~95% of spec (driver reserves ~500 MB) | The numbers in the table above apply directly. |
| **ROCm** (dedicated VRAM) | Hardware spec | ~90–95% of spec (HIP runtime slightly heavier than CUDA's) | RX 7900 XTX (24 GB) → comfortably runs 14B Q5 + 32K context with 2 parallel slots. |
| **Metal** (Apple unified) | Total system RAM | **~70%** of system RAM | OS + browser + IDE eat ~30%. A 16 GB MBP has a *realistic* 11 GB budget — little headroom for Qwen3.5-9B Q6_K (~6.9 GB weights + ~1.3 GB KV at 32K, per §7) once macOS's own GPU working set is on the same memory. Use Q4_K_M (5 GB) on ≤16 GB; Q6_K wants ≥24 GB unified. |
| **Vulkan** (cross-vendor) | Hardware spec | No measured deployment yet (Preview — validated on the lavapipe CPU path only) | Expect ~20–40% below a tuned native backend on the same card. |
| **SYCL** (Intel Arc) | Hardware spec | Roadmap — Intel Arc uses Vulkan today | A770 (16 GB) target is conservative-equivalent to NVIDIA 16 GB. |

---

## 8. Deployment

Service dependency graph (identical across deployment modes):

```mermaid
graph LR
    LLM["llama-server"] -->|"healthy"| GL["geometric-lens"] -->|"healthy"| AP["atlas-proxy"]
    LLM -->|"healthy"| V3["v3-service"] -->|"healthy"| AP
    GL -->|"healthy"| V3
    SB["sandbox"] -->|"healthy"| AP

    style LLM fill:#5c1a1a,color:#fff
    style GL fill:#2d5016,color:#fff
    style V3 fill:#2d5016,color:#fff
    style SB fill:#2d5016,color:#fff
    style AP fill:#1a3a5c,color:#fff
```

`llama-server` and `sandbox` start independently. `geometric-lens` waits for `llama-server` to be healthy; `v3-service` waits for `llama-server` and `geometric-lens`; `atlas-proxy` waits for `llama-server`, `geometric-lens`, `v3-service`, and `sandbox`. The same `inference/entrypoint-v3.1.sh` drives Docker Compose, bare metal, and K3s, so context size, KV cache quantization, flash attention, and mlock are env-var-controlled and behavior is identical across those modes; the macOS hybrid path launches native llama-server via `scripts/atlas-llama-macos.sh`, which mirrors the entrypoint's flags.

Install and per-mode bring-up steps (NVIDIA / ROCm overrides, bare metal, macOS hybrid Metal, K3s manifests) are in [SETUP.md](SETUP.md); the macOS native path is in [SETUP_MACOS.md](SETUP_MACOS.md).

---

## 9. Data Flow

### T1: Simple File Write

```mermaid
sequenceDiagram
    participant U as User
    participant A as Client
    participant P as atlas-proxy :8090
    participant L as llama-server :8080

    U->>A: "Create a config file"
    A->>P: POST /v1/agent (SSE)
    P->>L: POST /v1/chat/completions<br/>response_format: json_object
    L-->>P: {"type":"tool_call","name":"write_file","args":{...}}
    Note over P: Tier = T1 (config file)<br/>Direct write, no V3
    P-->>P: Write file to disk
    P-->>A: SSE stream: file content
    A-->>U: File created
```

One LLM call. No V3 overhead.

### T2: Feature File Write

```mermaid
sequenceDiagram
    participant U as User
    participant A as Client
    participant P as atlas-proxy :8090
    participant L as llama-server :8080
    participant V as v3-service :8070
    participant G as geometric-lens :8099
    participant S as sandbox :30820

    U->>A: "Create a REST API handler"
    A->>P: POST /v1/agent (SSE)
    P->>L: POST /v1/chat/completions<br/>response_format: json_object
    L-->>P: {"type":"tool_call","name":"write_file","args":{...}}
    Note over P: Tier = T2 (≥10 lines, logic)<br/>Route to V3

    P->>V: POST /v3/generate (SSE)
    Note over V: Phase 0: Probe
    V->>L: POST /v1/chat/completions (generate code)
    L-->>V: probe candidate
    V->>L: POST /v1/embeddings (model hidden dim)
    L-->>V: embedding vector
    V->>G: POST /internal/lens/gx-score
    G-->>V: {cx_energy, gx_score, verdict}
    V->>S: POST /execute (test probe)
    S-->>V: {success: false}

    Note over V: Phase 1: PlanSearch + DivSampling
    V->>L: POST /v1/chat/completions (x K candidates)
    L-->>V: K candidates
    V->>S: POST /execute (test each)
    S-->>V: {success: true} for candidate 2

    Note over V: Phase 2: Lens select winner
    V->>G: POST /internal/lens/gx-score
    G-->>V: scores

    V-->>P: SSE result: winning code
    P-->>P: Write file to disk
    P-->>A: SSE stream: file content
    A-->>U: File created
```

Minimum 3 llama-server calls for algorithmic tasks (1 probe generation + 1 self-test generation + 1 embedding extraction); interactive tasks (games, UIs, framework code) skip self-test generation, so their minimum is 2. Maximum 30+ if Phase 3 repair engages all strategies.

### Edit Existing Code

```mermaid
sequenceDiagram
    participant U as User
    participant A as Client
    participant P as atlas-proxy :8090
    participant L as llama-server :8080

    U->>A: "Fix the bug in auth.py"
    A->>P: POST /v1/agent (SSE)
    P->>L: POST /v1/chat/completions<br/>response_format: json_object
    L-->>P: {"type":"tool_call","name":"read_file","args":{"path":"auth.py"}}
    P-->>P: Read file from disk
    P->>L: POST /v1/chat/completions (with file content)
    L-->>P: {"type":"tool_call","name":"edit_file","args":{"old_str":"...","new_str":"..."}}
    P-->>P: Apply old_str→new_str replacement
    P->>L: POST /v1/chat/completions (with edit result)
    L-->>P: {"type":"done","summary":"Fixed auth bug"}
    P-->>A: SSE stream: edited content
    A-->>U: File updated
```

Existing files over 5 lines are rejected for `write_file` — the model must use `edit_file` (surgical, ≤10 lines) or `structural_edit` (whole-node rewrite, .py/.html/.htm only). On `.py`/`.html`/`.htm` files, the per-step grammar gate (BiasBusters #2) actively bans `edit_file`/`write_file` from the tool-name production for the next decision so the model can't relapse to the wrong shortcut.
