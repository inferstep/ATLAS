"""
Multi-language sandbox execution server.

Supports: Python, JavaScript/TypeScript, Go, Java, Kotlin, Rust, C/C++, Ruby, PHP, Bash/Shell
Provides isolated code execution with resource limits and structured error reporting.

Security / trust model (load-bearing — read before "fixing" CodeQL alerts):
    This service IS the trust boundary. Its entire purpose is to execute
    agent-supplied code and shell commands on behalf of ATLAS. Isolation
    comes from the container, and only from what the container actually
    enforces: read-only rootfs, no-new-privileges, pids_limit (fork-bomb
    stop), tmpfs workspace, /workspace as the only writable host mount,
    and the compose `mem_limit` (written by `atlas init`; unlimited until
    then on a raw `compose up`). Per-call wall-clock is capped in-process
    via MAX_EXECUTION_TIME. Outbound network is NOT restricted — the
    sandbox sits on the regular bridge network so toolchains can fetch
    dependencies; do not describe it as egress-locked. The Python code in
    this file does NOT need to sanitize inputs to subprocess.run, validate
    user-controlled paths inside the workspace, or treat agent-supplied
    code as untrusted — that's the container's job.

    CodeQL flags `py/command-line-injection` here. Those alerts are
    by-design false positives: accepting + executing user-controlled
    commands is the requirement, and the cmd-list form (no shell=True at
    the Python layer) prevents Python-level injection. Don't add input
    validation that would break the sandbox's purpose; dismiss the
    alerts with rationale instead.

    `py/path-injection` is handled differently: every workspace path
    derived from request data goes through _contained_path(), whose
    normpath + prefix check is the sanitizer form CodeQL recognizes.
    Route any new request-derived file write through it and the query
    stays quiet without per-PR dismissals.
"""

# Defers annotation evaluation, so PEP 604 unions (`str | None`) in
# signatures parse on any Python >= 3.7. The container runs 3.13, but
# tests/infrastructure/test_boundary_regression.py imports this module with
# the host interpreter, and the package declares support down to 3.9
# (pyproject.toml requires-python), where evaluating `str | None` at def
# time raises TypeError.
from __future__ import annotations

import contextlib
import hashlib
import json
import os
import shutil
import signal
import tempfile
import asyncio
import subprocess
import logging
import re
import threading
import time
import uuid
from collections import deque
from typing import Dict, Optional, List
from pathlib import Path
from fastapi import FastAPI, HTTPException, Request
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
# Structured logging (JSON when ATLAS_LOG_FORMAT=json) + private-value
# masking + correlation IDs (see structured_log.py — canonical copy).
from structured_log import (install as _install_logging,  # noqa: E402
                            set_request_id as _set_rid)
_install_logging("sandbox")
logger = logging.getLogger(__name__)

app = FastAPI(title="ATLAS Code Execution Sandbox")


def _load_service_token() -> str:
    """Internal-auth token (Authorization: Bearer). Empty = auth
    disabled (pre-token behavior; `atlas doctor` warns). Never logged.
    This is the highest-value enforcement point in the stack — /shell
    and /execute run arbitrary commands against the bind-mounted
    workspace."""
    path = os.getenv("ATLAS_SERVICE_TOKEN_FILE",
                     "/run/atlas-secrets/service-token")
    try:
        with open(path) as fh:
            return fh.read().strip()
    except OSError:
        return ""


SERVICE_TOKEN = _load_service_token()


# Pure ASGI middleware, not BaseHTTPMiddleware, and the reason is cancellation.
#
# `@app.middleware("http")` builds a BaseHTTPMiddleware, which interposes on
# the receive channel: it consumes the ASGI messages itself and hands the
# endpoint a substitute. `http.disconnect` never arrives, so
# `Request.is_disconnected()` inside the handler returns False forever and a
# caller that went away is invisible. Measured directly -- the same probe
# reports "disconnected at 1.0s" without a middleware and "never disconnected"
# with one.
#
# These two do nothing that needs the body, so they operate on the scope and
# wrap `send`, leaving `receive` untouched for whoever is downstream.


class _ServiceTokenMiddleware:
    """Reject unauthenticated calls before they reach a route."""

    _OPEN_PATHS = ("/health", "/languages")

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http" or not SERVICE_TOKEN:
            await self.app(scope, receive, send)
            return
        if scope.get("path") in self._OPEN_PATHS:
            await self.app(scope, receive, send)
            return
        import hmac
        got = ""
        for name, value in scope.get("headers", []):
            if name.lower() == b"authorization":
                got = value.decode("latin-1")
                break
        if not hmac.compare_digest(got, f"Bearer {SERVICE_TOKEN}"):
            body = json.dumps({
                "error": "unauthorized",
                "detail": "internal service auth is enabled; send "
                          "Authorization: Bearer <service-token> "
                          "(secrets/service-token)"}).encode()
            await send({"type": "http.response.start", "status": 401,
                        "headers": [(b"content-type", b"application/json"),
                                    (b"content-length",
                                     str(len(body)).encode())]})
            await send({"type": "http.response.body", "body": body})
            return
        await self.app(scope, receive, send)


