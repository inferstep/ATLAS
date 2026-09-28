# ADR 0008: Harness mechanisms over model instructions

Status: accepted 2026-08; revised 2026-09-26 — the claim that the remaining
failures were model behaviour is withdrawn (see Revision). The decision stands.

## Context
Nine consecutive dogfooding runs of one task against
`gemma-4-12b-it-Q4_K_M` produced zero complete, correct results. The
harness defects found at the time were fixed (see CHANGELOG, the
2026-08-01/02 gate work), and the interventions tried in those runs
separate into two kinds. More harness defects affecting the same runs
were found in the following days; see the Revision below.

- Every **mechanism** added — syntax gates, the embedded-script gate,
  the stopped-render-loop and duplicate-binding checks, the node-size
  precondition — fired and prevented the failure it targeted. (One of
  them, the syntax gate's refusal of new files, was later found to
  cause failures of its own; see the Revision.)
- Every **instruction** improved — better rejection wording, a more
  precise verification message, tool guidance naming the right tool —
  was ignored at least once. Run 9 re-sent a byte-identical tool call
  against a rejection that named the file, the line, the cause and two
  concrete fixes.

Two 2026 results frame the same split. arXiv:2605.00334 (AgentFloor, 16
open-weight models 0.27B-32B, 16k+ runs) found no prompt-side lever that
transferred across models, and one structured-decomposition prompt that
regressed every model tried. arXiv:2605.22166 (Life-Harness, training-free,
evolved on Qwen3-4B and frozen) improved 116/126 model-environment settings
across 18 backbones, and its own ablation attributes the largest drops to
its two non-prompt layers — action validation and trajectory regulation.

Citations verified against arXiv 2026-08-02. Note that arXiv:2606.01522,
cited elsewhere as the calibration for a retry cap, is about error-message
detail and repair success and says nothing about retry limits; the
byte-identical rule below rests on our own run data and on determinism,
not on that paper.

## Revision (2026-09-26)
This ADR originally said "The harness defects those runs exposed are
fixed ... What remained was model behaviour", and ended "None of this is
expected to make a weak model complete tasks it otherwise fails ... Task
completion remains bounded by the model, which is what the quant and
control-vector work addresses." Both claims are withdrawn. Nothing in
the nine runs separated model behaviour from harness behaviour, and
commits over the next four days found harness causes for failures the
ADR had assigned to the model:

- c4012b6 (2026-08-02, an hour after this ADR): the error-loop breaker
  killed runs that were converging, each attempt answering the previous
  rejection. With it fixed, run 12 produced the first correct
  implementation of the task.
- 9a9517e: a context overflow ended runs at turn 3. The last-read
  restatement was appended after the history budget was spent. The
  commit's own words: the "model is done after three tool calls"
  symptom "was not the model giving up".
- b272fa8: a parse failure was the proxy's own content-loop cut, then
  diagnosed as a token-cap truncation, so the model was told to do the
  wrong thing.
- 8fcec3a and 8e6a5a1: the planner told the model to recreate a
  2000-line fixture, and could not see the workspace it planned against.
- 663406a: this ADR's change 1, the identical-retry refusal, blocked the
  verify-fix-verify loop (re-running `pytest` after a fix) and was
  narrowed.
- 47be143 (2026-08-06): the syntax gate's refusal of an unparseable new
  file blocked the write, run, read-the-traceback recovery. Three AoC
  sessions and a novel-benchmark session ended with the file never
  created. A mechanism can cause failures as well as prevent them.
- e40a63d ([ADR 0009](0009-embedding-convention-owned-client-side.md)):
  the lens scored every candidate 0.500, so candidate selection in these
  runs ran with every candidate tied.

What remains of the decision is unchanged: a mechanism changes what the
model can do or is shown, an instruction is per-model configuration, and
neither is assumed to be free of side effects. The cause of the task
failures that remain is not established. Model-side work (quant,
control vectors) tests one hypothesis. Harness causes are the other, and
telling them apart takes a controlled comparison: the same tasks and
harness across two models or quants, or one model across harness
versions.

## Decision
Interventions that change what the model **can** do, or what it is
**shown**, are shipped unconditionally and are expected to be
model-agnostic. Interventions that ask the model to **behave**
differently are treated as per-model configuration, and are never the
sole mechanism protecting a correctness property.

Three changes follow from it directly, all model-agnostic:

1. **A byte-identical re-send of a rejected tool call is refused before
   it executes** (`identicalRetryRefusal`). The harness is
   deterministic, so the same call against the same workspace produces
   the same rejection. Scoped to calls that failed — re-reading a file
   after editing it is byte-identical and correct — and cleared when the
   same call later succeeds. This replaces nothing: the existing
   repetition detector needs three occurrences and steers the following
   turn, which an identical pair never reaches.

2. **A completion claim the run cannot support is replaced by a
   harness-authored summary** (`unverifiedSummary`). The verification
   gate bounces `done` three times and then lets it through; the user
   saw the model's claim. The harness now states what was written and
   that nothing verified it, keeping the model's account labelled as
   unverified.

3. **`outline_file` reports embedded-language regions**
   (`embedded_region_outline`), reusing the block extraction the
   embedded-script gate already performs. The host grammar cannot see
   into a string literal, so the outline of a Flask app whose UI is one
   template named `function:index` and nothing else. It now names the
   `<script>` region, its line range, and the functions inside it,
   together with the fact that no selector reaches them.

Making `done` ungrammatical was considered and rejected: it requires
strict schema-GBNF, and Gemma-family models require
`ATLAS_GRAMMAR_MODE=loose` or they emit `done` instead of calling tools
at all. Rewriting the summary achieves the same user-facing property
without depending on the grammar mode.

## Consequences
Per-model configuration is now an explicit category rather than an
accident. Grammar strictness, sampler profile, reasoning mode and quant
choice already live in `.env`; the direction of travel is a per-model
harness profile alongside the existing per-model Lens/ASA bundles (ADR
0003), populated by a probe run at model-registration time. That is not
built here, and should not be until a probe has demonstrated it predicts
something — the probe-to-policy mapping is an engineering bet, not a
validated design, and Life-Harness got its transfer result by evolving a
harness against traces rather than by running a fixed battery.

These changes reduce wasted turns and stop unsupported success claims
reaching the user. Whether the tasks that still fail are limited by the
model or by the harness is not established (see the Revision). The
quant and control-vector work tests one of those hypotheses.
