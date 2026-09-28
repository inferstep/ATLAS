"""LLM generation and response post-processing for the V3 pipeline and
the bench harness.

`chat_completion()` calls llama-server's /v1/chat/completions endpoint with
structured messages, so the model's embedded chat template is applied
(llama-server runs with `--jinja`). Reasoning is controlled with the
`enable_thinking` chat-template kwarg (default off); templates that don't define
it ignore it. If a model returns its answer in `reasoning_content`, or leaves a
`<think>` block in `content`, that output is recovered/stripped so callers always
receive plain text. Any GGUF's own prompt format is honored without per-model
handling.
"""

import ast
import json
import os
import re
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Dict, List, Optional


def _service_token() -> str:
    """Internal-auth token (empty = auth disabled). Resolution: explicit
    ATLAS_SERVICE_TOKEN_FILE, the container secret mount, then the repo
    checkout's secrets/ file (stages/ lives under v3-service/, so
    parents[2] is the repo root)."""
    explicit = os.environ.get("ATLAS_SERVICE_TOKEN_FILE")
    candidates = [explicit] if explicit else [
        "/run/atlas-secrets/service-token",
        str(Path(__file__).resolve().parents[2] / "secrets" / "service-token"),
    ]
    for path in candidates:
        try:
            with open(path) as fh:
                return fh.read().strip()
        except OSError:
            continue
    return ""


def _auth_headers() -> dict:
    tok = _service_token()
    return {"Authorization": f"Bearer {tok}"} if tok else {}


def _install_auth_opener() -> None:
    """Cover every urllib call site in the bench pipeline with the
    internal-auth header (urllib merges these under explicit per-request
    headers)."""
    tok = _service_token()
    if not tok:
        return
    opener = urllib.request.build_opener()
    opener.addheaders = [("Authorization", f"Bearer {tok}")]
    urllib.request.install_opener(opener)


_install_auth_opener()

try:
    import httpx
    HAS_HTTPX = True
except ImportError:
    HAS_HTTPX = False

# Plain-English system prompt — no model-specific directives (no `/nothink`).
DEFAULT_SYSTEM_PROMPT = "You are an expert programmer. Respond directly and concisely."

_THINK_BLOCK = re.compile(r"<think>.*?</think>\s*", re.DOTALL)
_CHATML_TURN = re.compile(
    r"<\|im_start\|>(system|user|assistant)\s*\n(.*?)(?:<\|im_end\|>|\Z)",
    re.DOTALL,
)


def strip_reasoning_leak(text: str) -> str:
    """Remove a leaked reasoning block from model output. With
    `enable_thinking: false` reasoning lands in a separate field, but a model
    that emits a `<think>` block into the content anyway is handled here.
    Covers all three shapes: a closed `<think>...</think>` pair, an orphan
    closing tag (`...</think>answer` — keep the answer), and an orphan opening
    tag (`answer<think>truncated...` — keep the answer, drop the unclosed
    reasoning to end-of-text).

    Leading whitespace left behind by a removed block is framing and goes.
    Trailing bytes stay: when the content is an artifact, its final newline is
    part of the artifact, and this helper runs on every completion before any
    extractor sees it."""
    if not text:
        return text
    out = _THINK_BLOCK.sub("", text)
    # Orphan closing tag (text before the open was lost / pre-fill artifact):
    # the real content follows the close.
    if "</think>" in out and "<think>" not in out:
        out = out.split("</think>", 1)[1]
    # Orphan opening tag (reasoning truncated mid-thought, no close): the real
    # content, if any, precedes the open; everything after is reasoning.
    if "<think>" in out:
        out = out.split("<think>", 1)[0]
    return out.lstrip()


