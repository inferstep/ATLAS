# Candidate delivery: one rule

**ATLAS is an interactive coding agent, not a formal verifier.** For an ordinary
coding task there is no oracle to consult and no proof to be had. What can be
decided honestly is narrower and still useful: whether the candidate V3
selected may replace the model's own proposal, on facts that are typed and
bound to the exact bytes, with the user in the loop through the terminal and
the diff.

## The rule

A V3 candidate replaces the model's bytes when **no hard veto fired** and one
of two bases holds:

| Basis | Holds when | Recorded as |
| --- | --- | --- |
| Declared verification | a verification the client declared passed, at the declared strength, against these exact bytes | `candidate_authorized_strict` |
| V3 selection | the V3 selection path named these exact bytes, and every hard safety requirement below holds | `candidate_automatic_v3` |

Otherwise the model's own bytes land. A declared verification that failed is a
hard veto under either basis.

Nothing selects another rule. `task_contract.candidate_policy` is accepted and
ignored, so a client that still sends it keeps working, and
`ATLAS_CANDIDATE_POLICY` is not read. The model and the V3 service cannot
change the rule either.

## What the V3 selection basis checks

Nothing about the candidate is claimed to be correct, and no score, consensus,
Lens value, service verdict or hidden evaluator is consulted. What is checked
is everything that was never about evidence:

- V3 generation completed, and the candidate is the **exact selected winner**,
  identified by content hash rather than reconstructed from a score or an
  array position;
- request, invocation, route-entry and candidate identities are complete and
  attributable, and the candidate instance id separates duplicate bytes and
  hash-prefix collisions;
- the bytes are non-blank and materially different from the baseline;
- the canonical target is valid, inside the workspace, and grounded: either
  the client declared it as an expected output, or, when the client declared
  no outputs, it is the exact canonical target of the model's own structured
  mutation call (see "The structured mutation target" below);
- the structured mutation scope admits these exact bytes;
- no undeclared path is added or altered, and no protected asset is mutated;
- workspace, target, route, baseline and candidate identity are all fresh;
- language and artifact class match the target;
- applicable syntax and structural checks **ran** and passed;
- **no declared verification failed**, and none was left unobservable;
- the candidate is not weaker than the baseline on an applicable trusted check;
- nothing timed out, was cancelled, or exhausted memory, processes or output;
- a destructive operation still goes through its existing permission flow;
- the one-time exact-byte grant, revalidation, write, ledger, validation,
  settlement and provenance all succeed.

**Absence of an oracle is not failure.** No behavioral oracle, no declared
command, an unsupported adapter, no closure certificate, no independent critic:
each is *unavailable* evidence, recorded as such, and none of them rejects an
otherwise safe selected candidate. Two things are different:

- a requirement the client **did** declare binds. A failed check is a hard
  veto, and one that timed out, was cancelled, exhausted a resource or could
  not be observed cannot authorize a delivery;
- a syntax check that applies to the file but did **not run** (for example,
  the sandbox was down) is a hard veto (`execution_evidence_unavailable`).
  Replacing what the model wrote needs at least that check to have spoken.
  A class with no syntax check (`not_applicable`) is not vetoed on that
  ground.

Declared commands run against the exact candidate bytes in a staging
workspace, for a declared output and for the target the model's own call
named.

**The user's involvement does not change.** The existing permission prompts
still gate dangerous tools, deletion keeps its exact-object approval flow, and
what lands is reviewed as an ordinary workspace diff that can be revised or
undone. There is no candidate approval prompt: the competition between
candidates is internal, and asking a person to adjudicate it would be asking
them to review work they cannot see.

## Which basis holds, decided once

The delivery owner (`authorizeCandidateDelivery`) asks `grantFor` which basis
holds, using the same checks the mint makes: the declared-verification basis
first, then the V3 selection basis. It asks before the acquisition control and
before the mint, so the delivery decision, the capture-only answer and the
mint read one answer. The policy owner reads which basis earned a grant, not
the eligibility check alone.

If a delivery is refused before any byte moves, the model's own bytes are
written, on the new-file route as on the edit route (`writeFileWithV3` in
`proxy/tool_write_v3.go`, and `proxy/edit_route_delivery.go`).

### The structured mutation target

