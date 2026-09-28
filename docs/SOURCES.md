# Sources & Research

Papers and research that inform the ATLAS architecture. Organized by **status relative to the current release** so readers can tell at a glance which papers underpin what's actually shipping, what's held over from earlier versions, and what's on the roadmap.

Three status buckets:

1. **Active** — the paper directly underpins a component that ships in the current release. If you're implementing or extending that component, read these first.
2. **Deprecated / superseded** — the paper informed a V1/V2/V3.0 component that is no longer part of the current pipeline. Kept here for historical context and because older branches still reference them.
3. **Roadmap / exploratory** — the paper informs planned but unshipped work (V3.2 exploratory or later).

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

Tree-sitter-backed structural tooling: `structural_edit`, the symbol index, the structural veto on unresolved calls, and the call-graph layer (always on, Python files only). Tracked as [issue #39](https://github.com/inferstep/ATLAS/issues/39).

- **Sotnikov, D., 2026.** *chiasmus: tree-sitter + solver call graph for code analysis.* GitHub [yogthos/chiasmus](https://github.com/yogthos/chiasmus). Inspiration for the structural code-reasoning layer.

### Design constraints (shipped)

Papers that informed architectural decisions by describing what *not* to do.

- **Shumailov et al., 2024.** *The Curse of Recursion: Training on Generated Data Makes Models Forget (Model Collapse).* arXiv [2305.17493](https://arxiv.org/abs/2305.17493). Model collapse from synthetic data — the risk that gated every "train on our own traces" proposal. **This is a major reason ATLAS freezes the base model and trains only the Lens.**

---

## 2. Deprecated / superseded

Research that informed components that are *not* part of the current release. Kept for historical context.

- **Ebbinghaus, 1885**, *Über das Gedächtnis*; **ACT-R** (Anderson et al.); **Park et al., 2025**, *Memoria: Human-Inspired Memory Architecture*, arXiv [2310.03052](https://arxiv.org/abs/2310.03052). The decay term, its half-life baseline and the Hebbian co-occurrence weights of the pattern cache. **The pattern cache was removed in 2026-09**: it stored the solution of every successful session, evaluation runs included, and injected them into later runs.

- **Kirkpatrick et al., 2017.** *Overcoming Catastrophic Forgetting in Neural Networks (EWC).* PNAS, [doi:10.1073/pnas.1611835114](https://doi.org/10.1073/pnas.1611835114); **Lin, 1992 / Parisi et al., 2019**, Experience Replay / continual learning surveys. The diagonal Fisher penalty and the domain-stratified replay buffer of the Phase 4 online C(x) retrain. **Both were removed in 2026-09 with the lens retrain endpoint they served**, which every shipped deployment refused (the service cannot write its models dir) and no product path called. Lens retraining is host-side `atlas lens build`.

- **Liu et al., 2025.** *Compute-Optimal TTS: 0.5B beats GPT-4o.* arXiv [2512.02008](https://arxiv.org/abs/2512.02008). Motivated the Confidence Router's difficulty-aware routing. **The router was removed in the 2026-07 simplification** — it was reachable only through lens endpoints nothing in the product called.
- **VectifyAI, 2025.** *MAFIN 2.5 / PageIndex.* Reasoning-based retrieval over tree structures. Basis for the PageIndex V2 indexer (tree-sitter AST + BM25 + LLM-guided traversal), **removed with the retrieval stack in the 2026-08 simplification**.
- **Behrouz et al. (Google Research), 2025.** *Titans: Learning to Memorize at Test Time.* arXiv [2501.00663](https://arxiv.org/abs/2501.00663). Surprise-based memory — informed the cache consolidator's category-surprise momentum. **The consolidator (and the STM→LTM promotion it drove) was removed**, and the pattern cache itself was removed in 2026-09.

- **Leviathan et al., 2023.** *Fast Inference from Transformers via Speculative Decoding.* ICML 2023, arXiv [2211.17192](https://arxiv.org/abs/2211.17192). 2-3× speedup via draft models. **Used in V3.0's 14B spec-decode configuration; the current reference configuration runs 9B without a draft model** (hybrid DeltaNet architecture, `--parallel 4`, no draft model). The V3.0 spec-decode entrypoint has been removed from the tree; see git history for the configuration.
- **Hu et al., 2021.** *LoRA: Low-Rank Adaptation of Large Language Models.* arXiv [2106.09685](https://arxiv.org/abs/2106.09685). **Conceptual reference only.** ATLAS does not apply LoRA to the base model — weights stay frozen. Retained here because it seeded the PEFT mindset behind the small-auxiliary-network pattern used by the Lens.
- **Dettmers et al., 2023.** *QLoRA: Efficient Finetuning of Quantized LLMs.* arXiv [2305.14314](https://arxiv.org/abs/2305.14314). **Conceptual reference only.** Same status as LoRA — no QLoRA adapters on the base model.
- **Zhang et al., 2024.** *RAFT: Adapting Language Model to Domain Specific RAG.* arXiv [2403.10131](https://arxiv.org/abs/2403.10131). Considered during the (since-removed) RAG design but **never directly implemented**.
- **Asai et al., 2023.** *Self-RAG: Retrieve-Generate-Critique with self-reflection.* arXiv [2310.11511](https://arxiv.org/abs/2310.11511). **Not implemented** — and the retrieval stack it would have informed was removed.

---

## 3. Roadmap / exploratory

Research that informs planned work. None of these are part of the current release; they are here to give collaborators a reading list for the tracking issues.

### V3.2 (exploratory)

- **Karan & Chatterji, 2025.** *Reasoning with Sampling: Your Base Model is Smarter Than You Think.* arXiv [2510.14901](https://arxiv.org/abs/2510.14901). MCMC over logits during decoding — tracked as [issue #9](https://github.com/inferstep/ATLAS/issues/9).

---

## What this list intentionally excludes

- **MoE / expert quantization work** (DynaExq, MoPEQ, MxMoE) — relevant only to the V3 Phase 5/6 model-swap roadmap, which is not active development.
- **Medusa** (parallel decoding heads) — ATLAS uses standard draft-model speculative decoding (in V3.0), not Medusa.
- **Forward-Forward, Direct Feedback Alignment** — speculative alternative learning rules, not in current plans.
- **Community posts without a primary source** — referenced informally in PRs/issues but not cited here.

If a paper you expected to see is missing and it's actually load-bearing for something shipping, open an issue and we'll add it.
