# V3 Ablation Study

> **Withdrawn (2026-09-25).** The 74.6% LiveCodeBench result in this report,
> and the phase-by-phase gains derived from it, are withdrawn. They are not
> LiveCodeBench pass@1, for three reasons found in the benchmark runner (the
> V3.0 release code and the current `atlas/bench/v3_runner.py` behave the same
> way):
>
> 1. **LiveCodeBench's hidden tests were never run.** The dataset loader
>    cannot decode the private test suite, so it silently falls back to the
>    1-5 worked examples printed in each problem statement. Those examples
>    were the only tests the runner ever ran, and they were also the final
>    grade.
> 2. **A task counted as passed when any of the three Phase 1 candidates
>    passed those examples.** Lens selection only chose among candidates that
>    had already passed, so it could not change the count (conditions B and C
>    are both 403/599).
> 3. **Repair saw the examples' expected output.** Failing candidates'
>    "Expected / Got" output went into the PR-CoT and failure-analysis
>    prompts, and a repair counted only once it passed those same examples.
>
> The statements below that the real tests were "used only for final
> scoring", were "never exposed to the model", and that the result "uses no
> information from the benchmark test suite" are therefore incorrect. A
> re-grade of a 130-task sample of the published traces against the hidden
> tests found that 14 of its 90 published passes fail them. The rest of this
> report is kept unchanged as a historical record. ATLAS will be re-measured
> on held-out tasks once the current product is re-verified.
>
> **This is the V3.0 ablation report on Qwen3-14B.** No later version or
> model has been benchmarked. Forward-looking V3.1 predictions in §10 below were the
> author's plan at write time (2026-03); some shifted during
> implementation (e.g. G(x) was kept and deployed, not redesigned from
> scratch).

## 1. Executive Summary

V3 achieves **74.6% pass@1** on LiveCodeBench (599 tasks), up from a 54.9% V3 baseline
and 36-41% V2 baseline. The ablation isolates contributions from each V3 phase:

- **Phase 1** (Constraint-Driven Generation) is the dominant contributor at **+12.4pp**.
- **Phase 2** (Intelligent Compute / Lens Routing) provides **+0.0pp** — ineffective in its current form.
- **Phase 3** (Self-Verified Iterative Refinement) adds **+7.3pp**, rescuing 42 of 194 Phase 1 failures.

Total improvement over V3 baseline: **+19.7 percentage points** (54.9% to 74.6%).
Total improvement over V2: **+33pp** (41% to 74.6%), of which ~13pp comes from infrastructure
fixes between V2 and V3 (temperature tuning, ChatML fixes, prompt refinements).

## 2. Methodology

### Model and Hardware

| Parameter | Value |
|-----------|-------|
| Model | Qwen3-14B-Q4_K_M (frozen, no fine-tuning) |
| Draft model | Qwen3-0.6B-Q8_0 (speculative decoding) |
| Temperature | 0.6 |
| Hardware | RTX 5060 Ti 16GB VRAM |
| Runtime | K3s single-node, single GPU |

### Benchmark

| Parameter | Value |
|-----------|-------|
| Benchmark | LiveCodeBench v5 |
| Tasks | 599 |
| Evaluation | stdio (stdin/stdout pairs) |
| Metric | pass@1 |

### What "pass@1" Means Here

We report pass@1 — one solution submitted per task, scored pass/fail — but the generation
process behind that single submission is not single-shot. The V3 pipeline generates **k=3
candidates** per task, selects the best via Geometric Lens energy scoring, and if all
candidates fail, attempts iterative repair via Phase 3 (PR-CoT with self-generated tests).
The final submitted solution is either the best Phase 1 candidate or a Phase 3 repaired
solution.

This is comparable to other systems that use best-of-k, majority voting, or multi-turn
refinement before submitting, but it is **not** equivalent to single-generation pass@1
(k=1, no selection, no repair). For reference, our single-generation baseline (Condition A)
achieves 54.9%.

### Ablation Design

Each condition runs all 599 LCB tasks through the V3 pipeline with specific phases
enabled or disabled. All conditions share the same frozen model, prompts, and seed.
Crash recovery uses per-task checkpoint files with atomic writes.

