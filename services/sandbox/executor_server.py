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
# tests/infrastructure/sandbox/test_boundary_regression.py imports this module with
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
from typing import Dict, Optional, List, Tuple
from pathlib import Path
from fastapi import FastAPI, HTTPException, Request
from pydantic import BaseModel

logging.basicConfig(level=logging.INFO)
# Structured logging (JSON when ATLAS_LOG_FORMAT=json) + private-value
# masking + correlation IDs (see structured_log.py — canonical copy).
from structured_log import (install as _install_logging,
                            set_request_id as _set_rid)
_install_logging("sandbox")
logger = logging.getLogger(__name__)

app = FastAPI(title="ATLAS Code Execution Sandbox")


def _load_service_token() -> str:
    """Internal-auth token (Authorization: Bearer). No file = auth disabled (`atlas doctor` warns). Never logged. The
    highest-value enforcement point: /shell and /execute run commands against the bind-mounted workspace."""
    path = os.getenv("ATLAS_SERVICE_TOKEN_FILE",
                     "/run/atlas-secrets/service-token")
    try:
        with open(path) as fh:
            return fh.read().strip()
    except FileNotFoundError:
        return ""
    except OSError as error:  # there but unreadable: without a token the sandbox would check nothing
        raise SystemExit(f"the service token file {path} cannot be read ({error.strerror}), so the sandbox stops rather "
                         f"than run with no check. It runs as user {os.getuid()}: make the file readable for that user.") from error


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

from resource_contract import (
    OUTCOME_UNCLASSIFIED, EXEC_TOKEN_VAR, _apply_child_limits)

# The parts of the executor that live in modules beside it (services/sandbox/Dockerfile copies each one).
from executor_models import (
    ExecuteRequest, ExecuteResponse, ShellObservation, ShellRequest, ShellResponse,
    SyntaxCheckRequest, SyntaxCheckResponse)
import executor_languages
from executor_languages import (
    EXEC_CONTRACT,
    LANGUAGE_HANDLERS,
    _CheckNotFinished,
    _resource_limits_label,
    _run_cmd,
    _safe_overlay_path,
    _syntax_check_impl,
)
from start_up import serve

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

def normalize_language(lang: str) -> str:  # noqa: C901
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


@app.get("/health")
def health_check():
    resp = {
        "status": "healthy",
        "limits_enforced": executor_languages.SUPERVISION_AVAILABLE,
        "resource_limits": _resource_limits_label(),
    }
    if not executor_languages.SUPERVISION_AVAILABLE:
        resp["resource_limits_reason"] = executor_languages.SUPERVISION_REASON
    return resp


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


def _shell_observation_before(request: ShellRequest, root: Path) -> Tuple[Dict[str, str], Dict[str, object]]:
    """The hashes of the observed paths and of the workspace before a /shell command, when the request asks for them."""
    before_paths = {}
    before_digest = {}
    if request.observe_paths is not None:
        before_paths = _observe_paths(root, request.observe_paths)
        before_digest = _workspace_digest(root)
    return before_paths, before_digest


def _shell_observation(request: ShellRequest, root: Path, before_paths: Dict[str, str],
                       before_digest: Dict[str, object]) -> ShellObservation:
    """The observation of one /shell command: the hashes taken before it, and the ones taken now, after it."""
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
    return observation


def _shell_response(result: Dict, elapsed_ms: int, observation: Optional[ShellObservation]) -> ShellResponse:
    """The /shell answer: how the command ended, what it wrote, how long it ran, and any observation asked for."""
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
        limits_enforced=result.get("limits_enforced", False),
        resource_limits=result.get("resource_limits"),
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
        before_paths, before_digest = _shell_observation_before(request, root)

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
            observation = _shell_observation(request, root, before_paths, before_digest)

        return _shell_response(result, elapsed_ms, observation)
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
        result.limits_enforced = executor_languages.SUPERVISION_AVAILABLE
        result.resource_limits = _resource_limits_label()
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
            limits_enforced=executor_languages.SUPERVISION_AVAILABLE,
            resource_limits=_resource_limits_label(),
        )
    finally:
        shutil.rmtree(workspace, ignore_errors=True)


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


if __name__ == "__main__":
    logger.info(f"Supported languages: {list(LANGUAGE_HANDLERS.keys())}")
    serve(app, WORKSPACE_BASE)
