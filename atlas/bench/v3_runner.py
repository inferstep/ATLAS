#!/usr/bin/env python3
"""
ATLAS V3 Benchmark Runner.

Orchestrates the full V3 pipeline on LiveCodeBench:

  For each task:
    Phase 1: Generate k constraint-diverse candidates
      - PlanSearch → constraints
      - DivSampling → diverse prompts
      - Budget Forcing → token control
      - Sandbox test all k
      - If any pass → Lens selects best → DONE

    Phase 2: CxGx compute allocation (probe C(x) tier + G(x) escalation,
             floored at k=3)

    Phase 3: Verified iterative refinement (if 0/k pass)
      - PR-CoT repair (quick fix, 1-2 attempts)
      - Full refinement loop:
        - 3A: Failure analysis
        - 3B: Constraint refinement
        - 3E: Loop orchestration (max 2 iterations)

Telemetry: results/<run_id>/telemetry/v3_events.jsonl
"""

import json
import os
import re
import shutil
import sys
import threading
import time
import urllib.request
import urllib.error
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Dict, List, Optional, Tuple

# Force line-buffered stdout
sys.stdout.reconfigure(line_buffering=True)

from atlas.env import atlas_root

# The V3 pipeline stages live in v3-service/stages/ (shared with the V3
# service image) — put the checkout's v3-service on sys.path the same way
# the CLI does for geometric-lens.
sys.path.insert(0, str(Path(atlas_root()) / "v3-service"))

from atlas.bench.config import config
from atlas.bench.models import BenchmarkTask
from atlas.bench.runner import execute_code, execute_code_stdio
from atlas.bench.geo_learning import extract_embedding_urllib
from atlas.bench.best_of_k import (
    NEUTRAL_COMBINED, score_candidate, score_candidate_combined,
)

# V3 pipeline stages (shared with the V3 service)
from stages.llm_client import chat_completion, chatml_to_messages, extract_code
from stages.budget_forcing import BudgetForcing, BudgetForcingConfig
from stages import cxgx_gate
from stages.plan_search import PlanSearch, PlanSearchConfig
from stages.div_sampling import DivSampling, DivSamplingConfig
from stages.failure_analysis import (
    FailureAnalyzer, FailureAnalysisConfig, FailingCandidate,
)
from stages.constraint_refinement import (
    ConstraintRefiner, ConstraintRefinementConfig,
)
from stages.pr_cot import PRCoT, PRCoTConfig
from stages.refinement_loop import (
    RefinementLoop, RefinementLoopConfig,
    can_afford_iteration, estimate_iteration_ms,
)
from stages.self_test_gen import SelfTestGen, SelfTestGenConfig
from stages.candidate_selection import (
    CandidateInfo, energy_rank_key, select_candidate,
)
from stages.embedding_store import EmbeddingWriter


# --- Constants ----------------------------------------------------------------

# Resolved from .env (Docker) / atlas.conf (K3s) / explicit env var — see
# BenchmarkConfig.llama_url / .rag_url. No deployment-specific port hardcoding.
LENS_URL = config.rag_url
LLAMA_URL = config.llama_url
# Published Qwen3.5 benchmarks use: temp=0.6, top_k=20, top_p=0.95,
# max_tokens=32768+, thinking mode enabled. Match their settings.
MAX_TOKENS = 8192
BASE_TEMPERATURE = 0.6  # Qwen3.5 recommended for coding with thinking
DIVERSITY_TEMPERATURE = 0.8  # Slightly higher for candidate diversity


# --- Atomic I/O ----------------------------------------------------------------

def atomic_write_json(filepath, data):
    filepath = Path(filepath)
    filepath.parent.mkdir(parents=True, exist_ok=True)
    tmp = filepath.with_suffix('.tmp')
    try:
        with open(tmp, 'w') as f:
            json.dump(data, f, indent=2)
        shutil.move(str(tmp), str(filepath))
    except Exception:
        if tmp.exists():
            tmp.unlink()
        raise


def append_jsonl(filepath, record):
    filepath = Path(filepath)
    filepath.parent.mkdir(parents=True, exist_ok=True)
    with open(filepath, 'a') as f:
        f.write(json.dumps(record) + '\n')


def wrap_class_solution(code: str, task: BenchmarkTask) -> str:
    """Wrap 'class Solution' code with a stdin/stdout harness for stdio eval.

    Many LCB tasks provide a 'class Solution' method signature in the prompt.
    The model completes the class but doesn't add stdin/stdout handling.
    This wrapper parses the method signature from the task prompt and appends
    a harness that reads stdin, calls the method, and prints the result.

    Returns the original code unchanged if it's not a class Solution pattern,
    already has input() calls, or the task is not stdio eval.
    """
    if task.eval_mode != "stdio":
        return code
    if "class Solution" not in code:
        return code
    if "input()" in code:
        return code  # already handles stdin

    # Extract method signature from task prompt
    sig_match = re.search(
        r'class Solution:.*?def (\w+)\(self,?\s*(.*?)\)\s*(?:->.*?)?:',
        task.prompt, re.DOTALL,
    )
    if not sig_match:
        return code

    method_name = sig_match.group(1)
    params_str = sig_match.group(2).strip()

    # Parse parameter names (ignore type annotations)
    param_names = []
    if params_str:
        for p in params_str.split(','):
            name = p.split(':')[0].strip()
            if name:
                param_names.append(name)

    # Prepend typing imports (class may use List, Dict, etc. without importing)
    # then append stdin/stdout harness after the class definition.
    preamble = "from typing import List, Optional, Tuple, Dict, Set\nimport ast"

    reader_lines = []
    for name in param_names:
        reader_lines.append(f"{name} = ast.literal_eval(input())")
    call_args = ", ".join(param_names)
    reader_lines.append(f"result = Solution().{method_name}({call_args})")
    reader_lines.append("print(result)")

    harness = "\n".join(reader_lines)
    return preamble + "\n" + code + "\n\n" + harness


def find_completed_tasks(phase_dir):
    completed = set()
    per_task_dir = Path(phase_dir) / "per_task"
    if per_task_dir.exists():
        for f in per_task_dir.glob("*.json"):
            try:
                with open(f, 'r') as fh:
                    data = json.load(fh)
                    if 'task_id' in data:
                        completed.add(data['task_id'])
            except (json.JSONDecodeError, IOError):
                # best-effort: swallow on failure (caller continues)
                pass
    return completed


# --- Callable adapters for V3 components --------------------------------------

