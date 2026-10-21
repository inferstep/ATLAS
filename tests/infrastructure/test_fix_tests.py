"""The check that runs a fix's new tests without the fix.

Each case is a small repository made for the test: a base commit with a
fault, and a head commit with the fix and its tests. Go is a stand-in that
answers as `go test -json` does, so no Go toolchain is needed here.
"""
import importlib.util
import json
import shlex
import subprocess
import sys
import venv
from pathlib import Path

import pytest
import yaml

SCRIPT = Path(__file__).resolve().parents[2] / "scripts" / "fix_tests.py"
PATH_LINE = "import pathlib, sys\nsys.path.insert(0, str(pathlib.Path(__file__).resolve().parents[1]))\n"
FAULTY = "def add(a, b):\n    return a - b\n"
FIXED = "def add(a, b):\n    return a + b\n\n\ndef double(a):\n    return add(a, a)\n"
OLD_TEST = PATH_LINE + "import calc\n\n\ndef test_add_takes_two_numbers():\n    assert calc.add(0, 0) == 0\n"
# Answers as `go test -json` does. Like Go, it builds the tests of a folder
# together: a test file that calls a function no other file defines stops all.
GO_STAND_IN = """import json, pathlib, re, sys
here = pathlib.Path(".")
tests = "".join(p.read_text() for p in sorted(here.glob("*_test.go")))
code = "".join(p.read_text() for p in sorted(here.glob("*.go")) if not p.name.endswith("_test.go"))
undefined = [name for name in sorted(set(re.findall(r"\\b(new\\w+)\\(", tests))) if "func " + name + "(" not in code]
if undefined:
    print(json.dumps({"Action": "build-output", "Output": "./x_test.go:6:5: undefined: " + undefined[0] + "\\n"}))
    print(json.dumps({"Action": "output", "Package": "p", "Output": "FAIL\\tp [build failed]\\n"}))
    sys.exit(1)
told = json.loads(pathlib.Path("go_answers.json").read_text())
names = re.search(r"\\^\\((.*)\\)\\$", sys.argv[sys.argv.index("-run") + 1]).group(1).split("|")
failed = False
for name in names:
    action = told.get(name, "pass")
    failed |= action in ("fail", "panic", "subfail", "silent")
    if action == "subfail":
        print(json.dumps({"Action": "output", "Package": "p", "Test": name + "/a_case", "Output": "    x_test.go:12: status = 3, want an active vector\\n"}))
        print(json.dumps({"Action": "fail", "Package": "p", "Test": name + "/a_case", "Elapsed": 0.01}))
        print(json.dumps({"Action": "output", "Package": "p", "Test": name, "Output": "--- FAIL: " + name + " (0.00s)\\n"}))
    if action == "silent":
        print(json.dumps({"Action": "output", "Package": "p", "Test": name, "Output": "--- FAIL: " + name + " (0.00s)\\n"}))
    said = {"fail": "    x_test.go:9: want 5, got -1\\n", "skip": "    x_test.go:4: needs a terminal\\n",
            "panic": "panic: runtime error: index out of range [recovered]\\n\\t/src/runtime/panic.go:1\\n"
                     "\\t/src/proxy/calc.go:7 +0x1c\\n\\t/src/proxy/x_test.go:9 +0x2\\n"}.get(action)
    if said:
        print(json.dumps({"Action": "output", "Package": "p", "Test": name, "Output": said}))
    ended = "fail" if action in ("panic", "subfail", "silent") else action
    print(json.dumps({"Action": ended, "Package": "p", "Test": name, "Elapsed": 0.01}))
sys.exit(1 if failed else 0)
"""
# The stand-in is two files. The first line of the one on the path is fixed: a line that named the path of Python
# would not start where that path holds a space.
ON_THE_PATH = '#!/bin/sh\nexec {python} -S {code} "$@"\n'


