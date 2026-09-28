# Candidate authorization: obligations, evidence, and candidate staging

**For a request that declares structured obligations, this decides whether a
generated candidate lands.** For a request that declares none it has nothing to
say, and the existing decision in [EVIDENCE_WIRE.md](EVIDENCE_WIRE.md) keeps its
exact previous behaviour.

It was observe-only for three slices first — computed beside the live answer and
discarded — so the two could be compared before either depended on the other.
That comparison is `proxy/delivery_agreement_test.go`, and it is what this now
rests on.

Feasibility is still observe-only: `observeInvocationFeasibility` runs before
generation, its value is discarded, and nothing is skipped.

## Obligations: what a task owes

A validated `TaskContract` is turned into typed obligations by the proxy
(`proxy/obligation_kinds.go`). Six kinds:

| Kind | Subject | Comes from |
| --- | --- | --- |
| `artifact_exists` | canonical path | each declared output |
| `syntactic_validity` | canonical path | a declared output the proxy's own gate governs |
| `declared_command` | the exact command string | each declared verification command, at the strength its typed declaration states |
| `declared_example` | an opaque case id | a declared example |
| `baseline_preserved` | canonical path | a declared output that already carries current evidence |
| `unsupported` | — | a class nothing can measure |

Each obligation carries a **role**, and the roles are what removed the circular
premise that an artifact had to exist before it could be authorized to exist:

- **`target_identity`** — names an artifact the client asked for. Necessary and
  never sufficient: naming a path is not evidence about bytes.
- **`authorization_prerequisite`** — must be demonstrated *before* the bytes may
  land.
- **`post_delivery_settlement`** — owed *after* delivery. `artifact_exists` is
  one: nothing can evidence a file's existence before it is written, so
  requiring it up front made every task unclosable.

### Typed verification requirements

A bare command string carries no semantics. `python3 -c "import ast; ..."` and
`pytest -q tests/` are the same shape to a parser and demonstrate very different
things, and nothing can tell them apart honestly by reading them — a filename,
the word pytest and the presence of assertions are all things a caller can write
over anything at all.

So strength comes from a typed declaration and from nowhere else:

```jsonc
"verification_requirements_version": 1,
"verification_requirements": [{
  "command": "pytest -q tests/test_report.py",   // exact bytes; identity is its hash
  "kind": "behavioral",                          // syntax | runtime | behavioral
  "expects": "exit_zero",                        // closed vocabulary
  "assets": ["tests/test_report.py"],            // what it runs against
  "asset_authority": "client_supplied"           // client_supplied | workspace_observed
}]
```

Rules:

- The `kind` is the required strength. Nothing infers it from command text,
  filename, `pytest`, assertions or prose.
- A command declared in the older `verification` list and not typed here maps to
  **`runtime`, never `behavioral`**: it ran against these bytes and exited zero,
  which is what such a command actually shows.
- `client_supplied` assets can support behavioral evidence. `workspace_observed`
  ones cap the observation at runtime — and an asset **this session wrote** is
  workspace-observed whatever the contract claims, because the ledger records
  that it did. A request may not write its own oracle.
- A behavioral requirement whose assets cap it at runtime does not reach its own
  floor, so it does not authorize. The requirement is not quietly downgraded.
- Every required command must pass. Timeout, cancellation, refusal, mutation,
  stale state or a changed asset produces no authority at all.
- An unreadable version, kind, expectation or asset authority is refused at the
  request boundary rather than defaulted.

## Evidence: what was actually observed

Evidence carries typed **provenance**, not a `trusted` boolean, because a
boolean cannot say what a source is *capable* of demonstrating. Each source has
a ceiling it may never exceed:

| Source | Ceiling | Produced by |
| --- | --- | --- |
| `proxy_owned_validation` | `syntax` | the structural gate, at the final-byte observation |
| `client_declared_verification` | `behavioral` | a declared command run against a staged candidate |
| `model_generated` | none — never authorizes | the model's own self-tests |

Two producers, each with **exactly one** production call path, both in
`proxy/evidence_wiring.go`:

- `observeDeliveredCandidateSyntax` runs at the moment the bytes are fixed and
  the structural gate has just reported on exactly those bytes. The verdict is
  handed over, never recomputed — no second sandbox call, no second opinion.
- `observeCandidateVerification` stages the candidate and runs the commands the
  client declared. It runs at all only for a request that declared commands.

`TestEachProducerHasExactlyOneEnumeratedCallPath` fails if either gains a second
caller: two callers means two places the rules can drift apart.

