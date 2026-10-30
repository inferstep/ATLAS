"""The event protocol across the Go and Python boundary (PC-061), against a running proxy.

These tests start the real atlas-proxy binary and read its /events stream over HTTP with
iter_events from tests/cli/event_harness.py: the producer, the SSE wire and the consumer
are tested together.
"""

import os
import socket
import subprocess
import urllib.request

import pytest

from tests.cli.event_harness import iter_events

from .conftest import PROXY_BINARY, SKIP_NO_PROXY_BINARY, free_port, proxy_binary_available, wait_for_port

pytestmark = pytest.mark.skipif(not proxy_binary_available(), reason=SKIP_NO_PROXY_BINARY)


@pytest.fixture
def running_proxy():
    """Start the proxy on a free port, wait until it listens, yield the port."""
    port = free_port()
    proc = subprocess.Popen(
        [PROXY_BINARY],
        env={**os.environ, "ATLAS_PROXY_PORT": str(port), "ATLAS_KEEP_LLAMA_WARM": "0"},
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        wait_for_port(port)
        yield port
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            proc.kill()


def test_iter_events_skips_the_connected_sentinel_of_a_live_proxy(running_proxy):
    """A quiet proxy sends only the `: connected` comment (heartbeats come
    every 15 s). iter_events must not turn it into an event or fail on it:
    the first read just waits until the 0.5 s read timeout."""
    events = iter_events(f"http://127.0.0.1:{running_proxy}/events", timeout=0.5)
    with pytest.raises(socket.timeout):
        next(events)


def test_live_proxy_events_endpoint_returns_correct_content_type(running_proxy):
    """Headers arrive immediately and Content-Type is text/event-stream.
    Regression for the buffer-flush bug fixed in b7463e3.

    Uses readline() rather than read(N) — the SSE stream is line-based,
    and read(64) would block waiting for more bytes that won't arrive
    until either an envelope event or the 15s heartbeat fires."""
    url = f"http://127.0.0.1:{running_proxy}/events"
    with urllib.request.urlopen(url, timeout=2.0) as resp:
        assert resp.status == 200
        ct = resp.headers.get("Content-Type", "")
        assert ct == "text/event-stream", f"got {ct!r}"
        # The first SSE frame is `: connected\n\n` — readline reads up
        # to and including the first \n.
        first_line = resp.readline()
        assert b"connected" in first_line, \
            f"no connected sentinel in first line {first_line!r}"
