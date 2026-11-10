"""Per-language syntax verification and execution runners for the sandbox executor."""

from __future__ import annotations

import json
import logging
import os
import re
import time
from pathlib import Path
from typing import Dict, List, Optional

from fastapi import HTTPException

from executor_models import ExecuteResponse
from executor_parsers import _html_syntax_errors, _xml_syntax_errors, _yaml_syntax_errors
from resource_contract import (
    OUTCOME_TIMED_OUT,
    check_process_supervision,
    contract_from_env,
    outcome_is_complete,
    run_bounded,
    stopped_words,
    why_it_failed,
)

logger = logging.getLogger(__name__)

# THE resource contract for every untrusted command, validated here at import
# and imported by executor_server, so the language runners and the background
# job spawner share one object. A malformed operator budget stops the executor
# from starting rather than being clamped into something plausible at the
# moment a command runs.
EXEC_CONTRACT = contract_from_env()
# The one supervision result. executor_server reads it through this module at
# call time rather than copying it, so /health and every result agree.
SUPERVISION_AVAILABLE, SUPERVISION_REASON = check_process_supervision()
if not SUPERVISION_AVAILABLE:
    logger.warning("Resource limits: %s", SUPERVISION_REASON)


def _resource_limits_label() -> str:
    return "enforced" if SUPERVISION_AVAILABLE else "resource limits not enforced"


def _safe_overlay_path(name: str) -> Path:
    if not isinstance(name, str) or not name:
        raise HTTPException(status_code=400, detail="overlay file path is required")
    rel = Path(name)
    if rel.is_absolute() or name.startswith("\\") or ".." in rel.parts:
        raise HTTPException(status_code=400, detail=f"unsafe overlay file path: {name!r}")
    return rel


def _contained_path(base: Path, *parts: str) -> Path:
    """Join parts under base, enforcing containment.

    The inputs are already constrained (request filenames pass
    _safe_overlay_path; Java class/package names come from restrictive
    regexes), but those guards raise instead of transforming the value,
    which CodeQL's py/path-injection taint tracking cannot follow. This
    normpath + prefix check is the sanitizer form the query recognizes,
    so new language branches stop accruing one alert per PR.
    """
    resolved = os.path.normpath(os.path.join(str(base), *parts))
    if not resolved.startswith(str(base) + os.sep):
        raise HTTPException(status_code=400,
                            detail=f"unsafe file path: {'/'.join(parts)!r}")
    path = Path(resolved)
    # Every caller writes a check/source file under a fresh temp workspace, and
    # the filename may carry a subdirectory ("src/app.py", "static/app.js").
    # Create the parent so the write does not raise FileNotFoundError -- which
    # would make the syntax check report a valid file as unparseable, and (once
    # the proxy started sending real file paths) refuse a legitimate write of
    # any gated source file that lives in a subdirectory.
    path.parent.mkdir(parents=True, exist_ok=True)
    return path



def _extract_java_package(code: str) -> str | None:
    """Extract package name from Java source, return None if no package decl."""
    match = re.search(r'^\s*package\s+([\w.$]+)\s*;', code, re.MULTILINE)
    if not match:
        return None
    package_name = match.group(1)
    
    # VALIDATE: each dot-segment must be safe Java identifier
    segments = package_name.split('.')
    for seg in segments:
        if not re.fullmatch(r'[A-Za-z_$][A-Za-z0-9_$]*', seg):
            return None  # malformed/malicious package name, ignore it
    
    return package_name

def _extract_java_classname(code: str) -> str:
    """
    Extracts the public class, interface, enum, or record name from Java code.
    Defaults to 'Main' if no public type is found.
    """

    pattern = r"\bpublic\s+(?:(?:abstract|final|strictfp|sealed|non-sealed|@[A-Za-z0-9_$.]+)\s+)*(?:class|interface|enum|record)\s+([A-Za-z_$][A-Za-z0-9_$]*)"
    
    match = re.search(pattern, code)
    if match:
        return match.group(1)
    
    return "Main"


class _CheckNotFinished(Exception):
    """A syntax checker stopped before it reached a verdict."""

    def __init__(self, outcome: str):
        super().__init__(outcome)
        self.outcome = outcome