The ordinary interactive request declares no outputs. A person typing into the
TUI has told the client nothing structured about which files the task requires,
and the TUI sends none rather than guess them from the prose.

What such a request does have, once the model acts, is a structured tool call:
`write_file` or one of the edit tools, with a canonical path in its parsed
arguments. That path is the **structured mutation target**. For a `work`
request that declared no outputs, it grounds the delivery of the selected
candidate to that one path. It comes from the parsed call and from nothing
else: not the user's prose, not the model's prose, not a filename in a message,
not a plan or a summary or a Lens output.

It is a narrow thing on purpose:

- it is not an obligation. It never becomes an expected output, never enters
  completion, and never says a file the user asked for exists;
- it authorizes no other path, no additional file, and no deletion, move,
  rename or command; the existing permission flow for dangerous tools is
  untouched and cannot consult it;
- declared outputs are never widened by it: a request that named outputs is
  bound to those outputs on every basis;
- a `question` request can create no mutation authority at all, and a request
  with no contract grounds nothing;
- every hard veto, the one-time grant, the exact-byte comparison, the disk
  re-read and the settlement apply exactly as they do for a declared target.

The grant records which grounding it used (`declared_output` or
`structured_mutation_target`), and structural tests pin which owners read it.

## Proposal is not authorization

The service **proposes**; the proxy **authorizes**.

- `proposedV3Candidate` answers one question: are these bytes materially
  different from the caller's own? It reads no verdict — a structural test
  asserts it touches neither `Passed`, `Evidence`, `ClosureEligible` nor
  `Selection`.
- `closure_eligible`, selector score, consensus, Lens and ASA are V3's own
  **advisory metadata**. They do not authorize, and no field of them mints a
  grant. The selection itself is used only as an identity: which exact bytes
  V3 named.
- The proxy stages the proposal, produces its own evidence about those exact
  bytes, and decides.
- A rejected candidate leaves the model's own proposal exactly as it was.

**There is no service-certification path.** The service's closure verdict
does not deliver a candidate: the producer of a candidate never certifies it.
A request with no contract keeps the model's own bytes.

## When the producer is not consulted at all

Before any of the above runs, both byte-producing routes decide whether to ask
the pipeline for candidates. That decision is a **cost** rule, not a safety
one, and it is made per mutation:

| Reason | Predicate |
| --- | --- |
| `file_tier_below_threshold` | the file classifies below T2 (under 10 lines, or a config/data/style/doc class) |
| `edit_below_complexity_floor` | an edit whose result is under 80 lines with cyclomatic complexity under 8 |
| `producer_not_configured` | the session has no `V3URL` |
| `active_debug_iteration` | the session wrote this file and just watched it fail a run |
| `proposal_failed_syntax_guard` | a syntax or structural guard answered before the producer could be asked |
| `candidate_undeliverable` | no candidate could reach disk: the request is a `question`, or it sent no contract, so no target is grounded |
| `work_budget_allowance` | generation's time cap would leave the session less than the work allowance for everything after this write |
| `internal_unclassified` | fail-closed: a skip nobody taught the vocabulary about |

`writeGenerationBypass` owns the new-file answer and `editGenerationBypass` owns
the answer for the four edit tools; a structural test keeps the conditions out
of the routes themselves. Each skip writes one `candidate_generation_bypass`
capture record carrying the request, the tool, the reason and the predicate
inputs that decided it — path-free and content-free, like every capture record.

A client that sends no contract gets no V3 candidate. The TUI and both IDE
clients send one; absence stays reserved for external and legacy callers,
whose requests keep the model's bytes.

## Structured mutation scope

`write_file`, `edit_file`, `insert_after`, `replace_lines` and
`structural_edit` each name a canonical target and bound a mutation in fields.
`deriveMutationScope` reads that off the call: the tool, the canonical target,
the pre-call bytes and the caller's own result, plus the workspace and target
generations.

What a scope is not is evidence. It says WHERE a candidate may act and nothing
about whether it is any good:

- it cannot expand a path, change a target, authorize a deletion or weaken a
  permission;
- it mints nothing — the grant check refuses without one, which is the only
  direction it acts in, and every other condition still has to hold;
- a candidate outside the boundary its own call defined fires the
  `outside_structured_mutation_scope` veto.

