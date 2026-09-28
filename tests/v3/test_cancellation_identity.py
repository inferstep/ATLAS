"""A cancellation record names the request and invocation it cancelled.

The sealed Stage-A acquisition emitted 159,533 structured V3 records. All but
two carried a request id, and the two that did not were both

    [generate] parent disconnected; cancelled N in-flight generation(s)

emitted from the watcher thread `_watch_parent_for` starts. The scope's
invocation id is bound on the HANDLER thread; a `threading.Thread` inherits
no ContextVar, so inside the watcher both ids are empty and the record that
reports a cancellation cannot be joined to the work it cancelled.

Identity is passed explicitly here, never inherited. `copy_context()` would
also carry it, and is the wrong instrument: the watcher can outlive the
context it was started from, and a copied context would keep answering with a
stale identity long after the request it belonged to is gone.
"""
import ast
import io
import json
import os
import sys
import threading

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
if V3 not in sys.path:
    sys.path.insert(0, V3)

import structured_log as SL  # noqa: E402

MAIN = os.path.join(V3, "main.py")


@pytest.fixture(autouse=True)
def _clean_identity():
    SL.bind_identity("", "")
    yield
    SL.bind_identity("", "")


def _src():
    return open(MAIN, encoding="utf-8").read()


def _func(name):
    tree = ast.parse(_src())
    return next(n for n in ast.walk(tree)
                if isinstance(n, ast.FunctionDef) and n.name == name)


def _calls(node):
    return {getattr(c.func, "id", None) or getattr(c.func, "attr", None)
            for c in ast.walk(node) if isinstance(c, ast.Call)}


# --- the watcher binds, explicitly -------------------------------------------

def test_the_watcher_captures_identity_on_the_owning_thread():
    """The capture must happen in _watch_parent_for's own body, which runs on
    the handler thread, not inside the closure that runs on the new one."""
    outer = _func("_watch_parent_for")
    inner = next(n for n in ast.walk(outer)
                 if isinstance(n, ast.FunctionDef) and n.name == "_watch")
    inner_lines = {getattr(n, "lineno", None) for n in ast.walk(inner)}
    captured_outside = any(
        (getattr(c.func, "id", None) or getattr(c.func, "attr", None)) == "current_identity"
        and c.lineno not in inner_lines
        for c in ast.walk(outer) if isinstance(c, ast.Call))
    assert captured_outside, (
        "_watch_parent_for does not capture the identity on the owning thread")


def test_the_watcher_binds_before_it_can_log():
    """bind_identity must be the watcher's first action: anything logged
    before it would be unattributed, which is the whole defect."""
    inner = next(n for n in ast.walk(_func("_watch_parent_for"))
                 if isinstance(n, ast.FunctionDef) and n.name == "_watch")
    first = inner.body[0]
    call = getattr(first, "value", None)
    name = (getattr(getattr(call, "func", None), "id", None)
            or getattr(getattr(call, "func", None), "attr", None))
    assert name == "bind_identity", (
        "the watcher's first statement is %r, not bind_identity" % ast.dump(first)[:80])


def test_identity_is_not_inherited_implicitly():
    """Checked on the code, not the prose: the reason this is the wrong
    instrument is written in a comment that names it."""
    names = set()
    for node in ast.walk(ast.parse(_src())):
        if isinstance(node, ast.Call):
            names.add(getattr(node.func, "id", None) or getattr(node.func, "attr", None))
        elif isinstance(node, (ast.Import, ast.ImportFrom)):
            names.update(a.name for a in node.names)
    assert "copy_context" not in names, (
        "copy_context() carries a context that can outlive the request it "
        "came from; hand the identity over explicitly instead")


def test_the_watcher_and_release_log_through_the_governed_logger():
    """A bare print() bypasses the formatter that stamps identity."""
    for fn in ("_watch_parent_for", "_release_scope"):
        node = _func(fn)
        for call in ast.walk(node):
            if isinstance(call, ast.Call) and getattr(call.func, "id", None) == "print":
                pytest.fail("%s still logs with a bare print()" % fn)


# --- behaviour ----------------------------------------------------------------

def _run_watcher_like(identity, sink):
    """The production hand-off, reproduced: capture on this thread, bind on
    the worker as its first action, then log."""
    captured = identity

    def worker():
        SL.bind_identity(*captured)
        sink.append(SL.current_identity())

    t = threading.Thread(target=worker)
    t.start()
    t.join()


def test_a_worker_without_the_hand_off_sees_nothing():
    """The defect, stated: a thread inherits neither ContextVar."""
    SL.bind_identity("req-X", "inv-X")
    seen = []

    def worker():
        seen.append(SL.current_identity())

    t = threading.Thread(target=worker)
    t.start()
    t.join()
    assert seen == [("", "")], (
        "a plain thread inherited an identity; the hand-off would be untestable")


