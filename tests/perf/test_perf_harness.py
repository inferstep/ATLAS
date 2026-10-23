"""Performance harness: schema stability + budget gate."""

import pytest

from tests.perf import harness

BUDGETS = {"deterministic_max": {"cli_import_time_s": 3.0, "proxy_binary_bytes": 60_000_000}}


def test_measure_has_stable_schema():
    r = harness.measure(stamp="2026-07-05T00:00:00", git_commit="abc1234")
    assert r["schema_version"] == harness.SCHEMA_VERSION
    assert "deterministic" in r and "hardware" in r
    # hardware fields present but nullable (imported later)
    assert set(r["hardware"]) >= {"model", "first_token_ms", "tokens_per_sec"}


# violation: None when the check must pass; otherwise a word that one of its violations must contain.
@pytest.mark.parametrize("result, violation", [
    pytest.param({"deterministic": {"cli_import_time_s": 1.0, "proxy_binary_bytes": 1000}}, None,
                 id="within-budget-passes"),
    # One metric could not be measured (None) but another matched: not a regression.
    pytest.param({"deterministic": {"cli_import_time_s": 1.0, "proxy_binary_bytes": None}}, None,
                 id="one-missing-metric-passes"),
    pytest.param({"deterministic": {"cli_import_time_s": 9.9}}, "cli_import_time_s",
                 id="over-budget-fails"),
    # A result that matches no budgeted metric (renamed keys, empty file) must fail:
    # a pass would disarm the gate silently.
    pytest.param({}, "no budgeted metric",
                 id="no-matching-metric-fails"),
    pytest.param({"schema_version": harness.SCHEMA_VERSION + 1, "deterministic": {"cli_import_time_s": 1.0}},
                 "schema_version", id="other-schema-version-fails"),
])
def test_check_against_budgets(result, violation):
    verdict = harness.check(result, BUDGETS)
    if violation is None:
        assert verdict == {"passed": True, "violations": []}
    else:
        assert not verdict["passed"]
        assert any(violation in v for v in verdict["violations"]), verdict["violations"]


def test_real_budgets_file_loads():
    assert "deterministic_max" in harness.load_budgets()