| Condition | Active Phases | Description |
|-----------|---------------|-------------|
| A | None | All V3 features OFF — V2-style single generation |
| B | Phase 1 | PlanSearch + DivSampling + BudgetForcing (k=3, no repair) |
| C | Phase 1 + Phase 2 | Phase 1 + Lens-driven adaptive K, ReASC, S* tiebreaking |
| D | Phase 1 + Phase 3 | Phase 1 + PR-CoT repair, refinement loop, derivation chains |
| E | All phases | Full V3 pipeline (discontinued — see below) |

### Self-Test Integrity

Phase 3 uses **self-generated test cases** for internal verification: the model reasons
about the problem specification and generates input/output pairs independently. The real
LiveCodeBench test cases are used **only for final scoring**. This avoids the "answer key"
problem and makes results directly comparable to other systems' pass@1 numbers.
**[Incorrect: see the withdrawal notice at the top of this report.]**

Self-test generation success rate: approximately 98% of tasks receive valid self-generated
test cases.

## 3. Results

| Condition | Configuration | Passed | Total | Pass Rate | Delta vs. A |
|-----------|---------------|--------|-------|-----------|-------------|
| A | Baseline (no V3) | 329 | 599 | 54.9% | — |
| B | +Phase 1 | 403 | 599 | 67.3% | +12.4pp |
| C | +Phase 1+2 | 403 | 599 | 67.3% | +0.0pp |
| D | +Phase 1+3 | 447 | 599 | 74.6% | +19.7pp |

Condition E (full V3 with all phases) was stopped at 92/599 tasks and discontinued.
The Phase 2 ineffectiveness observed in Condition C made full-pipeline results redundant;
Condition D already captures the meaningful contribution of Phases 1 and 3.

### Comparison to V2 Baseline

| Baseline | Pass Rate | Delta to Condition D |
|----------|-----------|----------------------|
| V2 (historical) | 36-41% | +33 to +38pp |
| V3 Condition A | 54.9% | +19.7pp |

The V3 Condition A baseline (54.9%) is higher than V2 (36-41%) due to infrastructure
improvements between V2 and V3 runs (temperature tuning, ChatML fixes, prompt refinements).

## 4. Phase Contribution Analysis

### Phase 1: Constraint-Driven Generation (+12.4pp)

Phase 1 comprises three components working together:

| Component | Role |
|-----------|------|
| **PlanSearch** | Generates diverse solution plans via structured multi-step reasoning |
| **DivSampling** | Ensures candidate diversity across k=3 generations |
| **BudgetForcing** | Controls thinking token budget to prevent reasoning waste |

Phase 1 produces 3 candidate solutions per task. The best candidate is selected and
submitted. This alone lifts pass rate from 54.9% to 67.3%, making it the single most
impactful V3 component.

### Phase 2: Intelligent Compute (+0.0pp)

Phase 2 adds Lens-driven compute allocation and tiebreaking:

| Component | Issue |
|-----------|-------|
| **S* Tiebreaking** | Non-functional on stdio tasks — requires distinguishing input generation, which is not implemented for stdin/stdout evaluation |
| **Blend-ASC** | Adaptive K allocation adds no value when Phase 1 already generates 3 strong candidates |
| **ReASC** | Re-ranking provides no lift when candidates are already high quality |

Phase 2 is the clear candidate for redesign in V3.1.

### Phase 3: Self-Verified Iterative Refinement (+7.3pp)

Phase 3 attempts to rescue tasks that Phase 1 failed on:

| Component | Rescues | Share |
|-----------|---------|-------|
| **PR-CoT Repair** | 36 | 85.7% |
| **Refinement Loop** | 6 | 14.3% |
| **Derivation Chains** | 0 | 0.0% |
| **Total** | 42 | 100% |

Phase 3 processed 194 Phase 1 failures (from Condition B's 196 failures, minus 2 edge
cases) and rescued 42 of them, yielding a **21.6% rescue rate**.

## 5. Phase 3 Deep Dive

### PR-CoT Repair (36 rescues)