@pytest.fixture(scope="module")
def fix():
    spec = importlib.util.spec_from_file_location("atlas_fix_tests", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


@pytest.fixture(autouse=True)
def no_job_summary(monkeypatch):
    """A test here does not write into the summary of the job that runs it."""
    monkeypatch.delenv("GITHUB_STEP_SUMMARY", raising=False)


def git(root, *args):
    return subprocess.run(["git", "-C", str(root), "-c", "user.name=t", "-c", "user.email=t@example.invalid", *args],
                          capture_output=True, text=True, check=True).stdout.strip()


def commit(root, files, message):
    for path, text in files.items():
        (root / path).parent.mkdir(parents=True, exist_ok=True)
        (root / path).write_text(text, encoding="utf-8")
    git(root, "add", "-A")
    git(root, "commit", "-q", "-m", message)
    return git(root, "rev-parse", "HEAD")


@pytest.fixture
def repo(tmp_path):
    """A repository with a faulty base. Call it with the files of the fix; it returns the root and the base commit."""
    root = tmp_path / "repo"
    root.mkdir()
    git(root, "init", "-q")
    base = commit(root, {"proxy/calc.py": FAULTY, "proxy/tests/test_calc.py": OLD_TEST,
                         "proxy/go.mod": "module p\n", "proxy/calc.go": "package p\n",
                         "proxy/go_answers.json": json.dumps({"TestAdds": "fail", "TestOldCode": "fail"})}, "base")

    def with_fix(files):
        commit(root, files, "fix")
        return root, base
    return with_fix


def go_stand_in(folder, python=sys.executable):
    """A `go` for the tests, in this folder and run by this Python: the path of the file to call."""
    code = folder / "stand-ins" / "go.py"
    code.parent.mkdir()
    code.write_text(GO_STAND_IN, encoding="utf-8")
    go = folder / "bin" / "go"
    go.parent.mkdir()
    go.write_text(ON_THE_PATH.format(python=shlex.quote(python), code=shlex.quote(str(code))), encoding="utf-8")
    go.chmod(0o755)
    return str(go)


@pytest.fixture
def tools(tmp_path):
    return {"python": sys.executable, "go": go_stand_in(tmp_path)}


def new_test(body):
    return OLD_TEST + "\n\n" + body


def verdicts(fix, root, base, tools):
    rows, notes, product = fix.judge(root, base, tools, 120)
    return {test.name: name for test, name, _ in rows}, rows, notes, product


GO_FIXED = {"proxy/calc.go": "package p\n\nfunc newHelper() int { return 5 }\n", "proxy/go_answers.json": "{}"}


def go_test(name, body):
    return f"package p\n\nimport \"testing\"\n\nfunc {name}(t *testing.T) {{\n\t{body}\n}}\n"


@pytest.mark.parametrize("fix_files, test_name, its_class, reason", [
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
                  "def test_add_adds():\n    assert calc.add(2, 3) == 5\n")},
                 "test_add_adds", "fails on the base", "it guards the fix: an assertion, raised in the test: assert",
                 id="fails by an assertion on the base"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
                  "def test_add_gives_a_number():\n    assert isinstance(calc.add(2, 3), int)\n")},
                 "test_add_gives_a_number", "passes on the base too", "it does not guard the fix",
                 id="passes on the base too"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
                  "def test_double_doubles():\n    assert calc.double(4) == 8\n")},
                 "test_double_doubles", "needs the fix's code",
                 "on the base the test cannot run as written: AttributeError", id="calls a function only the fix adds"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_double.py":
                  PATH_LINE + "from calc import double\n\n\ndef test_double_doubles():\n    assert double(4) == 8\n"},
                 "test_double_doubles", "needs the fix's code", "on the base the test cannot run as written",
                 id="a new test file that does not import on the base"),
    pytest.param({"proxy/calc.py": "def add(a, b, round_to=None):\n    return a + b\n",
                  "proxy/tests/test_calc.py": new_test(
                      "def test_add_takes_a_rounding():\n    assert calc.add(2, 3, round_to=0) == 5\n")},
                 "test_add_takes_a_rounding", "needs the fix's code",
                 "on the base the test cannot run as written: TypeError: add() got an unexpected keyword argument "
                 "'round_to'", id="a call that does not fit the base's function"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
                  "def test_add_is_wrong():\n    assert calc.add(2, 3) == 6\n")},
                 "test_add_is_wrong", "not judged here",
                 "it fails on the base and with the fix: an assertion, raised in the test: assert 5 == 6",
                 id="fails with the fix too"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
                  "def test_add_still_subtracts():\n    assert calc.add(5, 3) == 2\n")},
                 "test_add_still_subtracts", "fails with the fix", "an assertion, raised in the test: assert 8 == 2",
                 id="passes on the base and fails with the fix"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": "import pytest\n" + new_test(
                  "@pytest.mark.parametrize('pair', [(2, 3), (4, 1)], "
                  "ids=['two and three - small', 'four [and] one'])\n"
                  "def test_add_adds(pair):\n    assert calc.add(*pair) == sum(pair)\n")},
                 "test_add_adds", "fails on the base", "it guards the fix: an assertion, raised in the test: assert",
                 id="a parametrized test is one test whatever its cases are called"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
                  "def test_add_adds_from_another_folder(tmp_path, monkeypatch):\n    monkeypatch.chdir(tmp_path)\n"
                  "    assert calc.add(2, 3) == 5\n")},
                 "test_add_adds_from_another_folder", "fails on the base",
                 "it guards the fix: an assertion, raised in the test: assert",
                 id="changes its folder and still fails in the test"),
    pytest.param({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": "import pytest\n" + new_test(
                  "@pytest.mark.skip(reason='needs a terminal')\n"
                  "def test_add_adds():\n    assert calc.add(2, 3) == 5\n")},
                 "test_add_adds", "not judged here", "skipped (needs a terminal)", id="skipped"),
    pytest.param({**GO_FIXED, "proxy/calc_test.go": go_test("TestAdds", "if add(2, 3) != 5 { t.Fatal() }")},
                 "TestAdds", "fails on the base", "it guards the fix: an assertion, raised in the test: want 5, got -1",
                 id="a Go test that fails on the base"),
    pytest.param({**GO_FIXED, "proxy/new_test.go": go_test("TestUsesNewCode", "if newHelper() != 5 { t.Fatal() }")},
                 "TestUsesNewCode", "needs the fix's code",
                 "on the base the test cannot run as written: ./x_test.go:6:5: undefined: newHelper",
                 id="a Go test that does not build on the base"),
    pytest.param({**GO_FIXED, "proxy/go_answers.json": json.dumps({"TestAdds": "skip"}),
                  "proxy/calc_test.go": go_test("TestAdds", "t.Skip()")},
                 "TestAdds", "not judged here", "skipped (needs a terminal)", id="a skipped Go test"),
])
def test_a_test_of_the_fix_gets_its_class_and_the_reason(fix, repo, tools, fix_files, test_name, its_class, reason):
    root, base = repo(fix_files)
    rows, _, _ = fix.judge(root, base, tools, 120)
    assert [(test.name, name) for test, name, _ in rows] == [(test_name, its_class)]
    assert rows[0][2].startswith(reason)