Every record names the request, invocation, candidate instance, candidate hash,
workspace generation and state, command identity, baseline identity and
obligation id. Identities and hashes only — no candidate byte, no command
string, no diagnostic text.

## Candidate staging

Behavioral evidence needs the client's declared command run against the
candidate. The candidate has not been chosen yet, so it cannot be run against
the workspace: writing it there first *is* the delivery the evidence was meant
to authorize.

The isolation already existed. `/shell` snapshots the workspace, overlays files
into the copy, runs there, and deletes the copy in a `finally` — the mechanism
V3 build verification has always used. What was missing was **observation**: the
snapshot is destroyed before the response returns, so nothing outside the
executor could say whether a command changed what it was testing.

`observe_paths` on `/shell` adds that. The executor reports facts and draws no
conclusion — it has no way to know what the client declared:

```
target_before / target_after          sha256 per observed path ("" = absent)
workspace_before / workspace_after    sha256 of the whole tree
workspace_files                       count
digest_truncated                      the tree was too large to describe exactly
```

`stageCandidate` (`proxy/candidate_staging.go`) drives it. The sequence is fixed:

1. validate the request, including that the bytes are the candidate the identity
   names;
2. materialize the exact candidate at the canonical target inside the overlay,
   and nowhere else;
3. confirm the staged target's hash equals the candidate hash **before** anything
   runs;
4. run each command through `validateShellCommand` — the same safety check the
   model's own commands go through — and the existing sandbox path, bound to the
   request context for cancellation;
5. re-hash target and workspace after every command;
6. refuse anything that cannot be shown to be about the bytes it claims;
7. the executor destroys the overlay on every path, including a timeout.

### Outcomes

A pass is narrow. Exit zero is not enough:

| Outcome | Meaning |
| --- | --- |
| `exited_zero` | ran, succeeded, changed nothing — **the only authorizing outcome** |
| `exited_nonzero` | ran and failed |
| `timed_out` | did not finish |
| `cancelled` | the request ended |
| `refused` | the safety gate declined it, or it would background work |
| `mutated_target` | it rewrote the candidate it was testing |
| `mutated_workspace` | it changed an input |
| `unobservable` | the executor could not describe the state, or the staged bytes were not the candidate |
| `budget_exceeded` | the set ran out of budget before this command |
| `unavailable` | no executor |

`mutated_target` matters most: a command that rewrote its own subject proved
something about bytes that no longer exist.

### Budget

`TaskContract`'s 64-command maximum is an **input-validation ceiling**, not an
execution budget. Staging has its own, much smaller (`defaultStagingBudget`):

```
MaxCommands            4
PerCommandTimeoutSec   60
TotalTimeoutSec        180
MaxCandidates          3
```

A declared set larger than the budget stages **nothing**. A set that runs out of
budget part way reports `Complete = false`. Neither runs a subset and calls it
finished: a partial set is not a smaller obligation.

## The trust boundary

The Go proxy is the sole authority on whether a command came from a validated
client contract.

- The proxy reads the declared commands out of the validated request, builds the
  staging request itself, and afterwards matches every returned result back
  against the obligations *it* derived. A result naming a command the request
  never declared, an obligation the proxy does not own, another request, or bytes
  other than the candidate matches nothing.
- **The V3 service is not in this path** and still never receives the task
  contract, so a direct caller of a V3 endpoint cannot manufacture
  `client_declared_verification` authority. `V3GenerateRequest` is pinned against
  gaining a contract field.
- **The model running the same command through `run_command` is a different,
  untrusted event.** It writes its own `VerificationRecord`, against the
  production workspace, after bytes have landed. No producer reads it.
- The sandbox executes and reports. Anything it says about authority is ignored,
  because the only things read out of its answer are hashes, an exit status and
  two flags.
- The model cannot add, alter, remove or relabel a declared command. Authority
  comes from the declaration, not from the text.

Client authority to *request* verification is not authority to delete production
files, bypass permissions, reach the network or escape the sandbox. Repository
tests execute only inside the isolated candidate workspace.

## The delivery route

```
V3 selects a winner            the envelope decides which bytes are a candidate
        │
        ▼
final-byte observation         the exact bytes are fixed; both producers speak
        │
        ▼
authorizeCandidateDelivery     the typed decision, and a grant if it authorizes
        │
        ├── contractless ──────► the existing decision delivers, unchanged
        │
        ├── refused ───────────► the caller's own content is restored
        │
        ▼
deliverAuthorizedCandidate     re-check, spend the grant, write, read back
        │
        ▼
post-delivery settlement       existence is discharged, or the run is incomplete
```

