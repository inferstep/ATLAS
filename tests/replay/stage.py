"""The stand-ins for the four services the proxy calls, and the proxy itself as a built binary.

A Stage runs one HTTP server per service. In record mode each request is
answered by a function and the exchange is kept. In play mode each request is
compared with the next recorded request and answered with the recorded
answer; the first request that differs is kept as the mismatch, and from then
on the stage answers with an error, because the rest of the recording no
longer fits.

The proxy makes its calls one after another, so the next recorded request is
the next one of the whole recording, whichever service it goes to: a call to
the sandbox where the recording has a call to V3 is a mismatch. A recording
of a session in which the proxy calls two services at the same time says
`"order": "per service"`, and then the next request of each service counts.
"""
from __future__ import annotations

import http.client
import http.server
import json
import os
import socket
import subprocess
import threading
import time
from pathlib import Path

REPO = Path(__file__).resolve().parents[2]
SERVICES = ("model", "sandbox", "v3", "lens")
SERVICE_ENV = {"model": ("ATLAS_INFERENCE_URL", "ATLAS_LLAMA_URL"), "sandbox": ("ATLAS_SANDBOX_URL",),
               "v3": ("ATLAS_V3_URL",), "lens": ("ATLAS_LENS_URL",)}
# Requests the proxy sends on a clock of its own, so their number and place vary
# from run to run. They are answered the same way each time and never recorded.
CLOCKED = {("GET", "/health"), ("GET", "/ready"), ("GET", "/slots"), ("POST", "/slots")}
WORKSPACE = "<workspace>"
# The proxy writes a fresh value into this file at the start of a session and
# asks the sandbox to read it back, to see that both work in the same folder.
PROBE_FILE, PROBE = ".atlas-mount-probe", "<mount-probe>"


def free_port() -> int:
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", 0))
        return probe.getsockname()[1]


def clocked_answer(service: str, method: str, path: str):
    """The fixed answer to a request the proxy sends on its own clock, or None when it is not one."""
    base = path.split("?")[0]
    if (method, base) not in CLOCKED and not base.startswith("/slots"):
        return None
    if base.startswith("/slots"):
        return 404, "text/plain", ""
    if service == "lens":
        if base == "/ready":
            return 200, "application/json", json.dumps({"ready": True, "llama_server": True, "lens_self_test": True})
        return 200, "application/json", json.dumps({
            "service": "geometric-lens", "status": "healthy", "subsystems": {
                "llama_server": {"reachable": True},
                "lens": {"cost_field_loaded": True, "gx_loaded": True, "cx_calibrated": True, "gx_calibrated": True,
                         "self_test_pass": True, "fingerprint_ok": None}}})
    return 200, "application/json", json.dumps({"status": "ok"})


class Stage:
    def __init__(self, upstreams=None, recording=None, workspace="", accept=False):
        self.upstreams, self.recording, self.workspace = upstreams, recording, str(workspace)
        # With accept, a request that differs only in its text takes the place
        # of the recorded one (to write the expected side of a recording again).
        self.accept, self.accepted = accept, 0
        self.lock = threading.Lock()
        self.exchanges, self.mismatch, self.servers, self.ports = [], None, [], {}
        self.waiting = list((recording or {}).get("exchanges", []))
        self.per_service = (recording or {}).get("order") == "per service"

    def start(self) -> dict[str, int]:
        for service in SERVICES:
            port = free_port()
            server = http.server.ThreadingHTTPServer(("127.0.0.1", port), self._handler(service))
            threading.Thread(target=server.serve_forever, daemon=True).start()
            self.servers.append(server)
            self.ports[service] = port
        return self.ports

    def stop(self) -> None:
        for server in self.servers:
            server.shutdown()
            server.server_close()

    def probe_value(self) -> str:
        try:
            return (Path(self.workspace) / PROBE_FILE).read_text(encoding="utf-8")
        except OSError:
            return ""

    def plain(self, text: str) -> str:
        """Text with what differs between two runs or two machines replaced by fixed words."""
        if self.probe_value():
            text = text.replace(self.probe_value(), PROBE)
        if self.workspace:
            text = text.replace(self.workspace, WORKSPACE)
        for service, port in self.ports.items():
            text = text.replace(f"127.0.0.1:{port}", f"<{service}>")
        return text

    def answer(self, service: str, method: str, path: str, body: str):
        clocked = clocked_answer(service, method, path)
        if clocked:
            return clocked
        request = {"service": service, "method": method, "path": path, "request": self.plain(body)}
        with self.lock:
            if self.upstreams is not None:
                status, content_type, text = self.upstreams[service](method, path, body)
                self.exchanges.append({**request, "response": {"status": status, "content_type": content_type,
                                                               "body": self.plain(text)}})
                return status, content_type, text
            if self.mismatch:
                return 599, "text/plain", "the replay stopped at an earlier request that differs from the recording"
            expected = self.next_recorded(service)
            if expected is None:
                self.mismatch = {"service": service, "expected": None, "got": request}
                return 599, "text/plain", "the recording has no more requests to this service"
            same_call = all(expected[k] == request[k] for k in ("service", "method", "path"))
            if same_call and self.accept and expected["request"] != request["request"]:
                expected["request"], self.accepted = request["request"], self.accepted + 1
            if {k: expected[k] for k in ("service", "method", "path", "request")} != request:
                self.mismatch = {"service": service, "expected": expected, "got": request}
                return 599, "text/plain", "this request differs from the recorded one"
            self.waiting.remove(expected)
            self.exchanges.append(expected)
            response = expected["response"]
            body = response["body"].replace(WORKSPACE, self.workspace).replace(PROBE, self.probe_value())
            return response["status"], response["content_type"], body

    def next_recorded(self, service: str):
        """The recorded request this one is held against, or None when the recording has none left for it."""
        if self.per_service:
            return next((exchange for exchange in self.waiting if exchange["service"] == service), None)
        return self.waiting[0] if self.waiting else None

    def not_asked(self) -> dict[str, int]:
        """How many recorded requests to each service the proxy did not send."""
        return {service: count for service in SERVICES
                if (count := sum(1 for exchange in self.waiting if exchange["service"] == service))}

    def _handler(self, service: str):
        stage = self

        class Handler(http.server.BaseHTTPRequestHandler):
            def log_message(self, *args):
                pass

            def _serve(self):
                body = self.rfile.read(int(self.headers.get("Content-Length", "0") or 0)).decode("utf-8", "replace")
                status, content_type, text = stage.answer(service, self.command, self.path, body)
                data = text.encode("utf-8")
                self.send_response(status)
                self.send_header("Content-Type", content_type)
                self.send_header("Content-Length", str(len(data)))
                self.end_headers()
                self.wfile.write(data)

            do_GET = do_POST = _serve

        return Handler


