"""The harness-defect detectors of the reliability runner (scripts/eval/e2e-reliability.py).

Each detector returns the harness defects it finds in a session: the faults of
ATLAS's own plumbing, apart from whether the model did the task. The runner
applies all nine; h2, which judges a rejection against the file on disk, is
in scripts/eval/e2e-reliability.py. Also the event types the TUI can render, which
h1 checks against.
"""
from __future__ import annotations

import json
import re
import subprocess
from pathlib import Path

from reliability_records import WRITE_TOOLS, Session, Task
from reliability_sandbox import _file_parses

REPO = Path(__file__).resolve().parents[2]

# --------------------------------------------------------------------------
# Harness-defect detectors
# --------------------------------------------------------------------------
#
# Each returns a list of human-readable defects. A session with an empty union
# counts toward the Harness Integrity Rate. These are deliberately conservative:
# anything ambiguous is NOT counted, so the reported rate is an upper bound on
# integrity only in the sense that undetected classes exist — never inflated by
# a false positive.

# Terminal outcomes a `done` payload may carry (proxy/types.go owns the
# vocabulary). Kept literal here because this script runs standalone against a
# deployed proxy and must not import the CLI package.
TERMINAL_STATUSES = ("completed", "incomplete", "stopped", "timed_out", "failed")


def h1_protocol(s: Session, known_types: set[str]) -> list[str]:
    """Every tool_call answered, stream terminated, every event type known."""
    out = []
    calls = len(s.of_type("tool_call"))
    results = len(s.of_type("tool_result"))
    # A capped session is cut at an arbitrary point, so the call that was
    # in flight when we stopped reading has no result yet. More than one
    # unanswered call is still a real mismatch.
    allowed_orphans = 1 if s.capped else 0
    if calls - results > allowed_orphans or results > calls:
        out.append(f"H1 protocol: {calls} tool_call vs {results} tool_result "
                   f"(orphaned call)")
    if s.capped:
        # This runner stopped reading at --timeout, so there was no
        # opportunity to send `done` and the socket closed mid-stream.
        # Charging that to the proxy as two protocol violations counts
        # our own deadline as its defect. Report the deadline instead.
        out.append(f"H1 timeout: runner cap cut the session at "
                   f"{s.wall_s:.0f}s before it finished")
    else:
        if not s.of_type("done"):
            out.append("H1 protocol: stream ended without a done event")
        if not s.stream_ok:
            out.append("H1 protocol: stream terminated abnormally")
    seen = {e.get("type") for e in s.events}
    unknown = sorted(t for t in seen if t and t not in known_types)
    if unknown:
        out.append(f"H1 protocol: event type(s) the TUI cannot render: {unknown}")
    return out


def h3_dead_end_steering(s: Session) -> list[str]:
    """Steering that names a remedy the very next call is rejected for using.

    The D10 class: a nudge offered an HTML <tag> selector for a .py file, the
    model complied, and the tool refused it as unsupported.
    """
    out = []
    results = s.of_type("tool_result")
    calls = s.of_type("tool_call")
    for i, ev in enumerate(results[:-1]):
        d = ev.get("data") or {}
        if d.get("success"):
            continue
        advice = str(d.get("error") or "")
        nxt = (results[i + 1].get("data") or {})
        if nxt.get("success"):
            continue
        nxt_err = str(nxt.get("error") or "")
        if not re.search(r"unknown selector|unsupported|not supported|HTML-only",
                         nxt_err, re.I):
            continue
        used = _next_selector(calls, i)
        if not used:
            continue
        # Match on FORM, not on the literal string. The observed defect was
        # advice offering `<body>` on a .py file and the model reaching for
        # `<script>`: a different tag, the same unsupported shape, so an
        # exact-string check would miss the very case this exists to catch.
        advised_forms = _selector_forms(advice)
        if _selector_form(used) in advised_forms:
            out.append(f"H3 dead-end steering: a rejection recommended a "
                       f"{_selector_form(used)} selector, and the next call was "
                       f"refused for using {used!r}")
    return out


