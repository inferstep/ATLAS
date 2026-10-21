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


def stand_in(folder, fails, says, status=7):
    """A command `fetch` that fails `fails` times with `says` and `status`, then passes. It counts its calls in a file."""
    path = folder / "fetch"
    path.write_text(f"""#!/bin/bash
calls=$(cat "{folder}/fetch.calls" 2>/dev/null || echo 0)
calls=$((calls + 1))
echo "$calls" > "{folder}/fetch.calls"
if [ "$calls" -le {fails} ]; then
  echo "downloading, try $calls"
  echo "{says}" >&2
  exit {status}
fi
echo "downloaded on try $calls"
""", encoding="utf-8")
    path.chmod(0o755)


def calls(folder):
    counted = folder / "fetch.calls"
    return int(counted.read_text()) if counted.exists() else 0


def run(folder, body, tries=None, wait="0"):
    script = "set -euo pipefail\n" + LOGS + function("retry_download") + setting("PIP_NETWORK_ERROR") + setting(
        "GO_NOT_A_NETWORK_ERROR") + setting("CURL_NOT_A_NETWORK_ERROR") + body
    # The folder of the run, which the script makes at its start, is given here.
    (folder / "run").mkdir(exist_ok=True)
    env = {"PATH": f"{folder}:/usr/bin:/bin", "ATLAS_DOWNLOAD_WAIT_SECONDS": wait, "ATLAS_RUN_DIR": str(folder / "run")}
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


@pytest.mark.parametrize("option, says, tried", [
    pytest.param("", TIMED_OUT, 3, id="no-option-every-error-is-tried-again"),
    # A step that also builds: only an error of the network is tried again.
    pytest.param('--only-if "$PIP_NETWORK_ERROR"', "error: invalid command 'bdist_wheel'", 1, id="pip-build-error-bdist-wheel"),
    pytest.param('--only-if "$PIP_NETWORK_ERROR"',
                 "ERROR: Could not build wheels for atlas, which is required to install pyproject.toml-based projects", 1,
                 id="pip-build-error-could-not-build-wheels"),
    pytest.param('--only-if "$PIP_NETWORK_ERROR"', "error: externally-managed-environment", 1,
                 id="pip-build-error-externally-managed"),
    pytest.param('--only-if "$PIP_NETWORK_ERROR"', TIMED_OUT, 3, id="pip-network-read-timed-out"),
    pytest.param('--only-if "$PIP_NETWORK_ERROR"',
                 "WARNING: Retrying (Retry(total=0, connect=None)) after connection broken by 'ReadTimeoutError(...)': "
                 "/simple/setuptools/", 3, id="pip-network-connection-broken"),
    pytest.param('--only-if "$PIP_NETWORK_ERROR"',
                 "ERROR: Could not fetch URL https://pypi.org/simple/setuptools/: connection error", 3,
                 id="pip-network-could-not-fetch-url"),
    pytest.param('--only-if "$PIP_NETWORK_ERROR"',
                 "NewConnectionError: Failed to establish a new connection: [Errno -3] Temporary failure in name resolution", 3,
                 id="pip-network-name-resolution"),
    # A download that checks what it got: a file that fails its check is not tried again.
    pytest.param('--not-if "$GO_NOT_A_NETWORK_ERROR"', "verifying github.com/x/y@v1.2.3: checksum mismatch", 1,
                 id="go-failed-check-checksum-mismatch"),
    pytest.param('--not-if "$GO_NOT_A_NETWORK_ERROR"', "SECURITY ERROR", 1, id="go-failed-check-security-error"),
    pytest.param('--not-if "$GO_NOT_A_NETWORK_ERROR"', "go: errors parsing go.mod:", 1,
                 id="go-failed-check-errors-parsing-go-mod"),
    pytest.param('--not-if "$GO_NOT_A_NETWORK_ERROR"', "stream error: stream ID 5; INTERNAL_ERROR; received from peer", 3,
                 id="go-network-stream-error"),
    pytest.param('--not-if "$GO_NOT_A_NETWORK_ERROR"', "dial tcp: lookup proxy.golang.org: i/o timeout", 3,
                 id="go-network-dial-timeout"),
    # The Go download: an answer that the file is not there is the same on every try.
    pytest.param('--not-if "$CURL_NOT_A_NETWORK_ERROR"',
                 "curl: (92) HTTP/2 stream 1 was not closed cleanly: INTERNAL_ERROR (err 2)", 3,
                 id="curl-network-http2-stream-not-closed"),
    pytest.param('--not-if "$CURL_NOT_A_NETWORK_ERROR"', "curl: (6) Could not resolve host: go.dev", 3,
                 id="curl-network-host-lookup"),
    pytest.param('--not-if "$CURL_NOT_A_NETWORK_ERROR"', "curl: (22) The requested URL returned error: 404", 1,
                 id="curl-file-not-there-404"),
])
def test_a_failing_step_is_tried_again_unless_its_error_is_the_same_on_every_try(tmp_path, option, says, tried):
    stand_in(tmp_path, fails=99, says=says)
    done = run(tmp_path, f'retry_download "The download" {option} -- fetch || echo "stopped with status $?"\n')
    assert calls(tmp_path) == tried
    assert len(retry_lines(done)) == tried - 1
    # The caller gets the output and the status of the last try.
    assert done.stdout.rstrip().endswith(f"downloading, try {tried}\n{says}\nstopped with status 7")


