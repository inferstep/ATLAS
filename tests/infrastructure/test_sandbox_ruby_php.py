"""
Tests for Ruby and PHP sandbox executor support.

Validates code execution, syntax checking, language detection via /languages
endpoint, compile errors, and runtime errors for both languages.

The code runs in the sandbox service, which has Ruby and PHP. No test here
is skipped for what the machine that runs pytest has: a sandbox that cannot
run a language fails its tests.
"""

import pytest

# importorskip, not a plain import — see test_llm.py: keeps collection
# alive on environments without the integration deps.
httpx = pytest.importorskip("httpx")



class TestRubyExecution:
    """Test Ruby code execution in sandbox."""
    def test_hello_world(self, sandbox_client: httpx.Client):
        """Basic ruby -c -> ruby pipeline: syntax check and run."""
        code = """\
puts "hello world"
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "ruby"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Hello world should succeed: {data}"
        assert data.get("compile_success") is True
        assert "hello world" in data.get("stdout", "")

    def test_computed_values(self, sandbox_client: httpx.Client):
        """Code that computes values should capture output."""
        code = """\
a = 2
b = 3
puts "Result: #{a + b}"
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "ruby"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Computed values should succeed: {data}"
        assert "Result: 5" in data.get("stdout", "")

    def test_compile_error(self, sandbox_client: httpx.Client):
        """Code with a syntax error should fail at the ruby -c stage."""
        code = """\
def broken
  puts "missing end"
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "ruby"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, f"Broken syntax should fail: {data}"
        assert data.get("compile_success") is False
        assert data.get("error_type") == "SyntaxError"

    def test_runtime_error_nomethod(self, sandbox_client: httpx.Client):
        """Calling a method on nil should raise NoMethodError (Ruby's nil-dereference equivalent)."""
        code = """\
x = nil
puts x.upcase
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "ruby"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, f"NoMethodError should fail: {data}"
        assert data.get("compile_success") is True
        assert "NoMethodError" in data.get("stderr", "")

    def test_runtime_error_division_by_zero(self, sandbox_client: httpx.Client):
        """Integer division by zero should raise ZeroDivisionError."""
        code = """\
a = 10
b = 0
puts a / b
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "ruby"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, f"Division by zero should fail: {data}"
        assert data.get("compile_success") is True
        assert "ZeroDivisionError" in data.get("stderr", "")


class TestRubySyntaxCheck:
    """Test Ruby syntax checking via /syntax-check."""

    def test_valid_ruby(self, sandbox_client: httpx.Client):
        """Valid syntax should report no errors."""
        code = """\
def main
  puts "ok"
end
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "ruby"},
            timeout=30.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, f"Valid Ruby should pass: {data}"
        assert not data.get("errors")

    def test_invalid_ruby(self, sandbox_client: httpx.Client):
        """Syntax error should report errors and fail validity."""
        code = """\
def main
  x =
end
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "ruby"},
            timeout=30.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is False, f"Invalid Ruby should fail: {data}"
        assert data.get("errors")

    def test_warning_only_is_valid(self, sandbox_client: httpx.Client):
        """Unused variable is a warning, not a syntax error — should still be valid."""
        code = """\
def main
  unused = 42
  puts "ok"
end
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "ruby"},
            timeout=30.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, f"Warning-only Ruby should still be valid: {data}"


class TestRubyLanguagesEndpoint:
    """Test /languages reports Ruby."""

    def test_ruby_in_languages(self, sandbox_client: httpx.Client):
        """Ruby should appear in the /languages response."""
        response = sandbox_client.get("/languages")
        assert response.status_code == 200
        data = response.json()
        languages = data.get("languages", {})
        assert "ruby" in languages, (
            f"Ruby missing from /languages: {list(languages.keys())}"
        )

# --- PHP ---

class TestPHPExecution:
    """Test PHP code execution in sandbox."""

    def test_hello_world(self, sandbox_client: httpx.Client):
        """Basic php -l -> php pipeline: lint and run."""
        code = """\
<?php
echo "hello world";
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "php"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Hello world should succeed: {data}"
        assert data.get("compile_success") is True
        assert "hello world" in data.get("stdout", "")

    def test_computed_values(self, sandbox_client: httpx.Client):
        """Code that computes values should capture output."""
        code = """\
<?php
$a = 2;
$b = 3;
echo "Result: " . ($a + $b);
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "php"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Computed values should succeed: {data}"
        assert "Result: 5" in data.get("stdout", "")

    def test_compile_error(self, sandbox_client: httpx.Client):
        """Code with a syntax error should fail at the php -l stage."""
        code = """\
<?php
function broken() {
    echo "missing brace"
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "php"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, f"Broken syntax should fail: {data}"
        assert data.get("compile_success") is False
        assert data.get("error_type") == "SyntaxError"

    def test_runtime_error_null(self, sandbox_client: httpx.Client):
        """Calling a method on null should raise an Error (PHP's null-dereference equivalent)."""
        code = """\
<?php
$x = null;
echo $x->upper();
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "php"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, f"Null method call should fail: {data}"
        assert data.get("compile_success") is True
        assert "Error" in data.get("stderr", "")

    def test_runtime_error_division_by_zero(self, sandbox_client: httpx.Client):
        """Integer division by zero should raise DivisionByZeroError."""
        code = """\
<?php
$a = 10;
$b = 0;
echo intdiv($a, $b);
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "php"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False, f"Division by zero should fail: {data}"
        assert data.get("compile_success") is True
        assert "DivisionByZeroError" in data.get("stderr", "")


class TestPHPSyntaxCheck:
    """Test PHP syntax checking via /syntax-check."""

    def test_valid_php(self, sandbox_client: httpx.Client):
        """Valid syntax should report no errors."""
        code = """\
<?php
function main() {
    echo "ok";
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "php"},
            timeout=30.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, f"Valid PHP should pass: {data}"
        assert not data.get("errors")

    def test_invalid_php(self, sandbox_client: httpx.Client):
        """Syntax error should report errors and fail validity."""
        code = """\
<?php
function main() {
    $x =
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "php"},
            timeout=30.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is False, f"Invalid PHP should fail: {data}"
        assert data.get("errors")

    def test_warning_only_is_valid(self, sandbox_client: httpx.Client):
        """Unused variable is a notice, not a syntax error — should still be valid."""
        code = """\
<?php
function main() {
    $unused = 42;
    echo "ok";
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "php"},
            timeout=30.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, f"Warning-only PHP should still be valid: {data}"


class TestPHPLanguagesEndpoint:
    """Test /languages reports PHP."""

    def test_php_in_languages(self, sandbox_client: httpx.Client):
        """PHP should appear in the /languages response."""
        response = sandbox_client.get("/languages")
        assert response.status_code == 200
        data = response.json()
        languages = data.get("languages", {})
        assert "php" in languages, (
            f"PHP missing from /languages: {list(languages.keys())}"
        )

