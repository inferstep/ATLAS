"""Tests for `atlas bench` — the first step of the model-onboarding loop
(bench → lens build --from-results → asa build).

The V3 runner itself needs a live llama-server, so it is faked at the
subprocess boundary: these tests pin down argument validation, the exact
runner invocation (command, env, cwd), and the exit-code contract for
failure / empty / successful runs.
"""

import json
import subprocess
from types import SimpleNamespace

import pytest

from atlas.bench.config import BenchmarkConfig
from atlas.commands import bench


def _fake_runner(monkeypatch, lines, returncode):
    """Make the runner that bench() starts print `lines` and exit with
    `returncode`. Returns the list of Popen calls."""
    calls = []

    def fake_popen(cmd, **kwargs):
        calls.append({"cmd": cmd, **kwargs})
        return SimpleNamespace(stdout=iter(lines), returncode=returncode,
                               wait=lambda: returncode)

    # bench() does `import subprocess` when it runs, so patch the module.
    monkeypatch.setattr(subprocess, "Popen", fake_popen)
    return calls


@pytest.mark.parametrize(
    ("argv", "message"),
    [
        (["--tasks", "-3"], "--tasks must be >= 0"),
        (["--strategy", "vibes"], "invalid choice"),
    ],
    ids=["negative-tasks", "unknown-strategy"],
)
def test_bad_argument_is_a_usage_error(capsys, argv, message):
    with pytest.raises(SystemExit) as exc:
        bench.main(argv)
    assert exc.value.code == 2
    assert message in capsys.readouterr().err


def test_runner_invocation_command_env_and_cwd(tmp_path, monkeypatch):
    monkeypatch.setattr(bench, "_atlas_root", lambda: tmp_path)
    calls = _fake_runner(monkeypatch, lines=[], returncode=1)

    bench.main(["--tasks", "5", "--strategy", "lens", "--run-id", "r1"])

    assert len(calls) == 1
    cmd = calls[0]["cmd"]
    assert cmd[1:3] == ["-m", "atlas.bench.v3_runner"]
    assert "--baseline" in cmd
    assert cmd[cmd.index("--run-id") + 1] == "r1"
    assert cmd[cmd.index("--selection-strategy") + 1] == "lens"
    assert cmd[cmd.index("--max-tasks") + 1] == "5"
    # Repo root as cwd so the benchmark package resolves regardless of
    # where the operator invoked the CLI from.
    assert calls[0]["cwd"] == str(tmp_path)
    # Generation stays serialized — the safe default for any model.
    assert calls[0]["env"]["ATLAS_PARALLEL_TASKS"] == "1"


def test_zero_tasks_omits_max_tasks_flag(tmp_path, monkeypatch):
    monkeypatch.setattr(bench, "_atlas_root", lambda: tmp_path)
    calls = _fake_runner(monkeypatch, lines=[], returncode=1)

    bench.main(["--run-id", "r2"])

    assert "--max-tasks" not in calls[0]["cmd"]


@pytest.mark.parametrize(
    ("runner_lines", "runner_exit", "expected_output"),
    [
        # A failing runner: the exit code and the runner's last lines,
        # which hold the actual failure.
        (["Loading LiveCodeBench", "boom: connection refused"], 3,
         ["exited with code 3", "connection refused"]),
        # A runner that exits 0 having processed nothing (an aborted
        # pre-flight): stale on-disk results must not be summarized as
        # this run's output.
        ([], 0, ["without processing any tasks"]),
    ],
    ids=["runner-fails", "runner-processes-no-tasks"],
)
def test_run_without_new_results_exits_one(tmp_path, monkeypatch, capsys,
                                           runner_lines, runner_exit,
                                           expected_output):
    monkeypatch.setattr(bench, "_atlas_root", lambda: tmp_path)
    _fake_runner(monkeypatch, runner_lines, runner_exit)

    assert bench.main(["--run-id", "r3"]) == 1

    out = capsys.readouterr().out
    for text in expected_output:
        assert text in out


def test_successful_run_reports_results_and_exits_zero(tmp_path, monkeypatch,
                                                       capsys):
    monkeypatch.setattr(bench, "_atlas_root", lambda: tmp_path)
    per_task = tmp_path / "benchmark" / "results" / "r5" / "v3_lcb" / "per_task"
    per_task.mkdir(parents=True)
    (per_task / "t1.json").write_text(json.dumps({"passed": True}))
    (per_task / "t2.json").write_text(json.dumps({"passed": False}))
    (per_task / "t3.json").write_text("{not json")  # counts as a failure
    _fake_runner(monkeypatch,
                 lines=["[1/3] LCB t1: PASS", "[2/3] LCB t2: FAIL",
                        "[3/3] LCB t3: FAIL", "BENCHMARK COMPLETE"],
                 returncode=0)

    assert bench.main(["--run-id", "r5"]) == 0

    out = capsys.readouterr().out
    assert "pass@1: 1/3" in out
    # The next onboarding step is printed for the operator.
    assert "lens build --force --from-results" in out


def test_runner_results_folder_is_the_one_atlas_bench_reads():
    # bench() reads <repo root>/benchmark/results/<run-id>/; the runner
    # writes under the bench config's results_dir. If the two drift apart,
    # a finished run reports "No results found".
    assert BenchmarkConfig().results_dir == bench._atlas_root() / "benchmark" / "results"
