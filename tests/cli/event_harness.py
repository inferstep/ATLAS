"""A small SSE consumer of the atlas event stream, used only by the tests.

The wire format itself (EVENT_TYPES, Event, parse_envelope) lives in
atlas.events; test_events.py checks this reader offline and
tests/e2e/test_events_live.py runs it against a real proxy.
"""

import urllib.request
from typing import Iterator, Optional

from atlas.events import Event, LegacyEventError, parse_envelope


def iter_sse_lines(stream) -> Iterator[str]:
    """Yield decoded `data:` lines from an SSE byte stream. Handles the
    standard SSE framing: lines prefixed `data: `, blank line as event
    delimiter. `event:` lines are echoed as `<event-name>: <data>` so the
    caller can distinguish stream-control events (`event: result`,
    `event: done`) from plain `data:` events.
    """
    pending_event: Optional[str] = None
    for raw in stream:
        if isinstance(raw, bytes):
            line = raw.decode("utf-8", errors="replace").rstrip("\n").rstrip("\r")
        else:
            line = raw.rstrip("\n").rstrip("\r")
        if not line:
            pending_event = None  # event boundary
            continue
        if line.startswith(":"):
            continue  # SSE comment / heartbeat
        if line.startswith("event:"):
            pending_event = line[len("event:"):].strip()
            continue
        if line.startswith("data:"):
            data = line[len("data:"):].lstrip()
            if pending_event:
                yield f"{pending_event}: {data}"
            else:
                yield data


def iter_events(url: str, timeout: float = 30.0) -> Iterator[Event]:
    """Stream typed Event objects from an SSE URL until the server closes
    the connection.

    Legacy `{stage, detail}` events are silently skipped — the caller
    explicitly opted into typed events via the Accept header.
    """
    request = urllib.request.Request(
        url, headers={"Accept": "application/json+envelope, text/event-stream"})
    with urllib.request.urlopen(request, timeout=timeout) as resp:
        for data in iter_sse_lines(resp):
            if (data == "[DONE]" or data.startswith("done: ")
                    or data.startswith("result: ") or data == ""):
                # done/result are stream-control frames (the v3 legacy
                # terminal frame rides `event: result`), not envelopes.
                continue
            try:
                yield parse_envelope(data)
            except LegacyEventError:
                continue  # caller opted into typed; skip legacy frames
