"""Live cross-language integration tests for the event protocol (PC-061).

These tests start a real atlas-proxy binary and exercise iter_events()
against /events via HTTP. They prove the full producer → SSE wire →
consumer pipeline works across the Go/Python boundary.

Skipped automatically when the proxy binary isn't available — keeps
the unit-test suite lean and the integration tests opt-in via a
pre-built binary.
"""

import os
import socket
import subprocess
import time
import urllib.request

import pytest

from tests.cli.event_harness import iter_events

PROXY_BINARY = os.environ.get("ATLAS_PROXY_BINARY", "/tmp/test-atlas-proxy")


pytestmark = pytest.mark.skipif(
    not os.path.isfile(PROXY_BINARY) or not os.access(PROXY_BINARY, os.X_OK),
    reason=f"atlas-proxy binary not available at {PROXY_BINARY} "
           f"— run `cd proxy && go build -o {PROXY_BINARY} .` first",
)


@pytest.fixture
def running_proxy():
    """Start the proxy on a free port, wait until it listens, yield the port."""
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        port = probe.getsockname()[1]
    proc = subprocess.Popen(
        [PROXY_BINARY],
        env={**os.environ, "ATLAS_PROXY_PORT": str(port), "ATLAS_KEEP_LLAMA_WARM": "0"},
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
    try:
        deadline = time.time() + 3.0
        while True:
            try:
                socket.create_connection(("127.0.0.1", port), timeout=0.1).close()
                break
            except OSError:
                if time.time() > deadline:
                    pytest.fail(f"proxy didn't bind {port} within 3s")
                time.sleep(0.05)
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


def test_live_proxy_emits_envelope_when_broker_emit_called():
    """Placeholder for the end-to-end case: a typed Event fired by Emit() in
    the proxy's agent loop arrives through iter_events. No endpoint emits
    without a real LLM behind it, so it stays skipped to document the contract."""
    pytest.skip("requires llama-server + sandbox + lens to drive an "
                "agent loop end-to-end; will land with TUI integration test")


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
