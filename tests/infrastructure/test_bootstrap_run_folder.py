"""The installer keeps its downloads and logs in one private temporary folder.

Some tests read the script. The others run the start of the script as it is
(up to the line that sets the end of the run) and then one of its own
functions, with a stand-in for each command that would download or install.
Every run has a time limit, and a run stops at once when a stand-in is not
the first of its name on the path.
"""
import os
import re
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

from tests.infrastructure.test_bootstrap_download_retry import TEXT, function, setting

pytestmark = pytest.mark.skipif(shutil.which("bash") is None, reason="the bootstrap script is a bash script")

MADE_AND_END_SET = "\nmake_run_dir\ntrap end_of_run EXIT\n"
KEPT = "    The downloads and logs of this run are kept in "
SHARED = r"/(?:var/tmp|tmp|dev/shm)"
TMPDIR = r"(?:\$TMPDIR|\$\{TMPDIR(?::?-[^}]*)?\})"
FIXED_NAME = re.compile(rf"""(?<![\w$}}.-]){SHARED}/[^\s"'`;|&)]+|{TMPDIR}/[\w.-]+""")
TEMPLATE = re.compile(r"""mktemp -d "[^\s"]*X{6,}\"""")
SHARED_ITSELF = re.compile(rf"""\b\w+=["']?(?:{SHARED}|{TMPDIR})["']?\s*(?:$|;|\)|&&|\|\|)""")
IN_THE_FOLDER = re.compile(r"\$\{?ATLAS_RUN_DIR\}?/([\w-]+(?:\.[\w-]+)*)")
SET_TO_A_FILE = r'(\w+)="\$ATLAS_RUN_DIR/([\w.-]+)"'
MESSAGE = re.compile(r"^\s*(?:log_(?:step|info|ok|warn|err|skip)|die|echo)\b")
WRITES = re.compile(r"(?:>>?|\btee(?: -a)?|\s-o|--log)\s*\"?\$")


def code_lines(text=TEXT):
    """The lines that are not comments, each with its number."""
    return [(number, line) for number, line in enumerate(text.splitlines(), 1) if not line.lstrip().startswith("#")]


def functions():
    """The name and the text of each function of the script."""
    return re.findall(r"^([a-z_0-9]+)\(\) \{\n(.*?)^\}\n", TEXT, re.M | re.S)


def names_in(line, variables):
    """The files of the run's folder that a line names, directly or through a variable set to one."""
    named = set(IN_THE_FOLDER.findall(line))
    return named | {name for variable, name in variables.items() if re.search(rf"\$\{{?{variable}\b", line)}


def start():
    """The script as it is, from its first line to the line that sets the end of the run."""
    assert MADE_AND_END_SET in TEXT, (
        "scripts/atlas-bootstrap.sh does not have the line `make_run_dir` with the line `trap end_of_run EXIT` after "
        "it. Fix: make the folder of the run at the top level, and set the end of the run in the next line.")
    return TEXT[:TEXT.index(MADE_AND_END_SET)] + MADE_AND_END_SET


def run(folder, body, first="", **settings):
    (folder / "tmp").mkdir(exist_ok=True)
    env = {"PATH": f"{folder}:/usr/bin:/bin", "TMPDIR": str(folder / "tmp"), "ATLAS_DOWNLOAD_WAIT_SECONDS": "0", **settings}
    return subprocess.run(["bash", "-c", first + start() + body], cwd=folder, env=env, capture_output=True, text=True, timeout=20)


def folders(folder):
    return sorted((folder / "tmp").glob("atlas-bootstrap.*"))


def stand_in(folder, name, text):
    """A command of this name that is first on the path. Returns the lines that stop the run when it is not."""
    (folder / name).write_text("#!/bin/bash\n" + text, encoding="utf-8")
    (folder / name).chmod(0o755)
    return (f'[[ "$(command -v {name})" == "{folder}/{name}" ]] || '
            f'{{ echo "the stand-in for {name} is not the first on the path"; exit 99; }}\n')


def paths_given(done, folder):
    """Each path inside the run's folder that the output of a run gives."""
    found = re.findall(re.escape(str(folder / "tmp")) + r"/atlas-bootstrap\.[\w.]+/[\w-]+(?:\.[\w-]+)*", done.stdout + done.stderr)
    return sorted(set(found))


