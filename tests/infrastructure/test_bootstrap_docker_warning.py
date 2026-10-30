from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
BOOTSTRAP = ROOT / "scripts" / "atlas-bootstrap.sh"
SETUP = ROOT / "docs" / "SETUP.md"


def test_docker_group_warning():
    script = BOOTSTRAP.read_text()

    assert "The docker group grants root-level privileges." in script

    for user in ("$SUDO_USER", "$USER"):
        assert (
            f'log_warn "Added {user} to the docker group. '
            'The docker group grants root-level privileges.'
        ) in script


def test_docker_skip_option_documented():
    script = BOOTSTRAP.read_text()
    guide = SETUP.read_text()

    assert "ATLAS_BOOTSTRAP_SKIP_DOCKER=1" in script
    assert "ATLAS_BOOTSTRAP_SKIP_DOCKER=1" in guide
    assert "root-level privileges" in guide


def test_warning_follows_docker_group_assignment():
    script = BOOTSTRAP.read_text().splitlines()

    for user in ("$SUDO_USER", "$USER"):
        assignment = f'usermod -aG docker "{user}"'
        matches = [
            i for i, line in enumerate(script)
            if assignment in line
        ]

        assert len(matches) == 1
        next_line = script[matches[0] + 1]

        assert "log_warn" in next_line
        assert "root-level privileges" in next_line
        assert "ATLAS_BOOTSTRAP_SKIP_DOCKER=1" in next_line
