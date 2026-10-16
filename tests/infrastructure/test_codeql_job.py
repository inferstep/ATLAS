"""The workflow `codeql`: the step of its `java-kotlin` leg that builds the JetBrains plugin."""
import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
# A stand-in for `./gradlew`. It writes each call down. The build fails as many times as the file `breaks` says, with
# Gradle's words when a download breaks off; else it compiles.
GRADLEW = """#!/bin/bash
echo "$*" >> "$CALLS"
left=$(cat "$BREAKS")
if [ "$left" -gt 0 ]; then
    echo $((left - 1)) > "$BREAKS"
    echo "FAILURE: Build failed with an exception."
    echo "> Could not resolve io.ktor:ktor-client-core:3.6.0."
    exit 1
fi
echo "> Task :compileKotlin"
"""


@pytest.mark.parametrize("breaks, ends, builds, said", [
    pytest.param(0, 0, 1, False, id="a build that passes is not built again"),
    pytest.param(1, 0, 2, True, id="a build that fails once is tried again, and the log is of the second try"),
    pytest.param(2, 1, 2, True, id="a build that fails twice fails the step with Gradle's own words"),
])
def test_a_build_of_the_plugin_that_fails_is_tried_once_more(tmp_path, breaks, ends, builds, said):
    workflow = yaml.safe_load((ROOT / ".github" / "workflows" / "codeql.yml").read_text(encoding="utf-8"))
    step = next(step["run"] for step in workflow["jobs"]["analyze"]["steps"] if step.get("name") == "build kotlin (forced, in-process)")
    (tmp_path / "gradlew").write_text(GRADLEW, encoding="utf-8")
    (tmp_path / "gradlew").chmod(0o755)
    (tmp_path / "breaks").write_text(f"{breaks}\n", encoding="utf-8")
    # As a runner runs a step: bash, which stops at the first command that fails.
    done = subprocess.run(["bash", "-e", "-c", step], cwd=tmp_path, capture_output=True, text=True, timeout=60,
                          env={"PATH": "/usr/bin:/bin", "CALLS": str(tmp_path / "calls"), "BREAKS": str(tmp_path / "breaks")})
    assert done.returncode == ends, done.stdout + done.stderr
    assert len((tmp_path / "calls").read_text(encoding="utf-8").splitlines()) == builds
    assert ("The build of the plugin failed. It is tried once more." in done.stdout) == said
    assert done.stdout.count("Could not resolve io.ktor:ktor-client-core:3.6.0.") == breaks
    # The check of the compile after this step reads the log, so the log is the last try's alone.
    log = (tmp_path / "kotlin-compile.log").read_text(encoding="utf-8")
    assert ("> Task :compileKotlin" in log) == (ends == 0)
    assert ("Could not resolve" in log) == (ends == 1)