It fails closed on an unknown tool, a path that does not resolve inside the
workspace, a spelling the two resolvers disagree about, a missing identity, a
deletion, a moved target, a moved workspace, and a request that has ended.
Every grant carries the `MutationScopeID` of the call it came from.

Hard proposal requirements survive unchanged, because they are about bytes
being usable rather than proven: materially different, valid identity, correct
file class, no language swap, no edit-boundary violation, nothing malformed,
and no target or workspace mutation during staging.

## How a route entry ends

Separate from the delivery decision, and answered at a different moment: the
decision says what was decided about a candidate, the routing disposition says
how the route entry that carried it ended. `skipped_infeasible`,
`producer_unavailable`, `producer_timed_out`, `cancelled`,
`no_candidate_produced`, `candidate_not_closure_eligible`,
`candidate_revoked_by_gate`, `baseline_retained`, `authorization_refused`,
`candidate_authorized`, and the fail-closed `internal_unclassified`.

`baseline_retained` means the producer offered nothing materially different, so
there was never a candidate; the record names no candidate hash, because the
only bytes in play are the caller's own. A candidate that WAS offered and then
withdrawn by a gate ends as `candidate_revoked_by_gate` and names the hash of
the bytes that were withdrawn.

## Why a candidate did not land

Once per route entry, at route exit, the proxy writes an
`automatic_delivery_attribution` record saying what became of the selected
candidate on that entry. It is derived from the decisions the live owners
already made — the refusal the eligibility owner named, the mint's reason, the
delivery outcome, the disposition the lifecycle recorded — and recomputes
nothing. No owner reads it; with the capture off nothing is built.

The record carries `outcome` (`landed` or `not_landed`) and a `refusal` from a
closed vocabulary (`proxy/automatic_attribution.go`), joined to the other
records by request, route entry, invocation and candidate identity where
present. A landed candidate has an empty refusal. The record keeps its
`policy_mode` and `policy_source` fields, now always `automatic_v3` and
`fixed`, so its readers keep their schema.

| Refusal | Established by |
| --- | --- |
| `route_not_entered` | analysis-side only: the producer was never consulted, so no entry exists; the `candidate_generation_bypass` record names the predicate |
| `no_candidate_produced` | the producer returned nothing, or nothing different from the caller's bytes |
| `v3_unavailable` | the producer was unreachable |
| `v3_timed_out` | the producer did not answer in time |
| `cancelled` | the request ended first (the route or the cancellation veto observed it) |
| `route_gate_revoked` | the route's own gate withdrew the candidate: rewrote beyond the edit, swapped language, did not parse, not closure-eligible |
| `hard_veto` | the veto owner observed a disqualifying fact not named below |
| `no_selection_identity` | the service named no selected candidate |
| `selected_hash_mismatch` | the service selected other bytes than arrived, or a grant was spent on bytes it did not name |
| `candidate_identity_incomplete` | the binding identity could not carry a grant |
| `no_mutation_scope` | the tool call bounded no mutation |
| `target_not_grounded` | no declared output and no structured mutation target |
| `target_mismatch` | the target the grant or delivery was about is not the structured target |
| `scope_expansion` | the candidate left its mutation boundary or reached an unauthorised path |
| `stale_baseline` | the target or workspace moved between decision and delivery |
| `authorization_unavailable` | no closure path, no supporting adapter, an owed prerequisite, an unusable service record |
| `grant_not_minted` | eligible, and the mint still refused for another reason |
| `capture_only_suppressed` | an acquisition control took the licence |
| `delivery_failed` | a grant was spent and the bytes did not land as authorized |
| `unattributed` | the facts fit no reason or contradict each other; an analysis treats this as a contradiction, never a guess |

## The decisions

Closed vocabulary. Every value is a statement about what happened, never about
how likely the candidate is to be correct.

| Decision | Meaning |
| --- | --- |
| `baseline_retained` | the model's own proposal is what lands |
| `candidate_authorized_strict` | a declared verification passed at the declared strength on these exact bytes |
| `candidate_automatic_v3` | the V3 selection path chose these exact bytes and every hard safety requirement held |
| `candidate_rejected_hard_veto` | something disqualifying was observed |

## Hard vetoes

