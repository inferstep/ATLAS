"""Candidate verification and scoring for the V3 pipeline: geometric-lens
scoring, task-type classification, language-aware smoke checks, build-command
verification, and the interactive-task lint."""

import json
import os
import urllib.error
import urllib.request
from pathlib import PurePath
from typing import Any, Dict, List, Optional, Tuple

import adapters
from stages.candidate_selection import NONFINITE_SCORE, finite_score


# --- Lens Scorer (calls Geometric Lens) ---------------------------------------------
#
# A Lens answer either scores or says why it did not. The failure is typed
# (`failure.kind`: embed_capacity with the server's token counts,
# model_server_error, model_server_unreachable, ...) and is carried into the
# candidate record. It is never turned into a number: an unscored candidate
# has no energy, no normalized energy and no G(x) score, so the min-energy
# selector cannot rank it first and the allocator cannot read it as neutral.
# A Lens without the typed boundary answered every failure with energy 0.0 /
# gx 0.5 / verdict "error"; that shape is read as unscored too, and so is
# any score field that is not a finite number (json.loads accepts NaN and
# Infinity, and reads 1e999 as an infinity).

LENS_ERROR = "lens_error"
LENS_UNREACHABLE = "lens_unreachable"
TOKEN_ASSERTION_ERROR = "token_assertion_error"
TOKEN_CAPACITY = "embed_capacity"

SCORING_CAPACITY_ENV = "ATLAS_LENS_SCORING_CAPACITY_TOKENS"
SCORING_MARGIN_ENV = "ATLAS_LENS_SCORING_MARGIN_TOKENS"


class TokenCapacityExceeded(RuntimeError):
    def __init__(self, input_tokens: int, capacity_tokens: int,
                 margin_tokens: int, max_input_tokens: int):
        self.input_tokens = input_tokens
        self.capacity_tokens = capacity_tokens
        self.margin_tokens = margin_tokens
        self.max_input_tokens = max_input_tokens
        super().__init__(
            f"candidate has {input_tokens} tokens; diagnostic scoring bound is "
            f"{max_input_tokens} ({capacity_tokens} qualified minus {margin_tokens} margin)"
        )


class LensUnavailable(RuntimeError):
    """The lens cannot score: unreachable, no model loaded, a broken
    answer, or its model server down. The lens is required
    (docs/adr/0011-the-lens-is-required.md), so the run stops on this rather
    than ranking candidates on neutral scores. An input the lens declines
    (embed_capacity, empty, non-finite) is not this: that candidate is
    unscored and the run goes on."""

    def __init__(self, reason: str):
        super().__init__(reason)
        self.reason = reason


# Typed lens failures that describe the lens or its model server rather than
# the input. The lens names them (geometric_lens/embed_capacity.py).
LENS_DOWN_KINDS = frozenset({"model_server_error", "model_server_unreachable",
                             "embedding_contract", "internal"})


def _raise_if_lens_down(failure: Optional[Dict[str, Any]]) -> None:
    if failure and failure.get("kind") in LENS_DOWN_KINDS:
        raise LensUnavailable(describe_lens_failure(failure))


def _lens_down_from_exception(exc: BaseException) -> Optional[LensUnavailable]:
    """The LensUnavailable an exception from a lens call means, or None when
    it is not about the lens (a defect on this side, kept as unscored)."""
    if isinstance(exc, (urllib.error.URLError, OSError, TimeoutError,
                        ConnectionError, ValueError)):
        # URLError covers HTTPError; ValueError covers a non-JSON answer.
        return LensUnavailable(describe_lens_failure(_failure_from_exception(exc)))
    return None


def _lens_failure(data: Dict[str, Any]) -> Optional[Dict[str, Any]]:
    """The typed failure a Lens answer carries, or None when it scored."""
    failure = data.get("failure")
    if isinstance(failure, dict) and failure.get("kind"):
        return dict(failure)
    if data.get("scored") is False or data.get("error") or data.get("verdict") == "error":
        detail = data.get("error") or data.get("verdict") or "unscored"
        return {"kind": LENS_ERROR, "detail": str(detail)[:200]}
    return None


