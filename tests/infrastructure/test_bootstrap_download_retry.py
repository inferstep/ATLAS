"""The bootstrap script tries a failed download again, and nothing else.

Each case runs the script's own function in bash, with a stand-in for the
command that downloads: a small script that fails a given number of times and
then passes. The wait between two tries is set to 0, and every run has a time
limit, so a loop that lost its end fails the test and does not run on.
"""
import re
import shutil
import subprocess
import time
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "atlas-bootstrap.sh"
TEXT = SCRIPT.read_text(encoding="utf-8")
LOGS = 'log_info() { echo "INFO $*"; }\nlog_ok() { echo "OK $*"; }\nlog_warn() { echo "WARN $*"; }\nlog_err() { echo "ERR $*"; }\n'
TIMED_OUT = "ERROR: HTTPSConnectionPool(host='pypi.org', port=443): Read timed out."
pytestmark = pytest.mark.skipif(shutil.which("bash") is None, reason="the bootstrap script is a bash script")


def function(name):
    found = re.search(rf"^{name}\(\) \{{\n.*?^\}}\n", TEXT, re.M | re.S)
    assert found, f"{name}() is not in scripts/atlas-bootstrap.sh"
    return found.group(0)


def setting(name):
    found = re.search(rf"^{name}='[^'\n]*'\n", TEXT, re.M)
    assert found, f"{name} is not set in scripts/atlas-bootstrap.sh"
    return found.group(0)


def stand_in(folder, fails, says, status=7, name="fetch"):
    """A command that fails `fails` times with `says` and `status`, then passes. It counts its calls in a file."""
    path = folder / name
    path.write_text(f"""#!/bin/bash
calls=$(cat "{folder}/{name}.calls" 2>/dev/null || echo 0)
calls=$((calls + 1))
echo "$calls" > "{folder}/{name}.calls"
if [ "$calls" -le {fails} ]; then
  echo "downloading, try $calls"
  echo "{says}" >&2
  exit {status}
fi
echo "downloaded on try $calls"
""", encoding="utf-8")
    path.chmod(0o755)
    return path


def calls(folder, name="fetch"):
    counted = folder / f"{name}.calls"
    return int(counted.read_text()) if counted.exists() else 0


def run(folder, body, tries=None):
    script = "set -euo pipefail\n" + LOGS + function("retry_download") + setting("PIP_NETWORK_ERROR") + setting(
        "GO_NOT_A_NETWORK_ERROR") + body
    env = {"PATH": f"{folder}:/usr/bin:/bin", "ATLAS_DOWNLOAD_WAIT_SECONDS": "0", "TMPDIR": str(folder)}
    if tries is not None:
        env["ATLAS_DOWNLOAD_TRIES"] = str(tries)
    return subprocess.run(["bash", "-c", script], cwd=folder, env=env, capture_output=True, text=True, timeout=20)


def retry_lines(done):
    return [line for line in done.stdout.splitlines() if line.startswith("WARN ") and "Trying it again" in line]


def test_a_download_that_fails_twice_and_then_passes_goes_on_with_two_retry_lines(tmp_path):
    stand_in(tmp_path, fails=2, says=TIMED_OUT)
    done = run(tmp_path, 'retry_download "The package download" -- fetch\necho "status $?"\necho "the install goes on"\n')
    assert done.returncode == 0, done.stdout + done.stderr
    assert calls(tmp_path) == 3
    assert retry_lines(done) == [
        f"WARN The package download failed (exit 7): {TIMED_OUT}. Trying it again in 0s (try 2 of 3)…",
        f"WARN The package download failed (exit 7): {TIMED_OUT}. Trying it again in 0s (try 3 of 3)…"]
    assert done.stdout.rstrip().endswith("downloaded on try 3\nstatus 0\nthe install goes on")


def test_a_download_that_fails_three_times_stops_with_its_own_error_and_status(tmp_path):
    stand_in(tmp_path, fails=99, says=TIMED_OUT)
    done = run(tmp_path, 'retry_download "The package download" -- fetch || echo "stopped with status $?"\n')
    assert calls(tmp_path) == 3
    assert len(retry_lines(done)) == 2
    assert done.stdout.rstrip().endswith(f"downloading, try 3\n{TIMED_OUT}\nstopped with status 7")


