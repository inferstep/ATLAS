"""Where the reliability runner (scripts/eval/e2e-reliability.py) judges the agent's code.

The agent writes and runs its code in the run's sandbox, whose Python is not
necessarily the runner's. Once a run names its sandbox (use_sandbox), the
checks parse and run the agent's code there; without one, on this host. Also
the parse helpers that the task checks share with the corruption detector, and
the identity of the evaluator that each result records.
"""
from __future__ import annotations

import ast
import json
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path
from typing import Optional

# --------------------------------------------------------------------------
# Helpers shared by the checks and the corruption detector
# --------------------------------------------------------------------------

def _extract_script(src: str) -> str | None:
    # `<script>` bare is the minority spelling. A model writing
    # `<script type="text/javascript">` or `<SCRIPT>` used to fall straight
    # through here, and the JS half of the quality score silently scored
    # nothing — the file read as "no JS" rather than "JS not analysed".
    m = re.search(r"<script\b[^>]*>(.*?)</script\b[^>]*>", src, re.S | re.I)
    return m.group(1) if m else None


def _js_parses(js: str) -> tuple[bool, str]:
    node = shutil.which("node")
    if not node:
        return True, "node unavailable — skipped"
    with tempfile.TemporaryDirectory() as folder:
        script = Path(folder) / "check.js"
        script.write_text(js)
        p = subprocess.run([node, "--check", str(script)],
                           capture_output=True, text=True, timeout=30)
        return p.returncode == 0, p.stderr.strip().splitlines()[0] if p.stderr else ""


# Set once from --sandbox-container. The sandbox's interpreter is the one that
# will run the agent's code, and it is not necessarily this script's.
_SANDBOX_CONTAINER = ""
# Container-side path of the run workspace, set from --subdir. Empty when no
# sandbox was configured, which is the only case that falls back to the host.
_SANDBOX_WORKDIR = ""


def use_sandbox(args) -> None:
    """Judge the agent's code in the run's sandbox: the container that
    --sandbox-container names, and the run workspace in it that --subdir names."""
    global _SANDBOX_CONTAINER, _SANDBOX_WORKDIR
    _SANDBOX_CONTAINER = args.sandbox_container or ""
    # Where the run workspace appears inside the sandbox. Both halves are
    # required before any check leaves the host interpreter, so a run without
    # --sandbox-container behaves exactly as it did before.
    _SANDBOX_WORKDIR = f"/workspace/{args.subdir}" if args.subdir else ""


def _runtime_argv(workdir: str = "") -> list[str]:
    """Run the agent's code in the interpreter that actually runs it.

    This script's Python is 3.9; the sandbox the agent writes and verifies its
    code in is 3.13. Judging one with the other is how a working program gets
    scored as broken. Measured on multifile_cli rep2, which wrote

        print(f"{i}: {status} {todo["text"]}")

    -- nested same-type quotes in an f-string, valid since PEP 701 (3.12+).
    The sandbox ran the app correctly (3 tests pass, `add` and `list` both
    work); this script raised SyntaxError and recorded a task failure, while
    ATLAS had truthfully reported deliverables_demonstrated.

    _sandbox_python_parses already established exactly this reasoning for the
    parse check -- "this script's own interpreter is not the one the code runs
    under ... ask the runtime that will execute it". It was never applied to
    the checks that EXECUTE the code, which is where it matters most.
    """
    if _SANDBOX_CONTAINER and _SANDBOX_WORKDIR:
        argv = ["docker", "exec"]
        if workdir:
            argv += ["-w", workdir]
        return argv + [_SANDBOX_CONTAINER, "python3"]
    return [sys.executable]


def _ws_path(ws: Path) -> str:
    """The workspace as the interpreter that will run the code sees it."""
    return _SANDBOX_WORKDIR or str(ws)


# What the sandbox's Python runs on the text it reads from stdin: it prints "OK",
# or "ERR" and the syntax error.
_PARSE_PROGRAM = (
    "import ast,sys\n"
    "try:\n"
    "    ast.parse(sys.stdin.read())\n"
    "    print('OK')\n"
    "except SyntaxError as e:\n"
    "    print('ERR', e)\n"
)


