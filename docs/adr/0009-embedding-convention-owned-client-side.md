# ADR 0009: The lens owns its embedding convention

Status: accepted 2026-08

## Context
`/embedding` on llama-server has two consumers inside ATLAS:

- whole-text `C(x)`/`G(x)`, which scores a completed candidate from one
  pooled vector (`extract_embedding`), and
- the per-step path (`/internal/lens/score-per-step`), which scores every
  token of a candidate and needs per-token vectors (`extract_per_token`).

Both functions are in `geometric-lens/geometric_lens/embedding_extractor.py`.
`--pooling` is server-global in llama.cpp, so one setting has to serve
both consumers, and only `--pooling none` returns per-token vectors
(`extract_per_token` refuses a pooled answer).

Scale carries the signal. The shipped cost field is fitted on
unnormalized mean-pooled vectors, so the lens keeps each vector's scale
unless the artifact's contract says it was trained normalized
(`extract_embedding`; `test_vector_scale_is_preserved` in
`geometric-lens/tests/test_embedding_contract.py`). Normalization does not
commute with pooling either: the mean of per-token unit vectors is not the
normalized mean of the raw vectors (`extract_embedding`).

A wrong convention passes the ordinary health checks: the services stay
ready, and C(x) returns a number in a plausible-looking range
(`_reject_prenormalized` in `embedding_extractor.py`). The drift check,
which re-scores fixed reference texts, is what catches a shift in every
energy, for a bundle that carries a `drift_fingerprint.json`
(`check_fingerprint` in `geometric-lens/geometric_lens/drift.py`).

## Decision
The lens derives the convention it needs from the most informative
response the server can give, instead of depending on server-side
settings to match its calibration.

- Requests pin `embd_normalize: -1` and pool client-side, in both
  `embedding_extractor.extract_embedding` and the bench's
  `extract_embedding_urllib`. The pooled vector is then identical under
  `--pooling none` and `--pooling mean`.
- `ATLAS_EMBED_POOLING` defaults to `none`. Per-token vectors pool down
  to the whole-text vector; a pooled response cannot be unpooled.
- Scale is preserved. Vectors are normalized only when an artifact's
  `embedding_contract` declares it was trained that way.
- A served shape that disagrees with the contract logs once instead of
  raising, because the shape no longer changes the resulting vector.
- A server that returns pre-normalized per-token vectors despite
  `embd_normalize: -1` is a hard error: the mean of unit vectors is a
  different direction than the pooled raw vector, and that difference is
  invisible in every health signal.

## Consequences
Pooling mode is an operational detail, not a correctness dependency. The
whole-text vector is the same under `--pooling none` and `--pooling mean`
(`test_pooling_mode_does_not_change_the_vector`), so both consumers share
one server running `--pooling none`. Normalization does not rescale the
input to a fitted model unless the artifact's contract says it was trained
normalized (`test_normalized_contract_still_normalizes`; both tests are in
`geometric-lens/tests/test_embedding_contract.py`).

A fail-soft default must not be indistinguishable from a real answer:
`0.500` from a scorer that never ran reads exactly like `0.500` from one
that did. Where a component degrades to a neutral value, the degradation
has to be visible in what it returns, not only in a log line. The lens
answers an input it did not score as a typed failure with no number, and
marks the fields it fills without a model or a calibration
(`gx_available`, `cx_calibrated`;
[ADR 0010](0010-lens-capacity-boundary-is-typed.md)). A lens that cannot
score stops the run ([ADR 0011](0011-the-lens-is-required.md)).
