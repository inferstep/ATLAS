"""
Tests for Sandbox executor service.

Validates code execution, syntax checking, test running,
linting, and resource limits.
"""

import pytest

# importorskip, not a plain import — see test_llm.py: keeps collection
# alive on environments without the integration deps.
httpx = pytest.importorskip("httpx")


class TestSandboxHealth:
    """Test Sandbox service health."""

    def test_health_endpoint_responds(self, sandbox_client: httpx.Client):
        """Health endpoint should return 200 OK."""
        response = sandbox_client.get("/health")
        assert response.status_code == 200, f"Health endpoint should return 200, got {response.status_code}"


class TestSandboxExecution:
    """Test code execution in sandbox."""

    def test_execute_simple_code(self, sandbox_client: httpx.Client):
        """Execute simple print statement should return output."""
        response = sandbox_client.post(
            "/execute",
            json={
                "code": 'print("hello world")',
                "language": "python"
            },
            timeout=60.0
        )
        assert response.status_code == 200, f"Execute should return 200, got {response.status_code}"
        data = response.json()
        assert data.get("success") is True or data.get("compile_success") is True, "Simple code should succeed"
        assert "hello world" in data.get("stdout", ""), "Output should contain 'hello world'"

    def test_execute_returns_computed_values(self, sandbox_client: httpx.Client):
        """Code that computes values should capture output."""
        code = """
result = 2 + 3
print(f"Result: {result}")
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert "Result: 5" in data.get("stdout", ""), "Should capture computed result"

    def test_syntax_error_caught(self, sandbox_client: httpx.Client):
        """Syntax errors should be caught before execution."""
        code = """
def broken(
    # Missing closing parenthesis
print("never runs")
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("compile_success") is False or data.get("success") is False, "Syntax error should fail compilation"
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        # Error message may mention "syntax", "SyntaxError", or describe the issue like "was never closed"
        has_syntax_error = (
            "syntax" in error_msg.lower() or
            "SyntaxError" in error_msg or
            "never closed" in error_msg or
            "unexpected" in error_msg.lower()
        )
        assert has_syntax_error, f"Error should mention syntax issue: {error_msg}"

    def test_runtime_error_caught(self, sandbox_client: httpx.Client):
        """Runtime errors should be caught and reported."""
        code = """
x = 1 / 0
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "Runtime error should cause failure"
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert "ZeroDivision" in error_msg or "division" in error_msg.lower(), "Error should mention division"

    def test_import_error_caught(self, sandbox_client: httpx.Client):
        """Import errors should be caught and reported."""
        code = """
import nonexistent_module_xyz123
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "Import error should cause failure"
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert "ModuleNotFound" in error_msg or "No module" in error_msg or "import" in error_msg.lower(), \
            f"Error should mention import failure: {error_msg}"


class TestSandboxPytest:
    """Test pytest execution in sandbox."""

    def test_pytest_pass_returns_success(self, sandbox_client: httpx.Client):
        """Passing tests should return success with test count."""
        code = """
def add(a, b):
    return a + b
"""
        # Test code needs to import from solution.py
        test_code = """
from solution import add

def test_add():
    assert add(1, 2) == 3

def test_add_negative():
    assert add(-1, -1) == -2
"""
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "test_code": test_code,
                "language": "python"
            },
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Tests should pass: {data}"
        assert data.get("tests_run", 0) >= 2, f"Should run at least 2 tests, ran {data.get('tests_run')}"
        assert data.get("tests_passed", 0) >= 2, f"Should pass at least 2 tests, passed {data.get('tests_passed')}"

    def test_pytest_fail_returns_failure_details(self, sandbox_client: httpx.Client):
        """Failing tests should return failure with details."""
        code = """
def add(a, b):
    return a - b  # Bug: subtracts instead of adds
"""
        # Test code needs to import from solution.py
        test_code = """
from solution import add

def test_add():
    assert add(1, 2) == 3, "1 + 2 should equal 3"
"""
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "test_code": test_code,
                "language": "python"
            },
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "Failing test should return success=False"
        assert data.get("tests_run", 0) >= 1, "Should run at least 1 test"
        assert data.get("tests_passed", 0) < data.get("tests_run", 1), "Some tests should fail"


class TestSandboxLinting:
    """Test pylint scoring in sandbox."""

    def test_pylint_returns_score(self, sandbox_client: httpx.Client):
        """Pylint should return a score for the code."""
        code = '''
"""Good module."""

def add(a, b):
    """Add two numbers."""
    return a + b
'''
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        # Lint score may or may not be present depending on sandbox config
        lint_score = data.get("lint_score")
        if lint_score is not None:
            assert isinstance(lint_score, (int, float)), f"Lint score should be numeric, got {type(lint_score)}"
            assert 0 <= lint_score <= 10, f"Lint score should be 0-10, got {lint_score}"


