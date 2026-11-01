"""Producer/consumer drift check for the event protocol.

Every chat-SSE event type the proxy emits must have a handling path in
the TUI, and the TUI must not carry handlers for events nothing emits.
Same for the 7 typed-envelope types on /events. Exceptions require an
entry in the documented allowlists below — an empty allowlist is the
healthy state.
"""

import re
from pathlib import Path

from tests.contracts import go_source

REPO = Path(__file__).resolve().parents[3]

# Events the proxy may emit that the TUI intentionally does not render.
# Each entry needs a reason. Keep empty unless a real exception appears.
ALLOWED_UNCONSUMED: dict = {}

# Case labels in appendChatEvent that are not proxy chat events.
# "__turn_done__" is the TUI-internal turn-end sentinel injected by its
# own stream reader, not a wire event.
ALLOWED_EXTRA_CONSUMERS = {"__turn_done__"}


def _proxy_chat_events() -> set:
    events = set()
    for go in (REPO / "proxy").rglob("*.go"):
        if go.name.endswith("_test.go"):
            continue
        src = go.read_text()
        events.update(re.findall(r'ctx\.Stream(?:Fn)?\(\s*"([a-z0-9_]+)"', src))
    # v3StageToEvent maps v3-service stage names onto event types.
    tools = go_source("proxy", "func v3StageToEvent")
    fn = tools[tools.index("func v3StageToEvent"):]
    fn = fn[:fn.index("\nfunc ", 1)]
    events.update(re.findall(r'return "([a-z0-9_]+)"', fn))
    return events


def _tui_chat_handlers() -> set:
    src = go_source("tui", "func (m *tuiModel) appendChatEvent")
    start = src.index("func (m *tuiModel) appendChatEvent")
    body = src[start:]
    body = body[:body.index("\nfunc ", 1)]
    handled = set()
    # Only the top-level `switch ev.Type` cases (one-tab indent). Nested
    # switches (tool names inside the tool_result handler, stage names
    # inside v3_progress) are deeper-indented and are not event types.
    for m in re.finditer(r'\n\tcase ([^\n]+?):(?:\n|$)', body):
        handled.update(re.findall(r'"([a-z0-9_]+)"', m.group(1)))
    # Multi-line top-level case lists continue on the next line(s).
    for m in re.finditer(r'\n\tcase ([^\n]*,)\n((?:\t\t"[^\n]+\n)+)', body):
        handled.update(re.findall(r'"([a-z0-9_]+)"', m.group(0)))
    return handled


def test_every_emitted_chat_event_has_a_tui_handler():
    emitted = _proxy_chat_events()
    handled = _tui_chat_handlers()
    orphans = emitted - handled - set(ALLOWED_UNCONSUMED)
    assert not orphans, (
        f"proxy emits chat events the TUI never handles: {sorted(orphans)}. "
        "Wire a handler in appendChatEvent, or add a documented exception "
        "to ALLOWED_UNCONSUMED.")


def test_every_tui_handler_has_a_producer():
    emitted = _proxy_chat_events()
    handled = _tui_chat_handlers()
    dead = handled - emitted - ALLOWED_EXTRA_CONSUMERS
    assert not dead, (
        f"TUI handles chat events nothing emits: {sorted(dead)}. "
        "Remove the dead handler, or add the producer.")


def test_envelope_types_agree_across_implementations():
    """The 7 typed-envelope types must match across the Go producer
    (proxy/events.go), the Go consumer (the TUI), and the Python spec
    (atlas/events.py)."""
    go_src = (REPO / "proxy" / "events.go").read_text()
    go_types = set(re.findall(r'Evt\w+\s*=\s*"([a-z_]+)"', go_src))

    py_src = (REPO / "atlas" / "events.py").read_text()
    m = re.search(r"EVENT_TYPES\s*=\s*\(([^)]+)\)", py_src)
    assert m, "EVENT_TYPES not found in atlas/events.py"
    py_types = set(re.findall(r'"([a-z_]+)"', m.group(1)))

    tui_src = go_source("tui", "Envelope")
    tui_types = set(re.findall(r'Evt\w+\s*=\s*"([a-z_]+)"', tui_src))

    assert go_types == py_types, (
        f"envelope types diverge: proxy {sorted(go_types)} vs "
        f"events.py {sorted(py_types)}")
    assert go_types == tui_types, (
        f"envelope types diverge: proxy {sorted(go_types)} vs "
        f"the TUI {sorted(tui_types)}")
