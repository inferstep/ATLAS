"""doctor must notice a llama-server that generates but cannot feed the lens.

GH #146: after pointing ATLAS at a host-run llama-server, the reporter hit
"issues with embeddings and the PC-202 hidden-states patch". Nothing in
`atlas doctor` looked at either, so the run reported everything passing and
the problem surfaced later as lens scores that were plausible and meaningless.
"""
import pytest

from atlas.commands import doctor


class _Probe:
    reachable = True
    url = "http://byo:8080"
    error = ""
    n_layers = 0
    model_name = "byo"

    def __init__(self, dim=0, patch=False, reachable=True, error=""):
        self.embedding_dim = dim
        self.has_hidden_states_patch = patch
        self.reachable = reachable
        self.error = error


@pytest.fixture
def probe(monkeypatch):
    import atlas.client as client
    holder = {}

    def _set(p):
        monkeypatch.setattr(client, "probe_llama", lambda *a, **k: p)
        holder["p"] = p

    return _set


def test_a_server_with_both_capabilities_passes(probe):
    probe(_Probe(dim=3840, patch=True))
    r = doctor.check_inference_contract()
    assert r.status == "pass"
    assert "3840" in r.message


def test_missing_embeddings_warns_and_names_the_flag(probe):
    probe(_Probe(dim=0, patch=True))
    r = doctor.check_inference_contract()
    assert r.status == "warn"
    assert "--embeddings" in r.message


def test_missing_hidden_states_warns_and_names_pc202(probe):
    probe(_Probe(dim=3840, patch=False))
    r = doctor.check_inference_contract()
    assert r.status == "warn"
    assert "PC-202" in r.message


def test_it_warns_rather_than_fails(probe):
    """ATLAS runs without the lens, degraded to sandbox-only verification.
    A hard failure would tell a working install it is broken."""
    probe(_Probe(dim=0, patch=False))
    assert doctor.check_inference_contract().status == "warn"


def test_an_unreachable_server_is_skipped_not_blamed(probe):
    """The health checks already own 'is it up'. Reporting a contract failure
    for a server that is simply down sends the user after the wrong thing."""
    probe(_Probe(reachable=False, error="connection refused"))
    r = doctor.check_inference_contract()
    assert r.status == "skip"
    assert "unreachable" in r.message