PR-CoT (Program Repair Chain-of-Thought) is the dominant Phase 3 component. Given a
failing solution and self-generated test cases, the model:

1. Runs the failing code against self-generated tests to identify the failure mode
2. Reasons about root cause using chain-of-thought
3. Generates a repaired solution
4. Validates against self-generated tests before submission

This component alone accounts for 85.7% of all Phase 3 rescues.

### Refinement Loop (6 rescues)

The refinement loop iteratively improves solutions through multiple repair cycles. It
captures cases where a single PR-CoT pass is insufficient but 2-3 iterations converge
on a correct solution.

### Derivation Chains (0 rescues)

Derivation chains attempt to decompose a problem into verifiable sub-problems and solve
them bottom-up. This approach yields **zero rescues** on LiveCodeBench. Competitive
programming problems are not readily decomposable into independently verifiable
sub-problems — they typically require holistic algorithmic insight rather than
compositional construction.

### Self-Test Generation

The self-test pipeline achieves approximately 98% success rate: for nearly all tasks,
the model can reason about the problem statement and generate valid input/output pairs.
These self-generated tests serve as the internal verification signal for all Phase 3
repair attempts. Real LCB tests are never exposed to the model.
**[Incorrect: see the withdrawal notice at the top of this report.]**

## 6. Pre-Revamp Comparison (Answer Key vs Self-Verification)

Before the V3 revamp, Phase 3 iterated against real LCB test cases — the model could see
error messages like "expected 42, got 41" and fix accordingly. This is equivalent to having
the answer key during an exam. A pre-revamp ablation measured the ceiling performance:

| Condition | Method | Pass Rate | Phase 3 Contribution |
|-----------|--------|-----------|---------------------|
| D (pre-revamp) | Answer key — real tests during repair | 78.6% | +11.3pp |
| D (post-revamp) | Self-verified — model-generated tests only | 74.6% | +7.3pp |

The 4.0pp gap (78.6% → 74.6%) represents the cost of legitimacy. The pre-revamp result
is retained as a ceiling reference but is not comparable to other systems' pass@1 numbers.
**[Incorrect: see the withdrawal notice at the top of this report.]**
The post-revamp result uses no information from the benchmark test suite during repair,
making it directly comparable.

### Why the Gap Exists