class _CorrelationIDMiddleware:
    """Carry the caller's request id into the logs and back out again."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope["type"] != "http":
            await self.app(scope, receive, send)
            return
        rid = ""
        for name, value in scope.get("headers", []):
            if name.lower() == b"x-atlas-request-id":
                rid = value.decode("latin-1")
                break
        _set_rid(rid)

        async def _send(message):
            if rid and message["type"] == "http.response.start":
                message.setdefault("headers", [])
                message["headers"] = list(message["headers"]) + [
                    (b"x-atlas-request-id", rid.encode("latin-1"))]
            await send(message)

        await self.app(scope, receive, _send)


# Added in this order so the correlation id is outermost: it must be set and
# echoed even on a request the token middleware rejects.
app.add_middleware(_ServiceTokenMiddleware)
app.add_middleware(_CorrelationIDMiddleware)


from html.parser import HTMLParser  # noqa: E402

from resource_contract import (  # noqa: E402
    OUTCOME_CANCELLED, OUTCOME_COMPLETED, OUTCOME_MEMORY_EXHAUSTED,
    OUTCOME_OUTPUT_LIMIT, OUTCOME_PROCESS_LIMIT, OUTCOME_SPAWN_FAILED,
    OUTCOME_TIMED_OUT, OUTCOME_UNCLASSIFIED, ResourceContract,
    contract_from_env, outcome_is_complete, run_bounded, stopped_words, why_it_failed,
    EXEC_TOKEN_VAR, _apply_child_limits, _kill_token)

# THE resource contract for every untrusted command, validated here at import.
# A malformed operator budget stops the executor from starting rather than
# being clamped into something plausible at the moment a command runs.
EXEC_CONTRACT = contract_from_env()
MAX_EXECUTION_TIME = EXEC_CONTRACT.wall_seconds
WORKSPACE_BASE = Path(os.getenv("WORKSPACE_BASE", "/tmp/sandbox"))

SUPPORTED_LANGUAGES = {
    "python", "py", "python3",
    "javascript", "js", "node",
    "typescript", "ts",
    "go", "golang",
    "rust", "rs",
    "c", "cpp", "c++",
    "bash", "sh", "shell",
    "html", "htm",
    "xml",
    "json",
    "yaml", "yml",
    "java",
    "kotlin", "kt", "kts",
    "ruby", "rb",
    "php",
}

def normalize_language(lang: str) -> str:
    lang = lang.lower().strip()
    if lang in ("python", "py", "python3"):
        return "python"
    if lang in ("javascript", "js", "node"):
        return "javascript"
    if lang in ("typescript", "ts"):
        return "typescript"
    if lang in ("go", "golang"):
        return "go"
    if lang in ("rust", "rs"):
        return "rust"
    if lang in ("c",):
        return "c"
    if lang in ("cpp", "c++"):
        return "cpp"
    if lang in ("bash", "sh", "shell"):
        return "bash"
    if lang in ("html", "htm"):
        return "html"
    if lang in ("xml",):
        return "xml"
    if lang in ("json",):
        return "json"
    if lang in ("yaml", "yml"):
        return "yaml"
    if lang in ("java",):
        return "java"
    if lang in ("kotlin", "kt", "kts",):
        return "kotlin"
    if lang in ("ruby", "rb"):
        return "ruby"
    if lang in ("php",):
        return "php"
    return lang


class ExecuteRequest(BaseModel):
    code: str
    language: str = "python"
    test_code: Optional[str] = None
    requirements: Optional[List[str]] = None
    timeout: int = 30
    # PC-046: Project-context files dropped into the workspace alongside
    # `solution.py` (or the language equivalent) so multi-file imports
    # resolve. Filename keys are relative to the workspace root; each is
    # validated to reject path traversal (`..`) and absolute paths
    # before being written. Used by V3's verified_sandbox and
    # smoke_compile_check to ship the rest of the project so e.g.
    # `import game_logic` resolves to the user's actual game_logic.py.
    files: Optional[Dict[str, str]] = None
    # Optional standard input piped to the run step. None (the default)
    # keeps existing behavior — the process inherits the server's stdin.
    stdin: Optional[str] = None


class ExecuteResponse(BaseModel):
    success: bool
    compile_success: bool
    tests_run: int
    tests_passed: int
    lint_score: Optional[float] = None
    stdout: str
    stderr: str
    error_type: Optional[str] = None
    error_message: Optional[str] = None
    execution_time_ms: int
    # HOW the run ended, as /shell says it: a caller need not read stderr for a phrase.
    timed_out: bool = False
    outcome: str = OUTCOME_COMPLETED


@app.get("/health")
def health_check():
    return {"status": "healthy"}


@app.get("/languages")
def list_languages():
    """List supported languages and their runtime versions."""
    versions = {}
    checks = {
        "python": ["python3", "--version"],
        "javascript": ["node", "--version"],
        "typescript": ["tsc", "--version"],
        "go": ["go", "version"],
        "java": ["javac", "--version"],
        "kotlin": ["kotlinc", "-version"],
        "ruby": ["ruby", "--version"],
        "php": ["php", "--version"],
        "rust": ["rustc", "--version"],
        "c": ["gcc", "--version"],
        "cpp": ["g++", "--version"],
        "bash": ["bash", "--version"],
    }
    for lang, cmd in checks.items():
        # Through the same owner as everything else. The argv here is fixed and
        # not workspace-controlled, so this is not the case the ceilings exist
        # for -- but a second spawning path is a second place a future edit can
        # put an untrusted argument, and one owner is the point.
        try:
            result = _run_cmd(cmd, timeout=5)
            versions[lang] = result["stdout"].strip().split("\n")[0]
        except Exception:
            versions[lang] = "not installed"
    return {"languages": versions}


# ---------------------------------------------------------------------------
# /shell — arbitrary command execution against the bind-mounted workspace.
#
# The agent loop's run_command tool used to fork bash inside the proxy
# container, but the proxy is a slim Go binary with no python/pip/node/etc
# — every "verify your fix" call hit "command not found". We now route
# shell commands through the sandbox, which has the full language matrix
# pre-installed AND has /workspace bind-mounted (rw) at the same path the
# proxy sees, so paths the agent learned from read_file / list_directory
# carry over verbatim.
#
# Safety: the proxy's validateShellCommand blocks only CATASTROPHIC commands
# (whole-project wipe like `rm -rf /`, fork bombs, device destruction) BEFORE
# the call reaches us — ordinary file ops (mv/cp/rm of a file/mkdir) are
# allowed. This endpoint is the executor, not the gate. The real boundary is
# the container: no-new-privileges, read-only rootfs, and /workspace as the
# ONLY writable host mount, so the blast radius is the project folder.
# ---------------------------------------------------------------------------


class ShellRequest(BaseModel):
    command: str
    cwd: Optional[str] = None  # absolute path inside container, defaults to /workspace
    timeout: int = 30          # seconds; capped at MAX_EXECUTION_TIME
    env: Optional[Dict[str, str]] = None
    # Optional ephemeral overlay used by V3 build verification. When
    # present, /shell copies a bounded workspace snapshot into /tmp,
    # overlays these relative file paths, runs the command there, then
    # deletes the snapshot. This lets V3 test a candidate without
    # writing it into the real bind-mounted project.
    files: Optional[Dict[str, str]] = None
    # Optional bounded observation, used by candidate staging. When present,
    # /shell hashes these relative paths and the snapshot as a whole BEFORE
    # and AFTER the command, and returns both. It reports facts and draws no
    # conclusion: whether a change is permitted is the caller's judgement, and
    # this endpoint has no way to know what the caller declared.
    observe_paths: Optional[List[str]] = None


class ShellObservation(BaseModel):
    """What the workspace looked like either side of one command.

    Hashes and counts only. No path contents, no command text, no output --
    a staging observation that carried any of those would put candidate bytes
    into the caller's telemetry, which is the one thing staging exists to
    avoid.
    """
    target_before: Dict[str, str] = {}
    target_after: Dict[str, str] = {}
    workspace_before: str = ""
    workspace_after: str = ""
    workspace_files: int = 0
    # True when the digest hit its own cap and therefore describes only part
    # of the workspace. A caller that needs an exact answer must treat this as
    # unobservable rather than as "unchanged".
    digest_truncated: bool = False


class ShellResponse(BaseModel):
    success: bool
    stdout: str
    stderr: str
    exit_code: int
    elapsed_ms: int
    # Present only when the request asked for it.
    timed_out: bool = False
    # How the command ended, from the closed vocabulary in resource_contract.
    # `completed` is the only member that means the command reached its own
    # end; every other one means it was stopped, and a stopped command
    # demonstrates nothing about the code it was pointed at. Defaulted so an
    # older executor answering a newer proxy is read as unclassified rather
    # than as success.
    outcome: str = "internal_unclassified"
    peak_memory_bytes: int = 0
    observation: Optional[ShellObservation] = None


# The bind-mounted project root. /workspace in every container
# deployment (compose/K3s mount it there); overridable so the executor
# can run directly on a host — e.g. the E2E acceptance test boots it
# under a pytest tmp dir.
WORKSPACE_ROOT = Path(os.getenv("ATLAS_SANDBOX_WORKSPACE_ROOT", "/workspace"))
SHELL_SNAPSHOT_IGNORE = {
    ".git",
    ".hg",
    ".svn",
    ".cache",
    ".venv",
    "venv",
    "__pycache__",
    ".pytest_cache",
    ".mypy_cache",
    ".ruff_cache",
    ".tox",
    "build",
    "coverage",
    ".coverage",
    "htmlcov",
    ".next",
    "dist",
    "target",
    "secrets",
}
SHELL_SNAPSHOT_IGNORE_SUFFIXES = (
    ".arrow",
    ".bin",
    ".db",
    ".gguf",
    ".gz",
    ".onnx",
    ".parquet",
    ".pt",
    ".safetensors",
    ".sqlite",
    ".tar",
    ".zip",
)
SHELL_SNAPSHOT_MAX_FILES = int(os.getenv("ATLAS_SHELL_SNAPSHOT_MAX_FILES", "20000"))
SHELL_SNAPSHOT_MAX_BYTES = int(os.getenv("ATLAS_SHELL_SNAPSHOT_MAX_BYTES", str(256 * 1024 * 1024)))
SHELL_SNAPSHOT_MAX_FILE_BYTES = int(os.getenv("ATLAS_SHELL_SNAPSHOT_MAX_FILE_BYTES", str(16 * 1024 * 1024)))


def _skip_shell_snapshot_path(path: Path) -> bool:
    name = path.name
    if name in SHELL_SNAPSHOT_IGNORE or name.startswith(".env"):
        return True
    return name.endswith(SHELL_SNAPSHOT_IGNORE_SUFFIXES)


def _copy_symlink_if_safe(src: Path, dest: Path, source_root: Path, snapshot: Path) -> bool:
    try:
        target = src.resolve(strict=True)
        rel_target = target.relative_to(source_root)
        link_target = os.readlink(src)
    except (OSError, ValueError):
        return False

    if os.path.isabs(link_target):
        snapshot_target = snapshot / rel_target
        link_target = os.path.relpath(snapshot_target, dest.parent)
    dest.parent.mkdir(parents=True, exist_ok=True)
    os.symlink(link_target, dest)
    return True


def _copy_workspace_snapshot(source: Path, snapshot: Path):
    files_copied = 0
    bytes_copied = 0
    source = source.resolve()

    for root, dirs, filenames in os.walk(source, topdown=True, followlinks=False):
        root_path = Path(root)
        try:
            rel_root = root_path.relative_to(source)
        except ValueError:
            continue
        target_dir = snapshot / rel_root
        target_dir.mkdir(parents=True, exist_ok=True)

        kept_dirs = []
        for name in dirs:
            src_dir = root_path / name
            if _skip_shell_snapshot_path(src_dir):
                continue
            if src_dir.is_symlink():
                _copy_symlink_if_safe(src_dir, target_dir / name, source, snapshot)
                continue
            kept_dirs.append(name)
        dirs[:] = kept_dirs

        for name in filenames:
            src = root_path / name
            if _skip_shell_snapshot_path(src):
                continue
            if src.is_symlink():
                _copy_symlink_if_safe(src, target_dir / name, source, snapshot)
                continue
            try:
                stat = src.stat()
            except OSError:
                continue
            if not src.is_file():
                continue
            if stat.st_size > SHELL_SNAPSHOT_MAX_FILE_BYTES:
                logger.info(
                    "shell overlay snapshot skipped large file: %s (%d bytes)",
                    src, stat.st_size,
                )
                continue

            files_copied += 1
            bytes_copied += stat.st_size
            if files_copied > SHELL_SNAPSHOT_MAX_FILES:
                raise HTTPException(
                    status_code=413,
                    detail=f"workspace snapshot file limit exceeded ({SHELL_SNAPSHOT_MAX_FILES})",
                )
            if bytes_copied > SHELL_SNAPSHOT_MAX_BYTES:
                raise HTTPException(
                    status_code=413,
                    detail=f"workspace snapshot byte limit exceeded ({SHELL_SNAPSHOT_MAX_BYTES})",
                )
            shutil.copy2(src, target_dir / name)


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


def _write_overlay_files(root: Path, files: Dict[str, str]):
    for name, content in files.items():
        rel = _safe_overlay_path(name)
        content = content if isinstance(content, str) else ""
        if len(content.encode("utf-8", "ignore")) > SHELL_SNAPSHOT_MAX_FILE_BYTES:
            raise HTTPException(status_code=413, detail=f"overlay file is too large: {name!r}")

        # Walk directories relative to an already-open snapshot root. O_NOFOLLOW
        # rejects symlink components and closes the resolve-then-write race that
        # a plain Path.write_text() would leave between containment checking and
        # the actual open.
        root_fd = os.open(root, os.O_RDONLY | os.O_DIRECTORY)
        parent_fd = os.dup(root_fd)
        try:
            for component in rel.parts[:-1]:
                with contextlib.suppress(FileExistsError):
                    os.mkdir(component, mode=0o700, dir_fd=parent_fd)
                next_fd = os.open(
                    component,
                    os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW,
                    dir_fd=parent_fd,
                )
                os.close(parent_fd)
                parent_fd = next_fd

            flags = os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW
            file_fd = os.open(rel.name, flags, 0o600, dir_fd=parent_fd)
            with os.fdopen(file_fd, "w", encoding="utf-8") as overlay_file:
                overlay_file.write(content)
        except OSError as e:
            raise HTTPException(
                status_code=400,
                detail=f"unsafe overlay file path: {name!r}",
            ) from e
        finally:
            os.close(parent_fd)
            os.close(root_fd)


# The observation digest has its own cap, separate from the snapshot's. A
# workspace big enough to blow it is one this endpoint cannot describe
# exactly, and saying so is the only honest answer.
SHELL_OBSERVE_MAX_FILES = int(os.getenv("ATLAS_SHELL_OBSERVE_MAX_FILES", "5000"))


def _hash_file(path: Path) -> str:
    h = hashlib.sha256()
    try:
        with open(path, "rb") as fh:
            for chunk in iter(lambda: fh.read(65536), b""):
                h.update(chunk)
    except OSError:
        return ""
    return h.hexdigest()


def _observe_paths(root: Path, names: List[str]) -> Dict[str, str]:
    """sha256 of each named relative path, or "" where it is absent, so a
    caller can tell "the file is not there" from "we did not look"."""
    out: Dict[str, str] = {}
    for name in names or []:
        try:
            rel = _safe_overlay_path(name)
        except HTTPException:
            out[name] = ""
            continue
        # The normpath + prefix form CodeQL follows; _contained_path would mkdir.
        path = os.path.normpath(os.path.join(str(root), rel))
        out[name] = _hash_file(Path(path)) if path.startswith(str(root) + os.sep) else ""
    return out


def _workspace_digest(root: Path) -> Dict[str, object]:
    """One digest over every file in the snapshot, plus the count.

    Names and hashes, never contents. Sorted so the same tree always digests
    the same way, and capped so an unbounded workspace reports truncation
    instead of a number that describes some of it.
    """
    entries: List[str] = []
    truncated = False
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if not _skip_shell_snapshot_path(Path(d))]
        for name in sorted(filenames):
            src = Path(dirpath) / name
            if _skip_shell_snapshot_path(src):
                continue
            if len(entries) >= SHELL_OBSERVE_MAX_FILES:
                truncated = True
                break
            try:
                rel = src.relative_to(root).as_posix()
            except ValueError:
                continue
            entries.append(rel + "\x00" + _hash_file(src))
        if truncated:
            break
    entries.sort()
    digest = hashlib.sha256("\n".join(entries).encode("utf-8")).hexdigest()
    return {"digest": digest, "files": len(entries), "truncated": truncated}


def _snapshot_workspace_with_overlay(files: Dict[str, str]) -> Path:
    snapshot = Path(tempfile.mkdtemp(prefix="shell-", dir=WORKSPACE_BASE))
    try:
        if WORKSPACE_ROOT.exists():
            _copy_workspace_snapshot(WORKSPACE_ROOT, snapshot)
        _write_overlay_files(snapshot, files)
        return snapshot
    except Exception:
        shutil.rmtree(snapshot, ignore_errors=True)
        raise


def _resolve_shell_cwd(raw_cwd: Optional[str], root: Path) -> Path:
    if not raw_cwd:
        return root
    try:
        requested = Path(raw_cwd)
        if requested.is_absolute():
            rel = requested.resolve(strict=False).relative_to(WORKSPACE_ROOT.resolve(strict=False))
        else:
            rel = requested
        if rel.is_absolute() or ".." in rel.parts:
            raise ValueError(f"cwd must be under {WORKSPACE_ROOT}")
        cwd = (root / rel).resolve(strict=False)
        cwd.relative_to(root.resolve(strict=False))
    except (OSError, ValueError) as e:
        raise HTTPException(status_code=400, detail=f"invalid cwd: {e}")
    if not cwd.exists():
        raise HTTPException(status_code=400, detail=f"cwd does not exist: {cwd}")
    return cwd


def _translate_workspace_command(command: str, root: Path) -> str:
    """Point absolute workspace paths at the ephemeral overlay root.

    V3 build commands often come from project detection and may include
    absolute `/workspace/...` paths. When /shell is running with an overlay
    snapshot, those paths must resolve inside the snapshot, not the real
    bind-mounted checkout.
    """
    workspace = str(WORKSPACE_ROOT.resolve(strict=False))
    replacement = str(root.resolve(strict=False))
    if workspace == replacement:
        return command
    pattern = rf"(?<![\w./-]){re.escape(workspace)}(?=$|/|[^\w./-])"
    return re.sub(pattern, replacement, command)


# ---------------------------------------------------------------------------
# Background jobs (PC-196)
# ---------------------------------------------------------------------------
#
# The agent's verify reflex is "run python app.py / npm start / cargo run
# and curl the result." Foreground /shell can't do that — the server
# blocks until killed. Models work around it with `timeout 5 ... || true`
# hacks that capture the startup banner but tear the server down before
# anything can curl it.
#
# Background jobs solve this cleanly: start_background spawns the
# command and returns a job_id immediately, tail_background lets the
# model peek at stdout/stderr, stop_background kills it. The model can
# now run a server, hit it from another command, then clean up.
#
# Process-global registry. Keyed by job_id (uuid4). Each entry holds:
#   proc:    subprocess.Popen
#   stdout:  deque of recent lines (bounded — long-running servers
#            otherwise eat unbounded memory)
#   stderr:  deque of recent lines
#   command: original command string for diagnostics
#   started: time.time() of spawn
#
# Cleanup: a janitor thread sweeps finished jobs every 30s, dropping
# entries older than BG_RETENTION_SEC. Models can still query a job
# right after it exits to read final output.

BG_MAX_LINES = 500          # ring buffer per stream
BG_MAX_JOBS = 32            # hard cap so a misbehaving model can't OOM us
BG_RETENTION_SEC = 600      # keep finished jobs around for 10 min
BG_MAX_AGE_SEC = 7200       # kill still-running jobs abandoned this long

_bg_jobs: Dict[str, dict] = {}
_bg_lock = threading.Lock()


def _bg_drain_stream(job_id: str, stream_name: str, fh):
    """Tail a Popen pipe in a background thread, append each line to
    the job's deque. Runs until the pipe closes (process exit)."""
    try:
        for raw in iter(fh.readline, ""):
            if raw == "":
                break
            with _bg_lock:
                job = _bg_jobs.get(job_id)
                if job is None:
                    return
                job[stream_name].append(raw.rstrip("\n"))
    except (OSError, ValueError):
        # pipe closed / process gone — normal end of life
        return


