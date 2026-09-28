"""atlas tui — Bubbletea TUI client (PC-062).

Replaces the Aider-based chat UI with a native Bubbletea TUI built into
the tui/ directory. Three panes (pipeline, events, chat) feed off
two SSE streams from atlas-proxy: /events (typed envelope visibility)
and /v1/agent (per-turn chat protocol). See docs/CLI.md for the full
keymap and slash-command reference.

Launch strategy:
  1. Locate the `atlas-tui` binary on PATH or in ~/.local/bin
  2. If missing or older than checkout source, build from tui/
  3. If still missing → print install instructions and exit
  4. Otherwise → ensure atlas-proxy is running, then exec the TUI

Pass-through args after `atlas tui` go straight to the binary, so e.g.
`atlas tui --proxy http://other:8090` works as expected.
"""

import os
import shutil
import subprocess
import sys
from typing import List, Optional

from atlas.runtime_artifacts import go_binary_is_current


def _find_atlas_dir() -> str:
    """Walk up from this file looking for the ATLAS repo root."""
    d = os.path.dirname(os.path.abspath(__file__))
    for _ in range(5):
        if os.path.exists(os.path.join(d, "tui", "go.mod")):
            return d
        d = os.path.dirname(d)
    if os.path.exists(os.path.join(os.getcwd(), "tui", "go.mod")):
        return os.getcwd()
    return ""


def _find_tui_binary(atlas_dir: str) -> Optional[str]:
    """Locate the atlas-tui binary. Returns absolute path or None."""
    on_path = shutil.which("atlas-tui")
    if on_path:
        return on_path
    for cand in (
        os.path.expanduser("~/.local/bin/atlas-tui"),
        os.path.join(atlas_dir, "tui", "atlas-tui") if atlas_dir else None,
    ):
        if cand and os.path.isfile(cand) and os.access(cand, os.X_OK):
            return cand
    return None


def _build_tui(atlas_dir: str) -> Optional[str]:
    """Build atlas-tui from source. Returns the built path or None."""
    go_bin = shutil.which("go")
    if not go_bin or not atlas_dir:
        return None
    src = os.path.join(atlas_dir, "tui")
    if not os.path.isfile(os.path.join(src, "go.mod")):
        return None
    output = os.path.expanduser("~/.local/bin/atlas-tui")
    os.makedirs(os.path.dirname(output), exist_ok=True)
    print("  Building atlas-tui from source...")
    try:
        result = subprocess.run(
            [go_bin, "build", "-o", output, "."],
            cwd=src, capture_output=True, text=True, timeout=180,
        )
        if result.returncode == 0:
            print(f"  Built: {output}")
            return output
        print(f"  Build failed: {result.stderr[:240]}")
    except Exception as e:
        print(f"  Build failed: {e}")
    return None


def _select_tui_binary(atlas_dir: str) -> Optional[str]:
    """Select a TUI binary whose behavior matches the current checkout."""
    binary = _find_tui_binary(atlas_dir)
    source_dir = os.path.join(atlas_dir, "tui") if atlas_dir else ""
    if binary and go_binary_is_current(
        binary,
        source_dir,
        extra_names=("demo_prompts_fallback.json",),
    ):
        return binary
    if binary:
        print("  atlas-tui source changed; rebuilding the installed binary...")
    return _build_tui(atlas_dir)


def main(argv: List[str]) -> int:
    """Entry point for `atlas tui [...args]`."""
    atlas_dir = _find_atlas_dir()

    binary = _select_tui_binary(atlas_dir)
    if not binary:
        sys.stderr.write(
            "atlas tui: atlas-tui binary not found and Go is not "
            "available to build it.\n"
            "Install Go 1.26.6+ (https://go.dev/dl/) or build manually:\n"
            "  cd tui && go build -o ~/.local/bin/atlas-tui .\n"
        )
        return 1

    # Ensure proxy is up AND its /workspace bind covers the user's cwd.
    # _ensure_proxy() handles both: health check + auto-realign via
    # force-recreate when cwd is outside the bind. The recreate is ~5s
    # — fast enough to do unconditionally, and necessary for tool calls
    # to work (the proxy can only read/write paths under its mount).
    from atlas.runtime import (
        DEMO_RAW_CAPABILITY,
        PROXY_URL,
        ensure_proxy,
    )
    if not ensure_proxy(required_capability=DEMO_RAW_CAPABILITY):
        sys.stderr.write(
            "atlas tui: atlas-proxy is unavailable or too old for the "
            "current demo contract and could not be rebuilt.\n"
            "From the ATLAS checkout, run: docker compose up -d --build "
            "atlas-proxy\n"
        )
        return 1

    # Default --proxy from env/runtime.py if the user didn't override it.
    args = list(argv)
    if "--proxy" not in args:
        args = ["--proxy", PROXY_URL] + args

    # Hand the TUI the checkout's service token path (internal auth) —
    # the TUI runs from arbitrary project dirs, so a cwd-relative
    # lookup wouldn't find it. Only the PATH crosses the env boundary,
    # never the token value.
    if not os.environ.get("ATLAS_SERVICE_TOKEN_FILE"):
        from atlas import token as _token
        tok_path = _token.token_path()
        if os.path.isfile(tok_path):
            os.environ["ATLAS_SERVICE_TOKEN_FILE"] = tok_path

    # Default --log to a stable path under ~/.cache so debugging the
    # TUI doesn't require the user to remember a flag. Alt-screen mode
    # makes it impractical to copy text out of the live view; the log
    # is the operator's read-only record of what the TUI received.
    # Override with --log <path> or ATLAS_TUI_LOG; "off" disables.
    if "--log" not in args and not os.environ.get("ATLAS_TUI_LOG"):
        log_dir = os.path.expanduser("~/.cache/atlas-tui")
        os.makedirs(log_dir, exist_ok=True)
        args = ["--log", os.path.join(log_dir, "debug.log")] + args
        print(f"  TUI debug log: {os.path.join(log_dir, 'debug.log')}")
    elif os.environ.get("ATLAS_TUI_LOG", "").lower() == "off":
        # Explicit opt-out — strip any default we'd set.
        os.environ.pop("ATLAS_TUI_LOG", None)

    # exec, not run — the TUI takes over the terminal and we want
    # signals (Ctrl+C, window resize) routed to it directly. CAVEAT:
    # execv replaces the Python process image, so atexit handlers
    # registered by the wrapper (notably stop_local_proxy in runtime.py)
    # never fire. Any local proxy launched by ensure_proxy() gets
    # orphaned and keeps running until something else (reboot, manual
    # kill, or the runtime's own stale-proxy reaper on the next run)
    # cleans it up. That orphan owns :8090 and collides with subsequent
    # `docker compose up` on the macOS hybrid path (#118). Stop it
    # explicitly here before exec so the cleanup actually runs.
    from atlas.runtime import stop_local_proxy
    stop_local_proxy()
    try:
        os.execv(binary, [binary, *args])
    except OSError as e:
        sys.stderr.write(f"atlas tui: exec failed: {e}\n")
        return 1
    return 0  # unreachable
