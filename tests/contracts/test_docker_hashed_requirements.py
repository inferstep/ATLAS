"""The service Dockerfiles install Python dependencies from hashed locks.

geometric-lens and v3-service must install requirements using --require-hashes
and --only-binary=:all: (issue #333, refs #202).

Each pin in a service's requirements.txt must match the version in its
requirements-lock.txt.
"""
import re
from pathlib import Path

import pytest

ROOT = Path(__file__).resolve().parents[2]

SERVICE_CONFIGS = [
    ("geometric-lens/Dockerfile", "geometric-lens/requirements.txt", "geometric-lens/requirements-lock.txt"),
    ("v3-service/Dockerfile", "v3-service/requirements.txt", "v3-service/requirements-lock.txt"),
]


@pytest.mark.parametrize("dockerfile,requirements,lockfile", SERVICE_CONFIGS)
def test_dockerfile_installs_with_require_hashes_and_only_binary(dockerfile, requirements, lockfile):
    df_path = ROOT / dockerfile
    lock_path = ROOT / lockfile

    assert df_path.exists(), f"{dockerfile} does not exist"
    assert lock_path.exists(), f"{lockfile} does not exist"

    df_content = df_path.read_text(encoding="utf-8")
    lock_content = lock_path.read_text(encoding="utf-8")

    assert "--require-hashes" in df_content, f"{dockerfile} must install with --require-hashes"
    assert "--only-binary=:all:" in df_content, f"{dockerfile} must install with --only-binary=:all:"
    assert "--hash=sha256:" in lock_content, f"{lockfile} has no sha256 hashes"


def _normalize_name(name: str) -> str:
    return re.sub(r"[-_.]+", "-", name).lower()


def _parse_locked_versions(lock_text: str) -> dict[str, str]:
    locked = {}
    for block in re.split(r"\n(?=[a-z0-9])", lock_text):
        m = re.match(r"([a-z0-9._-]+)(?:\[[^\]]*\])?==(\S+)", block.strip())
        if m:
            locked[_normalize_name(m.group(1))] = m.group(2)
    return locked


def _parse_requirements_pins(req_text: str) -> dict[str, str]:
    pinned = {}
    for line in req_text.splitlines():
        line = line.split("#", 1)[0].strip()
        if not line or line.startswith("-"):
            continue
        if ";" in line:
            part, marker = line.split(";", 1)
            line = part.strip()
            if 'sys_platform != "linux"' in marker:
                continue
        m = re.match(r"([A-Za-z0-9._-]+)==(\S+)", line)
        if m:
            pinned[_normalize_name(m.group(1))] = m.group(2)
    return pinned


@pytest.mark.parametrize("dockerfile,requirements,lockfile", SERVICE_CONFIGS)
def test_requirements_pins_match_lockfile(dockerfile, requirements, lockfile):
    req_path = ROOT / requirements
    lock_path = ROOT / lockfile

    assert req_path.exists(), f"{requirements} does not exist"
    assert lock_path.exists(), f"{lockfile} does not exist"

    req_pins = _parse_requirements_pins(req_path.read_text(encoding="utf-8"))
    locked_pins = _parse_locked_versions(lock_path.read_text(encoding="utf-8"))

    assert req_pins, f"{requirements} has no parsed pins"

    for pkg, req_version in req_pins.items():
        assert pkg in locked_pins, f"{pkg} from {requirements} not found in {lockfile}"
        locked_version = locked_pins[pkg]
        assert (locked_version == req_version or locked_version == f"{req_version}+cpu"), (
            f"{pkg} in {requirements} pins {req_version} but {lockfile} has {locked_version}"
        )
