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
import hashlib
import json
import logging
import os
import sys
import threading
from pathlib import Path

import pytest

V3 = os.path.join(os.path.dirname(__file__), "..", "..", "v3-service")
if V3 not in sys.path:
    sys.path.insert(0, V3)

import structured_log as SL

MAIN_TREE = ast.parse(Path(V3, "main.py").read_text(encoding="utf-8"))


@pytest.fixture(autouse=True)
def _clean_identity():
    SL.bind_identity("", "")
    yield
    SL.bind_identity("", "")


def function_named(tree, name):
    return next(n for n in ast.walk(tree) if isinstance(n, ast.FunctionDef) and n.name == name)


def called_names(node):
    """The names of all functions and methods called anywhere inside node."""
    return {getattr(c.func, "id", None) or getattr(c.func, "attr", None)
            for c in ast.walk(node) if isinstance(c, ast.Call)}


# --- the watcher binds, explicitly -------------------------------------------

def test_the_watcher_captures_identity_on_the_handler_thread_and_binds_it_first():
    outer = function_named(MAIN_TREE, "_watch_parent_for")
    watch = function_named(outer, "_watch")
    # The capture must be in _watch_parent_for's own body, which runs on the
    # handler thread, not inside _watch, which runs on the new thread.
    outer_body_calls = set()
    for statement in outer.body:
        if statement is not watch:
            outer_body_calls |= called_names(statement)
    assert "current_identity" in outer_body_calls, (
        "_watch_parent_for does not capture the identity on the owning thread")
    # bind_identity must be the watcher's first action: anything logged
    # before it would be unattributed, which is the whole defect.
    first = watch.body[0]
    assert called_names(first) == {"bind_identity"}, (
        "the watcher's first statement is %r, not bind_identity" % ast.unparse(first))


def test_identity_is_not_inherited_implicitly():
    """Checked on the code, not the prose: the reason this is the wrong
    instrument is written in a comment that names it."""
    names = called_names(MAIN_TREE)
    for node in ast.walk(MAIN_TREE):
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            names.update(alias.name for alias in node.names)
    assert "copy_context" not in names, (
        "copy_context() carries a context that can outlive the request it "
        "came from; hand the identity over explicitly instead")


def test_the_watcher_and_release_log_through_the_governed_logger():
    """A bare print() bypasses the formatter that stamps identity."""
    for name in ("_watch_parent_for", "_release_scope"):
        assert "print" not in called_names(function_named(MAIN_TREE, name)), (
            "%s still logs with a bare print()" % name)


def test_every_thread_the_service_starts_binds_identity_before_logging():
    """A new background thread that logs without binding is unattributable."""
    # _service_log is how the service's own threads log (see main.py).
    logging_calls = {"_service_log", "print", "info", "warning", "error", "exception"}
    offenders = []
    for starter in ast.walk(MAIN_TREE):
        if not isinstance(starter, ast.FunctionDef) or "Thread" not in called_names(starter):
            continue
        for fn in ast.walk(starter):
            if isinstance(fn, ast.FunctionDef) and fn is not starter:
                calls = called_names(fn)
                if calls & logging_calls and "bind_identity" not in calls:
                    offenders.append("%s::%s" % (starter.name, fn.name))
    assert not offenders, (
        "these service threads log without binding identity first: %s" % offenders)


# --- behaviour ----------------------------------------------------------------

def identity_seen_by_a_new_thread(handed_over):
    """The production hand-off, reproduced: the new thread binds the identity
    it was handed as its first action, then reads it back."""
    seen = []

    def worker():
        SL.bind_identity(*handed_over)
        seen.append(SL.current_identity())

    t = threading.Thread(target=worker)
    t.start()
    t.join()
    return seen[0]


def test_a_thread_without_the_hand_off_sees_no_identity():
    """The defect, stated: a thread inherits neither ContextVar."""
    SL.bind_identity("req-X", "inv-X")
    seen = []
    t = threading.Thread(target=lambda: seen.append(SL.current_identity()))
    t.start()
    t.join()
    assert seen == [("", "")], (
        "a plain thread inherited an identity; the hand-off would be untestable")


def test_a_watcher_thread_sees_only_the_identity_handed_to_it():
    SL.bind_identity("req-1", "inv-1")
    assert identity_seen_by_a_new_thread(SL.current_identity()) == ("req-1", "inv-1")
    # The next request hands over nothing: its thread must not still answer "req-1".
    assert identity_seen_by_a_new_thread(("", "")) == ("", "")


@pytest.mark.parametrize("identities", [
    pytest.param([("req-A", "inv-A"), ("req-B", "inv-B")], id="two-requests"),
    pytest.param([("req-shared", "inv-%d" % n) for n in range(4)],
                 id="four-invocations-of-one-request"),
])
def test_concurrent_handlers_each_hand_over_their_own_identity(identities):
    seen = {}

    def handler(identity):
        SL.bind_identity(*identity)
        seen[identity] = identity_seen_by_a_new_thread(SL.current_identity())

    threads = [threading.Thread(target=handler, args=(identity,)) for identity in identities]
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    assert seen == {identity: identity for identity in identities}


@pytest.mark.parametrize("bound, expected_ids", [
    pytest.param(("", ""), {}, id="absent-ids-stay-absent"),
    pytest.param(("req-c", "inv-c"), {"request_id": "req-c", "invocation_id": "inv-c"},
                 id="bound-ids-are-stamped"),
])
def test_a_cancellation_record_carries_exactly_the_bound_ids(bound, expected_ids):
    SL.bind_identity(*bound)
    record = logging.LogRecord("t", logging.INFO, __file__, 1,
                               "[generate] parent disconnected; cancelled 2 "
                               "in-flight generation(s)", None, None)
    out = json.loads(SL.JsonFormatter("v3-service").format(record))
    ids_in_record = {key: out[key] for key in ("request_id", "invocation_id") if key in out}
    assert ids_in_record == expected_ids


# --- the governed copies stay identical --------------------------------------

def test_governed_logger_copies_remain_byte_identical():
    root = os.path.join(os.path.dirname(__file__), "..", "..")
    copies = [
        os.path.join(root, "geometric-lens", "geometric_lens", "structured_log.py"),
        os.path.join(root, "sandbox", "structured_log.py"),
        os.path.join(root, "v3-service", "structured_log.py"),
    ]
    digests = {p: hashlib.sha256(Path(p).read_bytes()).hexdigest() for p in copies}
    assert len(set(digests.values())) == 1, digests