def test_only_the_tests_the_change_touches_are_run_and_each_gets_its_own_class(fix, repo, tools):
    root, base = repo({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
        "def test_add_adds():\n    assert calc.add(2, 3) == 5\n\n\n"
        "def test_add_gives_a_number():\n    assert isinstance(calc.add(2, 3), int)\n\n\n"
        "def test_double_doubles():\n    assert calc.double(4) == 8\n")})
    found, _, _, _ = verdicts(fix, root, base, tools)
    assert found == {"test_add_adds": "fails on the base", "test_add_gives_a_number": "passes on the base too",
                     "test_double_doubles": "needs the fix's code"}


def test_a_test_the_fix_only_changes_is_told_apart_from_a_test_it_adds(fix, repo, tools):
    changed = OLD_TEST.replace("assert calc.add(0, 0) == 0", "assert calc.add(0, 0) == 0  # zero stays zero")
    root, base = repo({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": changed + "\n\n"
                       "def test_add_gives_a_number():\n    assert isinstance(calc.add(2, 3), int)\n"})
    rows, _, _ = fix.judge(root, base, tools, 120)
    assert {(test.name, test.new, name) for test, name, _ in rows} == {
        ("test_add_takes_two_numbers", False, "passes on the base too"),
        ("test_add_gives_a_number", True, "passes on the base too")}
    assert sorted(fix.is_finding(test, name, False) for test, name, _ in rows) == [False, True]
    assert sorted(fix.is_finding(test, name, True) for test, name, _ in rows) == [False, False]


@pytest.mark.parametrize("faulty, fixed, said", [
    pytest.param({"proxy/limits.py": "LIMITS = {}\n\n\ndef limit(name):\n    return LIMITS[name]\n"},
                 {"proxy/limits.py": "LIMITS = {}\n\n\ndef limit(name):\n    return LIMITS.get(name, 0)\n",
                  "proxy/tests/test_limits.py": PATH_LINE + "import limits\n\n\n"
                  "def test_an_unknown_name_has_no_limit():\n    assert limits.limit('x') == 0\n"},
                 "an error (KeyError), raised in product code (proxy/limits.py)", id="raised in product code"),
    pytest.param({"proxy/keep.py": "import shutil\n\n\ndef keep(src, dst):\n    shutil.copyfile(src, dst)\n"},
                 {"proxy/keep.py": "import os\nimport shutil\n\n\ndef keep(src, dst):\n    if os.path.exists(src):\n"
                                   "        shutil.copyfile(src, dst)\n",
                  "proxy/tests/test_keep.py": PATH_LINE + "import keep\n\n\n"
                  "def test_a_file_that_is_not_there_is_left_out(tmp_path):\n"
                  "    keep.keep(str(tmp_path / 'none'), str(tmp_path / 'copy'))\n"},
                 "an error (FileNotFoundError), raised in the standard library (shutil.py), called from proxy/keep.py",
                 id="raised in the standard library"),
])
def test_an_error_the_fault_itself_raises_is_a_failure_on_the_base_with_its_kind_and_place(fix, repo, tools, faulty,
                                                                                         fixed, said):
    root, _ = repo(faulty)
    base = git(root, "rev-parse", "HEAD")
    commit(root, fixed, "fix")
    rows, _, _ = fix.judge(root, base, tools, 120)
    assert [name for _, name, _ in rows] == ["fails on the base"]
    assert said in rows[0][2]


@pytest.mark.parametrize("frames, said", [
    pytest.param(["proxy/tests/test_limits.py"], "in the test", id="a test file"),
    pytest.param(["proxy/tests/test_limits.py", "proxy/limits.py"], "in product code (proxy/limits.py)",
                 id="product code"),
    pytest.param(["tests/e2e/test_driver.py", "scripts/driver.py", "/usr/local/lib/python3.12/dataclasses.py"],
                 "in the standard library (dataclasses.py), called from scripts/driver.py",
                 id="the standard library, called from a script"),
    pytest.param(["tests/e2e/test_driver.py", "/usr/lib/python3.12/shutil.py"],
                 "in the standard library (shutil.py), called from the test",
                 id="the standard library, called from the test"),
    pytest.param(["tests/test_x.py", "proxy/calc.py", "/home/u/.local/lib/python3.12/site-packages/yaml/scanner.py"],
                 "in an installed package (scanner.py), called from proxy/calc.py",
                 id="an installed package, called from product code"),
    pytest.param(["/usr/lib/python3/dist-packages/_pytest/python.py"], "in an installed package (python.py)",
                 id="an installed package alone"),
])
def test_a_file_outside_the_repository_is_not_called_product_code(fix, frames, said):
    assert fix.place(frames, lambda path: "/tests/" in f"/{path}") == said


