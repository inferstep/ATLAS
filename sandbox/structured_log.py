"""Structured JSON logging + correlation IDs (Python services).

CANONICAL COPY NOTICE: byte-identical copies live in geometric-lens/
geometric_lens/, sandbox/, and v3-service/ (separate containers, no
shared package). tests/contracts/test_structured_log_contract.py
enforces they stay identical. Edit all copies together.

`install(service, root_logger)` attaches a JSON formatter when
ATLAS_LOG_FORMAT=json (else leaves the human format), plus the
private-value filter so records are masked before serialization. The
correlation ID for the current request is set via set_request_id() (from
an inbound X-ATLAS-Request-ID header) and included in every record.
"""

import json
import logging
import os
import contextvars

try:  # package layout (geometric-lens)
    from .private_values import filter_private_values
except ImportError:
    try:  # flat layout (sandbox/v3 copy)
        from private_values import filter_private_values  # type: ignore
    except ImportError:  # loaded as a standalone file (contract tests)
        import importlib.util as _ilu
        _spec = _ilu.spec_from_file_location(
            "_atlas_private_values",
            os.path.join(os.path.dirname(os.path.abspath(__file__)),
                         "private_values.py"))
        _mod = _ilu.module_from_spec(_spec)
        _spec.loader.exec_module(_mod)
        filter_private_values = _mod.filter_private_values

# contextvars, NOT threading.local: the async FastAPI services (lens,
# sandbox) interleave requests on one event-loop thread, so a thread-
# local id would bleed between concurrent requests. A ContextVar is
# isolated per async task AND per thread, so it is correct for both the
# async services and the thread-per-request v3 service.
_REQUEST_ID: "contextvars.ContextVar[str]" = contextvars.ContextVar(
    "atlas_request_id", default="")


# The invocation id identifies ONE /v3/generate or /v3/plan call. A request
# id can cover several (the planner and the generation pipeline share it, and
# a plan revision adds another), so joining relay traffic to a specific
# pipeline run needs both. Carried beside the request id for the same reason:
# a reader correlating a log line with a relay call must not have to infer
# which invocation it belonged to from timing.
_INVOCATION_ID: "contextvars.ContextVar[str]" = contextvars.ContextVar(
    "atlas_invocation_id", default="")


def set_request_id(request_id):
    _REQUEST_ID.set(request_id or "")


def get_request_id():
    return _REQUEST_ID.get()


def set_invocation_id(invocation_id):
    _INVOCATION_ID.set(invocation_id or "")


def get_invocation_id():
    return _INVOCATION_ID.get()


def current_identity():
    """(request_id, invocation_id) for the calling thread or task.

    Captured on the owning thread and handed to workers explicitly -- a
    worker inherits neither ContextVar, which is the defect that cost a
    whole acquisition.
    """
    return _REQUEST_ID.get(), _INVOCATION_ID.get()


def bind_identity(request_id, invocation_id):
    """Re-establish a captured identity inside a worker thread."""
    _REQUEST_ID.set(request_id or "")
    _INVOCATION_ID.set(invocation_id or "")


class JsonFormatter(logging.Formatter):
    def __init__(self, service):
        super().__init__()
        self.service = service

    def format(self, record):
        rec = {
            "ts": self.formatTime(record, "%Y-%m-%dT%H:%M:%S%z"),
            "level": record.levelname,
            "service": self.service,
            "logger": record.name,
            "msg": record.getMessage(),
        }
        rid = get_request_id()
        if rid:
            rec["request_id"] = rid
        inv = get_invocation_id()
        if inv:
            rec["invocation_id"] = inv
        if record.exc_text:
            # Pre-formatted (and private-value-masked) by
            # PrivateValueLogFilter.
            rec["exc"] = record.exc_text
        elif record.exc_info:
            # Filter not installed on this handler — mask here so the
            # traceback's `ExceptionType: <message>` line can't leak a
            # credential into the JSON record.
            rec["exc"] = filter_private_values(
                self.formatException(record.exc_info))
        return json.dumps(rec)


def install(service, root_logger=None):
    """JSON format when ATLAS_LOG_FORMAT=json; always attach the
    private-value filter. Idempotent."""
    root = root_logger or logging.getLogger()
    # private-value masking (shared filter)
    try:
        from .private_values import PrivateValueLogFilter
    except ImportError:  # flat layout (sandbox/v3 copy)
        from private_values import PrivateValueLogFilter  # type: ignore
    if os.environ.get("ATLAS_LOG_FORMAT", "").lower() == "json":
        fmt = JsonFormatter(service)
        for h in root.handlers:
            h.setFormatter(fmt)
    for h in root.handlers:
        if not any(isinstance(f, PrivateValueLogFilter) for f in h.filters):
            h.addFilter(PrivateValueLogFilter())
