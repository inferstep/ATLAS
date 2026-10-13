"""Plan generation for /v3/plan: the planning prompt, plan-JSON parsing, the
heuristic plan scorer, and diverse-sample plan selection."""

import json
import os
import re
from typing import Dict, List, Optional, Tuple

import adapters


# --- Plan generation (/v3/plan) ----------------------------------------------
#
# Generates a structured plan for an agent task. Reuses the same LLMAdapter
# the code-generation pipeline uses, but with a planning prompt template and
# a heuristic scorer (V3's lens-based scorer is for code embeddings, not prose
# plans, so it doesn't apply here).
#
# Why bother: when a compact coding model gets a multi-step task without a plan, it
# wanders through 12+ turns of recon before any real work. Forcing the
# model to commit to an ordered set of steps up front cuts the wander to
# zero — even a wrong plan beats no plan, because at least the wrongness
# is visible in one screen instead of buried in a trace.

PLAN_PROMPT_TEMPLATE = """You are an architect. Output ONLY a JSON plan, no other text. No markdown fences. No prose preamble.

User goal: {user_message}
Working directory: {working_dir}

{project_context}

Produce a plan as a SINGLE JSON object:
{{
  "steps": [
    {{"id": "s1", "action": "<concrete action>", "target": "<file path or url>", "why": "<one short sentence>"}},
    ...
  ],
  "verify_step": "<id of the step that verifies the fix works>",
  "rationale": "<one sentence on why this plan shape is right>"
}}

Rules:
- Each step is a single tool call: read_file, write_file, edit_file, structural_edit, delete_file, run_command, list_directory.
- Tool selection guidance:
    * read_file        — inspect a file before editing it
    * write_file       — create a NEW file (rejected for files >5 lines that already exist)
    * edit_file        — small, targeted string change (one function, one block) inside an existing file
    * structural_edit         — replace a WHOLE function, class, or HTML element by selector. Use for any
                         "replace the dashboard function" / "rewrite <body>" / "swap the validate method"
                         step. Selectors: `function:NAME`, `class:NAME`, `<tag>` (.py and .html only).
                         Strongly preferred over edit_file when the change is a whole-unit swap —
                         edit_file truncates on long old_str/new_str pairs (>1.5 KB hits max_tokens
                         mid-string and the JSON parse fails). structural_edit takes no old_str so it
                         doesn't truncate.
    * run_command      — build, test, run, curl. Verifies behavior.
    * delete_file      — remove a file
    * list_directory   — list a directory's contents
- The verify_step MUST exercise a behaviour the user asked for, against the running program: a request that adds or reads data (curl -X POST ... then a GET that shows it), pytest, python <script> that checks its output, go test, npm test, cargo test. Starting the server, or fetching the front page and reading a status code, is setup and does NOT verify anything. ls / cat / grep do NOT verify; they only inspect.
- Minimum 2 steps, maximum 6. Tighter is better.
- Cover EVERY explicit ask in the user goal: each feature, any persistence or networking the user
  asked for, and any deliverable they asked for (how to run it, a README, tests). A plan that leaves
  a stated ask out is wrong, even if it is tighter.
- Address the user's STATED problem only. Don't add unrelated work, don't re-architect.
- For "fix" intents, the plan shape should be: investigate (1 step) → change (1-3 steps) → verify (1 step).

JSON plan:"""


def _build_plan_prompt(user_message: str, working_dir: str,
                       project_context: Dict[str, str],
                       existing_files: Optional[List[str]] = None) -> str:
    """Render the planning prompt with project files inlined (truncated).

    `existing_files` is listed by NAME so the planner can see what is already
    there without paying for the content. Scoring a bad plan down only helps
    if some candidate is better; naming the files stops all three proposing
    the same "create the input data" opening step.
    """
    if project_context:
        ctx_lines = ["Files in project:"]
        for path, content in project_context.items():
            preview = content[:200]
            if len(content) > 200:
                preview += "\n..."
            ctx_lines.append(f"### {path}\n```\n{preview}\n```")
        ctx_str = "\n".join(ctx_lines)
    else:
        ctx_str = "(no project files inspected yet)"

    if existing_files:
        shown = sorted(existing_files)[:60]
        more = "" if len(existing_files) <= 60 else f" (+{len(existing_files)-60} more)"
        ctx_str += (
            "\n\nFiles that ALREADY EXIST in the workspace" + more + ":\n  "
            + "\n  ".join(shown)
            + "\n\nDo not plan to create any of these. They are already there — a step "
              "that writes one would overwrite it. If the task needs data from one, the "
              "code you plan should READ it at runtime. To change one, plan an edit, not "
              "a write."
        )
    return PLAN_PROMPT_TEMPLATE.format(
        user_message=user_message,
        working_dir=working_dir,
        project_context=ctx_str,
    )