def _failure_from_exception(exc: BaseException) -> Dict[str, Any]:
    if isinstance(exc, urllib.error.HTTPError):
        return {"kind": LENS_ERROR, "status": int(exc.code),
                "detail": f"HTTP {exc.code} from the lens"}
    if isinstance(exc, (urllib.error.URLError, OSError, TimeoutError, ConnectionError)):
        return {"kind": LENS_UNREACHABLE, "detail": type(exc).__name__}
    return {"kind": LENS_ERROR, "detail": type(exc).__name__}


def _nonfinite(field: str, value) -> Dict[str, Any]:
    return {"kind": NONFINITE_SCORE, "field": field,
            "detail": f"{field} is not a finite number: {value!r}"[:200]}


def _finite_fields(data: Dict[str, Any], fields, default=None) -> Optional[Dict[str, Any]]:
    """The failure for the first of `fields` whose value is present and
    not a finite number, or None when every one is a score. An absent
    field takes `default` (the pre-typed-boundary answer had no G(x))."""
    for field in fields:
        value = data.get(field, default)
        if finite_score(value) is None:
            return _nonfinite(field, value)
    return None


def describe_lens_failure(failure: Dict[str, Any]) -> str:
    """One line naming why a candidate is unscored, for logs and events."""
    if failure.get("kind") == "embed_capacity":
        return (f"embed_capacity: input {failure.get('input_tokens')} tokens, "
                f"physical batch {failure.get('capacity_tokens')} tokens")
    return f"{failure.get('kind')}: {failure.get('detail') or ''}".rstrip(": ")


def _positive_env(name: str) -> Optional[int]:
    raw = os.environ.get(name)
    if raw is None:
        return None
    try:
        value = int(raw)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be a positive integer") from exc
    if value <= 0 or str(value) != raw.strip():
        raise ValueError(f"{name} must be a canonical positive integer")
    return value


def candidate_token_assertion(code: str) -> Optional[Dict[str, int]]:
    """Count candidate bytes with the serving model's tokenizer before Lens scoring.

    Ordinary deployments leave both variables unset and keep the existing one-call
    scoring path. A frozen diagnostic sets both. Its declared safety margin is
    subtracted from the independently qualified embedding capacity; malformed or
    incomplete configuration fails closed. The tokenizer request uses the same
    model endpoint and request/invocation identity as generation and Lens traffic.
    """
    capacity_raw = os.environ.get(SCORING_CAPACITY_ENV)
    margin_raw = os.environ.get(SCORING_MARGIN_ENV)
    if capacity_raw is None and margin_raw is None:
        return None
    if capacity_raw is None or margin_raw is None:
        raise ValueError(f"{SCORING_CAPACITY_ENV} and {SCORING_MARGIN_ENV} must be set together")
    capacity = _positive_env(SCORING_CAPACITY_ENV)
    margin = _positive_env(SCORING_MARGIN_ENV)
    assert capacity is not None and margin is not None
    if margin >= capacity:
        raise ValueError("Lens scoring margin must be smaller than qualified capacity")

    body = json.dumps({"content": code, "add_special": True}).encode()
    req = urllib.request.Request(
        f"{adapters.INFERENCE_URL}/tokenize",
        data=body,
        headers=adapters._service_headers(),
    )
    with urllib.request.urlopen(req, timeout=30) as resp:
        data = json.loads(resp.read())
    tokens = data.get("tokens") if isinstance(data, dict) else None
    if not isinstance(tokens, list) or any(isinstance(t, bool) or not isinstance(t, int) for t in tokens):
        raise ValueError("model tokenizer returned a malformed token list")
    count = len(tokens)
    maximum = capacity - margin
    if count > maximum:
        raise TokenCapacityExceeded(count, capacity, margin, maximum)
    return {"input_tokens": count, "capacity_tokens": capacity,
            "margin_tokens": margin, "max_input_tokens": maximum}