def self_verify_execute(results: List[Tuple[bool, str, str]],
                        threshold: float = 0.6) -> Tuple[bool, str, str]:
    """Majority-vote self-verification from multiple test case results.

    Args:
        results: List of (passed, stdout, stderr) per self-test case.
        threshold: Fraction of tests that must pass (0.0-1.0).

    Returns:
        (majority_passed, combined_stdout, combined_stderr)
    """
    if not results:
        return False, "", "no self-test results"

    passes = sum(1 for p, _, _ in results if p)
    ratio = passes / len(results)

    all_stderr = [s for _, _, s in results if s]
    all_stdout = [s for _, s, _ in results if s]

    return (
        ratio >= threshold,
        "\n".join(all_stdout),
        "\n".join(all_stderr),
    )


class LLMAdapter:
    """Adapts chat_completion to the V3 LLMCallable signature:
    (prompt, temperature, max_tokens, seed) -> (response, tokens, time_ms).

    The prompt may be a ChatML string assembled by V3 components or raw text;
    it is normalized to chat messages and sent through the shared client, so the
    model's own chat template applies.

    Request serialization: some hybrid architectures hang when multiple slots
    generate concurrently via cont-batching. A class-level lock serializes
    generation by default; set ATLAS_LLM_PARALLEL=1 to allow concurrent calls
    (requires --no-cache-prompt on llama-server to prevent checkpoint-restore
    hang).
    """

    THINK_BUDGET_RATIO = 0.80
    MIN_OUTPUT_CHARS = 50

    # Serialize generation by default to avoid concurrent multi-slot hangs.
    # Set ATLAS_LLM_PARALLEL=1 to disable the lock.
    _llm_lock = threading.Lock()
    _parallel_mode = os.environ.get("ATLAS_LLM_PARALLEL", "0") == "1"

    def __init__(self, llm_url: str = "", max_retries: int = 2,
                 timeout: int = 900):
        self.llm_url = llm_url or LLAMA_URL
        self.max_retries = max_retries
        # Scale timeout by parallel tasks — shared GPU bandwidth means each
        # call takes proportionally longer with more concurrent tasks.
        parallel_tasks = int(os.environ.get("ATLAS_PARALLEL_TASKS", "1"))
        if LLMAdapter._parallel_mode and parallel_tasks > 1:
            self.timeout = timeout * parallel_tasks
        else:
            self.timeout = timeout
        self.call_count = 0
        self.total_tokens = 0
        self.total_time_ms = 0.0
        self.last_logprobs: List[float] = []

    @property
    def avg_call_ms(self) -> float:
        """Average observed per-call latency (0.0 before the first call).
        Feeds the refinement loop's one-iteration cost estimate."""
        if not self.call_count:
            return 0.0
        return self.total_time_ms / self.call_count

    def __call__(self, prompt: str, temperature: float,
                 max_tokens: int, seed: Optional[int]) -> Tuple[str, int, float]:
        self.call_count += 1

        # `prompt` may be a ChatML string (from phase modules) or raw text;
        # chatml_to_messages() normalizes it so the model's own template applies.
        # Generation is serialized unless ATLAS_LLM_PARALLEL=1 (see class docs).
        messages = chatml_to_messages(prompt)

        # Thinking on/off follows the system prompt's own ask (the
        # LLMCallable contract is positional, so there's no kwarg to carry
        # a tier). Prompts that instruct the model to think get thinking;
        # structured/concise prompts stay off. Note this is a deliberate
        # narrowing vs pre-migration, where thinking defaulted ON for any
        # prompt without an in-text /nothink — analysis/decomposition/
        # constraint prompts incidentally thought; they are now off because
        # thinking wastes tokens on structured output:
        #   budget_forcing think tiers   -> "Think step by step"
        #   pr_cot repair                -> "Think carefully about the root cause"
        #   refinement_loop code-gen     -> "Think through the approach"
        #   nothink / analysis / constraint prompts carry no think-language.
        # Reword those prompts and this marker list together.
        system_text = " ".join(m.get("content", "") for m in messages
                               if m.get("role") == "system").lower()
        enable_thinking = ("think step by step" in system_text
                           or "think carefully" in system_text
                           or "think through" in system_text)

        def _generate():
            return chat_completion(
                self.llm_url,
                messages=messages,
                temperature=temperature,
                max_tokens=max_tokens,
                seed=seed,
                enable_thinking=enable_thinking,
                want_logprobs=True,
                timeout=self.timeout,
            )

        if LLMAdapter._parallel_mode:
            r = _generate()
        else:
            with LLMAdapter._llm_lock:
                r = _generate()

        self.last_logprobs = r["logprobs"]
        tokens = r["tokens"]
        self.total_tokens += tokens
        self.total_time_ms += r["time_ms"]
        return r["content"], tokens, r["time_ms"]


class SandboxAdapter:
    """Adapts execute_code/execute_code_stdio to V3 SandboxCallable.

    V3 components expect: (code, test_case) -> (passed, stdout, stderr)

    In self_verify_mode, runs code against model-generated test cases
    instead of real benchmark tests. Uses majority vote for pass/fail.
    """

    def __init__(self, task: BenchmarkTask, timeout_sec: int = 30,
                 memory_mb: int = 512,
                 self_verify_mode: bool = False,
                 custom_test_cases: Optional[List] = None,
                 majority_threshold: float = 0.6):
        self.task = task
        self.timeout_sec = timeout_sec
        self.memory_mb = memory_mb
        self.call_count = 0
        self.self_verify_mode = self_verify_mode
        self.custom_test_cases = custom_test_cases or []
        self.majority_threshold = majority_threshold

    def __call__(self, code: str, test_case: str) -> Tuple[bool, str, str]:
        self.call_count += 1
        code = wrap_class_solution(code, self.task)

        if self.self_verify_mode and self.custom_test_cases:
            return self._run_self_tests(code)

        if self.task.eval_mode == "stdio":
            passed, stdout, stderr, _ = execute_code_stdio(
                code, self.task.test_inputs, self.task.test_outputs,
                timeout_sec=self.timeout_sec, memory_mb=self.memory_mb,
            )
        else:
            test_code = test_case or self.task.test_code
            passed, stdout, stderr, _ = execute_code(
                code, test_code,
                timeout_sec=self.timeout_sec, memory_mb=self.memory_mb,
            )
        return passed, stdout, stderr

    def _run_self_tests(self, code: str) -> Tuple[bool, str, str]:
        """Run code against self-generated test cases with majority vote."""
        results = []
        for tc in self.custom_test_cases:
            try:
                passed, stdout, stderr, _ = execute_code_stdio(
                    code, [tc.input_str], [tc.expected_output],
                    timeout_sec=self.timeout_sec, memory_mb=self.memory_mb,
                )
                results.append((passed, stdout, stderr))
            except Exception as e:
                results.append((False, "", str(e)))
        return self_verify_execute(results, self.majority_threshold)