A veto is a fact with an owner outside the model. One is enough, and nothing
outweighs one — a veto outranks even a passed declared verification.

`syntax_or_structural_failure`, `execution_evidence_unavailable`,
`candidate_mutated_protected_assets`, `language_or_target_mismatch`,
`stale_candidate_or_workspace_identity`, `declared_verification_failed`,
`unauthorized_path_expansion`, `weaker_than_baseline_on_a_trusted_check`,
`cancelled_or_timed_out`, `incomplete_evidence`,
`destructive_operation_without_permission`,
`outside_structured_mutation_scope`.

The vetoes are computed once, by the delivery owner, after the authorization
decision and the structural classification they read. The policy owner reads
that list; it does not compute a second one.

## Recorded signals, and their calibration status

Recorded with each decision, never thresholded. Every one is either the same
model grading its own output, a service ranking that output, or a scorer whose
normalisation carries its own calibration flag.

| Signal | Owner | Status |
| --- | --- | --- |
| Lens `gx_score_mean` severe veto (0.52) | geometric-lens | **calibrated**, on 188 live scores — as a *degeneracy veto*, not a correctness predictor |
| Lens `cx_normalized` / `energy_norm` | geometric-lens | carries a `calibrated` flag; uncalibrated when the normalisation artifact is absent. Ranking only |
| `closure_quality_threshold` (1.0) | v3-service | means "every required criterion complete", not "likely correct" |
| CodeT consensus / cluster agreement | v3-service | same model on both sides, so agreement is not independence |
| Best-of-K margin | v3-service | never measured against outcomes |
| proxy gate pass | proxy | trusted, and a syntax fact rather than a quality one |

**No defensible correctness threshold exists yet.** No signal decides delivery,
and none may be described as a probability of correctness until a calibration
against held-out outcomes supports it. The records are what such a calibration
would be computed from.

## Product decisions

- V3 is always on, and one rule decides what lands. There are no
  delivery modes and no switch that turns the pipeline off.
- Destructive operations keep their explicit permission flow. A candidate's
  delivery is not a permission.
- Human review of the final diff remains part of the product. ATLAS does not
  claim a universal correctness oracle.

## The acquisition control

An outcome-blind acquisition — an eligibility pilot, a calibration run — has one
invariant that outranks what it is measuring: **candidate bytes must never enter
the active task workspace.** A delivered candidate changes what the model sees
next, how many routes the task takes, which terminal it reaches and what
evidence exists at the end.

`ATLAS_CANDIDATE_CAPTURE_ONLY` is that control. It is a research tool, not a
product setting: operator configuration on a private experimental process,
default off, unreachable from any task contract, model output, service response
or header, failing closed to ordinary behaviour on any value it does not
recognise, and never used to measure the product.

It sits at the one place a candidate grant is created — inside
`authorizeCandidateDelivery`, after the basis check and immediately before
`mintAuthorizationGrant` — and a structural test pins that there is exactly one
minting caller and that the control is consulted before it.

What it suppresses is the licence, not the answer:

- the rule runs, the declared commands run against the exact staged candidate
  bytes, and the hard vetoes fire, all unchanged;
- no grant is minted, none is consumed, and the baseline stays on disk;
- the decision is recorded as what it was — `candidate_authorized_strict` stays
  `candidate_authorized_strict` rather than being flattened into
  `baseline_retained`;
- two private records carry it: the suppression, and the would-have disposition
  from the closed set `would_authorize_strict`, `would_deliver_automatic_v3`,
  `rejected_hard_veto`, `baseline_retained`,
  `capture_only_suppressed_delivery`;
- nothing model-facing mentions it, and no extra model turn results.

## What the user sees

Delivered bytes name their origin, from a closed vocabulary the terminal can
render: `model_proposal`, `strict_trusted_candidate` (declared verification
passed), `automatic_v3_candidate` (V3 selection). Only a decision that actually
delivers may claim a candidate origin; everything else is the model's own work
and says so.

The provenance is a server-side fact. It is not part of `modelFacingResult`: the
user needs to know what they are reading, and the model does not get to argue
with the answer. No internal confidence vocabulary is presented as a
correctness guarantee, because none of it is one.

## Obligations, evidence and staging

