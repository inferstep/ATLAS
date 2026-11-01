"""The model server runs as a non-root user, and the one who starts the image sets it.

The images create the account and declare no USER (issue #334 asked for a non-root
server). A compose file from before 3.2.0 sets no user: with a USER in the image,
that stack could not read the service token and would run with internal auth off.
"""
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[3]
INFERENCE_DOCKERFILES = [
    "inference/Dockerfile.v31",
    "inference/Dockerfile.vulkan",
    "inference/Dockerfile.rocm",
]


@pytest.mark.parametrize("dockerfile", INFERENCE_DOCKERFILES)
def test_the_image_creates_the_account_and_declares_no_user(dockerfile):
    content = (ROOT / dockerfile).read_text(encoding="utf-8")

    assert "useradd" in content
    assert not [line for line in content.splitlines() if line.startswith("USER ")]


def test_k3s_runs_the_model_server_as_the_images_account():
    template = (ROOT / "templates" / "llama-deployment.yaml.tmpl").read_text(encoding="utf-8")

    assert "runAsUser: 1001" in template