def extract_code(response: str) -> str:
    """
    Extract code from an LLM response.

    Handles various formats:
    - Markdown code blocks with any language label (```python, ```javascript, ...)
    - Plain code blocks (``` ... ```)
    - Raw code without blocks
    - optional <think>...</think> reasoning blocks (stripped before extraction)

    The bytes come back exactly as the model wrote them. The fence is framing
    and is not returned; everything inside it is, including the final newline
    when there is one, none when there is not, and every trailing blank line.
    Nothing here normalizes whitespace, line endings or indentation. Every
    candidate hash, selection record, authorization identity and disk write
    downstream is computed from this return value, so a byte dropped here is
    a candidate that matches nothing it was compared against. Only leading
    framing -- prose or whitespace before an unfenced artifact -- is trimmed.

    Args:
        response: Raw LLM response text

    Returns:
        The artifact's exact bytes
    """
    # Strip template-emitted thinking blocks first; they can consume tokens
    # before the actual code output
    think_pattern = r'<think>.*?</think>'
    response = re.sub(think_pattern, '', response, flags=re.DOTALL).lstrip()

    # Safety net: strip unclosed <think> tags (edge case where
    # thinking mode doesn't fully strip thinking). What precedes the tag is
    # the content, terminator included.
    if '<think>' in response and '</think>' not in response:
        response = response[:response.index('<think>')]

    # Try MBPP [BEGIN]...[DONE] delimiters first
    begin_done_pattern = r'\[BEGIN\]\s*\n(.*?)(?:\[DONE\]|$)'
    begin_matches = re.findall(begin_done_pattern, response, re.DOTALL)
    if begin_matches:
        # Return the last match (the model's answer, not the few-shot examples)
        return begin_matches[-1]

    # Extract fenced code with an optional language label. The V3 service
    # supports multiple languages, so limiting labels to Python leaves fences
    # such as ```javascript in the returned source and causes false syntax
    # failures downstream.
    pattern = r'```[^\S\r\n]*[A-Za-z0-9_+.#-]*[^\S\r\n]*\r?\n(.*?)```'
    matches = re.findall(pattern, response, re.DOTALL)

    if matches:
        # Return the longest match (likely the main code block), verbatim.
        return max(matches, key=len)

    # No code blocks found, assume raw code. Leading framing goes; the
    # artifact's own trailing bytes stay.
    code = response.lstrip()

    # Remove common LLM artifacts
    lines = code.split('\n')
    filtered_lines = []
    for line in lines:
        # Skip lines that look like explanations
        if line.strip().startswith('Here') and ':' in line:
            continue
        if line.strip().startswith('This function'):
            continue
        if line.strip().startswith('The function'):
            continue
        filtered_lines.append(line)

    return '\n'.join(filtered_lines)


_FENCED_CODE = re.compile(
    r'```[^\S\r\n]*[A-Za-z0-9_+.#-]*[^\S\r\n]*\r?\n(.*?)```',
    re.DOTALL,
)
_EXACT_FUNCTION = re.compile(
    r'\bImplement\s+exactly\s*:\s*(?:async\s+)?def\s+([A-Za-z_]\w*)\s*\(',
    re.IGNORECASE,
)
_EXACT_CLASS = re.compile(
    r'\bImplement\s+class\s+([A-Za-z_]\w*)\b', re.IGNORECASE,
)


def _requested_python_declarations(problem: str) -> List[str]:
    """Return only declarations explicitly named as the requested artifact.

    Existing project context and reference implementations can contain many
    declarations, so broad ``def`` matching would let context accidentally
    choose a response block.  These two forms are the generation contract's
    explicit target forms; callers without one retain their historical
    extraction policy.
    """
    names = _EXACT_FUNCTION.findall(problem or "")
    names.extend(_EXACT_CLASS.findall(problem or ""))
    return list(dict.fromkeys(names))


def _requested_python_function_contracts(problem: str) -> Dict[str, tuple]:
    """Return exact, parseable one-line function declarations in the request.

    The explicit ``Implement exactly:`` form names an interface, not merely a
    function.  Parsing the declaration lets post-processing distinguish that
    contract from annotations or aliases a model added on its own.
    """
    requested = set(_EXACT_FUNCTION.findall(problem or ""))
    contracts = {}
    lines = (problem or "").splitlines()
    for line_index, raw_line in enumerate(lines):
        declaration = raw_line.strip()
        if not declaration.startswith(("def ", "async def ")):
            continue
        # Only the declaration immediately owned by ``Implement exactly:``
        # is authoritative.  The generated problem also contains a reference
        # implementation whose model-authored signature may differ; scanning
        # every matching ``def`` let that later baseline overwrite the user's
        # exact contract.  Existing project context can contain the same name
        # too.  Neither is permission to rewrite the requested interface.
        previous = line_index - 1
        while previous >= 0 and not lines[previous].strip():
            previous -= 1
        if previous < 0 or not re.search(
            r"\bImplement\s+exactly\s*:\s*$",
            lines[previous], re.IGNORECASE,
        ):
            continue
        try:
            parsed = ast.parse(declaration + "\n    pass").body
        except SyntaxError:
            continue
        if len(parsed) != 1 or not isinstance(
            parsed[0], (ast.FunctionDef, ast.AsyncFunctionDef)
        ):
            continue
        node = parsed[0]
        if node.name in requested:
            contracts[node.name] = (declaration, node)
    return contracts