The two decisions are a **conjunction, not a race**. The envelope still chooses
which bytes are a candidate at all; the grant decides whether that candidate may
replace the caller's content. A typed refusal keeps the caller's own content and
never falls through to the winner the envelope liked.

`proxy/evidence_inertness_test.go` pins that exactly one function reaches the
decision, exactly one spends the grant, and each is called from exactly one
place.

### One-time grants

A decision is a statement about a moment. A grant is that decision made
spendable, and it is spent exactly once.

It binds request, invocation, candidate instance, candidate hash, canonical
target, workspace generation and state, baseline identity and hash, the target's
own ledger generation and tombstone state, obligation-set identity,
evidence-set identity, the selected candidate, and its own decision generation.
Consumption re-validates every one against a claim built from **live** state and
refuses on the first that differs. It never re-decides: a decision recomputed
under new state is a different decision wearing the old one's identity.

- The check that a grant is live and the mark that it is spent happen under one
  lock. Sixteen racing consumers get one success.
- Alias spellings of one target share one canonical grant.
- A later decision for the same target supersedes the earlier grant.
- Cancellation, terminal emission and session end retire every live grant and
  refuse further minting. Retirement sits at the **emission**, not the verdict:
  `finalizeCompletion` is also reached on the debt-recovery bounce, which keeps
  running.
- Capacity is 64 per request, and overflow refuses before anything is stored.
- Grants hold identities and hashes. The canonical target is one of them; no
  command text, candidate byte, source fragment or prompt is, and the telemetry
  record does not carry the path either.

### The write

Immediately before mutation, and in this order:

1. the bytes must hash to the grant's candidate;
2. the target must be the canonical one it was minted for;
3. workspace and baseline are re-read;
4. the grant is consumed, atomically and once, which re-validates every bound
   identity including the target's ledger generation and tombstone state;
5. the existing write path is handed the **exact** authorized bytes — no
   normalisation, no repair, no appended newline;
6. the existing validation runs on the bytes that landed;
7. what landed is read back and compared with what was authorized.

`ToolResult.AuthorizedDeliveryHash` is set only after step 7 succeeds. It is not
the pool record's `delivered` field, which is the service describing what it
selected before anything reached this filesystem — that is history; this is a
statement about disk.

| Failure | Result |
| --- | --- |
| mismatch before the write | nothing mutated, nothing spent, truthful refusal |
| write failed | no delivery claimed, mutation debt stands |
| validation failed after the write | never reported delivered; an eligible baseline is restored where one structurally exists |

### When the structural gate is down

A sandbox outage makes the gate report `not_run` — not a pass, not a failure,
nothing checked. No syntax evidence exists, so a **structured request refuses
the candidate and keeps the caller's content.** The previous behaviour delivered
the winner on the envelope's word alone.

This is fail-closed by design, and it is a real operational consequence: with the
sandbox down, structured requests stop delivering candidates rather than
delivering unchecked ones. Contractless traffic is unaffected.

## Post-delivery settlement

`artifact_exists` is the one obligation nothing can evidence in advance, which
is why it is settlement rather than a prerequisite. It is discharged only when
all of this holds at once:

- a grant was validly consumed and the exact bytes were confirmed on disk (only
  the delivery owner writes a settlement record, so a successful tool result, a
  selection label, prose or a stale ledger row cannot manufacture one);
- the exact candidate bytes are at the declared target **now**;
- the ledger's current hash equals what is on disk;
- the ledger generation has moved past what it was before this write — the
  ledger observes through its own owner after the tool returns, and settlement
  asks whether that happened rather than assuming it;
- the structural verdict is current for these bytes;
- every declared command the task states was covered by the delivery being
  settled (one that appears afterwards is a stronger obligation than the
  delivery answered);
- the target is neither tombstoned nor moved;
- the baseline the delivery replaced was one it was entitled to replace.

Settlement waives nothing else. Mutation debt, verification debt, background
hazards, deletion rules, action demand and unrelated outputs are owed by their
existing owners. It is scoped to targets this run actually delivered to, so an
output that was never produced stays `missingExpectedOutputs`' question.

The terminal asks one question of one owner, after the owners ahead of it have
spoken, and an unsettled delivery is `post_delivery_settlement_pending`.

## Authorization

Authorization decides whether trusted evidence meets the declared floor. What
happens to a candidate that does not reach it — and which rule owns that answer
— is [CANDIDATE_POLICY.md](CANDIDATE_POLICY.md).

