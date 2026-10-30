"""The model server inference images run as a non-root user.

The inference images must declare a non-root USER and configure the runtime
user environment (issue #334).
"""
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]
INFERENCE_DOCKERFILES = [
    "inference/Dockerfile",
    "inference/Dockerfile.v31",
    "inference/Dockerfile.vulkan",
    "inference/Dockerfile.rocm",
]


@pytest.mark.parametrize("dockerfile", INFERENCE_DOCKERFILES)
def test_inference_dockerfile_runs_as_non_root_user(dockerfile):
    path = ROOT / dockerfile
    assert path.exists(), f"{dockerfile} does not exist"
    content = path.read_text(encoding="utf-8")

    lines = [line.strip() for line in content.splitlines() if line.strip() and not line.strip().startswith("#")]
    user_directives = [line for line in lines if line.startswith("USER ")]

    assert user_directives, f"{dockerfile} has no USER directive; model server would run as root"
    last_user = user_directives[-1].split()[1]
    assert last_user != "root", f"{dockerfile} has root as its final USER"
    assert last_user == "atlas", f"{dockerfile} expected final USER atlas, got {last_user}"
    assert "useradd" in content, f"{dockerfile} must create the runtime user with useradd"