def _declares_name(code: str, name: str) -> bool:
    return bool(re.search(
        rf'(?m)^\s*(?:(?:async\s+)?def|class)\s+{re.escape(name)}\b', code
    ))


def _repair_unambiguous_python_syntax(code: str) -> str:
    """Repair only parser-identified, unambiguous punctuation/indent typos.

    This is deliberately not a general code fixer.  It removes an unmatched
    closing delimiter at the exact SyntaxError offset, or an indentation that
    Python reports as unexpected at top level.  The changed artifact is used
    only when the complete file then parses; otherwise the model bytes are
    returned unchanged for the normal sandbox/repair path to reject.
    """
    original = code
    current = code
    for _ in range(4):
        try:
            ast.parse(current)
            return current
        except SyntaxError as exc:
            if not exc.lineno or not exc.offset:
                return original
            lines = current.splitlines(keepends=True)
            if exc.lineno > len(lines):
                return original
            line = lines[exc.lineno - 1]
            pos = exc.offset - 1
            if (exc.msg.startswith("unmatched ") and 0 <= pos < len(line)
                    and line[pos] in ")]}"):
                lines[exc.lineno - 1] = line[:pos] + line[pos + 1:]
            elif exc.msg == "unexpected indent" and line[:1] in (" ", "\t"):
                lines[exc.lineno - 1] = line.lstrip(" \t")
            else:
                return original
            current = "".join(lines)
    try:
        ast.parse(current)
    except SyntaxError:
        return original
    return current


def _restore_exact_requested_signatures(code: str, problem: str) -> str:
    """Restore an explicit requested signature without rewriting function bodies.

    Models sometimes add equivalent-looking annotations or substitute typing
    aliases even when the request says the declaration is exact.  For a unique
    top-level target, replace only its header with the parseable declaration
    supplied by the user.  Ambiguous targets, one-line bodies, async/sync
    changes, or a result that does not parse are left byte-for-byte unchanged.
    """
    contracts = _requested_python_function_contracts(problem)
    if not contracts:
        return code
    try:
        # Parsed for validity only: unparseable input is returned untouched.
        ast.parse(code)
    except SyntaxError:
        return code
    current = code
    # Requests currently name one exact artifact, but deterministic iteration
    # keeps this safe if a future request explicitly names several functions.
    for name, (declaration, requested) in contracts.items():
        try:
            parsed = ast.parse(current)
        except SyntaxError:
            return code
        matches = [
            node for node in parsed.body
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name == name
        ]
        if len(matches) != 1:
            continue
        candidate = matches[0]
        if type(candidate) is not type(requested) or not candidate.body:
            continue
        # A same-line body cannot be separated from its header without a broad
        # rewrite.  Leave it to normal fail-closed handling.
        if candidate.body[0].lineno <= candidate.lineno:
            continue
        lines = current.splitlines(keepends=True)
        start = candidate.lineno - 1
        body_start = candidate.body[0].lineno - 1
        indent = lines[start][:len(lines[start]) - len(lines[start].lstrip(" \t"))]
        newline = "\r\n" if lines[start].endswith("\r\n") else "\n"
        trial = "".join(lines[:start] + [indent + declaration + newline] + lines[body_start:])
        try:
            checked = ast.parse(trial)
        except SyntaxError:
            continue
        restored = [
            node for node in checked.body
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
            and node.name == name
        ]
        if len(restored) != 1:
            continue
        restored_node = restored[0]
        restored_return = (
            ast.dump(restored_node.returns, include_attributes=False)
            if restored_node.returns is not None else None
        )
        requested_return = (
            ast.dump(requested.returns, include_attributes=False)
            if requested.returns is not None else None
        )
        if (ast.dump(restored_node.args, include_attributes=False)
                != ast.dump(requested.args, include_attributes=False)
                or restored_return != requested_return):
            continue
        current = trial
    return current


def extract_code_for_problem(
    response: str, problem: str, *, fallback: str = "longest"
) -> str:
    """Extract the requested artifact and conservatively syntax-gate Python.

    When the request explicitly names a function or class, a fenced block
    declaring that target outranks supplemental examples/tests even if those
    are longer or later.  Without an explicit target, historical longest/last
    behavior is preserved.  For explicit Python artifacts, only the narrow
    parser-proven repair above is attempted.
    """
    cleaned = strip_reasoning_leak(response or "")
    blocks = _FENCED_CODE.findall(cleaned)
    names = _requested_python_declarations(problem)
    if blocks:
        targeted = [b for b in blocks if all(_declares_name(b, n) for n in names)]
        choices = targeted or blocks
        code = choices[-1] if fallback == "last" else max(choices, key=len)
    else:
        code = extract_code(cleaned)
    if names:
        repaired = _repair_unambiguous_python_syntax(code)
        return _restore_exact_requested_signatures(repaired, problem)
    return code