def _token_assertion_failure(exc: BaseException) -> Dict[str, Any]:
    if isinstance(exc, TokenCapacityExceeded):
        return {"kind": TOKEN_CAPACITY,
                "input_tokens": exc.input_tokens,
                "capacity_tokens": exc.capacity_tokens,
                "margin_tokens": exc.margin_tokens,
                "max_input_tokens": exc.max_input_tokens,
                "detail": str(exc)[:200], "stage": "pre_lens_token_assertion"}
    failure = _failure_from_exception(exc)
    return {"kind": TOKEN_ASSERTION_ERROR,
            "detail": failure.get("detail") or type(exc).__name__,
            "stage": "pre_lens_token_assertion"}


def score_candidate_per_step(code: str) -> dict:
    """PC-207 wiring: per-step C(x)+G(x) scoring of a candidate.

    Returns the aggregate dict from `/internal/lens/score-per-step`
    (`first_off_rails_idx`, `gx_score_min`, `gx_score_mean`, etc.)
    plus `n_tokens`. A disabled Lens yields an empty dict (no per-step
    signal); a Lens that could not score, or could not be reached, yields
    `{"failure": {...}}` and nothing else, so no default can be read as a
    verdict. Never raises: a lens outage must not stop the pipeline.

    Cost on this hardware tier: ~7-15ms per token (lens batches the
    MLP + XGBoost calls), so a 500-token candidate adds ~3-7 seconds
    of latency. Worth it for the off-rails detection signal — see
    PC-207 in ISSUES.md for the empirical case (the May 6 53-min
    repetition loop would have been visible at first_off_rails_idx<5).
    """
    try:
        try:
            token_assertion = candidate_token_assertion(code)
        except Exception as exc:
            failure = _token_assertion_failure(exc)
            print(f"  [lens] per-step unscored ({describe_lens_failure(failure)})", flush=True)
            return {"failure": failure}
        body = json.dumps({"text": code}).encode()
        req = urllib.request.Request(
            f"{adapters.LENS_URL}/internal/lens/score-per-step",
            data=body,
            headers=adapters._service_headers(),
        )
        with urllib.request.urlopen(req, timeout=60) as resp:
            data = json.loads(resp.read())
        if not data.get("enabled"):
            raise LensUnavailable("the lens has no model loaded")
        failure = _lens_failure(data)
        _raise_if_lens_down(failure)
        if failure is None and not int(data.get("n_tokens", 0)):
            # 200 with an empty aggregate and no stated reason: the
            # defaults below would hand back gx=0.500 for every candidate,
            # a tie that reads exactly like a real verdict.
            failure = {"kind": LENS_ERROR, "detail": "no tokens scored"}
        agg = data.get("aggregate", {}) or {}
        if failure is None:
            failure = _finite_fields(
                agg, ("gx_score_min", "gx_score_mean"), default=0.5) or \
                _finite_fields(agg, ("cx_norm_max", "cx_norm_mean"), default=0.0)
        if failure is not None:
            print(f"  [lens] per-step unscored ({describe_lens_failure(failure)})", flush=True)
            return {"failure": failure}
        thresholds = data.get("thresholds")
        if not (
            isinstance(thresholds, dict)
            and all(isinstance(thresholds.get(k), (int, float))
                    for k in ("off_rails", "low", "severe"))
        ):
            thresholds = None
        result = {
            "n_tokens":            int(data.get("n_tokens", 0)),
            "gx_available":        bool(data.get("gx_available", False)),
            "first_off_rails_idx": int(agg.get("first_off_rails_idx", -1)),
            "gx_score_min":        float(agg.get("gx_score_min", 0.5)),
            "gx_score_mean":       float(agg.get("gx_score_mean", 0.5)),
            "cx_norm_max":         float(agg.get("cx_norm_max", 0.0)),
            "cx_norm_mean":        float(agg.get("cx_norm_mean", 0.0)),
            "latency_ms":          float(data.get("latency_ms", 0.0)),
            "thresholds":          thresholds,
        }
        if token_assertion is not None:
            result["token_assertion"] = token_assertion
        print(
            f"  [lens] candidate scored: n_tok={result['n_tokens']} "
            f"gx_min={result['gx_score_min']:.3f} gx_mean={result['gx_score_mean']:.3f} "
            f"off_rails={result['first_off_rails_idx']} lat={result['latency_ms']:.0f}ms",
            flush=True,
        )
        return result
    except LensUnavailable:
        raise
    except Exception as e:
        down = _lens_down_from_exception(e)
        if down is not None:
            raise down from e
        failure = _failure_from_exception(e)
        print(f"  [lens] per-step unscored ({describe_lens_failure(failure)})", flush=True)
        return {"failure": failure}


