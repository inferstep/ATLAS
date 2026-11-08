"""The two install scripts take HTTPS only, and they check each installer before it runs.

scripts/atlas-bootstrap.sh and scripts/install/install.sh run as root. One test
reads both scripts. The others run a function of the bootstrap in bash, with
stand-ins for curl, sudo and the other commands that the function looks up.
So nothing is downloaded, installed or unpacked, and each run has a time limit.
"""
import hashlib
import re
import shutil
import subprocess
from pathlib import Path

import pytest

from tests.infrastructure.bootstrap.test_bootstrap_download_retry import LOGS, function, setting

ROOT = Path(__file__).resolve().parents[3]
SCRIPTS = [ROOT / "scripts" / "atlas-bootstrap.sh", ROOT / "scripts" / "install" / "install.sh"]
PINS = ["CURL_NOT_A_NETWORK_ERROR", "DOCKER_INSTALL_COMMIT", "DOCKER_INSTALL_SHA256", "GO_VERSION", "GO_SHA256_AMD64",
        "GO_SHA256_ARM64"]
DOWNLOADED = "the downloaded file\n"
DOWNLOADED_SHA256 = hashlib.sha256(DOWNLOADED.encode()).hexdigest()
pytestmark = pytest.mark.skipif(shutil.which("bash") is None, reason="the bootstrap script is a bash script")


def curl_commands(path):
    """Each command of a script that runs curl, with its continued lines joined into one line."""
    text = path.read_text(encoding="utf-8").replace("\\\n", " ")
    code = [line.strip() for line in text.splitlines() if not line.lstrip().startswith("#")]
    # `command -v curl &>/dev/null` and `install -y curl` only name curl: no word other than `&>` follows it.
    return [line for line in code if re.search(r"\bcurl\s+[^&\s]", line)]


@pytest.mark.parametrize("path", SCRIPTS, ids=lambda path: path.name)
def test_every_curl_forces_https_and_no_download_goes_into_a_shell(path):
    commands = curl_commands(path)
    assert commands, f"no curl command was found in {path.name}, so this test reads the script wrongly"
    # `--proto '=https'` stops curl from following a redirect to plain HTTP.
    assert [command for command in commands if "--proto '=https' --tlsv1.2" not in command] == []
    assert [command for command in commands if re.search(r"\|\s*(\$SUDO\s+|sudo\s+)?(ba)?sh\b", command)] == []


def stand_in(folder, name, text):
    path = folder / name
    path.write_text("#!/bin/bash\n" + text, encoding="utf-8")
    path.chmod(0o755)


def curl_stand_in(folder):
    """A curl that writes down its arguments and writes DOWNLOADED to the file after -o."""
    stand_in(folder, "curl", f'echo "$*" >> "{folder}/curl.calls"\n'
                             'while [ $# -gt 0 ]; do\n'
                             f'  if [ "$1" = -o ]; then printf "{DOWNLOADED}" > "$2"; fi\n'
                             '  shift\n'
                             'done\n')


def run(folder, body, **settings):
    """Run `body` in bash after the bootstrap's pins and retry_download. $SUDO only writes down each command."""
    (folder / "run").mkdir()
    script = ("set -euo pipefail\n" + LOGS + 'log_step() { echo "STEP $*"; }\n' + 'die() { echo "DIE $*"; exit 1; }\n'
              + f'sudo_stand_in() {{ echo "$*" >> "{folder}/sudo.calls"; }}\nSUDO=sudo_stand_in\n'
              + "".join(setting(name) for name in PINS) + function("retry_download") + body)
    env = {"PATH": f"{folder}:/usr/bin:/bin", "ATLAS_RUN_DIR": str(folder / "run"), "ATLAS_DOWNLOAD_WAIT_SECONDS": "0",
           **settings}
    return subprocess.run(["bash", "-c", script], cwd=folder, env=env, capture_output=True, text=True, timeout=20)


def test_a_docker_installer_that_does_not_match_its_sha256_does_not_run(tmp_path):
    # docker is on the path but has no compose plugin, so install_docker goes on to the download.
    stand_in(tmp_path, "docker", "exit 1\n")
    curl_stand_in(tmp_path)
    done = run(tmp_path, function("install_docker") + "install_docker\n")
    assert done.returncode == 1, done.stdout + done.stderr
    assert done.stdout.splitlines()[-1] == "DIE The Docker installer does not match DOCKER_INSTALL_SHA256, so it did not run."
    assert re.search(r"https://raw\.githubusercontent\.com/docker/docker-install/[0-9a-f]{40}/install\.sh",
                     (tmp_path / "curl.calls").read_text())
    assert not (tmp_path / "sudo.calls").exists()


def go_stand_ins(folder):
    # The machine is x86_64, so the amd64 pin applies. Its Go is too old, so install_go installs one.
    stand_in(folder, "uname", "echo x86_64\n")
    stand_in(folder, "go", 'echo "go version go1.20 linux/amd64"\n')
    curl_stand_in(folder)


@pytest.mark.parametrize("settings", [
    pytest.param({}, id="the-pinned-sha256"),
    pytest.param({"ATLAS_GO_SHA256": "0" * 64}, id="atlas-go-sha256"),
])
def test_a_go_archive_that_does_not_match_its_sha256_is_removed_and_not_unpacked(tmp_path, settings):
    go_stand_ins(tmp_path)
    done = run(tmp_path, function("install_go") + 'install_go || echo "install_go returned $?"\n', **settings)
    assert done.stdout.splitlines()[-1] == "install_go returned 1", done.stdout + done.stderr
    assert ".linux-amd64.tar.gz does not match the SHA-256" in done.stdout
    assert not (tmp_path / "run" / "atlas-go.tar.gz").exists()
    # Nothing ran as root: /usr/local/go was not removed, and the archive was not unpacked.
    assert not (tmp_path / "sudo.calls").exists()


def test_another_go_version_without_atlas_go_sha256_stops_before_the_download(tmp_path):
    go_stand_ins(tmp_path)
    done = run(tmp_path, function("install_go") + 'install_go || echo "install_go returned $?"\n', ATLAS_GO_VERSION="1.27.3")
    assert done.stdout.splitlines()[-1] == "install_go returned 1", done.stdout + done.stderr
    assert ("Fix: set ATLAS_GO_SHA256 to the SHA-256 of go1.27.3.linux-amd64.tar.gz from https://go.dev/dl/"
            in done.stdout)
    assert not (tmp_path / "curl.calls").exists()


def test_another_go_version_is_unpacked_when_it_matches_atlas_go_sha256(tmp_path):
    go_stand_ins(tmp_path)
    done = run(tmp_path, function("install_go") + "ensure_target_profile_path() { :; }\ninstall_go\n",
               ATLAS_GO_VERSION="1.27.3", ATLAS_GO_SHA256=DOWNLOADED_SHA256)
    assert done.returncode == 0, done.stdout + done.stderr
    assert "https://go.dev/dl/go1.27.3.linux-amd64.tar.gz" in (tmp_path / "curl.calls").read_text()
    archive = tmp_path / "run" / "atlas-go.tar.gz"
    assert (tmp_path / "sudo.calls").read_text().splitlines() == ["rm -rf /usr/local/go", f"tar -C /usr/local -xzf {archive}"]