class EmbedAdapter:
    """Adapts extract_embedding_urllib to V3 EmbedCallable.

    Retries up to 3 times with backoff to handle transient 503/timeout
    errors when llama-server is busy with generation requests.
    """

    def __init__(self, llama_url: str, max_retries: int = 3):
        self.llama_url = llama_url
        self.call_count = 0
        self.max_retries = max_retries

    def __call__(self, text: str) -> List[float]:
        self.call_count += 1
        for attempt in range(self.max_retries):
            emb = extract_embedding_urllib(text, self.llama_url)
            if emb is not None:
                return emb
            if attempt < self.max_retries - 1:
                time.sleep(2 * (attempt + 1))
        raise RuntimeError(
            f"Embedding extraction failed after {self.max_retries} retries"
        )


# --- V3 Pipeline Orchestrator -------------------------------------------------

class V3Pipeline:
    """Orchestrates all V3 features for a single task.

    This is the core: given a task, run it through the full V3
    cascade and return the result.
    """

    def __init__(self, telemetry_dir: Path,
                 llama_url: str = LLAMA_URL,
                 enable_phase1: bool = True,
                 enable_phase3: bool = True,
                 selection_strategy: str = "lens"):
        self.telemetry_dir = telemetry_dir
        self.llama_url = llama_url
        self.enable_phase1 = enable_phase1
        self.enable_phase3 = enable_phase3
        self.selection_strategy = selection_strategy

        # Read V3 config from atlas.conf (with defaults)
        self._v3_conf = self._load_v3_config()

        # Embedding store for post-hoc analysis (V3.1 Section 5.2)
        self._emb_writer = EmbeddingWriter(telemetry_dir / "embeddings.emb")

        # Initialize V3 components
        self._init_phase1(telemetry_dir)
        self._init_phase3(telemetry_dir)

    @staticmethod
    def _load_v3_config() -> Dict[str, str]:
        """Load V3-specific config values from atlas.conf."""
        v3 = {}
        try:
            conf = config._conf
            v3["bf_default_tier"] = conf.get(
                "ATLAS_V3_BUDGET_FORCING_DEFAULT_TIER", "standard",
            ).strip('"')
            v3["ps_num_plans"] = int(conf.get(
                "ATLAS_V3_PLAN_SEARCH_NUM_PLANS", "3",
            ))
        except Exception:
            # best-effort: swallow on failure (caller continues)
            pass
        return v3

    def _init_phase1(self, telemetry_dir):
        self.budget_forcing = BudgetForcing(
            BudgetForcingConfig(
                enabled=self.enable_phase1,
                default_tier=self._v3_conf.get("bf_default_tier", "standard"),
            ),
            telemetry_dir=telemetry_dir,
        )
        self.plan_search = PlanSearch(
            PlanSearchConfig(
                enabled=self.enable_phase1,
                num_plans=self._v3_conf.get("ps_num_plans", 3),
            ),
            budget_forcing=self.budget_forcing,
            telemetry_dir=telemetry_dir,
        )
        self.div_sampling = DivSampling(
            DivSamplingConfig(enabled=self.enable_phase1),
            telemetry_dir=telemetry_dir,
        )

    def _init_phase3(self, telemetry_dir):
        fa_config = FailureAnalysisConfig(enabled=self.enable_phase3)
        cr_config = ConstraintRefinementConfig(enabled=self.enable_phase3)
        self.failure_analyzer = FailureAnalyzer(fa_config, telemetry_dir=telemetry_dir)
        self.constraint_refiner = ConstraintRefiner(cr_config, telemetry_dir=telemetry_dir)
        self.pr_cot = PRCoT(
            PRCoTConfig(enabled=self.enable_phase3),
            telemetry_dir=telemetry_dir,
        )
        self.refinement_loop = RefinementLoop(
            RefinementLoopConfig(enabled=self.enable_phase3),
            failure_analyzer=self.failure_analyzer,
            constraint_refiner=self.constraint_refiner,
            telemetry_dir=telemetry_dir,
        )
        self.self_test_gen = SelfTestGen(
            SelfTestGenConfig(enabled=self.enable_phase3),
            telemetry_dir=telemetry_dir,
        )

    def run_task(self, task: BenchmarkTask, task_id: str = "") -> Dict[str, Any]:
        """Run a single task through the full V3 pipeline.

        Returns a dict with:
          - passed: bool
          - code: str (winning code)
          - phase_solved: str ("phase1", "pr_cot", "refinement", "none")
          - candidates_generated: int
          - total_tokens: int
          - total_time_ms: float
          - telemetry: dict (per-phase details)
        """
        start_time = time.time()
        task_id = task_id or task.task_id
        llm = LLMAdapter(self.llama_url)
        sandbox = SandboxAdapter(task)
        embed = EmbedAdapter(self.llama_url)

        result = {
            "task_id": task_id,
            "passed": False,
            "code": "",
            "phase_solved": "none",
            "candidates_generated": 0,
            "total_tokens": 0,
            "total_time_ms": 0.0,
            "telemetry": {},
        }

        # Per-phase latency tracking
        latency = {}

        # ===== PROBE: quick candidate for a data-driven early exit =====
        # Generate a single candidate; its lens scores feed candidate
        # sorting, selection, and the CxGx allocation gate below. Uses
        # "standard" tier (up to 2048 thinking tokens) — matches Qwen3.5
        # published benchmark settings where thinking is enabled. Gives the
        # model enough reasoning budget to solve harder tasks at probe,
        # reducing cascade into Phase 3.
        probe_candidate = None
        probe_scores = dict(NEUTRAL_COMBINED)

        if self.enable_phase1:
            probe_start = time.time()
            try:
                chatml = self.budget_forcing.format_chatml(task.prompt, "standard")
                response, tokens, t_ms = llm(
                    chatml, BASE_TEMPERATURE, MAX_TOKENS, 42,
                )
                probe_code = extract_code(response)
                if probe_code:
                    # Combined C(x)+G(x) probe scoring: one embedding
                    # extraction feeds the cost field AND the XGBoost
                    # quality classifier, so the gate below sees both
                    # signals at the price of the C(x) call it replaced.
                    try:
                        probe_scores = score_candidate_combined(
                            probe_code, LENS_URL,
                        )
                        energy_raw = probe_scores["cx_energy"]
                        energy_norm = probe_scores["cx_normalized"]
                    except Exception:
                        energy_raw, energy_norm = None, None
                    result["telemetry"]["probe_cx_normalized"] = energy_norm
                    result["telemetry"]["probe_cx_calibrated"] = (
                        probe_scores["cx_calibrated"])
                    result["telemetry"]["probe_gx_score"] = (
                        probe_scores["gx_score"])
                    result["telemetry"]["probe_gx_available"] = (
                        probe_scores["gx_available"])
                    result["telemetry"]["probe_gx_verdict"] = (
                        probe_scores["verdict"])
                    probe_candidate = {
                        "index": 0,
                        "code": probe_code,
                        "response": response,
                        "tokens": tokens,
                        "time_ms": t_ms,
                        "energy": energy_raw,
                        "energy_norm": energy_norm,
                        "passed": None,
                    }
                    result["total_tokens"] += tokens
            except Exception as e:
                result["telemetry"]["probe_error"] = str(e)
            latency["probe_ms"] = (time.time() - probe_start) * 1000

        # ===== Sandbox-test probe for data-driven early exit =====
        # Instead of predicting difficulty from energy (unreliable on 9B),
        # test the probe directly: if it passes, skip PlanSearch/DivSampling.
        probe_passed_sandbox = False
        if probe_candidate and probe_candidate.get("code"):
            try:
                probe_sandbox = SandboxAdapter(task)
                passed, stdout, stderr = probe_sandbox(
                    probe_candidate["code"], "",
                )
                probe_candidate["passed"] = passed
                probe_candidate["stdout"] = stdout or ""
                probe_candidate["stderr"] = stderr or ""
                probe_passed_sandbox = passed
            except Exception as e:
                probe_candidate["passed"] = False
                probe_candidate["stdout"] = ""
                probe_candidate["stderr"] = str(e)
            # Store embedding early (overlaps with probe sandbox test)
            try:
                emb = embed(probe_candidate["code"])
                label = "PASS" if probe_passed_sandbox else "FAIL"
                self._emb_writer.write(task_id, 0, label, emb)
            except Exception:
                # best-effort: swallow on failure (caller continues)
                pass
            result["telemetry"]["probe_sandbox_passed"] = probe_passed_sandbox

        # ===== Phase 2: CxGx K + Budget Tier allocation =====
        phase2_start = time.time()
        if probe_passed_sandbox:
            # Data-driven early exit: probe already passes sandbox.
            # No need to generate more candidates.
            k = 1
            budget_tier = "nothink"
            bf_tier = self.budget_forcing.select_tier()
            result["telemetry"]["probe_early_exit"] = True
        else:
            # Probe FAILED sandbox — we need diverse candidates, and the
            # probe's own lens scores say how many. C(x) normalized energy
            # picks a base tier, G(x) escalates it when the quality
            # classifier contradicts C(x), and k never falls below 3.
            #
            # The bench has no outer wall-clock cap (the live pipeline's
            # ATLAS_V3_TIMEOUT has no counterpart here), so no budget cap
            # is passed. An earlier four-way comparison of this arm ran on a
            # patched out-of-tree runner and another model, on tasks the lens
            # was trained on; its arms were within noise, so it is not
            # evidence for the gate.
            alloc = cxgx_gate.allocate(
                cx_normalized=probe_scores["cx_normalized"],
                cx_calibrated=probe_scores["cx_calibrated"],
                gx_score=probe_scores["gx_score"],
                gx_available=probe_scores["gx_available"],
                gx_verdict=probe_scores["verdict"],
            )
            k = alloc.k
            budget_tier = alloc.tier
            bf_tier = alloc.tier
            result["telemetry"]["gated_k"] = alloc.k
            result["telemetry"]["gated_tier"] = alloc.tier
            result["telemetry"]["gated_base_tier"] = alloc.base_tier
            result["telemetry"]["gx_escalation"] = alloc.gx_escalation
            result["telemetry"]["alloc_reason"] = alloc.reason

        latency["phase2_alloc_ms"] = (time.time() - phase2_start) * 1000
        result["telemetry"]["adaptive_k"] = k
        result["telemetry"]["budget_tier"] = budget_tier

        # ===== Phase 1: Build candidate pool =====
        phase1_start = time.time()
        candidates = []
        constraints = []

        # Include probe as first candidate
        if probe_candidate:
            candidates.append(probe_candidate)

        # Generate constraint-diverse candidates via PlanSearch
        remaining_k = max(0, k - len(candidates))
        if self.enable_phase1 and remaining_k > 0:
            try:
                # PlanSearch does multiple sequential LLM calls (constraint
                # extraction + plan construction + code gen). Use a longer
                # timeout to handle long competition prompts at 9B speed.
                ps_llm = LLMAdapter(self.llama_url, timeout=300)
                ps_result = self.plan_search.generate(
                    problem=task.prompt, task_id=task_id,
                    llm_call=ps_llm, num_plans=remaining_k,
                )
                result["total_tokens"] += ps_llm.total_tokens
                for cs in ps_result.constraint_sets:
                    constraints.extend(cs.constraints)
                # Log constraint sets for qualitative analysis (V3.1 Section 5.3)
                result["telemetry"]["plansearch_constraints"] = [
                    {"plan_index": i, "constraints": cs.constraints}
                    for i, cs in enumerate(ps_result.constraint_sets)
                ]
                for i, code in enumerate(ps_result.candidates):
                    if not code:
                        continue
                    try:
                        energy_raw, energy_norm = score_candidate(
                            code, LENS_URL,
                        )
                    except Exception:
                        energy_raw, energy_norm = None, None
                    candidates.append({
                        "index": len(candidates),
                        "code": code,
                        "response": "",
                        "tokens": 0,
                        "time_ms": 0.0,
                        "energy": energy_raw,
                        "energy_norm": energy_norm,
                        "passed": None,
                    })
                result["telemetry"]["plansearch_tokens"] = ps_llm.total_tokens
            except Exception as e:
                result["telemetry"]["plansearch_error"] = str(e)

        # Fill remaining slots with DivSampling + Budget Forcing (parallel)
        if self.enable_phase1 and len(candidates) < k:
            def _generate_div_candidate(extra_idx):
                """Generate a single DivSampling candidate (thread-safe)."""
                try:
                    perturbed = self.div_sampling.apply(
                        task.prompt, candidate_index=extra_idx,
                        task_id=task_id,
                    )
                    chatml = self.budget_forcing.format_chatml(
                        perturbed, bf_tier,
                    )
                    max_tok = self.budget_forcing.get_max_tokens(bf_tier)
                    # Each thread creates its own LLMAdapter for thread safety
                    thread_llm = LLMAdapter(self.llama_url)
                    response, tokens, t_ms = thread_llm(
                        chatml, DIVERSITY_TEMPERATURE, max_tok,
                        42 + extra_idx,
                    )
                    code = extract_code(response)
                    if not code:
                        return None
                    try:
                        energy_raw, energy_norm = score_candidate(
                            code, LENS_URL,
                        )
                    except Exception:
                        energy_raw, energy_norm = None, None
                    return {
                        "code": code,
                        "response": response,
                        "tokens": tokens,
                        "time_ms": t_ms,
                        "energy": energy_raw,
                        "energy_norm": energy_norm,
                        "passed": None,
                    }
                except Exception:
                    return None

            fill_indices = list(range(len(candidates), k))
            with ThreadPoolExecutor(max_workers=min(len(fill_indices), 3)) as pool:
                futures = {pool.submit(_generate_div_candidate, idx): idx
                           for idx in fill_indices}
                for future in as_completed(futures):
                    cand = future.result()
                    if cand:
                        cand["index"] = len(candidates)
                        candidates.append(cand)
                        result["total_tokens"] += cand["tokens"]

        # Fallback: if no candidates at all, direct generation
        # Use BudgetForcing "standard" tier — allows thinking (up to 2048
        # tokens). Published Qwen3.5 benchmarks use full thinking mode
        # with temp=0.6 (65.6% LCB v6 with thinking vs ~39% without).
        if not candidates:
            chatml = self.budget_forcing.format_chatml(
                task.prompt, "standard",
            )
            response, tokens, t_ms = llm(
                chatml, BASE_TEMPERATURE, MAX_TOKENS, 42,
            )
            code = extract_code(response)
            try:
                energy_raw, energy_norm = score_candidate(
                    code, LENS_URL,
                )
            except Exception:
                energy_raw, energy_norm = None, None
            candidates.append({
                "index": 0,
                "code": code,
                "response": response,
                "tokens": tokens,
                "time_ms": t_ms,
                "energy": energy_raw,
                "energy_norm": energy_norm,
                "passed": None,
            })
            result["total_tokens"] += tokens

        latency["phase1_gen_ms"] = (time.time() - phase1_start) * 1000
        result["candidates_generated"] = len(candidates)

        # ===== Test ALL candidates in sandbox (pipelined, V3.1 4.2) =====
        # Sandbox tests + embedding storage run in parallel threads.
        # Candidates sorted by energy (low=easy first) for early-exit potential;
        # a candidate the Lens did not score has no energy and goes last.
        sandbox_start = time.time()
        candidates.sort(key=energy_rank_key)
        passing_candidates = []

        def _test_and_embed(cand):
            """Sandbox test + embedding storage for one candidate."""
            if not cand.get("code"):
                cand["passed"] = False
                return cand
            # Skip sandbox test if already tested (e.g., probe early exit)
            if cand.get("passed") is not None:
                return cand
            try:
                task_sandbox = SandboxAdapter(task)
                passed, stdout, stderr = task_sandbox(cand["code"], "")
                cand["passed"] = passed
                cand["stdout"] = stdout or ""
                cand["stderr"] = stderr or ""
            except Exception as e:
                cand["passed"] = False
                cand["stdout"] = ""
                cand["stderr"] = str(e)
            # Inline embedding storage (overlaps with other sandbox tests)
            try:
                emb = embed(cand["code"])
                label = "PASS" if cand.get("passed") else "FAIL"
                self._emb_writer.write(task_id, cand["index"], label, emb)
            except Exception:
                # best-effort: swallow on failure (caller continues)
                pass
            return cand

        n_workers = min(len(candidates), 3) if len(candidates) > 1 else 1
        if n_workers > 1:
            with ThreadPoolExecutor(max_workers=n_workers) as pool:
                futures = {pool.submit(_test_and_embed, c): c for c in candidates}
                for future in as_completed(futures):
                    cand = future.result()
                    if cand.get("passed"):
                        passing_candidates.append(cand)
            # as_completed order is thread-completion order — run-dependent.
            # Sort by energy (ascending, matching the product pipeline) so
            # the [0] fallbacks are deterministic; unscored candidates last.
            passing_candidates.sort(key=energy_rank_key)
        else:
            for cand in candidates:
                cand = _test_and_embed(cand)
                if cand.get("passed"):
                    passing_candidates.append(cand)

        latency["sandbox_ms"] = (time.time() - sandbox_start) * 1000

        # Log per-candidate energies, pass/fail, AND code for analysis.
        # Storing all candidate codes enables ablation replay: run once with
        # full pipeline, then derive other conditions by replaying selection
        # strategies on stored candidates (3.5x faster than 6 separate runs).
        result["telemetry"]["candidate_energies"] = [
            {"index": c["index"], "energy": c["energy"], "passed": c.get("passed")}
            for c in candidates
        ]
        result["telemetry"]["all_candidates"] = [
            {
                "index": c["index"],
                "code": c.get("code", ""),
                "energy": c["energy"],
                "energy_norm": c.get("energy_norm", 0.5),
                "passed": c.get("passed"),
                "tokens": c.get("tokens", 0),
            }
            for c in candidates
        ]

        # Store best candidate code even on failure (for analysis and the
        # `atlas lens build --from-results` corpus)
        if candidates and not passing_candidates:
            result["code"] = candidates[0]["code"]  # Best by energy (sorted)

        # ===== Select best passing candidate =====
        # Selection-strategy pick (lens = min C(x) energy by default). S*
        # tiebreaking used to run first for 2+ passers; across 118 H200
        # tiebreaks with the fixed stdin adapter every pair scored 0-0 and
        # 110/110 winners equaled the lens min-energy pick — zero signal.
        if passing_candidates:
            result["passed"] = True
            result["phase_solved"] = "phase1"

            # Build CandidateInfo objects for strategy-based selection
            candidate_infos = [
                CandidateInfo(
                    index=c["index"], code=c["code"],
                    energy=c["energy"], passed=True,
                    logprobs=llm.last_logprobs if c["index"] == candidates[-1]["index"] else None,
                )
                for c in passing_candidates
            ]

            selected = select_candidate(
                candidate_infos, strategy=self.selection_strategy,
                seed=42,
            )
            result["code"] = selected.code if selected else passing_candidates[0]["code"]

            result["telemetry"]["selection_strategy"] = self.selection_strategy

            result["telemetry"]["latency"] = latency
            result["total_tokens"] = max(result["total_tokens"], llm.total_tokens)
            result["total_time_ms"] = (time.time() - start_time) * 1000
            self._log_v3_event(task_id, result)
            return result

        # ===== Phase 3: Refinement cascade =====
        phase3_start = time.time()
        if not self.enable_phase3:
            result["telemetry"]["latency"] = latency
            result["total_time_ms"] = (time.time() - start_time) * 1000
            self._log_v3_event(task_id, result)
            return result

        # Build failing candidates list for Phase 3 (with actual error output)
        failing = [
            FailingCandidate(
                code=c["code"],
                error_output=c.get("stderr", "") or c.get("stdout", ""),
                index=c["index"],
            )
            for c in candidates if c.get("passed") is False and c["code"]
        ]

        # --- Self-Test Generation (generate ONCE, cache for all iterations) ---
        selftest_start = time.time()
        self_tests = self.self_test_gen.generate(
            problem=task.prompt, llm_call=llm, task_id=task_id,
        )
        latency["self_test_gen_ms"] = (time.time() - selftest_start) * 1000
        result["telemetry"]["self_tests_generated"] = len(self_tests.test_cases)

        # Create self-verify sandbox if we have self-tests
        if self_tests.test_cases:
            self_verify_sandbox = SandboxAdapter(
                task, self_verify_mode=True,
                custom_test_cases=self_tests.test_cases,
                majority_threshold=self.self_test_gen.config.majority_threshold,
            )
        else:
            # Fallback: no self-tests generated, use real sandbox
            # (this is a degraded mode, logged for analysis)
            self_verify_sandbox = sandbox
            result["telemetry"]["self_test_fallback"] = True

        # Run repair strategies SEQUENTIALLY: PR-CoT first (cheapest,
        # 2-6 calls), then the Refinement Loop (3-15 calls). Stop on the
        # first successful fix so losing strategies burn no calls.
        phase3_extra_tokens = 0
        phase3_strategies_tried = []

        # --- Strategy 1: PR-CoT quick repair (2-6 LLM calls) ---
        if failing:
            phase3_strategies_tried.append("pr_cot")
            pr_llm = LLMAdapter(self.llama_url, timeout=300)
            try:
                best_failing = failing[0]
                error_msg = best_failing.error_output or "All test cases failed"

                repair_result = self.pr_cot.repair(
                    problem=task.prompt,
                    code=best_failing.code,
                    error=error_msg,
                    llm_call=pr_llm,
                    task_id=task_id,
                )
                phase3_extra_tokens += pr_llm.total_tokens
                for repair_code in repair_result.repairs:
                    if not repair_code:
                        continue
                    try:
                        # Test repairs directly against real sandbox.
                        # Self-test gating was filtering valid repairs on 9B
                        # (0/15 success rate). Direct sandbox testing is more
                        # reliable — costs a few extra sandbox calls but
                        # eliminates false-negative self-test rejections.
                        real_passed, _, _ = sandbox(repair_code, "")
                        if real_passed:
                            result["passed"] = True
                            result["code"] = repair_code
                            result["phase_solved"] = "pr_cot"
                            break
                    except Exception:
                        continue
            except Exception as e:
                result["telemetry"]["pr_cot_error"] = str(e)

        # --- Strategy 2: Refinement Loop (3-15 LLM calls) ---
        # Entered only when one iteration (~3 sequential LLM calls at the
        # per-call latency observed on this task) fits inside the loop's
        # own max_time_ms budget — the binding constraint in bench, which
        # has no outer wall-clock cap. H200 join: 453/487 refinement
        # entries exhausted the budget with ZERO completed iterations
        # while burning ~6 minutes each.
        run_refinement = bool(not result["passed"] and failing)
        if run_refinement:
            est_ms = estimate_iteration_ms(llm.avg_call_ms)
            budget_ms = self.refinement_loop.config.max_time_ms
            if not can_afford_iteration(budget_ms, est_ms):
                run_refinement = False
                result["telemetry"]["refinement_skipped"] = {
                    "estimated_iteration_ms": round(est_ms),
                    "budget_ms": round(budget_ms),
                }
        if run_refinement:
            phase3_strategies_tried.append("refinement")
            ref_llm = LLMAdapter(self.llama_url, timeout=300)
            try:
                ref_result = self.refinement_loop.run(
                    problem=task.prompt,
                    failing_candidates=failing,
                    original_constraints=constraints,
                    llm_call=ref_llm,
                    sandbox_run=self_verify_sandbox,
                    embed_call=embed,
                    task_id=task_id,
                )
                phase3_extra_tokens += ref_llm.total_tokens
                if ref_result.solved:
                    real_passed, _, _ = sandbox(ref_result.winning_code, "")
                    if real_passed:
                        result["passed"] = True
                        result["code"] = ref_result.winning_code
                        result["phase_solved"] = "refinement"
                        result["telemetry"]["refinement_iterations"] = ref_result.total_iterations
            except Exception as e:
                result["telemetry"]["refinement_error"] = str(e)

        result["telemetry"]["phase3_strategies_tried"] = phase3_strategies_tried
        result["total_tokens"] += phase3_extra_tokens
        latency["phase3_total_ms"] = (time.time() - phase3_start) * 1000
        result["telemetry"]["latency"] = latency
        result["total_tokens"] = max(result["total_tokens"], llm.total_tokens)
        result["total_time_ms"] = (time.time() - start_time) * 1000
        self._log_v3_event(task_id, result)
        return result

    def _log_v3_event(self, task_id: str, result: Dict) -> None:
        """Log a unified V3 pipeline event to JSONL.

        Consolidates all per-task telemetry into a single event for
        the analysis pipeline (V3.1 Section 5.1).
        """
        event = {
            "task_id": task_id,
            "passed": result["passed"],
            "phase_solved": result["phase_solved"],
            "candidates_generated": result["candidates_generated"],
            "total_tokens": result["total_tokens"],
            "total_time_ms": result["total_time_ms"],
            "selection_strategy": self.selection_strategy,
            "timestamp": datetime.now(timezone.utc).isoformat(),
        }
        # Merge all telemetry sub-fields into the event
        telemetry = result.get("telemetry", {})
        for key, value in telemetry.items():
            event[key] = value
        try:
            append_jsonl(self.telemetry_dir / "v3_events.jsonl", event)
        except Exception:
            # best-effort: swallow on failure (caller continues)
            pass


