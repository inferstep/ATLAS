"""A syntax error has to say WHERE it is.

py_compile reports the location and the error on separate lines:

    File "/w/check.py", line 4
      def __init__(,filename="todos.json"):
                   ^
    SyntaxError: invalid syntax

The sandbox kept only the last of those. The proxy quotes the offending
source line back to the model, but it finds that line by matching "line N"
in the error string, so discarding the File frame left it nothing to match
and the rejection fell through to a generic guess.

Measured on a benchmark run: a model dropped `self` from a method signature,
producing `def __init__(,filename=...)`. It was told the likely cause was
nested double-quotes inside an f-string. It re-sent the same signature four
times, changing only whitespace, then re-sent it byte-identically three more
times until the repetition breaker ended the session. Both repetitions of
that task failed the same way, so the task went 2/2 to 0/2.
"""

import re
import subprocess
import sys
from pathlib import Path

import pytest

REPO = Path(__file__).resolve().parents[2]

# The measured content, reduced to the signature that broke.
BROKEN = (
    'import json\n'
    '\n'
    'class TodoStore:\n'
    '    def __init__(,filename="todos.json"):\n'
    '        self.filename = filename\n'
)


def _extract(stderr: str) -> list:
    """The sandbox's extraction, mirrored.

    Kept in step with _syntax_check_impl in sandbox/executor_server.py; the
    contract under test is that the returned string carries a line number.
    """
    errors, lineno = [], None
    for line in stderr.splitlines():
        line = line.strip()
        if line.startswith('File "'):
            match = re.search(r"line (\d+)", line)
            if match:
                lineno = match.group(1)
            continue
        if line and any(k in line for k in ("SyntaxError", "IndentationError", "TabError")):
            if lineno and "line " not in line:
                line = f"{line} (line {lineno})"
            errors.append(line)
    return errors


def _py_compile_stderr(tmp_path: Path, source: str) -> str:
    target = tmp_path / "check.py"
    target.write_text(source)
    result = subprocess.run(
        [sys.executable, "-m", "py_compile", str(target)],
        capture_output=True, text=True,
    )
    assert result.returncode != 0, "this source is supposed to be invalid"
    return result.stderr


def test_the_error_carries_the_line_number(tmp_path):
    errors = _extract(_py_compile_stderr(tmp_path, BROKEN))
    assert errors, "a failing compile must produce an error string"
    assert re.search(r"line (\d+)", errors[0]), (
        f"without a line number the proxy cannot quote the source: {errors[0]!r}"
    )


def test_the_line_number_points_at_the_broken_signature(tmp_path):
    """Off-by-one here would quote the wrong line, which is worse than none."""
    errors = _extract(_py_compile_stderr(tmp_path, BROKEN))
    lineno = int(re.search(r"line (\d+)", errors[0]).group(1))
    quoted = BROKEN.split("\n")[lineno - 1]
    assert "def __init__(" in quoted, f"quoted the wrong line: {quoted!r}"


def test_an_indentation_error_is_located_too(tmp_path):
    source = "def f():\n    return 1\n  return 2\n"
    errors = _extract(_py_compile_stderr(tmp_path, source))
    assert errors and re.search(r"line (\d+)", errors[0]), errors


def test_a_line_number_already_present_is_not_duplicated(tmp_path):
    """Some messages carry their own location; appending a second is noise."""
    stderr = 'File "/w/check.py", line 9\nSyntaxError: bad thing on line 9\n'
    assert _extract(stderr) == ["SyntaxError: bad thing on line 9"]


def test_the_sandbox_still_preserves_the_line_number():
    """Guards the extraction in the service itself, not just this mirror."""
    src = (REPO / "sandbox" / "executor_server.py").read_text()
    impl = src.split("def _syntax_check_impl", 1)[1].split("\n    elif lang ==", 1)[0]
    assert 'line.startswith(\'File "\')' in impl, (
        "the File frame carries the line number; dropping it again breaks "
        "the proxy's ability to quote the offending source"
    )
    assert 'f"{line} (line {lineno})"' in impl
