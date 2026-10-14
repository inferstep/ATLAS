"""A service token file that is there but cannot be read stops the service, with words that name the cause. A file
that is not there is no token, as before: the check is off, and `atlas doctor` warns."""
import importlib.util
import os
import sys
from pathlib import Path

import pytest

import adapters
from stages import llm_client

SANDBOX = Path(__file__).resolve().parents[2] / "sandbox"


def sandbox_server():
    """The sandbox's server module, loaded from its own file."""
    if str(SANDBOX) not in sys.path:
        sys.path.insert(0, str(SANDBOX))
    spec = importlib.util.spec_from_file_location("atlas_sandbox_executor_token", SANDBOX / "executor_server.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


@pytest.mark.parametrize("loader, who", [
    pytest.param(lambda: adapters._load_service_token, "the V3 service", id="the V3 service"),
    pytest.param(lambda: llm_client._service_token, "this", id="the V3 model client"),
    pytest.param(lambda: sandbox_server()._load_service_token, "the sandbox", id="the sandbox"),
])
def test_a_token_file_that_cannot_be_read_stops_it_and_one_that_is_not_there_is_no_token(tmp_path, monkeypatch,
                                                                                          loader, who):
    load = loader()
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(tmp_path / "not-there"))
    assert load() == ""

    if os.geteuid() == 0:
        pytest.skip("root reads a file of mode 000, so the case of a file that cannot be read needs a user that is not root")
    unreadable = tmp_path / "service-token"
    unreadable.write_text("a-stand-in-token\n")
    unreadable.chmod(0)
    monkeypatch.setenv("ATLAS_SERVICE_TOKEN_FILE", str(unreadable))
    with pytest.raises(SystemExit) as stop:
        load()
    assert f"the service token file {unreadable} cannot be read (Permission denied), so {who} stops" in str(stop.value)