# The probe's scores before it is scored: a run with no probe code has no
# signal, and the allocator keeps its floor. A lens that cannot score raises
# LensUnavailable; it is never answered with these.
NEUTRAL_COMBINED = {
    "cx_energy": 0.0, "cx_normalized": 0.5, "cx_calibrated": False,
    "gx_score": 0.5, "gx_available": False, "verdict": "unavailable",
}

# The answer for a candidate the Lens did not score: no number anywhere.
UNSCORED_COMBINED = {
    "cx_energy": None, "cx_normalized": None, "cx_calibrated": False,
    "gx_score": None, "gx_available": False, "verdict": "unscored",
}


def _unscored(failure: Dict[str, Any]) -> Dict[str, Any]:
    print(f"  [lens] candidate unscored ({describe_lens_failure(failure)})", flush=True)
    return {**UNSCORED_COMBINED, "failure": failure}


def score_candidate_combined(code: str) -> Dict[str, Any]:
    """Score code with Geometric Lens C(x) AND G(x) in one call.

    `/internal/lens/gx-score` extracts the embedding once and runs both
    models on it, so the pair costs no more than C(x) alone. Returns
    ``cx_energy``, ``cx_normalized``, ``cx_calibrated``, ``gx_score``,
    ``gx_available`` and ``verdict``; the CxGx allocation gate reads all
    six, everything else reads the C(x) three through score_candidate.

    A lens that cannot score (no model loaded, unreachable, a malformed
    answer, its model server down) raises ``LensUnavailable``: the
    lens is required. An input the lens declines (the embedding server's
    physical batch, an empty input, a non-finite score) yields
    ``UNSCORED_COMBINED`` plus the typed ``failure``: every score field is
    None, ``verdict`` is ``"unscored"``.

    Timeout note: 10s was tight under load — the lens shares the box with
    V3's streaming generator and llama-server, and a single hot probe
    could starve scoring long enough to trip the fallback. Bumped to 30s
    so transient contention doesn't masquerade as a broken lens
    (symptom: C(x)=0.00 / gx=0.50 sentinel pair).
    """
    try:
        try:
            token_assertion = candidate_token_assertion(code)
        except Exception as exc:
            return _unscored(_token_assertion_failure(exc))
        body = json.dumps({"text": code}).encode()
        req = urllib.request.Request(
            f"{adapters.LENS_URL}/internal/lens/gx-score",
            data=body,
            headers=adapters._service_headers(),
        )
        with urllib.request.urlopen(req, timeout=30) as resp:
            data = json.loads(resp.read())
        if not isinstance(data, dict):
            raise LensUnavailable("the lens answered a malformed body")
        if not data.get("enabled", False):
            raise LensUnavailable("the lens has no model loaded")
        failure = _lens_failure(data)
        _raise_if_lens_down(failure)
        if failure is not None:
            return _unscored(failure)
        if "cx_energy" not in data:
            raise LensUnavailable("the lens answered with no C(x) energy")
        failure = _finite_fields(data, ("cx_energy", "cx_normalized", "gx_score"),
                                 default=0.5)
        if failure is not None:
            return _unscored(failure)
        result = {
            "cx_energy": finite_score(data["cx_energy"]),
            "cx_normalized": finite_score(data.get("cx_normalized", 0.5)),
            "cx_calibrated": bool(data.get("cx_calibrated", False)),
            "gx_score": finite_score(data.get("gx_score", 0.5)),
            "gx_available": bool(data.get("gx_available", False)),
            "verdict": data.get("verdict", "unavailable"),
        }
        if token_assertion is not None:
            result["token_assertion"] = token_assertion
        return result
    except LensUnavailable:
        raise
    except Exception as e:
        down = _lens_down_from_exception(e)
        if down is not None:
            raise down from e
        return _unscored(_failure_from_exception(e))


