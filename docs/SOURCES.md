# Sources & Research

Papers and research that inform the ATLAS architecture. Organized by **status relative to the current release** so readers can tell at a glance which papers underpin what's actually shipping and what's on the roadmap.

Two status buckets:

1. **Active** — the paper directly underpins a component that ships in the current release. If you're implementing or extending that component, read these first.
2. **Roadmap / exploratory** — the paper informs planned but unshipped work (V3.2 exploratory or later).

Within each bucket, entries are grouped by the ATLAS subsystem they relate to.

---

## 1. Active in the current release

Research that informs components shipping in the current release.

### Test-Time Compute (foundational)

The premise behind ATLAS: scaling inference compute on a frozen model can match or exceed a bigger one.

- **Brown et al., 2024.** *Large Language Monkeys: Scaling Inference Compute with Repeated Sampling.* arXiv [2407.21787](https://arxiv.org/abs/2407.21787). Weaker models exceed stronger ones with enough samples — the core motivation for every ATLAS version.
- **Wang et al., 2023.** *Self-Consistency Improves Chain of Thought Reasoning in Language Models.* [ICLR 2023](https://openreview.net/forum?id=1PL1NIMMrw). Majority voting over sampled CoTs — conceptual predecessor to the V3 Phase 1 candidate pool.
- **Lightman et al., 2023.** *Let's Verify Step by Step.* arXiv [2305.20050](https://arxiv.org/abs/2305.20050). Process reward models — conceptual basis for the verification and repair phases.

### V3 Pipeline — Phase 1 (Constraint-Driven Generation, shipped)

- **Wang et al., 2025.** *PlanSearch: Planning as Search over Programs.* ICLR 2025 Spotlight, arXiv [2409.03733](https://arxiv.org/abs/2409.03733). **Primary citation for the PlanSearch component.**
- **Wang et al., 2025.** *Think Diverse: DivSampling through Perturbation-Based Diversity.* arXiv [2502.11027](https://arxiv.org/abs/2502.11027). **Primary citation for the DivSampling component.**
- **Muennighoff et al., 2025.** *s1: Simple Test-Time Scaling (Budget Forcing).* arXiv [2501.19393](https://arxiv.org/abs/2501.19393). **Primary citation for the Budget Forcing component.**
- **Aytes et al., 2025.** *Sketch-of-Thought: Efficient LLM Reasoning with Adaptive Cognitive-Inspired Sketching.* EMNLP 2025, arXiv [2503.05179](https://arxiv.org/abs/2503.05179). Informs compressed reasoning prompts used in V3 thinking tiers.

### V3 Pipeline — Phase 2 (Intelligent Compute, shipped)

- **Li et al. (UC Berkeley), 2025.** *S\*: Test Time Scaling for Code Generation via Distinguishing Inputs.* arXiv [2502.14382](https://arxiv.org/abs/2502.14382). **Primary citation for S\* candidate selection** (replaced the V2 Best-of-K approach).

### V3 Pipeline — Phase 3 (Verified Iterative Refinement, shipped)

- **Chen et al., 2022.** *CodeT: Code Generation with Generated Tests.* [ICLR 2023](https://openreview.net/forum?id=ktrw68Cmu9c). Blueprint for self-generated test harnesses in PR-CoT repair.
- **Zhang et al., 2023.** *Self-Edit: Fault-Aware Code Editor for Code Generation.* arXiv [2305.04087](https://arxiv.org/abs/2305.04087). Execution feedback loop — informs the refinement phase.
- **Shinn et al., 2023.** *Reflexion: Language Agents with Verbal Reinforcement Learning.* NeurIPS 2023. Verbal self-correction — informs the self-correcting posture of the Phase 3 repair strategies.
- **FunPRM, 2025.** *Function-Level Process Reward Models.* Meta-learning correction via function decomposition. Reference for fine-grained PRM signals in V3 repair.
- **ThinkPRM, 2025.** *Generative Verification via Reasoning.* Thinking-first verification outperforming direct generation. Basis for the thinking-first repair posture.

### Geometric Lens (core, shipped)

The current release ships C(x) (MLP cost field) and G(x) (XGBoost quality prediction).

- **Blondel et al. (Google DeepMind), 2025.** *Autoregressive Models are Secretly Energy-Based Models.* arXiv [2512.15605](https://arxiv.org/abs/2512.15605). Exact ARM↔EBM bijection. **Primary theoretical justification for the Lens existing at all** — the Lens is the EBM correction layer the paper describes.
- **Anthropic, 2026.** *When Models Manipulate Manifolds.* Internal research communication. Foundational intuition that motivated energy-based scoring.
- **Crespo, J., 2026.** *Everyone's Wrong About AI Programming — Except Maybe Anthropic.* Medium. Accessible framing of the geometric perspective that ATLAS operationalizes.
- **Aghajanyan et al., 2020.** *Intrinsic Dimensionality Explains the Effectiveness of Language Model Fine-Tuning.* arXiv [2012.13255](https://arxiv.org/abs/2012.13255). Theoretical backbone for training small auxiliary networks (Lens MLPs / XGBoost classifiers) instead of the base model.
- **Hong et al., 2024.** *GeLoRA: Geometric Adaptive Ranks for Efficient LoRA Fine-Tuning.* arXiv [2412.09250](https://arxiv.org/abs/2412.09250). Direct inspiration for the geometric scoring approach used by the Lens.

### Structural code reasoning (shipped)

Tree-sitter-backed structural tooling: `structural_edit`, the symbol index, the structural veto on unresolved calls, and the call-graph layer (always on, Python files only). Delivered under [issue #39](https://github.com/inferstep/ATLAS/issues/39).

- **Sotnikov, D., 2026.** *chiasmus: tree-sitter + solver call graph for code analysis.* GitHub [yogthos/chiasmus](https://github.com/yogthos/chiasmus). Inspiration for the structural code-reasoning layer.

### Design constraints (shipped)

Papers that informed architectural decisions by describing what *not* to do.

- **Shumailov et al., 2024.** *The Curse of Recursion: Training on Generated Data Makes Models Forget (Model Collapse).* arXiv [2305.17493](https://arxiv.org/abs/2305.17493). Model collapse from synthetic data — the risk that gated every "train on our own traces" proposal. **This is a major reason ATLAS freezes the base model and trains only the Lens.**

---

## 2. Roadmap / exploratory

Research that informs planned work. None of these are part of the current release; they are here to give collaborators a reading list for the tracking issues.

### V3.2 (exploratory)

- **Karan & Chatterji, 2025.** *Reasoning with Sampling: Your Base Model is Smarter Than You Think.* arXiv [2510.14901](https://arxiv.org/abs/2510.14901). MCMC over logits during decoding — tracked as [issue #9](https://github.com/inferstep/ATLAS/issues/9).

---

## What this list intentionally excludes

- **MoE / expert quantization work** (DynaExq, MoPEQ, MxMoE) — relevant only to the V3 Phase 5/6 model-swap roadmap, which is not active development.
- **Medusa** (parallel decoding heads) — ATLAS uses no speculative decoding: the serving entrypoint passes no draft model (`inference/entrypoint-v3.1.sh`).
- **Forward-Forward, Direct Feedback Alignment** — speculative alternative learning rules, not in current plans.
- **Community posts without a primary source** — referenced informally in PRs/issues but not cited here.

If a paper you expected to see is missing and it's actually load-bearing for something shipping, open an issue and we'll add it.
