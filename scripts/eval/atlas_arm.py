"""The ATLAS arm: one session through POST /v1/agent, sent as the TUI sends it."""
from __future__ import annotations

import json
import time
import urllib.error
import urllib.request

from result import ArmResult


def run_atlas(task, proxy_url: str, subdir: str, session_id: str, cap_s: int,
              urlopen=urllib.request.urlopen) -> ArmResult:
    """cap_s is the driver's own backstop; the stack's session timeout ends a
    session first."""
    body = json.dumps({
        "message": task.prompt, "mode": "yolo", "sandbox_subdir": subdir,
        "session_id": session_id, "task_contract": {"task_mode": task.mode},
    }).encode()
    req = urllib.request.Request(f"{proxy_url}/v1/agent", data=body,
                                 headers={"Content-Type": "application/json"})
    t0 = time.time()
    events = read_events(req, cap_s, urlopen)
    return result_of(events, time.time() - t0)


def read_events(req, cap_s: int, urlopen) -> list:
    events = []
    t0 = time.time()
    try:
        with urlopen(req, timeout=cap_s) as resp:
            for raw in resp:
                if time.time() - t0 > cap_s:
                    events.append({"type": "error", "data": {"error": "driver cap reached"}})
                    break
                line = raw.decode("utf-8", "replace").strip()
                if not line.startswith("data: "):
                    continue
                if line[6:] == "[DONE]":
                    break
                try:
                    events.append(json.loads(line[6:]))
                except ValueError:
                    continue  # one bad frame is not the session's outcome
    except (urllib.error.URLError, TimeoutError, OSError) as e:
        events.append({"type": "error", "data": {"error": f"stream failed: {e}"}})
    return events


def result_of(events: list, wall_s: float) -> ArmResult:
    """The session's own terminal status; no terminal event is its own outcome."""
    done = [e.get("data") or {} for e in events if e.get("type") == "done"]
    turns = sum(1 for e in events if e.get("type") == "turn_start")
    tokens = max([int((e.get("data") or {}).get("total_tokens") or 0)
                  for e in events if e.get("type") == "llm_call_end"] or [0])
    if not done:
        errors = [str((e.get("data") or {}).get("error", "")) for e in events if e.get("type") == "error"]
        return ArmResult("no_terminal", (errors or ["the stream ended without a done event"])[-1],
                         round(wall_s, 1), turns, tokens)
    last = done[-1]
    # A done event without a status is incomplete (docs/API.md).
    return ArmResult(last.get("status") or "incomplete", last.get("reason") or "",
                     round(wall_s, 1), turns, tokens)
