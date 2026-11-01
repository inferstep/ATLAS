"""scripts/uninstall.sh names every folder it removes, and refuses an empty setting before it removes anything.

No test here runs the script file, and none reads a real configuration. Each
test runs the script's own functions in bash with every setting pointed at a
folder that the test made under pytest's own folder. Three things keep a test
harmless when the script is wrong:
- the steps that talk to Kubernetes, K3s and helm are stand-ins that only
  write down that they were called;
- a run stops before `main` when a setting points outside the test's folder
  (the cases of a setting that the script must refuse name their value, give
  the answer "no", and use the root folder only as the text "/");
- `rm` is wrapped: a removal outside the test's folder is refused and written
  down, and every test fails when one was tried.
"""
import re
import shutil
import subprocess
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[3] / "scripts" / "uninstall.sh"
TEXT = SCRIPT.read_text(encoding="utf-8")
OWN = ("usage", "print_removals", "not_removable", "check_settings", "parse_args", "confirm", "remove_models", "remove_data", "main")
STAND_INS = ("remove_atlas_services", "remove_container_images", "remove_gpu_operator", "remove_k3s")
# `rm` as the tests give it to the script: a path outside the folder of the test, or one that leaves it through
# `..`, is refused and written down. ROOT is the folder of the test.
NET = """rm() {
    local arg
    for arg in "$@"; do
        case "$arg" in
            -*) ;;
            */..|*/../*) echo "rm $*" >> "$ROOT/refused"; return 97 ;;
            "$ROOT"/*) ;;
            *) echo "rm $*" >> "$ROOT/refused"; return 97 ;;
        esac
    done
    command rm "$@"
}
"""
pytestmark = pytest.mark.skipif(shutil.which("bash") is None, reason="the uninstall script is a bash script")


def function(name):
    found = re.search(rf"^{name}\(\) \{{\n.*?^\}}\n", TEXT, re.M | re.S)
    assert found, f"{name}() is not in scripts/uninstall.sh"
    return found.group(0)


class Install:
    """The folders of an install, made for one test. `apart` puts the projects folder outside the data folder."""

    def __init__(self, root, apart=True):
        self.root = root
        self.data = root / "install" / "data"
        self.projects = root / "my-projects" if apart else self.data / "projects"
        self.models = root / "install" / "models"
        self.beside = root / "install" / "beside"
        for folder in (self.data, self.projects / "app", self.models, self.beside):
            folder.mkdir(parents=True, exist_ok=True)
        for path in (self.data / "state.db", self.projects / "app" / "main.py", self.models / "a.gguf",
                     self.models / "default.gguf", self.models / "notes.txt", self.beside / "keep.txt"):
            path.write_text("made for the test\n", encoding="utf-8")

    def files(self):
        return sorted(str(path.relative_to(self.root)) for path in self.root.rglob("*") if path.is_file()
                      and path.name not in ("called", "refused"))

    def run(self, *options, answer="n", refused=(), **changed):
        """Run the script's own `main` with these options and this answer. `changed` sets a setting to another value;
        None takes it away. `refused` names the values outside the test's folder that the script must refuse."""
        values = {"ATLAS_MODELS_DIR": str(self.models), "ATLAS_DATA_DIR": str(self.data),
                  "ATLAS_PROJECTS_DIR": str(self.projects), **changed}
        settings = "".join(f'{name}="{value}"\n' for name, value in values.items() if value is not None)
        # The lines of the script that set its options and its three folder settings, before the first function.
        defaults = TEXT[TEXT.index("REMOVE_K3S=false\n"):TEXT.index("usage() {\n")]
        script = f"""set -euo pipefail
ROOT="{self.root}"
{NET}read() {{ echo "QUESTION: $2"; builtin read "$@"; }}
log_info() {{ echo "[INFO] $1"; }}
log_warn() {{ echo "[WARN] $1"; }}
log_error() {{ echo "[ERROR] $1" >&2; }}
K8S_DIR="$ROOT/checkout"
HOME="$ROOT/home"
ATLAS_NAMESPACE="atlas"
{settings}{defaults}
for value in "$ATLAS_MODELS_DIR" "$ATLAS_DATA_DIR" "$ATLAS_PROJECTS_DIR"; do
    [[ -z "$value" || "$value" == "$ROOT"/* || "|{"|".join(refused)}|" == *"|$value|"* ]] || {{
        echo "a setting points outside the folder of the test"; exit 99; }}
done
""" + "".join(f'{name}() {{ echo "{name}" >> "$ROOT/called"; }}\n' for name in STAND_INS) + "".join(
            function(name) for name in OWN) + 'main "$@"\n'
        done = subprocess.run(["bash", "-c", script, "uninstall.sh", *options], input=answer, cwd=self.root,
                              env={"PATH": "/usr/bin:/bin"}, capture_output=True, text=True, timeout=20)
        assert done.returncode != 99, done.stdout
        assert not (self.root / "refused").exists(), (
            "the script tried to remove something outside the folders of the test: " + (self.root / "refused").read_text())
        return done

    def called(self):
        record = self.root / "called"
        return record.read_text().split() if record.exists() else []