Self-generated tests are imperfect: the model sometimes generates test cases that are
correct but insufficient to catch the specific bug in the solution. With real tests, the
model gets exact failure feedback ("wrong answer on input [3, 1, 4]") which directly
guides the repair. Self-tests provide weaker signal ("passes all self-tests but may still
be wrong"), which is why the rescue rate drops from 36.9% (answer key) to 21.6%
(self-verified).

### Why Self-Verification is Still Valuable

Despite the weaker signal, self-verification rescues 42 tasks that Phase 1 cannot solve
at all. The 7.3pp improvement is achieved without any privileged information, confirming
that the model's ability to reason about problems, generate test cases, and iteratively
repair solutions is a genuine capability, not a test leakage artifact.

---

## 7. Infrastructure Fixes During Study

Two significant bugs were discovered and fixed during the ablation run:

### ChatML Bug in self_test_gen.py

The LLM callable was stripping thinking blocks (`<think>...</think>`) from responses
before the self-test generation parser could extract test cases. This caused silent
failures in self-test generation (0 cases generated, empty reason field). Fixed by
ensuring the raw ChatML response is preserved through the full parsing pipeline.

**Detection**: Telemetry showed `self_test_gen_events.jsonl` entries with `num_cases: 0`
and empty `reason` fields. The bug was silent — no error was raised, the pipeline simply
proceeded with zero self-tests, causing Phase 3 to skip all repair attempts.

### Derivation Chains on Competitive Programming

Beyond the 0% rescue rate (a finding, not a bug), investigation confirmed that
competitive programming tasks inherently resist the decomposition strategy. Problems
requiring dynamic programming, graph algorithms, or mathematical insight do not factor
into independently testable sub-routines. This is a fundamental limitation of the
approach on this task distribution, not an implementation error.

**Additionally**: The SandboxAdapter silently ignored the `test_case` parameter for stdio
mode tasks, meaning even well-decomposed sub-problems could not be tested. This bug
also affected S* tiebreaking in Phase 2 (which relies on distinguishing inputs run
through the sandbox).

---

## 8. Key Findings

1. **Phase 1 is the primary value driver.** Constraint-driven generation with diverse
   candidates accounts for 12.4 of the 19.7 percentage point improvement (63% of total
   gain over Condition A).

2. **Phase 2 is ineffective in its current form.** Zero marginal improvement. S*
   tiebreaking requires stdio-compatible distinguishing input generation that does not
   yet exist. Adaptive K allocation provides no benefit when Phase 1 already produces
   strong candidates at k=3.

3. **Phase 3 self-verified repair is legitimate and effective.** Using self-generated
   tests (not answer keys) for internal verification, PR-CoT rescues 36 additional tasks.
   The 21.6% rescue rate on Phase 1 failures demonstrates that iterative repair with
   self-verification is a viable strategy.

4. **PR-CoT dominates Phase 3.** At 85.7% of rescues, PR-CoT is the only Phase 3
   component that justifies its compute cost. Refinement loop contributes marginally.
   Derivation chains should be removed or redesigned.

5. **Competitive programming resists decomposition.** LCB tasks are not amenable to
   derivation chain approaches. This finding likely generalizes to other competitive
   programming benchmarks.

6. **Total system improvement is substantial.** From V2 (36-41%) to V3 (74.6%)
   represents a near-doubling of pass rate on a frozen 14B model with no fine-tuning,
   achieved entirely through test-time compute strategies.

## 9. Limitations

1. **Single-benchmark optimization.** All V3 phases were designed, tuned, and ablated on LiveCodeBench v5. GPQA Diamond and SciCode results are reported but neither benchmark received pipeline optimization. Cross-domain generalization remains untested.

2. **Phase 2 C(x) undertrained.** The C(x) cost field was retrained on self-embeddings for V3 (fixing the V2 nomic embedding failure), but the training dataset contained only ~60 samples -- too small to learn a meaningful energy landscape. This is the root cause of the +0.0pp Phase 2 result: with an undertrained C(x), neither adaptive K allocation nor S\* tiebreaking has a useful signal to act on.

3. **G(x) metric tensor dormant.** G(x) operates downstream of C(x), applying metric corrections via Δx = -G⁻¹∇C. With C(x) producing a weak/noisy energy landscape, G(x) has no meaningful geometry to navigate. The correction term contributes nothing. G(x) is being redesigned from the ground up for V3.1.

4. **SandboxAdapter stdio limitation.** S\* distinguishing input tiebreaking is implemented but non-functional on stdio-mode LCB tasks due to a bug where the SandboxAdapter silently ignores the `test_case` parameter for stdio evaluation. This also affects Phase 2's distinguishing input generation.

5. **Sequential task processing.** The benchmark pipeline processes tasks one at a time. This does not affect per-task accuracy but significantly impacts total benchmark runtime.

---

## 10. V3.1 Roadmap

| Initiative | Description | Expected Impact |
|------------|-------------|-----------------|
| Model swap to Qwen3.5-9B | Faster model with native multi-token prediction; frees VRAM | Higher throughput, more room for compute allocation |
| Lens Evolution (Phase 4) | Online C(x) recalibration during benchmark runs via replay buffer + EWC | Better routing accuracy over time |
| Phase 2 redesign | S* needs stdio-compatible distinguishing input generation | Unlock the +0.0pp gap — potential for additional gains |
| Derivation chain removal | Replace with alternative repair strategy or remove entirely | Reduce wasted compute on LCB tasks |
| Pipeline speed optimization | Reduce per-task latency (current V3 is compute-heavy: best-of-3 + Lens + repair) | Faster end-to-end benchmark runs |
| Target | 80-90% LCB pass@1 with improved throughput | |

Full ablation data: [`ablation/README.md`](ablation/README.md) (sibling to this file), which
indexes the per-task results on [HuggingFace](https://huggingface.co/datasets/itigges22/ATLAS/tree/main/benchmarks/v3_ablation).
