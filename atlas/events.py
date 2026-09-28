"""ATLAS event protocol — typed events streaming over SSE (PC-061).

This module is the canonical Python definition of the event envelope
and the consumer helpers. atlas-proxy's `/events` broker emits JSON in
this shape; the Go TUI consumes it via its own implementation of the
same contract (tui/consumer.go), and the tests here consume it via
`iter_events(url)`.

The schema is also documented in docs/PROTOCOL.md; this docstring is
the executable spec.

Envelope shape
--------------

    {
      "event_id":    "evt_<8 hex>",
      "timestamp":   <float — Unix seconds with microsecond precision>,
      "type":        "stage_start" | "stage_end" | "tool_call" |
                     "tool_result" | "metric" | "error" | "done",
      "stage":       <str — pipeline stage name; e.g., "phase2", "pr_cot">,
      "duration_ms": <int> | null,           # set on stage_end / tool_result
      "payload":     { ... type-specific ... }
    }

Per-type payload contracts
--------------------------

  stage_start   {detail?: str}
  stage_end     {detail?: str, success: bool, summary?: str}
  tool_call     {name: str, args_summary: str}
  tool_result   {name: str, success: bool, summary?: str}
  metric        {name: str, value: number, unit?: str}
  error         {stage: str, message: str, recoverable: bool}
  done          {success: bool, total_duration_ms: int, summary?: str,
                 status?: str, reason?: str}

`status` and `reason` are additive. `summary` keeps its prose meaning, and a
consumer that predates them reads the event exactly as before; a consumer that
uses them must pass the raw value through `terminal_status`, which maps
absent, malformed and unrecognised values to "incomplete" rather than letting
an unnameable outcome read as success.

One `done` event closes each agent pass. `/events` is a persistent
broker stream: it keeps heartbeating after a `done`, so EOF-without-done
only means truncation for consumers reading a single pass.

Legacy shape
------------

v3-service's own SSE endpoints (`/v3/generate`, `/v3/plan`) emit the
legacy `{"stage": ..., "detail": ...}` shape (consumed by the Go proxy
bridge, proxy/v3_bridge.go — not by this module). `parse_envelope` raises
`LegacyEventError` on that shape so callers fall back explicitly rather
than silently mis-parsing.
"""

from __future__ import annotations

import json
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Dict, Optional


# ---------------------------------------------------------------------------
# Type registry — exhaustive list of legal envelope types
# ---------------------------------------------------------------------------

# Terminal outcomes carried by a `done` payload's optional `status` key. The
# proxy owns the vocabulary (proxy/types.go); this is the consumer half.
TERMINAL_STATUSES = (
    "completed",
    "incomplete",
    "stopped",
    "timed_out",
    "failed",
)


def terminal_status(payload: dict | None) -> str:
    """Read a `done` payload's outcome, failing closed.

    Absent, malformed, or unrecognised becomes "incomplete". A run whose
    outcome cannot be named did not finish, and the one thing this must never
    do is let that read as "completed".
    """
    if not isinstance(payload, dict):
        return "incomplete"
    raw = payload.get("status")
    if isinstance(raw, str) and raw in TERMINAL_STATUSES:
        return raw
    return "incomplete"


EVENT_TYPES = (
    "stage_start",
    "stage_end",
    "tool_call",
    "tool_result",
    "metric",
    "error",
    "done",
)


# ---------------------------------------------------------------------------
# Envelope dataclass
# ---------------------------------------------------------------------------

@dataclass
class Event:
    """A single envelope-shaped event. `payload` is intentionally a dict —
    the per-type fields are documented above and validated in
    `parse_envelope`, but kept as `dict` so producers can add new payload
    fields without breaking the consumer.
    """
    event_id: str
    timestamp: float
    type: str
    stage: str
    payload: Dict[str, Any] = field(default_factory=dict)
    duration_ms: Optional[int] = None

    def to_dict(self) -> Dict[str, Any]:
        d: Dict[str, Any] = {
            "event_id": self.event_id,
            "timestamp": self.timestamp,
            "type": self.type,
            "stage": self.stage,
            "payload": self.payload,
        }
        if self.duration_ms is not None:
            d["duration_ms"] = self.duration_ms
        return d

    def to_json(self) -> str:
        return json.dumps(self.to_dict(), separators=(",", ":"))