@pytest.mark.parametrize("tries", [pytest.param(5, id="five-tries"), pytest.param(1, id="one-try-means-no-retry")])
def test_the_number_of_tries_is_a_setting_and_one_try_means_no_retry(tmp_path, tries):
    stand_in(tmp_path, fails=99, says=TIMED_OUT)
    done = run(tmp_path, 'retry_download "The package download" -- fetch || true\n', tries=tries)
    assert calls(tmp_path) == tries
    assert len(retry_lines(done)) == tries - 1


def test_with_a_log_file_the_output_goes_there_and_the_retry_line_is_still_printed(tmp_path):
    stand_in(tmp_path, fails=1, says=TIMED_OUT)
    done = run(tmp_path, f'retry_download "The pip and setuptools download" --log "{tmp_path}/pip.log" -- fetch\n')
    assert done.returncode == 0
    assert done.stdout.splitlines() == [
        f"WARN The pip and setuptools download failed (exit 7): {TIMED_OUT}. Trying it again in 0s (try 2 of 3)…"]
    assert (tmp_path / "pip.log").read_text().splitlines() == ["downloading, try 1", TIMED_OUT, "downloaded on try 2"]


def test_the_helper_keeps_its_two_files_in_a_private_temporary_folder_and_removes_it(tmp_path):
    # The folder is one of its own, inside the folder of the run. The stand-in looks at it during the second try.
    (tmp_path / "look").write_text(f"""#!/bin/bash
calls=$(cat "{tmp_path}/look.calls" 2>/dev/null || echo 0)
calls=$((calls + 1))
echo "$calls" > "{tmp_path}/look.calls"
if [ "$calls" = 2 ]; then
  ls "{tmp_path}/run" > "{tmp_path}/run.files"
  ls -ld "{tmp_path}"/run/* | cut -c1-10 > "{tmp_path}/folder.mode"
  ls "{tmp_path}"/run/* | sort > "{tmp_path}/folder.files"
fi
echo "{TIMED_OUT}" >&2
exit 7
""", encoding="utf-8")
    (tmp_path / "look").chmod(0o755)
    done = run(tmp_path, 'retry_download "The package download" -- look || true\n')
    assert done.returncode == 0, done.stdout + done.stderr
    assert len(retry_lines(done)) == 2, done.stdout + done.stderr
    in_the_run = (tmp_path / "run.files").read_text().split()
    assert len(in_the_run) == 1, in_the_run
    assert in_the_run[0].startswith("try."), in_the_run
    assert (tmp_path / "folder.mode").read_text().strip() == "drwx------"
    assert (tmp_path / "folder.files").read_text().split() == ["output", "status"]
    assert list((tmp_path / "run").iterdir()) == []