class LensEnergy(tuple):
    """``(raw_energy, normalized_energy, calibrated)`` that also names why it
    is unscored. Unpacks like the plain tuple it replaces; ``failure`` is
    None for a scored candidate."""

    failure: Optional[Dict[str, Any]] = None
    token_assertion: Optional[Dict[str, int]] = None

    def __new__(cls, energy, normalized, calibrated, failure=None,
                token_assertion=None):
        self = super().__new__(cls, (energy, normalized, calibrated))
        self.failure = failure
        self.token_assertion = token_assertion
        return self


def score_candidate(code: str) -> Tuple[Optional[float], Optional[float], bool]:
    """Score code with Geometric Lens C(x).

    Returns ``(raw_energy, normalized_energy, calibrated)``. The normalized
    value is neutral when this model has no calibration and must not drive
    adaptive routing in that case. Both energies are None, and
    ``.failure`` is set, when the Lens did not score the candidate.
    """
    d = score_candidate_combined(code)
    return LensEnergy(d["cx_energy"], d["cx_normalized"], d["cx_calibrated"],
                      d.get("failure"), d.get("token_assertion"))


# --- Task-type classifier (PC-022) -------------------------------------------

_INTERACTIVE_MARKERS = (
    "game", "tui", "terminal interface", "menu", "interactive",
    "pygame", "curses", "tkinter", "flask", "fastapi", "django",
    "streamlit", "gradio", "dashboard", "gui", "web app", "webapp",
    "cli tool", "command-line tool", "chat bot", "chatbot",
    "discord bot", "telegram bot", "snake", "tetris", "pong",
    "rpg", "shell", "repl", "live server", "scraper", "crawler",
    "watcher", "daemon",
)
_ALGORITHMIC_MARKERS = (
    "input:", "output:", "examples:", "sample input", "sample output",
    "constraints:", "test case", "leetcode", "codeforces", "hackerrank",
    "competitive programming", "function signature", "given an array",
    "given a string", "return the", "return an integer", "modulo 10",
)


def classify_task_type(problem: str) -> str:
    """Classify whether a task expects (input -> output) self-tests.

    Returns 'algorithmic' for problems with clear I/O contracts (the
    LiveCodeBench shape — synthesized self-tests are meaningful), or
    'interactive' for games/UIs/scripts/library code where I/O self-tests
    don't apply and would produce false failures (PC-022).
    """
    p = problem.lower()
    interactive_hits = sum(1 for m in _INTERACTIVE_MARKERS if m in p)
    algorithmic_hits = sum(1 for m in _ALGORITHMIC_MARKERS if m in p)
    if interactive_hits > 0 and interactive_hits >= algorithmic_hits:
        return "interactive"
    return "algorithmic"