def _parse_plan_json(raw: str) -> Optional[dict]:
    """Extract a plan dict from raw LLM output. Tolerates leading/trailing
    prose and markdown fences — the agent's output sanitizer normally
    strips these for tool args, but plans cross the wire as raw text so
    we strip here too."""
    if not raw:
        return None
    # Strip ```json ... ``` fences if present.
    fence_match = re.search(r"```(?:json)?\s*\n(.*?)\n```", raw, re.DOTALL)
    if fence_match:
        raw = fence_match.group(1)
    # Find the first {...} block — model sometimes prefixes "Here's the plan:".
    brace_start = raw.find("{")
    if brace_start < 0:
        return None
    # Scan to matching closing brace (depth-aware, ignores braces in strings
    # to be safe — though plan JSON shouldn't have embedded strings with `{`).
    depth = 0
    in_str = False
    escape = False
    end = -1
    for i in range(brace_start, len(raw)):
        c = raw[i]
        if escape:
            escape = False
            continue
        if c == "\\":
            escape = True
            continue
        if c == '"':
            in_str = not in_str
            continue
        if in_str:
            continue
        if c == "{":
            depth += 1
        elif c == "}":
            depth -= 1
            if depth == 0:
                end = i + 1
                break
    if end < 0:
        return None
    try:
        return json.loads(raw[brace_start:end])
    except (json.JSONDecodeError, ValueError):
        return None


# What a plan's verify step has to do to earn the verification credit: run
# the program or its tests, or fetch a page. The agent loop counts nothing
# else as verification, and proxy/command_evidence.go is the authority on
# that; this is only a heuristic for ranking plans. A build, a parse or a
# linter shows the code is well formed, not that it works, so a step made of
# nothing else earns no credit.
_VERIFY_CMD_RE = re.compile(
    r"\b(pytest|python3?|node|deno|bun|java|php|ruby|bash|curl|wget|https?|httpie)\b"
    r"|\b(cargo|go|dotnet|swift)\s+(run|test)\b"
    r"|\b(npm|yarn|pnpm)\s+(test|start)\b"
    r"|\bmake\s+(test|check|run)\b"
    r"|(^|\s)\./"
)
_STATIC_CHECK_RE = re.compile(
    r"py_compile|compileall|--check\b|--version\b"
    r"|\b(mypy|ruff|pylint|flake8|pyflakes|black|isort|tsc|eslint|prettier|stylelint"
    r"|markdownlint|shellcheck|hadolint|rubocop|golangci-lint|gofmt|javac|kotlinc|gcc|clang)\b"
    r"|\b(go|cargo)\s+(build|vet|check|fmt)\b"
    r"|\b(npm|yarn|pnpm)\s+(run\s+)?(build|lint)\b"
    r"|\bphp\s+-l\b|\bbash\s+-n\b"
)


def _verify_step_verifies(action: str) -> bool:
    """A segment of the step runs something, and is not itself a static check."""
    for seg in re.split(r"&&|\|\||;|\|", action.lower()):
        if _VERIFY_CMD_RE.search(seg) and not _STATIC_CHECK_RE.search(seg):
            return True
    return False


# Plan actions that CREATE a file. A step that creates something already on
# disk is not a plan, it is a data-loss bug waiting for the model to execute
# it — the edit tools exist for changing a file that is already there.
_CREATE_ACTIONS = ("write_file", "create", "generate")


def _strip_dot_slash(path: str) -> str:
    """`path` without its leading `./`, repeated, and nothing else. Not
    `lstrip("./")`, which strips a set of characters and so also ate the dot
    of a hidden name: `.env` became `env` (#277)."""
    return re.sub(r"^(?:\./)+", "", path)