def test_with_errexit_on_a_download_that_fails_three_times_stops_the_script(tmp_path):
    stand_in(tmp_path, fails=99, says=TIMED_OUT)
    done = run(tmp_path, 'retry_download "The package download" -- fetch\necho "the install goes on"\n')
    assert done.returncode == 7
    assert "the install goes on" not in done.stdout
    assert calls(tmp_path) == 3


@pytest.mark.parametrize("says", [
    "error: invalid command 'bdist_wheel'",
    "ERROR: Could not build wheels for atlas, which is required to install pyproject.toml-based projects",
    "error: externally-managed-environment",
])
def test_a_build_error_of_a_step_that_also_builds_is_not_tried_again(tmp_path, says):
    stand_in(tmp_path, fails=99, says=says, status=1)
    done = run(tmp_path, 'retry_download "The download for the ATLAS CLI install" --only-if "$PIP_NETWORK_ERROR" -- fetch'
                         ' || echo "stopped with status $?"\n')
    assert calls(tmp_path) == 1
    assert retry_lines(done) == []
    assert done.stdout.rstrip().endswith(f"{says}\nstopped with status 1")


@pytest.mark.parametrize("says", [
    TIMED_OUT,
    "WARNING: Retrying (Retry(total=0, connect=None)) after connection broken by 'ReadTimeoutError(...)': /simple/setuptools/",
    "ERROR: Could not fetch URL https://pypi.org/simple/setuptools/: connection error",
    "NewConnectionError: Failed to establish a new connection: [Errno -3] Temporary failure in name resolution",
])
def test_a_network_error_of_a_step_that_also_builds_is_tried_again(tmp_path, says):
    stand_in(tmp_path, fails=1, says=says, status=1)
    done = run(tmp_path, 'retry_download "The download for the ATLAS CLI install" --only-if "$PIP_NETWORK_ERROR" -- fetch\n')
    assert done.returncode == 0
    assert calls(tmp_path) == 2
    assert len(retry_lines(done)) == 1


@pytest.mark.parametrize("says, tried", [
    ("verifying github.com/x/y@v1.2.3: checksum mismatch", 1),
    ("SECURITY ERROR", 1),
    ("go: errors parsing go.mod:", 1),
    ("stream error: stream ID 5; INTERNAL_ERROR; received from peer", 3),
    ("dial tcp: lookup proxy.golang.org: i/o timeout", 3),
])
def test_a_download_whose_file_fails_its_check_is_not_tried_again(tmp_path, says, tried):
    stand_in(tmp_path, fails=99, says=says, status=1)
    done = run(tmp_path, 'retry_download "The Go module download" --not-if "$GO_NOT_A_NETWORK_ERROR" -- fetch'
                         ' || echo "stopped with status $?"\n')
    assert calls(tmp_path) == tried
    assert done.stdout.rstrip().endswith("stopped with status 1")


def test_the_number_of_tries_is_a_setting_and_one_try_means_no_retry(tmp_path):
    stand_in(tmp_path, fails=99, says=TIMED_OUT)
    assert run(tmp_path, 'retry_download "The package download" -- fetch || true\n', tries=5).returncode == 0
    assert calls(tmp_path) == 5
    (tmp_path / "fetch.calls").unlink()
    done = run(tmp_path, 'retry_download "The package download" -- fetch || true\n', tries=1)
    assert calls(tmp_path) == 1 and retry_lines(done) == []


def test_with_a_log_file_the_output_goes_there_and_the_retry_line_is_still_printed(tmp_path):
    stand_in(tmp_path, fails=1, says=TIMED_OUT)
    done = run(tmp_path, f'retry_download "The pip and setuptools download" --log "{tmp_path}/pip.log" -- fetch\n')
    assert done.returncode == 0
    assert done.stdout.splitlines() == [
        f"WARN The pip and setuptools download failed (exit 7): {TIMED_OUT}. Trying it again in 0s (try 2 of 3)…"]
    assert (tmp_path / "pip.log").read_text().splitlines() == ["downloading, try 1", TIMED_OUT, "downloaded on try 2"]


