"""Every background-execution site in the services is classified, on purpose.

A worker thread inherits none of its parent's request context: not the
request-ID ContextVar, not the request's locals. Anything request-scoped it
needs has to be handed to it explicitly. The one site that forgot cost a
complete 42-case acquisition -- PlanSearch's fan-out sent 28 unattributed
inference calls, an attribution-enforcing upstream refused all of them, and
the stage reported an empty candidate list rather than an error.

This is the structural half of that fix. The behavioural half lives in
test_plansearch_request_context.py; this one makes sure the next background
worker cannot be added without someone stating which of the three cases it
is. It fails on an unlisted site, not on a wrong one -- the classification
is a review artifact, and the test's job is to force the review.
"""
import ast
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
SERVICES = ["v3-service", "geometric-lens", "sandbox-service"]

# Constructs that start work somewhere the current context does not follow by
# itself. asyncio.create_task is deliberately NOT here: it copies the current
# context at creation time, so a ContextVar set on the request does reach it.
BACKGROUND_CALLS = {
    "ThreadPoolExecutor",
    "ProcessPoolExecutor",
    "Thread",
    "run_in_executor",
    "to_thread",
    "fork",
}

# --- the audit ---------------------------------------------------------------
# Key:   "<path>::<enclosing def>::<construct>"
# Value: one of
#   "propagates"  — hands the worker every request-scoped value it uses
#   "no-request"  — runs no request-scoped work, so there is nothing to hand
#   "defective"   — known broken; must be empty on a green tree
#
# Adding a background call without adding it here fails this test. That is the
# point: the entry is where you say which case you are in.
CLASSIFIED = {
    "v3-service/stages/plan_search.py::_fan_out::ThreadPoolExecutor":
        ("propagates",
         "Captures the request ID on the owning thread and re-establishes it "
         "in each worker for log correlation. The identity the outbound call "
         "is sent under does not travel this way at all: it lives on the "
         "request-scoped LLMAdapter, which the worker already holds."),
    "v3-service/main.py::_watch_parent_for::Thread":
        ("propagates",
         "Watches the parent socket for EOF and cancels a scope it was handed "
         "directly. It sends no outbound request, but it DOES log the "
         "cancellation, so it captures the request and invocation ids on the "
         "owning thread and binds them as its first action. Classified "
         "no-request until 2026-08-24 on the grounds that `label` was enough: "
         "the sealed Stage-A acquisition then produced 159,533 records of "
         "which the only two unattributable ones came from here."),
}


def _sites():
    """Every background-execution call site, keyed by file, function, construct."""
    found = {}
    for service in SERVICES:
        base = ROOT / service
        if not base.is_dir():
            continue
        for path in sorted(base.rglob("*.py")):
            rel = path.relative_to(ROOT).as_posix()
            parts = set(path.parts)
            if parts & {"tests", "test", "__pycache__", ".venv", "venv"}:
                continue
            if path.name.startswith("test_"):
                continue
            try:
                tree = ast.parse(path.read_text(encoding="utf-8"))
            except (SyntaxError, UnicodeDecodeError):
                continue
            for enclosing, call in _calls_with_scope(tree):
                name = _call_name(call)
                if name in BACKGROUND_CALLS:
                    found[f"{rel}::{enclosing}::{name}"] = (rel, call.lineno)
    return found


def _calls_with_scope(tree):
    """Yield (enclosing function name, Call node) for every call in the tree."""
    out = []

    def walk(node, scope):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                walk(child, child.name)
            elif isinstance(child, ast.ClassDef):
                walk(child, scope)
            else:
                if isinstance(child, ast.Call):
                    out.append((scope, child))
                walk(child, scope)

    walk(tree, "<module>")
    return out


def _call_name(call):
    func = call.func
    if isinstance(func, ast.Name):
        return func.id
    if isinstance(func, ast.Attribute):
        return func.attr
    return ""


def test_every_background_site_is_classified():
    sites = _sites()
    unlisted = sorted(set(sites) - set(CLASSIFIED))
    assert not unlisted, (
        "background-execution site(s) with no recorded request-context "
        "classification:\n  " + "\n  ".join(
            f"{k} (line {sites[k][1]})" for k in unlisted) +
        "\n\nA worker inherits no ContextVar and no local from its parent. "
        "Add an entry to CLASSIFIED in this file saying which it is: "
        "'propagates', 'no-request', or 'defective'.")


def test_no_site_is_still_defective():
    defective = sorted(k for k, (verdict, _) in CLASSIFIED.items()
                       if verdict == "defective")
    assert not defective, f"known-defective background sites: {defective}"


def test_classification_list_has_no_stale_entries():
    """A removed site must not keep a classification: the entry would read as
    a reviewed guarantee about code that no longer exists."""
    sites = _sites()
    stale = sorted(set(CLASSIFIED) - set(sites))
    assert not stale, (
        f"CLASSIFIED names site(s) that no longer exist: {stale}")


def test_every_classification_states_a_reason():
    for key, (verdict, reason) in CLASSIFIED.items():
        assert verdict in {"propagates", "no-request", "defective"}, key
        assert len(reason) > 40, f"{key} has no real reason recorded"


def test_inference_path_takes_no_contextvar_fallback():
    """The header resolver must not read the request-ID ContextVar.

    A fallback there is what made the defect silent: in a worker it reads as
    "no request", so the call goes out unattributed and a permissive upstream
    answers it. Asserted structurally because the behavioural test can only
    show the value that arrived, not where it was allowed to come from.
    """
    src = ast.parse((ROOT / "v3-service" / "adapters.py").read_text())
    resolver = next(
        (n for n in ast.walk(src)
         if isinstance(n, ast.FunctionDef) and n.name == "_inference_headers"),
        None)
    assert resolver is not None, "LLMAdapter._inference_headers is gone"
    names = {_call_name(c) for c in ast.walk(resolver) if isinstance(c, ast.Call)}
    assert "get_request_id" not in names, (
        "_inference_headers consults the request-ID ContextVar; a worker "
        "thread reads the default there and sends the call unattributed")
    assert "_service_headers" not in names, (
        "_inference_headers delegates to _service_headers, which falls back "
        "to the ContextVar")


def test_missing_identity_under_a_request_raises():
    """Fail closed: serving a request with no identity is a wiring error."""
    import sys
    v3 = str(ROOT / "v3-service")
    if v3 not in sys.path:
        sys.path.insert(0, v3)
    import adapters

    llm = adapters.LLMAdapter()
    llm.cancel_scope = adapters.CancelScope(invocation_id="inv")
    llm.request_identity = None
    with pytest.raises(adapters.RequestIdentityMissing):
        llm._inference_headers()