# --- V3 Benchmark Runner -------------------------------------------------------

class V3BenchmarkRunner:
    """Runs V3 benchmark with full pipeline."""

    def __init__(self, run_dir: Path, enable_phase1=True,
                 enable_phase3=True, selection_strategy="lens"):
        self.run_dir = Path(run_dir)
        self.telemetry_dir = self.run_dir / "telemetry"
        self.telemetry_dir.mkdir(parents=True, exist_ok=True)
        self.pipeline = V3Pipeline(
            self.telemetry_dir,
            enable_phase1=enable_phase1,
            enable_phase3=enable_phase3,
            selection_strategy=selection_strategy,
        )
        self._start_time = time.time()

    def run_lcb(self, tasks: List[BenchmarkTask],
                phase_name: str = "v3_lcb") -> Dict[str, Dict]:
        """Run LiveCodeBench tasks through V3 pipeline.

        When ATLAS_LLM_PARALLEL=1, runs multiple tasks concurrently using
        ATLAS_PARALLEL_TASKS workers (default 4). Otherwise runs sequentially.
        """
        phase_dir = self.run_dir / phase_name
        phase_dir.mkdir(parents=True, exist_ok=True)
        per_task_dir = phase_dir / "per_task"
        per_task_dir.mkdir(parents=True, exist_ok=True)

        completed = find_completed_tasks(phase_dir)
        remaining = [t for t in tasks if t.task_id not in completed]
        total = len(tasks)
        done = len(completed)

        if completed:
            print(f"  Resuming: {done}/{total} complete, {len(remaining)} remaining")

        # Load already-completed results
        results: Dict[str, Dict] = {}
        for f in per_task_dir.glob("*.json"):
            try:
                with open(f, 'r') as fh:
                    data = json.load(fh)
                    results[data['task_id']] = data
            except Exception:
                # best-effort: swallow on failure (caller continues)
                pass

        parallel_tasks = int(os.environ.get("ATLAS_PARALLEL_TASKS", "4"))
        use_parallel = LLMAdapter._parallel_mode and parallel_tasks > 1

        if use_parallel:
            print(f"  PARALLEL MODE: {parallel_tasks} concurrent tasks")
            self._run_parallel(remaining, results, per_task_dir, total, done,
                               parallel_tasks)
        else:
            self._run_serial(remaining, results, per_task_dir, total, done)

        # Save phase summary — `done` was tracked here historically for
        # progress logging; the value now flows through summary["total_tasks"]
        # below, so the standalone local was dead.
        passed = sum(1 for r in results.values() if r.get("passed"))
        summary = {
            "phase": phase_name,
            "total_tasks": len(results),
            "passed_tasks": passed,
            "pass_rate": passed / max(len(results), 1),
            "phase_breakdown": self._phase_breakdown(results),
        }
        atomic_write_json(phase_dir / "results.json", summary)

        return results

    def _process_one_task(self, task: BenchmarkTask) -> Dict:
        """Run a single task through the pipeline (thread-safe)."""
        task_start = time.time()
        try:
            return self.pipeline.run_task(task, task_id=task.task_id)
        except Exception as e:
            return {
                "task_id": task.task_id,
                "passed": False,
                "code": "",
                "phase_solved": "error",
                "candidates_generated": 0,
                "total_tokens": 0,
                "total_time_ms": (time.time() - task_start) * 1000,
                "error": str(e),
                "telemetry": {},
            }

    def _save_and_log(self, task_result: Dict, per_task_dir: Path,
                      done: int, total: int) -> None:
        """Save result atomically and print progress."""
        task_id = task_result["task_id"]
        safe_name = task_id.replace('/', '_')
        atomic_write_json(per_task_dir / f"{safe_name}.json", task_result)

        status = "PASS" if task_result["passed"] else "FAIL"
        phase = task_result.get("phase_solved", "?")
        elapsed = time.time() - self._start_time
        rate = done / (elapsed / 3600) if elapsed > 0 else 0
        tokens = task_result.get("total_tokens", 0)
        print(
            f"  [{done}/{total}] {task_id}: {status} "
            f"(via {phase}, {tokens} tok) "
            f"[{rate:.0f} tasks/hr]",
            flush=True,
        )

    def _run_serial(self, remaining, results, per_task_dir, total, done):
        """Process tasks one at a time (safe fallback)."""
        for task in remaining:
            task_result = self._process_one_task(task)
            results[task.task_id] = task_result
            done += 1
            self._save_and_log(task_result, per_task_dir, done, total)

    def _run_parallel(self, remaining, results, per_task_dir, total, done,
                      max_workers):
        """Process tasks concurrently using ThreadPoolExecutor.

        Each task gets its own LLMAdapter (per run_task), so thread safety
        relies on:
          - llama-server handling concurrent /completion requests (--no-cache-prompt)
          - atomic_write_json for per-task results (temp file + rename)
          - EmbeddingWriter._lock for binary embedding file
          - append_jsonl for JSONL telemetry (small writes are atomic on Linux)
          - print() with flush=True (inherently thread-safe in CPython via GIL)
        """
        _done_lock = threading.Lock()
        _done_counter = [done]  # mutable container for closure

        def process_and_save(task):
            task_result = self._process_one_task(task)
            with _done_lock:
                results[task.task_id] = task_result
                _done_counter[0] += 1
                current_done = _done_counter[0]
            self._save_and_log(task_result, per_task_dir, current_done, total)
            return task_result

        with ThreadPoolExecutor(max_workers=max_workers) as executor:
            futures = {
                executor.submit(process_and_save, task): task
                for task in remaining
            }
            for future in as_completed(futures):
                task = futures[future]
                try:
                    future.result()
                except Exception as e:
                    # Should not happen (process_and_save catches exceptions)
                    print(f"  UNEXPECTED ERROR on {task.task_id}: {e}",
                          flush=True)

    def _phase_breakdown(self, results: Dict[str, Dict]) -> Dict:
        """Compute breakdown of which phase solved each task."""
        breakdown = {
            "phase1": 0, "pr_cot": 0, "refinement": 0,
            "none": 0, "error": 0,
        }
        for r in results.values():
            phase = r.get("phase_solved", "none")
            if phase in breakdown:
                breakdown[phase] += 1
            else:
                breakdown["none"] += 1
        return breakdown


