"""Tests for atlas.events (PC-061) — event protocol schema + consumer.

The schema is the contract every producer (v3-service Python, atlas-proxy
Go) implements. These tests pin the contract: a malformed envelope must
fail with a clear message, a legacy `{stage, detail}` blob must be
detected (not silently mis-parsed), and the round-trip
`make_event → to_json → parse_envelope` must be exact.
"""

import io
import re
import urllib.request

import pytest

from atlas.events import (
    EVENT_TYPES,
    TERMINAL_STATUSES,
    LegacyEventError,
    SchemaError,
    make_event,
    new_event_id,
    parse_envelope,
    terminal_status,
)
from tests.cli.event_harness import iter_events, iter_sse_lines


# --- making events ----------------------------------------------------------

def test_event_ids_are_evt_plus_8_hex_and_unique():
    ids = {new_event_id() for _ in range(1000)}
    assert len(ids) == 1000
    for eid in ids:
        assert re.fullmatch(r"evt_[0-9a-f]{8}", eid), eid


def test_make_event_rejects_unknown_type():
    with pytest.raises(SchemaError, match="unknown event type"):
        make_event("bogus", "phase2")


@pytest.mark.parametrize("etype", EVENT_TYPES)
def test_make_event_builds_every_legal_type_with_defaults(etype):
    ev = make_event(etype, "phase2")
    assert ev.type == etype
    assert ev.stage == "phase2"
    assert ev.event_id.startswith("evt_")
    assert isinstance(ev.timestamp, float)
    assert "duration_ms" not in ev.to_dict()  # unset optional fields are left out


def test_event_round_trips_through_compact_json_and_through_dict():
    ev = make_event("tool_result", "phase2", payload={"name": "edit_file", "success": True},
                    duration_ms=4523, timestamp=1.5)
    assert ev.timestamp == 1.5
    assert ev.duration_ms == 4523

    raw = ev.to_json()
    assert ", " not in raw and ": " not in raw  # compact separators
    assert parse_envelope(raw) == ev
    assert parse_envelope(ev.to_dict()) == ev


# --- parse_envelope: schema errors and legacy frames ------------------------

VALID_ENVELOPE = {
    "event_id": "evt_aabb1122",
    "timestamp": 1.0,
    "type": "stage_start",
    "stage": "phase2",
    "payload": {},
}


@pytest.mark.parametrize("blob, message", [
    pytest.param("{not json", "not valid JSON", id="invalid json"),
    pytest.param("[1, 2, 3]", "must be a JSON object", id="json array"),
    pytest.param({**VALID_ENVELOPE, "type": "weird_type"}, "unknown event type",
                 id="unknown type"),
    pytest.param({**VALID_ENVELOPE, "timestamp": "not-a-number"}, "timestamp must be a number",
                 id="text timestamp"),
    pytest.param({**VALID_ENVELOPE, "payload": "not-an-object"}, "payload must be an object",
                 id="text payload"),
])
def test_parse_rejects_a_malformed_envelope(blob, message):
    with pytest.raises(SchemaError, match=message):
        parse_envelope(blob)


@pytest.mark.parametrize("missing", ["event_id", "timestamp", "type", "stage", "payload"])
def test_parse_rejects_missing_required_field(missing):
    blob = {key: value for key, value in VALID_ENVELOPE.items() if key != missing}
    with pytest.raises(SchemaError, match=f"missing required field '{missing}'"):
        parse_envelope(blob)


@pytest.mark.parametrize("blob", [
    pytest.param('{"stage": "phase2", "detail": "allocating"}', id="stage and detail"),
    pytest.param('{"stage": "phase2"}', id="stage only"),
])
def test_parse_flags_the_legacy_stage_detail_shape(blob):
    """v3-service emits `{stage, detail}`, and `{stage}` alone when it has no
    detail. Both must be flagged as legacy, not parsed as a broken envelope."""
    with pytest.raises(LegacyEventError, match="legacy"):
        parse_envelope(blob)


# --- reading an SSE stream --------------------------------------------------

