"""The steering build runs as the user who owns the models folder, and reaches the GPU by the host's group numbers."""
import re
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BOOTSTRAP = ROOT / "scripts" / "atlas-bootstrap.sh"


def test_the_steering_build_runs_as_the_target_user():
    script = BOOTSTRAP.read_text()
    start = script.index('docker run --rm "${gpu_run_args[@]}"')
    run = script[start:script.index("--entrypoint llama-cvector-generator")]

    assert '--user "$(id -u "$TARGET_USER"):$(id -g "$TARGET_USER")"' in run


@pytest.mark.skipif(shutil.which("getent") is None, reason="the bootstrap runs on Linux")
def test_host_gid_gives_the_number_or_else_the_name():
    function = re.search(r"^host_gid\(\) \{.*?^\}", BOOTSTRAP.read_text(), re.M | re.S).group()

    done = subprocess.run(["bash", "-c", f"{function}\nhost_gid root; host_gid no-such-group"],
                          capture_output=True, text=True)

    assert done.stdout.split() == ["0", "no-such-group"]