def test_the_helper_keeps_its_two_files_in_a_private_temporary_folder_and_removes_it(tmp_path):
    # Where the system puts a temporary folder differs from system to system, so the helper's own call says where.
    notes = 'mktemp() { local made; made=$(command mktemp "$@"); echo "$* $made" >> "%s/made"; echo "$made"; }\n' % tmp_path
    (tmp_path / "look").write_text(f"""#!/bin/bash
calls=$(cat "{tmp_path}/look.calls" 2>/dev/null || echo 0)
calls=$((calls + 1))
echo "$calls" > "{tmp_path}/look.calls"
if [ "$calls" = 2 ]; then
  folder=$(cut -d' ' -f2- "{tmp_path}/made")
  ls -ld "$folder" | cut -c1-10 > "{tmp_path}/folder.mode"
  ls "$folder" | sort > "{tmp_path}/folder.files"
fi
echo "{TIMED_OUT}" >&2
exit 7
""", encoding="utf-8")
    (tmp_path / "look").chmod(0o755)
    script = ("set -euo pipefail\n" + LOGS + notes + function("retry_download")
              + 'retry_download "The package download" -- look || true\n')
    env = {"PATH": f"{tmp_path}:/usr/bin:/bin", "ATLAS_DOWNLOAD_WAIT_SECONDS": "0"}
    subprocess.run(["bash", "-c", script], cwd=tmp_path, env=env, capture_output=True, text=True, timeout=20, check=True)
    made = (tmp_path / "made").read_text().splitlines()
    assert len(made) == 1 and made[0].startswith("-d "), made
    assert (tmp_path / "folder.mode").read_text().strip() == "drwx------"
    assert (tmp_path / "folder.files").read_text().split() == ["output", "status"]
    assert not Path(made[0].split(" ", 1)[1]).exists()


def test_a_step_that_fails_and_prints_nothing_is_tried_again_and_says_so(tmp_path):
    (tmp_path / "fetch").write_text(f'#!/bin/bash\necho x >> "{tmp_path}/fetch.calls"\nexit 3\n', encoding="utf-8")
    (tmp_path / "fetch").chmod(0o755)
    done = run(tmp_path, 'retry_download "The package download" -- fetch\necho "the install goes on"\n')
    assert done.returncode == 3 and "the install goes on" not in done.stdout
    assert (tmp_path / "fetch.calls").read_text().count("x") == 3
    assert retry_lines(done) == [f"WARN The package download failed (exit 3): no output. Trying it again in 0s (try {n} of 3)…"
                                 for n in (2, 3)]


def test_the_command_is_run_as_it_is_written_with_its_arguments(tmp_path):
    (tmp_path / "show").write_text('#!/bin/bash\nprintf "<%s>" "$@"\n', encoding="utf-8")
    (tmp_path / "show").chmod(0o755)
    done = run(tmp_path, 'retry_download "A step" -- show "two words" --only-if x\n')
    assert done.stdout == "<two words><--only-if><x>"
    wrong = run(tmp_path, 'retry_download "A step" --sometimes -- show || echo "status $?"\n')
    assert "is not an option of it" in wrong.stdout and wrong.stdout.rstrip().endswith("status 2")


def tui_build(tmp_path, download_fails, download_says, build_fails):
    """Run the script's own build step with a stand-in `go` that keeps what it was asked to do."""
    (tmp_path / "tui").mkdir()
    (tmp_path / "home").mkdir()
    (tmp_path / "go").write_text(f"""#!/bin/bash
echo "$*" >> "{tmp_path}/go.asked"
if [ "$1 $2" = "mod download" ]; then
  n=$(grep -c '^mod download' "{tmp_path}/go.asked")
  if [ "$n" -le {download_fails} ]; then echo "{download_says}" >&2; exit 1; fi
  exit 0
fi
if [ "$1" = "build" ]; then
  if [ {build_fails} = 1 ]; then echo "./main.go:9:2: undefined: missing" >&2; exit 1; fi
  : > "$3"; chmod +x "$3"
fi
""", encoding="utf-8")
    (tmp_path / "go").chmod(0o755)
    body = (function("build_atlas_tui") + 'run_as_target() { "$@"; }\n' + f'target_home_dir() {{ echo "{tmp_path}/home"; }}\n'
            + f'ATLAS_INSTALL_DIR="{tmp_path}"\n' + 'build_atlas_tui && echo "built" || echo "not built"\n')
    done = run(tmp_path, body)
    asked = (tmp_path / "go.asked").read_text().splitlines() if (tmp_path / "go.asked").exists() else []
    return done, [line.split(" -o ")[0] for line in asked]