def _sandbox_python_parses(text: str) -> tuple[Optional[bool], str]:
    """Parse `text` with the sandbox's Python, the one that will run it.

    Returns (None, "") when the sandbox cannot be reached, so the caller falls
    back to the local verdict rather than silently passing everything.
    """
    container = _SANDBOX_CONTAINER
    if not container:
        return None, ""
    try:
        proc = subprocess.run(
            ["docker", "exec", "-i", container, "python3", "-c", _PARSE_PROGRAM],
            input=text, capture_output=True, text=True, timeout=20)
    except (OSError, subprocess.SubprocessError):
        return None, ""
    out = (proc.stdout or "").strip()
    if out.startswith("OK"):
        return True, ""
    if out.startswith("ERR"):
        return False, out[4:].strip()
    return None, ""


EVALUATOR_VERSION = "e2e-eval-v2"


def _quality_parser():
    return _target_parse if _SANDBOX_CONTAINER else None


def evaluator_identity() -> dict:
    """Which interpreter judged Python syntax in this run.

    v1 used this script's interpreter for the quality count and three task
    checks; the dev server host is 3.9 and the sandbox runs 3.13, so code
    valid where it runs was scored unparseable. v2 asks the sandbox whenever
    one is configured and names the fallback when it is not.
    """
    target = "unavailable"
    if _SANDBOX_CONTAINER:
        try:
            proc = subprocess.run(["docker", "exec", _SANDBOX_CONTAINER, "python3", "-c",
                                   "import sys; print('%d.%d.%d' % sys.version_info[:3])"],
                                  capture_output=True, text=True, timeout=20)
            image = subprocess.run(["docker", "inspect", "-f", "{{.Image}}", _SANDBOX_CONTAINER],
                                   capture_output=True, text=True, timeout=20)
            if proc.returncode == 0:
                target = (f"sandbox {_SANDBOX_CONTAINER} python {proc.stdout.strip()} "
                          f"image {image.stdout.strip()[:19]}")
        except (OSError, subprocess.SubprocessError):
            # Docker did not answer: the target stays "unavailable", and the
            # record still names the host Python as the fallback.
            pass
    return {"version": EVALUATOR_VERSION, "python_parser": target,
            "fallback": f"host python {sys.version_info.major}.{sys.version_info.minor}"}


def _target_parse(text: str) -> tuple[Optional[bool], str]:
    return _sandbox_python_parses(text)


def _function_names(src: str) -> tuple[set, str]:
    """Top-level-and-nested def names, judged by the interpreter that runs the
    code. Returns (names, "") or (set(), error)."""
    ok, why = _target_parse(src)
    if ok is False:
        return set(), why
    try:
        tree = ast.parse(src)
    except SyntaxError as e:
        if ok is None:
            return set(), f"{e} (host python {sys.version_info.major}.{sys.version_info.minor}; sandbox unreachable)"
        names, err = _sandbox_function_names(src)
        return names, err
    return {n.name for n in ast.walk(tree) if isinstance(n, ast.FunctionDef)}, ""


# What the sandbox's Python runs on the source it reads from stdin: it prints the
# names of the functions the source defines, as a sorted JSON list.
_FUNCTION_NAMES_PROGRAM = (
    "import ast,json,sys\n"
    "t=ast.parse(sys.stdin.read())\n"
    "print(json.dumps(sorted({n.name for n in ast.walk(t) if isinstance(n, ast.FunctionDef)})))\n"
)


def _sandbox_function_names(src: str) -> tuple[set, str]:
    try:
        proc = subprocess.run(
            ["docker", "exec", "-i", _SANDBOX_CONTAINER, "python3", "-c", _FUNCTION_NAMES_PROGRAM],
            input=src, capture_output=True, text=True, timeout=20)
        return set(json.loads(proc.stdout)), ""
    except (OSError, subprocess.SubprocessError, ValueError) as e:
        return set(), f"sandbox could not list functions: {e}"


def _file_parses(path: Path) -> tuple[bool, str]:
    """Whole-file parse plus the embedded-script layer, mirroring the gates."""
    try:
        text = path.read_text()
    except (UnicodeDecodeError, OSError):
        return True, ""  # binary or unreadable: not our concern
    if path.suffix == ".py":
        # The interpreter that runs the code decides (evaluator v2). v1
        # consulted it only after a host 3.9 failure, so a file valid under
        # 3.9 and invalid under 3.13 would have passed.
        ok, why = _target_parse(text)
        if ok is False:
            return False, f"python: {why}"
        if ok is None:
            try:
                ast.parse(text)
            except SyntaxError as e:
                return False, f"python ({sys.version_info.major}.{sys.version_info.minor}): {e}"
    js = _extract_script(text) if path.suffix in (".py", ".html", ".htm") else None
    if js:
        ok, err = _js_parses(js)
        if not ok:
            return False, f"embedded js: {err}"
    return True, ""