def test_a_file_of_the_tree_that_pytest_names_by_its_whole_path_is_still_a_file_of_the_repository(fix, tmp_path):
    tree = tmp_path / "tree"
    (tree / "tests").mkdir(parents=True)
    output = (f"_________ test_a _________\n{tree}/tests/test_x.py:9: in test_a\n    assert f() == 1\n"
              f"_________ test_b _________\n{tree}/tests/test_x.py:14: in test_b\n    g()\n{tree}/proxy/calc.py:3: in g\n"
              f"_________ test_c _________\ntests/test_x.py:20: in test_c\n    h()\n/usr/lib/python3.12/shutil.py:5: in h\n")
    assert fix.raised_where(output, lambda path: "/tests/" in f"/{path}", tree) == {
        "test_a": "in the test", "test_b": "in product code (proxy/calc.py)",
        "test_c": "in the standard library (shutil.py), called from the test"}
    assert fix.from_the_root(str(tree) + "-other/tests/test_x.py", tree) == str(tree) + "-other/tests/test_x.py"


def test_a_test_that_asks_the_product_in_a_new_process_gets_the_tree_that_is_tested(fix, repo, tools, monkeypatch):
    # The job installs the pull request's package for editing. That is the code with the fix, for every Python that
    # starts in another folder. Here the path of the environment stands for that install.
    in_a_new_process = (
        "import subprocess, sys\n\n\n"
        "def test_the_product_answers_in_a_new_process_in_another_folder(tmp_path):\n"
        "    done = subprocess.run([sys.executable, '-c', 'import prod; print(prod.answer())'], cwd=tmp_path,\n"
        "                          capture_output=True, text=True)\n"
        "    assert done.stdout.strip() == '42', done.stdout + done.stderr\n\n\n"
        "def test_the_product_answers_in_the_tests_own_process():\n"
        "    import prod\n"
        "    assert prod.answer() == 42\n")
    root, _ = repo({"prod/__init__.py": "def answer():\n    return 41\n"})
    base = git(root, "rev-parse", "HEAD")
    commit(root, {"prod/__init__.py": "def answer():\n    return 42\n", "proxy/tests/test_prod.py": in_a_new_process}, "fix")
    monkeypatch.setenv("PYTHONPATH", str(root))
    found, rows, _, _ = verdicts(fix, root, base, tools)
    assert found == {"test_the_product_answers_in_a_new_process_in_another_folder": "fails on the base",
                     "test_the_product_answers_in_the_tests_own_process": "fails on the base"}, rows


def test_a_run_for_an_edit_of_a_pull_request_cancels_no_run_in_any_workflow():
    # An edit of the text can come while the run of a push is in progress. A workflow that starts for an edit and
    # cancels runs in progress would end that run, and `checks ran` reads a cancelled run as a job that did not run.
    workflows = sorted((SCRIPT.parents[1] / ".github" / "workflows").glob("*.yml"))
    starts_for_an_edit = []
    for path in workflows:
        workflow = yaml.safe_load(path.read_text(encoding="utf-8"))
        triggers = workflow.get("on", workflow.get(True)) or {}
        on_a_pull_request = triggers.get("pull_request") if isinstance(triggers, dict) else None
        if "edited" not in ((on_a_pull_request or {}).get("types") or []):
            continue
        starts_for_an_edit.append(path.name)
        groups = [workflow.get("concurrency")] + [job.get("concurrency") for job in workflow["jobs"].values()]
        for concurrency in filter(None, groups):
            group = concurrency if isinstance(concurrency, str) else concurrency.get("group", "")
            cancels = isinstance(concurrency, dict) and concurrency.get("cancel-in-progress") not in (None, False)
            assert not cancels or "github.event.action == 'edited' && github.run_id ||" in group, (
                f"{path.name} starts for an edit of a pull request and cancels runs in progress, and the run of an edit is "
                f"in the group of the others (`{group}`). An edit of the text would cancel the run of a push, and `checks "
                "ran` then says that a job did not run. Fix: give the run of an edit a group of its own, as "
                "fix-tests.yml does, or take the cancelling out.")
    assert "fix-tests.yml" in starts_for_an_edit, "this test no longer finds the workflows that start for an edit"


LOADS_A_SCRIPT = """import importlib.util, pathlib


def load():
    path = pathlib.Path(__file__).resolve().parents[1] / "scripts" / "tool.py"
    spec = importlib.util.spec_from_file_location("tool", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_the_tool_answers():
    assert load().answer() == 42


def test_a_file_that_was_never_there_is_read():
    assert (pathlib.Path(__file__).resolve().parents[1] / "notes" / "old.txt").read_text() == "x"
"""


