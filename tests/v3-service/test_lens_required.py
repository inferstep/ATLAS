"""The lens is required (docs/adr/0011-the-lens-is-required.md).

V3 used to answer a lens that could not score with neutral scores and go
on ranking. It now stops, and /v3/generate tells its caller why in the
result the caller is waiting for; the proxy stops the run on it.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import main  # noqa: E402

def test_a_lens_that_cannot_score_is_answered_as_such(monkeypatch):
    """/v3/generate answers a lens that cannot score with a typed result the
    proxy stops the run on: no code, not even the baseline, and the reason.
    Before, the pipeline answered neutral scores and the run went on."""
    import json
    import threading
    import urllib.request
    from http.server import ThreadingHTTPServer

    import adapters
    import scoring

    def lens_down(**kwargs):
        raise scoring.LensUnavailable("lens_unreachable: URLError")

    monkeypatch.setattr(adapters, "SERVICE_TOKEN", "")
    monkeypatch.setattr(main.pipeline, "run", lens_down)
    srv = ThreadingHTTPServer(("127.0.0.1", 0), main.V3Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    try:
        req = urllib.request.Request(
            f"http://127.0.0.1:{srv.server_address[1]}/v3/generate",
            data=json.dumps({"file_path": "solve.py",
                             "baseline_code": "print(1)\n"}).encode(),
            headers={"Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=10) as resp:
            body = resp.read().decode()
    finally:
        srv.shutdown()
        srv.server_close()
    frame = body[body.index("event: result"):]
    result = json.loads(frame.split("data: ", 1)[1].split("\n", 1)[0])
    assert result["lens_unavailable"] == "lens_unreachable: URLError"
    assert result["code"] == "" and result["passed"] is False
    assert "data: [DONE]" in body