def smoke_compile_check(code: str, sandbox, language: str = "python", filename: str = "") -> Tuple[bool, str, str]:
    """Lightweight verification for interactive tasks: code parses + compiles.

    Replaces synthetic-I/O self-tests for tasks where (input -> output)
    pairs are nonsensical (curses games, pygame apps, flask servers, …).

    PC-048: language-aware. Everything the sandbox /syntax-check endpoint
    covers goes through it; anything else fails explicitly instead of
    being accepted without evidence.
    """
    lang = (language or "python").lower()

    verified_languages = {
        "python", "py", "javascript", "typescript", "go", "java", "kotlin",
        "rust", "c", "cpp", "ruby", "php", "bash", "html", "htm", "xml", "json", "yaml", "yml",
    }
    if lang not in verified_languages or not hasattr(sandbox, "syntax_check"):
        return False, "", f"syntax verification unavailable for language: {lang}"
    normalized = {
        "py": "python", "htm": "html", "yml": "yaml",
    }.get(lang, lang)
    # The filename travels so the sandbox can scope path-dependent checks -- a
    # Jinja-template parse applies to templates/*.html but not to Vue/Angular
    # HTML elsewhere, and it cannot tell them apart from the bytes alone.
    return sandbox.syntax_check(code, normalized, filename)


BUILD_EVIDENCE_LIMIT = 4000
ALLOWED_BUILD_PREFIXES = (
    "npm run build",
    "npm run test",
    "npm test",
    "pnpm run build",
    "pnpm run test",
    "pnpm test",
    "yarn build",
    "yarn test",
    "yarn run build",
    "yarn run test",
    "bun run build",
    "bun run test",
    "bun test",
    "npx tsc --noEmit",
    "npx next build",
    "python -m py_compile",
    "python -m pytest",
    "pytest",
    "go build",
    "go test",
    "cargo build",
    "cargo check",
    "make",
    "cmake --build",
    "bash -n",
)
DISALLOWED_BUILD_TOKENS = (
    ";", "&&", "||", "|", "&", "<", ">", "`", "$(", "\n", "\r", "\x00",
)


def _bounded_evidence(text: str, limit: int = BUILD_EVIDENCE_LIMIT) -> str:
    text = text or ""
    if len(text) <= limit:
        return text
    return text[:limit] + "\n... (truncated)"


def _build_command_allowed(command: str) -> bool:
    command = (command or "").strip()
    if not command or any(token in command for token in DISALLOWED_BUILD_TOKENS):
        return False
    for prefix in ALLOWED_BUILD_PREFIXES:
        if command == prefix or command.startswith(prefix + " "):
            return True
    return False


def _project_relative_path(file_path: str, working_dir: str = "/workspace") -> str:
    if not file_path:
        raise ValueError("file_path is required for build verification")
    path = PurePath(file_path)
    root = PurePath(working_dir or "/workspace")
    if not root.is_absolute() or ".." in root.parts:
        raise ValueError(f"working_dir must be an absolute project root: {working_dir}")
    if path.is_absolute():
        try:
            rel = path.relative_to(root)
        except ValueError as e:
            raise ValueError(f"file_path must be under working_dir: {file_path}") from e
    else:
        rel = path
    if rel.is_absolute() or ".." in rel.parts:
        raise ValueError(f"unsafe file_path for build verification: {file_path}")
    return str(rel)