def chatml_to_messages(prompt: str) -> List[Dict[str, str]]:
    """Convert a ChatML-formatted string into structured chat messages. Callers
    that assemble a ChatML prompt can pass it straight to `chat_completion`. A
    string with no ChatML markers becomes a single user message."""
    turns = _CHATML_TURN.findall(prompt or "")
    if not turns:
        return [{"role": "user", "content": (prompt or "").strip()}]
    messages = []
    for role, content in turns:
        content = content.strip()
        # Drop a trailing empty `assistant` turn (the generation cue).
        if role == "assistant" and not content:
            continue
        # Scrub any lingering `/nothink` directive from migrated system prompts.
        if role == "system":
            content = content.replace("/nothink", "").strip()
        messages.append({"role": role, "content": content})
    return messages or [{"role": "user", "content": (prompt or "").strip()}]


def _parse_logprobs(data: dict) -> List[float]:
    """Parse per-token logprobs from an OpenAI-style chat-completions response
    (`choices[0].logprobs.content[].logprob`). Returns [] if absent."""
    try:
        lp = data["choices"][0].get("logprobs") or {}
        toks = lp.get("content") or []
        return [t["logprob"] for t in toks if "logprob" in t]
    except (KeyError, IndexError, TypeError):
        return []


def chat_completion(
    llm_url: str,
    user: Optional[str] = None,
    system: Optional[str] = DEFAULT_SYSTEM_PROMPT,
    messages: Optional[List[Dict[str, str]]] = None,
    temperature: float = 0.0,
    max_tokens: int = 16384,
    seed: Optional[int] = None,
    enable_thinking: bool = False,
    want_logprobs: bool = False,
    timeout: float = 600.0,
    extra_body: Optional[dict] = None,
) -> Dict:
    """Generate a completion model-agnostically via /v1/chat/completions.

    Provide EITHER `user` (+ optional `system`) OR a pre-built `messages` list
    (e.g. from `chatml_to_messages()`). Returns dict:
    {content, reasoning, tokens, time_ms, logprobs, raw}. `content` is cleaned
    (reasoning-leak stripped); if `content` is empty but the model emitted
    `reasoning_content`, that is used as the content fallback.
    """
    if messages is None:
        messages = []
        if system:
            messages.append({"role": "system", "content": system})
        messages.append({"role": "user", "content": user or ""})

    body = {
        "messages": messages,
        "temperature": temperature,
        "max_tokens": max_tokens,
        "stream": False,
        # The model's OWN jinja template decides how to honor this; templates
        # without the kwarg ignore it harmlessly.
        "chat_template_kwargs": {"enable_thinking": enable_thinking},
    }
    if seed is not None:
        body["seed"] = seed
    if want_logprobs:
        body["logprobs"] = True
        body["top_logprobs"] = 1
    if extra_body:
        body.update(extra_body)

    endpoint = f"{llm_url}/v1/chat/completions"
    payload = json.dumps(body).encode("utf-8")
    start = time.time()
    if HAS_HTTPX:
        resp = httpx.post(endpoint, json=body, timeout=timeout,
                          headers=_auth_headers())
        resp.raise_for_status()
        data = resp.json()
    else:
        req = urllib.request.Request(
            endpoint, data=payload, headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=timeout) as r:
            data = json.loads(r.read().decode("utf-8"))
    time_ms = (time.time() - start) * 1000

    msg = (data.get("choices") or [{}])[0].get("message", {}) or {}
    content = msg.get("content") or ""
    reasoning = msg.get("reasoning_content") or ""
    # Universal fallback: a reasoning model that ignored enable_thinking and put
    # its answer in reasoning_content still yields usable output.
    if not content.strip() and reasoning.strip():
        content = reasoning
    content = strip_reasoning_leak(content)

    usage = data.get("usage", {}) or {}
    tokens = usage.get("completion_tokens", 0)
    return {
        "content": content,
        "reasoning": reasoning,
        "tokens": tokens,
        "time_ms": time_ms,
        "logprobs": _parse_logprobs(data) if want_logprobs else [],
        "raw": data,
    }