def test_a_test_that_stops_on_a_file_the_fix_adds_needs_the_fixs_code(fix, repo, tools):
    root, base = repo({"proxy/calc.py": FIXED, "scripts/tool.py": "def answer():\n    return 42\n",
                       "tests/test_tool.py": LOADS_A_SCRIPT})
    found, rows, _, _ = verdicts(fix, root, base, tools)
    assert found["test_the_tool_answers"] == "needs the fix's code"
    detail = next(detail for test, _, detail in rows if test.name == "test_the_tool_answers")
    assert "the base has no scripts/tool.py, a file the fix adds" in detail
    # A file that is missing on the base and on the pull request alike is not something the fix brings.
    assert found["test_a_file_that_was_never_there_is_read"] == "not judged here"


# "missing": the test cannot run as written on the base, because it needs a name or a file that only the fix brings.
@pytest.mark.parametrize("text, added, state", [
    pytest.param("AttributeError: 'NoneType' object has no attribute 'name'", [], "fail",
                 id="an attribute of a wrong value"),
    pytest.param("AttributeError: module 'calc' has no attribute 'double'", [], "missing", id="a name a module lacks"),
    pytest.param("AttributeError: type object 'Session' has no attribute 'prompts'", [], "missing",
                 id="a name a class lacks"),
    pytest.param("NameError: name 'double' is not defined", [], "missing", id="a name that is not defined"),
    pytest.param("ImportError: cannot import name 'double' from 'calc'", [], "missing", id="a name it cannot import"),
    pytest.param("fixture 'tools' not found", [], "missing", id="a fixture that is not there"),
    pytest.param("TypeError: add() takes 2 positional arguments but 3 were given", [], "missing",
                 id="one argument too many"),
    pytest.param("TypeError: add() missing 1 required positional argument: 'b'", [], "missing",
                 id="one argument too few"),
    pytest.param("TypeError: unsupported operand type(s) for +: 'int' and 'str'", [], "fail",
                 id="a value of a wrong type"),
    pytest.param("python3: can't open file '/tmp/base/scripts/tool.py': [Errno 2] No such file or directory",
                 ["scripts/tool.py"], "missing", id="a file the fix adds"),
    pytest.param("FileNotFoundError: [Errno 2] No such file or directory: '/tmp/x/notes/old.txt'", ["scripts/tool.py"],
                 "fail", id="a file the fix does not add"),
    pytest.param("FileNotFoundError: [Errno 2] No such file or directory: '/tmp/x/other_tool.py'", ["tool.py"], "fail",
                 id="a file whose name only ends like one the fix adds"),
])
def test_a_failure_is_missing_only_for_a_name_or_a_file_that_the_fix_brings(fix, text, added, state):
    assert fix.failure(text, added=added).state == state


def test_a_fix_with_no_new_or_changed_test_has_nothing_to_run(fix, repo, tools):
    root, base = repo({"proxy/calc.py": FIXED})
    rows, notes, product = fix.judge(root, base, tools, 120)
    assert (rows, notes, product) == ([], [], "code")
    assert fix.summary(rows, notes, product) == [
        "fix tests: a fix of product code. No test: this fix adds or changes no test, so there is nothing to run."]


@pytest.mark.parametrize("files, kind", [
    pytest.param({"proxy/Dockerfile": "FROM scratch\n", "inference/Dockerfile.v31": "FROM scratch\n"}, "folder",
                 id="only build files of product folders"),
    pytest.param({"proxy/Dockerfile": "FROM scratch\n", "proxy/calc.py": FIXED}, "code",
                 id="a build file and product code"),
    pytest.param({"proxy/tests/test_calc.py": OLD_TEST + "\n\ndef test_more():\n    assert calc.add(0, 0) == 0\n"},
                 "outside", id="only a test of a product folder"),
    pytest.param({"docs/CLI.md": "text\n", ".github/workflows/x.yml": "on: push\n"}, "outside", id="docs and CI"),
    pytest.param({"scripts/tool.py": "x = 1\n"}, "outside", id="a script"),
])
def test_a_fix_is_named_by_what_it_changes(fix, repo, tools, files, kind):
    root, base = repo(files)
    _, _, product = fix.judge(root, base, tools, 120)
    assert product == kind
    line = fix.headline([], product)
    assert line.startswith("fix tests: " + fix.CHANGES[kind] + ". ")
    assert ("outside product code" in line) is (kind == "outside")
    assert ("a build or settings file" in line) is (kind == "folder")


def test_the_go_stand_in_starts_where_the_path_of_python_holds_a_space(fix, repo, tmp_path):
    folder = tmp_path / "a folder"
    folder.mkdir()
    venv.create(folder / "a python", with_pip=False, symlinks=True)
    go = go_stand_in(folder, str(folder / "a python" / "bin" / "python"))
    root, base = repo({**GO_FIXED, "proxy/calc_test.go": go_test("TestAdds", "if add(2, 3) != 5 { t.Fatal() }")})
    found, _, _, _ = verdicts(fix, root, base, {"python": sys.executable, "go": go})
    assert found == {"TestAdds": "fails on the base"}


