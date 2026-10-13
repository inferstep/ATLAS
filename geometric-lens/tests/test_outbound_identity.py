"""Outbound attribution on every model-bound Lens call, through one transport.

The Lens receives X-ATLAS-Request-ID and X-ATLAS-V3-Invocation-ID from V3 and
must forward exactly that pair on its embedding and served-model calls. These
tests run the real ASGI middleware and the real urllib transport against a
header-capturing HTTP stub; nothing contacts a model.
"""
import json
import os
import re
import socket
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.join(HERE, "..")
sys.path.insert(0, ROOT)

from geometric_lens import embedding_extractor as ee
from geometric_lens import model_transport as mt
from geometric_lens import structured_log as sl

CAPTURED = []
LOCK = threading.Lock()


class _Stub(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _reply(self, obj):
        b = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(b)))
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        with LOCK:
            CAPTURED.append({"path": self.path, "rid": self.headers.get("X-ATLAS-Request-ID", ""),
                             "inv": self.headers.get("X-ATLAS-V3-Invocation-ID", "")})
        self._reply({"object": "list", "data": [{"id": "stub-model"}]})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(n)
        with LOCK:
            CAPTURED.append({"path": self.path, "rid": self.headers.get("X-ATLAS-Request-ID", ""),
                             "inv": self.headers.get("X-ATLAS-V3-Invocation-ID", ""), "body": body.decode("utf-8", "replace")})
        text = json.loads(body).get("content", "")
        self._reply([{"index": 0, "embedding": [[0.01 * (i % 7) for i in range(8)] for _ in range(max(1, len(text.split())))]}])


@pytest.fixture(scope="module")
def stub():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    httpd = ThreadingHTTPServer(("127.0.0.1", port), _Stub)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    url = f"http://127.0.0.1:{port}"
    old = {k: os.environ.get(k) for k in ("LLAMA_URL", "LLAMA_EMBED_URL")}
    os.environ["LLAMA_URL"] = url
    os.environ["LLAMA_EMBED_URL"] = url
    yield url
    httpd.shutdown()
    for k, v in old.items():
        if v is None:
            os.environ.pop(k, None)
        else:
            os.environ[k] = v


@pytest.fixture(autouse=True)
def _clear():
    with LOCK:
        CAPTURED.clear()
    sl.bind_identity("", "")
    yield
    sl.bind_identity("", "")


def _captured(path_prefix="/embedding"):
    with LOCK:
        return [c for c in CAPTURED if c["path"].startswith(path_prefix)]


# --- the transport --------------------------------------------------------------------

def test_no_bound_identity_means_no_attribution_headers(stub):
    ee._post_embedding("def f(): pass")
    c = _captured()
    assert len(c) == 1 and c[0]["rid"] == "" and c[0]["inv"] == ""


def test_bound_pair_is_forwarded_exactly(stub):
    sl.bind_identity("req-1", "inv-1")
    ee._post_embedding("def f(): pass")
    assert _captured()[-1]["rid"] == "req-1" and _captured()[-1]["inv"] == "inv-1"


