"""The request and response bodies of the /execute and /syntax-check endpoints."""

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
