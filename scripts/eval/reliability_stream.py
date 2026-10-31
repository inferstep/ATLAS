"""How the reliability runner reads a session's event stream and answers its permission prompts.

A deletion always asks for permission, also in the mode the runner sends, and
the proxy then waits for an answer for as long as its own limit says (ten
minutes, unless the stack sets another). An unattended run has nobody to
answer. A runner that sends nothing makes each such call cost the session
that whole wait, and the session then ends on a timeout that says nothing
about the product. So the runner answers by a policy that the run states, and
keeps every answer with the session.
"""
from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from collections.abc import Callable, Iterable

PROMPT_POLICIES = {
    "deny": "answer no at once: an unattended run has nobody who could approve",
    "allow": "answer yes at once",
    "wait": "send no answer, and let the proxy's own limit end the wait",
}


def read_stream(lines: Iterable[bytes], take: Callable[[dict], None], deadline: float, raw_sink=None) -> str:
    """Hand each event of an event stream to `take`, and say how the reading ended.

    "done": the stream said it was done. "cap": `deadline` passed. "end": the
    stream stopped without saying done. The deadline is checked on every
    line, because urlopen's timeout is per read: a session that keeps
    streaming never trips it. The exact lines go to `raw_sink` before
    anything parses them.
    """
    for raw in lines:
        if time.time() > deadline:
            return "cap"
        text = raw.decode("utf-8", "replace")
        if raw_sink is not None:
            raw_sink.write(text)
        line = text.strip()
        if not line.startswith("data: "):
            continue
        payload = line[6:]
        if payload == "[DONE]":
            return "done"
        try:
            take(json.loads(payload))
        except json.JSONDecodeError:
            take({"type": "__unparseable__", "raw": payload[:200]})
    return "end"


def answer_prompt(url: str, session_id: str, event: dict, policy: str, opener=urllib.request.urlopen) -> dict:
    """Answer one permission_request by the run's policy. Returns what the session keeps about it.

    `delivered` is what the proxy said: True when the answer reached the
    waiting call, False when no call was waiting for it, None when no answer
    was sent or the proxy could not be asked (then `error` says why).
    """
    data = event.get("data") or {}
    kept = {"tool": data.get("tool_name", ""), "tool_call_id": data.get("tool_call_id", ""),
            "policy": policy, "answer": "", "delivered": None}
    if policy == "wait":
        return kept
    body = json.dumps({"session_id": session_id, "tool_call_id": kept["tool_call_id"], "decision": policy,
                       "scope": "once"}).encode()
    request = urllib.request.Request(f"{url}/v1/permission", data=body, headers={"Content-Type": "application/json"})
    kept["answer"] = policy
    try:
        with opener(request, timeout=10) as response:
            kept["delivered"] = bool(json.load(response).get("delivered"))
    except urllib.error.HTTPError as error:
        kept["delivered"] = False if error.code == 404 else None
        if error.code != 404:
            kept["error"] = f"the proxy answered {error.code}"
    except (urllib.error.URLError, TimeoutError, OSError, ValueError) as error:
        kept["error"] = f"the answer could not be sent: {error}"
    return kept