def _run_check(cmd: List[str], timeout: int, cwd: Path) -> Dict:
    """Run one syntax checker to a verdict, or raise _CheckNotFinished.

    A checker killed at its wall clock or memory ceiling, or one that never
    started, exits non-zero with empty stderr. Every branch of
    _syntax_check_impl reads errors out of stderr, so such a run used to come
    back with no errors -- valid -- for python, typescript, go, java, kotlin,
    rust, c/cpp, ruby and php alike.
    """
    result = _run_cmd(cmd, timeout=timeout, cwd=cwd)
    if not outcome_is_complete(result["outcome"]):
        raise _CheckNotFinished(result["outcome"])
    return result


# What a CommonJS parse says about code that is wrong only because it is an
# ES module.
_ESM_ONLY_ERRORS = (
    "Cannot use import statement outside a module",
    "Unexpected token 'export'",
    "Cannot use 'import.meta' outside a module",
    "await is only valid in async functions and the top level bodies of modules",
)


def _javascript_syntax_errors(code: str, workspace: Path, filename: Optional[str]) -> List[str]:
    """`node --check` JavaScript as the module type its name fixes.

    Checking a typeless .js name left the module type to Node. From Node
    20.19, a file with import/export is detected as a module and `--check`
    then compiles nothing, so garbage or a truncated module exited 0; before
    20.19 every valid module was rejected. A .cjs name is always CommonJS and
    a .mjs name always a module, on every Node version: a .js file is checked
    as CommonJS, and again as a module when that parse failed only on module
    syntax.
    """
    name = filename or "check.js"
    reported = _contained_path(workspace, name)
    suffix = reported.suffix.lower()
    order = [suffix] if suffix in (".cjs", ".mjs") else [".cjs", ".mjs"]
    for i, ext in enumerate(order):
        path = reported.with_suffix(ext)
        path.write_text(code)
        result = _run_check(["node", "--check", str(path)], timeout=5, cwd=workspace)
        if result["returncode"] == 0:
            return []
        stderr = result.get("stderr", "").strip()
        if i + 1 < len(order) and any(m in stderr for m in _ESM_ONLY_ERRORS):
            continue
        return [stderr.replace(str(path), str(reported)) or "SyntaxError"]
    return []


# tsc's diagnostic codes 1000-1999 are syntax and grammar errors. Everything
# else it reports about one file checked alone is about what that file refers
# to: a sibling module or a package it cannot see (TS2307), types (TS2322),
# strictness (TS7006). A few 1xxx codes are about compiler options, not the
# text, and are left out too.
_TS_ERROR_RE = re.compile(r"error TS(\d+)")
_TS_OPTION_CODES = {1208, 1259, 1343, 1375, 1378, 1470, 1479}


def _is_ts_syntax_code(code: int) -> bool:
    return 1000 <= code < 2000 and code not in _TS_OPTION_CODES


def _typescript_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`tsc` on the TypeScript file alone; only its syntax diagnostics count."""
    errors = []
    # tsc on the one file; only its syntax diagnostics count (see
    # _is_ts_syntax_code). A lone-file check cannot see the modules the
    # file imports, so its other errors are about the check, not the code.
    result = _run_check(["tsc", "--noEmit", "--strict", str(fpath)], timeout=10, cwd=workspace)
    if result["returncode"] != 0:
        output = result.get("stdout", "") + "\n" + result.get("stderr", "")
        for line in output.splitlines():
            match = _TS_ERROR_RE.search(line)
            if match and _is_ts_syntax_code(int(match.group(1))):
                errors.append(line.strip())
        # tsc failed without a diagnostic of its own: say what it said.
        if not errors and not _TS_ERROR_RE.search(output) and output.strip():
            errors.append(output.strip().splitlines()[-1])
    return errors


def _go_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`gofmt -e` on the Go file: a parse, with no build."""
    errors = []
    # Use gofmt -e for fast syntax-only checking (no compilation, no go.mod needed)
    result = _run_check(["gofmt", "-e", str(fpath)], timeout=5, cwd=workspace)
    if result["returncode"] != 0:
        stderr = result.get("stderr", "")
        for line in stderr.splitlines():
            line = line.strip()
            if line:
                errors.append(line)
    return errors


