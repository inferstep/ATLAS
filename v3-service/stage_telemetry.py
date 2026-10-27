"""Stage telemetry of the V3 pipeline: the table of which stage closes which
phase, the fold of a run's events into the per-task summary rows, the
telemetry directory, and the run's emit(), which records its progress
events and streams them."""

import os
import time
from pathlib import Path
from typing import Any, Dict, List, Optional

_TELEMETRY_DISABLE_VALUES = {"0", "off", "none", "disabled", "false"}

# stage name -> summary phase. Stages not listed (token/llm_*/task_type/…)
# don't contribute a phase row.
_STAGE_PHASE = {}
for _phase, _stages in {
    "probe": ("probe", "probe_light", "probe_retry", "probe_failed",
              "probe_error", "probe_scored", "probe_sandbox", "probe_pass"),
    "self_test": ("self_test_gen", "self_test_done", "self_test_error",
                  "self_test_skip", "self_test_untrusted"),
    "allocation": ("phase2", "phase2_allocated", "diagnostic_allocation"),
    "generation": ("phase1", "plansearch", "plansearch_done",
                   "plansearch_error", "divsampling", "divsampling_done",
                   "divsampling_error", "divsampling_stop", "lens_per_step",
                   "lens_unscored"),
    "sandbox": ("sandbox_test", "sandbox_pass", "sandbox_fail",
                "sandbox_done", "smoke_check", "interactive_lint",
                "self_test_verify", "build_verify",
                "build_verify_unavailable", "runtime_compare"),
    "veto": ("lens_veto", "structural_veto", "call_graph_veto"),
    "selection": ("selected", "consensus"),
    "repair_pr_cot": ("phase3", "call_chain_context", "pr_cot",
                      "pr_cot_pass", "pr_cot_failed", "pr_cot_error"),
    "repair_refinement": ("refinement", "refinement_pass",
                          "refinement_failed", "refinement_error",
                          "refinement_verify_failed", "refinement_skip"),
    "fallback": ("fallback", "fallback_all_vetoed", "fallback_unverified",
                 "budget_exhausted", "budget_no_verified_candidate"),
}.items():
    for _s in _stages:
        _STAGE_PHASE[_s] = _phase


def _resolve_telemetry_dir() -> Optional[Path]:
    """Resolve the stage-telemetry directory for the live service.

    ``ATLAS_V3_TELEMETRY_DIR`` names the directory; a disable value
    (``0``/``off``/``none``/``disabled``/``false``) turns telemetry off;
    unset/empty falls back to ``/data/telemetry`` when writable (the
    compose volume), else telemetry is disabled. Never raises: telemetry must not break generation.
    """
    configured = os.environ.get("ATLAS_V3_TELEMETRY_DIR", "").strip()
    if configured.lower() in _TELEMETRY_DISABLE_VALUES:
        return None
    candidate = Path(configured) if configured else Path("/data/telemetry")
    try:
        candidate.mkdir(parents=True, exist_ok=True)
        probe = candidate / ".write_probe"
        probe.touch()
        probe.unlink()
        return candidate
    except OSError as e:
        if configured:
            print(f"  [telemetry] {candidate} not writable ({e}) — stage telemetry disabled. To keep its data and write it "
                  f"again, run once on the host: docker compose run --rm --no-deps --user root v3-service chown -R "
                  f"{os.getuid()}:{os.getgid()} {candidate}", flush=True)
        return None


def _summarize_phases(events: List[Dict[str, Any]]) -> List[Dict[str, Any]]:
    """Fold the run's progress events into ordered per-phase rows.

    Each row carries the phase name, the stage that closed it (its
    outcome marker), that stage's detail, and the span between the
    phase's first and last event. Derived purely from the events the
    run already emits — no extra instrumentation in the hot path.
    """
    rows: List[Dict[str, Any]] = []
    by_phase: Dict[str, Dict[str, Any]] = {}
    for ev in events:
        phase = _STAGE_PHASE.get(ev.get("stage", ""))
        if phase is None:
            continue
        row = by_phase.get(phase)
        if row is None:
            row = {"phase": phase, "first_ms": round(ev.get("t", 0.0) * 1000)}
            by_phase[phase] = row
            rows.append(row)
        row["outcome"] = ev.get("stage", "")
        row["detail"] = str(ev.get("detail", ""))[:120]
        row["duration_ms"] = round(ev.get("t", 0.0) * 1000) - row["first_ms"]
    return rows


def _progress_emitter(events, start, progress_callback):
    """The run's emit(stage, detail, **data). It keeps every event but a
    token in `events` and streams each one to the progress callback."""
    def emit(stage, detail="", **data):
        ev = {"stage": stage, "detail": detail, "t": time.time() - start}
        if data:
            ev["data"] = data
        # Token deltas stream live through the callback but are not
        # stored: one dict per token would make the final `event: result`
        # frame multi-MB on long generations.
        if stage != "token":
            events.append(ev)
        if progress_callback:
            try:
                progress_callback(stage, detail, **data)
            except TypeError:
                progress_callback(stage, detail)
    return emit