def test_the_module_download_is_tried_again_and_the_build_runs_once_after_it(tmp_path):
    done, asked = tui_build(tmp_path, 2, "stream error: stream ID 5; INTERNAL_ERROR; received from peer", build_fails=0)
    assert asked == ["mod download", "mod download", "mod download", "build"]
    assert len(retry_lines(done)) == 2 and "The Go module download failed (exit 1)" in retry_lines(done)[0]
    assert done.stdout.rstrip().endswith("built")
    assert (tmp_path / "home" / ".local" / "bin" / "atlas-tui").exists()


def test_the_build_finds_a_go_that_is_on_the_path_and_not_under_usr_local(tmp_path):
    done, asked = tui_build(tmp_path, 0, "", build_fails=0)
    assert "command not found" not in done.stdout + done.stderr
    assert asked == ["mod download", "build"] and done.stdout.rstrip().endswith("built")
    lines = [line for line in TEXT.splitlines() if "/usr/local/go/bin:" in line and " go " in line and "sh -c" in line]
    assert len(lines) == 2 and all("PATH=\\\"/usr/local/go/bin:\\$PATH\\\" go" in line for line in lines), lines


def test_a_compile_error_is_not_tried_again(tmp_path):
    done, asked = tui_build(tmp_path, 0, "", build_fails=1)
    assert asked == ["mod download", "build"]
    assert retry_lines(done) == []
    assert "undefined: missing" in done.stdout and done.stdout.rstrip().endswith("not built")


def test_when_the_modules_cannot_be_downloaded_the_build_is_not_started(tmp_path):
    done, asked = tui_build(tmp_path, 99, "dial tcp: lookup proxy.golang.org: i/o timeout", build_fails=0)
    assert asked == ["mod download"] * 3
    assert "atlas-tui build failed (exit 1)" in done.stdout and done.stdout.rstrip().endswith("not built")


def test_each_step_says_which_of_its_errors_is_tried_again():
    calls_of = dict(re.findall(r'retry_download "([^"]+)"((?: --[a-z-]+ \S+)*) -- ', TEXT.split("\nretry_download() {", 1)[1]))
    assert calls_of["The pip and setuptools download"] == ' --only-if "$PIP_NETWORK_ERROR" --log /tmp/atlas-pip.log'
    assert calls_of["The download for the ATLAS CLI install"] == ' --only-if "$PIP_NETWORK_ERROR"'
    assert calls_of["The Go module download"] == ' --not-if "$GO_NOT_A_NETWORK_ERROR"'


def test_the_wait_between_two_tries_is_kept(tmp_path):
    stand_in(tmp_path, fails=1, says=TIMED_OUT)
    script = ("set -euo pipefail\n" + LOGS + function("retry_download") + 'retry_download "The package download" -- fetch\n')
    env = {"PATH": f"{tmp_path}:/usr/bin:/bin", "ATLAS_DOWNLOAD_WAIT_SECONDS": "1", "TMPDIR": str(tmp_path)}
    start = time.monotonic()
    done = subprocess.run(["bash", "-c", script], cwd=tmp_path, env=env, capture_output=True, text=True, timeout=20)
    assert done.returncode == 0 and "Trying it again in 1s (try 2 of 3)" in done.stdout
    assert 1.0 <= time.monotonic() - start < 15


def test_only_the_download_steps_of_the_script_go_through_the_retry():
    through = re.findall(r'retry_download "([^"]+)"', TEXT.split("\nretry_download() {", 1)[1])
    assert through == ["The pip and setuptools download", "The download for the ATLAS CLI install", "The Go module download"]
    build = re.search(r"^.*\bgo build -o '\$out'.*$", TEXT, re.M).group(0)
    assert "retry_download" not in build
    assert TEXT.index("go mod download\"") < TEXT.index("go build -o '$out'")