def _java_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`javac`, parse only, on the Java file."""
    errors = []
    # Parse only (-XDshould-stop.ifNoError=PARSE): compiling the file alone
    # reported every reference to a sibling class or a package it could
    # not see ("cannot find symbol", "package ... does not exist") as a
    # syntax error, so valid multi-file code was refused.
    result = _run_check(
        ["javac", "-XDshould-stop.ifNoError=PARSE", "-d", str(workspace), str(fpath)],
        timeout=10, cwd=workspace
    )
    if result["returncode"] != 0:
        stderr = result.get("stderr", "")
        for line in stderr.splitlines():
            if "error:" in line:
                errors.append(line.strip())
        if not errors and stderr.strip():
            errors.append(stderr.strip().split("\n")[-1])
    return errors


def _kotlin_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`kotlinc` on the Kotlin file; only its syntax errors count."""
    errors = []
    classes_dir = workspace / "synccheck_out"
    classes_dir.mkdir(exist_ok=True)

    # No syntax-only check in kotlinc. Full compile to temp
    # output dir is the check, same approach as Java
    result = _run_check(
        ["kotlinc", "-d", str(classes_dir), str(fpath)],
        timeout=30, cwd=workspace
    )
    if result["returncode"] != 0:
        stderr = result.get("stderr", "")
        diagnosed = False
        for line in stderr.splitlines():
            # kotlinc emits errors as either an `e:`-prefixed line or a
            # `file.kt:L:C: error:` line. Match both — but NOT a bare
            # "error" substring, which also hits `w:` warning lines that
            # merely mention the word (false-positive syntax failures).
            if line.strip().startswith("e:") or ": error:" in line:
                diagnosed = True
                # Only parse errors, which Kotlin 2 labels "syntax
                # error:". kotlinc has no parse-only mode; compiling the
                # file alone makes every sibling class and library an
                # "unresolved reference", and a type mismatch is not a
                # syntax error either.
                if "syntax error" in line.lower():
                    errors.append(line.strip())
        if not diagnosed and stderr.strip():
            errors.append(stderr.strip().split("\n")[-1])
    return errors


def _rust_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`rustc` on the Rust file, with no output kept."""
    errors = []
    # rustc --edition 2021 with no codegen for syntax-only
    result = _run_check(
        ["rustc", "--edition", "2021", "--crate-type", "bin", str(fpath), "-o", "/dev/null"],
        timeout=10, cwd=workspace
    )
    if result["returncode"] != 0:
        stderr = result.get("stderr", "")
        for line in stderr.splitlines():
            if "error" in line.lower():
                errors.append(line.strip())
        if not errors and stderr.strip():
            errors.append(stderr.strip().split("\n")[-1])
    return errors


def _c_or_cpp_syntax_errors(lang: str, fpath: Path, workspace: Path) -> List[str]:
    """`gcc` or `g++` (by lang) with -fsyntax-only on the C or C++ file."""
    errors = []
    compiler = "gcc" if lang == "c" else "g++"
    flags = ["-std=c17"] if lang == "c" else ["-std=c++17"]
    # -fsyntax-only: parse and type-check only, no codegen
    result = _run_check(
        [compiler] + flags + ["-fsyntax-only", str(fpath)],
        timeout=10, cwd=workspace
    )
    if result["returncode"] != 0:
        stderr = result.get("stderr", "")
        for line in stderr.splitlines():
            if "error:" in line:
                errors.append(line.strip())
        if not errors and stderr.strip():
            errors.append(stderr.strip().split("\n")[-1])
    return errors


def _ruby_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`ruby -c` on the Ruby file: a syntax check that runs nothing."""
    errors = []
    # Use ruby -c for syntax-only checking (no execution)
    result = _run_check(["ruby", "-c", str(fpath)], timeout=5, cwd=workspace)
    if result["returncode"] != 0:
        stderr = result.get("stderr", "")
        for line in stderr.splitlines():
            if "syntax error" in line or "error" in line.lower():
                errors.append(line.strip())
        if not errors and stderr.strip():
            errors.append(stderr.strip().split("\n")[-1])
    return errors


