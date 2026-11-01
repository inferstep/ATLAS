"""The request and response bodies of the /execute, /syntax-check, and /shell endpoints."""

from __future__ import annotations

from typing import Dict, List, Optional

from pydantic import BaseModel

from resource_contract import OUTCOME_COMPLETED


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
    limits_enforced: bool = False
    resource_limits: Optional[str] = None


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
    limits_enforced: bool = False
    resource_limits: Optional[str] = None
