#!/usr/bin/env python3
"""Derive ablation conditions B-E from a Condition F (full pipeline) run.

Since conditions B-F all generate the same candidates (same model, seeds,
k=3 from PlanSearch), the only differences are:
  - Selection strategy: random (B) vs lens (C, D, E, F)
  - Phase 2 S* tiebreaking: ON in D, F; OFF in B, C, E
  - Phase 3 repair: ON in E, F; OFF in B, C, D

This script replays each condition's selection logic on stored candidate
data, producing per-task results without re-running any LLM calls.

Usage:
    python3 scripts/eval/derive_ablation.py <condition_f_dir> <output_dir>

Example:
    python3 scripts/eval/derive_ablation.py \
        benchmark/results/v31_full_F \
        benchmark/results/v31_ablation_derived

Produces: output_dir/{B,C,D,E}/ with per-task JSONs and summary stats.
"""

import json
import shutil
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2]))
# stages/ (the V3 pipeline stages) lives in v3-service
sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

from stages.candidate_selection import CandidateInfo, select_candidate


# Ablation condition definitions
CONDITIONS = {
    "B": {
        "description": "Phase 1 only, random selection",
        "selection_strategy": "random",
        "include_phase2": False,
        "include_phase3": False,
    },
    "C": {
        "description": "Phase 1 + Lens selection",
        "selection_strategy": "lens",
        "include_phase2": False,
        "include_phase3": False,
    },
    "D": {
        "description": "Phase 1 + Phase 2 + Lens selection",
        "selection_strategy": "lens",
        "include_phase2": True,
        "include_phase3": False,
    },
    "E": {
        "description": "Phase 1 + Phase 3 + Lens selection",
        "selection_strategy": "lens",
        "include_phase2": False,
        "include_phase3": True,
    },
}


def _derive_from_passing(task_data: dict, condition: dict, telemetry: dict,
                         candidates: list, passing: list) -> dict:
    """Replay Phase 1 selection for a task with passing candidates: the S* winner
    of Condition F when the condition has Phase 2 and S* ran, else its strategy."""
    strategy = condition["selection_strategy"]

    if condition["include_phase2"] and len(passing) >= 2:
        # Phase 2 (S*) tiebreaking was applied in Condition F.
        # If S* triggered in F, use its result for D. Otherwise
        # fall through to lens selection. The S* result is the same
        # because candidates are identical.
        s_star_triggered = telemetry.get("s_star_triggered", False)
        if s_star_triggered:
            # S* already picked the winner in F — same result for D
            return {
                "task_id": task_data["task_id"],
                "passed": True,
                "code": task_data.get("code", ""),
                "phase_solved": "phase1",
                "candidates_generated": len(candidates),
                "total_tokens": task_data.get("total_tokens", 0),
                "derived_from": "condition_F",
                "selection_strategy": strategy,
                "s_star_triggered": True,
            }

    # Apply selection strategy
    candidate_infos = [
        CandidateInfo(
            index=c.get("index", i),
            code=c.get("code", ""),
            energy=c.get("energy", 0.0),
            passed=True,
        )
        for i, c in enumerate(passing)
    ]
    selected = select_candidate(candidate_infos, strategy=strategy, seed=42)
    code = selected.code if selected else passing[0]["code"]

    return {
        "task_id": task_data["task_id"],
        "passed": True,
        "code": code,
        "phase_solved": "phase1",
        "candidates_generated": len(candidates),
        "total_tokens": task_data.get("total_tokens", 0),
        "derived_from": "condition_F",
        "selection_strategy": strategy,
    }


