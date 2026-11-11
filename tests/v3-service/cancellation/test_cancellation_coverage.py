"""No production inference path may run without request-scoped cancellation.

The first cancellation fix covered /v3/generate and missed /v3/plan, which
builds its own LLMAdapter and serves real traffic. A generation there was as
uncancellable as the one that cost a real acquisition its 27th case. These
tests fail if a new handler, or a new adapter construction on a server path,
reintroduces that hole.
"""
import ast
import os
import re
import socket
import sys
import time
from types import SimpleNamespace

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "..", "services", "v3-service")
sys.path.insert(0, V3)
import adapters
import main


def src(name):
    with open(os.path.join(V3, name), encoding="utf-8") as fh:
        return fh.read()


def test_every_adapter_construction_is_accounted_for():
    """Each LLMAdapter build is either scope-assigned or an explicit exception."""
    found = []
    for name in ("pipeline.py", "run_steps.py", "planning.py", "adapters.py", "main.py"):
        for i, line in enumerate(src(name).split("\n"), 1):
            if "LLMAdapter(" in line and "class " not in line and not line.strip().startswith("#"):
                found.append((name, i, line.strip()))
    assert found, "no adapter constructions found; the guard is looking in the wrong place"
    for name, line_no, line in found:
        body = src(name)
        # A comment that names cancel_scope does not assign it.
        window = "\n".join(code for code in body.split("\n")[line_no - 1:line_no + 8]
                           if not code.strip().startswith("#"))
        assert "cancel_scope" in window, (
            f"{name}:{line_no} builds an LLMAdapter without assigning a cancel "
            f"scope within the next few lines: {line}")


def test_both_inference_handlers_create_a_scope():
    body = src("main.py")
    tree = ast.parse(body)
    handlers = {n.name for n in ast.walk(tree)
                if isinstance(n, ast.FunctionDef) and n.name.startswith("_handle_")}
    # Handlers that reach inference must take a scope; the rest must not need one.
    inference_handlers = {"_handle_generate", "_handle_plan"}
    assert inference_handlers <= handlers, handlers
    for h in sorted(inference_handlers):
        i = body.index(f"def {h}(")
        j = body.find("\n    def ", i + 1)
        block = body[i:j if j > 0 else len(body)]
        assert "_watch_parent_for(" in block, f"{h} runs inference without a cancel scope"
        assert "_release_scope(" in block, f"{h} never releases its scope"


def test_the_watcher_helper_uses_peek_not_read():
    """A watcher that consumes a byte would corrupt the request it guards."""
    body = src("main.py")
    i = body.index("def _watch_parent_for(")
    j = body.index("def _release_scope(")
    helper = body[i:j]
    assert "MSG_PEEK" in helper
    assert re.search(r"recv\(1,\s*socket\.MSG_PEEK", helper), helper
    assert "sock.read(" not in helper


def test_the_watcher_does_not_busy_spin():
    body = src("main.py")
    assert "WATCH_POLL_SEC" in body
    i = body.index("WATCH_POLL_SEC = ")
    val = float(body[i:].split("=")[1].split("\n")[0].strip())
    assert 0.05 <= val <= 1.0, f"watcher poll interval {val}s is outside a sane range"
    helper = body[body.index("def _watch_parent_for("):body.index("def _release_scope(")]
    assert "select.select" in helper, "the watcher must block on select, not spin"


def test_release_is_idempotent_and_bounded():
    body = src("main.py")
    i = body.index("def _release_scope(")
    j = body.index("class V3Handler")
    block = body[i:j]
    assert "stop_watch.set()" in block
    assert "scope.cancel()" in block
    assert "join(timeout=" in block, "watcher join must be bounded"


def test_scope_cancel_is_safe_to_call_twice():
    scope = adapters.CancelScope("inv")
    assert scope.cancel() == 0
    assert scope.cancel() == 0
    assert scope.cancelled


def test_a_successful_request_is_not_marked_cancelled_before_release():
    """Normal completion must not look like a cancellation to anything that
    inspects the scope during the run."""
    scope = adapters.CancelScope("inv")
    assert scope.cancelled is False
    class C:
        closed = 0
        def close(self):
            C.closed += 1
    c = C()
    scope.register(c)
    scope.unregister(c)
    assert scope.cancelled is False
    assert C.closed == 0


class _ScriptedParent:
    """A stand-in for the handler's socket. Each peek gets the next answer
    in the script, bytes or an exception to raise; after the script, every
    peek would block."""

    def __init__(self, answers):
        self.answers = list(answers)
        self.looks = 0

    def recv(self, size, flags):
        self.looks += 1
        answer = self.answers.pop(0) if self.answers else BlockingIOError()
        if isinstance(answer, Exception):
            raise answer
        return answer


@pytest.mark.parametrize("answers, looks", [
    pytest.param([b""], 1, id="a-hang-up-cancels"),
    pytest.param([ConnectionResetError()], 1, id="a-broken-socket-cancels"),
    pytest.param([ValueError()], 1, id="a-socket-that-cannot-peek-cancels"),
    pytest.param([BlockingIOError(), b""], 2, id="a-peek-that-would-block-keeps-watching"),
    pytest.param([b"x", b""], 2, id="request-bytes-keep-watching"),
])
def test_the_watcher_cancels_when_the_parent_hangs_up_or_breaks(monkeypatch, answers, looks):
    # select always says the socket is ready, so the script alone decides.
    monkeypatch.setattr(main, "select", SimpleNamespace(select=lambda r, w, x, timeout: (r, [], [])))
    parent = _ScriptedParent(answers)

    scope, stop_watch, watcher = main._watch_parent_for(SimpleNamespace(connection=parent), "test")
    watcher.join(timeout=5)
    still_watching = watcher.is_alive()
    cancelled = scope.cancelled
    main._release_scope(scope, stop_watch, watcher, "test")

    assert not still_watching
    assert cancelled
    assert parent.looks == looks


def test_a_watcher_that_cannot_select_stops_without_cancelling(monkeypatch):
    # The handler's own release cancels at its exit; the watcher does not.
    def select_fails(r, w, x, timeout):
        raise OSError("bad file descriptor")
    monkeypatch.setattr(main, "select", SimpleNamespace(select=select_fails))

    scope, stop_watch, watcher = main._watch_parent_for(SimpleNamespace(connection=_ScriptedParent([])), "test")
    watcher.join(timeout=5)
    still_watching = watcher.is_alive()
    cancelled = scope.cancelled
    main._release_scope(scope, stop_watch, watcher, "test")

    assert not still_watching
    assert not cancelled


def test_a_quiet_parent_is_watched_until_the_handler_stops_the_watcher():
    # The real select on a socket pair: while the parent sends nothing, each poll times out.
    handler_end, parent_end = socket.socketpair()
    scope, stop_watch, watcher = main._watch_parent_for(SimpleNamespace(connection=handler_end), "test")
    time.sleep(3 * main.WATCH_POLL_SEC)
    watching_while_quiet = watcher.is_alive()
    cancelled_while_quiet = scope.cancelled
    stop_watch.set()
    watcher.join(timeout=5)
    still_watching = watcher.is_alive()
    main._release_scope(scope, stop_watch, watcher, "test")
    handler_end.close()
    parent_end.close()

    assert watching_while_quiet
    assert not cancelled_while_quiet
    assert not still_watching
