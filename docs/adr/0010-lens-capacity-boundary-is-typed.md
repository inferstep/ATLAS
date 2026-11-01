# ADR 0010: The embedding capacity limit is a typed boundary, not a score

Status: accepted 2026-09

## Context
llama-server processes an `/embedding` request in one physical batch
(`-ub`, `ATLAS_UBATCH`) and refuses a longer input with HTTP 500:

    input (2055 tokens) is too large to process. increase the physical
    batch size (current batch size: 2048)

Every Lens score is one forward over the whole sequence: whole-text C(x)
and G(x) read the mean of all per-token hidden states, and the per-step
path reads each token's state from the same forward
(`geometric-lens/geometric_lens/embed_capacity.py`,
`embedding_extractor.py`). A number put in place of a score reads as a
verdict: to the min-energy selector, energy 0.0 is the best energy in the
pool, and to the allocator, 0.5 is a neutral verdict
(`v3-service/scoring.py`). The case that showed it is in the
[CHANGELOG](../../CHANGELOG.md) ("Lens scoring boundary").

The limit is not a property of the text. The generation budgets that
produce candidates (PlanSearch code at 4,096 tokens, DivSampling tiers up
to 12,288, the proxy's `ATLAS_MAX_TOKENS` at 8,192 for a write) all exceed
the largest micro-batch `atlas tier fit` will choose (2,048), and the
compute buffer grows with the micro-batch (about ubatch × n_embd × 280
bytes: 4.4 GB at 4,096 on a 3,840-dim model), so no configuration on the
reference hardware scores every candidate ATLAS can generate
(`v3-service/stages/plan_search.py`; `get_max_tokens` in
`v3-service/stages/budget_forcing.py`; `agentMaxTokens` in
`proxy/system_prompt.go`; `fit_runtime_knobs` in `atlas/commands/fit.py`).

Splitting the input is not a score either. Tokens in a later piece are
embedded without the context of the earlier ones, so the pooled vector and
the per-token states are not the ones the artifacts were fitted on.
`atlas lens build` does average line-boundary chunks for
training samples the server refuses (`_embed_text` in
`atlas/commands/lens.py`), but the shipped calibration records no length
range (`geometric-lens/geometric_lens/models/cx_normalization.json`), so
nothing in it shows that a chunked score means what a whole-sequence score
means. Serving a chunked score as calibrated would change C(x), G(x) and
the ranking with no evidence behind the change.

## Decision
The Lens reports an input it did not score as a typed failure, not as a
number. An answer from a lens with its models loaded says `scored`; an
unscored answer carries `failure.kind` and no number in any score field.
The kinds are `embed_capacity` (with the server's `input_tokens` and
`capacity_tokens`), `model_server_error`, `model_server_unreachable`,
`embedding_contract`, `nonfinite_score` for a NaN or infinite value, and
`internal` (`failure_from_exception` in
`geometric-lens/geometric_lens/embed_capacity.py`). The per-step path with
a `layer` answers `empty_input` when the server returns no token vectors;
on the other paths an empty answer raises and is typed `internal`, which
consumers read as the lens down (`evaluate_per_step` in
`geometric-lens/geometric_lens/service.py`;
[ADR 0011](0011-the-lens-is-required.md)).

Some answers carry numbers that are not scores. With no model loaded, the
whole-text answer says `enabled: false`, which the product reads as the
lens down and the bench as neutral (`NEUTRAL_COMBINED` in
`atlas/bench/best_of_k.py`). With no G(x) model, every G(x) field holds 0.5
and the answer says `gx_available: false`. With no `cx_normalization.json`,
normalized C(x) is 0.5 and the answer says `cx_calibrated: false`; such a
lens still scores with its raw energies
([ADR 0011](0011-the-lens-is-required.md)). These are in
`evaluate_combined` and `evaluate_per_step` (`service.py`) and
`normalize_cx_energy` (`geometric-lens/geometric_lens/calibration.py`). The
bench's `/internal/lens/score-text` answers `models_not_loaded` when no
model is loaded (`geometric-lens/main.py`).

Consumers read a score field that is not a finite number as unscored as
well (`tests/v3-service/lens/test_lens_nonfinite_scores.py`). Consumers keep the
candidate, its identity and its sandbox result, record the failure, rank
the candidate after every scored one, and deliver it only as the last
verified candidate standing, saying so
(`tests/v3-service/lens/test_lens_unscored_candidates.py`). The proxy logs
an unscored write and applies no threshold to it. The bench does the same
for its own pool.

The capacity is reported, not gated. The lens reports the physical batch
it knows (declared through `LLAMA_EMBED_CAPACITY_TOKENS`, which compose
sets from `ATLAS_UBATCH`, or observed from a refusal, which replaces the
declaration) on `/health` and `/ready`, and the proxy's status dimensions
mark `lens_scoring` partial when that capacity is below the per-turn
generation ceiling. `/ready` keeps its gate: a deployment that scores
every input shorter than its batch is a working deployment.

No input is truncated and no input is split in the serving path. Scoring
past the physical batch needs one convention for training and serving,
measured C(x) and G(x) distributions across the full length range, and
a length baseline refit over that range. That is a separate research
slice, not a serving change.

## Consequences
An unscored candidate cannot win selection on a default value,
and the pool record says which candidate was unscored and why. A
deployment whose micro-batch is below its generation ceiling reports it
in `atlas doctor`, the TUI badge and `atlas lens check`, and long
candidates in that deployment are delivered on sandbox evidence alone
when nothing scored passes. Whether to raise `ATLAS_UBATCH` is a
decision about VRAM, made on a measurement.