# ---------------------------------------------------------------------------
# Errors
# ---------------------------------------------------------------------------

class EventError(ValueError):
    """Base class for all envelope parsing errors."""


class LegacyEventError(EventError):
    """The blob looked like the legacy {stage, detail} shape, not an
    envelope. Callers can catch this to fall back to legacy handling."""


class SchemaError(EventError):
    """Envelope was malformed — missing required field, wrong type,
    unknown event_type, etc."""


# ---------------------------------------------------------------------------
# Producer helpers (used by tests and any Python producer; production
# emitters live in their own services)
# ---------------------------------------------------------------------------

def new_event_id() -> str:
    """Short, log-readable, session-unique. 8 hex chars from a uuid4 is
    enough — collision risk is negligible for any realistic stream."""
    return "evt_" + uuid.uuid4().hex[:8]


def make_event(type: str, stage: str, payload: Optional[Dict[str, Any]] = None,
                duration_ms: Optional[int] = None,
                timestamp: Optional[float] = None) -> Event:
    """Build a well-formed Event with sensible defaults.

    Validates `type` is one of EVENT_TYPES — catches typos at producer
    time rather than letting them through to the consumer.
    """
    if type not in EVENT_TYPES:
        raise SchemaError(f"unknown event type {type!r}; "
                          f"must be one of {EVENT_TYPES}")
    return Event(
        event_id=new_event_id(),
        timestamp=timestamp if timestamp is not None else time.time(),
        type=type,
        stage=stage,
        payload=payload or {},
        duration_ms=duration_ms,
    )


# ---------------------------------------------------------------------------
# Parser / validator
# ---------------------------------------------------------------------------

_REQUIRED = ("event_id", "timestamp", "type", "stage", "payload")
# v3-service legacy frames are {stage, detail} plus an optional "data" key
# when structured data rides along (v3-service/pipeline.py emit()).
_LEGACY_KEYS = {"stage", "detail", "data"}


def parse_envelope(blob: Any) -> Event:
    """Parse a JSON blob (str or dict) into an Event.

    Raises:
      LegacyEventError — the blob is the legacy {stage, detail} shape
      SchemaError       — the blob is malformed
    """
    if isinstance(blob, str):
        try:
            blob = json.loads(blob)
        except json.JSONDecodeError as e:
            raise SchemaError(f"not valid JSON: {e}") from e
    if not isinstance(blob, dict):
        raise SchemaError(f"envelope must be a JSON object, got {type(blob).__name__}")

    # Legacy detection: a subset of the legacy keyset containing "stage",
    # with no envelope keys.
    keys = set(blob.keys())
    if (keys <= _LEGACY_KEYS and "stage" in keys
            and "type" not in keys and "event_id" not in keys):
        raise LegacyEventError(
            f"blob is the legacy {{stage, detail}} shape, not an envelope: "
            f"{blob!r}. Opt into v2 events via the "
            f"Accept: application/json+envelope header.")

    for key in _REQUIRED:
        if key not in blob:
            raise SchemaError(f"missing required field {key!r}: {blob!r}")

    if blob["type"] not in EVENT_TYPES:
        raise SchemaError(f"unknown event type {blob['type']!r}; "
                          f"must be one of {EVENT_TYPES}")
    if not isinstance(blob["timestamp"], (int, float)):
        raise SchemaError(f"timestamp must be a number, got "
                          f"{type(blob['timestamp']).__name__}")
    if not isinstance(blob["payload"], dict):
        raise SchemaError(f"payload must be an object, got "
                          f"{type(blob['payload']).__name__}")

    return Event(
        event_id=blob["event_id"],
        timestamp=float(blob["timestamp"]),
        type=blob["type"],
        stage=blob["stage"],
        payload=blob["payload"],
        duration_ms=blob.get("duration_ms"),
    )