def test_no_line_of_the_script_names_a_file_directly_in_the_shared_temporary_folder():
    # The name that mktemp makes from a template is new each time, so a template is not a fixed name.
    found = [f"line {number}: {line.strip()}" for number, line in code_lines()
             if FIXED_NAME.search(TEMPLATE.sub("mktemp -d", line)) or SHARED_ITSELF.search(line)]
    assert not found, (
        "scripts/atlas-bootstrap.sh names a file directly in the shared temporary folder, or sets a variable to that "
        "folder:\n  " + "\n  ".join(found) + "\nEvery download and every log of a run belongs in the folder that "
        'the run makes for itself. Fix: write "$ATLAS_RUN_DIR/<name>" in each of these lines.')


def test_a_path_that_is_built_from_the_folder_has_a_slash_after_it():
    # After the folder's variable comes a slash, or nothing more of the same word (the folder itself is meant).
    uses = re.compile(r"""(?:\$ATLAS_RUN_DIR\b|\$\{ATLAS_RUN_DIR(?::[-?][^}]*)?\})(?![/"\s]|$)""")
    found = [f"line {number}: {line.strip()}" for number, line in code_lines() if uses.search(line)]
    assert not found, (
        "scripts/atlas-bootstrap.sh builds a name from the folder of the run with no slash after it:\n  "
        + "\n  ".join(found) + "\nSuch a file stands beside the folder, directly in the shared temporary folder. "
        'Fix: write "$ATLAS_RUN_DIR/<name>".')


def test_the_folder_is_made_by_mktemp_at_the_start_and_everything_else_runs_after_it():
    assert re.search(r"""^    ATLAS_RUN_DIR=\$\(mktemp -d "\$\{TMPDIR:-/tmp\}/[\w.-]*X{6,}"\) \\$""", function("make_run_dir"), re.M), (
        "make_run_dir() does not make the folder with `mktemp -d \"${TMPDIR:-/tmp}/<name>.XXXXXX\"`. Fix: let mktemp "
        "make the folder; it gives a new name each run and mode 700.")
    calls = [line.split()[0] for _number, line in code_lines() if re.match(r"(?:make_run_dir|trap|main)\b(?!\(\))", line)]
    assert calls == ["make_run_dir", "trap", "main"], (
        f"the calls at the top level of the script come as {calls}. Fix: call make_run_dir once, set the end of the run "
        "in the line after it, and call main at the end.")
    used = [f"line {number}: {line.strip()}" for number, line in code_lines(start().split(MADE_AND_END_SET)[0])
            if re.match(r"\S", line) and not re.match(r"[a-z_0-9]+\(\) +\{|\}|set -|if |else|fi\b", line)]
    assert not used, (
        "a command runs at the top level before the run's folder is made:\n  " + "\n  ".join(used)
        + "\nFix: move it below the line `make_run_dir`, so that it cannot write a file before the folder is there.")


def test_the_folder_of_a_run_is_new_each_run_and_has_mode_700(tmp_path):
    seen = []
    for _ in range(2):
        done = run(tmp_path, 'echo "made $ATLAS_RUN_DIR"\n: > "$ATLAS_RUN_DIR/a-log"\nexit 3\n')
        assert done.returncode == 3, done.stdout + done.stderr
        seen.append(done.stdout.splitlines()[0][len("made "):])
    assert seen[0] != seen[1]
    assert [str(path) for path in folders(tmp_path)] == sorted(seen)
    for path in folders(tmp_path):
        about = path.stat()
        assert stat.S_ISDIR(about.st_mode), oct(about.st_mode)
        assert stat.S_IMODE(about.st_mode) == 0o700, oct(about.st_mode)
        assert about.st_uid == os.getuid()
        assert path.parent == tmp_path / "tmp"


def test_an_install_that_passed_removes_the_folder_and_gives_no_path(tmp_path):
    done = run(tmp_path, 'echo "a line" > "$ATLAS_RUN_DIR/atlas-pip.log"\nmkdir "$ATLAS_RUN_DIR/try.1"\nlog_ok "ATLAS CLI installed"\n')
    assert done.returncode == 0, done.stdout + done.stderr
    assert folders(tmp_path) == []
    assert str(tmp_path / "tmp") not in done.stdout + done.stderr