def _known_files(project_context: Dict[str, str], existing_files: Optional[List[str]]) -> set:
    """Relative paths already present: the proxy's listing and the context it
    shipped. This service has no workspace mount, so it lists no directory."""
    return {_strip_dot_slash(f) for f in [*(project_context or {}), *(existing_files or [])]}



# ---------------------------------------------------------------------------
# Deterministic plan normalisation
#
# Audited on the literal prompt "Build me a snake game.", the planner produced:
#   s1 write_file index.html | s2 write_file game.js | s3 write_file style.css
#   s4 edit_file  index.html  "Link the CSS and JS files to the HTML document."
#   s5 run_command "python3 -m http.server 8000"   <- verify_step
#
# Two defects, both deterministic and both fixable without a model call:
#
#   * s4 manufactures an edit. index.html is greenfield in this same plan, and
#     the CSS/JS paths are known at planning time, so the links belong in s1's
#     initial write. The split creates an exact-span edit for a quantized model
#     that measurably cannot perform them -- one dogfood session died looping
#     on precisely this edit_file/index.html pair.
#   * s5 is setup, not verification. Starting a server proves the server
#     starts; it cannot fail when the application is inert. The plan prompt
#     invites this by listing `curl` as a valid verification command, and a
#     session duly shipped a broken game as "verified" off `curl -I`.
#
# Normalisation runs before scoring so a plan cannot win on structure it only
# has because it split a file write in two.

_SERVER_START_RE = re.compile(
    r"\b(http\.server|python\s+-m\s+http|flask\s+run|npm\s+(run\s+)?(start|dev)|"
    r"serve\b|uvicorn|gunicorn|rails\s+server|php\s+-S)", re.I)
# NOTE: filenames are deliberately NOT matched here. `app.py` and `server.py`
# say nothing on their own — a CLI application named app.py is verified by
# running it, and penalising `python app.py` would punish the correct plan.
# Only unambiguous server COMMANDS above, plus stated intent below.

# The command form is not enough: a plan that runs its own `server.py` reads
# as an ordinary script invocation. What gives it away is the INTENT, and the
# planner states it plainly in `why` — "Start the server to verify the game
# loads". Observed live after the first fix landed, which is why this reads
# the rationale as well as the command.
#
# Word order is not intent. "Start the server to verify" and "Verify the
# server starts" say the same thing, and three winning plans in a row said it
# the second way -- "Verify the application starts without errors", "Verify
# the server starts successfully" -- and were scored as real verification while
# their losing siblings, phrased "Start the server...", were flagged
# (acceptance runs, 2026-09-15). Both orders match; "application" and
# "service" join the nouns. Still nothing matches on a filename.
_START_NOUNS = r"(server|app|application|service|site|page|game)"
_START_VERBS = r"(start|starts|started|launch|launches|host|hosts|spin\s*up|spins\s*up|serve|serves|is\s+running|comes?\s+up)"
_START_INTENT_RE = re.compile(
    r"\b" + _START_VERBS + r"\b[^.]{0,40}\b" + _START_NOUNS + r"\b"
    r"|\b" + _START_NOUNS + r"\b[^.]{0,40}\b" + _START_VERBS + r"\b", re.I)


def _is_server_start(step: dict) -> bool:
    cmd = f"{step.get('action','')} {step.get('target','')}"
    if _SERVER_START_RE.search(cmd):
        return True
    # Intent phrasing only counts on a step that actually runs something.
    if (step.get("action") or "").strip().lower() in ("run_command", "run_background"):
        return bool(_START_INTENT_RE.search(step.get("why") or ""))
    return False