@pytest.mark.parametrize("path, refused", [
    pytest.param("$ROOT/made/inside.txt", False, id="a file inside the folder"),
    pytest.param("/no-such-folder-of-this-test/x", True, id="a full path outside"),
    pytest.param("$ROOT/made/../../no-such-folder-of-this-test", True, id="out of the folder through .."),
    pytest.param("$ROOT/..", True, id="the folder above"),
    pytest.param("$ROOT", True, id="the folder itself"),
    pytest.param("relative/path", True, id="a path that is not full"),
    pytest.param("", True, id="an empty path"),
])
def test_the_net_of_these_tests_lets_a_removal_through_only_inside_the_folder_of_the_test(tmp_path, path, refused):
    # Each path that must be refused is one that does not exist, so this test removes nothing when the net is wrong.
    (tmp_path / "made").mkdir()
    (tmp_path / "made" / "inside.txt").write_text("made for the test\n", encoding="utf-8")
    done = subprocess.run(["bash", "-c", f'ROOT="{tmp_path}"\n{NET}rm -rf "{path}"\necho "status $?"\n'], cwd=tmp_path,
                          env={"PATH": "/usr/bin:/bin"}, capture_output=True, text=True, timeout=20)
    assert done.stdout.strip() == ("status 97" if refused else "status 0")
    assert (tmp_path / "refused").exists() == refused
    assert (tmp_path / "made" / "inside.txt").exists() == refused


@pytest.mark.parametrize("options, apart, listed", [
    pytest.param((), True, [], id="no option"),
    pytest.param(("--k3s",), True, [], id="--k3s"),
    pytest.param(("--models",), True, ["models"], id="--models"),
    pytest.param(("--data",), True, ["data", "projects"], id="--data"),
    pytest.param(("--data",), False, ["data", "projects"], id="--data, projects folder in the data folder"),
    pytest.param(("--models", "--data"), True, ["models", "data", "projects"], id="--models --data"),
    pytest.param(("--all",), True, ["models", "data", "projects"], id="--all"),
    pytest.param(("--all",), False, ["models", "data", "projects"], id="--all, projects folder in the data folder"),
])
def test_the_list_names_the_folders_of_the_chosen_options_and_no_other(tmp_path, options, apart, listed):
    install = Install(tmp_path, apart)
    before = install.files()
    done = install.run(*options, answer="n")
    assert "QUESTION: " in done.stdout, done.stdout + done.stderr
    lines_before_the_question = done.stdout.split("QUESTION: ")[0].splitlines()
    for name, path in (("models", install.models), ("data", install.data), ("projects", install.projects)):
        lines = [line for line in lines_before_the_question if line.rstrip().endswith(f": {path}")]
        assert len(lines) == (1 if name in listed else 0), (name, lines_before_the_question)
        assert all(f"{name} folder" in line for line in lines), lines
    assert done.returncode == 0
    assert done.stdout.rstrip().endswith("Aborted.")
    assert install.files() == before
    assert install.called() == []


