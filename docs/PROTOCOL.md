# ATLAS Event Protocol

ATLAS services emit a typed JSON event stream over Server-Sent Events (SSE). This document is the wire-format spec — atlas-proxy (Go) is the live envelope producer. The Go TUI consumes the stream via its own implementation of this contract (`tui/chat.go`); `atlas/events.py` is the canonical Python spec implementation (envelope types, `make_event`, `parse_envelope`), used by the test suite via the consumer harness in `tests/cli/event_harness.py`.

## Transport

**Server-Sent Events (SSE)** — `text/event-stream`, server-push only. Cancellation is out of scope for this protocol; clients cancel via [`POST /cancel`](API.md#post-cancel).

Each event arrives as one SSE frame:

```
data: {"event_id":"evt_aabb1122",...}

```

(Two newlines terminate the frame.) The Python test helper `iter_sse_lines` (`tests/cli/event_harness.py`) handles the framing.

### SSE control frames (server → client only)

The protocol uses three SSE comment / control patterns. None are envelope events; consumers must skip them via the lines parser:

| Frame | When | Why |
|---|---|---|
| `: connected\n\n` | First body byte after a successful `/events` connection (atlas-proxy only) | Forces the response headers + first body chunk to leave the server immediately. Without it, Go buffers the response until the first envelope or 15s heartbeat fires, and clients with short connect timeouts see "no response received". |
| `: heartbeat\n\n` | Every 15s during quiet stretches (atlas-proxy only) | Keeps proxies / load balancers from idling out the connection. |
| `event: result\ndata: {...}\n\n` | Right before stream end on v3-service's `/v3/generate` and `/v3/plan` | Carries the final `result` dict (pipeline result or plan). The proxy bridge consumes it to build the tool result / plan; envelope subscribers on `/events` never see it. |

The Python `iter_sse_lines` helper already filters comment lines (any line starting with `:`) automatically. Named-event lines (`event: result`) come through prefixed (`result: <data>`) so the caller can distinguish them.

## Single-session broadcast model (current limitation)

atlas-proxy's `/events` endpoint broadcasts every envelope from every concurrent agent session to every connected subscriber — there is no `session_id` field in the envelope and no `?session_id=X` filter on the endpoint. With one ATLAS running at a time, a subscriber sees only its own session. v3-service's SSE endpoints are per-request streaming, so no interleaving occurs there.

## Envelope

Every event is a JSON object with this shape:

```json
{
  "event_id":    "evt_<8 hex chars>",
  "timestamp":   1714600000.123,
  "type":        "stage_start" | "stage_end" | "tool_call" | "tool_result" | "metric" | "error" | "done",
  "stage":       "<pipeline stage name>",
  "duration_ms": 4523,                  // optional — set on stage_end / tool_result
  "payload":     { ... type-specific ... }
}
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `event_id` | string | yes | Format: `evt_` + 8 hex chars. Session-unique. The Go producer draws the 4 bytes from `crypto/rand` (`proxy/events.go`); the Python helper truncates a uuid4. |
| `timestamp` | number | yes | Unix seconds with microsecond precision. Producers MUST emit in monotonically non-decreasing order. |
| `type` | string | yes | One of the seven legal values listed below. |
| `stage` | string | yes | Logical pipeline stage (`agent`, `llm`, `tool`, `v3:<stage>`, etc.). The proxy's v3 bridge prefixes the v3-service stage name with `v3:` and passes it through verbatim — suffixes like `_pass`/`_done`/`_failed` are kept (e.g. `v3:sandbox_pass`). |
| `payload` | object | yes | Type-specific. Always an object, never null. |
| `duration_ms` | int | optional | Set on `stage_end` and `tool_result` — wall-clock duration of the stage / tool execution. |

## Event types

### `stage_start`

A logical stage has begun. Pairs with a later `stage_end` event (typically — long-running stages that crash without an end event are valid; consumers must handle missing `stage_end`).

```json
{
  "type": "stage_start",
  "stage": "v3:phase2",
  "payload": {
    "detail": "allocating candidates"   // optional human-readable
  }
}
```

### `stage_end`

A logical stage finished. `success` is required so consumers can distinguish completed-OK from completed-failed without inspecting the next event.

```json
{
  "type": "stage_end",
  "stage": "v3:phase2",
  "duration_ms": 4523,
  "payload": {
    "success": true,
    "detail": "selected candidate #3",   // optional
    "summary": "..."                     // optional
  }
}
```

### `tool_call`

The agent is invoking a tool. When the tool executes, a `tool_result` for the same tool name follows within the same stage (typically a few hundred ms later). Pre-execution rejections — permission denial, truncated-args detection, workspace-boundary violation — emit no `tool_result`, so consumers must tolerate an unpaired `tool_call`.

```json
{
  "type": "tool_call",
  "stage": "tool",
  "payload": {
    "name": "edit_file",
    "args_summary": "src/snake.py: replace render() body",   // truncated to 80 chars
    "turn": 3
  }
}
```

### `tool_result`

```json
{
  "type": "tool_result",
  "stage": "tool",
  "duration_ms": 487,
  "payload": {
    "name": "edit_file",
    "success": true,
    "error": ""   // failure detail, truncated to 120 chars; empty on success
  }
}
```

### `metric`

A measured value worth surfacing. `value` is loosely typed — a number (e.g. `{"name": "total_tokens", "value": 12500}` on the `llm` stage) or a string. The v3 bridge emits repeated frames within one v3 stage as progress metrics whose `value` is the human-readable detail line:

```json
{
  "type": "metric",
  "stage": "v3:sandbox_test",
  "payload": {
    "name": "progress",
    "value": "Testing 3 candidates..."
  }
}
```

`unit` (string) is an optional third payload field.

### `error`

Something went wrong. The payload carries `message` only; the envelope's top-level `stage` says where.

```json
{
  "type": "error",
  "stage": "llm",
  "payload": {
    "message": "model output was empty"
  }
}
```

### `done`

Closes one agent pass. The `/events` broker is a persistent stream — it keeps heartbeating after a `done`, emitting one per pass — so EOF-without-`done` indicates truncation (network drop, server crash) only for consumers reading a single pass.

```json
{
  "type": "done",
  "stage": "pipeline",
  "payload": {
    "success": true,
    "total_duration_ms": 12453,
    "summary": "..."   // optional
  }
}
```

The agent-loop `done` (`stage: "agent"`) additionally carries the run's outcome, from the same fields as the SSE terminal: `status` (`completed`, `incomplete`, `stopped`, `failed` or `timed_out`; `success` is `status == "completed"`), `reason`, and `unresolved` — the exit gates that spent their bounces with their finding still true, comma-separated, present only when there are some. It also carries the run's token accounting: `total_tokens`, plus `fenced_calls` / `fenced_tokens` — how many `@fenced` sub-call generations the pass made and what they cost, already folded into `total_tokens`. Each individual sub-call attempt also emits a `stage_end` on stage `fenced_fetch` with `path`, `attempt`, `generated_tokens`, `content_bytes`, and `success` in the payload, so a retried fetch is visible as two attempts.

## Producer endpoints

| Service | Endpoint | Notes |
|---|---|---|
| atlas-proxy | `GET /events` | Broadcasts all envelope events from any active session to every connected subscriber. Heartbeat every 15s to defeat proxy idle timeouts. |
| v3-service | `POST /v3/generate`, `POST /v3/plan` | Emit legacy `{stage, detail}` frames plus a terminal `event: result` control frame — no envelopes (see below). |

## v3-service streams and typed events

v3-service's endpoints emit the legacy `{stage, detail}` shape (with an optional `data` key when structured data rides along). Envelope consumers subscribe to atlas-proxy's `GET /events` instead — the proxy's v3 bridge translates the `{stage, detail}` frames it receives from v3-service into envelopes.

Consumers reading a mixed stream must filter the non-envelope frames — the Python test helper `iter_events()` (`tests/cli/event_harness.py`) does this automatically: `{stage, detail}` frames (with or without `data`) raise `LegacyEventError` and are skipped, and `result:`/`done:` control frames are skipped as stream control rather than parsed as envelopes.

## Python implementation: `atlas/events.py`

`atlas/events.py` is the spec module shipped with the CLI: `EVENT_TYPES`, the `Event` dataclass, `make_event(type, stage, payload)` for producers, and `parse_envelope(blob)` for consumers — `parse_envelope` raises `LegacyEventError` if the blob is the v3-service `{stage, detail}` shape and `SchemaError` if it's malformed:

```python
from atlas.events import parse_envelope

ev = parse_envelope(blob)      # one frame → Event
print(ev.type, ev.stage, ev.payload)
```

The streaming consumer helpers (`iter_sse_lines`, `iter_events`, `is_terminal`, `collect`, `assert_monotonic`) live in the test harness, `tests/cli/event_harness.py` — they are assertion tooling for the suite, not part of the shipped package.

## Stage names from v3-service

The proxy's v3 bridge emits `v3:` + the v3-service stage name verbatim, suffixes included (`v3:sandbox_pass`). A stage-name change closes the previous stage (`stage_end`, `success: true`) and opens the new one (`stage_start`); a repeated stage name emits a `metric` (`name: "progress"`).

## Schema versioning

This document describes **v1** of the protocol. Future schema changes:

- **Backward-compatible additions** (new event types, new optional payload fields): bump the `Accept` header version (`application/json+envelope; v=2`). Consumers that don't recognize the version still work — they just ignore unknown event types.
- **Breaking changes** (renamed fields, type changes): require a new endpoint path (`/events/v2`). The old endpoint stays for one release window.

## Test contract

The schema is pinned by `tests/cli/test_events.py` (Python consumer) and `proxy/events_test.go` (Go producer). Any change to the wire format MUST update both, in lockstep. Producer/consumer drift (proxy emissions ↔ TUI handlers, envelope-type parity across implementations) is pinned by `tests/contracts/test_event_contract.py`.