def normalize_plan(plan: dict) -> Tuple[dict, List[str]]:
    """Collapse manufactured edits and mark setup-only verification.

    Returns (plan, notes). Pure and deterministic — no model call.
    """
    notes: List[str] = []
    steps = plan.get("steps")
    if not isinstance(steps, list) or not steps:
        return plan, notes

    # 1. write_file X ... edit_file X  ->  one write_file X.
    #    Only when the write comes FIRST in this same plan (the artifact is
    #    greenfield here), so an edit to a pre-existing file is untouched.
    written: Dict[str, dict] = {}
    keep: List[dict] = []
    for s in steps:
        if not isinstance(s, dict):
            keep.append(s)
            continue
        action = (s.get("action") or "").strip().lower()
        target = (s.get("target") or "").strip()
        if action == "write_file" and target:
            written[target] = s
            keep.append(s)
            continue
        if action in ("edit_file", "structural_edit") and target in written:
            base = written[target]
            base["why"] = (base.get("why", "").rstrip(". ")
                           + ". Also: " + (s.get("why") or "").strip())
            notes.append(
                f"collapsed {action} on {target} into its initial write "
                f"(greenfield artifact — everything known at planning time "
                f"belongs in the first write)")
            continue
        keep.append(s)

    if len(keep) != len(steps):
        plan = dict(plan)
        plan["steps"] = keep
        # The verify_step id may have pointed at a collapsed step.
        ids = {s.get("id") for s in keep if isinstance(s, dict)}
        if plan.get("verify_step") not in ids:
            for s in reversed(keep):
                if isinstance(s, dict) and (s.get("action") or "").lower() == "run_command":
                    plan["verify_step"] = s.get("id")
                    break

    # 2. Flag a verify_step that only starts a server.
    vid = plan.get("verify_step")
    for s in plan.get("steps", []):
        if isinstance(s, dict) and s.get("id") == vid and _is_server_start(s):
            notes.append(
                f"verify_step {vid} only starts a server, which is setup and "
                f"cannot fail on an inert application")
            plan = dict(plan)
            plan["verify_is_setup_only"] = True
            break
    return plan, notes


def _score_plan(plan: dict, user_message: str,  # noqa: C901
                existing_files: set = frozenset()) -> Tuple[float, List[str]]:
    """Heuristic plan scorer. Returns (score in [0,1], reasons[]).

    Plans aren't sandbox-buildable so the lens doesn't help us pick a
    winner. Instead we check structural properties that correlate with
    "this plan will actually solve the user's problem":
      - has a verify_step
      - step count is in [2, 6]
      - verify_step's action runs an actual verification command
      - target paths reference files the user named
      - rationale is present

    Reasons are returned alongside the score so the picker can stream
    "plan #2 won because: has verify, target matches" to the TUI.
    """
    reasons: List[str] = []
    score = 0.0

    steps = plan.get("steps") or []
    if not isinstance(steps, list):
        return 0.0, ["steps is not a list"]

    n = len(steps)
    if 2 <= n <= 6:
        score += 0.2
        reasons.append(f"step count {n} in range")
    elif n > 0:
        # A 1-step or 7+ step plan is a yellow flag, not a fail.
        score += 0.05
        reasons.append(f"step count {n} outside [2,6]")
    else:
        return 0.0, ["empty plan"]

    verify_step_id = plan.get("verify_step")
    verify_step = None
    for s in steps:
        if isinstance(s, dict) and s.get("id") == verify_step_id:
            verify_step = s
            break
    if verify_step is not None:
        score += 0.3
        reasons.append(f"verify_step={verify_step_id}")
        action = (verify_step.get("action") or "") + " " + (verify_step.get("target") or "")
        if plan.get("verify_is_setup_only"):
            score -= 0.3
            reasons.append("verify_step only starts a server — setup, not verification")
        elif _verify_step_verifies(action):
            score += 0.2
            reasons.append("verify_step references a real verification command")
        elif _STATIC_CHECK_RE.search(action.lower()):
            reasons.append("verify_step only checks that the code is well formed — not verification")
        else:
            reasons.append("verify_step doesn't reference a verification command")
    else:
        reasons.append("missing or invalid verify_step")

    # Planning to CREATE a file that already exists. Measured on aoc_sonar:
    # the winning plan's step 1 was `write_file input.txt` — "create the
    # necessary input data" — against a 2000-line fixture already on disk.
    # The model then tried to retype it from memory, degenerated into
    # repeating one line, had its stream cut mid-JSON, and the run died on
    # three unparseable responses. A sibling task executed the same step
    # successfully and corrupted the fixture. The plan scored 1.00 both
    # times, because nothing here looked at what was already there.
    clobbered = []
    for st in steps:
        if not isinstance(st, dict):
            continue
        action = (st.get("action") or "").lower()
        target = _strip_dot_slash((st.get("target") or "").strip())
        if not target or not any(a in action for a in _CREATE_ACTIONS):
            continue
        if target in existing_files:
            clobbered.append(target)
    if clobbered:
        # Heavy: a plan that opens by overwriting existing input is worse
        # than a vaguer plan that does not.
        score -= 0.5
        reasons.append(
            "plans to create file(s) that already exist: "
            + ", ".join(sorted(set(clobbered))[:3])
            + " — edit them instead of recreating them")

    # Target-vs-user-message overlap. If the user said "fix index.html",
    # plans that touch index.html beat plans that don't.
    # Length bounds (max 128 for the stem, max 16 for the extension) cap
    # regex backtracking on pathological input like "a." repeated many
    # times — both groups need `.` so without bounds this is polynomial.
    mentioned_files = set(re.findall(
        r"[\w./-]{1,128}\.[a-zA-Z0-9]{1,16}", user_message.lower()))
    target_hits = 0
    for s in steps:
        if not isinstance(s, dict):
            continue
        target = (s.get("target") or "").lower()
        for f in mentioned_files:
            if f in target:
                target_hits += 1
                break
    if mentioned_files:
        target_score = min(0.2, target_hits * 0.1)
        score += target_score
        if target_hits:
            reasons.append(f"{target_hits} step(s) target user-mentioned files")

    if plan.get("rationale"):
        score += 0.1
        reasons.append("rationale present")

    return min(score, 1.0), reasons


