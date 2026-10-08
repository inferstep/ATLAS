"""The judge of a run of the tests that need a running service, and the one list of those tests.

The result files here are written by pytest itself, for a test file made for
the case, so the judge is held against the file as pytest writes it.
"""
import ast
import importlib.util
import re
import subprocess
import sys
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "tests_counted.py"
PASSES = "def test_one():\n    assert True\n\n\ndef test_two():\n    assert True\n"
MIXED = '''import pytest


@pytest.fixture
def broken():
    raise RuntimeError("the fixture broke")


def test_passes():
    assert True


def test_passes_too():
    assert True


def test_fails():
    assert 1 == 2


def test_ends_with_an_error(broken):
    assert True


def test_skips():
    pytest.skip("the service   is not there")


def test_skips_for_the_same_reason():
    pytest.skip("the service is not there")


@pytest.mark.skipif(True, reason="a tool is missing & <more>")
def test_skips_by_a_condition():
    assert True
'''


@pytest.fixture(scope="module")
def counted():
    spec = importlib.util.spec_from_file_location("atlas_tests_counted_under_test", SCRIPT)
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    yield module
    sys.modules.pop(spec.name, None)


def result_file(folder, tests):
    """Run pytest on a test file with this text; the result file that pytest wrote."""
    folder.mkdir(parents=True, exist_ok=True)
    (folder / "test_made.py").write_text(tests, encoding="utf-8")
    result = folder / "result.xml"
    subprocess.run([sys.executable, "-m", "pytest", "test_made.py", "-q", "-p", "no:cacheprovider", "--rootdir", str(folder),
                    f"--junitxml={result}"], cwd=folder, capture_output=True, text=True, timeout=120, check=False)
    return result


def group_of(counted, monkeypatch, expected):
    """Give the group `sandbox` this number of tests for the test, and nothing else of it another value."""
    group = counted.GROUPS["sandbox"]
    monkeypatch.setitem(counted.GROUPS, "sandbox", group._replace(expected=expected))


# --- reading a result file ----------------------------------------------------------------------------------------

def test_the_numbers_of_a_result_file_that_pytest_wrote(counted, tmp_path):
    numbers = counted.read(result_file(tmp_path, MIXED).read_text(encoding="utf-8"))
    assert numbers == {"collected": 7, "passed": 2, "failed": 2, "skipped": 3,
                       "failed_tests": ["test_fails", "test_ends_with_an_error"],
                       "skip_reasons": {"the service is not there": 2, "a tool is missing & <more>": 1}}


def test_a_file_that_a_missing_package_skips_as_a_whole_is_one_skipped_test_and_no_other(counted, tmp_path):
    whole = "import pytest\n\nmissing = pytest.importorskip('a_package_that_is_not_there')\n\n\n" + PASSES
    numbers = counted.read(result_file(tmp_path, whole).read_text(encoding="utf-8"))
    assert numbers["collected"] == 1
    assert numbers["skipped"] == 1
    assert numbers["passed"] == 0
    assert numbers["skip_reasons"] == {
        "a whole file was skipped: could not import 'a_package_that_is_not_there': No module named 'a_package_that_is_not_there'": 1}
    assert counted.faults(numbers, 2)[0] == "1 tests were collected, and there are 2"


def test_a_passing_test_before_a_failed_one_is_not_named_as_failed(counted):
    result = ('<testsuites><testsuite errors="0" failures="1" skipped="0" tests="3"><testcase name="test_first"/>'
              '<testcase classname="a" name="test_second" time="0.1"></testcase>'
              '<testcase classname="a" name="test_a &amp; b"><failure message="no">where</failure></testcase>'
              "</testsuite></testsuites>")
    assert counted.read(result)["failed_tests"] == ["test_a & b"]


