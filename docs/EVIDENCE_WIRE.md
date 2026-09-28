# Evidence wire: the versioned envelope between v3-service and the proxy

The V3 service answers `/v3/generate` with a `passed` boolean, a phase name and
a score. None of those says what was actually demonstrated: `passed` covers a
compile smoke, a partial oracle score and a complete one indistinguishably, and
the proxy had no way to tell them apart. The envelope carries the evidence
itself, versioned, so a consumer can read what happened instead of guessing
from a boolean.

The envelope decides which bytes are a **candidate**. A generated candidate may
replace the caller's content, and carry V3 provenance, only when the envelope
is available and self-consistent, its selection concluded a `verified_winner`,
its record is closure-eligible, and its hash names the exact bytes Go would
write.

For a request that declares structured obligations that is necessary and no
longer sufficient: the typed authorization path also has to grant it, and a
refusal there keeps the caller's own content. See
[CANDIDATE_AUTHORIZATION.md](CANDIDATE_AUTHORIZATION.md). For a request that
declares none, what follows is the whole rule and is unchanged. `passed`, `phase_solved`, `winning_score` and the verification-evidence
strings authorize nothing; `passed` stays on the wire as a compatibility and
telemetry field.

Because sanitisation rewrites a candidate after the service earned its
evidence, authorization is re-asked of the final bytes: a hash that no longer
matches revokes to the caller's baseline and withdraws provenance, and the
revocation continues through the same final-byte validation the other gates
use.

## Ownership

| Layer | Owns | Must not |
| --- | --- | --- |
| `v3-service/contract.py` | The domain: vocabulary, coverage arithmetic, comparability, ranking, closure policy, and the wire envelope that renders them | — |
| `v3-service/pipeline.py` | Orchestration: runs verifiers, supplies the selected record | Decide wire shape |
| `v3-service/adapters.py` | Adapter knowledge: which criteria an adapter can observe, what its grading means in contract terms, and the live-record → contract-record bridge | Decide closure, ranking or selection |
| `v3-service/main.py` | Transport: calls `adapters.evidence_envelope`, writes its output | Hold any policy — pinned by `test_main_serialises_but_decides_nothing` |
| `v3-service/evidence.py` | Compatibility only, pending retirement | Gain any new behaviour — pinned by `test_no_new_behaviour_was_added_to_the_retiring_prototype` |
| `proxy/types.go` | The one Go wire representation, beside `V3GenerateResponse` | — |
| `proxy/v3_bridge.go` | The response boundary: decode plus strict validation and availability | Re-derive domain policy, or infer strength from `passed`/`phase_solved`/`winning_score`/`verification_evidence` |

No separate evidence module exists on either side. One contract, one serialiser,
one wire type, one validator — pinned by
`test_one_canonical_contract_one_serialiser_no_duplicate_policy`.

Local `ToolResult` validation (syntax, structural) stays completely separate:
it says what **the proxy** checked about the bytes it wrote. The envelope says
what **the service** demonstrated about a candidate. Merging them would let a
local syntax pass read as behavioural evidence.

## Envelope

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

## Absent, unavailable, available

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

## Hashes before provenance

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

## Golden fixtures

`v3-service/testdata/evidence_wire_cases.json` is one document holding all 13
cases: id, description, the exact response body written by the real serialiser,
and what both sides must conclude (availability, strength, selection status,
whether the evidence describes the delivered bytes, and the reason an
unavailable envelope must give). It is built and verified byte-for-byte by
`tests/v3-service/test_contract_genericity.py`; regenerate with
`ATLAS_WRITE_EVIDENCE_FIXTURES=1 pytest tests/v3-service/test_contract_genericity.py`.
The Go tests (`proxy/contract_gate_test.go`) read the same document, decode
those exact bytes and check the declared expectations independently — so the
two languages agree with the contract rather than with each other, and adding a
case on the Python side automatically binds the Go side to it.

## What selection currently compares

The candidate pool holds artifacts **V3 generated**. It does not hold the
one it would replace.

`baseline_code` — the caller's own content, the incumbent — reaches the
service and is appended to the problem statement as a "Reference
implementation" under an instruction to improve on it. That is its only
use. It receives no adapter, no contract record, no sandbox execution, no
consensus probe, no lens score, no place in `passing`, and no place in the
selection pool. Two comments still call it "candidate #0"
(`proxy/tools.go`, `v3-service/main.py`); the pool's index 0 is the
**phase-zero probe candidate**, a fresh generation from phase 0, not the
incumbent.

Three names, three different artifacts:

| name | what it is |
| --- | --- |
| incumbent baseline | the caller's bytes, `baseline_code`; never a pool member |
| phase-zero probe candidate | pool index 0, generated in phase 0 |
| generated alternatives | PlanSearch / DivSampling members |

So **`best_not_closure_eligible` means "best among the V3-generated
candidates"** — nothing more. It carries no claim that the winner beats the
incumbent, because the incumbent was never measured. The tests that assert
"candidate zero remains selectable" pin the phase-zero probe candidate's
place in the generated pool; none of them compares the incumbent, and
`test_candidate_zero_is_preserved_and_can_win` ranks two synthetic records
rather than a real baseline.

A replacement-eligibility policy therefore cannot be built on today's
selection: it would need the incumbent to carry a comparable record, or an
equally rigorous comparison outside the pipeline.

## Benchmark-only pool capture

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

## `evidence.py`: retired

The prototype that predated `contract.py` is **deleted**. Every symbol moved to
the layer that owns it, with no compatibility module, alias or shim left behind:

| Was in `evidence.py` | Now |
| --- | --- |
| `select_adapter`, adapter id constants | `adapters.py` — adapter routing (`adapters.ADAPTER_*`) |
| `js_is_instrumentable`, `extract_inline_script`, `js_probe_source_inline`, `js_probe_source`, `parse_probe_output`, `combine_runs`, the JS harness and its regexes, `INTERACTIVE_REQUIRED` / `INTERACTIVE_OPTIONAL` | **deleted** — the browser probe was a verifier for one artifact class with criteria named for one game; removed with the evidence modes that gated it |
| `result`, `result_from_adapter`, `grade_interactive` | `adapters.contract_record`, which builds contract records from raw observations |
| `selection_mode`, `probing_enabled`, `selection_enabled`, `OFF`/`SHADOW`/`ENFORCE` | **deleted** — there is one mode: the adapter's own record decides closure, `contract.select` fills the envelope's selection, the lens chooses the delivered bytes |
| `may_return_early`, `may_return_early_result`, `at_least`, `rank_key`, `STRENGTH_ORDER` and the prototype strength scale | **deleted** — superseded by `contract.select`, `contract.rank_key` and the contract's own strength ordering |

Sentinels in `tests/v3-service/test_contract_genericity.py` prove the file is
gone, that no Python file imports it, that each moved symbol has exactly one
definition in exactly one owner, and that the superseded policy has no
definition anywhere. `tests/v3-service/test_capability_matrix.py` proves that
no browser-probe or game-shaped criterion vocabulary survives in the service.
