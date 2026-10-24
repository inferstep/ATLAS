# ADR 0008: Harness mechanisms over model instructions

Status: accepted 2026-08; revised 2026-09-26 — the claim that the remaining
failures were model behaviour is withdrawn. The decision stands.

## Context
An intervention in the agent loop is one of two kinds. A **mechanism**
changes what the model can do or what it is shown: a gate that refuses
a write, a check that runs before a tool call executes, an outline that
names what a file holds. An **instruction** asks the model to behave
differently: clearer rejection wording, a more precise verification
message, tool guidance that names the right tool.

A mechanism acts the same way whatever the model. Whether an instruction
is followed depends on the model: a model can answer a rejection that
names the file, the line, the cause and two concrete fixes with the same
call, byte for byte (a recorded case is in the comment of
`identicalRetryRefusal` in `proxy/detectors.go`).

Two 2026 results frame the same split. arXiv:2605.00334 (AgentFloor, 16
open-weight models 0.27B-32B, 16k+ runs) found no prompt-side lever that
transferred across models, and one structured-decomposition prompt that
regressed every model tried. arXiv:2605.22166 (Life-Harness, training-free,
evolved on Qwen3-4B and frozen) improved 116/126 model-environment settings
across 18 backbones, and its own ablation attributes the largest drops to
its two non-prompt layers — action validation and trajectory regulation.

A mechanism is not free of side effects either. It can cause failures as
well as prevent them: refusing a new file that does not parse would block
the loop of writing it, running it and reading the traceback, so such a
file lands with a warning ([CHANGELOG](../../CHANGELOG.md), "Changed: a
new file that does not parse lands with a warning").

## Decision
Interventions that change what the model **can** do, or what it is
**shown**, are shipped unconditionally and are expected to be
model-agnostic. Interventions that ask the model to **behave**
differently are treated as per-model configuration, and are never the
sole mechanism protecting a correctness property. Neither kind is
assumed to be free of side effects.

Three changes follow from it directly, all model-agnostic:

1. **A byte-identical re-send of a rejected tool call is refused before
   it executes** (`identicalRetryRefusal` in `proxy/detectors.go`). The
   harness is deterministic, so the same call against the same workspace
   produces the same rejection. Only failed calls are remembered, and a
   call that succeeds clears them all, because a success may have changed
   the workspace (`clearFailedToolCall`). Re-running a test after a
   successful fix is therefore not refused, and neither is re-reading a
   file after an edit. Polling a background job (`tail_background`) is
   exempt. This replaces nothing: the repetition detector needs three
   occurrences of a call and steers the following turn (`recordToolCall`
   and `toolRepeatThreshold` in `proxy/detectors.go`), which an identical
   pair never reaches.

2. **A completion claim the run cannot support is replaced by a
   harness-authored summary** (`unverifiedSummary` in
   `proxy/guardrails.go`). The verification gate bounces `done` three
   times and then lets it through (`maxGateBounces` in `proxy/agent.go`),
   so without this the user would read the model's claim unchanged. The
   harness states whether anything was written and that nothing verified
   it. When the run counts as completed, it keeps the model's account,
   labelled as unverified; otherwise it drops it (`modelProseIfAuthorized`
   in `proxy/agent.go`).

3. **`outline_file` reports embedded-language regions**
   (`embedded_region_outline` in `v3-service/symbols.py`), reusing the
   block extraction the embedded-script gate already performs. The host
   grammar cannot see into a string literal, so the outline of a Flask
   app whose UI is one template would name `function:index` and nothing
   else. The outline names the `<script>` region, its line range, and the
   functions inside it, together with the fact that no selector reaches
   them (`embeddedRegionNote` in `proxy/tools.go`).

`done` is not made ungrammatical. That needs strict schema-GBNF, and the
gemma entry runs with `ATLAS_GRAMMAR_MODE=loose` (its `grammar_mode` in
`atlas/commands/model_registry.py`). Rewriting the summary gives the user
the same property without depending on the grammar mode.

## Consequences
Per-model configuration is an explicit category. Grammar strictness,
sampler profile, reasoning mode and quant choice already live in `.env`;
the direction of travel is a per-model
harness profile alongside the existing per-model Lens/ASA bundles (ADR
0003), populated by a probe run at model-registration time. That is not
built here, and should not be until a probe has demonstrated it predicts
something — the probe-to-policy mapping is an engineering bet, not a
validated design, and Life-Harness got its transfer result by evolving a
harness against traces rather than by running a fixed battery.

The first two changes keep a rejected call from running again unchanged,
and keep an unsupported success claim from reaching the user as the
model wrote it. They do not tell what causes the task failures that
remain. Model-side work (quant, control vectors) tests one hypothesis;
harness causes are the other. Telling them apart takes a controlled
comparison: the same tasks and harness across two models or quants, or
one model across harness versions.
