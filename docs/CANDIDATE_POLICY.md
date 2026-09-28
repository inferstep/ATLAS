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
ignored, so older clients keep working. `ATLAS_CANDIDATE_POLICY` is no longer
read. The model and the V3 service cannot change the rule either.

### Why there is one rule

Until 2026-09-26 there were three modes (`strict`, `advisory`, `automatic_v3`),
chosen per request or by an operator default. The default was `strict`, and
under `strict` a request that declared no outputs never generated a candidate.
The ordinary interactive request declares no outputs, so V3 ran only for
clients that opted in, and the configuration that was measured was not the
configuration that shipped. `advisory` computed an answer and delivered
nothing. The modes are removed: V3 is always on, and this one rule decides
what lands.

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
named. Before 2026-09-26 they ran only for declared outputs, so a request that
declared commands and no outputs could never have its candidate verified.

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

This fixed a defect found while removing the modes: the eligibility check
allowed a candidate, the decision said "delivers", the mint then refused, and
the write route wrote nothing at all while telling the model its content was
kept. Now, if a delivery is refused before any byte moves, the model's own
bytes are written, as on the edit route.

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

**There is no service-certification path.** A request that declared no output
knowledge once delivered on the service's own closure verdict — the producer of
a candidate certifying that candidate. It is gone. A request with no contract
keeps the model's own bytes.

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

A client that sends no contract gets no V3 candidate. The TUI sends one; the
VS Code extension does not yet, so its requests keep the model's bytes until
it does.

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

- V3 is always on, and one rule decides what lands (2026-09-26). There are no
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

## Where it lives

| File | What it owns |
| --- | --- |
| `proxy/candidate_policy.go` | the rule, the decision vocabulary, the telemetry record |
| `proxy/advisory_policy.go` | the veto vocabulary, the signal set, and the policy owner (`decideCandidatePolicy`) |
| `proxy/candidate_delivery.go` | which basis holds, the vetoes, and the one delivery owner |
| `proxy/authorization_grant.go` | the grant check (`grantFor`) and the one mint |
| `proxy/automatic_delivery.go` | whether the exact selected candidate may land, and the grant basis |
| `proxy/candidate_provenance.go` | what the terminal is told about delivered bytes |
| `proxy/verification_requirements.go` | typed verification requirements and asset authority |
| `proxy/evidence_wiring.go` | the staged declared-verification producer |
| `proxy/tools.go` | the new-file route: proposal, staging, policy, delivery |
| `proxy/edit_route_delivery.go` | the edit route, through the same owners |
| `proxy/candidate_reachability.go` | whether the producer is consulted at all, and why not |