def test_the_suites_of_a_result_file_are_added_up(counted):
    result = ('<testsuites><testsuite name="one" errors="1" failures="0" skipped="2" tests="5"></testsuite>'
              '<testsuite name="two" errors="0" failures="1" skipped="0" tests="4"></testsuite></testsuites>')
    numbers = counted.read(result)
    assert (numbers["collected"], numbers["failed"], numbers["skipped"], numbers["passed"]) == (9, 2, 2, 5)


def test_a_file_with_no_suite_is_not_a_result_file(counted):
    with pytest.raises(counted.Unreadable):
        counted.read("<html>not found</html>")


# --- what keeps a run from holding --------------------------------------------------------------------------------

def numbers(collected=4, failed=0, skipped=0, failed_tests=(), skip_reasons=None):
    return {"collected": collected, "passed": collected - failed - skipped, "failed": failed, "skipped": skipped,
            "failed_tests": list(failed_tests), "skip_reasons": skip_reasons or {}}


@pytest.mark.parametrize("measured, says", [
    (numbers(), []),
    (numbers(collected=3), ["3 tests were collected, and there are 4"]),
    (numbers(collected=5), ["5 tests were collected, and there are 4"]),
    (numbers(failed=2, failed_tests=["test_a", "test_b"]), ["2 test(s) failed (test_a; test_b)"]),
    (numbers(failed=1), ["1 test(s) failed"]),
    (numbers(collected=8, failed=7, failed_tests=["a", "b", "c", "d", "e", "f", "g"]),
     ["8 tests were collected, and there are 4", "7 test(s) failed (a; b; c; d; e; and 2 more test(s))"]),
    # A skipped test counts as collected: all 4 are there, and 2 of them were not run.
    (numbers(skipped=2, skip_reasons={"the service is not there": 2}), ["2 test(s) were skipped (2: the service is not there)"]),
    (numbers(skipped=2), ["2 test(s) were skipped (the result file gives no reason)"]),
    (numbers(collected=9, skipped=8, skip_reasons={"one": 2, "two": 2, "three": 1, "four": 1, "five": 1, "six": 1}),
     ["9 tests were collected, and there are 4",
      "8 test(s) were skipped (2: one; 2: two; 1: three; 1: four; 1: five; and 1 more reason(s))"]),
    (numbers(collected=3, failed=1, skipped=1, failed_tests=["test_a"], skip_reasons={"no tool": 1}),
     ["3 tests were collected, and there are 4", "1 test(s) failed (test_a)", "1 test(s) were skipped (1: no tool)"]),
])
def test_each_thing_that_keeps_a_run_from_holding_is_named(counted, measured, says):
    assert counted.faults(measured, 4) == says


# --- the command --------------------------------------------------------------------------------------------------

def test_a_run_in_which_every_test_of_the_group_ran_and_passed_holds(counted, tmp_path, monkeypatch, capsys):
    group_of(counted, monkeypatch, 2)
    assert counted.main(["sandbox", str(result_file(tmp_path, PASSES))]) == 0
    assert capsys.readouterr().out == "tests counted: all 2 tests that need a running sandbox service ran and passed.\n"


def test_a_run_with_a_skipped_test_does_not_hold_though_pytest_ended_with_status_0(counted, tmp_path, monkeypatch, capsys):
    skips = PASSES + "\n\ndef test_three():\n    import pytest\n    pytest.skip('ruby is not in the sandbox')\n"
    folder = tmp_path / "run"
    result = result_file(folder, skips)
    done = subprocess.run([sys.executable, "-m", "pytest", "test_made.py", "-q", "-p", "no:cacheprovider", "--rootdir", str(folder)],
                          cwd=folder, capture_output=True, text=True, timeout=120, check=False)
    assert done.returncode == 0, "pytest itself calls such a run a success"
    group_of(counted, monkeypatch, 3)
    assert counted.main(["sandbox", str(result)]) == 1
    said = capsys.readouterr().out
    assert said.startswith("::error title=tests counted::the run of the tests that need a running sandbox service does not hold: ")
    assert "1 test(s) were skipped (1: ruby is not in the sandbox)" in said
    assert "Fix: " in said


