"""The baseline arm: the same model through a minimal read/write/run loop.

No V3, lens, gates or grammar (#238). The budget, sampling and command limits
are ATLAS's own, so the gap between the arms measures the harness (#242).
"""
from __future__ import annotations

import json
import socket
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Optional

from result import COMPLETED, ArmResult

SYSTEM_PROMPT = (
    "You are working in a project directory. Use tools by replying with exactly "
    "one JSON object and nothing else:\n"
    '{"tool": "read_file", "path": "..."} reads a file.\n'
    '{"tool": "write_file", "path": "...", "content": "..."} writes a whole file.\n'
    '{"tool": "run_command", "command": "..."} runs a shell command in the project directory.\n'
    '{"tool": "done", "summary": "..."} ends the task.\n'
    "After each tool call you receive its result. Finish the task, check that it "
    "works, then reply with done."
)
NO_JSON_REPLY = "Reply with exactly one JSON object, one of the four shown at the start."
MAX_TOKENS = 8192                # ATLAS's per-turn ceiling
RUN_TIMEOUT_S = 30               # ATLAS's run_command default...
RUN_TIMEOUT_CAP_S = 300          # ...and cap
READ_CAP_BYTES = 200_000         # a file bigger than this is cut, and says so
CHARS_PER_TOKEN = 3              # low on purpose: an overflow ends a turn, a spare token does not


class Workspace:
    """The run's directory, as the driver writes it and as the sandbox mounts it."""

    def __init__(self, host: Path, in_sandbox: str):
        self.host = host.resolve()
        self.in_sandbox = in_sandbox.rstrip("/")

    def resolve(self, path: str) -> Optional[Path]:
        """A path inside the workspace, or None for one outside it."""
        if path.startswith(self.in_sandbox + "/"):
            path = path[len(self.in_sandbox) + 1:]
        target = (self.host / path).resolve()
        if target != self.host and self.host not in target.parents:
            return None
        return target


def run_baseline(task, ws: Workspace, llama_url: str, sandbox_url: str,
                 budget_s: float, context_tokens: int, post=None) -> ArmResult:
    post = post or post_json
    messages = [{"role": "system", "content": SYSTEM_PROMPT},
                {"role": "user", "content": task.prompt}]
    t0, turns, tokens = time.time(), 0, 0
    while True:
        left = budget_s - (time.time() - t0)
        if left <= 0:
            return ArmResult("timed_out", "the budget ran out", _since(t0), turns, tokens)
        try:
            reply, used = ask_model(llama_url, fit_context(messages, context_tokens), left, post)
        except (socket.timeout, TimeoutError):
            return ArmResult("timed_out", "the budget ran out mid-reply", _since(t0), turns, tokens)
        except (urllib.error.URLError, OSError, ValueError, KeyError) as e:
            return ArmResult("failed", f"model call failed: {e}", _since(t0), turns, tokens)
        turns, tokens = turns + 1, tokens + used
        messages.append({"role": "assistant", "content": reply})
        call = first_json_object(reply)
        if call is not None and call.get("tool") == "done":
            return ArmResult(COMPLETED, "done", _since(t0), turns, tokens)
        answer = NO_JSON_REPLY if call is None else execute(call, ws, sandbox_url, post)
        messages.append({"role": "user", "content": answer})


def ask_model(llama_url: str, messages: list, timeout: float, post) -> tuple:
    """The model's reply text and the tokens the call used."""
    data = post(f"{llama_url}/v1/chat/completions", {
        "messages": messages, "max_tokens": MAX_TOKENS, "stream": False,
        "chat_template_kwargs": {"enable_thinking": False},
        "samplers": ["top_k"], "top_k": 1,  # ATLAS's agent sampling (#242)
    }, timeout)
    msg = data["choices"][0]["message"]
    used = int((data.get("usage") or {}).get("total_tokens") or 0)
    return (msg.get("content") or "").strip(), used


