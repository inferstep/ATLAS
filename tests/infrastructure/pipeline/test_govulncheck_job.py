"""The govulncheck job judges the Go that the test jobs build with."""

from pathlib import Path

import yaml

WORKFLOWS = Path(__file__).resolve().parents[3] / ".github" / "workflows"


def go_versions(name):
    jobs = yaml.safe_load((WORKFLOWS / name).read_text(encoding="utf-8"))["jobs"]
    return {step["with"]["go-version"] for job in jobs.values() for step in job.get("steps", [])
            if step.get("uses", "").startswith("actions/setup-go@")}


# The standard library's findings depend on the Go version: on an older Go,
# govulncheck reports holes that the test jobs' Go has closed, and on a newer
# one it misses holes that the test jobs' Go still has.
def test_govulncheck_runs_on_the_go_of_the_go_test_jobs():
    assert go_versions("govulncheck.yml") == go_versions("test.yml")