def _next_selector(calls: list[dict], i: int) -> str:
    """The selector that the call after the i-th result used, or "" when there is none."""
    used = ""
    if i + 1 < len(calls):
        used = str(((calls[i + 1].get("data") or {}).get("args") or {})
                   .get("selector") or "")
    return used


def _selector_form(sel: str) -> str:
    sel = sel.strip()
    if sel.startswith("<") and sel.endswith(">"):
        return "<tag>"
    if sel.startswith("function:"):
        return "function:NAME"
    if sel.startswith("class:"):
        return "class:NAME"
    return "other"


def _selector_forms(text: str) -> set[str]:
    forms = set()
    if re.search(r"<[a-zA-Z][\w-]*>", text):
        forms.add("<tag>")
    if "function:" in text:
        forms.add("function:NAME")
    if "class:" in text:
        forms.add("class:NAME")
    return forms


def h4_gate_escape(s: Session, task: Task = None) -> list[str]:
    """An action-intent session that exited having changed nothing, ungated.

    The D11 class: one gate spent the shared bounce budget, so the
    done-without-action gate never ran.
    """
    # A question SHOULD exit without writing. Scoring that as a gate escape
    # reported a defect on both conversational probes for behaving exactly as
    # asked — the inverse of the H9 check sitting right below.
    if task is not None and task.conversational:
        return []
    # Read the tool name off the RESULT rather than pairing positionally with
    # the calls: one unanswered call (a client timeout mid-stream) shifted
    # every pair after it and silently mis-scored the rest of the session.
    productive = any((e.get("data") or {}).get("success")
                     and (e.get("data") or {}).get("tool") in WRITE_TOOLS
                     for e in s.of_type("tool_result"))
    if productive:
        return []
    if not s.of_type("done"):
        return []
    # A classified terminal answers this directly: anything but "completed"
    # is the run saying it did not finish, which is the opposite of escaping.
    # Absent or unrecognised falls back to the prose match below rather than
    # being read as completion.
    for e in s.of_type("done"):
        raw = (e.get("data") or {}).get("status")
        if isinstance(raw, str) and raw in TERMINAL_STATUSES and raw != "completed":
            return []
    # The breaker ending honestly is not an escape — it says it stopped.
    summary, texts = _summary_and_texts(s)
    # "Stopped:" is how the repeated-refusal breaker opens its summary, and
    # "ran out of turns" is how the turn-cap exit does. Matching only
    # "stopped after" scored both of those as escaping silently while they
    # were saying exactly what happened.
    if re.search(r"\bstopped\b|could not|unable to|failed to|ran out of turns|"
                 r"nothing was written|no changes were made",
                 summary + texts, re.I):
        return []
    return ["H4 gate escape: exited with no successful write on an "
            "action-intent prompt, without saying it had stopped"]


def _summary_and_texts(s: Session) -> tuple[str, str]:
    """The summaries of the session's done events, and the content of its text
    events, each joined into one string."""
    summary = " ".join(str((e.get("data") or {}).get("summary") or "")
                       for e in s.of_type("done"))
    texts = " ".join(str((e.get("data") or {}).get("content") or "")
                     for e in s.of_type("text"))
    return summary, texts


def _reads(p: Path) -> str | None:
    try:
        return p.read_text()
    except (OSError, UnicodeDecodeError):
        return None


def h5_corrupt_write(s: Session, task: Task) -> list[str]:
    out = []
    for p in sorted(s.workspace.rglob("*")):
        if not p.is_file() or p.suffix not in (".py", ".html", ".htm", ".js"):
            continue
        # A file the session never touched cannot be a corrupt WRITE. Measured:
        # bugfind_tiebreak is seeded with ATLAS's own v3-service/adapters.py,
        # which contains a regex literal for stripping script tags. The
        # extractor's own `<script>` pattern matches inside that literal,
        # pulls out `]*\bsrc=)[^>]*>(.*?)`, hands that to `node --check`, and
        # reports the pristine fixture as unparseable. Both reps of the task
        # lost harness integrity over a file they never opened — the sessions
        # wrote nothing at all (quality.files == 0).
        #
        # Seeded bytes are the reference, not an allowlist: the moment the
        # session changes the file it is checked like anything else, so a
        # fixture the agent genuinely corrupts is still caught.
        seeded = task.files.get(p.name)
        if seeded is not None and _reads(p) == seeded:
            continue
        ok, why = _file_parses(p)
        if not ok:
            out.append(f"H5 corrupt write: {p.name} left unparseable ({why})")
    for name in task.must_exist:
        if not (s.workspace / name).exists():
            out.append(f"H5 corrupt write: {name} was deleted")
    return out


