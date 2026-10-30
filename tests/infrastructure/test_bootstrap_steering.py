"""The steering build runs as the user who owns the models folder, and reaches the GPU by the host's group numbers."""
import re
import shutil
import subprocess
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
BOOTSTRAP = ROOT / "scripts" / "atlas-bootstrap.sh"
needs_getent = pytest.mark.skipif(shutil.which("getent") is None, reason="the bootstrap runs on Linux")


def shell_function(name):
    return re.search(rf"^{name}\(\) \{{.*?^\}}", BOOTSTRAP.read_text(), re.M | re.S).group()


def test_the_steering_build_runs_as_the_target_user():
    script = BOOTSTRAP.read_text()
    start = script.index('docker run --rm "${gpu_run_args[@]}"')
    run = script[start:script.index("--entrypoint llama-cvector-generator")]

    assert '--user "$(id -u "$TARGET_USER"):$(id -g "$TARGET_USER")"' in run


@needs_getent
def test_host_gid_gives_the_number_or_else_the_name():
    done = subprocess.run(["bash", "-c", f"{shell_function('host_gid')}\nhost_gid root; host_gid no-such-group"],
                          capture_output=True, text=True)

    assert done.stdout.split() == ["0", "no-such-group"]


@needs_getent
def test_an_env_that_already_names_a_backend_still_gets_the_group_numbers(tmp_path):
    (tmp_path / ".env").write_text("ATLAS_BACKEND=rocm\n")
    functions = "\n".join(shell_function(name) for name in (
        "env_file_value", "set_env_file_value", "host_gid", "persist_backend_selection"))

    subprocess.run(["bash", "-c", f"TARGET_USER=root\nlog_ok() {{ :; }}\n{functions}\npersist_backend_selection"],
                   cwd=tmp_path, check=True)

    body = (tmp_path / ".env").read_text()
    assert "ATLAS_VIDEO_GID=" in body
    assert "ATLAS_RENDER_GID=" in body
    assert "ATLAS_LLAMA_USER=0:0" in body
