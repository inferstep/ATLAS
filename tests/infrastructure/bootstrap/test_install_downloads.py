"""The two install scripts take HTTPS only, and they check each installer before it runs.

scripts/atlas-bootstrap.sh and scripts/install/install.sh run as root. One test
reads both scripts. The others run a function of one script in bash, with
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


def curl_stand_in(folder, served=DOWNLOADED, status=0):
    """A curl that writes down its arguments, writes `served` to the file after -o and exits with `status`."""
    (folder / "served.txt").write_text(served, encoding="utf-8")
    stand_in(folder, "curl", f'echo "$*" >> "{folder}/curl.calls"\n'
                             'while [ $# -gt 0 ]; do\n'
                             f'  if [ "$1" = -o ]; then cp "{folder}/served.txt" "$2"; fi\n'
                             '  shift\n'
                             'done\n'
                             f'exit {status}\n')


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


INSTALL_SH_TEXT = SCRIPTS[1].read_text(encoding="utf-8")
INSTALL_SH_PINS = ["K3S_VERSION", "K3S_INSTALLER_SHA256", "HELM_VERSION", "HELM_INSTALLER_SHA256", "GPU_OPERATOR_VERSION"]
# The installers that the curl stand-in serves in the K3s and Helm tests. Each one writes down how it ran.
K3S_INSTALLER = 'echo "INSTALL_K3S_VERSION=$INSTALL_K3S_VERSION $*" > installer.ran\n'
HELM_INSTALLER = """echo "DESIRED_VERSION=$DESIRED_VERSION" > installer.ran
# Like get-helm-3, it puts helm on the path. This helm only writes down its arguments.
cat > helm <<'END'
#!/bin/bash
echo "$*" >> helm.calls
END
chmod +x helm
"""


def install_sh_pin(name):
    found = re.search(rf"^{name}='([^'\n]*)'$", INSTALL_SH_TEXT, re.M)
    assert found, f"{name} is not set in scripts/install/install.sh"
    return found.group(1)


def install_sh_function(name):
    found = re.search(rf"^{name}\(\) \{{\n.*?^\}}\n", INSTALL_SH_TEXT, re.M | re.S)
    assert found, f"{name}() is not in scripts/install/install.sh"
    return found.group(0)


def run_install_sh(folder, body):
    """Run `body` in bash after install.sh's pins. The run finds only the commands in `folder`."""
    # k3s and helm are not on this path, so the functions install them.
    for name in ["bash", "sh", "cat", "chmod", "cp", "grep", "mktemp", "rm", "sha256sum"]:
        (folder / name).symlink_to(shutil.which(name))
    (folder / "tmp").mkdir()
    script = ("set -euo pipefail\n" + 'log_info() { echo "INFO $*"; }\nlog_error() { echo "ERROR $*"; }\n'
              + "".join(f"{name}='{install_sh_pin(name)}'\n" for name in INSTALL_SH_PINS) + body)
    # mktemp makes the installer's file in folder/tmp, so a test can see that the function removed it.
    env = {"PATH": str(folder), "TMPDIR": str(folder / "tmp")}
    return subprocess.run(["bash", "-c", script], cwd=folder, env=env, capture_output=True, text=True, timeout=20)


@pytest.mark.parametrize(("curl_status", "sha256"), [
    # curl writes a matching file and then fails, so only the failed download can stop the step.
    pytest.param(22, hashlib.sha256(K3S_INSTALLER.encode()).hexdigest(), id="a-failed-download"),
    pytest.param(0, "0" * 64, id="a-sha256-mismatch"),
])
def test_a_k3s_installer_that_did_not_download_or_does_not_match_its_sha256_does_not_run(tmp_path, curl_status, sha256):
    curl_stand_in(tmp_path, served=K3S_INSTALLER, status=curl_status)
    done = run_install_sh(tmp_path, install_sh_function("install_k3s")
                          + f"SKIP_K3S=false\nK3S_INSTALLER_SHA256='{sha256}'\ninstall_k3s\n")
    assert done.returncode == 1, done.stdout + done.stderr
    assert done.stdout.splitlines()[-1] == ("ERROR The K3s installer did not download, or it does not match "
                                            "K3S_INSTALLER_SHA256. It did not run.")
    assert not (tmp_path / "installer.ran").exists()
    assert list((tmp_path / "tmp").iterdir()) == []


