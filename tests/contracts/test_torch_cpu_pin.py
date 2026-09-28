"""The CPU-torch pre-install in a service Dockerfile must pin the SAME
torch version as that service's requirements.txt.

Both Dockerfiles install torch from the CPU-only index first, then run the
requirements install. When the two pins drift apart, the second pip
"upgrades" torch from PyPI and silently drags the ~8 GB nvidia/cu*
dependency stack into a CPU-only image (observed 2026-07-20: lens/v3
images at 8.29/7.91 GB instead of ~3 GB, and rebuilds failing outright on
a 43 GB host).
"""

import re
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

# geometric-lens is the only service that ships torch: it owns C(x)/G(x)
# scoring. v3-service parses and orchestrates and imports no torch — see
# test_v3_service_ships_no_torch below, which keeps it that way.
SERVICES = [
    ("geometric-lens", REPO / "geometric-lens" / "Dockerfile",
     REPO / "geometric-lens" / "requirements.txt"),
]

DOCKERFILE_TORCH = re.compile(r"pip install [^&]*?torch==([0-9][\w.+]*)")
REQUIREMENTS_TORCH = re.compile(r"^torch==([0-9][\w.+]*)\s*$", re.MULTILINE)


@pytest.mark.parametrize("name,dockerfile,requirements",
                         SERVICES, ids=[s[0] for s in SERVICES])
def test_torch_pins_match(name, dockerfile, requirements):
    df_text = dockerfile.read_text()
    req_text = requirements.read_text()

    df_pin = DOCKERFILE_TORCH.search(df_text)
    req_pin = REQUIREMENTS_TORCH.search(req_text)
    assert df_pin, f"{name}: no torch pin found in {dockerfile}"
    assert req_pin, f"{name}: no torch pin found in {requirements}"
    assert df_pin.group(1) == req_pin.group(1), (
        f"{name}: Dockerfile pre-installs torch=={df_pin.group(1)} but "
        f"requirements.txt pins torch=={req_pin.group(1)}. The requirements "
        f"install will replace the CPU wheel with PyPI's CUDA build "
        f"(~8 GB of nvidia/cu* deps in a CPU-only image). Keep both pins "
        f"identical."
    )


@pytest.mark.parametrize("name,dockerfile,requirements",
                         SERVICES, ids=[s[0] for s in SERVICES])
def test_torch_preinstall_uses_cpu_index(name, dockerfile, requirements):
    df_text = dockerfile.read_text()
    torch_stmt = re.search(
        r"pip install[^&]*torch==[^&]*", df_text)
    assert torch_stmt, f"{name}: no torch install statement in {dockerfile}"
    assert "download.pytorch.org/whl/cpu" in torch_stmt.group(0), (
        f"{name}: the torch pre-install must use the CPU-only index "
        f"(--index-url https://download.pytorch.org/whl/cpu)."
    )


def test_ci_lens_job_preinstalls_the_same_torch():
    """CI's lens test job pre-installs CPU torch the way the Dockerfile does.

    It drifted once: CI pre-installed 2.12.1 while requirements.txt pinned
    2.13.0, so the requirements install replaced the CPU wheel with PyPI's
    build on every run, the failure this file exists to prevent in images.
    """
    workflow = (REPO / ".github" / "workflows" / "test.yml").read_text()
    ci_pin = re.search(
        r"pip install torch==([0-9][\w.+]*) --index-url "
        r"https://download\.pytorch\.org/whl/cpu", workflow)
    req_pin = REQUIREMENTS_TORCH.search(
        (REPO / "geometric-lens" / "requirements.txt").read_text())
    assert ci_pin, "test.yml: no CPU-index torch pre-install in the lens job"
    assert req_pin
    assert ci_pin.group(1) == req_pin.group(1), (
        f"test.yml pre-installs torch=={ci_pin.group(1)} but "
        f"geometric-lens/requirements.txt pins torch=={req_pin.group(1)}. "
        f"Keep them identical.")


def test_v3_service_ships_no_torch():
    """v3-service must not install torch: it imports none.

    It shipped ~900 MB of unimported torch until 2026-08-01 (E2/W1) —
    the image was 1.37 GB, nearly all of it a tensor stack the service
    never touches. Scoring lives in geometric-lens, inference in
    llama-server. If a stage ever genuinely needs torch, call the lens
    rather than re-adding a second copy of it here.
    """
    for path in (REPO / "v3-service" / "requirements.txt",
                 REPO / "v3-service" / "Dockerfile"):
        assert not re.search(r"(?m)^\s*torch[=<>~ ]|pip install[^&\n]*torch",
                             path.read_text()), (
            f"{path.name} reintroduces torch into v3-service. No module "
            f"under v3-service/ imports it; route tensor work to the lens.")

    src = " ".join(
        p.read_text() for p in (REPO / "v3-service").rglob("*.py")
        if "__pycache__" not in str(p))
    assert not re.search(r"(?m)^\s*(import torch|from torch\b)", src), (
        "a v3-service module imports torch — either route the work to "
        "geometric-lens or update this contract deliberately.")
