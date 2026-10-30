"""structured_log.py copies stay byte-identical + behave correctly.

The copy list lives in tests/contracts/__init__.py — that registry is what
puts a copy under contract, and test_no_unregistered_copies (in
test_private_value_filtering.py) fails if a copy exists without being in it.
"""

import asyncio
import hashlib
import importlib.util
import json
import logging

from tests.contracts import COPY_NOTICE_MARKER, STRUCTURED_LOG_COPIES as COPIES


def test_copies_byte_identical():
    digests = {p: hashlib.sha256(p.read_bytes()).hexdigest() for p in COPIES}
    assert len(set(digests.values())) == 1, (
        f"structured_log.py copies drifted — edit all {len(COPIES)} together:\n"
        + "\n".join(f"  {p}: {d[:16]}" for p, d in digests.items()))


def test_each_copy_points_at_this_contract():
    """A reader who opens any copy must be told it is a copy, and where the
    enforcement lives. Without this the notice is decoration that can rot."""
    for path in COPIES:
        text = path.read_text()
        assert COPY_NOTICE_MARKER in text, f"{path}: no copy notice"
        assert "tests/contracts/test_structured_log_contract.py" in text, (
            f"{path}: copy notice does not name the enforcing test")


def _load(path):
    spec = importlib.util.spec_from_file_location(
        f"sl_{path.parent.name}", path)
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


def test_request_id_roundtrip():
    m = _load(COPIES[0])
    m.set_request_id("req-abc")
    assert m.get_request_id() == "req-abc"
    m.set_request_id("")
    assert m.get_request_id() == ""


def test_json_formatter_shape():
    m = _load(COPIES[0])
    m.set_request_id("req-json")
    rec = logging.LogRecord("n", logging.INFO, "f", 1, "hello", None, None)
    d = json.loads(m.JsonFormatter("svc").format(rec))
    assert d["service"] == "svc"
    assert d["level"] == "INFO"
    assert d["msg"] == "hello"
    assert d["request_id"] == "req-json"
    m.set_request_id("")


def test_request_id_isolated_across_async_tasks():
    """Concurrent async tasks must not see each other's request id — the
    reason this uses contextvars, not threading.local (which would bleed
    between interleaved requests on one event-loop thread)."""
    m = _load(COPIES[0])
    seen = {}

    async def worker(name):
        m.set_request_id(f"req-{name}")
        await asyncio.sleep(0)          # yield: another task runs here
        await asyncio.sleep(0)
        seen[name] = m.get_request_id()  # must still be our own id

    async def run():
        await asyncio.gather(*(worker(n) for n in ("a", "b", "c", "d")))

    asyncio.run(run())
    assert seen == {"a": "req-a", "b": "req-b", "c": "req-c", "d": "req-d"}, seen
