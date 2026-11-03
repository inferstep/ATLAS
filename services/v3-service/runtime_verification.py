"""Compare Python initialization before a candidate replaces submitted code.

This observes the existing sandbox environment, NOT a clean installation or
the user's requirements. Only a successful baseline import makes a later
candidate failure a regression. Candidate code never executes in this service.
"""

import hashlib
import shlex
from pathlib import PurePosixPath

from scoring import _project_relative_path, _bounded_evidence


IMPORT_COMPLETE = "__ATLAS_IMPORT_COMPLETED__"


class PythonImportComparison:
    """Request-local, bounded observations through the sandbox overlay runner."""

    def __init__(self, sandbox, baseline, file_path, working_dir,
                 remaining_ms=None, check_cancel=None):
        self.sandbox = sandbox
        self.baseline = baseline
        self.working_dir = working_dir or "/workspace"
        self.remaining_ms = remaining_ms
        self.check_cancel = check_cancel
        self.cache = {}
        self.unavailable = ""
        self.path = ""
        self.overlay_path = ""
        self.command = ""
        try:
            self.path = _project_relative_path(file_path, self.working_dir)
            # /shell overlays are relative to /workspace, not its selected
            # working directory. Keep that boundary distinct from imports.
            subdir = _project_relative_path(self.working_dir, "/workspace")
            self.overlay_path = str(PurePosixPath(subdir) / self.path)
            path = PurePosixPath(self.path)
            parts = list(path.with_suffix("").parts)
            if parts and parts[-1] == "__init__":
                parts.pop()
            if path.suffix != ".py" or not parts or not all(p.isidentifier() for p in parts):
                raise ValueError("target is not an importable Python module path")
            # The working-directory snapshot supplies complete sibling files;
            # prompt context may be truncated and must not replace them.
            script = (
                "import importlib,pathlib,sys; "
                f"p=pathlib.Path({self.path!r}).resolve(); "
                "sys.path.insert(0,str(p.parent)); sys.path.insert(0,str(pathlib.Path.cwd())); "
                f"m=importlib.import_module({'.'.join(parts)!r}); "
                "assert pathlib.Path(m.__file__).resolve()==p, 'import resolved outside candidate'; "
                f"print({IMPORT_COMPLETE!r})"
            )
            self.command = "python -c " + shlex.quote(script)
        except ValueError as exc:
            self.unavailable = str(exc)

    @staticmethod
    def _hash(code):
        return hashlib.sha256(code.encode("utf-8")).hexdigest()

    def _observe(self, code):
        key = self._hash(code)
        if key in self.cache:
            return self.cache[key]
        observation = {"status": "unavailable", "exit_code": None,
                       "stdout": "", "stderr": "", "duration_ms": 0}
        if self.check_cancel:
            self.check_cancel()
        left = self.remaining_ms() if self.remaining_ms else None
        # run_command's transport allows ten seconds beyond its execution
        # timeout. Account for that within the existing pipeline deadline.
        timeout = 15 if left is None else min(15, int(left / 1000) - 10)
        if timeout < 1:
            observation["stderr"] = "insufficient remaining budget for an import comparison"
            return observation
        if not hasattr(self.sandbox, "run_command"):
            observation["stderr"] = "sandbox overlay runner unavailable"
            return observation
        try:
            ok, out, err, meta = self.sandbox.run_command(
                self.command, files={self.overlay_path: code},
                cwd=self.working_dir, timeout=timeout)
            meta = meta or {}
            exit_code = meta.get("exit_code")
            # A timeout, cancellation, transport error, or resource limit is
            # not evidence of a Python exception or a successful import.
            unavailable = (meta.get("timed_out") or
                           meta.get("outcome") not in (None, "", "completed") or
                           type(exit_code) is not int or exit_code < 0)
            status = "unavailable" if unavailable else (
                "passed" if ok and exit_code == 0 else "failed")
            # sys.exit(0) while importing is not a completed import. A zero
            # exit without the post-import marker establishes no observation.
            if status == "passed" and IMPORT_COMPLETE not in (out or "").splitlines():
                status = "unavailable"
                err = "interpreter exited without completing the target import"
            observation.update(status=status, exit_code=exit_code,
                               stdout=_bounded_evidence(out), stderr=_bounded_evidence(err),
                               duration_ms=int(meta.get("elapsed_ms") or 0))
        except Exception as exc:
            observation["stderr"] = f"sandbox import comparison unavailable: {type(exc).__name__}"
        self.cache[key] = observation
        return observation

    def check(self, code):
        """Return admission, diagnostic, evidence; never certify fulfillment."""
        evidence = {
            "verifier": "python_import_comparison", "status": "unavailable",
            "environment": "existing_sandbox_snapshot", "command": self.command,
            "baseline_hash": self._hash(self.baseline), "candidate_hash": self._hash(code),
        }
        if self.unavailable or not self.baseline:
            evidence["reason"] = self.unavailable or "no submitted baseline to compare"
            evidence["stderr"] = evidence["reason"]
            return True, "", evidence
        base = self._observe(self.baseline)
        evidence["baseline"] = dict(base)
        if base["status"] != "passed":
            evidence["reason"] = "baseline import not established; no comparative runtime verdict"
            evidence["stderr"] = evidence["reason"]
            return True, "", evidence
        candidate = self._observe(code)
        evidence["candidate"] = dict(candidate)
        # Retain the existing cross-service evidence fields: the Go transport
        # deliberately ignores fields it does not understand, including the
        # two detailed observations above.
        evidence.update(candidate)
        if candidate["status"] == "passed":
            return True, "", evidence
        detail = candidate["stderr"] or candidate["stdout"] or "no diagnostic returned"
        if candidate["status"] == "failed":
            error = ("Python import comparison: the submitted baseline imports, but this "
                     f"candidate does not import. {detail}")
        else:
            error = ("Python import comparison: the submitted baseline imports, but this "
                     f"candidate's import could not be observed. {detail}")
        evidence["reason"] = error
        return False, error, evidence