# --- Main Entry Point ----------------------------------------------------------

def load_lcb_tasks():
    """Load LiveCodeBench dataset."""
    from atlas.bench.datasets import LiveCodeBenchDataset
    ds = LiveCodeBenchDataset()
    ds.load()
    return ds.tasks


def run_v3_benchmark(run_id=None, smoke_only=False, max_tasks=None,
                     enable_phase1=True,
                     enable_phase3=True, selection_strategy="lens"):
    """Run V3 benchmark on LiveCodeBench."""
    if run_id is None:
        run_id = f"v3_run_{datetime.now().strftime('%Y%m%d_%H%M%S')}"

    run_dir = config.results_dir / run_id
    run_dir.mkdir(parents=True, exist_ok=True)

    # Save run metadata
    meta = {
        "run_id": run_id,
        "start_time": datetime.now(timezone.utc).isoformat(),
        "version": "v3",
        "enable_phase1": enable_phase1,
        "enable_phase3": enable_phase3,
        "selection_strategy": selection_strategy,
        "smoke_only": smoke_only,
        "max_tasks": max_tasks,
    }
    atomic_write_json(run_dir / "run_meta.json", meta)

    print("=" * 60)
    print("  ATLAS V3 Benchmark")
    print(f"  Run ID: {run_id}")
    print(f"  Results: {run_dir}")
    print(f"  Phase 1: {'ON' if enable_phase1 else 'OFF'}")
    print(f"  Phase 3: {'ON' if enable_phase3 else 'OFF'}")
    print("=" * 60)

    # Pre-flight checks
    print("\nPre-flight checks...")
    try:
        req = urllib.request.Request(f"{LLAMA_URL}/health")
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode('utf-8'))
        print(f"  llama-server: OK ({data.get('status', '?')})")
    except Exception as e:
        print(f"  llama-server: FAILED ({e})")
        print("  Aborting benchmark — llama-server not reachable")
        sys.exit(1)

    try:
        req = urllib.request.Request(f"{LENS_URL}/health")
        with urllib.request.urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode('utf-8'))
        print(f"  Geometric Lens: OK ({data.get('status', '?')})")
    except Exception:
        print("  Geometric Lens: WARNING — lens scoring unavailable")

    # Check Lens model availability (retry once after short delay)
    lens_ok = False
    for lens_attempt in range(2):
        try:
            test_body = json.dumps({"text": "test"}).encode("utf-8")
            req = urllib.request.Request(
                f"{LENS_URL}/internal/lens/score-text",
                data=test_body,
                headers={"Content-Type": "application/json"},
            )
            with urllib.request.urlopen(req, timeout=15) as resp:
                lens_data = json.loads(resp.read().decode('utf-8'))
            if lens_data.get("error"):
                print(f"  Lens model: NOT LOADED ({lens_data['error']})")
                print("    Probe scoring unavailable — CxGx gate falls back to k=3")
            else:
                print(f"  Lens model: OK (energy={lens_data.get('energy', '?')})")
            lens_ok = True
            break
        except Exception:
            if lens_attempt == 0:
                time.sleep(3)
    if not lens_ok:
        print("  Lens model: UNAVAILABLE — CxGx gate falls back to k=3")

    # Load dataset
    print("\nLoading LiveCodeBench...", end=" ", flush=True)
    tasks = load_lcb_tasks()
    print(f"{len(tasks)} tasks")

    if smoke_only:
        tasks = tasks[:10]
        print(f"  SMOKE MODE: running {len(tasks)} tasks only")
    elif max_tasks:
        tasks = tasks[:max_tasks]
        print(f"  LIMITED MODE: running {len(tasks)} tasks")

    # Run benchmark
    print(f"\nRunning V3 pipeline on {len(tasks)} tasks...")
    print("-" * 60)

    runner = V3BenchmarkRunner(
        run_dir,
        enable_phase1=enable_phase1,
        enable_phase3=enable_phase3,
        selection_strategy=selection_strategy,
    )
    results = runner.run_lcb(tasks)

    # Summary
    passed = sum(1 for r in results.values() if r.get("passed"))
    total = len(results)
    rate = passed / max(total, 1)

    # Phase breakdown
    breakdown = {}
    for r in results.values():
        phase = r.get("phase_solved", "none")
        breakdown[phase] = breakdown.get(phase, 0) + 1

    print("\n" + "=" * 60)
    print("  V3 BENCHMARK COMPLETE")
    print(f"  pass@1: {passed}/{total} ({rate*100:.1f}%)")
    print("  Solved by:")
    for phase, count in sorted(breakdown.items()):
        print(f"    {phase}: {count}")
    print(f"  Results: {run_dir}")
    print("=" * 60)

    # Update metadata
    meta["end_time"] = datetime.now(timezone.utc).isoformat()
    meta["total_tasks"] = total
    meta["passed_tasks"] = passed
    meta["pass_rate"] = rate
    meta["phase_breakdown"] = breakdown
    atomic_write_json(run_dir / "run_meta.json", meta)

    return run_dir


