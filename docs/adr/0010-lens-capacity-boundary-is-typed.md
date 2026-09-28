# ADR 0010: The embedding capacity limit is a typed boundary, not a score

Status: accepted 2026-09

## Context
llama-server processes an `/embedding` request in one physical batch
(`-ub`, `ATLAS_UBATCH`) and refuses a longer input with HTTP 500:

    input (2055 tokens) is too large to process. increase the physical
    batch size (current batch size: 2048)

Every Lens score is one forward over the whole sequence: whole-text C(x)
and G(x) read the mean of all per-token hidden states, and the per-step
path reads each token's state from the same forward. In a candidate
selection acquisition on 2026-09-04 one candidate of 2,055 tokens met a
physical batch of 2,048. The Lens caught the error and answered with its
defaults: energy 0.0, normalized 0.5, G(x) 0.5, verdict "error". The
min-energy selector reads 0.0 as the best energy in the pool, the
allocator reads 0.5 as a neutral verdict, and the record carried no trace
of why. The acquisition's mechanism gate stopped the analysis on exactly
that record.

The limit is not a property of the text. The generation budgets that
produce candidates (PlanSearch code at 4,096 tokens, DivSampling tiers up
to 12,288, the proxy's `ATLAS_MAX_TOKENS` at 8,192 for a write) all exceed
the largest micro-batch `atlas tier fit` will choose (2,048), and the
compute buffer grows with the micro-batch (about ubatch × n_embd × 280
bytes: 4.4 GB at 4,096 on a 3,840-dim model), so no configuration on the
reference hardware scores every candidate ATLAS can generate.

Splitting the input is not a score either. Tokens in a later piece are
embedded without the context of the earlier ones, so the pooled vector and
the per-token states are not the ones the artifacts were fitted on, and
the length baseline in `cx_normalization.json` was fitted on whole
sequences. `atlas lens build` does average line-boundary chunks for
training samples the server refuses; that convention has never been
measured against whole-sequence scoring, and the shipped calibration
records no length range. Serving a chunked score as calibrated would
change C(x), G(x) and the ranking with no evidence behind the change.

## Decision
The Lens reports a score it did not compute as a typed failure and never
as a number. Every scoring answer says `scored`; an unscored answer
carries `failure.kind` (`embed_capacity` with the server's `input_tokens`
and `capacity_tokens`, `model_server_error`, `model_server_unreachable`,
`embedding_contract`, `nonfinite_score` for a NaN or infinite value,
`internal`) and `null` in every score field. Consumers read a score
field that is not a finite number as unscored as well. Consumers keep the candidate, its identity and its sandbox result, record
the failure, rank the candidate after every scored one, and deliver it
only as the last verified candidate standing, saying so. The proxy logs
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
measured C(x) and G(x) distributions across the full length range, a
length baseline refit over that range, and the same acquisition rerun.
That is a separate research slice, not a serving change.

## Consequences
An unscored candidate can no longer win selection on a default value,
and the pool record says which candidate was unscored and why. A
deployment whose micro-batch is below its generation ceiling reports it
in `atlas doctor`, the TUI badge and `atlas lens check`, and long
candidates in that deployment are delivered on sandbox evidence alone
when nothing scored passes. Raising `ATLAS_UBATCH` remains a measured
decision about VRAM, not something this change makes.