def test_a_go_test_file_that_uses_only_old_code_is_judged_alone_when_the_package_does_not_build(fix, repo, tools):
    root, base = repo({**GO_FIXED,
                       "proxy/new_test.go": go_test("TestUsesNewCode", "if newHelper() != 5 { t.Fatal() }"),
                       "proxy/old_test.go": go_test("TestOldCode", "if add(2, 3) != 5 { t.Fatal() }")})
    found, _, notes, _ = verdicts(fix, root, base, tools)
    assert found == {"TestUsesNewCode": "needs the fix's code", "TestOldCode": "fails on the base"}
    assert notes == []


@pytest.mark.parametrize("answer, said", [
    pytest.param("fail", "an assertion, raised in the test: want 5, got -1", id="the test's own message"),
    pytest.param("subfail", "an assertion, raised in the test: status = 3, want an active vector",
                 id="the message of a subtest"),
    pytest.param("silent",
                 "an assertion, raised in the test: the test reported a failure, and its output has no message",
                 id="no message"),
    pytest.param("panic",
                 "an error (panic), raised in product code (calc.go): panic: runtime error: "
                 "index out of range [recovered]",
                 id="a panic, with the place it was raised"),
])
def test_a_go_failure_keeps_the_tests_own_message_or_the_panic_and_says_when_there_is_none(fix, repo, tools,
                                                                                          monkeypatch, answer, said):
    root, base = repo({**GO_FIXED, "proxy/calc_test.go": go_test("TestAdds", "if add(2, 3) != 5 { t.Fatal() }")})
    original = fix.run_go

    def answers_on_the_base(tree, tests, go, limit):
        if tree != root:
            (tree / "proxy" / "go_answers.json").write_text(json.dumps({"TestAdds": answer}), encoding="utf-8")
        return original(tree, tests, go, limit)
    monkeypatch.setattr(fix, "run_go", answers_on_the_base)
    found, rows, _, _ = verdicts(fix, root, base, tools)
    assert found == {"TestAdds": "fails on the base"}
    assert rows[0][2] == "it guards the fix: " + said


def test_the_output_of_another_go_test_whose_name_starts_the_same_is_not_taken(fix, repo, tools, monkeypatch):
    both = (go_test("TestAdds", "if add(2, 3) != 5 { t.Fatal() }")
            + "\nfunc TestAddsMore(t *testing.T) {\n\tif add(2, 4) != 6 { t.Fatal() }\n}\n")
    root, base = repo({**GO_FIXED, "proxy/calc_test.go": both})
    original = fix.run_go

    def answers_on_the_base(tree, tests, go, limit):
        if tree != root:
            (tree / "proxy" / "go_answers.json").write_text(json.dumps({"TestAdds": "silent", "TestAddsMore": "fail"}),
                                                           encoding="utf-8")
        return original(tree, tests, go, limit)
    monkeypatch.setattr(fix, "run_go", answers_on_the_base)
    _, rows, _, _ = verdicts(fix, root, base, tools)
    assert {test.name: detail for test, _, detail in rows} == {
        "TestAdds": "it guards the fix: an assertion, raised in the test: the test reported a failure, and its output "
                    "has no message",
        "TestAddsMore": "it guards the fix: an assertion, raised in the test: want 5, got -1"}


def test_more_go_test_files_than_the_limit_are_not_tried_one_by_one_and_that_is_said(fix, repo, tools, monkeypatch):
    monkeypatch.setattr(fix, "ISOLATE_AT_MOST", 1)
    root, base = repo({**GO_FIXED,
                       "proxy/new_test.go": go_test("TestUsesNewCode", "if newHelper() != 5 { t.Fatal() }"),
                       "proxy/old_test.go": go_test("TestOldCode", "if add(2, 3) != 5 { t.Fatal() }")})
    found, _, notes, _ = verdicts(fix, root, base, tools)
    assert found == {"TestUsesNewCode": "needs the fix's code", "TestOldCode": "needs the fix's code"}
    assert notes == [("2 changed Go test files did not build together on the base; they were not tried one by one "
                      "(the limit is 1)")]


@pytest.mark.parametrize("files, note", [
    pytest.param({"extensions/vscode/test/sse.test.ts": "it('parses', () => {});\n"},
                 "extensions/vscode/test/sse.test.ts: a TypeScript or JavaScript test file; this check does not read "
                 "those yet", id="a TypeScript test file"),
    pytest.param({"geometric-lens/tests/test_score.py": "def test_scores():\n    assert True\n"},
                 "geometric-lens/tests/test_score.py: the packages its tests need are not installed in this job",
                 id="a test whose packages the job does not install"),
])
def test_a_test_file_this_check_cannot_run_is_named_as_not_judged(fix, repo, tools, files, note):
    root, base = repo(files)
    rows, notes, _ = fix.judge(root, base, tools, 120)
    assert (rows, notes) == ([], [note])