def derive_condition(task_data: dict, condition: dict) -> dict:
    """Replay selection logic for one task under a given condition.

    Args:
        task_data: Per-task JSON from the Condition F run.
        condition: Condition definition dict.

    Returns:
        Derived result dict with passed/code/phase_solved.
    """
    telemetry = task_data.get("telemetry", {})
    candidates = telemetry.get("all_candidates", [])

    if not candidates:
        # No candidates stored — can't derive
        return {
            "task_id": task_data["task_id"],
            "passed": False,
            "code": task_data.get("code", ""),
            "phase_solved": "none",
            "candidates_generated": task_data.get("candidates_generated", 0),
            "total_tokens": task_data.get("total_tokens", 0),
            "derived_from": "condition_F",
            "selection_strategy": condition["selection_strategy"],
        }

    # Find passing candidates from stored data
    passing = [c for c in candidates if c.get("passed") is True]

    # Phase 1 selection (without Phase 3)
    if passing:
        return _derive_from_passing(task_data, condition, telemetry, candidates, passing)

    # No passing candidates
    if condition["include_phase3"]:
        # Phase 3 runs on failures — same candidates fail → same Phase 3 result
        # Use the Condition F result directly (Phase 3 is deterministic on
        # the same failing candidates with the same seeds)
        phase_solved = task_data.get("phase_solved", "none")
        if phase_solved in ("pr_cot", "refinement", "derivation"):
            return {
                "task_id": task_data["task_id"],
                "passed": True,
                "code": task_data.get("code", ""),
                "phase_solved": phase_solved,
                "candidates_generated": len(candidates),
                "total_tokens": task_data.get("total_tokens", 0),
                "derived_from": "condition_F",
                "selection_strategy": condition["selection_strategy"],
            }

    # Failed — no passing candidates, no Phase 3 success
    best_code = ""
    if candidates:
        # Pick best by energy (lowest)
        sorted_cands = sorted(candidates, key=lambda c: c.get("energy", 0.0))
        best_code = sorted_cands[0].get("code", "")

    return {
        "task_id": task_data["task_id"],
        "passed": False,
        "code": best_code,
        "phase_solved": "none",
        "candidates_generated": len(candidates),
        "total_tokens": task_data.get("total_tokens", 0),
        "derived_from": "condition_F",
        "selection_strategy": condition["selection_strategy"],
    }


def main():
    if len(sys.argv) < 3:
        print(__doc__)
        sys.exit(1)

    f_dir = Path(sys.argv[1])
    out_dir = Path(sys.argv[2])

    per_task_dir = f_dir / "v3_lcb" / "per_task"
    if not per_task_dir.exists():
        print(f"ERROR: {per_task_dir} not found")
        sys.exit(1)

    # Load all Condition F results
    task_files = sorted(per_task_dir.glob("*.json"))
    print(f"Loaded {len(task_files)} task results from Condition F")

    f_results = {}
    for tf in task_files:
        with open(tf) as f:
            data = json.load(f)
        task_id = data.get("task_id", tf.stem.replace("_", "/"))
        f_results[task_id] = data

    # Check that all_candidates is stored
    sample = next(iter(f_results.values()), {})
    if not sample.get("telemetry", {}).get("all_candidates"):
        print("WARNING: Condition F run does not have all_candidates stored.")
        print("Re-run with the updated v3_runner.py that saves all candidate codes.")
        sys.exit(1)

    # Derive each condition
    for cond_name, cond_def in CONDITIONS.items():
        cond_dir = out_dir / f"condition_{cond_name}"
        cond_task_dir = cond_dir / "v3_lcb" / "per_task"
        cond_task_dir.mkdir(parents=True, exist_ok=True)

        passed_count = 0
        total = 0
        phase_counts = {}

        for task_id, f_data in f_results.items():
            derived = derive_condition(f_data, cond_def)
            total += 1
            if derived["passed"]:
                passed_count += 1

            phase = derived.get("phase_solved", "none")
            phase_counts[phase] = phase_counts.get(phase, 0) + 1

            # Save per-task JSON
            safe_name = task_id.replace("/", "_") + ".json"
            with open(cond_task_dir / safe_name, "w") as f:
                json.dump(derived, f, indent=2)

        # Save summary
        summary = {
            "condition": cond_name,
            "description": cond_def["description"],
            "total_tasks": total,
            "passed": passed_count,
            "pass_rate": passed_count / total if total > 0 else 0,
            "phase_solved_counts": phase_counts,
            "derived_from": str(f_dir),
        }
        with open(cond_dir / "summary.json", "w") as f:
            json.dump(summary, f, indent=2)

        pct = summary["pass_rate"] * 100
        print(f"  Condition {cond_name} ({cond_def['description']}): "
              f"{passed_count}/{total} ({pct:.1f}%) — {phase_counts}")

    # Copy Condition F results as-is
    f_copy_dir = out_dir / "condition_F"
    if not f_copy_dir.exists():
        shutil.copytree(f_dir, f_copy_dir)
        f_total = len(f_results)
        f_passed = sum(1 for d in f_results.values() if d.get("passed"))
        print(f"  Condition F (Full pipeline): "
              f"{f_passed}/{f_total} ({f_passed/f_total*100:.1f}%)")

    print(f"\nResults saved to: {out_dir}")
    print("NOTE: Condition A (baseline) requires a separate run with --baseline")


if __name__ == "__main__":
    main()