def main():
    import argparse
    parser = argparse.ArgumentParser(description="ATLAS V3 Benchmark Runner")
    parser.add_argument("--run-id", type=str, default=None)
    parser.add_argument("--smoke", action="store_true",
                        help="Smoke test (10 tasks only)")
    parser.add_argument("--max-tasks", type=int, default=None,
                        help="Limit number of tasks")
    parser.add_argument("--no-phase1", action="store_true",
                        help="Disable Phase 1 features")
    parser.add_argument("--no-phase3", action="store_true",
                        help="Disable Phase 3 features")
    parser.add_argument("--baseline", action="store_true",
                        help="Baseline mode: all V3 features OFF (equivalent to V2)")
    parser.add_argument("--selection-strategy", type=str, default="lens",
                        choices=["lens", "random", "logprob", "oracle"],
                        help="Candidate selection strategy (default: lens)")
    args = parser.parse_args()

    if args.baseline:
        args.no_phase1 = True
        args.no_phase3 = True

    run_dir = run_v3_benchmark(
        run_id=args.run_id,
        smoke_only=args.smoke,
        max_tasks=args.max_tasks,
        enable_phase1=not args.no_phase1,
        enable_phase3=not args.no_phase3,
        selection_strategy=args.selection_strategy,
    )

    if run_dir:
        print(f"\nResults saved to: {run_dir}")


if __name__ == "__main__":
    main()
