# ADR 0004: V3 failures fall back to the model's own content

Status: accepted (V3.1.x behavior, E2E-pinned 2026-07); narrowed by 0011
(2026-09): a lens that cannot score stops the run, with no fallback

## Context
The V3 pipeline (candidates, scoring, selection) sits between the
model's proposed write and the disk. It can be unavailable, slow, or
return garbage.

## Decision
V3 errors, timeouts (ATLAS_V3_TIMEOUT, default 180s), and malformed
responses fall back to writing the model's own content directly, with
the fallback visible (logged, v3_used unset in the tool result) — never
a silent skip, never a hard turn failure. Rationale: the model's
content already passed the syntax/guardrail gates, so the fallback is
safe, and an unavailable enhancement layer must not brick the product.

## Consequences
Users on a degraded stack silently lose the quality uplift but keep a
working agent; the tool result and events make the degradation
observable. Pinned by tests/e2e/test_v3_lens_acceptance.py failure
modes.

## Revision 2026-09-27
The fallback stays, and the run's final summary now says so too: it
names each file whose bytes on disk were written after V3 ran out of
time or was unavailable ("V3 did not check these files: ..."). A file
changed afterwards is not named. `proxy/v3_fallback_note.go`, pinned
by `proxy/v3_fallback_note_test.go`. A lens that cannot score is not
this case: the run stops (ADR 0011).
