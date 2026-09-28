#!/usr/bin/env python3
"""Run the candidate-staging integration test against a real executor.

The Go test in `proxy/staging_integration_test.go` needs a live
`sandbox/executor_server.py`: the whole point of it is to check that the real
executor's snapshot is isolated, its `finally` really deletes, and its
observation really describes the tree either side. A stub cannot answer any of
that.

This starts one on a free port against a throwaway workspace, runs the test
against it, and tears it down. Proxy and executor are pointed at the SAME
workspace directory, which is the alignment production requires -- so the
"staging never writes the production workspace" assertions are checked against
the directory the executor actually serves, not one it could never reach.

Exit code is the Go test's. `--keep` leaves the executor running and prints its
URL, for iterating by hand.
"""
from __future__ import annotations

import argparse
import contextlib
import os
import shutil
import socket
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.request
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
EXECUTOR = ROOT / "sandbox" / "executor_server.py"
PROXY = ROOT / "proxy"
TESTS = (
    "TestRealExecutorClassifiesEveryStagedOutcome|"
    "TestRealExecutorRunIsCancellable|"
    "TestRealExecutorRunsTheWholeDeclaredSetInOrder|"
    "TestRealExecutorKeepsConcurrentCandidatesIsolated|"
    "TestRealExecutorDestroysEveryStagingSnapshot|"
    "TestRealExecutorLeaksNoContent"
)

SERVE = """\
import importlib.util, os, sys, uvicorn
sys.path.insert(0, {sandbox!r})
spec = importlib.util.spec_from_file_location("executor_server_integration", {executor!r})
mod = importlib.util.module_from_spec(spec)
sys.modules[spec.name] = mod
spec.loader.exec_module(mod)
uvicorn.run(mod.app, host="127.0.0.1", port={port}, log_level="warning")
"""


def free_port() -> int:
    with contextlib.closing(socket.socket()) as s:
        s.bind(("127.0.0.1", 0))
        return int(s.getsockname()[1])


def wait_healthy(url: str, proc: subprocess.Popen, timeout: float = 30.0) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            return False
        try:
            with urllib.request.urlopen(url + "/health", timeout=2) as resp:
                if resp.status == 200:
                    return True
        except (urllib.error.URLError, OSError):
            time.sleep(0.25)
    return False


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument("--keep", action="store_true",
                    help="leave the executor running and print its URL")
    ap.add_argument("--timeout", default="10m", help="go test -timeout value")
    args = ap.parse_args()

    for tool, why in ((shutil.which("go"), "Go is not installed"),
                      (EXECUTOR.exists() or None, f"{EXECUTOR} is missing")):
        if not tool:
            print(f"staging-integration: {why}", file=sys.stderr)
            return 2
    try:
        import uvicorn  # noqa: F401
        import fastapi  # noqa: F401
    except ImportError as exc:
        print(f"staging-integration: {exc.name} is not installed", file=sys.stderr)
        return 2

    workdir = Path(tempfile.mkdtemp(prefix="atlas-staging-integration-"))
    ws, base = workdir / "workspace", workdir / "base"
    ws.mkdir()
    base.mkdir()
    port = free_port()
    url = f"http://127.0.0.1:{port}"

    serve = workdir / "serve.py"
    serve.write_text(SERVE.format(sandbox=str(ROOT / "sandbox"),
                                  executor=str(EXECUTOR), port=port))
    env = dict(os.environ,
               ATLAS_SANDBOX_WORKSPACE_ROOT=str(ws),
               WORKSPACE_BASE=str(base))
    proc = subprocess.Popen([sys.executable, str(serve)], env=env,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            text=True)
    try:
        if not wait_healthy(url, proc):
            out = ""
            if proc.stdout is not None:
                with contextlib.suppress(Exception):
                    out = proc.stdout.read()[-2000:]
            print(f"staging-integration: executor never became healthy\n{out}",
                  file=sys.stderr)
            return 2
        if args.keep:
            print(f"ATLAS_STAGING_SANDBOX_URL={url}")
            print(f"ATLAS_STAGING_SANDBOX_WORKSPACE={ws}")
            print(f"ATLAS_STAGING_SANDBOX_BASE={base}")
            proc.wait()
            return 0
        result = subprocess.run(
            ["go", "test", "-count=1", "-timeout", args.timeout, "-v",
             "-run", TESTS, "."],
            cwd=PROXY,
            env=dict(env, ATLAS_STAGING_SANDBOX_URL=url,
                     ATLAS_STAGING_SANDBOX_WORKSPACE=str(ws),
                     ATLAS_STAGING_SANDBOX_BASE=str(base)))
        return result.returncode
    finally:
        proc.terminate()
        with contextlib.suppress(subprocess.TimeoutExpired):
            proc.wait(timeout=10)
        if proc.poll() is None:
            proc.kill()
        if not args.keep:
            shutil.rmtree(workdir, ignore_errors=True)


if __name__ == "__main__":
    raise SystemExit(main())