@pytest.mark.parametrize("lines, expected", [
    pytest.param([b'data: {"a": 1}\n', b"\n", b'data: {"b": 2}\n', b"\n"],
                 ['{"a": 1}', '{"b": 2}'], id="data lines"),
    pytest.param([b"event: result\n", b'data: {"final": true}\n', b"\n"],
                 ['result: {"final": true}'], id="event name prefixes its data"),
    pytest.param([b": heartbeat\n", b'data: {"x": 1}\n', b"\n"],
                 ['{"x": 1}'], id="comment heartbeat skipped"),
    pytest.param(['data: {"a": 1}\n', "\n", 'data: {"b": 2}\n', "\n"],
                 ['{"a": 1}', '{"b": 2}'], id="str lines read like bytes"),
])
def test_iter_sse_lines(lines, expected):
    assert list(iter_sse_lines(lines)) == expected


def test_iter_events_skips_sse_control_frames(monkeypatch):
    """The proxy's /events stream carries `: connected` and `: heartbeat`
    comments; v3-service ends with an `event: result` frame and `[DONE]`, and
    its legacy frames have no envelope. iter_events must skip all of them
    without raising and yield only the envelope."""
    stream = io.BytesIO(
        b": connected\n\n"
        b": heartbeat\n\n"
        b'data: {"stage": "phase2", "detail": "allocating"}\n\n'
        b'data: {"event_id":"evt_aabb","timestamp":1.0,"type":"metric",'
        b'"stage":"x","payload":{"name":"y","value":1}}\n\n'
        b'event: result\ndata: {"final":true}\n\n'
        b"data: [DONE]\n\n"
    )
    monkeypatch.setattr(urllib.request, "urlopen", lambda request, timeout: stream)
    assert [ev.type for ev in iter_events("http://proxy/events")] == ["metric"]


def test_known_pipeline_sequence_round_trips_via_sse():
    """A realistic run, written as SSE and read back through iter_sse_lines
    and parse_envelope, arrives complete, in order and unchanged: the
    timestamps still rise and the last event is still `done`."""
    sequence = [
        make_event("stage_start", "probe", timestamp=100.0),
        make_event("stage_end", "probe", timestamp=100.5, duration_ms=500,
                   payload={"success": True}),
        make_event("stage_start", "phase2", timestamp=100.6),
        make_event("tool_call", "phase2", timestamp=100.7,
                   payload={"name": "edit_file", "args_summary": "src/x.py"}),
        make_event("tool_result", "phase2", timestamp=101.2, duration_ms=500,
                   payload={"name": "edit_file", "success": True}),
        make_event("metric", "lens", timestamp=101.3,
                   payload={"name": "gx_score", "value": 0.83}),
        make_event("stage_end", "phase2", timestamp=101.4, payload={"success": True}),
        make_event("done", "pipeline", timestamp=101.5,
                   payload={"success": True, "total_duration_ms": 1500}),
    ]
    stream = io.BytesIO(b"".join(f"data: {ev.to_json()}\n\n".encode() for ev in sequence))

    received = [parse_envelope(line) for line in iter_sse_lines(stream)]

    assert received == sequence


# --- terminal status: the consumer half of the proxy's outcome contract -----
#
# `summary` is prose, so every consumer that needed to know whether a run
# finished was matching English. `status` is the contract; the one rule that
# matters is that anything unnameable reads as incomplete, never completed.

def test_terminal_status_reads_the_classified_outcomes():
    for s in TERMINAL_STATUSES:
        assert terminal_status({"status": s}) == s


@pytest.mark.parametrize("payload", [
    pytest.param(None, id="no payload"),
    pytest.param("not a dict", id="text payload"),
    pytest.param({}, id="no status"),
    pytest.param({"status": None}, id="null status"),
    pytest.param({"status": ""}, id="empty status"),
    pytest.param({"status": "COMPLETED"}, id="wrong case"),
    pytest.param({"status": "success"}, id="unknown word success"),
    pytest.param({"status": "ok"}, id="unknown word ok"),
    pytest.param({"status": 1}, id="number status"),
    pytest.param({"status": ["completed"]}, id="list status"),
    pytest.param({"summary": "Made your change — it's on disk."}, id="prose summary only"),
    # A proxy that predates `status`: its done event still parses (see the
    # pipeline round trip above), but it must never read as completed.
    pytest.param({"success": True, "total_duration_ms": 1200, "summary": "all done"},
                 id="done payload from before status"),
])
def test_absent_or_malformed_status_fails_closed(payload):
    assert terminal_status(payload) == "incomplete"
