"""The interactive product path: a TUI work request gets V3's candidate.

    TUI-shaped request  -->  REAL atlas-proxy  -->  REAL v3-service
    (task_mode work, NO expected outputs)

The TUI knows nothing structured about which files a task requires, so it
declares none. What it does have is the model's own structured tool call: a
write_file naming one canonical target. That structured mutation target is
what grounds the delivery of the selected candidate to exactly that path --
and nothing else: no obligation is invented and completion is unchanged.

There is one delivery rule. An older client may still send a
candidate_policy, and an operator may still have ATLAS_CANDIDATE_POLICY set;
the proxy reads neither, so neither changes what lands.

Same fixtures as test_v3_lens_acceptance.py: scripted fake llama, real proxy,
real v3-service, fake lens, real sandbox executor.
"""
import pytest

from tests.e2e.conftest import drive_agent_turn, start_proxy
from tests.e2e.test_v3_lens_acceptance import (  # fixtures, used by name
    CAND_A,
    _agent_body, _assert_no_human_gate_inside_v3, _payload, _write_result,
    fake_lens, fake_llama, proxy, v3_service, workspace,
)

# Exactly what the TUI sends for an ordinary work message: the mode, and
# nothing about files.
TUI_CONTRACT = {"task_mode": "work"}
OLDER_POLICY_SPELLINGS = ["strict", "advisory", "automatic_v3"]


def _assert_the_candidate_landed(events, root):
    result = _write_result(events)
    assert result["data"].get("success") is True, result["data"]
    payload = _payload(result)
    assert payload.get("v3_used") is True, payload
    written = (root / "todo_app.py").read_text()
    assert written == CAND_A, "the selected candidate did not land on the model's own target"
    # One target, the one the tool call named. Nothing else appeared.
    assert sorted(p.name for p in root.iterdir()) == ["todo_app.py"]
    _assert_no_human_gate_inside_v3(events)


def test_a_tui_work_request_delivers_to_the_structured_target(proxy, workspace):  # noqa: F811
    events = drive_agent_turn(
        proxy, _agent_body(workspace, task_contract=TUI_CONTRACT),
        deadline_s=180.0)
    _assert_the_candidate_landed(events, workspace)


@pytest.mark.parametrize("policy", OLDER_POLICY_SPELLINGS)
def test_an_older_clients_policy_changes_nothing(proxy, workspace, policy):  # noqa: F811
    events = drive_agent_turn(
        proxy, _agent_body(workspace, task_contract={**TUI_CONTRACT, "candidate_policy": policy}),
        deadline_s=180.0)
    _assert_the_candidate_landed(events, workspace)


@pytest.fixture()
def proxy_operator_strict(fake_llama, fake_lens, v3_service, sandbox_executor):  # noqa: F811
    port, proc = start_proxy({
        "ATLAS_INFERENCE_URL": f"http://127.0.0.1:{fake_llama}",
        "ATLAS_LENS_URL": f"http://127.0.0.1:{fake_lens}",
        "ATLAS_SANDBOX_URL": f"http://127.0.0.1:{sandbox_executor}",
        "ATLAS_V3_URL": f"http://127.0.0.1:{v3_service}",
        "ATLAS_CANDIDATE_POLICY": "strict",
    })
    yield port
    proc.terminate()
    proc.wait(timeout=10)


def test_an_operator_policy_changes_nothing(proxy_operator_strict, workspace):  # noqa: F811
    events = drive_agent_turn(
        proxy_operator_strict, _agent_body(workspace, task_contract=TUI_CONTRACT),
        deadline_s=180.0)
    _assert_the_candidate_landed(events, workspace)
