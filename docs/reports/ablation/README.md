# V3 Ablation Study — Raw Results

This documents the per-task pass/fail data from the ATLAS V3.0 ablation study on
LiveCodeBench (599 tasks). They are the raw traces behind the 74.6% result,
which is **withdrawn**: the runner graded only the examples printed in each
problem, counted any of three candidates passing as a pass, and let repair see
the examples' expected output (see the notice at the top of
[V3_ABLATION_STUDY.md](../V3_ABLATION_STUDY.md)). The traces are kept so the
withdrawal can be checked. Every pass rate on this page comes from that run.

## Where the data lives

Conditions A–D are published on HuggingFace under
[`benchmarks/v3_ablation/`](https://huggingface.co/datasets/itigges22/ATLAS/tree/main/benchmarks/v3_ablation),
in the same `<condition>/v3_lcb/per_task/*.json` layout described below. Condition E
is in this directory, at `condition_e_full/`.

Per-run embedding traces for every condition are on HuggingFace under
[`ablation_traces/`](https://huggingface.co/datasets/itigges22/ATLAS/tree/main/ablation_traces).

## Ablation Conditions

| Condition | HuggingFace path | Phases Active | Pass@1 | Tasks | Description |
|-----------|------------------|---------------|--------|-------|-------------|
| **A** | `benchmarks/v3_ablation/condition_a_baseline/` | None | **54.9%** (329/599) | 599 | Baseline: frozen Qwen3-14B, no V3 pipeline |
| **B** | `benchmarks/v3_ablation/condition_b_phase1/` | Phase 1 | **67.3%** (403/599) | 599 | +PlanSearch, DivSampling, Budget Forcing |
| **C** | `benchmarks/v3_ablation/condition_c_phase1_2/` | Phase 1+2 | **67.3%** (403/599) | 599 | +Blend-ASC, ReASC, S* tiebreaking |
| **D** | `benchmarks/v3_ablation/condition_d_phase1_3/` | Phase 1+3 | **74.6%** (447/599) | 599 | +PR-CoT Repair, Refinement Loop, Derivation Chains |
| **E** | `condition_e_full/` (this directory) | All | Partial | 94 | Full pipeline (discontinued — OOM at scale) |

## Key Findings

- **Phase 1 (Constraint-Driven Generation)**: +12.4pp (54.9% → 67.3%)
- **Phase 2 (Intelligent Compute)**: +0.0pp (67.3% → 67.3%) — no measurable improvement
- **Phase 3 (Verified Iterative Refinement)**: +7.3pp (67.3% → 74.6%)
- **Total V3 improvement**: +19.7pp over baseline

## Data Format

Each condition directory contains:

```
condition_X/
├── summary.json              # Aggregate results: pass_rate, total_tasks, timing
├── telemetry/
│   ├── v3_events.jsonl       # Per-task V3 pipeline events
│   ├── plan_search_events.jsonl  # PlanSearch constraint/plan details
│   ├── route_decisions.jsonl     # Routing decisions per task
│   └── ...                       # Additional per-component telemetry
└── v3_lcb/
    └── per_task/
        ├── task_001.json     # Per-task result: passed, code, phase_solved, candidates
        ├── task_002.json
        └── ...               # 599 files per condition
```

### Per-Task JSON Format

```json
{
  "task_id": "livecodebench_v5_001",
  "passed": true,
  "code": "def solution()...",
  "phase_solved": "phase1",       // "phase1", "pr_cot", "refinement", "derivation", "none"
  "candidates_generated": 3,
  "total_tokens": 4521,
  "total_time_ms": 12340.5,
  "telemetry": {
    "probe_sandbox_passed": false,
    "adaptive_k": 3,
    "plansearch_constraints": [...],
    "candidate_energies": [...]
  }
}
```

## Reproduction

All conditions used:
- **Model**: Qwen3-14B-Q4_K_M + Qwen3-0.6B-Q8_0 draft (speculative decoding)
- **Dataset**: LiveCodeBench v5 (599 tasks, bzantium mirror)
- **Seeds**: Fixed seed 42 for all conditions
- **1 seed per condition** (k=3 candidates per task in Phase 1)
- **Hardware**: RTX 5060 Ti 16GB, single GPU

Condition D cannot be reproduced at this commit, and reproducing it would
not be useful: the runner no longer has a `--no-phase2` flag, the Qwen3-14B
configuration and several V3.0 stages no longer exist, and the grading flaws
described above are still present in the current runner.

To reproduce condition A (baseline):

```bash
python -m atlas.bench.v3_runner \
  --baseline \
  --run-id condition_a_reproduction
```

## Computing Pass@1 from Raw Data

Fetch a condition, then count passes:

```bash
huggingface-cli download itigges22/ATLAS \
  --repo-type dataset \
  --include "benchmarks/v3_ablation/condition_d_phase1_3/*" \
  --local-dir ./ablation_data
```

```python
import json, glob

condition = "./ablation_data/benchmarks/v3_ablation/condition_d_phase1_3"
tasks = glob.glob(f"{condition}/v3_lcb/per_task/*.json")
passed = sum(1 for t in tasks if json.load(open(t)).get("passed", False))
total = len(tasks)
print(f"Pass@1: {passed}/{total} = {passed/total:.1%}")
# Prints 447/599: the published count of tasks that passed the printed
# examples. This is not LiveCodeBench pass@1 (see the withdrawal notice).
```
