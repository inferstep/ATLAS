"""
Tests for Java and Kotlin sandbox executor support.

Validates code execution, syntax checking, class name extraction,
compile errors, and runtime errors for JVM languages.

The code runs in the sandbox service, which has the JDK and Kotlin. No
test here is skipped for what the machine that runs pytest has: a sandbox
that cannot run a language fails its tests.
"""

import pytest

# importorskip, not a plain import — see test_llm.py: keeps collection
# alive on environments without the integration deps.
httpx = pytest.importorskip("httpx")



class TestJavaExecution:
    """Test Java code execution in sandbox."""

    def test_hello_world(self, sandbox_client: httpx.Client):
        """Basic javac → java pipeline: compile and run."""
        code = """\
public class Main {
    public static void main(String[] args) {
        System.out.println("hello world");
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Hello world should succeed: {data}"
        assert data.get("compile_success") is True
        assert "hello world" in data.get("stdout", "")

    def test_computed_values(self, sandbox_client: httpx.Client):
        """Code that computes values should capture output."""
        code = """\
public class Main {
    public static void main(String[] args) {
        int a = 2, b = 3;
        System.out.println("Result: " + (a + b));
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True
        assert "Result: 5" in data.get("stdout", "")

    def test_custom_class_name(self, sandbox_client: httpx.Client):
        """Public class name extraction: file must be Calculator.java."""
        code = """\
public class Calculator {
    public static void main(String[] args) {
        System.out.println("sum=" + (10 + 20));
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, (
            f"Custom class name should work: {data}"
        )
        assert "sum=30" in data.get("stdout", "")

    def test_compile_error(self, sandbox_client: httpx.Client):
        """Missing semicolon should fail compilation."""
        code = """\
public class Main {
    public static void main(String[] args) {
        int x = 1
        System.out.println(x);
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("compile_success") is False
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert "error" in error_msg.lower(), (
            f"Compile error not reported: {error_msg}"
        )

    def test_runtime_error_npe(self, sandbox_client: httpx.Client):
        """NullPointerException should be caught as runtime error."""
        code = """\
public class Main {
    public static void main(String[] args) {
        String s = null;
        System.out.println(s.length());
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("compile_success") is True
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert "NullPointerException" in error_msg, (
            f"NPE not reported: {error_msg}"
        )

    def test_runtime_error_division_by_zero(self, sandbox_client: httpx.Client):
        """ArithmeticException from integer division by zero."""
        code = """\
public class Main {
    public static void main(String[] args) {
        int x = 1 / 0;
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert (
            "ArithmeticException" in error_msg
            or "/ by zero" in error_msg
        )

    def test_import_nonexistent_package(self, sandbox_client: httpx.Client):
        """Importing a package that does not exist should fail compilation."""
        code = """\
import xyz.NonExistent;
public class Main {
    public static void main(String[] args) {
        NonExistent obj = new NonExistent();
    }
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert (
            "package" in error_msg.lower()
            or "does not exist" in error_msg.lower()
        ), f"Error should mention missing package: {error_msg}"


class TestJavaSyntaxCheck:
    """Test /syntax-check endpoint for Java."""

    def test_valid_java(self, sandbox_client: httpx.Client):
        """Well-formed Java should pass syntax check."""
        code = """\
public class Main {
    public static void main(String[] args) {
        System.out.println("ok");
    }
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, f"Valid Java rejected: {data}"
        assert data.get("errors") == [] or data.get("errors") is None

    def test_invalid_java(self, sandbox_client: httpx.Client):
        """Broken Java should fail syntax check with error details."""
        code = """\
public class Main {
    public static void main(String[] args) {
        int x = 1
    }
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "java"},
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is False
        errors = data.get("errors", [])
        assert len(errors) > 0, "Should report at least one error"


class TestJavaLanguagesEndpoint:
    """Test /languages reports Java."""

    def test_java_in_languages(self, sandbox_client: httpx.Client):
        """Java should appear in the /languages response."""
        response = sandbox_client.get("/languages")
        assert response.status_code == 200
        data = response.json()
        languages = data.get("languages", {})
        assert "java" in languages, (
            f"Java missing from /languages: {list(languages.keys())}"
        )


class TestJavaPathSafety:
    """Regression tests for path-traversal rejection in /syntax-check.
    The guard is in Python, not javac."""

    def test_reject_dot_dot_filename(self, sandbox_client: httpx.Client):
        """Filename with ../ should be rejected as unsafe."""
        code = 'public class Main { public static void main(String[] a) {} }'
        response = sandbox_client.post(
            "/syntax-check",
            json={
                "code": code,
                "language": "java",
                "filename": "../../../etc/passwd.java",
            },
            timeout=60.0,
        )
        assert response.status_code == 400, (
            f"../ filename should be rejected, got {response.status_code}"
        )

    def test_reject_absolute_filename(self, sandbox_client: httpx.Client):
        """Absolute path filename should be rejected as unsafe."""
        code = 'public class Main { public static void main(String[] a) {} }'
        response = sandbox_client.post(
            "/syntax-check",
            json={
                "code": code,
                "language": "java",
                "filename": "/tmp/evil.java",
            },
            timeout=60.0,
        )
        assert response.status_code == 400, (
            f"Absolute filename should be rejected, got {response.status_code}"
        )

class TestJavaPackagedClass:
    """package com.example file should also run safely"""
    def test_packaged_class(self, sandbox_client: httpx.Client):
        code = '''
package com.example;

public class Main {
    public static void main(String[] args) {
        System.out.println("hello");
    }
}
    '''
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "language": "java", 
            },
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Packaged class failed: {data}"
        assert "hello" in data.get("stdout", ""), f"Unexpected output: {data}"

    def test_malicious_package_ignored(self, sandbox_client: httpx.Client):
        """Malicious package name should be ignored (falls back to flat structure)."""
        code = '''
package ../../evil;
public class Main {
    public static void main(String[] args) {
        System.out.println("hello");
    }
}
'''
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "language": "java", 
            },
            timeout=60.0,
        )
        assert response.status_code == 200
        # Wait, javac will actually throw a compile error because 'package ../../evil;' is invalid syntax!
        # The executor gracefully handles this as a compile error because the package extraction ignored it, 
        # so it treated it as a flat file and just passed it to javac.
        data = response.json()
        assert data.get("success") is False
        assert data.get("compile_success") is False
        assert "error:" in data.get("stderr", "")

    def test_advanced_public_modifiers(self, sandbox_client: httpx.Client):
        """Regex should correctly extract the class name bypassing modifiers."""
        code = '''
public final class SecureApp {
    public static void main(String[] args) {
        System.out.println("secure execution");
    }
}
'''
        response = sandbox_client.post(
            "/execute",
            json={
                "code": code,
                "language": "java",
            },
            timeout=60.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True, f"Advanced modifiers failed: {data}"
        assert "secure execution" in data.get("stdout", "")


# Kotlin has no filename-class-match rule (unlike Java's public class Foo ->
# Foo.java) and no package-directory requirement, so there are no class-name
# or package-structure tests below — those cases don't exist for Kotlin.
# Compile output is a single fat jar run via `java -jar`.

class TestKotlinExecution:
    """Test Kotlin code execution in sandbox."""

    def test_hello_world(self, sandbox_client: httpx.Client):
        """Basic kotlinc -> java -jar pipeline: compile and run."""
        code = """\
fun main() {
    println("hello world")
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "kotlin"},
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
fun main() {
    val a = 2
    val b = 3
    println("Result: ${a + b}")
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("success") is True
        assert "Result: 5" in data.get("stdout", "")

    def test_compile_error(self, sandbox_client: httpx.Client):
        """Unterminated string literal should fail compilation."""
        code = """\
fun main() {
    println("unterminated
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("compile_success") is False
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert "error" in error_msg.lower(), (
            f"Compile error not reported: {error_msg}"
        )

    def test_runtime_error_npe(self, sandbox_client: httpx.Client):
        """Null-assertion (!!) on null should throw NullPointerException."""
        code = """\
fun main() {
    val s: String? = null
    println(s!!.length)
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("compile_success") is True
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert "NullPointerException" in error_msg, (
            f"NPE not reported: {error_msg}"
        )

    def test_runtime_error_division_by_zero(self, sandbox_client: httpx.Client):
        """Integer division by zero throws ArithmeticException at runtime.

        The divisor is a variable, not a literal — kotlinc rejects a literal
        `1 / 0` at compile time, so a var defers the failure to runtime.
        """
        code = """\
fun main() {
    val zero = 0
    println(1 / zero)
}
"""
        response = sandbox_client.post(
            "/execute",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("compile_success") is True
        assert data.get("success") is False
        error_msg = data.get("stderr", "") + data.get("error_message", "")
        assert (
            "ArithmeticException" in error_msg
            or "/ by zero" in error_msg
        )


class TestKotlinSyntaxCheck:
    """Test /syntax-check endpoint for Kotlin."""

    def test_valid_kotlin(self, sandbox_client: httpx.Client):
        """Well-formed Kotlin should pass syntax check."""
        code = """\
fun main() {
    println("ok")
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, f"Valid Kotlin rejected: {data}"
        assert data.get("errors") == [] or data.get("errors") is None

    def test_invalid_kotlin(self, sandbox_client: httpx.Client):
        """Broken Kotlin should fail syntax check with error details."""
        code = """\
fun main() {
    val x: Int =
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is False
        errors = data.get("errors", [])
        assert len(errors) > 0, "Should report at least one error"

    def test_warning_only_is_valid(self, sandbox_client: httpx.Client):
        """Warning-only code must pass — a kotlinc `w:` warning line that
        merely mentions 'error' must not be mis-parsed as a real error.
        Regression guard for the syntax-check error-matching fix."""
        code = """\
fun main() {
    val unused = 42
    println("ok")
}
"""
        response = sandbox_client.post(
            "/syntax-check",
            json={"code": code, "language": "kotlin"},
            timeout=90.0,
        )
        assert response.status_code == 200
        data = response.json()
        assert data.get("valid") is True, (
            f"Warning-only Kotlin should be valid: {data}"
        )


class TestKotlinLanguagesEndpoint:
    """Test /languages reports Kotlin."""

    def test_kotlin_in_languages(self, sandbox_client: httpx.Client):
        """Kotlin should appear in the /languages response."""
        response = sandbox_client.get("/languages")
        assert response.status_code == 200
        data = response.json()
        languages = data.get("languages", {})
        assert "kotlin" in languages, (
            f"Kotlin missing from /languages: {list(languages.keys())}"
        )