def test_the_hand_off_restores_the_owning_identity():
    SL.bind_identity("req-X", "inv-X")
    seen = []
    _run_watcher_like(SL.current_identity(), seen)
    assert seen == [("req-X", "inv-X")]


def test_two_concurrent_requests_cannot_exchange_identities():
    errs, seen = [], {}
    lock = threading.Lock()

    def request(tag):
        SL.bind_identity("req-" + tag, "inv-" + tag)
        out = []
        _run_watcher_like(SL.current_identity(), out)
        with lock:
            seen[tag] = out
        if out != [("req-" + tag, "inv-" + tag)]:
            errs.append((tag, out))

    ta = threading.Thread(target=request, args=("A",))
    tb = threading.Thread(target=request, args=("B",))
    ta.start(); tb.start(); ta.join(); tb.join()
    assert not errs, errs
    assert seen["A"] == [("req-A", "inv-A")]
    assert seen["B"] == [("req-B", "inv-B")]


def test_parallel_invocations_under_one_request_keep_distinct_invocation_ids():
    out = []
    lock = threading.Lock()

    def invocation(n):
        SL.bind_identity("req-shared", "inv-%d" % n)
        got = []
        _run_watcher_like(SL.current_identity(), got)
        with lock:
            out.extend(got)

    ts = [threading.Thread(target=invocation, args=(i,)) for i in range(4)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()
    assert {r for r, _ in out} == {"req-shared"}
    assert {i for _, i in out} == {"inv-0", "inv-1", "inv-2", "inv-3"}


def test_a_reused_worker_carries_no_stale_identity():
    SL.bind_identity("req-1", "inv-1")
    first, second = [], []
    _run_watcher_like(SL.current_identity(), first)
    # The next request binds nothing: the worker must not still answer "req-1".
    _run_watcher_like(("", ""), second)
    assert first == [("req-1", "inv-1")]
    assert second == [("", "")]


def test_an_absent_identity_stays_absent_rather_than_borrowed():
    import logging
    SL.bind_identity("", "")
    fmt = SL.JsonFormatter("v3-service")
    rec = logging.LogRecord("t", logging.INFO, __file__, 1, "cancelled 1", None, None)
    out = json.loads(fmt.format(rec))
    assert "request_id" not in out
    assert "invocation_id" not in out


def test_a_bound_cancellation_record_carries_both_ids():
    import logging
    SL.bind_identity("req-c", "inv-c")
    fmt = SL.JsonFormatter("v3-service")
    rec = logging.LogRecord("t", logging.INFO, __file__, 1,
                            "[generate] parent disconnected; cancelled 2 "
                            "in-flight generation(s)", None, None)
    out = json.loads(fmt.format(rec))
    assert out["request_id"] == "req-c"
    assert out["invocation_id"] == "inv-c"


# --- structural: every service thread that logs must bind -------------------

def test_every_thread_the_service_starts_binds_identity_before_logging():
    """A new background thread that logs without binding is unattributable."""
    tree = ast.parse(_src())
    offenders = []
    for node in ast.walk(tree):
        if not isinstance(node, ast.FunctionDef):
            continue
        starts_thread = any(
            (getattr(c.func, "id", None) or getattr(c.func, "attr", None)) == "Thread"
            for c in ast.walk(node) if isinstance(c, ast.Call))
        if not starts_thread:
            continue
        inner = [n for n in ast.walk(node)
                 if isinstance(n, ast.FunctionDef) and n is not node]
        for fn in inner:
            logs = any(
                (getattr(c.func, "id", None) or getattr(c.func, "attr", None))
                in {"print", "info", "warning", "error", "exception"}
                for c in ast.walk(fn) if isinstance(c, ast.Call))
            binds = "bind_identity" in _calls(fn)
            if logs and not binds:
                offenders.append("%s::%s" % (node.name, fn.name))
    assert not offenders, (
        "these service threads log without binding identity first: %s" % offenders)


# --- the governed copies stay identical --------------------------------------

def test_governed_logger_copies_remain_byte_identical():
    import hashlib
    root = os.path.join(os.path.dirname(__file__), "..", "..")
    copies = [
        os.path.join(root, "geometric-lens", "geometric_lens", "structured_log.py"),
        os.path.join(root, "sandbox", "structured_log.py"),
        os.path.join(root, "v3-service", "structured_log.py"),
    ]
    digests = {p: hashlib.sha256(open(p, "rb").read()).hexdigest() for p in copies}
    assert len(set(digests.values())) == 1, digests