def generate_plan(
    user_message: str,
    working_dir: str,
    project_context: Dict[str, str],
    existing_files: Optional[List[str]] = None,
    n_candidates: int = 3,
    progress_callback=None,
    cancel_scope=None,
    request_identity=None,
) -> dict:
    """Generate a plan via diverse LLM sampling + heuristic scoring.

    Returns a dict matching the proxy's expected schema:
      {
        "steps": [...],
        "verify_step": "sN",
        "candidates_tested": int,
        "winning_score": float,
        "winning_index": int,
        "rationale": str,
        "reasons": [str],  # why the winner won
      }

    On total failure (no candidate parses), returns a single-step
    fallback plan that asks the model to plan inline. Better than
    blocking the agent loop on planner-pipeline errors.
    """

    def emit(stage: str, detail: str = "", **data):
        if progress_callback:
            try:
                progress_callback(stage, detail, **data)
            except TypeError:
                progress_callback(stage, detail)

    emit("plan_start", f"generating {n_candidates} candidate plans")

    # PC-206: thinking-aware infrastructure shipped — planner CAN run with
    # Template-level reasoning ON via ATLAS_PLAN_THINKING=1. Default is OFF
    # because empirically on the reference local model + this codebase's
    # hardware tier, thinking pushes planner latency from ~5-30s to >4min
    # per candidate (model spends the full token budget reasoning before
    # emitting JSON). On faster GPU tiers the design's aspirational
    # "reasoning > latency cost" trade may be worth it — flip the env var
    # there. When ON, max_tokens jumps to 8192 to fit reasoning + answer.
    plan_thinking = os.environ.get("ATLAS_PLAN_THINKING", "0").lower() in ("1", "true", "yes")
    plan_max_tokens = 8192 if plan_thinking else 2048
    llm = adapters.LLMAdapter(progress_callback=progress_callback, thinking=plan_thinking)
    # Same request-scoped cancellation as the generate path: a planner call is
    # inference like any other, and an uncancellable one outlives its parent
    # exactly the same way.
    llm.cancel_scope = cancel_scope
    # Same reason as the generate path: the adapter carries the identity its
    # calls are sent under. The planner runs on the request thread today, so
    # nothing here needs a thread hop -- but the adapter no longer falls back
    # to the request-ID ContextVar, so an unset identity here would strip
    # attribution off every /v3/plan generation.
    llm.request_identity = request_identity
    # What is already on disk, as the proxy lists it. Needed before the
    # prompt: naming the files stops all three candidates proposing the same
    # "create the input data" opening step, which scoring alone cannot fix
    # when every candidate shares the flaw.
    existing = _known_files(project_context, existing_files)

    prompt = _build_plan_prompt(user_message, working_dir, project_context, sorted(existing))

    candidates: List[Tuple[Optional[dict], float, List[str]]] = []
    # Diverse sampling via temperature spread. Cheap version of V3's
    # PlanSearch — three samples at 0.3 / 0.5 / 0.7 give us breadth
    # without the full plansearch infrastructure.
    temperatures = [0.3, 0.5, 0.7][:n_candidates]
    for i, temp in enumerate(temperatures):
        emit("plan_candidate", f"candidate {i+1}/{n_candidates} (temp={temp})",
             index=i, temperature=temp)
        try:
            # plan_max_tokens varies with thinking mode (PC-206): 2048 covers
            # a 6-step plan + rationale when thinking is off, 8192 leaves
            # room for ~6KB of reasoning preamble plus the JSON answer.
            raw, tokens, t_ms = llm(prompt, temp, plan_max_tokens, 42 + i)
        except Exception as e:
            emit("plan_candidate_error", f"candidate {i+1} failed: {e}", index=i)
            candidates.append((None, 0.0, [f"llm error: {e}"]))
            continue
        plan = _parse_plan_json(raw)
        if plan is None:
            preview = raw[:500] if raw else "(empty)"
            print(f"  [plan] candidate {i+1} unparseable. raw preview:\n{preview}\n",
                  flush=True)
            emit("plan_candidate_unparseable", f"candidate {i+1} didn't parse",
                 index=i)
            candidates.append((None, 0.0, ["unparseable"]))
            continue
        # Normalise BEFORE scoring, so a plan cannot win on structure it only
        # has because it split one file write into a write plus an edit.
        plan, norm_notes = normalize_plan(plan)
        score, reasons = _score_plan(plan, user_message, existing)
        reasons.extend(norm_notes)
        emit("plan_candidate_scored", f"candidate {i+1} score={score:.2f}",
             index=i, score=score, reasons=reasons)
        candidates.append((plan, score, reasons))

    # Pick winner. Tie-break: shorter plan wins (less waffle).
    best_idx = -1
    best_score = -1.0
    best_steps = 999
    for i, (plan, score, _) in enumerate(candidates):
        if plan is None:
            continue
        n_steps = len(plan.get("steps") or [])
        if score > best_score or (score == best_score and n_steps < best_steps):
            best_score = score
            best_steps = n_steps
            best_idx = i

    # A penalty is not a floor. If the winner still only starts a server, the
    # plan is asserting that setup is verification, and the agent will follow
    # it -- which is exactly how a broken game shipped as "complete and
    # verified". Strip the claim rather than pass it on: the loop's own
    # verification gate then governs, and it demands a real green run.
    if best_idx >= 0:
        winner = candidates[best_idx][0]
        if winner and winner.get("verify_is_setup_only"):
            winner.pop("verify_step", None)
            winner["verify_step_removed"] = "setup-only verification is not verification"
            emit("plan_verify_stripped",
                 "winning plan's verify_step only started a server — removed, "
                 "the agent's verification gate governs instead")

    if best_idx < 0:
        # All candidates failed. Return a minimal fallback so the agent
        # loop doesn't block — the plan-adherence gate will be lenient
        # if it sees this shape.
        emit("plan_failed", "no candidate parsed — returning fallback")
        return {
            "steps": [
                {"id": "s1", "action": "investigate the request and act",
                 "target": working_dir, "why": "planner failed; deferring to agent"},
            ],
            "verify_step": None,
            "candidates_tested": len(candidates),
            "winning_score": 0.0,
            "winning_index": -1,
            "rationale": "planner-pipeline fallback (no parseable candidate)",
            "reasons": ["all candidates failed to parse"],
        }

    plan, score, reasons = candidates[best_idx]
    plan["candidates_tested"] = len([c for c in candidates if c[0] is not None])
    plan["winning_score"] = score
    plan["winning_index"] = best_idx
    plan["reasons"] = reasons
    emit("plan_selected", f"plan {best_idx+1} won (score={score:.2f})",
         index=best_idx, score=score, steps=len(plan.get("steps") or []))
    return plan
