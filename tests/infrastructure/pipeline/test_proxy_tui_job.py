"""The job `proxy and TUI tests (no model)` of the workflow `tests`: its step that builds the proxy and the TUI."""
import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
# A stand-in for `go`. It writes each call down. `go mod download` fails as many times as the file `breaks` says,
# with words as Go's own when a download breaks off; every other call passes.
GO = """#!/bin/bash
echo "$*" >> "$CALLS"
if [ "$1 $2" = "mod download" ]; then
    left=$(cat "$BREAKS")
    if [ "$left" -gt 0 ]; then
        echo $((left - 1)) > "$BREAKS"
        echo "go: example.org/a/module@v1.0.0: read: unexpected EOF" >&2
        exit 1
    fi
fi
"""


@pytest.mark.parametrize("breaks, ends, calls", [
    pytest.param(1, 0, ["build -o {temp}/atlas-proxy .", "mod download", "mod download", "build -o atlas-tui ."],
                 id="a download that breaks off once is tried again, and the build goes on"),
    pytest.param(2, 1, ["build -o {temp}/atlas-proxy .", "mod download", "mod download"],
                 id="a download that breaks off twice fails the step, and nothing is built after it"),
])
def test_a_download_of_the_tuis_modules_that_breaks_off_is_tried_once_more(tmp_path, breaks, ends, calls):
    workflow = yaml.safe_load((ROOT / ".github" / "workflows" / "test.yml").read_text(encoding="utf-8"))
    step = next(step["run"] for step in workflow["jobs"]["proxy-tui-tests"]["steps"] if step.get("name") == "build the proxy and the TUI")
    for folder in ("checkout/proxy", "checkout/tui", "bin", "temp"):
        (tmp_path / folder).mkdir(parents=True)
    (tmp_path / "bin" / "go").write_text(GO, encoding="utf-8")
    (tmp_path / "bin" / "go").chmod(0o755)
    (tmp_path / "breaks").write_text(f"{breaks}\n", encoding="utf-8")
    # As a runner runs a step: bash, which stops at the first command that fails.
    done = subprocess.run(["bash", "-e", "-c", step], cwd=tmp_path / "checkout", capture_output=True, text=True, timeout=60,
                          env={"PATH": f"{tmp_path / 'bin'}:/usr/bin:/bin", "RUNNER_TEMP": str(tmp_path / "temp"),
                               "CALLS": str(tmp_path / "calls"), "BREAKS": str(tmp_path / "breaks")})
    assert done.returncode == ends, done.stdout + done.stderr
    assert (tmp_path / "calls").read_text(encoding="utf-8").splitlines() == [call.format(temp=tmp_path / "temp") for call in calls]
    assert "The download of the TUI's modules broke off. It is tried once more." in done.stdout