def verify_build_command(
    code: str,
    sandbox,
    build_command: str,
    file_path: str,
    project_files: Dict[str, str],
    working_dir: str,
    emit=None,
) -> Tuple[bool, str, str, Dict[str, Any]]:
    command = (build_command or "").strip()
    evidence: Dict[str, Any] = {
        "verifier": "build_command",
        "command": command,
        "status": "unavailable",
        "exit_code": None,
        "duration_ms": 0,
        "stdout": "",
        "stderr": "",
    }
    if not command:
        return True, "", "", {}
    if not hasattr(sandbox, "run_command"):
        evidence["stderr"] = "sandbox build command runner is unavailable"
        if emit:
            emit("build_verify_unavailable", evidence["stderr"], command=command)
        return False, "", evidence["stderr"], evidence
    if not _build_command_allowed(command):
        evidence["stderr"] = f"build command is not allowed by verification policy: {command}"
        if emit:
            emit("build_verify_unavailable", evidence["stderr"], command=command)
        return False, "", evidence["stderr"], evidence

    try:
        rel_path = _project_relative_path(file_path, working_dir)
    except ValueError as e:
        evidence["stderr"] = str(e)
        if emit:
            emit("build_verify_unavailable", evidence["stderr"], command=command)
        return False, "", evidence["stderr"], evidence

    # Build verification snapshots the real /workspace inside the sandbox.
    # Do not overlay `project_files` here: that map is prompt context from
    # the proxy and may be intentionally truncated for token budget. Only
    # overlay the candidate under test so full project files remain intact.
    overlay = {rel_path: code}
    ok, out, err, meta = sandbox.run_command(
        command,
        files=overlay,
        cwd=working_dir or "/workspace",
        timeout=60,
    )
    evidence.update({
        "status": "passed" if ok else "failed",
        "exit_code": meta.get("exit_code"),
        "duration_ms": int(meta.get("elapsed_ms") or 0),
        "stdout": _bounded_evidence(out),
        "stderr": _bounded_evidence(err),
    })
    if emit:
        emit(
            "build_verify",
            f"{command}: {'OK' if ok else 'FAIL'}",
            command=command,
            status=evidence["status"],
            exit_code=evidence["exit_code"],
            duration_ms=evidence["duration_ms"],
        )
    if not ok:
        return False, out, err or out or f"build command failed: {command}", evidence
    return True, out, err, evidence


# interactive_lint's reason when it could not parse the code and checked
# nothing.
INTERACTIVE_LINT_NOT_RUN = "not run: the code does not parse in this interpreter"


