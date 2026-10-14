"""Unit tests for the service-token module and init generation step.

All tokens are synthetic fixtures.
"""

import http.server
import os
import stat
import subprocess
import sys
import threading
import urllib.request
from pathlib import Path

from atlas import token as token_mod


def test_read_token_missing_file(tmp_path, monkeypatch):
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE",
                       str(tmp_path / "absent"))
    assert token_mod.read_token() == ""
    assert token_mod.auth_headers() == {}


def test_read_token_and_headers(tmp_path, monkeypatch):
    tok = tmp_path / "service-token"
    tok.write_text("atlas-st-unitfixture\n")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tok))
    assert token_mod.read_token() == "atlas-st-unitfixture"
    assert token_mod.auth_headers() == {
        "Authorization": "Bearer atlas-st-unitfixture"}


def test_permission_check_flags_loose_file(tmp_path, monkeypatch):
    secrets = tmp_path / "secrets"
    secrets.mkdir(mode=0o700)
    tok = secrets / "service-token"
    tok.write_text("atlas-st-unitfixture\n")
    tok.chmod(0o644)  # loose
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tok))
    ok, detail = token_mod.check_file_permissions()
    assert not ok
    assert "0o644" in detail
    assert "atlas-st-unitfixture" not in detail  # never leak the value


def test_permission_check_passes_strict(tmp_path, monkeypatch):
    secrets = tmp_path / "secrets"
    secrets.mkdir(mode=0o700)
    tok = secrets / "service-token"
    tok.write_text("atlas-st-unitfixture\n")
    tok.chmod(0o600)
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tok))
    ok, detail = token_mod.check_file_permissions()
    assert ok, detail


def test_init_step_generates_and_keeps_token(tmp_path):
    from atlas.commands import init as init_mod

    class Args:
        dry_run = False
        rotate_token = False

    path = init_mod._step_write_service_token(str(tmp_path), Args(), False)
    assert os.path.isfile(path)
    mode = stat.S_IMODE(os.stat(path).st_mode)
    assert mode == 0o600, oct(mode)
    first = Path(path).read_text().strip()
    assert first.startswith("atlas-st-")

    # Re-run keeps the existing token (live stack keeps working)
    init_mod._step_write_service_token(str(tmp_path), Args(), False)
    kept = Path(path).read_text().strip()
    assert kept == first

    # --rotate-token regenerates
    class Rotate(Args):
        rotate_token = True

    init_mod._step_write_service_token(str(tmp_path), Rotate(), False)
    second = Path(path).read_text().strip()
    assert second != first
    assert second.startswith("atlas-st-")
    assert stat.S_IMODE(os.stat(path).st_mode) == 0o600


def stand_in(name, seen, moved_to=""):
    """A local server that writes down the Authorization of each request in `seen`. A path that starts with /moved
    is sent on to `moved_to` + /after + the path."""
    class Handler(http.server.BaseHTTPRequestHandler):
        def do_GET(self):
            seen[name + self.path] = self.headers.get("Authorization", "")
            self.send_response(302 if self.path.startswith("/moved") else 200)
            if self.path.startswith("/moved"):
                self.send_header("Location", f"{moved_to}/after{self.path}")
            self.send_header("Content-Length", "0")
            self.end_headers()

        def log_message(self, *args):
            pass

    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server, f"http://127.0.0.1:{server.server_port}"


def test_the_token_goes_only_to_an_address_of_the_stack_and_not_on_to_another(tmp_path, monkeypatch):
    seen = {}
    other, other_address = stand_in("other", seen)
    proxy, proxy_address = stand_in("proxy", seen, moved_to=other_address)
    # The proxy is the stand-in "proxy". The other one is on the same host, on another port.
    monkeypatch.setenv("ATLAS_PROXY_URL", proxy_address)
    tok = tmp_path / "service-token"
    tok.write_text("atlas-st-unitfixture\n")
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tok))
    monkeypatch.setattr(urllib.request, "_opener", None)  # the global opener of the test goes away after it
    token_mod.install_urllib_opener()

    for address in (f"{proxy_address}/asked", f"{other_address}/asked", f"{proxy_address}/moved"):
        urllib.request.urlopen(address, timeout=10).close()
    # A call to the proxy that sets the header itself, as the call sites with auth_headers() do.
    own_header = urllib.request.Request(f"{proxy_address}/moved-with-its-own-header", headers=token_mod.auth_headers())
    urllib.request.urlopen(own_header, timeout=10).close()
    # An address set after the opener is installed counts: `atlas lens` sets LLAMA_EMBED_URL while it runs.
    monkeypatch.setenv("LLAMA_EMBED_URL", other_address)
    urllib.request.urlopen(f"{other_address}/once-it-is-an-address-of-the-stack", timeout=10).close()
    proxy.shutdown()
    other.shutdown()

    assert seen == {
        "proxy/asked": "Bearer atlas-st-unitfixture",
        "other/asked": "",
        "proxy/moved": "Bearer atlas-st-unitfixture",
        "other/after/moved": "",
        "proxy/moved-with-its-own-header": "Bearer atlas-st-unitfixture",
        "other/after/moved-with-its-own-header": "",
        "other/once-it-is-an-address-of-the-stack": "Bearer atlas-st-unitfixture",
    }


def test_the_bench_started_as_a_module_installs_the_cli_opener(tmp_path):
    """`python -m atlas.bench.v3_runner` does not start through `atlas`, so it installs the CLI's opener itself."""
    tok = tmp_path / "service-token"
    tok.write_text("atlas-st-unitfixture\n")
    start_the_bench_with_help = (
        "import runpy, sys, urllib.request\n"
        "sys.argv = ['v3_runner', '--help']\n"
        "try:\n"
        "    runpy.run_module('atlas.bench.v3_runner', run_name='__main__')\n"
        "except SystemExit:\n"
        "    pass\n"
        "print('handlers:', [type(handler).__name__ for handler in urllib.request._opener.handlers])\n")
    done = subprocess.run([sys.executable, "-c", start_the_bench_with_help], cwd=Path(__file__).resolve().parents[2],
                          env={**os.environ, "ATLAS_SERVICE_TOKEN_FILE": str(tok)}, capture_output=True, text=True, timeout=120)
    assert "_TokenForTheStackOnly" in done.stdout, done.stdout[-2000:] + done.stderr[-2000:]