def test_partial_identity_stays_partial(stub):
    sl.bind_identity("req-only", "")
    ee._post_embedding("x = 1")
    assert (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("req-only", "")
    sl.bind_identity("", "inv-only")
    ee._post_embedding("x = 2")
    assert (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("", "inv-only")


def test_repeated_calls_under_one_binding_carry_the_same_pair(stub):
    sl.bind_identity("req-r", "inv-r")
    for _ in range(3):
        ee._post_embedding("y = 3")
    assert {(c["rid"], c["inv"]) for c in _captured()} == {("req-r", "inv-r")}


def test_served_model_probe_carries_the_bound_pair(stub):
    from geometric_lens import service
    service._served_model_probed = False
    service._served_model_id = ""
    sl.bind_identity("req-p", "inv-p")
    assert service._probe_served_model() == "stub-model"
    c = _captured("/v1/models")
    assert c and c[-1]["rid"] == "req-p" and c[-1]["inv"] == "inv-p"
    service._served_model_probed = False
    service._served_model_id = ""


def test_headers_never_carry_content(stub):
    sl.bind_identity("req-c", "inv-c")
    ee._post_embedding("SECRET_CANDIDATE_BYTES = 1")
    c = _captured()[-1]
    assert "SECRET" not in c["rid"] + c["inv"]
    assert set(mt.model_headers().keys()) <= {"Authorization", mt.REQUEST_ID_HEADER, mt.INVOCATION_ID_HEADER, "Content-Type"}


def test_concurrent_threads_never_exchange_identities(stub):
    """Each thread binds its own pair and makes several calls; every outbound
    call must carry the pair of the thread that made it. Repeated to shake out
    ordering luck."""
    for _round in range(20):
        with LOCK:
            CAPTURED.clear()
        errors = []

        def worker(i):
            try:
                sl.bind_identity(f"req-{i}", f"inv-{i}")
                for k in range(3):
                    ee._post_embedding(f"z{i}_{k} = {k}")
                    if (sl.current_identity()) != (f"req-{i}", f"inv-{i}"):
                        errors.append(("identity drifted", i))
            except Exception as e:  # pragma: no cover
                errors.append(repr(e))
        threads = [threading.Thread(target=worker, args=(i,)) for i in range(12)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert errors == []
        for c in _captured():
            i = int(re.search(r"z(\d+)_", c["body"]).group(1))
            assert (c["rid"], c["inv"]) == (f"req-{i}", f"inv-{i}"), c


def test_background_thread_without_binding_forwards_nothing(stub):
    sl.bind_identity("req-main", "inv-main")
    out = {}

    def bg():
        ee._post_embedding("bg = 1")
        out["identity"] = sl.current_identity()
    t = threading.Thread(target=bg)
    t.start()
    t.join()
    assert out["identity"] == ("", "")
    assert (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("", "")


# --- the middleware binds and clears --------------------------------------------------

@pytest.fixture(scope="module")
def app_client(stub, tmp_path_factory):
    tmp = tmp_path_factory.mktemp("lens")
    os.environ["ATLAS_SERVICE_TOKEN_FILE"] = str(tmp / "no-token")
    from fastapi.testclient import TestClient
    # The Lens app uses flat imports (config, main); another
    # service's flat modules of the same names may already be cached when several
    # test trees share one session, so import the Lens app from its own root.
    for name in ("main", "config"):
        mod = sys.modules.get(name)
        if mod is not None and not str(getattr(mod, "__file__", "")).startswith(os.path.abspath(ROOT)):
            del sys.modules[name]
    sys.path.insert(0, os.path.abspath(ROOT))
    cwd = os.getcwd()
    os.chdir(ROOT)
    try:
        import importlib
        main = importlib.import_module("main")
    finally:
        os.chdir(cwd)
    assert hasattr(main, "app"), "the Lens main module was not the one imported"
    with TestClient(main.app) as client:
        yield client, main


def test_middleware_binds_both_ids_for_the_request_and_clears_after(app_client, stub):
    client, main = app_client
    seen = {}

    @main.app.get("/internal/_identity_probe_test")
    def _probe():
        seen["during"] = sl.current_identity()
        ee._post_embedding("probe = 1")
        return {"ok": True}

    r = client.get("/internal/_identity_probe_test", headers={"X-ATLAS-Request-ID": "req-m", "X-ATLAS-V3-Invocation-ID": "inv-m"})
    assert r.status_code == 200 and r.headers.get("X-ATLAS-Request-ID") == "req-m"
    assert seen["during"] == ("req-m", "inv-m")
    assert (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("req-m", "inv-m")
    r = client.get("/internal/_identity_probe_test")
    assert seen["during"] == ("", "") and (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("", "")


def test_exception_in_a_handler_still_clears_the_binding(app_client, stub):
    client, main = app_client
    state = {}

    @main.app.get("/internal/_identity_raise_test")
    def _raise():
        state["during"] = sl.current_identity()
        raise RuntimeError("boom")

    with pytest.raises(RuntimeError):
        client.get("/internal/_identity_raise_test", headers={"X-ATLAS-Request-ID": "req-x", "X-ATLAS-V3-Invocation-ID": "inv-x"})
    assert state["during"] == ("req-x", "inv-x")
    assert sl.current_identity() == ("", "")


def test_concurrent_http_requests_keep_their_own_pairs(app_client, stub):
    client, main = app_client

    @main.app.get("/internal/_identity_concurrent_test")
    def _conc(tag: str):
        time.sleep(0.01)
        ee._post_embedding(f"conc_{tag} = 1")
        return {"identity": list(sl.current_identity())}

    for _round in range(5):
        with LOCK:
            CAPTURED.clear()
        results = {}

        def hit(i):
            r = client.get(f"/internal/_identity_concurrent_test?tag={i}",
                           headers={"X-ATLAS-Request-ID": f"r{i}", "X-ATLAS-V3-Invocation-ID": f"i{i}"})
            results[i] = r.json()["identity"]
        threads = [threading.Thread(target=hit, args=(i,)) for i in range(8)]
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        assert all(results[i] == [f"r{i}", f"i{i}"] for i in range(8)), results
        for c in _captured():
            i = int(re.search(r"conc_(\d+)", c["body"]).group(1))
            assert (c["rid"], c["inv"]) == (f"r{i}", f"i{i}"), c


# --- structural guards ----------------------------------------------------------------

def _src(name):
    return open(os.path.join(ROOT, name), encoding="utf-8").read()


def test_every_model_bound_call_uses_the_single_transport():
    for name in ("geometric_lens/embedding_extractor.py", "geometric_lens/service.py"):
        text = _src(name)
        assert "urlopen(" not in text and "Request(" not in text.replace("model_request(", ""), name
    assert "model_request(" in _src("geometric_lens/embedding_extractor.py")
    assert "model_request(" in _src("geometric_lens/service.py")
    assert "_model_headers()" in _src("main.py")


def test_no_second_identity_store_exists():
    for dirpath, _d, files in os.walk(os.path.join(ROOT, "geometric_lens")):
        for f in files:
            if f.endswith(".py") and f != "structured_log.py":
                text = open(os.path.join(dirpath, f), encoding="utf-8").read()
                assert "ContextVar(" not in text, f
    assert "ContextVar(" not in _src("main.py")


def test_scoring_and_selection_never_read_the_attribution_headers():
    for name in ("geometric_lens/service.py", "geometric_lens/cost_field.py", "geometric_lens/thresholds.py",
                 "geometric_lens/calibration.py", "geometric_lens/identity.py"):
        text = _src(name)
        assert "X-ATLAS-Request-ID" not in text and "X-ATLAS-V3-Invocation-ID" not in text, name
        assert "identity_headers(" not in text and "current_identity(" not in text, name


# --- declared startup identity ----------------------------------------------------------

def test_startup_identity_is_bound_only_when_both_variables_are_declared(monkeypatch, stub):
    monkeypatch.delenv(mt.STARTUP_REQUEST_ID_ENV, raising=False)
    monkeypatch.delenv(mt.STARTUP_INVOCATION_ID_ENV, raising=False)
    with mt.startup_identity() as declared:
        assert declared is None and sl.current_identity() == ("", "")
        ee._post_embedding("boot = 0")
    assert (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("", "")
    monkeypatch.setenv(mt.STARTUP_REQUEST_ID_ENV, "probe-req")
    with mt.startup_identity() as declared:      # half a pair is ignored
        assert declared is None and sl.current_identity() == ("", "")
    monkeypatch.setenv(mt.STARTUP_INVOCATION_ID_ENV, "probe-inv")
    sl.bind_identity("req-before", "inv-before")
    with mt.startup_identity() as declared:
        assert declared == ("probe-req", "probe-inv") and sl.current_identity() == ("probe-req", "probe-inv")
        ee._post_embedding("boot = 1")
    assert (_captured()[-1]["rid"], _captured()[-1]["inv"]) == ("probe-req", "probe-inv")
    assert sl.current_identity() == ("req-before", "inv-before")   # restored, not cleared


def test_startup_identity_restores_on_exception(monkeypatch):
    monkeypatch.setenv(mt.STARTUP_REQUEST_ID_ENV, "probe-req")
    monkeypatch.setenv(mt.STARTUP_INVOCATION_ID_ENV, "probe-inv")
    sl.bind_identity("", "")
    with pytest.raises(RuntimeError):
        with mt.startup_identity():
            raise RuntimeError("boot failed")
    assert sl.current_identity() == ("", "")


def test_self_test_runs_under_the_declared_startup_identity():
    text = _src("main.py")
    assert text.count("_run_lens_self_test()") >= 2
    # every boot / readiness self-test call sits inside the declared startup identity
    assert "with _startup_identity():\n        _run_lens_self_test()" in text
    assert "with _startup_identity():\n            _run_lens_self_test()" in text
