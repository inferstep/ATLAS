"""Grade a workspace with a task's grader, in a container with no network."""
from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
from dataclasses import dataclass
from pathlib import Path
from typing import Optional

PASS, FAIL, GRADER_ERROR = "pass", "fail", "grader_error"
TIMEOUT_EXIT = 124  # what coreutils `timeout` exits with
OUTPUT_CAP = 1_000_000  # characters of grader output kept per session


@dataclass(frozen=True)
class Grade:
    outcome: str      # PASS, FAIL or GRADER_ERROR
    reason: str       # the grader's first line, or why it could not grade
    output: str = ""  # the grader's whole output, for the private record


def build_workspace(task, dest: Path, overlay: Optional[Path] = None) -> None:
    """dest holds the task's seed, with the overlay's files on top."""
    dest.mkdir(parents=True, exist_ok=True)
    for src in (task.seed, overlay):
        if src is not None and src.is_dir():
            shutil.copytree(src, dest, dirs_exist_ok=True)


def grade(task, workspace: Path, image: str, run=subprocess.run) -> Grade:
    """The task's grader on a copy of the workspace, so it cannot change the original."""
    with tempfile.TemporaryDirectory(prefix="eval-grade-") as tmp:
        copy = Path(tmp) / "w"
        try:
            # Links stay links, as the sandbox saw them: the host never follows
            # one out of the workspace, and a dangling one is part of the work.
            shutil.copytree(workspace, copy, symlinks=True)
        except OSError as e:  # shutil.Error too: one task's copy must not stop the block
            return Grade(GRADER_ERROR, f"the workspace could not be copied: {str(e)[:200]}")
        try:
            p = run(grader_argv(task, copy, image), capture_output=True, text=True,
                    timeout=task.grader_timeout_s + 60)
        except subprocess.TimeoutExpired:
            return Grade(GRADER_ERROR, f"the grader container outlived {task.grader_timeout_s} s")
        except OSError as e:
            return Grade(GRADER_ERROR, f"the grader could not start: {e}")
    g = outcome_of(p.returncode, p.stdout, p.stderr, task.grader_timeout_s)
    return Grade(g.outcome, g.reason, full_output(p.stdout, p.stderr))


def full_output(stdout: str, stderr: str) -> str:
    """stdout, then stderr under a marker, cut at OUTPUT_CAP with a note."""
    text = (stdout or "") + (f"\n[stderr]\n{stderr}" if stderr else "")
    if len(text) > OUTPUT_CAP:
        return text[:OUTPUT_CAP] + f"\n[cut: the grader wrote {len(text)} characters]"
    return text


def resolve_image(image: str, run=subprocess.run) -> str:
    """The image's content ID (sha256:...), so every grade in a block uses one
    image even if its tag moves; "" when docker does not know it."""
    try:
        p = run(["docker", "image", "inspect", "--format", "{{.Id}}", image],
                capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.TimeoutExpired):
        return ""
    out = (p.stdout or "").strip()
    return out if p.returncode == 0 and out.startswith("sha256:") else ""


def grader_argv(task, copy: Path, image: str) -> list:
    """A throwaway container from the sandbox image: no network, the caller's
    uid, the copy at /w, the grader (and its data, if any) read-only."""
    argv = ["docker", "run", "--rm", "--network", "none",
            "--user", f"{os.getuid()}:{os.getgid()}",
            "-v", f"{copy}:/w", "-v", f"{task.grader}:/grade:ro"]
    data = task.dir / "grader"
    if data.is_dir():
        argv += ["-v", f"{data}:/grader:ro"]
    return argv + ["-w", "/w", "--entrypoint", "timeout", image,
                   str(task.grader_timeout_s), "/grade", "/w"]


def outcome_of(code: int, stdout: str, stderr: str, timeout_s: int) -> Grade:
    """Exit 0 passes and 1 fails. Anything else says nothing about the work."""
    line = _first_line(stdout) or _first_line(stderr)
    if code == 0:
        return Grade(PASS, line)
    if code == 1:
        return Grade(FAIL, line)
    if code == TIMEOUT_EXIT:
        return Grade(GRADER_ERROR, f"the grader ran past {timeout_s} s")
    return Grade(GRADER_ERROR, f"the grader exited {code}: {line}")


def check_controls(task, image: str, run=subprocess.run) -> list:
    """Why this task's grader cannot be trusted, if it can't: its reference
    must pass and its plausible wrong answer must fail."""
    problems = []
    for kind, want in (("pass", PASS), ("fail", FAIL)):
        if not task.control(kind).is_dir():
            problems.append(f"{task.id}: no controls/{kind}")
            continue
        with tempfile.TemporaryDirectory(prefix="eval-control-") as tmp:
            ws = Path(tmp) / "w"
            build_workspace(task, ws, overlay=task.control(kind))
            got = grade(task, ws, image, run)
        if got.outcome != want:
            problems.append(f"{task.id}: controls/{kind} graded {got.outcome} ({got.reason})")
    return problems


def _first_line(text: str) -> str:
    for line in (text or "").splitlines():
        if line.strip():
            return line.strip()[:200]
    return ""
