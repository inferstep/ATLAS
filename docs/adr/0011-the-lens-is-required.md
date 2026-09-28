# ADR 0011: The lens is required

Status: accepted 2026-09. Supersedes 0005. Narrows 0004: a lens that
cannot score is no longer a V3 failure answered with the model's own
content.

## Context
ADR 0005 made the Geometric Lens optional. A lens that was switched
off, had no model loaded, or could not reach llama-server degraded to
"no signal":

- a write the agent loop scores was written unscored, and nothing told
  the user;
- V3 ranked its candidates on neutral scores (C(x) 0.5, G(x) 0.5), so
  the lens's share of selection and allocation was gone without a
  trace in the result;
- a lens with no G(x) model returned its calibrated thresholds beside
  0.5 placeholder scores, and `severe_mean` 0.52 read every candidate
  as severe.

The product is designed and measured with V3 and the lens on
(steering always on, no toggles). A stack that runs without the lens
is a configuration nobody measured, and it looked healthy.

## Decision
The lens is required. If it cannot score, ATLAS stops and says why.

**Can score.** The lens is reachable, switched on, has C(x) and G(x)
loaded, passed its self-test, has not drifted from the served model,
and can reach llama-server. An uncalibrated lens can score: it returns
raw energies, and only the calibrated uses (normalized routing, veto
and correction thresholds) wait for calibration, as its status says.

**An input the lens declines is not a lens that is down.** Failure
kinds `embed_capacity`, `empty_input` and `nonfinite_score` mean this
input is unscored (ADR 0010). The run goes on. The kinds
`model_server_error`, `model_server_unreachable`, `embedding_contract`
and `internal`, a transport error, a non-200 answer, an unreadable
body, and `enabled: false` mean the lens is down.

**Before a request.** The proxy asks the lens `/ready`, then `/health`
(`/ready` answers 200 for a lens that is switched off or has no G(x)
model). The answer is cached for 5 s. If the lens cannot score, the
request gets HTTP 503 `dependency_down` with the reason and "Run
`atlas doctor`." Nothing is streamed and no work starts.

**During a run.** A per-write score that says the lens is down ends
the run before that write: the tool call is answered "not run", the
terminal event is `failed` / `lens_unavailable`, and the summary gives
the reason and says whether earlier changes are on disk.

**Inside V3.** Scoring raises `LensUnavailable`. The pipeline stages
re-raise it; they do not log it as a stage failure. The service sends
a result with `lens_unavailable` and no code. The proxy bridge turns
that into a typed error, and the write and edit routes do not apply
the model's bytes as a fallback. The run ends as above.

**Status.** The proxy's `/ready` and `/health` apply the same check
as the request path (`lens_ready`, with `lens_reason` when false).
`/v1/calibration/status` carries `can_score` and the verdicts
`disabled`, `drifted`, `self-test-failed` and
`model-server-unreachable`. The `direct_agent` dimension is `blocked`
while the lens cannot score. `atlas doctor` fails on a blocked
dimension. The TUI badge shows a failure and names the command to run
(`atlas doctor`, or `atlas lens build` when artifacts are missing).

**Drift.** A drifted lens withdraws its thresholds on every scoring
endpoint, so no veto or correction acts on a drifted model. A lens
with no G(x) model returns no thresholds.

## Consequences
- A model with no lens artifacts (no C(x) or G(x) bundle) cannot run
  requests until `atlas lens build` or `atlas model install-artifacts`
  gives it one. The support matrix lists which models ship a bundle.
- `GEOMETRIC_LENS_ENABLED=false` now refuses every request. The
  variable still exists; removing it is follow-up work.
- ADR 0004 still governs V3's own failures (error, timeout, malformed
  response): they fall back to the model's content, visibly. Whether
  those should stop the run too is open.
- Drift is detected only where a `drift_fingerprint.json` exists. The
  stop path is wired and tested; nothing writes the fingerprint yet.
- The benchmark harness (`atlas/bench`) keeps its own lens client with
  the old neutral fallback. It is not the product path.
- Pinned by `proxy/lens_required_test.go`,
  `tests/v3-service/test_lens_required.py`,
  `geometric-lens/tests/test_withheld_thresholds.py`, and
  `tests/e2e/test_v3_lens_acceptance.py`
  (`test_an_unreachable_lens_refuses_the_request`,
  `test_a_lens_that_v3_cannot_reach_stops_the_run`).

## Revision 2026-09-27
`GEOMETRIC_LENS_ENABLED` is removed: the lens has no off switch, as
nothing else in ATLAS does. The `disabled` verdict is gone. A lens
with no model loaded reports `no-artifacts`, and its scoring answers
say `enabled: false`, which the proxy and V3 read as a lens that
cannot score.
