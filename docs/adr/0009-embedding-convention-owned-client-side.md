# ADR 0009: The lens owns its embedding convention

Status: accepted 2026-08

## Context
`/embedding` on llama-server has two consumers inside ATLAS:

- whole-text `C(x)`/`G(x)`, which scores a completed candidate, and
- the per-step PRM path (`score-per-step`, PC-207), which scores every
  token of a candidate as it is produced.

The first wants one pooled vector; the second needs per-token vectors.
`--pooling` is server-global in llama.cpp, so one setting has to serve
both, and the two requirements were treated as a conflict to be pinned
rather than resolved. `mean` was pinned, and per-step raised
`ValueError: extract_per_token needs per-token embeddings` on every
call. The service fails soft, so each candidate scored the neutral
default 0.500 and candidate selection ran with every candidate tied.

Normalization failed the same way, one layer down. Training read
`/embedding` without an `embd_normalize` field; this llama.cpp build
defaults that field to L2, but the stored training vectors
(`training_embeddings_3840d.json`) have ‖v‖≈137, so the artifacts were
fitted on unnormalized vectors. Serving normalized ones fed the cost
field inputs 137x smaller than its calibration:

| input | ‖v‖ | C(x) |
|---|---|---|
| clean function | 110.5 | 2.56 |
| repetition loop | 142.9 | 9.46 |
| truncated junk | 97.1 | 2.00 |
| any of the three, L2-normalized | 1.00 | 0.76-0.78 |

Calibration is `pass_energy_mean` 9.25, `fail_energy_mean` 11.81. The
normalized column spans 0.02 across inputs a lens exists to tell apart.

Both defects report healthy. `/health` and `/ready` return 200, the
per-step endpoint returns `200 OK` carrying its neutral default, and
`C(x)` returns a number in a plausible-looking range. The documented
diagnostic in TROUBLESHOOTING.md asserted that a norm far from 1.0 meant
a misconfigured server, which is the inverse of the truth and made the
misconfiguration read as correct.

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
Pooling mode becomes an operational detail rather than a correctness
dependency, and per-step scoring produces signal for the first time in
this deployment. Normalization no longer silently rescales the input to
a fitted model.

Both measured failures shared a shape: a fail-soft default that is
indistinguishable from a real answer. `0.500` from a scorer that never
ran reads exactly like `0.500` from one that did. Where a component
degrades to a neutral value, the degradation needs to be visible in what
it returns, not only in a log line — `n_tokens: 0` and `latency_ms: 0`
were in the response the whole time and nothing was reading them.

Every benchmark number recorded before this change was produced with the
lens contributing nothing, so none of them is evidence for or against the
lens. [Ed. 2026-09-25: an earlier version of this paragraph cited a "54% to
75% task-success improvement" from harness fixes. No run in the repository
supports that figure, and it is withdrawn.]
Whether the lens earns its keep is now an open measurement, not a
settled one.