def _bg_janitor():
    """Sweep the job table every 30s. Daemon thread. Two responsibilities:

    * Finished jobs: stamp ended_at when the janitor first observes the
      process done (a job the caller never polls would otherwise carry no
      ended_at and be reaped on the first sweep, losing its output before
      the retention window), then drop the entry BG_RETENTION_SEC later.
    * Abandoned running jobs: kill the process group once a job passes
      BG_MAX_AGE_SEC so leaked servers can't exhaust pids_limit; the entry
      is then reaped through the normal finished-job path.
    """
    while True:
        time.sleep(30)
        now = time.time()
        with _bg_lock:
            for jid in list(_bg_jobs.keys()):
                job = _bg_jobs[jid]
                proc = job["proc"]
                if proc.poll() is not None:
                    ended = job.get("ended_at")
                    if not ended:
                        job["ended_at"] = now
                    elif ended < now - BG_RETENTION_SEC:
                        del _bg_jobs[jid]
                elif job["started_at"] < now - BG_MAX_AGE_SEC:
                    try:
                        os.killpg(proc.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        # best-effort: swallow on failure (caller continues)
                        pass


threading.Thread(target=_bg_janitor, daemon=True).start()


class BackgroundStartRequest(BaseModel):
    command: str
    cwd: Optional[str] = None
    env: Optional[Dict[str, str]] = None


class BackgroundStartResponse(BaseModel):
    job_id: str
    pid: int
    started_at: float


class BackgroundOutputResponse(BaseModel):
    job_id: str
    running: bool
    exit_code: Optional[int]
    stdout: List[str]
    stderr: List[str]
    elapsed_sec: float
    command: str


class BackgroundJobSummary(BaseModel):
    job_id: str
    command: str
    started_at: float
    running: bool


class BackgroundJobListResponse(BaseModel):
    jobs: List[BackgroundJobSummary]


class BackgroundStopResponse(BaseModel):
    job_id: str
    killed: bool
    exit_code: Optional[int]
    stdout: List[str]
    stderr: List[str]


def _resolve_bg_cwd(raw_cwd: Optional[str]) -> Path:
    """Same workspace-boundary check as run_shell."""
    if raw_cwd:
        try:
            cwd = Path(raw_cwd).resolve()
        except (OSError, ValueError) as e:
            raise HTTPException(status_code=400, detail=f"invalid cwd: {e}")
        if not (cwd == WORKSPACE_ROOT or WORKSPACE_ROOT in cwd.parents):
            raise HTTPException(
                status_code=400,
                detail=f"cwd must be under {WORKSPACE_ROOT}, got {cwd}",
            )
        if not cwd.exists():
            raise HTTPException(status_code=400, detail=f"cwd does not exist: {cwd}")
        return cwd
    return WORKSPACE_ROOT


@app.post("/jobs/start", response_model=BackgroundStartResponse)
def background_start(request: BackgroundStartRequest):
    """Spawn a background process and return a job_id.
    Returns immediately — does NOT wait for the process to print
    anything. Caller polls /jobs/{id}/output for stdout/stderr."""
    if not request.command or not request.command.strip():
        raise HTTPException(status_code=400, detail="command is required")
    cwd = _resolve_bg_cwd(request.cwd)
    with _bg_lock:
        if len(_bg_jobs) >= BG_MAX_JOBS:
            raise HTTPException(
                status_code=429,
                detail=f"too many active jobs ({BG_MAX_JOBS}). Stop existing jobs first.",
            )
    env = os.environ.copy()
    if request.env:
        env.update(request.env)
    try:
        # The same ceilings the foreground path installs. A background job is
        # untrusted code with a longer life, not a different trust class, and
        # before this it was the one execution site with no memory bound at
        # all. setsid comes from the shared owner's preexec, which is also
        # what /jobs/stop's killpg depends on.
        env[EXEC_TOKEN_VAR] = uuid.uuid4().hex
        proc = subprocess.Popen(
            ["bash", "-c", request.command],
            cwd=str(cwd),
            env=env,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            preexec_fn=_apply_child_limits(EXEC_CONTRACT),
        )
    except (OSError, ValueError) as e:
        raise HTTPException(status_code=500, detail=f"spawn failed: {e}")
    job_id = uuid.uuid4().hex[:12]
    job = {
        "proc": proc,
        "command": request.command,
        "started_at": time.time(),
        "stdout": deque(maxlen=BG_MAX_LINES),
        "stderr": deque(maxlen=BG_MAX_LINES),
    }
    with _bg_lock:
        _bg_jobs[job_id] = job
    threading.Thread(target=_bg_drain_stream, args=(job_id, "stdout", proc.stdout), daemon=True).start()
    threading.Thread(target=_bg_drain_stream, args=(job_id, "stderr", proc.stderr), daemon=True).start()
    return BackgroundStartResponse(job_id=job_id, pid=proc.pid, started_at=job["started_at"])


@app.get("/jobs", response_model=BackgroundJobListResponse)
def background_list():
    """Every job this sandbox is holding, whoever started it.

    The registry is process-wide and has no session concept, so a server left
    running by an earlier session keeps its port and the next session cannot
    name it: `/jobs/{id}` needs an id it never saw. Without this the agent
    gets "Address already in use" and the advice to "identify and stop that
    program" with no way to do either.
    """
    with _bg_lock:
        items = [
            BackgroundJobSummary(
                job_id=job_id,
                command=job["command"],
                started_at=job["started_at"],
                running=job["proc"].poll() is None,
            )
            for job_id, job in _bg_jobs.items()
        ]
    items.sort(key=lambda j: j.started_at)
    return BackgroundJobListResponse(jobs=items)


@app.get("/jobs/{job_id}/output", response_model=BackgroundOutputResponse)
def background_output(job_id: str, lines: int = 50):
    """Snapshot of the job's recent stdout/stderr + run state."""
    with _bg_lock:
        job = _bg_jobs.get(job_id)
        if job is None:
            raise HTTPException(status_code=404, detail=f"unknown job_id: {job_id}")
        proc = job["proc"]
        rc = proc.poll()
        running = rc is None
        if not running and "ended_at" not in job:
            job["ended_at"] = time.time()
        # Snapshot the deques (thread-safe copy under lock)
        stdout = list(job["stdout"])[-max(1, lines):]
        stderr = list(job["stderr"])[-max(1, lines):]
        elapsed = time.time() - job["started_at"]
        cmd = job["command"]
    return BackgroundOutputResponse(
        job_id=job_id, running=running, exit_code=rc,
        stdout=stdout, stderr=stderr, elapsed_sec=elapsed, command=cmd,
    )


@app.post("/jobs/{job_id}/stop", response_model=BackgroundStopResponse)
def background_stop(job_id: str):
    """SIGTERM the process group, wait briefly, SIGKILL if still alive.
    Returns the final stdout/stderr buffer."""
    with _bg_lock:
        job = _bg_jobs.get(job_id)
        if job is None:
            raise HTTPException(status_code=404, detail=f"unknown job_id: {job_id}")
        proc = job["proc"]
    killed = False
    if proc.poll() is None:
        try:
            os.killpg(proc.pid, signal.SIGTERM)
        except ProcessLookupError:
            # best-effort: swallow on failure (caller continues)
            pass
        try:
            proc.wait(timeout=3)
        except subprocess.TimeoutExpired:
            try:
                os.killpg(proc.pid, signal.SIGKILL)
            except ProcessLookupError:
                # best-effort: swallow on failure (caller continues)
                pass
            proc.wait(timeout=2)
        killed = True
    with _bg_lock:
        job["ended_at"] = time.time()
        stdout = list(job["stdout"])
        stderr = list(job["stderr"])
    return BackgroundStopResponse(
        job_id=job_id, killed=killed, exit_code=proc.poll(),
        stdout=stdout[-50:], stderr=stderr[-50:],
    )


@app.post("/shell", response_model=ShellResponse)
async def run_shell(request: ShellRequest, http: Request):
    """Run a shell command against the bind-mounted workspace.

    Async, and the reason is cancellation. A caller that goes away -- a reset
    connection, a closed one, an aborted request, a shutdown -- used to leave
    the command running to its own deadline, holding a CPU and a memory budget
    for an answer nobody would read, and producing evidence about a request
    that no longer existed. A synchronous handler cannot notice: it is already
    blocked in the threadpool when the socket dies.

    So the work runs in a worker and this coroutine watches the connection.
    Starlette's is_disconnected is the supported way to ask; the executor's own
    cancellation callback is the supported way to tell. Nothing here touches a
    socket directly, and the bounded runner still owns every ceiling and every
    kill -- cancellation is one more reason it already knows how to stop for,
    and it stays distinct from a timeout and from resource exhaustion in the
    outcome it reports.
    """
    if not request.command or not request.command.strip():
        raise HTTPException(status_code=400, detail="command is required")

    timeout = min(max(1, request.timeout), MAX_EXECUTION_TIME)

    # One flag per request, never shared. A flag reused across requests is how
    # a closed descriptor cancels whoever inherits its number next.
    gone = threading.Event()

    async def _watch_downstream():
        """Set the flag when the caller stops waiting for the answer."""
        try:
            while not gone.is_set():
                if await http.is_disconnected():
                    gone.set()
                    return
                await asyncio.sleep(0.1)
        except asyncio.CancelledError:
            raise
        except Exception:  # a transport that cannot be asked is not a cancel
            return

    snapshot = None
    root = WORKSPACE_ROOT
    try:
        if request.files:
            snapshot = _snapshot_workspace_with_overlay(request.files)
            root = snapshot
        # Resolve cwd. Default to /workspace (or the temp overlay root);
        # if the caller provides one, require it to live under the
        # workspace boundary. The path must already exist (no auto-mkdir).
        cwd = _resolve_shell_cwd(request.cwd, root)
        command = _translate_workspace_command(request.command, root)

        observation = None
        before_paths = {}
        before_digest = {}
        if request.observe_paths is not None:
            before_paths = _observe_paths(root, request.observe_paths)
            before_digest = _workspace_digest(root)

        start = time.time()
        watcher = asyncio.ensure_future(_watch_downstream())
        try:
            result = await asyncio.to_thread(
                _run_cmd, ["bash", "-c", command], timeout, cwd, request.env,
                None, gone.is_set)
        except asyncio.CancelledError:
            # An explicit cancellation or a shutdown. The worker is told to
            # stop by the same flag the watcher uses, and it takes the process
            # tree with it before this returns.
            gone.set()
            raise
        finally:
            watcher.cancel()
        elapsed_ms = int((time.time() - start) * 1000)

        if request.observe_paths is not None:
            after_digest = _workspace_digest(root)
            observation = ShellObservation(
                target_before=before_paths,
                target_after=_observe_paths(root, request.observe_paths),
                workspace_before=str(before_digest.get("digest", "")),
                workspace_after=str(after_digest.get("digest", "")),
                workspace_files=int(after_digest.get("files", 0)),
                digest_truncated=bool(before_digest.get("truncated"))
                or bool(after_digest.get("truncated")),
            )

        return ShellResponse(
            success=result["success"],
            stdout=result["stdout"],
            stderr=result["stderr"],
            exit_code=result["returncode"],
            elapsed_ms=elapsed_ms,
            timed_out=bool(result.get("timed_out")),
            # HOW it ended, not merely whether it exited zero. A command
            # stopped at a resource ceiling exits non-zero exactly like a
            # failing test, and the caller has to be able to tell them apart
            # without reading stderr for a phrase.
            outcome=str(result.get("outcome", OUTCOME_UNCLASSIFIED)),
            peak_memory_bytes=int(result.get("peak_memory_bytes", 0)),
            observation=observation,
        )
    finally:
        if snapshot is not None:
            shutil.rmtree(snapshot, ignore_errors=True)


@app.post("/execute", response_model=ExecuteResponse)
def execute_code(request: ExecuteRequest):
    """Execute code in isolated environment."""
    lang = normalize_language(request.language)

    if lang not in ("python", "javascript", "typescript", "go", "java", "kotlin", "rust", "c", "cpp", "ruby", "php", "bash"):
        raise HTTPException(
            status_code=400,
            detail=f"Language '{request.language}' not supported. Supported: python, javascript, typescript, go, java, kotlin, rust, c, cpp, ruby, php, bash"
        )

    workspace = tempfile.mkdtemp(dir=WORKSPACE_BASE)
    timeout = min(request.timeout, MAX_EXECUTION_TIME)

    # PC-046: Drop project-context files into the workspace BEFORE the
    # language handler runs so any `import other_module` in the candidate
    # resolves against the rest of the user's project. Routed through the
    # same O_NOFOLLOW/dir_fd walking helper as the /shell overlay writer so
    # both write paths enforce identical containment (no absolute paths, no
    # `..` traversal, no symlink components). Bad entries are skipped per
    # file (we don't want a malformed name to block legitimate verification).
    if request.files:
        for name, content in request.files.items():
            try:
                _write_overlay_files(Path(workspace), {name: content})
            except HTTPException as e:
                # Strip CR/LF so a crafted filename/detail can't forge
                # extra log records (py/log-injection).
                safe_name = str(name).replace("\r", "").replace("\n", "")
                safe_detail = str(e.detail).replace("\r", "").replace("\n", "")
                logger.warning(
                    "PC-046: rejected sandbox file %r: %s",
                    safe_name, safe_detail)

    try:
        handler = LANGUAGE_HANDLERS[lang]
        result = handler(
            code=request.code,
            test_code=request.test_code,
            workspace=Path(workspace),
            timeout=timeout,
            requirements=request.requirements,
            stdin=request.stdin,
        )
        return result
    except Exception as e:
        logger.exception(f"Execution error for {lang}")
        return ExecuteResponse(
            success=False,
            compile_success=False,
            tests_run=0,
            tests_passed=0,
            stdout="",
            stderr=str(e),
            error_type=type(e).__name__,
            error_message=str(e),
            execution_time_ms=0,
        )
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


class SyntaxCheckRequest(BaseModel):
    code: str
    language: str = "python"
    filename: Optional[str] = None


class SyntaxCheckResponse(BaseModel):
    valid: bool
    errors: List[str]
    language: str
    check_time_ms: int
    # "checked": the checker ran to its own conclusion and `valid` is its
    # verdict. "not_run": the resource contract stopped it, or it never
    # started, so there is no verdict; `valid` is false and `outcome` says how
    # it ended. A stopped checker exits non-zero with empty stderr, and every
    # branch below builds its errors from stderr, so it used to read as a clean
    # parse (valid: true).
    status: str = "checked"
    outcome: str = OUTCOME_COMPLETED


@app.post("/syntax-check", response_model=SyntaxCheckResponse)
def syntax_check(request: SyntaxCheckRequest):
    """Check code syntax without executing. Returns parse/compile errors."""
    lang = normalize_language(request.language)
    workspace = tempfile.mkdtemp(dir=WORKSPACE_BASE)
    start = time.time()

    try:
        errors = _syntax_check_impl(lang, request.code, Path(workspace), request.filename)
        elapsed = int((time.time() - start) * 1000)
        return SyntaxCheckResponse(
            valid=len(errors) == 0,
            errors=errors,
            language=lang,
            check_time_ms=elapsed,
        )
    except _CheckNotFinished as stopped:
        elapsed = int((time.time() - start) * 1000)
        return SyntaxCheckResponse(
            valid=False,
            errors=[f"syntax verification unavailable: the checker ended {stopped.outcome}"],
            language=lang,
            check_time_ms=elapsed,
            status="not_run",
            outcome=stopped.outcome,
        )
    except HTTPException:
        raise
    except Exception as e:
        elapsed = int((time.time() - start) * 1000)
        return SyntaxCheckResponse(
            valid=False,
            errors=[str(e)],
            language=lang,
            check_time_ms=elapsed,
        )
    finally:
        shutil.rmtree(workspace, ignore_errors=True)

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


def _looks_like_jinja_template(filename: Optional[str]) -> bool:
    """True only for files that are Jinja templates by convention: under a
    templates/ directory, or a .jinja/.jinja2 name. Vue/Angular/Handlebars HTML
    shares `{{ }}` but is NOT Jinja and lives elsewhere, so it is never handed
    to a Jinja parser."""
    if not filename:
        return False
    f = filename.replace("\\", "/").lower()
    return ("templates/" in f) or f.endswith((".jinja", ".jinja2"))


def _jinja_template_errors(code: str, filename: Optional[str]) -> List[str]:
    """Jinja syntax errors for a file that is actually a Jinja template, else
    []. Requires a statement tag `{%` to be present -- `{{ }}` interpolation
    alone is too widely shared to attribute to Jinja. Unknown-tag/filter/test
    errors are dropped: those signal a third-party extension this parser has
    not loaded, not a typo. Never raises."""
    if not _looks_like_jinja_template(filename):
        return []
    if "{%" not in code:
        return []
    try:
        import jinja2
    except Exception:
        return []  # fail open: no Jinja available here
    try:
        jinja2.Environment(autoescape=True).parse(code)  # parsed only, never rendered
        return []
    except jinja2.TemplateSyntaxError as e:
        msg = (getattr(e, "message", "") or "").lower()
        if any(s in msg for s in ("unknown tag", "no filter named",
                                  "no test named", "not registered")):
            return []
        loc = f" (line {e.lineno})" if getattr(e, "lineno", None) else ""
        return [f"TemplateSyntaxError: {e.message}{loc}"]
    except Exception:
        return []  # not a confident syntax verdict -- stay silent


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


class _StructureParser(HTMLParser):
    """Counts the markup html.parser saw."""

    def __init__(self):
        super().__init__()
        self.markup = 0

    def handle_starttag(self, tag, attrs):
        self.markup += 1

    def handle_startendtag(self, tag, attrs):
        self.markup += 1

    def handle_decl(self, decl):
        self.markup += 1


def _html_structure_errors(code: str) -> List[str]:
    """What can be said of HTML without rejecting what HTML allows.

    html.parser accepts any text -- JavaScript, Python, an empty string, a
    page cut off inside its <script> -- so a clean parse proved nothing, and
    it was counted as a syntax pass for completion. Two checks can fail: the
    text has markup at all (a template made only of {% %} or {{ }} blocks is
    exempt), and it does not end inside a tag, a comment, or a <script> or
    <style> element, which is what a cut-off generation leaves. Unclosed
    ordinary elements, fragments and Vue or Angular templates still pass.
    """
    parser = _StructureParser()
    try:
        parser.feed(code)
    except Exception as e:
        return [str(e)]
    errors = []
    unclosed = getattr(parser, "cdata_elem", None)
    leftover = getattr(parser, "rawdata", "")
    if unclosed:
        errors.append(f"the document ends inside an unclosed <{unclosed}> element")
    elif re.match(r"<[!/?A-Za-z]", leftover.lstrip()):
        errors.append("the document ends inside an unfinished tag or comment: "
                      + leftover.lstrip()[:60])
    if parser.markup == 0 and "{%" not in code and "{{" not in code:
        errors.append("no HTML markup: the text has no tags")
    return errors


def _syntax_check_impl(lang: str, code: str, workspace: Path, filename: Optional[str] = None) -> List[str]:
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

    elif lang == "go":
        fpath = _contained_path(workspace, filename or "main.go")
        fpath.write_text(code)
        # Use gofmt -e for fast syntax-only checking (no compilation, no go.mod needed)
        result = _run_check(["gofmt", "-e", str(fpath)], timeout=5, cwd=workspace)
        if result["returncode"] != 0:
            stderr = result.get("stderr", "")
            for line in stderr.splitlines():
                line = line.strip()
                if line:
                    errors.append(line)

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

    elif lang == "kotlin":
        fpath = _contained_path(workspace, filename or "Source.kt")
        fpath.write_text(code)
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

    elif lang == "rust":
        fpath = _contained_path(workspace, filename or "check.rs")
        fpath.write_text(code)
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

    elif lang in ("c", "cpp"):
        ext = ".c" if lang == "c" else ".cpp"
        fpath = _contained_path(workspace, filename or f"check{ext}")
        fpath.write_text(code)
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

    elif lang == "ruby":
        fpath = _contained_path(workspace, filename or "main.rb")
        fpath.write_text(code)
        # Use ruby -c for syntax-only checking (no execution)
        result = _run_check(["ruby", "-c", str(fpath)], timeout=5, cwd=workspace)
        if result["returncode"] != 0:
            stderr = result.get("stderr", "")
            for line in stderr.splitlines():
                if "syntax error" in line or "error" in line.lower():
                    errors.append(line.strip())
            if not errors and stderr.strip():
                errors.append(stderr.strip().split("\n")[-1])

    elif lang == "php":
        fpath = _contained_path(workspace, filename or "main.php")
        fpath.write_text(code)
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

    elif lang == "bash":
        fpath = _contained_path(workspace, filename or "check.sh")
        fpath.write_text(code)
        result = _run_check(["bash", "-n", str(fpath)], timeout=5, cwd=workspace)
        if result["returncode"] != 0:
            errors.append(result.get("stderr", "").strip())

    elif lang == "json":
        try:
            json.loads(code)
        except json.JSONDecodeError as e:
            errors.append(str(e))

    elif lang in ("yaml", "yml"):
        try:
            import yaml
            # safe_load_all, not safe_load: a multi-document file ("a: 1"
            # --- "b: 2") is valid YAML and extremely common in Kubernetes
            # and Compose manifests, but safe_load rejects it with "expected
            # a single document in the stream". That false rejection is why
            # the proxy's direct-write path carried no syntax gate at all,
            # which let genuinely unparseable files reach disk. list() forces
            # the generator so every document is actually parsed.
            list(yaml.safe_load_all(code))
        except Exception as e:
            errors.append(str(e))

    elif lang in ("html", "htm"):
        errors.extend(_html_structure_errors(code))
        # A Jinja template can parse cleanly as HTML and still 500 on EVERY
        # render: `{% for x in xs %)` closes the tag with `)` instead of `}`.
        # html.parser sees only text and passes it; Flask compiles the template
        # on first render, so the server starts, the file imports and the page
        # returns 500 with a TemplateSyntaxError nothing upstream caught.
        errors.extend(_jinja_template_errors(code, filename))

    elif lang == "xml":
        from defusedxml import ElementTree as ET
        from defusedxml.common import DefusedXmlException
        try:
            ET.fromstring(code)
        except (ET.ParseError, DefusedXmlException) as e:
            errors.append(str(e))

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
    except Exception:
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

# --- Rust ---

def execute_rust(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "main.rs"
    main_file.write_text(code)

    # Compile
    binary = workspace / "program"
    r = _run_cmd(["rustc", str(main_file), "-o", str(binary)], 30)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    # Run
    r = _run_cmd([str(binary)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- C ---

def execute_c(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "solution.c"
    main_file.write_text(code)

    binary = workspace / "program"
    r = _run_cmd(["gcc", "-o", str(binary), str(main_file), "-lm", "-Wall"], 15)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    r = _run_cmd([str(binary)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


# --- C++ ---

def execute_cpp(code, test_code, workspace, timeout, stdin=None, **_):
    start = time.time()
    main_file = workspace / "solution.cpp"
    main_file.write_text(code)

    binary = workspace / "program"
    r = _run_cmd(["g++", "-o", str(binary), str(main_file), "-std=c++17", "-Wall"], 15)
    if not r["success"]:
        return ExecuteResponse(
            success=False, compile_success=False,
            tests_run=0, tests_passed=0,
            stdout="", **why_it_failed(r, "CompileError", _classify_error),
            execution_time_ms=int((time.time() - start) * 1000),
        )

    r = _run_cmd([str(binary)], timeout, cwd=workspace, stdin=stdin)

    return ExecuteResponse(
        success=r["success"], compile_success=True,
        tests_run=1, tests_passed=1 if r["success"] else 0,
        stdout=r["stdout"], **why_it_failed(r, None, _classify_error),
        execution_time_ms=int((time.time() - start) * 1000),
    )


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


if __name__ == "__main__":
    import uvicorn
    WORKSPACE_BASE.mkdir(parents=True, exist_ok=True)
    logger.info(f"Supported languages: {list(LANGUAGE_HANDLERS.keys())}")
    uvicorn.run(app, host="0.0.0.0", port=8020)