def _php_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`php -l` on the PHP file: a lint that runs nothing."""
    errors = []
    # Use php -l for lint/syntax-only checking (no execution)
    result = _run_check(["php", "-l", str(fpath)], timeout=5, cwd=workspace)
    if result["returncode"] != 0:
        # The real "PHP Parse error: ..." detail goes to stderr (with
        # display_errors=Off, the Debian CLI default); stdout carries
        # only the generic "Errors parsing main.php" summary. Scan both
        # so builds that route the message to stdout still work.
        output = (result.get("stderr", "") + "\n" + result.get("stdout", "")).strip()
        for line in output.splitlines():
            line = line.strip()
            # PHP explicitly tags syntax issues as "Parse error" or "Fatal error"
            if "parse error" in line.lower() or "error" in line.lower():
                # Filter out PHP's generic summary line "Errors parsing main.php"
                if not line.startswith("Errors parsing"):
                    errors.append(line)
        if not errors and output:
            errors.append(output.split("\n")[-1])
    return errors


def _bash_syntax_errors(fpath: Path, workspace: Path) -> List[str]:
    """`bash -n` on the script: a parse that runs nothing."""
    errors = []
    result = _run_check(["bash", "-n", str(fpath)], timeout=5, cwd=workspace)
    if result["returncode"] != 0:
        errors.append(result.get("stderr", "").strip())
    return errors


def _syntax_check_impl(lang: str, code: str, workspace: Path, filename: Optional[str] = None) -> List[str]:  # noqa: C901
    """Language-specific syntax checking. Returns list of error strings."""
    # Reject path-traversal filenames (absolute, .., backslash escapes)
    # before any language branch can write outside the workspace.
    if filename:
        _safe_overlay_path(filename)
    errors = []

    if lang == "python":
        # Use py_compile for fast AST parse
        fpath = _contained_path(workspace, filename or "check.py")
        fpath.write_text(code)
        result = _run_check(["python3", "-m", "py_compile", str(fpath)], timeout=5, cwd=workspace)
        if result["returncode"] != 0:
            # py_compile reports the location and the error on separate lines:
            #
            #   File "/w/check.py", line 5
            #     def __init__(,filename="todos.json"):
            #                  ^
            #   SyntaxError: invalid syntax
            #
            # Keeping only the SyntaxError line drops the one fact that makes
            # the error actionable. The caller quotes the offending source line
            # back to the model, but it locates that line by finding "line N"
            # in this string, so discarding the File frame left it with nothing
            # to match and the model got a generic guess instead of its own
            # broken line. Measured: a model dropped `self` from a method
            # signature, was told the likely cause was nested quotes in an
            # f-string, and re-sent the same signature four times.
            stderr = result.get("stderr", "")
            lineno = None
            for line in stderr.splitlines():
                line = line.strip()
                if line.startswith('File "'):
                    match = re.search(r"line (\d+)", line)
                    if match:
                        lineno = match.group(1)
                    continue
                if line and any(kind in line for kind in ("SyntaxError", "IndentationError", "TabError")):
                    if lineno and "line " not in line:
                        line = f"{line} (line {lineno})"
                    errors.append(line)
            if not errors and stderr.strip():
                errors.append(stderr.strip().split("\n")[-1])

    elif lang == "javascript":
        errors.extend(_javascript_syntax_errors(code, workspace, filename))

    elif lang == "typescript":
        fpath = _contained_path(workspace, filename or "check.ts")
        fpath.write_text(code)
        errors.extend(_typescript_syntax_errors(fpath, workspace))

    elif lang == "go":
        fpath = _contained_path(workspace, filename or "main.go")
        fpath.write_text(code)
        errors.extend(_go_syntax_errors(fpath, workspace))

    elif lang == "java":
        class_name = _extract_java_classname(code) 
        package = _extract_java_package(code) 

        if package:
            # com.exampe => com/example (the package dir is created by
            # _contained_path, which makes the parent for every check file)
            fpath = _contained_path(workspace, *package.split('.'),
                                    f"{class_name}.java")
        else:
            fpath = _contained_path(workspace, f"{class_name}.java")

        fpath.write_text(code)
        errors.extend(_java_syntax_errors(fpath, workspace))

    elif lang == "kotlin":
        fpath = _contained_path(workspace, filename or "Source.kt")
        fpath.write_text(code)
        errors.extend(_kotlin_syntax_errors(fpath, workspace))

    elif lang == "rust":
        fpath = _contained_path(workspace, filename or "check.rs")
        fpath.write_text(code)
        errors.extend(_rust_syntax_errors(fpath, workspace))

    elif lang in ("c", "cpp"):
        ext = ".c" if lang == "c" else ".cpp"
        fpath = _contained_path(workspace, filename or f"check{ext}")
        fpath.write_text(code)
        errors.extend(_c_or_cpp_syntax_errors(lang, fpath, workspace))

    elif lang == "ruby":
        fpath = _contained_path(workspace, filename or "main.rb")
        fpath.write_text(code)
        errors.extend(_ruby_syntax_errors(fpath, workspace))

    elif lang == "php":
        fpath = _contained_path(workspace, filename or "main.php")
        fpath.write_text(code)
        errors.extend(_php_syntax_errors(fpath, workspace))

    elif lang == "bash":
        fpath = _contained_path(workspace, filename or "check.sh")
        fpath.write_text(code)
        errors.extend(_bash_syntax_errors(fpath, workspace))

    elif lang == "json":
        try:
            json.loads(code)
        except json.JSONDecodeError as e:
            errors.append(str(e))

    elif lang in ("yaml", "yml"):
        errors.extend(_yaml_syntax_errors(code))

    elif lang in ("html", "htm"):
        errors.extend(_html_syntax_errors(code, filename))

    elif lang == "xml":
        errors.extend(_xml_syntax_errors(code))

    else:
        errors.append(f"syntax verification is unavailable for language: {lang}")

    return errors


# ---------------------------------------------------------------------------
# Language handlers
# ---------------------------------------------------------------------------

def _run_cmd(cmd: List[str], timeout: int, cwd: Path = None, env: dict = None,
             stdin: Optional[str] = None, cancelled=None) -> Dict:
    """Run one untrusted command through the one resource owner.

    Every limit -- time, memory, process count, output bytes -- is installed
    before the command starts and applies to everything it spawns, however it
    spawns it. The answer carries HOW the command ended, because a command
    stopped at a ceiling exits non-zero exactly like a failing test and nothing
    downstream may confuse the two.
    """
    result = run_bounded(cmd, EXEC_CONTRACT.for_request(timeout),
                         cwd=cwd, env=env, stdin=stdin, cancelled=cancelled)
    return {
        "success": result.success,
        "stdout": result.stdout[-4000:],
        "stderr": result.stderr[-2000:],
        "returncode": result.returncode,
        "timed_out": result.outcome == OUTCOME_TIMED_OUT,
        "outcome": result.outcome,
        "stopped": stopped_words(result.outcome, EXEC_CONTRACT.for_request(timeout)),
        "peak_memory_bytes": result.peak_memory_bytes,
        "peak_processes": result.peak_processes,
        "survivors": result.survivors,
        "limits_enforced": SUPERVISION_AVAILABLE,
        "resource_limits": _resource_limits_label(),
    }


def _classify_error(stderr: str) -> Optional[str]:
    """Extract error type from stderr."""
    patterns = [
        (r"SyntaxError", "SyntaxError"),
        (r"NameError", "NameError"),
        (r"TypeError", "TypeError"),
        (r"ValueError", "ValueError"),
        (r"ImportError|ModuleNotFoundError", "ImportError"),
        (r"IndexError", "IndexError"),
        (r"KeyError", "KeyError"),
        (r"AttributeError", "AttributeError"),
        (r"ZeroDivisionError", "ZeroDivisionError"),
        (r"FileNotFoundError", "FileNotFoundError"),
        (r"ReferenceError", "ReferenceError"),
        (r"error\[E\d+\]", "CompileError"),
        (r"error:", "CompileError"),
        (r"undefined reference", "LinkError"),
        (r"cannot find", "NotFoundError"),
        (r"timed out", "Timeout"),
    ]
    for pattern, error_type in patterns:
        if re.search(pattern, stderr):
            return error_type
    return "RuntimeError" if stderr.strip() else None


# --- Python ---

def execute_python(code, test_code, workspace, timeout, requirements, stdin=None, **_):
    start = time.time()
    main_file = workspace / "solution.py"
    main_file.write_text(code)

    # Syntax check
    try:
        compile(code, "solution.py", "exec")
    except SyntaxError as e:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", stderr=f"Line {e.lineno}: {e.msg}",
            error_type="SyntaxError", error_message=f"Line {e.lineno}: {e.msg}",
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Install requirements
    if requirements:
        r = _run_cmd(["pip", "install", "--target", str(workspace), "--quiet"] + requirements, timeout)
        if not r["success"]:
            return ExecuteResponse(
                success=False, compile_success=True,
                tests_run=0, tests_passed=0,
                stdout="", **why_it_failed(r, "DependencyError", _classify_error),
                execution_time_ms=int((time.time() - start) * 1000),
            )

    # Lint
    lint_score = None
    try:
        # Through the owner: pylint parses model-authored code, which makes
        # this an untrusted execution however fixed its argv looks.
        lr = _run_cmd(
            ["python", "-m", "pylint", "--score=y", "--exit-zero", str(main_file)],
            timeout=15)
        m = re.search(r"rated at ([\d.]+)/10", lr["stdout"])
        if m:
            lint_score = float(m.group(1))
    except Exception:  # noqa: S110
        # best-effort: swallow on failure (caller continues)
        pass

    # Run
    if test_code:
        (workspace / "test_solution.py").write_text(test_code)
        r = _run_cmd(["python", "-m", "pytest", "-v", "--tb=short", str(workspace)], timeout, cwd=workspace,
                     stdin=stdin)
        passed = int(m.group(1)) if (m := re.search(r"(\d+) passed", r["stdout"])) else 0
        failed = int(m.group(1)) if (m := re.search(r"(\d+) failed", r["stdout"])) else 0
        total = passed + failed or 1
    else:
        # cwd=workspace, like the pytest branch above. Without it the process
        # ran from the image WORKDIR, which is read-only in a `read_only:
        # true` container: a candidate told to read `input.txt` could not see
        # the file this same request had just staged for it, and any relative
        # write raised `[Errno 30] Read-only file system` before the candidate
        # did anything. The workspace is the only directory this request may
        # write to, and it is removed when the request is answered.
        r = _run_cmd(
            ["python", "-c", f"import sys; sys.path.insert(0,'{workspace}'); import solution"],
            timeout, cwd=workspace, stdin=stdin
        )
        passed = 1 if r["success"] else 0
        total = 1

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=total, tests_passed=passed,
        lint_score=lint_score,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- JavaScript ---

def execute_javascript(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "solution.js"
    main_file.write_text(code)

    # Syntax check via node --check
    r = _run_cmd(["node", "--check", str(main_file)], 10)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "SyntaxError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd(["node", str(main_file)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- TypeScript ---

def execute_typescript(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "solution.ts"
    main_file.write_text(code)

    # Type check via tsc --noEmit
    r = _run_cmd(["tsc", "--noEmit", "--strict", "--esModuleInterop", str(main_file)], 15)
    compile_success = r["success"]
    if not compile_success:
        # Still try to run — TS errors are often non-fatal for execution
        logger.info(f"TypeScript type errors: {r['stderr'][:200]}")

    # Run via tsx (faster than ts-node, handles ESM)
    r = _run_cmd(["tsx", str(main_file)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=compile_success,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- Go ---

def execute_go(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "main.go"
    main_file.write_text(code)

    # Init module
    _run_cmd(["go", "mod", "init", "sandbox"], 5, cwd=workspace)

    # Build (compile check)
    r = _run_cmd(["go", "build", "-o", str(workspace / "program"), str(main_file)], 30, cwd=workspace)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd([str(workspace / "program")], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- Java ---
def execute_java(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    class_name = _extract_java_classname(code) 
    package = _extract_java_package(code) 

    if package:
        # com.exampe => com/example
        fpath = _contained_path(workspace, *package.split('.'),
                                f"{class_name}.java")
        fpath.parent.mkdir(parents=True, exist_ok=True)
        fully_qualified_name = f"{package}.{class_name}"
    else:
        fpath = _contained_path(workspace, f"{class_name}.java")
        fully_qualified_name = class_name

    fpath.write_text(code)

    # Compile with -d workspace so .class appears in matching package dir
    r = _run_cmd(["javac", "-d", str(workspace), str(fpath)], 30, cwd=workspace)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run 
    r = _run_cmd(["java", "-cp", str(workspace), fully_qualified_name], timeout, cwd=workspace, stdin=stdin)
    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )

# --- Kotlin --- 

def execute_kotlin(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    fpath = workspace / "Source.kt"
    fpath.write_text(code)
    jar_path = workspace / "output.jar"
    
    # Compile
    r = _run_cmd(
        ["kotlinc", "-include-runtime", "-d", str(jar_path), str(fpath)],
        60, cwd=workspace
    )

    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0, 
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd(["java", "-jar", str(jar_path)], timeout, cwd=workspace, stdin=stdin)
    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )

# --- Rust, C and C++: compiled to `program`, then run once ---

def _compile_and_run(code, workspace, source, program, compile_command, compile_timeout, timeout, stdin):
    """Write `code` to `source`, compile it into `program` with
    `compile_command`, and run the program once. A failed compile is a
    CompileError, and the program does not run."""
    start = time.time()
    source.write_text(code)

    r = _run_cmd(compile_command, compile_timeout)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    r = _run_cmd([str(program)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


def execute_rust(code, test_code, workspace, timeout, stdin=None, **_):
    source, program = workspace / "main.rs", workspace / "program"
    return _compile_and_run(code, workspace, source, program,
                            ["rustc", str(source), "-o", str(program)], 30, timeout, stdin)


def execute_c(code, test_code, workspace, timeout, stdin=None, **_):
    source, program = workspace / "solution.c", workspace / "program"
    return _compile_and_run(code, workspace, source, program,
                            ["gcc", "-o", str(program), str(source), "-lm", "-Wall"], 15, timeout, stdin)


def execute_cpp(code, test_code, workspace, timeout, stdin=None, **_):
    source, program = workspace / "solution.cpp", workspace / "program"
    return _compile_and_run(code, workspace, source, program,
                            ["g++", "-o", str(program), str(source), "-std=c++17", "-Wall"], 15, timeout, stdin)


# --- Ruby ---

def execute_ruby(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "main.rb"
    main_file.write_text(code)

    # Syntax check (no compile step for Ruby)
    r = _run_cmd(["ruby", "-c", str(main_file)], 15)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "SyntaxError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd(["ruby", str(main_file)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- PHP ---

def execute_php(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "main.php"
    main_file.write_text(code)

    # Lint check (no compile step for PHP)
    r = _run_cmd(["php", "-l", str(main_file)], 15)
    if not r["success"]:
        # Real parse error lives in stderr; stdout only has generic summary line
        error_output = r["stderr"] or r["stdout"]
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed({**r, "stderr": error_output}, "SyntaxError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd(["php", str(main_file)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- Bash ---

def execute_bash(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    script = workspace / "solution.sh"
    script.write_text(code)
    script.chmod(0o755)

    # Syntax check
    r = _run_cmd(["bash", "-n", str(script)], 5)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "SyntaxError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd(["bash", str(script)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# Handler dispatch
LANGUAGE_HANDLERS = {
    "python": execute_python,
    "javascript": execute_javascript,
    "typescript": execute_typescript,
    "go": execute_go,
    "rust": execute_rust,
    "c": execute_c,
    "cpp": execute_cpp,
    "bash": execute_bash,
    "java": execute_java,
    "kotlin": execute_kotlin,
    "ruby": execute_ruby,
    "php": execute_php,
}
