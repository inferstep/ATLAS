"""Tests for atlas.events (PC-061) — event protocol schema + consumer.

The schema is the contract every producer (v3-service Python, atlas-proxy
Go) implements. These tests pin the contract: a malformed envelope must
fail with a clear message, a legacy `{stage, detail}` blob must be
detected (not silently mis-parsed), and the round-trip
`make_event → to_json → parse_envelope` must be exact.
"""

import io
import time

import pytest

from atlas.events import (
    EVENT_TYPES,
    LegacyEventError,
    SchemaError,
    make_event,
    new_event_id,
    parse_envelope,
)
from tests.cli.event_harness import assert_monotonic, is_terminal, iter_sse_lines


# ---------------------------------------------------------------------------
# new_event_id + make_event
# ---------------------------------------------------------------------------

def test_event_id_format_is_short_and_prefixed():
    eid = new_event_id()
    assert eid.startswith("evt_")
    assert len(eid) == len("evt_") + 8
    # all hex after the prefix
    int(eid[len("evt_"):], 16)


def test_event_ids_are_unique_across_many_calls():
    ids = {new_event_id() for _ in range(1000)}
    assert len(ids) == 1000


def test_make_event_rejects_unknown_type():
    with pytest.raises(SchemaError, match="unknown event type"):
        make_event("bogus", "phase2")


@pytest.mark.parametrize("etype", EVENT_TYPES)
def test_make_event_accepts_every_legal_type(etype):
    ev = make_event(etype, "phase2")
    assert ev.type == etype
    assert ev.stage == "phase2"
    assert ev.event_id.startswith("evt_")
    assert isinstance(ev.timestamp, float)


def test_make_event_uses_provided_timestamp():
    ev = make_event("stage_start", "phase2", timestamp=1.5)
    assert ev.timestamp == 1.5


def test_make_event_carries_duration():
    ev = make_event("stage_end", "phase2", duration_ms=4523)
    assert ev.duration_ms == 4523


# ---------------------------------------------------------------------------
# Event.to_json / to_dict shape
# ---------------------------------------------------------------------------

def test_to_dict_omits_none_optional_fields():
    ev = make_event("stage_start", "phase2", payload={"detail": "x"})
    d = ev.to_dict()
    assert "duration_ms" not in d


def test_to_dict_includes_set_optional_fields():
    ev = make_event("stage_end", "phase2", duration_ms=10)
    d = ev.to_dict()
    assert d["duration_ms"] == 10


def test_to_json_is_compact_and_round_trips():
    ev = make_event("metric", "lens",
                     payload={"name": "gx_score", "value": 0.83})
    raw = ev.to_json()
    # Compact separators — no spaces between keys.
    assert ", " not in raw
    assert ": " not in raw
    parsed = parse_envelope(raw)
    assert parsed.type == "metric"
    assert parsed.payload == {"name": "gx_score", "value": 0.83}


# ---------------------------------------------------------------------------
# parse_envelope — schema validation
# ---------------------------------------------------------------------------

def test_parse_accepts_dict_input():
    ev = make_event("done", "pipeline",
                     payload={"success": True, "total_duration_ms": 12345})
    parsed = parse_envelope(ev.to_dict())
    assert parsed.event_id == ev.event_id


def test_parse_rejects_non_object_blob():
    with pytest.raises(SchemaError, match="must be a JSON object"):
        parse_envelope("[1, 2, 3]")


def test_parse_rejects_invalid_json():
    with pytest.raises(SchemaError, match="not valid JSON"):
        parse_envelope("{not json")


@pytest.mark.parametrize("missing", ["event_id", "timestamp", "type", "stage", "payload"])
def test_parse_rejects_missing_required_field(missing):
    blob = {
        "event_id": "evt_aabb1122",
        "timestamp": 1.0,
        "type": "stage_start",
        "stage": "phase2",
        "payload": {},
    }
    del blob[missing]
    with pytest.raises(SchemaError, match=f"missing required field '{missing}'"):
        parse_envelope(blob)


def test_parse_rejects_unknown_event_type():
    blob = {
        "event_id": "evt_aabb1122",
        "timestamp": 1.0,
        "type": "weird_type",
        "stage": "phase2",
        "payload": {},
    }
    with pytest.raises(SchemaError, match="unknown event type"):
        parse_envelope(blob)


