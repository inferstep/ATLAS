"""API-version + error-taxonomy contract.

The proxy's error-code set (Go) must match the documented/schema'd
closed set, and the version constants must be present. Prevents a Go
rename from silently desyncing the machine-readable schema clients rely
on. Parsing only.
"""

import json
import re
from pathlib import Path

from tests.contracts import go_source

REPO = Path(__file__).resolve().parents[2]

CANONICAL_CODES = [
    "unauthorized",
    "invalid_input",
    "unsupported_operation",
    "dependency_unavailable",
    "resource_limit",
    "internal_error",
]


def test_go_error_codes_match_canonical_set():
    src = go_source("proxy", 'ErrorCode = "')
    codes = re.findall(r'ErrorCode = "([a-z_]+)"', src)
    assert codes == CANONICAL_CODES, (
        f"Go error codes {codes} != canonical {CANONICAL_CODES}")


def test_error_schema_enum_matches():
    schema = json.loads(
        (REPO / "docs" / "schemas" / "error_envelope.schema.json").read_text())
    enum = schema["properties"]["error"]["enum"]
    assert enum == CANONICAL_CODES, (
        "error_envelope schema enum drifted from the canonical set")


def test_version_constants_present():
    src = go_source("proxy", "const APIVersion")
    assert re.search(r'const APIVersion = "\d+\.\d+\.\d+"', src)
    assert re.search(r'const ProtocolVersion = \d+', src)


def test_version_endpoint_registered():
    src = (REPO / "proxy" / "main.go").read_text()
    assert '"/version"' in src and "handleVersion" in src


def test_sse_envelope_schema_present():
    schema = json.loads(
        (REPO / "docs" / "schemas" / "sse_envelope.schema.json").read_text())
    assert "type" in schema["required"]
    assert "data" in schema["required"]


# --- The structured task contract, sent by every owned client ----------------
#
# The proxy still decides everything from the user's English. The contract is
# the client stating, in a typed field, what only the client knows. Absence is
# reserved for external and legacy callers: an owned sender that omits it is
# indistinguishable from a stranger's request, so every one of ours declares a
# mode even when it has nothing else to declare.

import ast
import pathlib
import subprocess

REPO = pathlib.Path(__file__).resolve().parents[2]


def _tracked_python_files():
    """Files git tracks. Repository-owned means committed, so gitignored trees
    -- the red-team harnesses and their frozen archives -- are out of scope by
    construction rather than by a hand-maintained skip list."""
    out = subprocess.run(["git", "ls-files", "*.py"], cwd=REPO,
                         capture_output=True, text=True, check=True)
    return [REPO / line for line in out.stdout.splitlines() if line]


def _owned_python_senders():
    """Every repository-owned Python module that POSTs an agent request."""
    hits = []
    for path in _tracked_python_files():
        if "__pycache__" in str(path):
            continue
        text = path.read_text(errors="replace")
        if "/v1/agent" not in text:
            continue
        tree = ast.parse(text, filename=str(path))
        for node in ast.walk(tree):
            # A POST to the agent endpoint, however it is spelled.
            if not isinstance(node, ast.Call):
                continue
            src = ast.get_source_segment(text, node) or ""
            if "/v1/agent" in src and ("request" in src or "Request" in src):
                hits.append((str(path.relative_to(REPO)), node.lineno, src[:90]))
    return hits


def test_owned_python_senders_are_inventoried():
    """A new owned sender cannot appear without being classified here."""
    senders = _owned_python_senders()
    files = sorted({f for f, _, _ in senders})
    # Classification, by file. Anything not listed fails.
    declares_contract = {"tests/e2e/conftest.py", "scripts/e2e-reliability.py",
                         # Sends through agent_request_body, and reads the 503
                         # a request gets while the lens cannot score, which
                         # the streaming helper cannot return.
                         "tests/e2e/test_v3_lens_acceptance.py",
                         # Sends the request a recording holds, and work when
                         # a recording holds no mode.
                         "tests/replay/stage.py"}
    # Auth probes: deliberately malformed bodies rejected at 401 before the
    # handler ever decodes them. Adding a contract would change what they test.
    auth_probes = {"tests/e2e/test_service_auth.py"}
    # Sends each held-out task's own mode, which the suite loader defaults to
    # work and limits to work or question (scripts/eval/suite.py).
    declares_task_mode = {"scripts/eval/atlas_arm.py"}
    known = declares_contract | auth_probes | declares_task_mode
    unknown = [f for f in files if f not in known]
    assert not unknown, (
        f"unclassified owned agent senders: {unknown}. Every owned sender must "
        f"send task_contract.task_mode, or be registered here with its reason."
    )
    for f in sorted(declares_contract):
        text = (REPO / f).read_text()
        assert "task_contract" in text, f"{f} posts agent requests without a task contract"
        assert '"task_mode": "work"' in text or "'task_mode': 'work'" in text, (
            f"{f} does not default to work"
        )
    for f in sorted(declares_task_mode):
        text = (REPO / f).read_text()
        assert '"task_contract": {"task_mode": task.mode}' in text, (
            f"{f} does not send the task's mode"
        )


def test_e2e_helper_defaults_to_work():
    """drive_agent_turn is the e2e suite's only sender; it declares work."""
    spec = REPO / "tests/e2e/conftest.py"
    tree = ast.parse(spec.read_text(), filename=str(spec))
    fn = next(n for n in ast.walk(tree)
              if isinstance(n, ast.FunctionDef) and n.name == "agent_request_body")
    src = ast.get_source_segment(spec.read_text(), fn)
    assert "task_contract" in src and "work" in src
    # It must not invent obligations the caller never stated. The JSON keys,
    # not the words: the docstring explains why they are absent.
    assert '"expected_outputs"' not in src, "the e2e helper fabricates expected outputs"
    assert '"verification"' not in src, "the e2e helper fabricates verification"


def test_benchmark_harness_declares_work_without_evaluator_leakage():
    """The benchmark sends work, and never turns its scorer into an obligation."""
    text = (REPO / "scripts/e2e-reliability.py").read_text()
    assert '"task_contract"' in text, "the benchmark harness sends no task contract"
    assert '"task_mode": "work"' in text
    # The offline evaluator and holdout are scoring machinery, not agent-facing
    # requirements, and must not appear as contract obligations.
    start = text.index('"task_contract"')
    window = text[start:start + 400]
    for leak in ("expected_outputs", "verification", "holdout", "evaluator"):
        assert leak not in window, f"the benchmark contract leaks {leak}"
