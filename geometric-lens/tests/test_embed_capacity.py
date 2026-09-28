"""The /embedding physical-batch limit is a transport boundary, not a score.

llama-server processes an embedding request in one physical batch and
rejects any input longer than `-ub` with HTTP 500:

    input (2055 tokens) is too large to process. increase the physical
    batch size (current batch size: 2048)

Every Lens score (whole-text C(x)/G(x) and per-step) is computed from one
forward over the whole sequence, so an input past that limit cannot be
scored on the calibrated convention. The service reports that as a typed
failure carrying the two numbers the server gave, and never a number that
reads like a verdict (energy 0.0, gx 0.5, an empty per-step aggregate).

The stub server below tokenizes deterministically (whitespace pieces plus
one BOS token), so the boundary is judged in tokens, never in characters
or bytes, and enforces exactly llama-server's rule: reject when tokens
exceed the capacity, accept at the capacity.
"""
import json
import math
import os
import socket
import sys
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.join(HERE, "..")
sys.path.insert(0, ROOT)

from geometric_lens import embed_capacity as ec  # noqa: E402
from geometric_lens import embedding_extractor as ee  # noqa: E402
from geometric_lens import service  # noqa: E402

CAPACITY = 2048
DIM = 4

STATE = {"capacity": CAPACITY, "plain_500": False}
LOCK = threading.Lock()
REQUESTS = []


def stub_tokens(text: str) -> int:
    """One token per whitespace piece plus BOS: the count llama-server reports."""
    return len(text.split()) + 1


class _LlamaStub(BaseHTTPRequestHandler):
    def log_message(self, *a):
        pass

    def _reply(self, status, obj):
        body = json.dumps(obj).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        if self.path == "/health":
            self._reply(200, {"status": "ok"})
            return
        self._reply(200, {"object": "list", "data": [{"id": "stub-model"}]})

    def do_POST(self):
        n = int(self.headers.get("Content-Length", 0))
        text = json.loads(self.rfile.read(n)).get("content", "")
        tokens = stub_tokens(text)
        with LOCK:
            REQUESTS.append(tokens)
            plain = STATE["plain_500"]
            capacity = STATE["capacity"]
        if plain:
            self._reply(500, {"error": {"code": 500,
                                        "message": "failed to process",
                                        "type": "server_error"}})
            return
        if tokens > capacity:
            self._reply(500, {"error": {
                "code": 500,
                "message": (f"input ({tokens} tokens) is too large to process. "
                            f"increase the physical batch size "
                            f"(current batch size: {capacity})"),
                "type": "server_error"}})
            return
        rows = [[0.5 + 0.01 * ((i + j) % 5) for j in range(DIM)]
                for i in range(tokens)]
        self._reply(200, [{"index": 0, "embedding": rows}])


@pytest.fixture(scope="module")
def llama():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    httpd = ThreadingHTTPServer(("127.0.0.1", port), _LlamaStub)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{port}"
    httpd.shutdown()


class _TinyCostField:
    """A deterministic C(x): the mean of the vector's coordinates."""

    def __call__(self, x):
        return x.mean(dim=-1, keepdim=True)

    def parameters(self):
        import torch
        return iter([torch.zeros(1, DIM)])


@pytest.fixture(autouse=True)
def _lens(monkeypatch, llama):
    monkeypatch.setenv("LLAMA_URL", llama)
    monkeypatch.setenv("LLAMA_EMBED_URL", llama)
    monkeypatch.delenv("LLAMA_EMBED_CAPACITY_TOKENS", raising=False)
    monkeypatch.setattr(service, "_ensure_models_loaded", lambda: True)
    cx_cfg = {"midpoint": 0.5, "steepness": 4.0}
    monkeypatch.setattr(
        service, "_snapshot_weights",
        lambda: (_TinyCostField(), None, None, None, None, cx_cfg, None))
    ee.set_embedding_contract(None)
    ec.reset()
    with LOCK:
        STATE["capacity"] = CAPACITY
        STATE["plain_500"] = False
        REQUESTS.clear()
    yield
    ec.reset()


def text_of(n_tokens: int, piece: str = "x") -> str:
    """A text the stub tokenizes to exactly `n_tokens` (pieces + BOS)."""
    assert n_tokens >= 1
    return " ".join([piece] * (n_tokens - 1))


def _assert_scored(result, n_tokens):
    assert result["scored"] is True
    assert result.get("failure") is None
    assert result["n_tokens"] == n_tokens


def _assert_capacity_failure(result, input_tokens, capacity=CAPACITY):
    assert result["enabled"] is True
    assert result["scored"] is False
    failure = result["failure"]
    assert failure["kind"] == "embed_capacity"
    assert failure["input_tokens"] == input_tokens
    assert failure["capacity_tokens"] == capacity
    # No number that could be read as a verdict.
    assert result["per_step"] == [] if "per_step" in result else True
    assert result.get("aggregate", {}) == {}
    for key in ("cx_energy", "cx_normalized", "gx_score"):
        assert result.get(key) is None, key
    assert result.get("cx_calibrated") is False
    assert result.get("verdict", "unscored") == "unscored"


