# ADR 0004: V3 failures fall back to the model's own content

Status: accepted (V3.1.x behavior, E2E-pinned 2026-07); narrowed by
[0011](0011-the-lens-is-required.md) (2026-09): a lens that cannot score
stops the run, with no fallback; revised 2026-09-27: the final summary
names the files V3 did not check

## Context
The V3 pipeline (candidates, scoring, selection) sits between the
model's proposed write and the disk. It can be unavailable, slow, or
return garbage.

## Decision
V3 errors, timeouts and malformed responses fall back to writing the
model's own content directly, with the fallback visible: never a silent
skip, never a hard turn failure (`writeFileWithV3` in `proxy/tool_write_v3.go`,
`deliverEditCandidate` in `proxy/edit_route_delivery.go`). One V3 call
gets at most `ATLAS_V3_TIMEOUT` (default 300 s) or half the time the
session has left, whichever is shorter; `ATLAS_V3_TIMEOUT=0` turns off
both caps (`v3CallTimeout` and `callV3GenerateStreaming` in
`proxy/v3_bridge.go`).

Rationale: the fallback content still passes the gates every write
passes (syntax, structural and comparative; `writeWithoutCandidate` in
`proxy/tool_write_v3.go`), so the fallback is safe, and an unavailable
enhancement layer must not brick the product.

A lens that cannot score is not a V3 failure in this sense: nothing is
written and the run stops ([ADR 0011](0011-the-lens-is-required.md)).

## Consequences
Users on a degraded stack lose what V3 adds but keep a working agent,
and they can see it. The tool result does not claim V3 (`v3_used` is
unset). A `write_file` fallback also sends a text event that says V3
was unavailable or ran out of time (`writeWithoutCandidate` in
`proxy/tool_write_v3.go`); an edit fallback keeps the caller's content and only
logs the reason (`deliverEditCandidate`). The run's
final summary names each file whose bytes on disk V3 did not check,
with the reason ("V3 did not check these files: ..."). A file changed
afterwards is not named (`v3FallbackNote` in
`proxy/v3_fallback_note.go`, pinned by `proxy/v3_fallback_note_test.go`).
The fallback is pinned end to end by
`tests/e2e/test_v3_lens_acceptance.py`
(`test_v3_failure_modes_fall_back_visibly`).