@pytest.mark.parametrize("ends, status", [("exit 3", 3), ('die "The step failed."', 1), ("false", 1)])
def test_an_install_that_failed_keeps_the_folder_and_the_last_line_gives_its_path(tmp_path, ends, status):
    done = run(tmp_path, f'echo "a line" > "$ATLAS_RUN_DIR/atlas-pip.log"\n{ends}\necho "the install goes on"\n')
    assert done.returncode == status
    assert "the install goes on" not in done.stdout
    (kept,) = folders(tmp_path)
    assert done.stdout.splitlines()[-1] == f"{KEPT}{kept}"
    assert (kept / "atlas-pip.log").read_text() == "a line\n"


@pytest.mark.parametrize("number, started_by, says_so", [("0", "dev", True), ("0", "root", False), ("0", "", False), ("1000", "dev", False)])
def test_the_last_line_says_that_the_folder_belongs_to_root_only_after_a_run_through_sudo(tmp_path, number, started_by, says_so):
    # The number of the user is a stand-in: the script asks `id -u`, and the test answers for it.
    done = run(tmp_path, 'echo "a line" > "$ATLAS_RUN_DIR/atlas-pip.log"\nexit 3\n', first=f"id() {{ echo {number}; }}\n",
               SUDO_USER=started_by)
    (kept,) = folders(tmp_path)
    note = " (the folder belongs to root: read its logs with sudo)" if says_so else ""
    assert done.returncode == 3
    assert done.stdout.splitlines()[-1] == f"{KEPT}{kept}{note}"


def test_a_run_that_failed_before_it_wrote_a_file_leaves_no_folder_and_gives_no_path(tmp_path):
    done = run(tmp_path, 'log_err "Unsupported distro."\nexit 2\n')
    assert done.returncode == 2
    assert folders(tmp_path) == []
    assert KEPT not in done.stdout
    assert str(tmp_path / "tmp") not in done.stdout + done.stderr


@pytest.mark.parametrize("says", [
    'log_warn "The build failed. Log: $ATLAS_RUN_DIR/a.log"',
    'log_err "The pull failed. Log: $ATLAS_RUN_DIR/a.log"',
    'log_info "The output is also in $ATLAS_RUN_DIR/a.log."',
    'log_ok "Done (log: $ATLAS_RUN_DIR/a.log)"',
    'log_step "See $ATLAS_RUN_DIR/a.log"',
    'log_skip "Skipped; see $ATLAS_RUN_DIR/a.log"',
    '( log_warn "The build failed. Log: $ATLAS_RUN_DIR/a.log" )',
    'log_warn "The build failed. Log: $ATLAS_RUN_DIR/a.log" | cat',
])
def test_a_message_that_names_a_file_of_the_folder_keeps_the_folder_though_the_install_passed(tmp_path, says):
    done = run(tmp_path, f'echo "a line" > "$ATLAS_RUN_DIR/a.log"\n{says}\necho "the install goes on"\n')
    assert done.returncode == 0, done.stdout + done.stderr
    (kept,) = folders(tmp_path)
    assert paths_given(done, tmp_path) == [str(kept / "a.log")]
    assert (kept / "a.log").read_text() == "a line\n"
    assert done.stdout.splitlines()[-2:] == ["the install goes on", f"{KEPT}{kept}"]


