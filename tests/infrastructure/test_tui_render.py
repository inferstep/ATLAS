"""The TUI actually runs, paints, and consumes the proxy's event stream.

`tests/contracts/api/test_event_contract.py` proves the proxy and the TUI agree on
event NAMES by reading both sources. It cannot prove the binary starts, paints a
frame, or survives the events it is sent — and a Bubble Tea program refuses to
run at all without a terminal, so a subprocess pipe would only ever prove that
it exits.

This allocates a pty, runs the real binary against the running proxy, and reads
the painted frames back. Live-stack test: deselected by the default
`-m 'not integration'`, run it with `-m integration` on a host with the stack up
and `tui/atlas-tui` built.
"""
import errno
import fcntl
import os
import pty
import re
import select
import struct
import subprocess
import termios
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
BINARY = REPO / "tui" / "atlas-tui"
PROXY = os.environ.get("ATLAS_PROXY_URL", "http://127.0.0.1:8090")

# CSI/OSC/charset-select/keypad-mode. Frames are mostly escapes, and asserting
# on styled text without stripping them matches nothing.
ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[()][AB0]|\x1b[=>]")


def _strip(s: str) -> str:
    return ANSI.sub("", s)


@pytest.fixture(scope="module")
def painted() -> str:
    """Run the TUI under a pty for a few seconds and return the stripped output."""
    if not BINARY.exists():
        pytest.skip(f"{BINARY} not built (cd tui && go build -o atlas-tui .)")

    master, slave = pty.openpty()
    # The files pane only renders at >=90 columns, so a default 80x24 would
    # silently test a narrower layout than anyone actually uses.
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 45, 160, 0, 0))
    env = dict(os.environ, TERM="xterm-256color", ATLAS_PROXY_URL=PROXY,
               ATLAS_TUI_MOUSE="off", ATLAS_TUI_LOG="off")
    proc = subprocess.Popen([str(BINARY), "-proxy", PROXY, "-mouse", "off"],
                            stdin=slave, stdout=slave, stderr=slave,
                            env=env, close_fds=True)
    os.close(slave)

    chunks = []
    deadline = time.time() + 8.0
    while time.time() < deadline:
        r, _, _ = select.select([master], [], [], 0.4)
        if master in r:
            try:
                data = os.read(master, 65536)
            except OSError:
                break
            if not data:
                break
            chunks.append(data.decode("utf-8", "replace"))
        if proc.poll() is not None:
            break

    # Ask it to quit; a TUI that already exited has closed the pty, which
    # makes the write fail. That is the expected race, not a fault, so it is
    # narrowed to the already-gone errnos and the terminate() below is what
    # actually guarantees the process is reaped.
    if proc.poll() is None:
        try:
            os.write(master, b"/quit\r")
            time.sleep(1.0)
            if proc.poll() is None:
                os.write(master, b"\x03")
                time.sleep(0.5)
        except OSError as e:
            if e.errno not in (errno.EIO, errno.EBADF, errno.EPIPE):
                raise
    if proc.poll() is None:
        proc.terminate()
        try:
            proc.wait(timeout=5)
        except subprocess.TimeoutExpired:
            proc.kill()
    os.close(master)
    return _strip("".join(chunks))


def test_tui_paints_a_frame(painted):
    assert len(painted) > 200, (
        f"TUI painted only {len(painted)} bytes — it likely exited immediately")


def test_tui_does_not_panic(painted):
    assert "panic:" not in painted, "Go panic in the TUI"
    assert "goroutine " not in painted, "Go stack trace in the TUI output"


def test_tui_reaches_the_proxy(painted):
    """A TUI that cannot reach the proxy still paints, so check for the banner."""
    bad = re.search(r"connection refused|dial tcp|cannot reach|no such host",
                    painted, re.I)
    assert not bad, f"TUI could not reach the proxy at {PROXY}: {bad.group(0)!r}"


def test_tui_renders_its_input_affordances(painted):
    """The prompt line is the one element every frame carries."""
    assert re.search(r"Type a message|for command|for help", painted), (
        "no input affordance painted — the layout did not render")
