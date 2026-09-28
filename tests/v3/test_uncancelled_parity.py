"""The transport swap must not change uncancelled generation semantics.

c98184c and 7124bb5 replaced urlopen with http.client so cancellation could
reach a blocked read. That is a change to the one path every V3 generation
uses, and "the suite still passes" is not the same claim as "the request on
the wire is the same request". These compare both transports against the same
recording server, field by field.
"""
import json
import os
import sys
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
sys.path.insert(0, V3)
import adapters  # noqa: E402


class Recorder(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    seen = []
    lock = threading.Lock()
    mode = "ok"

    def log_message(self, *a):
        pass

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        with Recorder.lock:
            Recorder.seen.append({
                "method": self.command, "path": self.path,
                "headers": {k.lower(): v for k, v in self.headers.items()},
                "body": body, "len": n})
        if Recorder.mode == "500":
            self.send_response(500)
            self.send_header("Content-Length", "11")
            self.end_headers()
            self.wfile.write(b"upstream ko")
            return
        if Recorder.mode == "empty":
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        payload = {"choices": [{"text": "```python\nx = 1\n```"}],
                   "usage": {"total_tokens": 7, "completion_tokens": 7}}
        if Recorder.mode == "large":
            payload["choices"][0]["text"] = "```python\n" + ("y = 1\n" * 20000) + "```"
        if Recorder.mode == "malformed":
            data = b"data: {not json}\n\ndata: [DONE]\n\n"
        else:
            data = (f"data: {json.dumps(payload)}\n\n" "data: [DONE]\n\n").encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)


@pytest.fixture()
def rec():
    Recorder.seen = []
    Recorder.mode = "ok"
    srv = ThreadingHTTPServer(("127.0.0.1", 0), Recorder)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    old = adapters.INFERENCE_URL
    adapters.INFERENCE_URL = f"http://127.0.0.1:{srv.server_address[1]}"
    try:
        yield srv
    finally:
        adapters.INFERENCE_URL = old
        srv.shutdown()
        srv.server_close()


def via_urlopen(payload):
    """The pre-c98184c transport, reconstructed for comparison only."""
    req = urllib.request.Request(
        f"{adapters.INFERENCE_URL}/v1/chat/completions",
        data=payload, headers=adapters._service_headers())
    with urllib.request.urlopen(req, timeout=600) as resp:
        return resp.status, resp.read()


def via_httpclient(payload):
    a = adapters.LLMAdapter(progress_callback=None)
    with a._open_inference(payload) as resp:
        return resp.status, resp.read()


PAYLOAD = json.dumps({"model": "default", "messages": [{"role": "user", "content": "hi"}],
                      "stream": False}).encode()


def test_request_on_the_wire_is_identical(rec):
    via_urlopen(PAYLOAD)
    via_httpclient(PAYLOAD)
    with Recorder.lock:
        old, new = Recorder.seen[0], Recorder.seen[1]
    assert old["method"] == new["method"] == "POST"
    assert old["path"] == new["path"] == "/v1/chat/completions"
    assert old["body"] == new["body"] == PAYLOAD
    assert old["len"] == new["len"] == len(PAYLOAD)
    # Framing: both must declare Content-Length, never chunk the request.
    assert old["headers"].get("content-length") == new["headers"].get("content-length")
    assert "transfer-encoding" not in new["headers"], "the request was chunked"
    for h in adapters._service_headers():
        assert new["headers"].get(h.lower()) == old["headers"].get(h.lower()), h


def test_successful_response_parses_identically(rec):
    so, bo = via_urlopen(PAYLOAD)
    sn, bn = via_httpclient(PAYLOAD)
    assert so == sn == 200
    assert bo == bn


def test_large_response_is_identical(rec):
    Recorder.mode = "large"
    so, bo = via_urlopen(PAYLOAD)
    sn, bn = via_httpclient(PAYLOAD)
    assert so == sn and bo == bn and len(bn) > 100_000


def test_empty_response_is_identical(rec):
    Recorder.mode = "empty"
    so, bo = via_urlopen(PAYLOAD)
    sn, bn = via_httpclient(PAYLOAD)
    assert so == sn == 200 and bo == bn == b""


def test_non_200_raises_the_same_class(rec):
    Recorder.mode = "500"
    with pytest.raises(urllib.error.HTTPError) as old:
        via_urlopen(PAYLOAD)
    with pytest.raises(urllib.error.HTTPError) as new:
        via_httpclient(PAYLOAD)
    assert old.value.code == new.value.code == 500


def test_connection_refusal_is_an_oserror_either_way():
    old = adapters.INFERENCE_URL
    adapters.INFERENCE_URL = "http://127.0.0.1:9"
    try:
        with pytest.raises(OSError):
            via_urlopen(PAYLOAD)
        with pytest.raises(OSError):
            via_httpclient(PAYLOAD)
    finally:
        adapters.INFERENCE_URL = old


def test_malformed_stream_body_reaches_the_parser_unchanged(rec):
    Recorder.mode = "malformed"
    so, bo = via_urlopen(PAYLOAD)
    sn, bn = via_httpclient(PAYLOAD)
    assert bo == bn, "the parser would see different bytes"


def test_call_count_and_cleanup(rec):
    for _ in range(5):
        via_httpclient(PAYLOAD)
    with Recorder.lock:
        assert len(Recorder.seen) == 5
    assert threading.active_count() < 20


def test_timeout_value_is_preserved():
    import inspect
    src = inspect.getsource(adapters.LLMAdapter._open_inference)
    assert "timeout=600" in src, "the transport swap changed the timeout"