class TestSandboxResourceLimits:
    """Test resource limits in sandbox."""

    @pytest.mark.slow
    def test_timeout_enforced_on_infinite_loop(self, sandbox_client: httpx.Client):
        """Infinite loop should be terminated by timeout."""
        code = """
while True:
    pass
"""
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "language": "python",
                "timeout": 5  # Request 5 second timeout
            },
            timeout=120.0  # HTTP timeout longer than code timeout
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "Infinite loop should fail"
        error_msg = data.get("stderr", "") + data.get("error_message", "") + data.get("error_type", "")
        assert "timeout" in error_msg.lower() or "timed out" in error_msg.lower() or "Timeout" in error_msg, \
            f"Error should mention timeout: {error_msg}"

    @pytest.mark.slow
    def test_memory_limit_enforced(self, sandbox_client: httpx.Client):
        """Memory-intensive code should be limited or handled gracefully."""
        # This test verifies memory-intensive code doesn't crash the sandbox
        # The exact behavior depends on system configuration
        code = """
# Try to allocate memory
data = []
for i in range(10000):
    data.append('x' * 10000)
print(f"Allocated {len(data)} items")
"""
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "language": "python",
                "timeout": 30
            },
            timeout=120.0
        )
        assert response.status_code == 200
        # Just verify the sandbox handles it without crashing
        # Memory limit enforcement depends on container configuration