def _recovered_after(s: Session, idx: int) -> bool:
    """True when the session kept working after the event at `idx`.

    A successful tool call plus a `done` afterwards means the proxy caught
    the condition, told the model, and the model carried on.
    """
    rest = s.events[idx + 1:]
    worked = any(e.get("type") == "tool_result"
                 and (e.get("data") or {}).get("success") for e in rest)
    finished = any(e.get("type") == "done" for e in rest)
    return worked and finished


def model_output_guards(s: Session) -> list[str]:
    """The categories of the proxy's model-output guards in a session.

    A guard is the proxy catching the model's own malformed output and telling
    it: a parse failure, content swallowed by an unescaped quote, or content
    whose intended bytes were ambiguous. Every such error event carries a
    "category". The plumbing worked, so these are counted for the summary and
    never as a harness defect (h6_service_fault).
    """
    return [str((ev.get("data") or {}).get("category"))
            for ev in s.of_type("error") if (ev.get("data") or {}).get("category")]


def _ended_on_work_deadline(s: Session) -> bool:
    for ev in reversed(s.of_type("done")):
        d = ev.get("data") or {}
        return d.get("reason") == "work_deadline" or d.get("status") == "timed_out"
    return False


def h6_service_fault(s: Session) -> list[str]:
    out = []
    for idx, ev in enumerate(s.events):
        if ev.get("type") != "error":
            continue
        d = ev.get("data") or {}
        # A model-output guard is not a service fault, recovered or not: see
        # model_output_guards. Smoke run 2026-09-27 (smallrung_toml): the
        # swallowed_content guard caught a tool call cut by an unescaped quote,
        # told the model, and the run still showed "1 harness defect" for it.
        if d.get("category"):
            continue
        detail = _error_detail(d)
        # The cap event is this runner's own, appended when it stops reading
        # at --timeout. Counting it as a service fault charges our deadline
        # to the proxy a second time — h1_protocol already reports it as the
        # timeout it is.
        if "harness cap:" in str(detail):
            continue
        # The session's own work deadline cut an LLM stream in flight. The
        # terminal status already reports that (timed_out, work_deadline); no
        # dependency failed. Smoke run 2026-09-28 (multifile_cli rep 2).
        if ("context deadline exceeded" in str(detail)
                or "context canceled" in str(detail)) and _ended_on_work_deadline(s):
            continue
        # A parse failure the session recovered from is the proxy doing its
        # job, not a service outage. Measured 2026-08-03 on flask_pause rep2:
        # the model emitted a 20 KB tool call that ran out of tokens
        # mid-JSON, the proxy classified it (category=truncated_tool), told
        # the model, and the session went on to pass the task — and was
        # scored a harness defect for it. Counting recovered model behaviour
        # here puts a floor under harness integrity that no amount of
        # correct proxy behaviour can lift. An unrecovered one still counts:
        # that is a session the model never got back from.
        if "parse model response" in str(detail) and _recovered_after(s, idx):
            continue
        out.append(f"H6 service fault: error event {str(detail)[:160]!r}")
    _tool_result_faults(s, out)
    return out


def _error_detail(d: dict) -> str:
    """The text of an error event, from the data the event carries."""
    # The proxy's error events carry "error" (see the TUI's own case);
    # "message" is what this harness uses for a stream-level failure it
    # synthesises. Reading only one of them reported every real error as
    # the string "None".
    detail = d.get("error") or d.get("message") or json.dumps(d)[:120]
    return detail


def _tool_result_faults(s: Session, out: list[str]) -> None:
    """Add to `out` a service fault for each tool result whose error names a
    failed service."""
    for ev in s.of_type("tool_result"):
        err = str((ev.get("data") or {}).get("error") or "")
        if re.search(r"\b5\d\d\b.*(?:proxy|v3|lens|sandbox)|connection refused|"
                     r"service unavailable|internal server error", err, re.I):
            out.append(f"H6 service fault: {err[:120]!r}")


