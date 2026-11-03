"""The TUI survives its own command surface.

test_tui_render.py proves the binary starts and paints. It says nothing about
the 21 slash commands and 7 key handlers, which is most of what a user
actually touches — and a command that wedges the UI or panics would not show
up anywhere else.

What this asserts, precisely: every command repaints and none crashes or
hangs the program. `/help` and `/context` additionally have to produce their
own content. The rest are liveness checks, not behaviour checks — a command
that silently does the wrong thing still passes here.

Live-stack test: carries the integration marker, deselected by default.
"""
import errno
import fcntl
import os
import pty
import re
import select
import struct
import subprocess
import tempfile
import termios
import time
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]
BINARY = REPO / "clients" / "tui" / "atlas-tui"
PROXY = os.environ.get("ATLAS_PROXY_URL", "http://127.0.0.1:8090")
ANSI = re.compile(r"\x1b\[[0-9;?]*[A-Za-z]|\x1b\][^\x07]*\x07|\x1b[()][AB0]|\x1b[=>]")

# (label, keystrokes, regex the frame must match afterwards)
STEPS = [
    ("/help", "/help\r", r"help|command|/diff|/undo"),
    ("/context", "/context\r", r"context|token|ctx|model"),
    ("/show", "/show\r", r"."),
    ("/hide", "/hide\r", r"."),
    ("ctrl+t", "\x14", r"."),
    ("pgup", "\x1b[5~", r"."),
    ("pgdown", "\x1b[6~", r"."),
    ("/clear", "/clear\r", r"."),
    # Second wave. Excluded deliberately: /commit runs git, /demo spawns a
    # session, /quit exits, and /undo /redo /accept /deny mutate state — none
    # of which belong in an automated pass.
    ("/diff", "/diff\r", r"."),
    ("/copy", "/copy\r", r"."),
    ("/compact", "/compact\r", r"."),
    ("/mouse", "/mouse\r", r"."),
    ("/drop", "/drop\r", r"."),
    ("/review", "/review\r", r"."),
    ("/add", "/add\r", r"."),          # bare: should explain it needs a path
    ("/run", "/run\r", r"."),          # bare: should explain it needs a command
    ("/nonsense", "/nonsense\r", r"."),  # unknown command must not wedge it
]


@pytest.fixture(scope="module")
def driven():
    """Run the TUI, send every step, return {label: frame} plus liveness."""
    if not BINARY.exists():
        pytest.skip(f"{BINARY} not built (cd clients/tui && go build -o atlas-tui .)")

    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack("HHHH", 45, 160, 0, 0))
    env = dict(os.environ, TERM="xterm-256color", ATLAS_PROXY_URL=PROXY,
               ATLAS_TUI_MOUSE="off", ATLAS_TUI_LOG="off")
    # Temp cwd: /diff and /add read the working directory, and an automated
    # pass must not be able to act on the repo it is testing.
    workdir = tempfile.mkdtemp(prefix="atlas-tui-test-")
    proc = subprocess.Popen([str(BINARY), "-proxy", PROXY, "-mouse", "off"],
                            stdin=slave, stdout=slave, stderr=slave,
                            env=env, close_fds=True, cwd=workdir)
    os.close(slave)

    def pump(seconds):
        chunks, end = [], time.time() + seconds
        while time.time() < end:
            r, _, _ = select.select([master], [], [], 0.3)
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
        return ANSI.sub("", "".join(chunks))

    pump(6)
    frames = {}
    for label, keys, _ in STEPS:
        if proc.poll() is not None:
            break
        os.write(master, keys.encode())
        frames[label] = pump(2.5)
    alive = proc.poll() is None

    if proc.poll() is None:
        try:
            os.write(master, b"\x03")
            time.sleep(1.0)
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
    return {"frames": frames, "alive": alive}


def test_every_command_repaints(driven):
    missing = [label for label, _, _ in STEPS
               if not driven["frames"].get(label)]
    assert not missing, f"no repaint after: {missing}"


def test_no_command_panics(driven):
    for label, frame in driven["frames"].items():
        assert "panic:" not in frame, f"Go panic after {label}"
        assert "goroutine " not in frame, f"Go stack trace after {label}"


def test_help_and_context_render_their_content(driven):
    for label, _, want in STEPS:
        if want == r".":
            continue
        frame = driven["frames"].get(label, "")
        assert re.search(want, frame, re.I), f"{label} produced no matching content"


def test_tui_survives_the_whole_sequence(driven):
    assert driven["alive"], "the TUI exited partway through its own commands"