`decideAuthorization` matches evidence against the identity built from the
**live** request — never copied off the record being checked, or every record
would be asked whether it matched itself and every mismatch reason would be
unreachable. The reason vocabulary is closed; the first thing that was wrong is
what gets reported.

`authorized`, `target_not_declared`, `obligation_unknown`, `adapter_unsupported`,
`evidence_missing`, `evidence_too_weak`, `provenance_untrusted`,
`candidate_mismatch`, `request_or_invocation_mismatch`, `workspace_stale`,
`baseline_not_preserved`, `command_mismatch`, `legacy_record`,
`post_delivery_settlement_pending`, `unknown`.

Command identity is **per-obligation**: a syntax record names no command, and a
behavioral one must name the exact command its obligation declared. The expected
value is derived from the client's declaration, never read off the record.

Evidence records the workspace the observation was **bound to**, not the live
one. A producer that re-read the live state would leave every record
self-consistent and staleness undetectable — noticing the two have diverged is
the reader's job.

### Baseline preservation

Preservation is **derived, never produced**: a producer for it would be asserting
a comparison rather than observing a fact. The derivation runs over evidence
already matched to the asked-for identity, so currency, candidate bytes and
invocation are settled before it starts. What is left is strength and witness:

| Baseline | Preserved by |
| --- | --- |
| syntax | current syntax evidence over the exact candidate bytes — the same claim the baseline holds |
| behavioral | the command that **established** it passing again on the candidate |
| oracle | matching oracle evidence |
| none (new file) | nothing is owed |

Weaker never preserves stronger, and the comparison never promotes what it
compares. Syntax over a behavioral baseline stays refused: that is how a working
artifact avoids being replaced by one that merely parses. A different command
exiting zero is behavioral evidence about *something*, and not about what the
baseline showed.

## Feasibility

`decideInvocationFeasibility` asks, before generation, whether this invocation
could close at all. Closed reasons: `closure_path_available`, `no_trusted_source`,
`adapter_cannot_measure`, `unsupported_obligation`, `baseline_floor_unreachable`,
`unspecified_contract`, `unknown`.

It reads `producibleStrengths()`, which is derived from the producer wiring
inventory rather than declared separately — so a producer that loses its call
path makes the tasks that need it infeasible, automatically.

Baseline preservation is reachable exactly when something this build *can*
produce reaches the baseline's strength. `TestTheTwoBaselineDerivationsAgree`
cross-checks that rule against the authorization owner's: a build where one says
unreachable and the other preserves anyway is a build whose pre-generation and
post-generation answers are about different systems.

Feasibility asks whether closure is **possible**, not whether it will happen.
Whether the behavioral evidence turns out to name the command that established
the baseline is a fact about the run, settled at authorization.

A contract that declares nothing is `unspecified_contract`. Staging cannot fix
that: what is missing is the declaration, not the executor.

An invocation's answer is frozen once computed. A later invocation recomputes
from what is available then.

## Where it lives

| File | Owns |
| --- | --- |
| `proxy/obligation_kinds.go` | the six kinds, the three roles, derivation, `baselineWitness` |
| `proxy/syntax_evidence.go` | the proxy-owned syntax producer, `workspaceIdentity` |
| `proxy/verification_evidence.go` | the client-declared verification producer |
| `proxy/staging_contract.go` | the staging request/result types and their fail-closed validation |
| `proxy/candidate_staging.go` | `stageCandidate` — the one thing that executes |
| `proxy/evidence_wiring.go` | the producer inventory and the two production call paths |
| `proxy/authorization_decision.go` | `decideAuthorization`, baseline-preservation derivation |
| `proxy/authorization_grant.go` | the one-time grant: minting, consumption, retirement |
| `proxy/candidate_delivery.go` | the live authorization owner and the grant consumer |
| `proxy/delivery_settlement.go` | post-delivery existence settlement |
| `proxy/feasibility_decision.go` | `decideInvocationFeasibility` (still observe-only) |
| `sandbox/executor_server.py` | `/shell` observation — facts, no conclusions |

## Telemetry

Every decision, observation and grant transition goes to the private shadow sink
and nowhere else: no SSE event, no model-visible text.

`influences_live_decision` is **derived, not asserted**: true for a request with
structured obligations, because there the decision owns delivery, and false for
one without. It said false while nothing read the answer; leaving that in place
once the typed path started deciding would have made every record a lie about
its own weight.

No command text, candidate bytes, source fragments, stdout, stderr, prompts or
credentials enter any log or telemetry surface.
