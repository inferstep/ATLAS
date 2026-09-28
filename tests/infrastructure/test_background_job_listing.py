"""The sandbox must be able to name the jobs it is holding.

The job registry is process-wide and has no session concept, so a server an
earlier session left running keeps its port. `/jobs/{id}` needs an id the new
session never saw, and the bind failure's own advice — "identify and stop that
program" — was unfollowable. An observed session hit "Address already in use"
on port 5001 against a server started 50 minutes earlier by a different run.

GET /jobs lists the registry whole so the proxy can name the offender and the
model can stop_background it.
"""
import importlib.util
import os
import sys
from pathlib import Path

import pytest

SANDBOX_DIR = Path(__file__).resolve().parents[2] / "sandbox"


@pytest.fixture(scope="module")
def executor(tmp_path_factory):
    root = tmp_path_factory.mktemp("ws-jobs")
    os.environ["ATLAS_SANDBOX_WORKSPACE_ROOT"] = str(root)
    sys.path.insert(0, str(SANDBOX_DIR))
    spec = importlib.util.spec_from_file_location(
        "executor_server_jobs", SANDBOX_DIR / "executor_server.py")
    mod = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = mod
    try:
        spec.loader.exec_module(mod)
    except Exception:
        sys.modules.pop(spec.name, None)
        raise
    return mod


def test_an_empty_registry_lists_nothing(executor):
    assert executor.background_list().jobs == []


def test_a_running_job_is_listed_with_its_command(executor):
    started = executor.background_start(
        executor.BackgroundStartRequest(command="sleep 30"))
    try:
        jobs = executor.background_list().jobs
        mine = [j for j in jobs if j.job_id == started.job_id]
        assert len(mine) == 1, jobs
        assert mine[0].command == "sleep 30"
        assert mine[0].running is True
        assert mine[0].started_at > 0
    finally:
        executor.background_stop(started.job_id)


def test_a_stopped_job_is_listed_as_not_running(executor):
    """Still listed — the id is what makes a stale job actionable, and a
    just-exited job is worth telling apart from one still holding a port."""
    started = executor.background_start(
        executor.BackgroundStartRequest(command="sleep 30"))
    executor.background_stop(started.job_id)
    mine = [j for j in executor.background_list().jobs if j.job_id == started.job_id]
    assert len(mine) == 1
    assert mine[0].running is False