@pytest.mark.parametrize("tests, expected, says", [
    (PASSES, 3, "2 tests were collected, and there are 3"),
    (PASSES + "\n\ndef test_three():\n    assert 1 == 2\n", 3, "1 test(s) failed (test_three)"),
])
def test_a_run_with_a_failed_test_or_with_another_number_of_tests_does_not_hold(counted, tmp_path, monkeypatch, capsys, tests,
                                                                                expected, says):
    group_of(counted, monkeypatch, expected)
    assert counted.main(["sandbox", str(result_file(tmp_path, tests))]) == 1
    assert says in capsys.readouterr().out


@pytest.mark.parametrize("written", [None, "", "pytest could not start"])
def test_a_result_file_that_cannot_be_read_is_not_a_pass(counted, tmp_path, capsys, written):
    result = tmp_path / "result.xml"
    if written is not None:
        result.write_text(written, encoding="utf-8")
    assert counted.main(["sandbox", str(result)]) == 2
    said = capsys.readouterr().out
    assert "::error title=tests counted::the result file" in said
    assert "cannot be read" in said
    assert "so no test is known to have run. Fix: " in said


@pytest.mark.parametrize("arguments", [[], ["sandbox"], ["no-such-group", "result.xml"], ["--files"], ["--files", "no-such-group"],
                                       ["sandbox", "result.xml", "one more"]])
def test_a_call_that_names_no_group_is_refused(counted, capsys, arguments):
    assert counted.main(arguments) == 2
    said = capsys.readouterr().err
    assert "give one of the groups sandbox, proxy, model" in said
    assert "Fix: " in said


def test_the_files_of_a_group_are_printed_for_the_pytest_command(counted, capsys):
    assert counted.main(["--files", "proxy"]) == 0
    assert capsys.readouterr().out.split() == ["tests/infrastructure/test_tui_render.py", "tests/infrastructure/test_tui_commands.py",
                                               "tests/infrastructure/test_control_plane.py"]


# --- the one list: the groups, the mark, the page, the jobs ---------------------------------------------------------

def test_each_file_of_a_group_is_there_and_is_in_one_group_only(counted):
    files = [file for group in counted.GROUPS.values() for file in group.files]
    assert len(files) == len(set(files))
    for file in files:
        assert (ROOT / file).is_file(), file


def test_the_groups_hold_the_files_that_carry_the_mark_and_no_other(counted):
    marked = re.findall(r'"/(tests/infrastructure/test_\w+\.py)"', (ROOT / "tests" / "conftest.py").read_text(encoding="utf-8"))
    in_a_group = sorted(file for group in counted.GROUPS.values() for file in group.files)
    assert sorted(marked) == in_a_group, (
        "tests/conftest.py gives the mark `integration` to another list of files than the groups of scripts/tests_counted.py "
        "hold. The plain jobs leave a marked file out, so a file that is in no group is run nowhere. Fix: put the file "
        "into the group of the place that can run it, and count its tests into the group's number and the page's table.")


def number_of_tests(file):
    """The number of test functions of a file. A function that runs once for each of a list of values is refused."""
    tree = ast.parse((ROOT / file).read_text(encoding="utf-8"))
    functions = [node for node in ast.walk(tree) if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)) and node.name.startswith("test")]
    many = [node.name for node in functions if any("parametrize" in ast.unparse(mark) for mark in node.decorator_list)]
    assert not many, (
        f"{file}: {many} now runs once for each of its values, so the number of its tests is no longer the number of its "
        "functions. Fix: count its values in this test, then give the new number to the group and to the page.")
    return len(functions)


