"""The lens service's check of the service token. With a token configured, a request to /internal/ with no token or
another token is refused, and a request with the token reaches its route. A route outside /internal/ stays open, as
the stack's probes need."""
import importlib
import os
import sys

import pytest
from fastapi.testclient import TestClient

ROOT = os.path.abspath(os.path.join(os.path.dirname(os.path.abspath(__file__)), ".."))
TOKEN = "a-stand-in-token"


@pytest.fixture(scope="module")
def lens(tmp_path_factory):
    """The lens service's module, from its own folder, with a route of the test inside /internal/ and one outside."""
    os.environ["ATLAS_SERVICE_TOKEN_FILE"] = str(tmp_path_factory.mktemp("lens") / "no-token")
    # Another service's modules of the same names may be loaded already, when several test trees share one session.
    for name in ("main", "config"):
        loaded = sys.modules.get(name)
        if loaded is not None and not str(getattr(loaded, "__file__", "")).startswith(ROOT):
            del sys.modules[name]
    sys.path.insert(0, ROOT)
    cwd = os.getcwd()
    os.chdir(ROOT)
    try:
        main = importlib.import_module("main")
    finally:
        os.chdir(cwd)

    @main.app.get("/internal/_service_token_probe_test")
    def inside():
        return {"reached": "inside"}

    # The internal routes of the lens are POSTs: a check that let a POST through would leave them open.
    @main.app.post("/internal/_service_token_probe_test")
    def inside_post():
        return {"reached": "inside"}

    @main.app.get("/_service_token_probe_test")
    def outside():
        return {"reached": "outside"}

    return main


@pytest.mark.parametrize("method, path, header, status, says", [
    pytest.param("GET", "/internal/_service_token_probe_test", None, 401, "unauthorized", id="no token is refused"),
    pytest.param("GET", "/internal/_service_token_probe_test", "Bearer another-token", 401, "unauthorized",
                 id="another token is refused"),
    pytest.param("GET", "/internal/_service_token_probe_test", f"Bearer {TOKEN}", 200, '"reached":"inside"',
                 id="the token reaches the route"),
    pytest.param("POST", "/internal/_service_token_probe_test", None, 401, "unauthorized", id="a POST with no token is refused"),
    pytest.param("GET", "/_service_token_probe_test", None, 200, '"reached":"outside"', id="a route outside /internal/ stays open"),
])
def test_with_a_token_configured_only_a_request_with_the_token_reaches_an_internal_route(lens, monkeypatch, method, path, header,
                                                                                          status, says):
    monkeypatch.setattr(lens, "_SERVICE_TOKEN", TOKEN)
    # The service is not started: the check stands before every route, and no route here needs a model.
    client = TestClient(lens.app)
    headers = {"Authorization": header} if header else {}
    answer = client.request(method, path, headers=headers, json={"training_data": []} if method == "POST" else None)
    assert (answer.status_code, says in answer.text) == (status, True), answer.text
    # An answer never holds the token.
    assert TOKEN not in answer.text