def failing_step(tmp_path, step):
    """Run one step of the script so that it fails, with stand-ins. Returns the run and the name of the log it must give."""
    stubs = ('run_as_target() { "$@"; }\n' + f'target_home_dir() {{ echo "{tmp_path}/home"; }}\n' + f'ATLAS_INSTALL_DIR="{tmp_path}"\n'
             + 'DOCKER_PREFIX=""\nGPU_VENDOR="nvidia"\ncompose_files_args() { echo "-f /no-such-folder/compose.yml"; }\n')
    helper = function("retry_download") + setting("PIP_NETWORK_ERROR") + setting("GO_NOT_A_NETWORK_ERROR")
    if step == "the TUI build":
        (tmp_path / "tui").mkdir()
        (tmp_path / "home").mkdir()
        guard = stand_in(tmp_path, "go", '[ "$1" = build ] && { echo "./main.go:9:2: undefined: missing" >&2; exit 1; }\nexit 0\n')
        return run(tmp_path, guard + helper + stubs + function("build_atlas_tui") + "build_atlas_tui || die 'atlas-tui build failed.'\n"), "atlas-tui-build.log"
    if step == "the CLI install":
        guard = stand_in(tmp_path, "python3", 'case "$*" in *"-e ."*) echo "error: invalid command bdist_wheel" >&2; exit 1 ;; esac\nexit 0\n')
        return run(tmp_path, guard + helper + stubs + function("install_atlas_cli") + "install_atlas_cli || die 'ATLAS CLI installation failed.'\n"), "atlas-pip.log"
    if step == "the model download":
        (tmp_path / "scripts").mkdir()
        (tmp_path / "scripts" / "download-models.sh").write_text('#!/bin/bash\necho "curl: (28) Operation timed out"\nexit 28\n', encoding="utf-8")
        (tmp_path / "scripts" / "download-models.sh").chmod(0o755)
        return run(tmp_path, stubs + function("download_models") + "download_models\n"), "atlas-models.log"
    fails = "pull" if step == "the image pull" else "up"
    guard = stand_in(tmp_path, "docker", f'case "$*" in *" {fails}"*) echo "Error response from daemon: denied" >&2; exit 1 ;; esac\nexit 0\n')
    log = "atlas-compose-pull.log" if step == "the image pull" else "atlas-compose.log"
    return run(tmp_path, guard + stubs + function("start_compose") + "start_compose\n"), log


@pytest.mark.parametrize("step", ["the TUI build", "the CLI install", "the model download", "the image pull", "the start of the containers"])
def test_the_path_that_a_failed_step_gives_is_a_file_that_is_there_after_the_run(tmp_path, step):
    done, log = failing_step(tmp_path, step)
    assert done.returncode == 1, done.stdout + done.stderr
    assert "is not the first on the path" not in done.stdout
    (kept,) = folders(tmp_path)
    assert str(kept / log) in paths_given(done, tmp_path), done.stdout + done.stderr
    for given in paths_given(done, tmp_path):
        assert Path(given).is_file(), f"{given} is named in the output and is not there"
        assert Path(given).stat().st_size > 0, f"{given} is named in the output and is empty"
    assert done.stdout.splitlines()[-1] == f"{KEPT}{kept}"
    assert sorted(path.name for path in (tmp_path / "tmp").iterdir()) == [kept.name]


def test_a_message_names_only_a_file_that_its_own_function_writes():
    wrong = []
    for name, body in functions():
        variables = dict(re.findall(SET_TO_A_FILE, body))
        lines = [line for _number, line in code_lines(body)]
        written = set().union(*(names_in(line, variables) for line in lines if WRITES.search(line)))
        named = set().union(*(names_in(line, variables) for line in lines if MESSAGE.match(line)))
        wrong += [f"{name}() names {file} in a message and does not write it" for file in sorted(named - written)]
    assert not wrong, (
        "a message of scripts/atlas-bootstrap.sh gives the path of a file that its function does not write:\n  "
        + "\n  ".join(wrong) + "\nFix: name the file the step writes its output to, with the same words in both lines.")


def test_only_a_step_that_failed_names_a_file_so_an_install_that_passed_can_remove_the_folder():
    named = []
    for name, body in functions():
        variables = dict(re.findall(SET_TO_A_FILE, body))
        named += [f"{name}(): {line.strip()}" for _number, line in code_lines(body)
                  if re.match(r"\s*log_(?:step|info|ok|skip)\b", line) and names_in(line, variables)]
    assert not named, (
        "a message of a step that passed names a file of the run's folder:\n  " + "\n  ".join(named) + "\nA message "
        "that names a file keeps the folder, so every install would leave its folder behind. Fix: take the path out "
        "of the message; a step that fails names its log in a warning or an error.")


def test_the_download_of_the_docker_installer_takes_https_only():
    # The line is found by the file it writes, the installer in the folder of the run.
    lines = [line.strip() for _number, line in code_lines() if "curl " in line and '-o "$ATLAS_RUN_DIR/get-docker.sh"' in line]
    assert len(lines) == 1, lines
    wrong = [flag for flag in ("--proto '=https'", "--tlsv1.2") if flag not in lines[0].split(" https://")[0]]
    assert not wrong, (
        f"the line that downloads the Docker installer does not have {' and '.join(wrong)}: {lines[0]}\nWithout them "
        "curl follows a redirect to a plain HTTP address. Fix: give curl both flags before the address.")