def test_parse_rejects_non_numeric_timestamp():
    blob = {
        "event_id": "evt_aabb1122",
        "timestamp": "not-a-number",
        "type": "stage_start",
        "stage": "phase2",
        "payload": {},
    }
    with pytest.raises(SchemaError, match="timestamp must be a number"):
        parse_envelope(blob)


def test_parse_rejects_non_object_payload():
    blob = {
        "event_id": "evt_aabb1122",
        "timestamp": 1.0,
        "type": "stage_start",
        "stage": "phase2",
        "payload": "not-an-object",
    }
    with pytest.raises(SchemaError, match="payload must be an object"):
        parse_envelope(blob)


# ---------------------------------------------------------------------------
# Legacy detection — critical for back-compat window
# ---------------------------------------------------------------------------

def test_parse_detects_full_legacy_shape():
    """The exact `{stage, detail}` shape v3-service emits today must be
    flagged as legacy, not silently parsed as broken envelope."""
    legacy = '{"stage": "phase2", "detail": "allocating"}'
    with pytest.raises(LegacyEventError, match="legacy"):
        parse_envelope(legacy)


def test_parse_detects_stage_only_legacy_shape():
    """v3-service occasionally omits detail: emit_sse(stage) → {stage} only.
    That must also be detected as legacy."""
    legacy = '{"stage": "phase2"}'
    with pytest.raises(LegacyEventError):
        parse_envelope(legacy)


def test_parse_does_not_treat_envelope_as_legacy():
    """An envelope that happens to have a 'stage' key (it always does)
    must NOT be flagged as legacy."""
    ev = make_event("stage_start", "phase2", payload={"detail": "x"})
    parsed = parse_envelope(ev.to_json())
    assert parsed.type == "stage_start"


# ---------------------------------------------------------------------------
# SSE line iteration
# ---------------------------------------------------------------------------

def test_iter_sse_lines_yields_data_payloads():
    raw = (b"data: {\"a\": 1}\n"
           b"\n"
           b"data: {\"b\": 2}\n"
           b"\n")
    out = list(iter_sse_lines(io.BytesIO(raw).readlines()))
    assert out == ['{"a": 1}', '{"b": 2}']


def test_iter_sse_lines_handles_event_name_lines():
    raw = (b"event: result\n"
           b"data: {\"final\": true}\n"
           b"\n")
    out = list(iter_sse_lines(io.BytesIO(raw).readlines()))
    # event-named frames come through prefixed so callers can route them
    assert out == ['result: {"final": true}']


def test_iter_sse_lines_skips_comment_heartbeats():
    raw = (b": heartbeat\n"
           b"data: {\"x\": 1}\n"
           b"\n")
    out = list(iter_sse_lines(io.BytesIO(raw).readlines()))
    assert out == ['{"x": 1}']


def test_iter_sse_lines_handles_str_input():
    """Some test fixtures and in-process producers will hand us str, not bytes."""
    lines = ["data: {\"a\": 1}\n", "\n", "data: {\"b\": 2}\n", "\n"]
    out = list(iter_sse_lines(iter(lines)))
    assert out == ['{"a": 1}', '{"b": 2}']


# ---------------------------------------------------------------------------
# Sequencing helpers
# ---------------------------------------------------------------------------

def test_is_terminal_only_for_done():
    for t in EVENT_TYPES:
        ev = make_event(t, "x")
        assert is_terminal(ev) == (t == "done")


def test_assert_monotonic_passes_on_sorted_stream():
    seq = [make_event("stage_start", "x", timestamp=t) for t in (1.0, 1.5, 2.0)]
    assert_monotonic(seq)  # no raise


def test_assert_monotonic_tolerates_equal_timestamps():
    seq = [make_event("metric", "x", timestamp=1.0) for _ in range(3)]
    assert_monotonic(seq)


def test_assert_monotonic_raises_on_decreasing_timestamp():
    seq = [
        make_event("stage_start", "x", timestamp=2.0),
        make_event("stage_end", "x", timestamp=1.5),
    ]
    with pytest.raises(SchemaError, match="non-monotonic"):
        assert_monotonic(seq)