def test_a_step_that_fails_and_prints_nothing_is_tried_again_and_says_so(tmp_path):
    (tmp_path / "fetch").write_text(f'#!/bin/bash\necho x >> "{tmp_path}/fetch.calls"\nexit 3\n', encoding="utf-8")
    (tmp_path / "fetch").chmod(0o755)
    done = run(tmp_path, 'retry_download "The package download" -- fetch\necho "the install goes on"\n')
    # With errexit on, as in the script, the last try that failed stops the script with its status.
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


@pytest.mark.parametrize("download_fails, download_says, build_fails, asked, says, last_line", [
    pytest.param(0, "", 0,
                 ["mod download", "build"], "OK atlas-tui built", "built", id="go-on-the-path-downloads-and-builds"),
    pytest.param(2, "stream error: stream ID 5; INTERNAL_ERROR; received from peer", 0,
                 ["mod download"] * 3 + ["build"], "OK atlas-tui built", "built", id="module-download-tried-again-then-one-build"),
    pytest.param(0, "", 1,
                 ["mod download", "build"], "undefined: missing", "not built", id="compile-error-not-tried-again"),
    pytest.param(99, "dial tcp: lookup proxy.golang.org: i/o timeout", 0,
                 ["mod download"] * 3, "atlas-tui build failed (exit 1)", "not built", id="modules-not-downloaded-build-not-started"),
])
def test_the_tui_build_tries_only_the_module_download_again(tmp_path, download_fails, download_says, build_fails, asked,
                                                            says, last_line):
    # The script's own build step, with a stand-in `go` that writes down what it was asked to do.
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
    # Like sudo, the stand-in for the target user's command gives that command a PATH of its own,
    # which does not hold the stand-in go: the script must run the go it found itself.
    done = run(tmp_path, function("build_atlas_tui") + 'run_as_target() { env PATH=/usr/bin:/bin "$@"; }\n'
               + f'target_home_dir() {{ echo "{tmp_path}/home"; }}\n' + f'ATLAS_INSTALL_DIR="{tmp_path}"\n'
               + 'build_atlas_tui && echo "built" || echo "not built"\n')
    # The stand-in go is on the path only, not under /usr/local/go, and each go command found it.
    assert [line.split(" -o ")[0] for line in (tmp_path / "go.asked").read_text().splitlines()] == asked
    # Each new try of the module download prints one line.
    assert len(retry_lines(done)) == asked.count("mod download") - 1
    assert all("The Go module download failed (exit 1)" in line for line in retry_lines(done))
    assert says in done.stdout
    assert done.stdout.splitlines()[-1] == last_line
    assert (tmp_path / "home" / ".local" / "bin" / "atlas-tui").exists() == (last_line == "built")


def test_only_the_four_download_steps_go_through_the_retry_each_with_the_errors_it_tries_again():
    after_the_helper = TEXT.split("\nretry_download() {", 1)[1]
    assert re.findall(r'retry_download "([^"]+)"', after_the_helper) == [
        "The pip and setuptools download", "The download for the ATLAS CLI install", "The Go download",
        "The Go module download"]
    assert dict(re.findall(r'retry_download "([^"]+)"((?: --[a-z-]+ \S+)*) -- ', after_the_helper)) == {
        "The pip and setuptools download": ' --only-if "$PIP_NETWORK_ERROR" --log "$ATLAS_RUN_DIR/atlas-pip.log"',
        "The download for the ATLAS CLI install": ' --only-if "$PIP_NETWORK_ERROR"',
        "The Go download": ' --not-if "$CURL_NOT_A_NETWORK_ERROR"',
        "The Go module download": ' --not-if "$GO_NOT_A_NETWORK_ERROR"'}
    # The build is not tried again, and it comes after the module download.
    build = re.search(r"^.*'\$go_cmd' build -o '\$out'.*$", TEXT, re.M).group(0)
    assert "retry_download" not in build
    assert TEXT.index("'$go_cmd' mod download\"") < TEXT.index("'$go_cmd' build -o '$out'")


def test_the_wait_between_two_tries_is_kept(tmp_path):
    stand_in(tmp_path, fails=1, says=TIMED_OUT)
    start = time.monotonic()
    done = run(tmp_path, 'retry_download "The package download" -- fetch\n', wait="1")
    assert done.returncode == 0 and "Trying it again in 1s (try 2 of 3)" in done.stdout
    assert 1.0 <= time.monotonic() - start < 15
