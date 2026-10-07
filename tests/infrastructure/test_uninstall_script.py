"""scripts/uninstall.sh names every folder it removes, and refuses an empty setting before it removes anything.

No test here runs the script file, and none reads a real configuration. Each
test runs the script's own functions in bash with every setting pointed at a
folder that the test made under pytest's own folder. Three things keep a test
harmless when the script is wrong:
- the steps that talk to Kubernetes, K3s and helm are stand-ins that only
  write down that they were called;
- a run stops before `main` when a setting points outside the test's folder;
- `rm` is wrapped: a removal outside the test's folder is refused and written
  down, and every test fails when one was tried.
"""
import re
import shutil
import subprocess
from pathlib import Path

import pytest

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "uninstall.sh"
TEXT = SCRIPT.read_text(encoding="utf-8")
SETTINGS = ("ATLAS_MODELS_DIR", "ATLAS_DATA_DIR", "ATLAS_PROJECTS_DIR")
OWN = ("usage", "print_removals", "check_settings", "parse_args", "confirm", "remove_models", "remove_data", "main")
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


def defaults():
    """The lines of the script that set its options and its three folder settings, before the first function."""
    return TEXT[TEXT.index("REMOVE_K3S=false\n"):TEXT.index("usage() {\n")]


class Install:
    """The folders of an install, made for one test. `apart` puts the projects folder outside the data folder."""

    def __init__(self, root, apart):
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

    def run(self, *options, answer="n", **changed):
        """Run the script's own `main` with these options and this answer. `changed` sets a setting to another value;
        None takes it away."""
        values = {"ATLAS_MODELS_DIR": str(self.models), "ATLAS_DATA_DIR": str(self.data),
                  "ATLAS_PROJECTS_DIR": str(self.projects), **changed}
        settings = "".join(f'{name}="{value}"\n' for name, value in values.items() if value is not None)
        script = f"""set -euo pipefail
ROOT="{self.root}"
{NET}read() {{ echo "QUESTION: $2"; builtin read "$@"; }}
log_info() {{ echo "[INFO] $1"; }}
log_warn() {{ echo "[WARN] $1"; }}
log_error() {{ echo "[ERROR] $1" >&2; }}
K8S_DIR="$ROOT/checkout"
ATLAS_NAMESPACE="atlas"
{settings}{defaults()}
for value in "$ATLAS_MODELS_DIR" "$ATLAS_DATA_DIR" "$ATLAS_PROJECTS_DIR"; do
    [[ -z "$value" || "$value" == "$ROOT"/* ]] || {{ echo "a setting points outside the folder of the test"; exit 99; }}
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
    ("$ROOT/made/inside.txt", False),
    ("/no-such-folder-of-this-test/x", True),
    ("$ROOT/made/../../no-such-folder-of-this-test", True),
    ("$ROOT/..", True),
    ("$ROOT", True),
    ("relative/path", True),
    ("", True),
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


def the_list(done):
    """The lines that the script prints before its question."""
    assert "QUESTION: " in done.stdout, done.stdout + done.stderr
    return done.stdout.split("QUESTION: ")[0].splitlines()


def lines_with(lines, path):
    return [line for line in lines if line.rstrip().endswith(f": {path}")]


@pytest.mark.parametrize("apart", [False, True])
@pytest.mark.parametrize("option", ["--data", "--all"])
def test_the_list_names_the_data_folder_and_the_projects_folder_with_their_paths_before_the_question(tmp_path, apart, option):
    install = Install(tmp_path, apart)
    before = install.files()
    done = install.run(option, answer="n")
    listed = the_list(done)
    assert len(lines_with(listed, install.data)) == 1, listed
    assert len(lines_with(listed, install.projects)) == 1, listed
    assert "projects folder" in lines_with(listed, install.projects)[0]
    assert done.returncode == 0 and done.stdout.rstrip().endswith("Aborted.")
    assert install.files() == before and install.called() == []


@pytest.mark.parametrize("options, named", [
    ((), ()),
    (("--k3s",), ()),
    (("--models",), ("models",)),
    (("--data",), ("data", "projects")),
    (("--models", "--data"), ("models", "data", "projects")),
    (("--all",), ("models", "data", "projects")),
])
def test_the_list_names_the_folders_of_the_chosen_options_and_no_other(tmp_path, options, named):
    install = Install(tmp_path, apart=True)
    listed = the_list(install.run(*options, answer="n"))
    for name in ("models", "data", "projects"):
        assert len(lines_with(listed, getattr(install, name))) == (1 if name in named else 0), (name, listed)


def test_with_the_answer_yes_data_removes_the_two_folders_that_the_list_named_and_nothing_else(tmp_path):
    install = Install(tmp_path, apart=True)
    done = install.run("--data", answer="y")
    assert done.returncode == 0, done.stdout + done.stderr
    assert not install.data.exists() and not install.projects.exists()
    assert install.files() == ["install/beside/keep.txt", "install/models/a.gguf", "install/models/default.gguf",
                               "install/models/notes.txt"]
    assert install.called() == ["remove_atlas_services", "remove_container_images"]


def test_with_the_answer_yes_models_removes_the_model_files_and_leaves_the_folder_and_the_rest(tmp_path):
    install = Install(tmp_path, apart=False)
    done = install.run("--models", answer="y")
    assert done.returncode == 0, done.stdout + done.stderr
    assert install.files() == ["install/beside/keep.txt", "install/data/projects/app/main.py", "install/data/state.db",
                               "install/models/notes.txt"]


def test_with_the_answer_yes_all_removes_what_data_and_models_remove(tmp_path):
    install = Install(tmp_path, apart=True)
    done = install.run("--all", answer="y")
    assert done.returncode == 0, done.stdout + done.stderr
    assert install.files() == ["install/beside/keep.txt", "install/models/notes.txt"]
    assert install.called() == list(STAND_INS)


@pytest.mark.parametrize("answer", ["n", "", "x", "no"])
def test_without_the_answer_yes_nothing_is_removed(tmp_path, answer):
    install = Install(tmp_path, apart=True)
    before = install.files()
    done = install.run("--all", answer=answer)
    assert done.returncode == 0 and "Aborted." in done.stdout
    assert install.files() == before and install.called() == []


@pytest.mark.parametrize("value", ["", None])
@pytest.mark.parametrize("option, setting", [
    ("--data", "ATLAS_DATA_DIR"), ("--data", "ATLAS_PROJECTS_DIR"), ("--models", "ATLAS_MODELS_DIR"),
    ("--all", "ATLAS_DATA_DIR"), ("--all", "ATLAS_PROJECTS_DIR"), ("--all", "ATLAS_MODELS_DIR"),
])
def test_an_empty_setting_stops_the_script_before_its_question_and_nothing_is_removed(tmp_path, option, setting, value):
    install = Install(tmp_path, apart=True)
    before = install.files()
    done = install.run(option, answer="y", **{setting: value})
    assert done.returncode == 1, done.stdout + done.stderr
    assert f"[ERROR] {setting} is empty" in done.stderr and "Nothing was removed." in done.stderr
    assert "Fix: set it in " in done.stderr
    assert "QUESTION" not in done.stdout
    assert install.files() == before and install.called() == []


def test_two_empty_settings_are_both_named(tmp_path):
    install = Install(tmp_path, apart=True)
    done = install.run("--all", answer="y", ATLAS_DATA_DIR="", ATLAS_MODELS_DIR="")
    assert done.returncode == 1
    assert "[ERROR] ATLAS_DATA_DIR is empty" in done.stderr and "[ERROR] ATLAS_MODELS_DIR is empty" in done.stderr
    assert "ATLAS_PROJECTS_DIR is empty" not in done.stderr


def test_an_empty_setting_of_an_option_that_was_not_given_does_not_stop_the_script(tmp_path):
    install = Install(tmp_path, apart=True)
    done = install.run("--models", answer="y", ATLAS_DATA_DIR="", ATLAS_PROJECTS_DIR=None)
    assert done.returncode == 0, done.stdout + done.stderr
    assert not (install.models / "a.gguf").exists() and (install.projects / "app" / "main.py").exists()
    assert install.run(answer="n", ATLAS_DATA_DIR="", ATLAS_PROJECTS_DIR="", ATLAS_MODELS_DIR="").returncode == 0


def test_the_help_text_says_for_data_and_for_all_that_the_projects_folder_is_removed(tmp_path):
    install = Install(tmp_path, apart=True)
    before = install.files()
    done = install.run("--help")
    assert done.returncode == 0 and "QUESTION" not in done.stdout
    options = dict(re.findall(r"^  (--?[a-z0-9-]+(?:, --[a-z-]+)?) +(.*(?:\n {10,}.*)*)", done.stdout, re.M))
    for option in ("--data", "--all"):
        text = " ".join(options[option].split())
        assert "data folder" in text and "projects folder" in text, (option, text)
    assert "projects folder" not in options["--models"] and "projects folder" not in options["--k3s"]
    for name, path in (("Models", install.models), ("Data", install.data), ("Projects", install.projects)):
        assert re.search(rf"^  {name} folder: +{re.escape(str(path))}$", done.stdout, re.M), done.stdout
    assert install.files() == before and install.called() == []


def removals():
    """The lines of the script that remove files, each with its number."""
    return [(number, line) for number, line in enumerate(TEXT.splitlines(), 1)
            if re.match(r"\s*rm\s+-", line)]


def test_every_removal_in_the_script_fails_on_an_empty_value():
    assert len(removals()) >= 4, removals()
    wrong = [f"line {number}: {line.strip()}" for number, line in removals()
             if not re.fullmatch(r'\s*rm -r?f "\$\{ATLAS_\w+:\?\}[^$"]*"[^$\s]*', line)]
    assert not wrong, (
        "scripts/uninstall.sh removes a path that can be empty:\n  " + "\n  ".join(wrong) + "\nWith an empty setting "
        'such a line removes another folder than the one meant. Fix: write the setting as "${NAME:?}", which stops '
        "the script when it is empty.")


def test_each_setting_that_a_removal_uses_is_in_the_list_and_in_the_check_of_empty_settings():
    removed = set(re.findall(r"\$\{(ATLAS_\w+):\?\}", "\n".join(line for _number, line in removals())))
    assert removed == set(SETTINGS), removed
    for name in sorted(removed):
        assert f"${name}" in function("print_removals"), (
            f"the script removes ${{{name}}} and print_removals() does not print it. Fix: add a line to the list "
            "that names the folder and prints its path.")
        assert f'-n "${name}"' in function("check_settings"), (
            f"the script removes ${{{name}}} and check_settings() does not refuse it when it is empty. Fix: add it "
            "under the option that removes it.")
    main = function("main")
    assert main.index("check_settings") < main.index("print_removals") < main.index("confirm") < main.index("remove_"), (
        "main() does not run check_settings, print_removals and the question in that order before the first removal.")