# ---------------------------------------------------------------------------
# End-to-end pipeline shape — pins the contract callers will rely on
# ---------------------------------------------------------------------------

def test_iter_events_skips_sse_control_frames():
    """The atlas-proxy /events endpoint emits `: connected` and
    `: heartbeat` SSE comment lines. iter_events must skip them
    (they're not envelopes) without raising. Same for `event: result`
    legacy framing on v3-service's done sequence."""
    from atlas.events import parse_envelope, LegacyEventError
    from tests.cli.event_harness import iter_sse_lines
    raw = (
        b": connected\n\n"
        b": heartbeat\n\n"
        b"data: {\"event_id\":\"evt_aabb\",\"timestamp\":1.0,\"type\":\"metric\","
        b"\"stage\":\"x\",\"payload\":{\"name\":\"y\",\"value\":1}}\n\n"
        b": heartbeat\n\n"
        b"event: result\ndata: {\"final\":true}\n\n"
        b"data: [DONE]\n\n"
    )
    parsed = []
    for line in iter_sse_lines(io.BytesIO(raw).readlines()):
        if line == "[DONE]" or line.startswith("result: "):
            continue
        try:
            parsed.append(parse_envelope(line))
        except LegacyEventError:
            continue
    assert len(parsed) == 1
    assert parsed[0].type == "metric"


def test_known_pipeline_sequence_round_trips_via_sse():
    """Simulate a realistic event sequence emitted over SSE, then consume it
    through iter_sse_lines + parse_envelope to confirm the full pipeline
    works end-to-end."""
    t = time.time()
    sequence = [
        make_event("stage_start", "probe", timestamp=t),
        make_event("stage_end", "probe", timestamp=t + 0.5,
                    duration_ms=500,
                    payload={"success": True}),
        make_event("stage_start", "phase2", timestamp=t + 0.6),
        make_event("tool_call", "phase2", timestamp=t + 0.7,
                    payload={"name": "edit_file", "args_summary": "src/x.py"}),
        make_event("tool_result", "phase2", timestamp=t + 1.2,
                    duration_ms=500,
                    payload={"name": "edit_file", "success": True}),
        make_event("metric", "lens", timestamp=t + 1.3,
                    payload={"name": "gx_score", "value": 0.83}),
        make_event("stage_end", "phase2", timestamp=t + 1.4,
                    payload={"success": True}),
        make_event("done", "pipeline", timestamp=t + 1.5,
                    payload={"success": True, "total_duration_ms": 1500}),
    ]
    sse_bytes = b"".join(
        f"data: {ev.to_json()}\n\n".encode() for ev in sequence
    )
    received = []
    for line in iter_sse_lines(io.BytesIO(sse_bytes).readlines()):
        received.append(parse_envelope(line))

    assert len(received) == len(sequence)
    assert [ev.type for ev in received] == [ev.type for ev in sequence]
    assert is_terminal(received[-1])
    assert_monotonic(received)


# --- terminal status: the consumer half of the proxy's outcome contract -----
#
# `summary` is prose, so every consumer that needed to know whether a run
# finished was matching English. `status` is the contract; the one rule that
# matters is that anything unnameable reads as incomplete, never completed.

def test_terminal_status_reads_the_classified_outcomes():
    from atlas.events import terminal_status, TERMINAL_STATUSES
    for s in TERMINAL_STATUSES:
        assert terminal_status({"status": s}) == s


@pytest.mark.parametrize("payload", [
    None,
    {},
    {"status": None},
    {"status": ""},
    {"status": "COMPLETED"},
    {"status": "success"},
    {"status": "ok"},
    {"status": 1},
    {"status": ["completed"]},
    {"summary": "Made your change — it's on disk."},
    "not a dict",
])
def test_absent_or_malformed_status_fails_closed(payload):
    from atlas.events import terminal_status
    assert terminal_status(payload) == "incomplete"


def test_a_legacy_done_payload_still_parses():
    """A proxy that predates `status` emits summary only, and that must stay
    a readable event — just never a completed one."""
    from atlas.events import terminal_status
    legacy = {"success": True, "total_duration_ms": 1200, "summary": "all done"}
    assert legacy["summary"] == "all done"
    assert terminal_status(legacy) == "incomplete"
