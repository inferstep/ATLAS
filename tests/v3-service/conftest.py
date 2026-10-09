"""Shared fixtures for the v3-service tests.

The service modules read their upstream URLs from module attributes that
default to the published service ports (inference 8080, lens 8099, sandbox
30820). On a host running the stack, a test that forgets to stub one of those
calls talks to the live services. The deploy gate runs this suite on such a
host. This backstop points every upstream at an unroutable address first; a
test that needs a specific URL sets its own, which wins because it runs later.
"""
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

import adapters

_UNROUTABLE = "http://127.0.0.1:9"


@pytest.fixture(autouse=True)
def _no_live_services(monkeypatch):
    for name in ("INFERENCE_URL", "LENS_URL", "SANDBOX_URL"):
        monkeypatch.setattr(adapters, name, _UNROUTABLE)