@pytest.mark.parametrize("option, apart, files_left, stand_ins_called", [
    pytest.param("--models", False,
                 ["install/beside/keep.txt", "install/data/projects/app/main.py", "install/data/state.db",
                  "install/models/notes.txt"],
                 ["remove_atlas_services", "remove_container_images"], id="--models, projects folder in the data folder"),
    pytest.param("--data", True,
                 ["install/beside/keep.txt", "install/models/a.gguf", "install/models/default.gguf",
                  "install/models/notes.txt"],
                 ["remove_atlas_services", "remove_container_images"], id="--data"),
    pytest.param("--all", True, ["install/beside/keep.txt", "install/models/notes.txt"],
                 ["remove_atlas_services", "remove_container_images", "remove_gpu_operator", "remove_k3s"], id="--all"),
])
def test_with_the_answer_yes_an_option_removes_what_the_list_named_and_nothing_else(
        tmp_path, option, apart, files_left, stand_ins_called):
    install = Install(tmp_path, apart)
    done = install.run(option, answer="y")
    assert done.returncode == 0, done.stdout + done.stderr
    assert install.files() == files_left
    # The data folder and the projects folder go as a whole: no empty folder is left behind.
    assert [path for path in tmp_path.rglob("*") if path.is_dir() and not any(path.iterdir())] == []
    assert install.called() == stand_ins_called


@pytest.mark.parametrize("answer", ["n", "", "x", "no"], ids=["n", "empty", "x", "no"])
def test_without_the_answer_yes_nothing_is_removed(tmp_path, answer):
    install = Install(tmp_path)
    before = install.files()
    done = install.run("--all", answer=answer)
    assert done.returncode == 0
    assert "Aborted." in done.stdout
    assert install.files() == before
    assert install.called() == []


@pytest.mark.parametrize("value", ["", None], ids=["empty", "not set"])
@pytest.mark.parametrize("option, setting", [
    ("--data", "ATLAS_DATA_DIR"), ("--data", "ATLAS_PROJECTS_DIR"), ("--models", "ATLAS_MODELS_DIR"),
    ("--all", "ATLAS_DATA_DIR"), ("--all", "ATLAS_PROJECTS_DIR"), ("--all", "ATLAS_MODELS_DIR"),
])
def test_an_empty_setting_stops_the_script_before_its_question_and_nothing_is_removed(tmp_path, option, setting, value):
    install = Install(tmp_path)
    before = install.files()
    done = install.run(option, answer="y", **{setting: value})
    assert done.returncode == 1, done.stdout + done.stderr
    assert f"[ERROR] {setting} is empty" in done.stderr
    assert "Nothing was removed." in done.stderr
    assert "Fix: set it in " in done.stderr
    assert "QUESTION" not in done.stdout
    assert install.files() == before
    assert install.called() == []


def test_two_empty_settings_are_both_named(tmp_path):
    install = Install(tmp_path)
    done = install.run("--all", answer="y", ATLAS_DATA_DIR="", ATLAS_MODELS_DIR="")
    assert done.returncode == 1
    assert "[ERROR] ATLAS_DATA_DIR is empty" in done.stderr
    assert "[ERROR] ATLAS_MODELS_DIR is empty" in done.stderr
    assert "ATLAS_PROJECTS_DIR is empty" not in done.stderr


# Each option that removes a folder, with each setting that names a folder it removes.
OPTION_AND_SETTING = [("--data", "ATLAS_DATA_DIR"), ("--data", "ATLAS_PROJECTS_DIR"), ("--models", "ATLAS_MODELS_DIR")]


@pytest.mark.parametrize("option, setting", OPTION_AND_SETTING)
@pytest.mark.parametrize("value, says", [
    ("/", "is the root folder (/)"),
    ("//", "is the root folder (//)"),
    ("my-code", "is not a full path (my-code)"),
    ("./my-code", "is not a full path (./my-code)"),
    ("{root}/install/data/../..", "has a '.' or '..' part"),
    ("{root}/install/./data", "has a '.' or '..' part"),
    ("{root}/home", "is, or holds, your home folder"),
    ("{root}/home/", "is, or holds, your home folder"),
    ("{root}", "is, or holds, your home folder"),
    ("{root}/checkout", "is, or holds, the folder of this repository"),
    ("{root}//checkout/", "is, or holds, the folder of this repository"),
])
def test_a_folder_that_must_not_be_removed_stops_the_script_before_its_question(tmp_path, option, setting, value, says):
    # The home folder and the folder of the repository are stand-ins inside the folder of the test. The answer is
    # "no" in every case, so a script that does not refuse removes nothing either.
    install = Install(tmp_path)
    before = install.files()
    value = value.replace("{root}", str(tmp_path))
    done = install.run(option, answer="n", refused=(value,), **{setting: value})
    assert done.returncode == 1, done.stdout + done.stderr
    assert f"[ERROR] {setting} {says}" in done.stderr, done.stderr
    assert "Nothing was removed." in done.stderr
    assert "Fix: set it in " in done.stderr
    assert "QUESTION" not in done.stdout
    assert install.files() == before
    assert install.called() == []