# --- the boundary, per-step path ----------------------------------------------------

def test_below_the_boundary_scores_every_token():
    out = service.evaluate_per_step(text_of(CAPACITY - 1))
    _assert_scored(out, CAPACITY - 1)
    assert len(out["per_step"]) == CAPACITY - 1
    assert math.isfinite(out["aggregate"]["cx_energy_mean"])


def test_exactly_the_boundary_is_accepted():
    """llama-server rejects `tokens > n_ubatch`; equality is processed."""
    out = service.evaluate_per_step(text_of(CAPACITY))
    _assert_scored(out, CAPACITY)


def test_one_past_the_boundary_is_a_typed_transport_failure():
    out = service.evaluate_per_step(text_of(CAPACITY + 1))
    _assert_capacity_failure(out, CAPACITY + 1)


def test_the_r4_shape_2055_tokens_against_2048():
    out = service.evaluate_per_step(text_of(2055))
    _assert_capacity_failure(out, 2055, 2048)
    assert out["n_tokens"] == 0          # tokens scored, not tokens sent


def test_substantially_longer_input_reports_its_real_size():
    out = service.evaluate_per_step(text_of(12288))
    _assert_capacity_failure(out, 12288)


def test_unicode_boundary_is_judged_in_tokens_not_characters():
    piece = "λ→🐍"                       # 3 characters, 9 bytes, 1 token
    accepted = text_of(CAPACITY, piece)
    out = service.evaluate_per_step(accepted)
    _assert_scored(out, CAPACITY)
    assert len(accepted) > CAPACITY      # more characters than tokens

    rejected = text_of(CAPACITY + 1, piece)
    out = service.evaluate_per_step(rejected)
    _assert_capacity_failure(out, CAPACITY + 1)
    assert out["failure"]["input_tokens"] < len(rejected)
    assert out["failure"]["input_tokens"] < len(rejected.encode("utf-8"))


# --- the boundary, whole-text path --------------------------------------------------

def test_combined_scores_below_and_at_the_boundary():
    for n in (12, CAPACITY):
        out = service.evaluate_combined(text_of(n))
        assert out["scored"] is True
        assert math.isfinite(out["cx_energy"])
        assert out["cx_calibrated"] is True


def test_combined_past_the_boundary_carries_no_energy():
    out = service.evaluate_combined(text_of(2055))
    _assert_capacity_failure(out, 2055)
    assert out["gx_available"] is False
    assert "latency_ms" not in out or out["latency_ms"] >= 0


# --- several candidates: identity and independence ---------------------------------

def test_one_long_candidate_among_several_fails_alone():
    short, long_, edge = text_of(10), text_of(2055), text_of(CAPACITY)
    results = [service.evaluate_per_step(t) for t in (short, long_, edge)]
    _assert_scored(results[0], 10)
    _assert_capacity_failure(results[1], 2055)
    _assert_scored(results[2], CAPACITY)
    # The failure names the input that failed, and only it was rejected.
    assert results[1]["failure"]["input_tokens"] == stub_tokens(long_)
    assert ec.snapshot()["embed_capacity_rejections"] == 1


# --- other upstream failures stay distinct ------------------------------------------

def test_plain_upstream_500_is_not_a_capacity_finding():
    with LOCK:
        STATE["plain_500"] = True
    out = service.evaluate_per_step(text_of(20))
    assert out["scored"] is False
    assert out["failure"]["kind"] == "model_server_error"
    assert out["failure"]["status"] == 500
    assert "input_tokens" not in out["failure"]
    assert out.get("aggregate", {}) == {}
    assert ec.snapshot()["embed_capacity_rejections"] == 0
    assert ec.snapshot()["embed_capacity_tokens"] is None

    out = service.evaluate_combined(text_of(20))
    assert out["scored"] is False
    assert out["failure"]["kind"] == "model_server_error"
    assert out["cx_energy"] is None and out["gx_score"] is None


def test_unreachable_model_server_is_typed(monkeypatch):
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    monkeypatch.setenv("LLAMA_EMBED_URL", f"http://127.0.0.1:{port}")
    out = service.evaluate_combined(text_of(20))
    assert out["scored"] is False
    assert out["failure"]["kind"] == "model_server_unreachable"
    assert out["cx_energy"] is None


# --- the capacity contract the service can report -----------------------------------

def test_capacity_is_unknown_until_declared_or_observed():
    snap = ec.snapshot()
    assert snap["embed_capacity_tokens"] is None
    assert snap["embed_capacity_source"] is None
    assert snap["embed_capacity_rejections"] == 0