def test_a_k3s_installer_that_matches_its_sha256_runs_with_the_pinned_version(tmp_path):
    curl_stand_in(tmp_path, served=K3S_INSTALLER)
    stand_in(tmp_path, "kubectl", "exit 0\n")
    stand_in(tmp_path, "sleep", "exit 0\n")
    sha256 = hashlib.sha256(K3S_INSTALLER.encode()).hexdigest()
    done = run_install_sh(tmp_path, install_sh_function("install_k3s")
                          + f"SKIP_K3S=false\nK3S_INSTALLER_SHA256='{sha256}'\ninstall_k3s\n")
    assert done.returncode == 0, done.stdout + done.stderr
    k3s_version = install_sh_pin("K3S_VERSION")
    assert (f"https://raw.githubusercontent.com/k3s-io/k3s/{k3s_version}/install.sh"
            in (tmp_path / "curl.calls").read_text())
    assert (tmp_path / "installer.ran").read_text() == f"INSTALL_K3S_VERSION={k3s_version} --write-kubeconfig-mode 644\n"
    assert list((tmp_path / "tmp").iterdir()) == []


@pytest.mark.parametrize(("curl_status", "sha256"), [
    # curl writes a matching file and then fails, so only the failed download can stop the step.
    pytest.param(22, hashlib.sha256(HELM_INSTALLER.encode()).hexdigest(), id="a-failed-download"),
    pytest.param(0, "0" * 64, id="a-sha256-mismatch"),
])
def test_a_helm_installer_that_did_not_download_or_does_not_match_its_sha256_does_not_run(tmp_path, curl_status,
                                                                                           sha256):
    curl_stand_in(tmp_path, served=HELM_INSTALLER, status=curl_status)
    # The cluster has a GPU only after helm has run.
    stand_in(tmp_path, "kubectl", "[ -e helm.calls ] && echo '\"nvidia.com/gpu\"'\n")
    done = run_install_sh(tmp_path, install_sh_function("check_gpu_available") + install_sh_function("install_gpu_operator")
                          + f"SKIP_GPU=false\nHELM_INSTALLER_SHA256='{sha256}'\ninstall_gpu_operator\n")
    assert done.returncode == 1, done.stdout + done.stderr
    assert done.stdout.splitlines()[-1] == ("ERROR The Helm installer did not download, or it does not match "
                                            "HELM_INSTALLER_SHA256. It did not run.")
    assert not (tmp_path / "installer.ran").exists()
    assert not (tmp_path / "helm").exists()
    assert list((tmp_path / "tmp").iterdir()) == []


def test_a_helm_installer_that_matches_its_sha256_runs_with_the_pinned_version(tmp_path):
    curl_stand_in(tmp_path, served=HELM_INSTALLER)
    # The cluster has a GPU only after helm has run.
    stand_in(tmp_path, "kubectl", "[ -e helm.calls ] && echo '\"nvidia.com/gpu\"'\n")
    sha256 = hashlib.sha256(HELM_INSTALLER.encode()).hexdigest()
    done = run_install_sh(tmp_path, install_sh_function("check_gpu_available") + install_sh_function("install_gpu_operator")
                          + f"SKIP_GPU=false\nHELM_INSTALLER_SHA256='{sha256}'\ninstall_gpu_operator\n")
    assert done.returncode == 0, done.stdout + done.stderr
    helm_version = install_sh_pin("HELM_VERSION")
    assert (f"https://raw.githubusercontent.com/helm/helm/{helm_version}/scripts/get-helm-3"
            in (tmp_path / "curl.calls").read_text())
    assert (tmp_path / "installer.ran").read_text() == f"DESIRED_VERSION={helm_version}\n"
    assert (f"install gpu-operator nvidia/gpu-operator --version {install_sh_pin('GPU_OPERATOR_VERSION')} "
            in (tmp_path / "helm.calls").read_text())
    assert list((tmp_path / "tmp").iterdir()) == []
