"""The scripts of scripts/install/ work from that folder, and the two that gate a deploy refuse.

No test here reaches a real stack. Each runs a copy of the script in pytest's own
folder, with stand-ins for `docker` and `curl` first on the path.
"""
import os
import shutil
import stat
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
INSTALL = ROOT / "scripts" / "install"


def test_install_runs_the_build_script_in_its_folder(tmp_path):
    assert (INSTALL / "build-containers.sh").stat().st_mode & stat.S_IXUSR
    stand_in = tmp_path / "install" / "build-containers.sh"
    stand_in.parent.mkdir()
    stand_in.write_text('#!/bin/bash\necho "the build ran"\n', encoding="utf-8")
    stand_in.chmod(0o755)
    install = (ROOT / "scripts" / "install.sh").read_text(encoding="utf-8")
    build_images = install[install.index("build_images() {"):install.index("# Process templates")]

    # install.sh runs main when it is loaded, so only its build step runs here.
    script = f'SCRIPT_DIR="{tmp_path}"\nFORCE_BUILD=true\nlog_info() {{ :; }}\n{build_images}\nbuild_images\n'
    done = subprocess.run(["bash", "-c", script], capture_output=True, text=True, timeout=30)

    assert done.returncode == 0, done.stderr
    assert done.stdout == "the build ran\n"


def test_a_deploy_refuses_a_checkout_with_uncommitted_changes(tmp_path):
    checkout = tmp_path / "checkout"
    script = checkout / "scripts" / "install" / "deploy-gated.sh"
    script.parent.mkdir(parents=True)
    subprocess.run(["git", "init", "-q", str(checkout)], check=True)
    subprocess.run(["git", "-C", str(checkout), "-c", "user.name=test", "-c", "user.email=test@example.invalid",
                    "commit", "-q", "--allow-empty", "-m", "first"], check=True)
    shutil.copy(INSTALL / "deploy-gated.sh", script)  # untracked, so the checkout has a change
    stand_ins = tmp_path / "bin"
    stand_ins.mkdir()
    called = tmp_path / "docker-was-called"
    (stand_ins / "docker").write_text(f'#!/bin/bash\ntouch "{called}"\n', encoding="utf-8")
    (stand_ins / "docker").chmod(0o755)
    record = tmp_path / "record"

    done = subprocess.run(["bash", str(script)], capture_output=True, text=True, timeout=30,
                          env={**os.environ, "PATH": f"{stand_ins}:{os.environ['PATH']}",
                               "ATLAS_DEPLOY_DIR": str(record)})

    assert done.returncode == 1, done.stdout + done.stderr
    assert done.stdout.splitlines()[-1] == "NOT DEPLOYED: the checkout has uncommitted changes"
    assert not (record / "DEPLOYED_SHA").exists()
    assert not called.exists()


def test_a_container_on_an_older_build_fails_the_check(tmp_path):
    script = tmp_path / "scripts" / "install" / "verify-deployed.sh"
    script.parent.mkdir(parents=True)
    shutil.copy(INSTALL / "verify-deployed.sh", script)
    stand_ins = tmp_path / "bin"
    stand_ins.mkdir()
    (stand_ins / "docker").write_text("""#!/bin/bash
case "$*" in
  "compose ps -q atlas-proxy") echo a-container ;;
  *"{{.Config.Image}}"*) echo atlas-proxy:dev ;;
  *"{{.Image}}"*) echo sha256:0000000000000000 ;;
  "images -q atlas-proxy:dev") echo 1111111111111111 ;;
esac
""", encoding="utf-8")
    (stand_ins / "curl").write_text("#!/bin/bash\nexit 7\n", encoding="utf-8")
    for stand_in in stand_ins.iterdir():
        stand_in.chmod(0o755)

    done = subprocess.run(["bash", str(script), "atlas-proxy"], capture_output=True, text=True, timeout=30,
                          env={**os.environ, "PATH": f"{stand_ins}:{os.environ['PATH']}"})

    assert done.returncode == 1, done.stdout + done.stderr
    assert "[STALE] atlas-proxy" in done.stdout
    assert "Fix:   docker compose up -d --force-recreate atlas-proxy --no-deps" in done.stdout