def build_proxy(target: Path) -> Path:
    """The proxy of this checkout as a binary: the one ATLAS_PROXY_BINARY names, or a fresh build."""
    named = os.environ.get("ATLAS_PROXY_BINARY", "")
    if named and os.access(named, os.X_OK):
        return Path(named)
    binary = target / "atlas-proxy"
    subprocess.run(["go", "build", "-o", str(binary), "."], cwd=REPO / "proxy", check=True)
    return binary


def start_proxy(binary: Path, ports: dict[str, int], home: Path, more_env=None):
    """Start the proxy against the stand-ins with a fixed, small environment. Returns its port and process."""
    port = free_port()
    token = home / "service-token"
    token.write_text("replay-placeholder-token\n", encoding="utf-8")
    token.chmod(0o600)
    env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "HOME": str(home), "ATLAS_PROXY_PORT": str(port),
           "ATLAS_KEEP_LLAMA_WARM": "0", "ATLAS_PERMISSION_TIMEOUT_SEC": "30", "ATLAS_SERVICE_TOKEN_FILE": str(token),
           "ATLAS_MODEL_NAME": "replay-model", **(more_env or {})}
    for service, names in SERVICE_ENV.items():
        for name in names:
            env[name] = f"http://127.0.0.1:{ports[service]}"
    process = subprocess.Popen([str(binary)], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
    deadline = time.time() + 20
    while time.time() < deadline:
        try:
            socket.create_connection(("127.0.0.1", port), timeout=0.5).close()
            return port, process
        except OSError:
            time.sleep(0.05)
    process.terminate()
    raise RuntimeError("the proxy did not start: " + process.communicate(timeout=5)[1].decode("utf-8", "replace")[-1500:])


def drive(port: int, body: dict, cap: float = 120.0) -> list[dict]:
    """Send one request to POST /v1/agent, answer each permission prompt with yes, and return the events.

    Like every sender this repository owns, it declares a task mode: the one
    the recording holds, and work when a recording holds none.
    """
    body = {**body, "task_contract": body.get("task_contract") or {"task_mode": "work"}}
    headers = {"Content-Type": "application/json", "Accept": "text/event-stream",
               "Authorization": "Bearer replay-placeholder-token"}
    connection = http.client.HTTPConnection("127.0.0.1", port, timeout=cap)
    connection.request("POST", "/v1/agent", json.dumps(body), headers)
    response = connection.getresponse()
    if response.status != 200:
        raise RuntimeError(f"POST /v1/agent answered {response.status}: {response.read()[:300]!r}")
    events = []
    for raw in response:
        line = raw.decode("utf-8", "replace").strip()
        if not line.startswith("data: "):
            continue
        if line[6:] == "[DONE]":
            break
        event = json.loads(line[6:])
        events.append(event)
        if event.get("type") == "permission_request":
            answer = http.client.HTTPConnection("127.0.0.1", port, timeout=10)
            answer.request("POST", "/v1/permission", json.dumps({
                "session_id": body["session_id"], "tool_call_id": event["data"]["tool_call_id"], "decision": "allow",
                "scope": "once"}), headers)
            answer.getresponse().read()
            answer.close()
    connection.close()
    return events
