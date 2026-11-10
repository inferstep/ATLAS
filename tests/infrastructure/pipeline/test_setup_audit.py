"""The audit accepts each required document at its current or released path."""

import importlib.util
import sys
from pathlib import Path

import pytest


CURRENT_INCIDENT = "docs/project/INCIDENT_RESPONSE.md"
RELEASED_INCIDENT = "docs/INCIDENT_RESPONSE.md"
CURRENT_SECURITY = ".github/SECURITY.md"
RELEASED_SECURITY = "SECURITY.md"
DOCUMENT_PATHS = (CURRENT_INCIDENT, RELEASED_INCIDENT, CURRENT_SECURITY, RELEASED_SECURITY)
MISSING_INCIDENT = (
    "Missing incident response plan "
    "(checked docs/project/INCIDENT_RESPONSE.md, docs/INCIDENT_RESPONSE.md)"
)
MISSING_SECURITY = "Missing security policy (checked .github/SECURITY.md, SECURITY.md)"


@pytest.mark.parametrize("default_branch", ["main", "release"])
@pytest.mark.parametrize(
    "statuses, expected_result, expected_evidence, expected_paths",
    [
        pytest.param(
            (200, 404, 200, 404), "PASS", f"{CURRENT_INCIDENT} + {CURRENT_SECURITY}",
            (CURRENT_INCIDENT, CURRENT_SECURITY), id="current",
        ),
        pytest.param(
            (404, 200, 404, 200), "PASS", f"{RELEASED_INCIDENT} + {RELEASED_SECURITY}",
            DOCUMENT_PATHS, id="released",
        ),
        pytest.param(
            (200, 404, 404, 200), "PASS", f"{CURRENT_INCIDENT} + {RELEASED_SECURITY}",
            (CURRENT_INCIDENT, CURRENT_SECURITY, RELEASED_SECURITY), id="incident-moved-first",
        ),
        pytest.param(
            (404, 200, 200, 404), "PASS", f"{RELEASED_INCIDENT} + {CURRENT_SECURITY}",
            (CURRENT_INCIDENT, RELEASED_INCIDENT, CURRENT_SECURITY), id="security-moved-first",
        ),
        pytest.param(
            (200, 200, 200, 200), "PASS", f"{CURRENT_INCIDENT} + {CURRENT_SECURITY}",
            (CURRENT_INCIDENT, CURRENT_SECURITY), id="prefer-current",
        ),
        pytest.param(
            (404, 404, 200, 404), "FAIL", f"{MISSING_INCIDENT} + {CURRENT_SECURITY}",
            (CURRENT_INCIDENT, RELEASED_INCIDENT, CURRENT_SECURITY), id="missing-incident",
        ),
        pytest.param(
            (200, 404, 404, 404), "FAIL", f"{CURRENT_INCIDENT} + {MISSING_SECURITY}",
            (CURRENT_INCIDENT, CURRENT_SECURITY, RELEASED_SECURITY), id="missing-security",
        ),
        pytest.param(
            (404, 404, 404, 404), "FAIL", f"{MISSING_INCIDENT} + {MISSING_SECURITY}",
            DOCUMENT_PATHS, id="missing-both",
        ),
        pytest.param(
            (403, 200, 403, 200), "PASS", f"{RELEASED_INCIDENT} + {RELEASED_SECURITY}",
            DOCUMENT_PATHS, id="denied-current-with-released-present",
        ),
        pytest.param(
            (403, 403, 200, 404), "FAIL", f"{MISSING_INCIDENT} + {CURRENT_SECURITY}",
            (CURRENT_INCIDENT, RELEASED_INCIDENT, CURRENT_SECURITY), id="denied-incident",
        ),
        pytest.param(
            (200, 404, 403, 403), "FAIL", f"{CURRENT_INCIDENT} + {MISSING_SECURITY}",
            (CURRENT_INCIDENT, CURRENT_SECURITY, RELEASED_SECURITY), id="denied-security",
        ),
    ],
)
def test_incident_response_requires_both_documents_on_the_default_branch(
    monkeypatch, default_branch, statuses, expected_result, expected_evidence, expected_paths,
):
    script = Path(__file__).resolve().parents[3] / "scripts" / "setup" / "audit.py"
    spec = importlib.util.spec_from_file_location("setup_audit", script)
    audit = importlib.util.module_from_spec(spec)
    monkeypatch.setitem(sys.modules, spec.name, audit)
    spec.loader.exec_module(audit)
    reader = audit.Audit.__new__(audit.Audit)
    reader.org = "example-org"
    reader.full = "example-org/example-repo"
    reader.default = default_branch
    reader.rows = []
    reader.repo_info = {}
    reader.workflows = {}
    responses = {
        f"repos/{reader.full}/contents/{path}?ref={default_branch}": (status, {})
        for path, status in zip(DOCUMENT_PATHS, statuses)
    }
    responses[f"orgs/{reader.org}/members?per_page=100"] = (200, [])
    responses[f"orgs/{reader.org}/teams"] = (200, [])
    requests = []

    def fake_gh(path):
        requests.append(path)
        return responses[path]

    monkeypatch.setattr(audit, "gh", fake_gh)

    reader._ops_rows(alerts=204)

    (row,) = [row for row in reader.rows if row[1] == "Incident response plan"]
    assert row == (
        "Ops", "Incident response plan", expected_result,
        f"{expected_evidence} on {default_branch}",
    )
    assert [path for path in requests if "/contents/" in path] == [
        f"repos/{reader.full}/contents/{path}?ref={default_branch}" for path in expected_paths
    ]