def test_the_lines_a_change_touches_are_read_from_the_diff(fix, repo):
    root, base = repo({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": OLD_TEST.replace("== 0", "== 0  # same")})
    touched = fix.changed_lines(root, base)
    assert touched["proxy/calc.py"] == {2, 3, 4, 5, 6}
    assert touched["proxy/tests/test_calc.py"] == {7}


def test_a_test_that_only_loses_a_line_is_a_changed_test(fix, repo, tools):
    root, _ = repo({"proxy/tests/test_calc.py": OLD_TEST + "    assert calc.add(2, 3) == 5\n"})
    base = git(root, "rev-parse", "HEAD")
    commit(root, {"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": OLD_TEST}, "the fix takes a line out of a test")
    assert fix.changed_lines(root, base)["proxy/tests/test_calc.py"] == {7, 8}
    rows, _, _ = fix.judge(root, base, tools, 120)
    assert [(test.name, test.new, name) for test, name, _ in rows] == [
        ("test_add_takes_two_numbers", False, "passes on the base too")]


PYTHON_TEST_FILE = ("import pytest\n\n\n@pytest.mark.parametrize('n', [1, 2])\ndef test_a(n):\n    assert n\n\n\n"
                    "class TestGroup:\n    def test_b(self):\n        assert True\n\n\ndef helper():\n    return 1\n")
GO_TEST_FILE = ("package p\n\nfunc TestA(t *testing.T) {\n\tx()\n}\n\nfunc helper() {\n}\n\n"
                "func TestB(t *testing.T) { y() }\n")


@pytest.mark.parametrize("path, source, line, names", [
    pytest.param("tests/test_x.py", PYTHON_TEST_FILE, 4, ["test_a"], id="python: a decorator"),
    pytest.param("tests/test_x.py", PYTHON_TEST_FILE, 6, ["test_a"], id="python: the body"),
    pytest.param("tests/test_x.py", PYTHON_TEST_FILE, 11, ["TestGroup::test_b"], id="python: a method of a test class"),
    pytest.param("tests/test_x.py", PYTHON_TEST_FILE, 15, [], id="python: a helper"),
    pytest.param("tests/test_x.py", PYTHON_TEST_FILE, 1, [], id="python: an import"),
    pytest.param("p/x_test.go", GO_TEST_FILE, 4, ["TestA"], id="go: the body"),
    pytest.param("p/x_test.go", GO_TEST_FILE, 7, [], id="go: a helper"),
    pytest.param("p/x_test.go", GO_TEST_FILE, 10, ["TestB"], id="go: a test on one line"),
])
def test_a_test_is_found_by_the_lines_of_its_function_and_its_decorators(fix, path, source, line, names):
    assert [test.name for test in fix.tests_of(path, source) if test.line <= line <= test.last] == names


@pytest.mark.parametrize("base, head, name", [
    ("fail", "pass", "fails on the base"),
    ("missing", "pass", "needs the fix's code"),
    ("pass", "pass", "passes on the base too"),
    ("fail", "fail", "not judged here"),
    ("missing", "fail", "not judged here"),
    ("missing", "missing", "not judged here"),
    ("none", "fail", "not judged here"),
    ("pass", "fail", "fails with the fix"),
    ("pass", "missing", "fails with the fix"),
    ("none", "pass", "not judged here"),
    ("pass", "skip", "not judged here"),
])
def test_each_pair_of_outcomes_has_one_class(fix, base, head, name):
    assert fix.verdict(fix.Outcome(base, "x"), fix.Outcome(head, "y"))[0] == name


def rows_of(fix, *names, new=True):
    return [(fix.Test("python", "tests/test_x.py", f"test_{n}", n + 1, n + 1, new), name, "detail")
            for n, name in enumerate(names)]


def test_the_summary_counts_each_class_apart_and_new_tests_apart_from_changed_ones(fix):
    rows = rows_of(fix, "fails on the base", "fails on the base", "needs the fix's code", "passes on the base too")
    rows += rows_of(fix, "passes on the base too", "fails on the base", new=False)
    lines = fix.summary(rows, [], "code")
    assert lines[0] == "fix tests: a fix of product code. Guarded: 3 test(s) fail on the base."
    assert lines[1] == ("For information: 1 new test(s) pass on the base too. Beside a test that fails on the base they "
                        "are most likely controls.")
    assert lines[2] == ("4 new test(s): 2 fails on the base; 1 needs the fix's code; 1 passes on the base too. "
                        "2 changed test(s): 1 fails on the base; 1 passes on the base too.")
    assert lines[6] == "| `tests/test_x.py::test_0` | new | fails on the base | detail |"
    assert lines[10] == "| `tests/test_x.py::test_0` | changed | passes on the base too | detail |"


@pytest.mark.parametrize("names, new, line", [
    pytest.param(("fails on the base", "needs the fix's code"), True, "Guarded: 1 test(s) fail on the base.",
                 id="one fails on the base"),
    pytest.param(("fails on the base", "passes on the base too", "passes on the base too"), True,
                 "Guarded: 1 test(s) fail on the base.", id="one fails on the base beside two that pass"),
    pytest.param(("passes on the base too", "needs the fix's code"), True,
                 "Not shown: 1 test(s) ran on the base and none failed. 1 more need code or a file the fix adds.",
                 id="one passes on the base, one needs code"),
    pytest.param(("passes on the base too", "passes on the base too", "not judged here"), True,
                 "Not shown: 2 test(s) ran on the base and none failed.", id="two pass on the base, one not judged"),
    pytest.param(("passes on the base too",), False, "Not shown: 1 test(s) ran on the base and none failed.",
                 id="a changed test passes on the base"),
    pytest.param(("needs the fix's code", "needs the fix's code"), True,
                 "Cannot tell: every test needs code or a file the fix adds, so none could run on the base.",
                 id="every test needs code"),
    pytest.param(("not judged here",), True,
                 "Cannot tell: no test of the fix could be judged on the base (0 need the fix's code).",
                 id="the only test is not judged"),
    pytest.param(("needs the fix's code", "not judged here", "fails with the fix"), True,
                 "Cannot tell: no test of the fix could be judged on the base (1 need the fix's code).",
                 id="none judged, one needs code"),
    pytest.param((), True, "No test: this fix adds or changes no test, so there is nothing to run.", id="no test"),
])
def test_the_headline_says_how_far_the_fix_is_shown_to_be_guarded(fix, names, new, line):
    assert fix.headline(rows_of(fix, *names, new=new), "code") == f"fix tests: a fix of product code. {line}"


def test_a_new_test_that_passes_on_the_base_beside_a_guard_is_information_and_not_a_finding(fix, capsys):
    rows = rows_of(fix, "fails on the base", "needs the fix's code", "passes on the base too", "passes on the base too")
    rows += rows_of(fix, "passes on the base too", new=False)
    fix.report(rows, [], "code", True)
    out = capsys.readouterr().out
    assert [line for line in out.splitlines() if line.startswith("::")] == []
    assert out.splitlines()[0] == "fix tests: a fix of product code. Guarded: 1 test(s) fail on the base."
    assert out.splitlines()[1] == ("For information: 2 new test(s) pass on the base too. Beside a test that fails on the "
                                   "base they are most likely controls.")


def test_only_a_new_test_that_passes_without_the_fix_is_annotated(fix, capsys):
    rows = rows_of(fix, "not judged here", "needs the fix's code", "passes on the base too")
    rows += rows_of(fix, "passes on the base too", new=False)
    fix.report(rows, [], "code", True)
    out = capsys.readouterr().out
    assert out.splitlines()[1] == "1 new test(s) pass on the base too: a control, or a test that does not test the fix."
    notes = [line for line in out.splitlines() if line.startswith("::")]
    assert len(notes) == 1
    assert notes[0].startswith("::warning file=tests/test_x.py,line=3,title=this new test passes without the fix::test_2 ")
    assert "Fix: make the test fail on the fault the pull request fixes." in notes[0]
    assert "If it is a control that holds what the fix must not change, say so in the pull request." in notes[0]


def test_the_job_summary_gets_the_report(fix, monkeypatch, tmp_path, capsys):
    monkeypatch.setenv("GITHUB_STEP_SUMMARY", str(tmp_path / "summary.md"))
    fix.report(rows_of(fix, "fails on the base"), [], "code", True)
    written = (tmp_path / "summary.md").read_text(encoding="utf-8")
    assert written.startswith("fix tests: a fix of product code. Guarded: 1 test(s) fail on the base.\n")
    assert written == capsys.readouterr().out


def test_a_finding_is_reported_and_does_not_fail_the_job(fix, repo, capsys):
    root, base = repo({"proxy/calc.py": FIXED, "proxy/tests/test_calc.py": new_test(
        "def test_add_gives_a_number():\n    assert isinstance(calc.add(2, 3), int)\n")})
    assert fix.main(["--base", base, "--title", "fix(proxy): add adds", "--root", str(root), "--github"]) == 0
    out = capsys.readouterr().out
    assert "1 new test(s) pass on the base too" in out
    assert "::warning file=proxy/tests/test_calc.py," in out


@pytest.mark.parametrize("title", ["feat(proxy): a new tool", "docs: fix a typo", "refactor(tui): fix the layout code"])
def test_a_pull_request_that_is_not_a_fix_has_nothing_to_check(fix, capsys, title):
    assert fix.main(["--base", "HEAD", "--title", title]) == 0
    assert "nothing to check" in capsys.readouterr().out


@pytest.mark.parametrize("title", ["fix: the thing", "fix(proxy): the thing", "fix(proxy)!: the thing"])
def test_a_fix_title_is_judged(fix, title):
    assert fix.FIX_TITLE.match(title)


def test_a_base_it_cannot_read_ends_with_status_2_and_a_fix(fix, repo, capsys):
    root, _ = repo({"proxy/calc.py": FIXED})
    assert fix.main(["--base", "no-such-commit", "--title", "fix: x", "--root", str(root)]) == 2
    err = capsys.readouterr().err
    assert err.startswith("FAIL fix tests: `git diff")
    assert "Fix: run this in a checkout that holds the base commit" in err
