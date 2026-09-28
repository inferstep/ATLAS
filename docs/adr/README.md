# Architecture Decision Records

An ADR records a significant decision: its context, the decision, and its
consequences. Significant means what [GOVERNANCE](../../GOVERNANCE.md#proposals-rfc--adr--epic)
lists: architecture changes, new dependencies, security-model changes,
release policy, support-matrix changes, and any breaking change. Each one
starts as an **RFC issue**.

## Status

The line under the title gives the status, the date, and the RFC issue:

```
Status: Proposed. RFC: #123
Status: Accepted 2026-10. RFC: #123
Status: Superseded by 0012 (2026-11)
```

| Status | Meaning |
|---|---|
| Proposed | Under discussion. The RFC's comment period is open, and the ADR is a pull request. |
| Accepted | Decided. The ADR is merged and the RFC issue became an Epic. |
| Superseded by NNNN | Replaced by a later ADR, which names this one. |

An accepted ADR isn't rewritten. A later decision gets a new ADR that
references it. Smaller changes are added to the status line with a date:
*narrowed by NNNN* (a later ADR limits part of it), *revised* (a dated
correction, explained in the text), or *retired* (the thing it governs was
removed).

## Writing one

1. Open the **RFC** issue form. The comment period is 7 days.
2. For line-by-line review, open a pull request adding
   `docs/adr/NNNN-short-title.md`, with the next free number and status
   Proposed.
3. Use three sections: **Context**, **Decision**, **Consequences**.
4. After a maintainer accepts the RFC, set the status to Accepted with the
   month and merge.

## Index

| ADR | Decision | Status |
|---|---|---|
| [0001](0001-local-single-user-trust-model.md) | Local single-user trust model | Accepted |
| [0002](0002-keep-redis.md) | Keep Redis as the lens-service state store | Superseded by 0007 |
| [0003](0003-per-model-bundles.md) | Direct mode model-agnostic; V3/Lens/ASA per-model bundles | Accepted |
| [0004](0004-v3-fail-soft.md) | V3 failures fall back to the model's own content | Accepted, narrowed by 0011 |
| [0005](0005-lens-optionality.md) | Lens is optional and calibration-gated | Superseded by 0011 |
| [0006](0006-release-strategy.md) | Two-phase, test-gated, signed image releases | Accepted |
| [0007](0007-sqlite-state-store.md) | SQLite as the lens-service state store | Accepted, retired 2026-09 |
| [0008](0008-harness-mechanisms-over-model-instructions.md) | Harness mechanisms over model instructions | Accepted, revised 2026-09 |
| [0009](0009-embedding-convention-owned-client-side.md) | The lens owns its embedding convention | Accepted |
| [0010](0010-lens-capacity-boundary-is-typed.md) | The embedding capacity limit is a typed boundary, not a score | Accepted |
| [0011](0011-the-lens-is-required.md) | The lens is required | Accepted |
