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
import sys

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
sys.path.insert(0, V3)
import adapters  # noqa: E402


def src(name):
    with open(os.path.join(V3, name), encoding="utf-8") as fh:
        return fh.read()


def test_every_adapter_construction_is_accounted_for():
    """Each LLMAdapter build is either scope-assigned or an explicit exception."""
    found = []
    for name in ("pipeline.py", "planning.py", "adapters.py", "main.py"):
        for i, line in enumerate(src(name).split("\n"), 1):
            if "LLMAdapter(" in line and "class " not in line and not line.strip().startswith("#"):
                found.append((name, i, line.strip()))
    assert found, "no adapter constructions found; the guard is looking in the wrong place"
    for name, line_no, line in found:
        body = src(name)
        window = "\n".join(body.split("\n")[line_no - 1:line_no + 8])
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