class TestSandboxResponseFormat:
    """Test sandbox response format."""

    def test_response_has_required_fields(self, sandbox_client: httpx.Client):
        """Response should have all required fields."""
        response = sandbox_client.post(
            "/execute",
            json={"code": "print('test')", "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()

        # Check required fields exist
        assert "success" in data or "compile_success" in data, "Response should have success field"
        assert "stdout" in data or "output" in data, "Response should have stdout/output field"

    def test_execution_time_tracked(self, sandbox_client: httpx.Client):
        """Execution time should be tracked in response."""
        response = sandbox_client.post(
            "/execute",
            json={"code": "print('test')", "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()

        # Check for execution time field
        has_time = "execution_time_ms" in data or "execution_time" in data or "duration" in data
        if has_time:
            time_val = data.get("execution_time_ms") or data.get("execution_time") or data.get("duration")
            assert time_val >= 0, "Execution time should be non-negative"


class TestSandboxCodeExecution:
    """Test various code execution scenarios."""

    def test_execute_empty_string(self, sandbox_client: httpx.Client):
        """Empty string code should execute without crashing."""
        response = sandbox_client.post(
            "/execute",
            json={"code": "", "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200, "Empty code should not crash"

    def test_execute_only_whitespace(self, sandbox_client: httpx.Client):
        """Whitespace-only code should execute without error."""
        response = sandbox_client.post(
            "/execute",
            json={"code": "   \n\n   \t\t\n", "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200, "Whitespace code should not crash"

    def test_execute_only_comments(self, sandbox_client: httpx.Client):
        """Comment-only code should execute successfully."""
        code = "# This is a comment\n# Another comment"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True or data.get("compile_success") is True, "Comments should succeed"

    def test_execute_print_unicode(self, sandbox_client: httpx.Client):
        """Unicode output should be captured correctly."""
        code = 'print("Hello 世界 🌍")'
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        stdout = data.get("stdout", "")
        assert "世界" in stdout or "Hello" in stdout, f"Unicode output not captured: {stdout}"

    def test_execute_multiline_code(self, sandbox_client: httpx.Client):
        """Multi-line code should execute correctly."""
        code = """
x = 1
y = 2
z = x + y
print(f"Sum: {z}")
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert "Sum: 3" in data.get("stdout", ""), "Multi-line code should work"

    def test_execute_code_with_imports(self, sandbox_client: httpx.Client):
        """Code with standard library imports should work."""
        code = """
import math
print(f"Pi: {math.pi:.2f}")
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert "Pi: 3.14" in data.get("stdout", ""), "Standard library import should work"


class TestSandboxSyntaxErrors:
    """Test various syntax error handling."""

    def test_missing_colon(self, sandbox_client: httpx.Client):
        """Missing colon should be caught as syntax error."""
        code = "if True\n    print('x')"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False or data.get("compile_success") is False

    def test_mismatched_parentheses(self, sandbox_client: httpx.Client):
        """Mismatched parentheses should be caught."""
        code = "print((1 + 2)"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False or data.get("compile_success") is False

    def test_mismatched_quotes(self, sandbox_client: httpx.Client):
        """Mismatched quotes should be caught."""
        code = 'print("hello)'
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False or data.get("compile_success") is False

    def test_invalid_indentation(self, sandbox_client: httpx.Client):
        """Invalid indentation should be caught."""
        code = "def f():\nprint('bad indent')"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False or data.get("compile_success") is False


class TestSandboxRuntimeErrors:
    """Test various runtime error handling."""

    def test_name_error(self, sandbox_client: httpx.Client):
        """NameError should be caught."""
        code = "print(undefined_variable)"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "NameError should fail"
        error = data.get("stderr", "") + data.get("error_message", "")
        assert "NameError" in error or "undefined" in error.lower() or "not defined" in error

    def test_type_error(self, sandbox_client: httpx.Client):
        """TypeError should be caught."""
        code = "'string' + 5"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "TypeError should fail"

    def test_index_error(self, sandbox_client: httpx.Client):
        """IndexError should be caught."""
        code = "lst = [1, 2, 3]\nprint(lst[10])"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "IndexError should fail"

    def test_key_error(self, sandbox_client: httpx.Client):
        """KeyError should be caught."""
        code = "d = {'a': 1}\nprint(d['b'])"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "KeyError should fail"

    def test_zero_division_error(self, sandbox_client: httpx.Client):
        """ZeroDivisionError should be caught."""
        code = "x = 10 / 0"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "ZeroDivisionError should fail"

    def test_attribute_error(self, sandbox_client: httpx.Client):
        """AttributeError should be caught."""
        code = "x = 'string'\nprint(x.nonexistent_method())"
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, "AttributeError should fail"


class TestSandboxPytestAdvanced:
    """Test advanced pytest scenarios."""

    def test_single_passing_test(self, sandbox_client: httpx.Client):
        """Single passing test should report success."""
        code = "def double(x): return x * 2"
        test_code = """
from solution import double
def test_double():
    assert double(3) == 6
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "test_code": test_code, "language": "python"},
            timeout=60.0
        )
        data = response.json()
        assert data.get("success") is True, "Single passing test should succeed"

    def test_multiple_tests_all_pass(self, sandbox_client: httpx.Client):
        """Multiple passing tests should all be counted."""
        code = "def inc(x): return x + 1"
        test_code = """
from solution import inc
def test_inc_1(): assert inc(0) == 1
def test_inc_2(): assert inc(1) == 2
def test_inc_3(): assert inc(-1) == 0
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "test_code": test_code, "language": "python"},
            timeout=60.0
        )
        data = response.json()
        assert data.get("tests_run", 0) >= 3, f"Should run 3 tests, ran {data.get('tests_run')}"
        assert data.get("tests_passed", 0) >= 3, f"Should pass 3 tests, passed {data.get('tests_passed')}"

    def test_multiple_tests_some_fail(self, sandbox_client: httpx.Client):
        """Mixed test results should be reported correctly."""
        code = "def inc(x): return x + 1"
        test_code = """
from solution import inc
def test_pass(): assert inc(1) == 2
def test_fail(): assert inc(1) == 10  # Wrong expectation
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "test_code": test_code, "language": "python"},
            timeout=60.0
        )
        data = response.json()
        assert data.get("tests_run", 0) >= 2, "Should run 2 tests"
        assert data.get("tests_passed", 0) < data.get("tests_run", 0), "Some tests should fail"


class TestSandboxLintingAdvanced:
    """Test pylint scoring scenarios."""

    def test_pylint_good_code_high_score(self, sandbox_client: httpx.Client):
        """Well-documented code should get high lint score."""
        code = '''"""Module for arithmetic operations."""


def add(num_a: int, num_b: int) -> int:
    """Add two integers.

    Args:
        num_a: First number
        num_b: Second number

    Returns:
        Sum of the two numbers
    """
    return num_a + num_b
'''
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "python"},
            timeout=60.0
        )
        data = response.json()
        lint_score = data.get("lint_score")
        if lint_score is not None:
            assert lint_score >= 5.0, f"Good code should score >= 5, got {lint_score}"


class TestWorkingDirectoryContract:
    """Where a candidate executes from.

    `/execute` stages the request's `files` into a fresh per-request
    workspace and writes the candidate there as `solution.py`, but the
    plain-execution branch launched the process without a `cwd`, so it ran
    from the image WORKDIR — a read-only directory in a `read_only: true`
    container. A candidate told to read `input.txt` therefore could not see
    the file the same request had just staged for it, and any relative write
    raised `[Errno 30] Read-only file system`. The pytest branch two lines
    away already passed `cwd=workspace`, so the two ways of running the same
    candidate disagreed about where it stood.
    """

    REPORT = (
        "import os\n"
        "print('CWD=' + os.getcwd())\n"
        "print('LISTING=' + ','.join(sorted(os.listdir('.'))))\n"
        "try:\n"
        "    print('READ=' + repr(open('input.txt').read()))\n"
        "except OSError as e:\n"
        "    print('READ_FAIL=' + type(e).__name__)\n"
        "try:\n"
        "    open('scratch.txt', 'w').write('x')\n"
        "    print('WRITE=ok')\n"
        "except OSError as e:\n"
        "    print('WRITE_FAIL=' + str(e))\n"
    )

    @staticmethod
    def _fields(stdout: str):
        out = {}
        for line in stdout.splitlines():
            if "=" in line:
                k, v = line.split("=", 1)
                out[k] = v
        return out

    def test_execution_starts_in_the_per_request_workspace(self, sandbox_client):
        """The staged file is readable by the name the task states."""
        response = sandbox_client.post(
            "/execute",
            json={"code": self.REPORT, "language": "python",
                  "files": {"input.txt": "push 1\npush 2\n"}},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data["success"] is True, data.get("stderr")
        f = self._fields(data["stdout"])

        # The workspace is where the request's files were staged, and it is
        # where the candidate runs.
        assert f["CWD"].startswith("/tmp/sandbox/"), f["CWD"]
        assert "input.txt" in f["LISTING"], f["LISTING"]
        assert "solution.py" in f["LISTING"], f["LISTING"]
        assert f.get("READ") == repr("push 1\npush 2\n"), f
        assert f.get("WRITE") == "ok", f

    def test_the_pytest_branch_already_ran_from_the_workspace(self, sandbox_client):
        """Pin on the neighbouring branch, so a regression there cannot make
        the test above pass for the wrong reason."""
        test_code = (
            "import os\n"
            "def test_cwd_is_the_workspace():\n"
            "    assert os.getcwd().startswith('/tmp/sandbox/')\n"
            "    assert open('input.txt').read() == 'staged\\n'\n"
        )
        response = sandbox_client.post(
            "/execute",
            json={"code": "x = 1\n", "language": "python",
                  "test_code": test_code,
                  "files": {"input.txt": "staged\n"}},
            timeout=90.0,
        )
        data = response.json()
        assert data["tests_passed"] >= 1, data.get("stdout", "")[-800:]

    def test_the_container_root_stays_read_only(self, sandbox_client):
        """Running from the workspace must not have made anything else
        writable. `/app`, `/etc` and `/` stay exactly as hardened as before."""
        code = (
            "for p in ('/app/x', '/etc/x', '/x'):\n"
            "    try:\n"
            "        open(p, 'w').write('x')\n"
            "        print('WROTE ' + p)\n"
            "    except OSError as e:\n"
            "        print('BLOCKED ' + p + ' ' + type(e).__name__)\n"
        )
        data = sandbox_client.post(
            "/execute", json={"code": code, "language": "python"},
            timeout=60.0).json()
        assert "WROTE" not in data["stdout"], data["stdout"]
        for p in ("/app/x", "/etc/x", "/x"):
            assert f"BLOCKED {p}" in data["stdout"], data["stdout"]

    def test_each_request_gets_its_own_workspace_and_it_is_removed(
            self, sandbox_client):
        """Distinct per request, and gone once the request is answered."""
        import concurrent.futures

        def one():
            data = sandbox_client.post(
                "/execute",
                json={"code": "import os\nprint('CWD=' + os.getcwd())\n",
                      "language": "python"},
                timeout=60.0).json()
            return self._fields(data["stdout"])["CWD"]

        with concurrent.futures.ThreadPoolExecutor(max_workers=3) as pool:
            paths = list(pool.map(lambda _: one(), range(3)))
        assert len(set(paths)) == 3, paths

        probe = ("import os\n"
                 + "".join(f"print({p!r} + '=' + str(os.path.exists({p!r})))\n"
                           for p in paths))
        data = sandbox_client.post(
            "/execute", json={"code": probe, "language": "python"},
            timeout=60.0).json()
        for p in paths:
            assert f"{p}=False" in data["stdout"], data["stdout"]

    def test_failure_reporting_is_unchanged(self, sandbox_client):
        """A candidate that raises is still a failure with its traceback,
        and the cwd change must not swallow either."""
        data = sandbox_client.post(
            "/execute",
            json={"code": "raise ValueError('boom')\n", "language": "python"},
            timeout=60.0).json()
        assert data["success"] is False
        assert "ValueError" in data["stderr"] and "boom" in data["stderr"]