def first_json_object(text: str) -> Optional[dict]:
    """The first JSON object in the reply that names a tool."""
    decoder = json.JSONDecoder()
    i = text.find("{")
    while i >= 0:
        try:
            obj, _ = decoder.raw_decode(text, i)
        except ValueError:
            obj = None
        if isinstance(obj, dict) and isinstance(obj.get("tool"), str):
            return obj
        i = text.find("{", i + 1)
    return None


def execute(call: dict, ws: Workspace, sandbox_url: str, post) -> str:
    tool = call.get("tool")
    if tool == "read_file":
        return read_file(ws, str(call.get("path", "")))
    if tool == "write_file":
        return write_file(ws, str(call.get("path", "")), call.get("content"))
    if tool == "run_command":
        return run_command(ws, sandbox_url, str(call.get("command", "")), call.get("timeout"), post)
    return f"unknown tool {tool!r}: use read_file, write_file, run_command or done"


def read_file(ws: Workspace, path: str) -> str:
    target = ws.resolve(path)
    if target is None:
        return f"error: {path} is outside the project directory"
    try:
        data = target.read_bytes()
    except OSError as e:
        return f"error: cannot read {path}: {e.strerror or e}"
    text = data[:READ_CAP_BYTES].decode("utf-8", "replace")
    if len(data) > READ_CAP_BYTES:
        text += f"\n[cut: the file has {len(data)} bytes]"
    return text


def write_file(ws: Workspace, path: str, content) -> str:
    target = ws.resolve(path)
    if target is None:
        return f"error: {path} is outside the project directory"
    if not isinstance(content, str):
        return "error: write_file needs a content string"
    try:
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(content)
    except OSError as e:
        return f"error: cannot write {path}: {e.strerror or e}"
    return f"wrote {len(content.encode())} bytes to {path}"


def run_command(ws: Workspace, sandbox_url: str, command: str, timeout, post) -> str:
    """The same sandbox endpoint and limits as ATLAS's run_command."""
    t = timeout if isinstance(timeout, int) and timeout > 0 else RUN_TIMEOUT_S
    t = min(t, RUN_TIMEOUT_CAP_S)
    try:
        out = post(f"{sandbox_url}/shell",
                   {"command": command, "cwd": ws.in_sandbox, "timeout": t}, t + 30)
    except (urllib.error.URLError, OSError, ValueError) as e:
        return f"error: the command could not run: {e}"
    return (f"exit code {out.get('exit_code')}\n"
            f"stdout:\n{out.get('stdout', '')}\nstderr:\n{out.get('stderr', '')}")


def fit_context(messages: list, context_tokens: int) -> list:
    """The conversation, with the oldest tool results replaced by a note when
    it would not fit. The system prompt and the task always stay."""
    budget = (context_tokens - MAX_TOKENS) * CHARS_PER_TOKEN
    fitted = [dict(m) for m in messages]
    for i in range(2, len(fitted)):
        if sum(len(m["content"]) for m in fitted) <= budget:
            break
        if fitted[i]["role"] == "user" and not fitted[i]["content"].startswith("[result omitted"):
            fitted[i]["content"] = f"[result omitted: {len(fitted[i]['content'])} characters]"
    return fitted


def server_props(llama_url: str, get=None) -> dict:
    """The model server's /props: its context window and what it serves."""
    return (get or get_json)(f"{llama_url}/props", 30)


def model_identity(props: dict) -> dict:
    """What the model server says it serves, as far as /props tells. n_ctx is
    the context window; the rest names the model and the server build."""
    settings = props.get("default_generation_settings") or {}
    return {"n_ctx": int(settings["n_ctx"]), "model": settings.get("model"),
            "model_path": props.get("model_path"), "build_info": props.get("build_info")}


def post_json(url: str, body: dict, timeout: float) -> dict:
    req = urllib.request.Request(url, data=json.dumps(body).encode(),
                                 headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as resp:
        return json.loads(resp.read())


def get_json(url: str, timeout: float) -> dict:
    with urllib.request.urlopen(url, timeout=timeout) as resp:
        return json.loads(resp.read())


def _since(t0: float) -> float:
    return round(time.time() - t0, 1)