def h9_tier_misapplied(s: Session, task: Task) -> list[str]:
    """V3 ran, or files were edited, in answer to a question.

    The tiers exist so the heavy pipeline does not run on everything. A
    question should be answered from the conversational tier: a wrong answer
    is a model limit, but spending a multi-minute V3 pipeline on "what does
    this function do" is a product defect that costs the user minutes, and
    editing code nobody asked to have edited is worse than slow.
    """
    if not task.conversational:
        return []
    out = []
    v3 = [e for e in s.events
          if str(e.get("type") or "").startswith("v3_")]
    if v3:
        kinds = sorted({str(e.get("type")) for e in v3})[:4]
        out.append(f"H9 tier: the V3 pipeline ran on a question ({kinds})")
    # Same positional-pairing hazard as H4 — read the tool off the result.
    wrote = [r for r in s.of_type("tool_result")
             if (r.get("data") or {}).get("tool") in WRITE_TOOLS
             and (r.get("data") or {}).get("success")]
    if wrote:
        names = sorted({(r.get("data") or {}).get("tool") for r in wrote})
        out.append(f"H9 tier: a question caused file writes ({names})")
    return out


def h8_anchored_on_injected_text(s: Session) -> list[str]:
    """The model anchored an edit on text ATLAS injected, not on file content.

    read_file appends a call-graph footer to the content it returns, and the
    loop injects "[system note]:" correctives. Neither is on disk, so an
    old_str copied from them can never match — the edit fails through no fault
    of the model's, and it burns a turn (a measured session spent all three of
    its failures this way and stopped). Attributing that to the model would
    make the harness understate exactly the defects it exists to find.
    """
    markers = ("## Call graph (within this file)", "[system note]:",
               "--- end of ", "The lines below are ATLAS analysis")
    out = []
    for ev in s.of_type("tool_call"):
        d = ev.get("data") or {}
        old = str(((d.get("args") or {}).get("old_str")) or "")
        if not old:
            continue
        for m in markers:
            if m in old:
                out.append(f"H8 injected-text anchor: {d.get('name')} old_str "
                           f"copied ATLAS's own {m.strip()!r}, which is not on disk")
                break
    return out


def h7_background_leak(sandbox: str, s: Session) -> list[str]:
    """A background job may outlive the session, but not silently.

    Persistence is deliberate: an agent loop is one user message, so killing
    jobs at its end would break "start the dev server" followed by "now curl
    it". The defect is a job that keeps its port with nobody told — the next
    turn then fails on a bound port with no explanation. So this fires only
    when jobs are running AND the session never said so.
    """
    if not sandbox:
        return []
    p = subprocess.run(["docker", "exec", sandbox, "sh", "-c",
                        "ps -eo args | grep -v grep | grep -c 'python app' || true"],
                       capture_output=True, text=True, timeout=30)
    n = (p.stdout or "0").strip()
    if not (n.isdigit() and int(n) > 0):
        return []
    announced = any(
        "still running" in str((e.get("data") or {}).get("summary") or "").lower()
        or "stop_background" in str((e.get("data") or {}).get("summary") or "")
        for e in s.of_type("done"))
    if announced:
        return []
    return [f"H7 silent background leak: {n} job(s) still holding ports and the "
            f"session never said so"]


# --------------------------------------------------------------------------
# TUI coverage
# --------------------------------------------------------------------------

def tui_handled_types() -> set[str]:
    """Event types the TUI's dispatcher has a case for.

    Located by content marker rather than filename so a file move does not
    silently empty the set (the repo's contract-test convention).
    """
    out: set[str] = set()
    for go in sorted((REPO / "tui").glob("*.go")):
        if go.name.endswith("_test.go"):
            continue
        src = go.read_text()
        if "appendChatEvent" not in src:
            continue
        for m in re.finditer(r'^\s*case ((?:"[a-z0-9_]+"(?:,\s*)?)+):',
                             src, re.M):
            out.update(re.findall(r'"([a-z0-9_]+)"', m.group(1)))
    return out