def test_the_number_of_each_group_is_the_number_of_test_functions_in_its_files_and_the_gates_page_has_each(counted):
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    rows = {file: (int(count), runs_in) for file, count, runs_in in
            re.findall(r"^\| `(tests/infrastructure/test_\w+\.py)` \| (\d+) \| [^|]+ \| ([^|]+?) \|$", page, re.MULTILINE)}
    for name, group in counted.GROUPS.items():
        in_the_files = {file: number_of_tests(file) for file in group.files}
        assert sum(in_the_files.values()) == group.expected, (
            f"the files of the group `{name}` have {sum(in_the_files.values())} tests ({in_the_files}), and its number in "
            f"scripts/tests_counted.py is {group.expected}. A run of the group fails while the two differ. Fix: give the "
            "new number to the group and to the rows of docs/quality/gates.md, in the same change.")
        for file, count in in_the_files.items():
            assert rows.get(file) == (count, group.runs_in), (
                f"the row of {file} in docs/quality/gates.md is {rows.get(file)} (its tests, the job that runs it), and the "
                f"group `{name}` gives {(count, group.runs_in)}. Fix: the row of the page.")
    assert sorted(rows) == sorted(file for group in counted.GROUPS.values() for file in group.files)
    said = " ".join(page.split())
    assert f"all {counted.GROUPS['model'].expected} of them" in said, "the section of the nightly run gives another number"


def job_of(name):
    workflow = yaml.safe_load((ROOT / ".github" / "workflows" / "test.yml").read_text(encoding="utf-8"))
    found = [job for job in workflow["jobs"].values() if job.get("name") == name]
    assert len(found) == 1, f"test.yml has {len(found)} job(s) with the name `{name}`"
    return found[0]


@pytest.mark.parametrize("name", ["sandbox", "proxy"])
def test_the_job_of_a_group_runs_its_files_and_gives_the_result_file_to_the_judge(counted, name):
    group = counted.GROUPS[name]
    job = job_of(re.fullmatch(r"the job `(.+)`", group.runs_in).group(1))
    assert "continue-on-error" not in job
    runs = [step for step in job["steps"] if "run" in step]
    tests = [step for step in runs if "-m pytest" in step["run"]]
    assert len(tests) == 1
    command = " ".join(tests[0]["run"].replace("\\\n", " ").split())
    assert f"-m integration $(python scripts/tests_counted.py --files {name})" in command
    written = re.search(r'--junitxml="(\$RUNNER_TEMP/[\w-]+\.xml)"', command).group(1)
    judges = [step for step in runs if step["run"].strip() == f'python scripts/tests_counted.py {name} "{written}"']
    assert len(judges) == 1, "the judge reads the file that pytest wrote, in a step of its own"
    # The judge runs whenever the tests ran: no condition, and nothing lets the job go on after its red.
    assert "if" not in judges[0]
    assert "continue-on-error" not in judges[0]
    assert job["steps"].index(judges[0]) == job["steps"].index(tests[0]) + 1


def test_no_file_of_the_sandbox_group_asks_the_machine_that_runs_pytest_for_a_tool(counted):
    for file in counted.GROUPS["sandbox"].files:
        text = (ROOT / file).read_text(encoding="utf-8")
        for asks in ("shutil.which", "skipif"):
            assert asks not in text, (
                f"{file} has `{asks}`. The code of these tests runs in the sandbox service, so what the machine that runs "
                "pytest has says nothing, and a skipped test fails the job. Fix: take the condition out; a sandbox that "
                "cannot run a language fails the test.")


def test_the_sandbox_job_waits_for_the_health_answer_of_the_service_and_not_for_a_number_of_seconds():
    job = job_of("sandbox tests (containerized)")
    start = next(step for step in job["steps"] if "docker run" in step.get("run", ""))
    assert "http://127.0.0.1:8020/health" in start["run"]
    assert "::error::The sandbox gave no health answer" in start["run"]
    assert "Fix: " in start["run"]
    assert "docker logs sandbox-test" in start["run"]
    assert "exit 1" in start["run"]
    assert job["steps"].index(start) < next(n for n, step in enumerate(job["steps"]) if "-m pytest" in step.get("run", ""))