def test_declared_capacity_comes_from_the_environment(monkeypatch):
    monkeypatch.setenv("LLAMA_EMBED_CAPACITY_TOKENS", "1024")
    snap = ec.snapshot()
    assert snap["embed_capacity_tokens"] == 1024
    assert snap["embed_capacity_source"] == "declared"


def test_observed_capacity_overrides_a_declaration(monkeypatch):
    monkeypatch.setenv("LLAMA_EMBED_CAPACITY_TOKENS", "4096")   # wrong on purpose
    service.evaluate_per_step(text_of(2055))
    snap = ec.snapshot()
    assert snap["embed_capacity_tokens"] == 2048
    assert snap["embed_capacity_source"] == "observed"
    assert snap["embed_capacity_rejections"] == 1
    assert snap["embed_capacity_max_rejected_tokens"] == 2055


def test_malformed_declaration_is_ignored(monkeypatch):
    for bad in ("", "0", "-5", "many", "12.5"):
        monkeypatch.setenv("LLAMA_EMBED_CAPACITY_TOKENS", bad)
        assert ec.snapshot()["embed_capacity_tokens"] is None


def test_capacity_parser_reads_llama_servers_message():
    msg = ("input (2055 tokens) is too large to process. increase the "
           "physical batch size (current batch size: 2048)")
    assert ec.parse_capacity_rejection(msg) == (2055, 2048)
    # The older wording carries no numbers but is the same rejection.
    assert ec.parse_capacity_rejection(
        "input is too large to process. increase the physical batch size"
    ) == (None, None)
    assert ec.parse_capacity_rejection("failed to process") is None


# --- the HTTP surface ------------------------------------------------------------------

@pytest.fixture(scope="module")
def app_client(llama, tmp_path_factory):
    tmp = tmp_path_factory.mktemp("lens-capacity")
    os.environ["ATLAS_SERVICE_TOKEN_FILE"] = str(tmp / "no-token")
    os.environ["LLAMA_URL"] = llama
    os.environ["LLAMA_EMBED_URL"] = llama
    from fastapi.testclient import TestClient
    for name in ("main", "config"):
        mod = sys.modules.get(name)
        if mod is not None and not str(getattr(mod, "__file__", "")).startswith(
                os.path.abspath(ROOT)):
            del sys.modules[name]
    sys.path.insert(0, os.path.abspath(ROOT))
    cwd = os.getcwd()
    os.chdir(ROOT)
    try:
        import importlib
        main = importlib.import_module("main")
    finally:
        os.chdir(cwd)
    assert hasattr(main, "app")
    with TestClient(main.app) as client:
        yield client, main


def test_health_reports_the_capacity_contract(app_client, monkeypatch):
    client, _ = app_client
    lens = client.get("/health").json()["subsystems"]["lens"]
    assert lens["embed_capacity_tokens"] is None
    assert lens["embed_capacity_source"] is None
    assert lens["embed_capacity_rejections"] == 0

    monkeypatch.setenv("LLAMA_EMBED_CAPACITY_TOKENS", "2048")
    lens = client.get("/health").json()["subsystems"]["lens"]
    assert lens["embed_capacity_tokens"] == 2048
    assert lens["embed_capacity_source"] == "declared"


def test_ready_reports_capacity_without_changing_its_gate(app_client, monkeypatch):
    client, main = app_client
    # A lens whose self-test passed: the gate is then open, and a capacity
    # observation must not close it.
    monkeypatch.setitem(main._BOOT_STATE, "self_test_pass", True)
    monkeypatch.setitem(main._BOOT_STATE, "self_test_error", None)
    monkeypatch.setenv("LLAMA_EMBED_CAPACITY_TOKENS", "2048")
    before = client.get("/ready")
    assert before.status_code == 200
    assert before.json()["ready"] is True
    assert before.json()["embed_capacity_tokens"] == 2048
    # A capacity observation is information, never a readiness verdict.
    ec.observe_rejection(2055, 2048)
    after = client.get("/ready")
    assert after.status_code == 200
    assert after.json()["embed_capacity_tokens"] == 2048


def test_endpoint_failures_carry_no_numbers(app_client, monkeypatch):
    client, main = app_client

    def boom(*a, **kw):
        raise RuntimeError("scorer exploded")
    monkeypatch.setattr(service, "evaluate_per_step", boom)
    monkeypatch.setattr(service, "evaluate_combined", boom)

    per_step = client.post("/internal/lens/score-per-step",
                           json={"text": "def f(): pass"}).json()
    assert per_step["scored"] is False
    assert per_step["failure"]["kind"] == "internal"
    assert per_step["aggregate"] == {} and per_step["per_step"] == []
    assert per_step["n_tokens"] == 0

    combined = client.post("/internal/lens/gx-score",
                           json={"text": "def f(): pass"}).json()
    assert combined["scored"] is False
    assert combined["failure"]["kind"] == "internal"
    for key in ("cx_energy", "cx_normalized", "gx_score"):
        assert combined[key] is None
    assert combined["verdict"] == "unscored"
    assert combined["cx_calibrated"] is False


