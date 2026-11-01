"""The two records of the reliability runner (scripts/eval/e2e-reliability.py).

A Task is what a session is asked: its prompt, the fixture files it starts
from, and the check that decides whether the requested change landed. A
Session is what one run of a task produced: its event stream, its workspace,
and what the runner found in them.
"""
from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable


@dataclass
class Task:
    name: str
    prompt: str
    files: dict[str, str]
    check: Callable[[Path], tuple[bool, str]]
    # Files the session is expected to leave parseable. Any workspace file is
    # checked for corruption regardless; this is the subset that must exist.
    must_exist: tuple[str, ...] = ()
    # Fixtures that are INPUT DATA and must come back untouched. Not every
    # fixture qualifies: add_function is handed stats.py precisely so it can
    # edit it. Only files the task never asks to change belong here, or the
    # check fails a session for doing exactly what it was told.
    immutable: tuple[str, ...] = ()
    # Follow-up messages sent after the first, each carrying the prior
    # exchange as history. Every task before this was a single message, which
    # left the way people actually use the tool — correct me, now do this too
    # — completely unexercised.
    followups: tuple[str, ...] = ()
    # A question, not a job. The tiers exist so V3 does not run on everything:
    # a question should get an answer from the conversational tier, with no
    # writes and no multi-minute pipeline. Both are checked.
    conversational: bool = False


# insert_after and replace_lines were missing, so a session whose only
# successful write used one of them scored as "no successful write" — H4 fired
# on go_offbyone and multifile_cli, both of which had written correctly. The
# same omission has now been found in the lens breaker, the worked-example
# generator, the productive-change counter, the write-gate chain, and here:
# a set of tool names that nobody updates when a tool is added.
WRITE_TOOLS = {"write_file", "edit_file", "structural_edit", "delete_file",
               "move_file", "insert_after", "replace_lines"}


def _tool_payload(result: dict) -> dict:
    payload = (result.get("data") or {}).get("data")
    if isinstance(payload, str):
        try:
            payload = json.loads(payload)
        except json.JSONDecodeError:
            return {}
    return payload if isinstance(payload, dict) else {}


@dataclass
class Session:
    task: str
    rep: int
    events: list[dict]
    workspace: Path
    wall_s: float
    stream_ok: bool
    defects: list[str] = field(default_factory=list)
    task_passed: bool = False
    task_detail: str = ""
    quality: dict = field(default_factory=dict)
    # Containers that restarted, were OOM-killed or went away while the
    # session ran (stack_changes). A measured outcome over an unstable stack
    # says so.
    stack_changes: list[str] = field(default_factory=list)
    # Each permission prompt of the session and what the runner answered.
    prompts: list[dict] = field(default_factory=list)

    def of_type(self, t: str) -> list[dict]:
        return [e for e in self.events if e.get("type") == t]

    @property
    def v3(self) -> dict:
        """What the V3 pipeline did in this session, read from the stream.

        planner: /v3/plan events. writes: write-tool calls. generated: write
        calls during which the generation pipeline emitted anything (beyond
        the planner). delivered: write results whose bytes V3's candidate
        supplied (v3_used). A run labelled as measuring ATLAS with zero
        generations measured the agent loop without V3, and says so.
        """
        planner = sum(1 for e in self.events if e.get("type") in ("v3_plan", "plan_loaded"))
        writes = generated = delivered = 0
        in_write = saw_v3 = False
        for e in self.events:
            t = str(e.get("type") or "")
            d = e.get("data") or {}
            if t == "tool_call":
                in_write = d.get("name") in WRITE_TOOLS
                saw_v3 = False
                if in_write:
                    writes += 1
            elif in_write and t.startswith("v3_") and t != "v3_plan":
                saw_v3 = True
            elif t == "tool_result" and in_write:
                generated += saw_v3
                delivered += bool(_tool_payload(e).get("v3_used"))
                in_write = saw_v3 = False
        return {"planner_events": planner, "write_calls": writes,
                "generated": generated, "delivered": delivered}

    @property
    def capped(self) -> bool:
        """True when this runner stopped reading at --timeout.

        The stream was cut from this side, so the absence of a `done`
        event says nothing about the proxy's behaviour.
        """
        return any("harness cap:" in str(e.get("data", {}).get("error", ""))
                   for e in self.of_type("error"))