**This decides whether a generated candidate lands.** It owns the answer for a
request that declares its outputs, and for a `work` request whose model call
names a structured mutation target
([the structured mutation target](#the-structured-mutation-target));
see `authorizeCandidateDelivery` in `proxy/candidate_delivery.go`. A request
that sends no contract, or a `question`, gets no V3 candidate
(`candidate_undeliverable` in `proxy/candidate_reachability.go`), except under
the operator's capture-only acquisition, which generates one and never delivers
it ([the acquisition control](#the-acquisition-control)).

`proxy/delivery_agreement_test.go` drives each shape a request takes through the
real write path. It checks that a candidate lands exactly when this decision
says it delivers, with the bytes the decision named, and never for a request
the decision does not own.

Feasibility is observe-only: `observeInvocationFeasibility` runs before
generation and records its answer to private telemetry, and generation goes
ahead whatever it says (`proxy/feasibility_decision.go`).

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

## The evidence wire

The versioned envelope between v3-service and the proxy.

The V3 service answers `/v3/generate` with a `passed` boolean, a phase name and
a score. None of those says what was actually demonstrated: `passed` covers a
compile smoke, a partial oracle score and a complete one indistinguishably, so
the proxy cannot tell them apart from those fields. The envelope carries the
evidence itself, versioned, so a consumer can read what happened instead of
guessing from a boolean.

The envelope decides which bytes are a **candidate**. A generated candidate may
replace the caller's content, and carry V3 provenance, only when the envelope
is available and self-consistent, its selection concluded a `verified_winner`,
its record is closure-eligible, and its hash names the exact bytes Go would
write.

That is necessary, not sufficient: the typed authorization path also has to
grant the candidate, and a refusal there keeps the caller's own content (see
[Authorization](#authorization)). A request that sends
no contract, or a `question`, gets no V3 candidate to deliver
(`candidateDeliverable` in `proxy/candidate_reachability.go`). `passed`,
`phase_solved`, `winning_score` and the verification-evidence strings authorize
nothing; `passed` stays on the wire as a compatibility and telemetry field.

Because sanitisation rewrites a candidate after the service earned its
evidence, authorization is re-asked of the final bytes: a hash that no longer
matches revokes to the caller's baseline and withdraws provenance, and the
revocation continues through the same final-byte validation the other gates
use.

### Ownership

| Layer | Owns | Must not |
| --- | --- | --- |
| `v3-service/contract.py` | The domain: vocabulary, coverage arithmetic, comparability, ranking, closure policy, and the wire envelope that renders them | — |
| `v3-service/pipeline.py` | Orchestration: runs verifiers, supplies the selected record | Decide wire shape |
| `v3-service/adapters.py` | Adapter knowledge: which criteria an adapter can observe, what its grading means in contract terms, and the live-record → contract-record bridge | Decide closure, ranking or selection |
| `v3-service/main.py` | Transport: calls `adapters.evidence_envelope`, writes its output | Hold any policy — pinned by `test_main_serialises_but_decides_nothing` |
| `proxy/types.go` | The one Go wire representation, beside `V3GenerateResponse` | — |
| `proxy/v3_bridge.go` | The response boundary: decode plus strict validation and availability | Re-derive domain policy, or infer strength from `passed`/`phase_solved`/`winning_score`/`verification_evidence` |

No separate evidence module exists on either side, and no compatibility module
stands in for one (`test_the_retiring_prototype_is_gone`). One contract, one
serialiser, one wire type, one validator — pinned by
`test_one_canonical_contract_one_serialiser_no_duplicate_policy` and
`test_no_compatibility_aliases_or_duplicate_implementations_remain`. The three
tests are in `tests/v3-service/pipeline/test_contract_genericity.py`.

Local `ToolResult` validation (syntax, structural) stays completely separate:
it says what **the proxy** checked about the bytes it wrote. The envelope says
what **the service** demonstrated about a candidate. Merging them would let a
local syntax pass read as behavioural evidence.

### Envelope

```
wire_version            transport shape (major-compatible; unknown major = unavailable)
record_schema_version   contract.SCHEMA_VERSION, versioned independently
identity                contract id+version, adapter id+version or calibration id,
                        artifact scope, evaluation-context hash, candidate-content hash
evaluation              execution status, supported, evidence strength,
                        requirements complete, closure eligible, quality scores
coverage                required / demonstrated / missing / unmeasurable, optional observations
selection               status, reason, tied / incomparable / ineligible counts
delivery                delivered-content hash, describes_delivered_candidate
```

Criterion ids are opaque strings end to end. No task vocabulary appears in the
schema or in the policy; `test_generic_contract_stays_prompt_agnostic` enforces it.

Candidate evidence (`evaluation`, `coverage`) and selection evidence
(`selection`) are separate objects, because collapsing them is how "the best of
a bad pool" becomes "verified". A best record that is not closure-eligible is
`selection.status = best_not_closure_eligible` with
`evaluation.closure_eligible = false` — and `passed` is untouched.

### Absent, unavailable, available

Three states, never two:

- **absent** — no envelope was sent: a legacy service, or a run that measured
  nothing. `evidence` is omitted entirely.
- **unavailable** — an envelope arrived and cannot be trusted: unknown wire
  major, incomplete identity, unknown enum value, or an internal contradiction
  (closure claimed over a non-`ok` execution, over incomplete requirements, for
  an unsupported artifact, or a verified winner without closure eligibility).
  Never "failed": nothing about the candidate was demonstrated either way.
- **available** — structurally valid and internally consistent. Says nothing
  yet about whether it describes the bytes being delivered.

The service never emits a malformed envelope: a record that cannot be
serialised is sent as no evidence plus `evidence_unavailable_reason`. The Go
validation exists for buggy or future producers, and the golden fixtures
include three damaged envelopes so that path is exercised.

### Hashes before provenance

`EvidenceSupportsProvenanceFor` is the rule, and `v3DeliveryAuthorized` is the
only place it is applied:

| Condition | Why |
| --- | --- |
| non-empty candidate code | nothing to deliver otherwise |
| availability `available` | present, same-major, self-consistent |
| `selection.status == verified_winner` | a winner, not merely a best record |
| `evaluation.closure_eligible` | that winner met its own contract's floor |
| `candidate_content_hash` equals sha256 of the final bytes | the evidence is about what will be written |

The producer's own `describes_delivered_candidate` flag is never trusted — the
consumer hashes what it is delivering. `delivered` in a pool record is likewise
the service describing what it selected, written before anything reached this
filesystem: history, not a statement about disk. The live result is
`ToolResult.AuthorizedDeliveryHash`, set only after a re-read of the target
matched the bytes a one-time authorization was spent on. Every other outcome (best-not-eligible,
tied, incomparable, ineligible, no winner, hash mismatch, absent, unknown
version, malformed, contradictory) delivers the caller's baseline with no
provenance.

### Golden fixtures

`v3-service/testdata/evidence_wire_cases.json` is one document holding all 13
cases: id, description, the exact response body written by the real serialiser,
and what both sides must conclude (availability, strength, selection status,
whether the evidence describes the delivered bytes, and the reason an
unavailable envelope must give). It is built and verified byte-for-byte by
`tests/v3-service/pipeline/test_contract_genericity.py`; regenerate with
`ATLAS_WRITE_EVIDENCE_FIXTURES=1 pytest tests/v3-service/pipeline/test_contract_genericity.py`.
The Go tests (`proxy/contract_gate_test.go`) read the same document, decode
those exact bytes and check the declared expectations independently — so the
two languages agree with the contract rather than with each other, and adding a
case on the Python side automatically binds the Go side to it.

### What selection compares

The candidate pool holds the artifacts V3 generated **and** the one it would
replace (#259).

`baseline_code` (the caller's own content, the incumbent) has two roles:
- It is prose in the problem statement: the "Reference implementation" the
  generated candidates work from.
- It is a candidate. Its exact bytes get the same adapter, contract record,
  sandbox verification, vetoes and lens score as a generated one, and they
  enter `passing` and the selection pool at index -1.

The lens ranks the incumbent with the rest:
- A generated candidate replaces it only when the candidate passes the same
  checks and the lens ranks it higher.
- A candidate that differs from it only in whitespace never replaces it.
- When the incumbent is selected, the service returns its exact bytes with
  `phase_solved: "incumbent"`. The proxy reads that as no proposal and
  writes the bytes as the model's own.

Every replacement must also keep the incumbent's role: each top-level def,
class and assignment name it binds, and each name it imports from another
project file. A candidate that drops one fails verification with evidence
`verifier: incumbent_role`, and it is left out of consensus.

Three names, three different artifacts:

| name | what it is |
| --- | --- |
| incumbent | the caller's bytes, `baseline_code`; pool index -1, never generated and never counted in `k` |
| phase-zero probe candidate | pool index 0, generated in phase 0 |
| generated alternatives | PlanSearch / DivSampling members |

So `best_not_closure_eligible` means "best in the pool, the incumbent
included".

### Benchmark-only pool capture

Verification hands the pipeline one bit. A candidate that passed 9 of 10
generated cases and one that passed none produce the same contract record,
and the rejected candidates' bytes are gone when the run returns — so a run
where every suite scored 0/N cannot be attributed to the candidates or to
the answer key from what was retained.

`ATLAS_V3_CAPTURE_POOL=<absolute path>` turns on an append-only JSONL sink
in `pipeline.py` holding `candidate_evaluation` (exact candidate bytes,
hash, length, role including candidate zero, contract record, and every
generated case with its input, generated expected output, observed actual
and a classified outcome), one `selection_summary` and one `capture_status`
per run. Unset is fully inert: no path is opened and no record is built.

It is an instrument, not a feature. It decides nothing, its output is
untracked, and every failure mode — a relative path, a missing parent, a
symlink at the final component, a full disk, the byte cap — disables
capture rather than changing the run. Candidate source reaches this file
and no other surface; `test_candidate_bytes_reach_no_serialiser_or_emitter`
pins that. The sink names the bytes the **service returned**, never the
delivered artifact: only Go knows what was written, so the delivered hash
is joined offline from the runner's authorization telemetry.

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

`proxy/evidence_wiring_test.go` pins that exactly one function reaches the
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
the candidate and keeps the caller's content.**

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
— is [the rule](#the-rule) above.

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

It reads `producibleStrengthsWith()`, which is derived from the producer wiring
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

| File | What it owns |
| --- | --- |
| `proxy/candidate_policy.go` | the rule, the decision vocabulary, the telemetry record, and what the terminal is told about delivered bytes |
| `proxy/advisory_policy.go` | the veto vocabulary, the signal set, and the policy owner (`decideCandidatePolicy`) |
| `proxy/candidate_delivery.go` | which basis holds, the vetoes, and the one delivery owner; the live authorization owner and the grant consumer |
| `proxy/authorization_grant.go` | the grant check (`grantFor`) and the one mint; the one-time grant: minting, consumption, retirement |
| `proxy/automatic_delivery.go` | whether the exact selected candidate may land, and the grant basis |
| `proxy/verification_requirements.go` | typed verification requirements and asset authority |
| `proxy/evidence_wiring.go` | the staged declared-verification producer; the producer inventory and the two production call paths |
| `proxy/tool_write_v3.go` | the new-file route: proposal, staging, policy, delivery |
| `proxy/edit_route_delivery.go` | the edit route, through the same owners |
| `proxy/candidate_reachability.go` | whether the producer is consulted at all, and why not |
| `proxy/obligation_kinds.go` | the six kinds, the three roles, derivation, `baselineWitness` |
| `proxy/syntax_evidence.go` | the proxy-owned syntax producer, `workspaceIdentity` |
| `proxy/verification_evidence.go` | the client-declared verification producer |
| `proxy/staging_contract.go` | the staging request/result types and their fail-closed validation |
| `proxy/candidate_staging.go` | `stageCandidate` — the one thing that executes |
| `proxy/authorization_decision.go` | `decideAuthorization`, baseline-preservation derivation |
| `proxy/delivery_settlement.go` | post-delivery existence settlement |
| `proxy/feasibility_decision.go` | `decideInvocationFeasibility` (observe-only) |
| `sandbox/executor_server.py` | `/shell` observation — facts, no conclusions |

## Telemetry

Every decision, observation and grant transition goes to the private shadow sink
and nowhere else: no SSE event, no model-visible text.

`influences_live_decision` is **derived, not asserted**: true for a request with
structured obligations, because there the decision owns delivery, and false for
one without.

No command text, candidate bytes, source fragments, stdout, stderr, prompts or
credentials enter any log or telemetry surface.