def test_endpoint_passes_the_typed_capacity_failure_through(app_client, monkeypatch):
    client, main = app_client
    out = client.post("/internal/lens/score-per-step",
                      json={"text": text_of(2055)}).json()
    _assert_capacity_failure(out, 2055)
    lens = client.get("/health").json()["subsystems"]["lens"]
    assert lens["embed_capacity_tokens"] == 2048
    assert lens["embed_capacity_source"] == "observed"


# --- a non-finite number never leaves the service as a score ---------------------------
#
# json.dumps emits NaN / Infinity as tokens standard JSON does not have. A
# degenerate forward is reported as the typed unscored shape instead, on
# every scoring endpoint (the service-level rule is pinned in
# test_nonfinite_scores.py).

class _NonFiniteField(_TinyCostField):
    def __init__(self, value):
        self.value = value

    def __call__(self, x):
        return x.mean(dim=-1, keepdim=True) * 0.0 + self.value


def _standard_json(response):
    assert response.status_code == 200
    assert "NaN" not in response.text and "Infinity" not in response.text
    return json.loads(response.text)


@pytest.mark.parametrize("value", [float("nan"), float("inf"), float("-inf")])
def test_score_text_endpoint_reports_a_nonfinite_energy_unscored(app_client, monkeypatch, value):
    client, _ = app_client
    monkeypatch.setattr(service, "_cost_field", _NonFiniteField(value))
    out = _standard_json(client.post("/internal/lens/score-text",
                                     json={"text": text_of(8)}))
    assert out["scored"] is False and out["calibrated"] is False
    assert out["energy"] is None and out["normalized"] is None
    assert out["failure"]["kind"] == ec.KIND_NONFINITE


def test_gx_score_endpoint_reports_a_nonfinite_energy_unscored(app_client, monkeypatch):
    client, _ = app_client
    monkeypatch.setattr(
        service, "_snapshot_weights",
        lambda: (_NonFiniteField(float("nan")), None, None, None, None,
                 {"midpoint": 0.5, "steepness": 4.0}, None))
    out = _standard_json(client.post("/internal/lens/gx-score",
                                     json={"text": text_of(8)}))
    assert out["scored"] is False and out["verdict"] == "unscored"
    for key in ("cx_energy", "cx_normalized", "gx_score"):
        assert out[key] is None, key
    assert out["failure"] == {"kind": ec.KIND_NONFINITE, "field": "cx_energy",
                              "detail": "nan"}


def test_per_step_endpoint_reports_a_nonfinite_energy_unscored(app_client, monkeypatch):
    client, _ = app_client
    monkeypatch.setattr(
        service, "_snapshot_weights",
        lambda: (_NonFiniteField(float("inf")), None, None, None, None,
                 {"midpoint": 0.5, "steepness": 4.0}, None))
    out = _standard_json(client.post("/internal/lens/score-per-step",
                                     json={"text": text_of(8)}))
    assert out["scored"] is False and out["n_tokens"] == 0
    assert out["per_step"] == [] and out["aggregate"] == {}
    assert out["failure"]["kind"] == ec.KIND_NONFINITE
    assert out["failure"]["field"] == "cx_energy"


def test_drift_withdraws_the_per_step_thresholds(app_client, monkeypatch):
    """Per-step scores carry the thresholds the proxy's corrective and V3's
    veto act on. On drift they are withdrawn here as on the other scoring
    endpoints; per-step used to skip the drift flags entirely."""
    client, main = app_client
    scored = {"enabled": True, "scored": True, "gx_available": True, "n_tokens": 3,
              "cx_calibrated": True, "per_step": [], "aggregate": {"gx_score_min": 0.7},
              "thresholds": {"off_rails": 0.3, "low": 0.4, "severe": 0.2}}
    monkeypatch.setattr(service, "evaluate_per_step", lambda text, layer=None: dict(scored))
    monkeypatch.setitem(main._BOOT_STATE, "fingerprint_ok", True)
    fresh = client.post("/internal/lens/score-per-step", json={"text": "x = 1"}).json()
    assert fresh["drifted"] is False and fresh["thresholds"] is not None
    monkeypatch.setitem(main._BOOT_STATE, "fingerprint_ok", False)
    drifted = client.post("/internal/lens/score-per-step", json={"text": "x = 1"}).json()
    assert drifted["drifted"] is True
    assert drifted["thresholds"] is None
    assert drifted["cx_calibrated"] is False
