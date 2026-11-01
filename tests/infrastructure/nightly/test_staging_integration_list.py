"""scripts/staging-integration.py runs the Go tests that need a real executor by a fixed list of names (TESTS).
A test of proxy/staging_integration_test.go that asks for the executor but is not on the list would never run,
and a name on the list that no such test has would match nothing. These tests keep the two the same.

A test asks for the executor when its own body calls stagingIntegrationEnv(t), as every one of them does."""

import ast
import re
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts" / "staging-integration.py"
TEST_FILE = ROOT / "proxy" / "staging_integration_test.go"


def names_the_script_runs():
    """The test names joined in TESTS, read from the script's source."""
    for node in ast.parse(SCRIPT.read_text()).body:
        if isinstance(node, ast.Assign) and any(
                isinstance(target, ast.Name) and target.id == "TESTS" for target in node.targets):
            return set(ast.literal_eval(node.value).split("|"))
    raise AssertionError(f"{SCRIPT} has no TESTS")


def executor_tests_in_the_go_file():
    """The top-level Go tests of the file whose body calls stagingIntegrationEnv(t)."""
    names = set()
    for function in re.split(r"^func ", TEST_FILE.read_text(), flags=re.MULTILINE)[1:]:
        body = function.split("\n}\n", 1)[0]
        name = re.match(r"(Test\w+)\(t \*testing\.T\)", body)
        if name and "stagingIntegrationEnv(t)" in body:
            names.add(name.group(1))
    return names


def test_the_staging_script_runs_every_test_that_needs_the_executor():
    missing = executor_tests_in_the_go_file() - names_the_script_runs()

    assert not missing, f"tests the staging script never runs: {sorted(missing)}"


def test_every_name_the_staging_script_runs_is_such_a_test():
    unknown = names_the_script_runs() - executor_tests_in_the_go_file()

    assert not unknown, f"names in TESTS that no test of {TEST_FILE.name} has: {sorted(unknown)}"