@pytest.mark.parametrize("option, setting", OPTION_AND_SETTING)
@pytest.mark.parametrize("value", [
    "{root}/checkout/data",
    "{root}/home/projects",
    "{root}/checkout-data",
    "{root}/homework",
    "{root}/check",
    "{root}/hom",
    "{root}/install/data/",
    "{root}//install//data",
])
def test_a_folder_inside_the_home_folder_or_the_repository_is_not_refused(tmp_path, option, setting, value):
    install = Install(tmp_path)
    done = install.run(option, answer="n", **{setting: value.replace("{root}", str(tmp_path))})
    assert done.returncode == 0, done.stdout + done.stderr
    assert "QUESTION" in done.stdout


def test_an_empty_setting_of_an_option_that_was_not_given_does_not_stop_the_script(tmp_path):
    install = Install(tmp_path)
    done = install.run("--models", answer="y", ATLAS_DATA_DIR="", ATLAS_PROJECTS_DIR=None)
    assert done.returncode == 0, done.stdout + done.stderr
    assert not (install.models / "a.gguf").exists()
    assert (install.projects / "app" / "main.py").exists()
    assert install.run(answer="n", ATLAS_DATA_DIR="", ATLAS_PROJECTS_DIR="", ATLAS_MODELS_DIR="").returncode == 0


def test_the_help_text_says_for_data_and_for_all_that_the_projects_folder_is_removed(tmp_path):
    install = Install(tmp_path)
    before = install.files()
    done = install.run("--help")
    assert done.returncode == 0
    assert "QUESTION" not in done.stdout
    options = dict(re.findall(r"^  (--?[a-z0-9-]+(?:, --[a-z-]+)?) +(.*(?:\n {10,}.*)*)", done.stdout, re.M))
    for option in ("--data", "--all"):
        text = " ".join(options[option].split())
        assert "data folder" in text, (option, text)
        assert "projects folder" in text, (option, text)
    assert "projects folder" not in options["--models"]
    assert "projects folder" not in options["--k3s"]
    for name, path in (("Models", install.models), ("Data", install.data), ("Projects", install.projects)):
        assert re.search(rf"^  {name} folder: +{re.escape(str(path))}$", done.stdout, re.M), done.stdout
    assert install.files() == before
    assert install.called() == []


def removals():
    """The lines of the script that remove files, each with its number."""
    return [(number, line) for number, line in enumerate(TEXT.splitlines(), 1)
            if re.match(r"\s*rm\s+-", line)]


def test_every_removal_in_the_script_fails_on_an_empty_value():
    lines = removals()
    assert len(lines) >= 4, lines
    wrong = [f"line {number}: {line.strip()}" for number, line in lines
             if not re.fullmatch(r'\s*rm -r?f "\$\{ATLAS_\w+:\?\}[^$"]*"[^$\s]*', line)]
    assert not wrong, (
        "scripts/uninstall.sh removes a path that can be empty:\n  " + "\n  ".join(wrong) + "\nWith an empty setting "
        'such a line removes another folder than the one meant. Fix: write the setting as "${NAME:?}", which stops '
        "the script when it is empty.")


def test_each_setting_that_a_removal_uses_is_in_the_list_and_in_the_check_of_empty_settings():
    removed = set(re.findall(r"\$\{(ATLAS_\w+):\?\}", "\n".join(line for _number, line in removals())))
    assert removed == {"ATLAS_MODELS_DIR", "ATLAS_DATA_DIR", "ATLAS_PROJECTS_DIR"}, removed
    # check_settings() looks at the settings that it puts in `names`. Its message names every setting too, so a
    # search of the whole function would find a setting that it no longer looks at.
    checked = " ".join(re.findall(r'names="([^"]*)"', function("check_settings"))).split()
    for name in sorted(removed):
        assert f"${name}" in function("print_removals"), (
            f"the script removes ${{{name}}} and print_removals() does not print it. Fix: add a line to the list "
            "that names the folder and prints its path.")
        assert name in checked, (
            f"the script removes ${{{name}}} and check_settings() does not look at it. Fix: add it under the option "
            "that removes it.")
    main = function("main")
    assert main.index("check_settings") < main.index("print_removals") < main.index("confirm") < main.index("remove_"), (
        "main() does not run check_settings, print_removals and the question in that order before the first removal.")
