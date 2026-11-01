# ADR 0011: The lens is required

Status: accepted 2026-09; revised 2026-09-27: the lens has no off switch.
Supersedes [0005](0005-lens-optionality.md). Narrows
[0004](0004-v3-fail-soft.md): a lens that cannot score is no longer a V3
failure answered with the model's own content.

## Context
V3 and the lens have no off switch, and steering is always on wherever a
vector is installed for the served model (`docs/CANDIDATE_POLICY.md`;
`SUPPORT_MATRIX.md`, § Feature paths; [CHANGELOG](../../CHANGELOG.md),
"Removed: `GEOMETRIC_LENS_ENABLED`"). A lens that cannot score and
answers with neutral numbers looks healthy while it adds nothing: V3
would rank its candidates on scores of 0.5, a write would go unscored,
and nothing would tell the user ([CHANGELOG](../../CHANGELOG.md), "Changed: the
lens is required; ATLAS stops and says why when it cannot score").

## Decision
The lens is required. If it cannot score, ATLAS stops and says why.

**Can score.** The lens is reachable, has C(x) and G(x) loaded, passed
its self-test, has not drifted from the served model, and can reach
llama-server (`lensHealthVerdict` in `proxy/lens_required.go`). An
uncalibrated lens can score: it returns raw energies, and only the
calibrated uses (normalized routing, veto and correction thresholds)
wait for calibration, as its status says (`lensVerdictCanScore` in
`proxy/lens.go`).

**An input the lens declines is not a lens that is down.** Failure
kinds `embed_capacity`, `empty_input` and `nonfinite_score` mean this
input is unscored ([ADR 0010](0010-lens-capacity-boundary-is-typed.md)).
The run goes on. An empty answer is `empty_input` only on the per-step
path with a `layer`; elsewhere it is typed `internal`, and the run stops
(ADR 0010). The kinds `model_server_error`,
`model_server_unreachable`, `embedding_contract` and `internal`, a
transport error, a non-200 answer, an unreadable body, and
`enabled: false` mean the lens is down (`lensDownKinds` in
`proxy/lens_required.go`, `scoreContentForAgent` in `proxy/lens.go`,
`LENS_DOWN_KINDS` in `v3-service/scoring.py`).

**No off switch.** The lens ignores `GEOMETRIC_LENS_ENABLED`
(`test_the_lens_has_no_off_switch` in
`geometric-lens/tests/test_nonfinite_scores.py`). Its scoring answers
say `enabled: false` only when no model is loaded
(`geometric-lens/geometric_lens/service.py`), and such a lens reports
`no-artifacts` (`probeLensStatus` in `proxy/lens.go`).

**Before a request.** The proxy asks the lens `/ready`, then `/health`,
because `/ready` answers 200 for a lens with no G(x) model. The answer
is cached for 5 s (`probeLensReadiness` and `lensReadyTTL` in
`proxy/lens_required.go`). If the lens cannot score, the request gets
HTTP 503 with the error code `dependency_unavailable`, the reason, and
"Run `atlas doctor`." Nothing is streamed and no work starts
(`handleAgent` in `proxy/agent.go`; `ErrDependencyDown` in
`proxy/main.go`).

**During a run.** A per-write score that says the lens is down ends
the run before that write: the tool call is answered "not run", the
terminal event is `failed` / `lens_unavailable`, and the summary gives
the reason and says whether earlier changes are on disk (`stopLensDown`
and `lensUnavailableSummary` in `proxy/lens_required.go`).

**Inside V3.** Scoring raises `LensUnavailable`
(`v3-service/scoring.py`). The pipeline stages re-raise it; they do not
log it as a stage failure (`v3-service/pipeline.py`). The service sends
a result with `lens_unavailable` and no code (`_handle_generate` in
`v3-service/main.py`). The proxy bridge turns that into a typed error
(`callV3GenerateStreaming` in `proxy/v3_bridge.go`), and the write and
edit routes do not apply the model's bytes as a fallback
(`writeFileWithV3` in `proxy/tool_write_v3.go`, `deliverEditCandidate` in
`proxy/edit_route_delivery.go`). The run ends as above.

**Status.** The proxy's `/ready` and `/health` apply the same check as
the request path (`lens_ready`, with `lens_reason` when false;
`handleReady` and `handleHealth` in `proxy/main.go`).
`/v1/calibration/status` carries `can_score`, which is true only for
the verdicts `supported` and `uncalibrated`, and the verdicts `drifted`,
`self-test-failed` and `model-server-unreachable` beside the artifact
verdicts (`LensStatus` and `probeLensStatus` in `proxy/lens.go`). The
`direct_agent` dimension is `blocked` while the lens cannot score
(`buildDimensions` in `proxy/lens.go`). `atlas doctor` fails on a
blocked dimension (`check_status_dimensions` in
`atlas/commands/doctor.py`). The TUI badge shows a failure and names the
command to run: `atlas doctor`, or `atlas lens build` when artifacts are
missing (`renderOneBadge` and `badgeActionHint` in `tui/commands.go`).

**Drift.** A drifted lens withdraws its thresholds on every scoring
endpoint, so no veto or correction acts on a drifted model
(`_apply_drift_flags` in `geometric-lens/main.py`). A lens with no G(x)
model returns no thresholds
(`geometric-lens/tests/test_withheld_thresholds.py`).

## Consequences
- A model with no lens artifacts (no C(x) or G(x) bundle) cannot run
  requests until `atlas lens build` or `atlas model install-artifacts`
  gives it one (`lensHealthVerdict` in `proxy/lens_required.go`). The
  support matrix lists which models ship a bundle (`SUPPORT_MATRIX.md`).
- [ADR 0004](0004-v3-fail-soft.md) still governs V3's own failures
  (error, timeout, malformed response): they fall back to the model's
  content, visibly. Whether those should stop the run too is open.
- Drift is detected where the bundle carries a `drift_fingerprint.json`;
  a bundle without one is not checked (`check_fingerprint` in
  `geometric-lens/geometric_lens/drift.py`). `atlas lens build` writes
  one with each bundle it builds, unless a reference text cannot be
  scored (`_write_bundle_fingerprint` in `atlas/commands/lens.py`).
- The benchmark harness (`atlas/bench`) keeps its own lens client with a
  neutral fallback, so it can run for a model that has no lens bundle
  yet, to build one (`NEUTRAL_COMBINED` in `atlas/bench/best_of_k.py`).
  It is not the product path.
- Pinned by `proxy/lens_required_test.go`,
  `tests/v3-service/test_lens_required.py`,
  `geometric-lens/tests/test_withheld_thresholds.py`, and
  `tests/e2e/test_v3_lens_acceptance.py`
  (`test_an_unreachable_lens_refuses_the_request`,
  `test_a_lens_that_v3_cannot_reach_stops_the_run`).