def interactive_lint(code: str) -> Tuple[bool, str]:  # noqa: C901
    """Heuristic checks beyond compile-OK for interactive (terminal/UI) tasks.

    Compile-OK is necessary but not sufficient: a snake game using
    `sys.stdin.read(1)` without termios setup parses fine, runs without
    crashing, and silently fails on every keypress. We've seen this in real
    user runs (ISSUES.md PC-034). Detect the most common failure shapes
    statically before accepting the probe.

    Returns (passed, reason). reason is empty when passed.
    """
    import ast as _ast
    try:
        tree = _ast.parse(code)
    except SyntaxError:
        # The compile gate above owns syntax, so this is not a second
        # failure -- but the lint did not run either, and must not say OK.
        return True, INTERACTIVE_LINT_NOT_RUN

    has_curses = False
    has_termios_setraw = False
    has_raw_stdin_read = False
    has_blocking_input_loop = False
    # PC-047: track curses-bottom-row anti-patterns (unwrapped addstr to
    # LINES-N or COLS-N — addstr to the very last cell always returns ERR
    # in curses, which is why so many "snake game" runs crash with
    # `_curses.error: addwstr() returned ERR`).
    bottom_row_addstr_nodes: List[Tuple[int, str]] = []  # (lineno, snippet)
    try_except_curses_lines: set = set()

    def _is_lines_or_cols_minus(node: _ast.AST, name: str) -> bool:
        """True if node is a `curses.{name} - <int>` BinOp expression
        (or just `LINES - <int>` if the model imported the names)."""
        if not isinstance(node, _ast.BinOp) or not isinstance(node.op, _ast.Sub):
            return False
        left = node.left
        # curses.LINES - N
        if (isinstance(left, _ast.Attribute) and left.attr == name
                and isinstance(left.value, _ast.Name) and left.value.id == "curses"):
            return True
        # bare LINES - N (after `from curses import LINES, COLS`)
        if isinstance(left, _ast.Name) and left.id == name:
            return True
        return False

    # First pass: find every `try: ... except curses.error / except _curses.error`
    # block and record the line ranges they protect, so we can skip
    # already-wrapped addstr calls below.
    for node in _ast.walk(tree):
        if isinstance(node, _ast.Try):
            handles_curses = False
            for handler in node.handlers:
                exc = handler.type
                if isinstance(exc, _ast.Attribute) and exc.attr == "error":
                    if (isinstance(exc.value, _ast.Name)
                            and exc.value.id in ("curses", "_curses")):
                        handles_curses = True
                elif isinstance(exc, _ast.Name) and exc.id == "Exception":
                    handles_curses = True  # broad catch covers curses.error
            if handles_curses:
                start = node.lineno
                end = max((getattr(n, "end_lineno", node.lineno) or node.lineno)
                          for n in _ast.walk(node))
                for ln in range(start, end + 1):
                    try_except_curses_lines.add(ln)

    for node in _ast.walk(tree):
        if isinstance(node, _ast.Import):
            for alias in node.names:
                if alias.name == "curses":
                    has_curses = True
        elif isinstance(node, _ast.ImportFrom):
            if node.module in ("curses", "termios", "tty"):
                has_curses = has_curses or node.module == "curses"
                has_termios_setraw = has_termios_setraw or node.module in ("termios", "tty")
        elif isinstance(node, _ast.Call):
            func = node.func
            if isinstance(func, _ast.Attribute):
                # sys.stdin.read(1) without termios setup
                if (
                    func.attr == "read"
                    and isinstance(func.value, _ast.Attribute)
                    and func.value.attr == "stdin"
                    and isinstance(func.value.value, _ast.Name)
                    and func.value.value.id == "sys"
                ):
                    has_raw_stdin_read = True
                # termios.tcsetattr / tty.setraw / tty.setcbreak
                if func.attr in ("tcsetattr", "setraw", "setcbreak"):
                    has_termios_setraw = True
                # PC-047: addstr / addnstr / addch with a LINES-N first arg
                # (writing to a row near the bottom — last row always errors)
                # or a COLS-N second arg pair that targets the last column.
                if func.attr in ("addstr", "addnstr", "addch"):
                    args = node.args
                    if args:
                        first = args[0]
                        if _is_lines_or_cols_minus(first, "LINES"):
                            if node.lineno not in try_except_curses_lines:
                                snippet = f"line {node.lineno}: {func.attr}(curses.LINES - N, ...) without try/except curses.error"
                                bottom_row_addstr_nodes.append((node.lineno, snippet))
        elif isinstance(node, _ast.While):
            # Look for `while True: ... input(...)` shape — blocking input
            # in an interactive loop is almost always wrong.
            for sub in _ast.walk(node):
                if isinstance(sub, _ast.Call) and isinstance(sub.func, _ast.Name) and sub.func.id == "input":
                    has_blocking_input_loop = True
                    break

    # Raw stdin read without termios is a near-certain bug for interactive
    # keystroke handling — single-char read is line-buffered and can't see
    # arrow-key escape sequences.
    if has_raw_stdin_read and not has_termios_setraw and not has_curses:
        return False, "raw sys.stdin.read without termios/tty setup or curses — keystrokes won't register"

    # input() inside a `while True` of a TUI flow blocks until Enter; usually
    # intended to be a non-blocking key read.
    if has_blocking_input_loop and not has_curses and not has_termios_setraw:
        return False, "input() in a loop with no curses/termios — blocks on Enter, can't read single keystrokes"

    # PC-047: unwrapped addstr to the bottom row will always raise
    # `_curses.error: addwstr() returned ERR` at runtime (writing the last
    # cell of any window is undefined and historically returns ERR). The
    # idiomatic fix is `try: stdscr.addstr(...) except curses.error: pass`.
    # Fail the lint so V3 prefers a candidate that has the wrap.
    if has_curses and bottom_row_addstr_nodes:
        first = bottom_row_addstr_nodes[0][1]
        return False, f"curses bottom-row write without try/except curses.error wrap — {first} (will raise ERR at runtime)"

    return True, ""
