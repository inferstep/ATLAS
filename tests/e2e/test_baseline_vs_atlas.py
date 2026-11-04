"""scripts/eval/baseline-vs-atlas.py: what main runs and what it reports.

The suite, llama-server's address and run_task are stand-ins, so no model
is asked and no task is run.
"""
import importlib.util
import json
import sys
from pathlib import Path
from types import SimpleNamespace

REPO = Path(__file__).resolve().parents[2]
_spec = importlib.util.spec_from_file_location(
    "baseline_vs_atlas", REPO / "scripts" / "eval" / "baseline-vs-atlas.py")
baseline_vs_atlas = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(baseline_vs_atlas)


def test_main_runs_each_arm_task_and_rep_and_counts_what_passed(monkeypatch, capsys, tmp_path):
    ran = []

    def run_task(suite, task, url, arm, attempts, temperature):
        ran.append((arm, task, attempts, temperature))
        # The loop arm passes everything; the one-shot arm only its first run.
        passed = arm == "loop" or len(ran) == 1
        return {"task": task, "arm": arm, "passed": passed, "detail": "d", "attempts": 1, "wall_s": 0.1}
    monkeypatch.setattr(baseline_vs_atlas, "_load_suite", lambda: SimpleNamespace(TASKS={"a": "a", "b": "b"}))
    monkeypatch.setattr(baseline_vs_atlas, "_llama_url", lambda: "http://llama")
    monkeypatch.setattr(baseline_vs_atlas, "run_task", run_task)
    out = tmp_path / "results.json"
    monkeypatch.setattr(sys, "argv", ["baseline-vs-atlas.py", "--tasks", " a, ,b", "--reps", "2",
                                      "--arms", "oneshot, loop,", "--attempts", "3", "--temperature", "0.2",
                                      "--json", str(out)])

    rc = baseline_vs_atlas.main()

    assert rc == 0
    assert [(arm, task) for arm, task, _, _ in ran] == [
        ("oneshot", "a"), ("oneshot", "a"), ("oneshot", "b"), ("oneshot", "b"),
        ("loop", "a"), ("loop", "a"), ("loop", "b"), ("loop", "b")]
    assert {(attempts, temperature) for _, _, attempts, temperature in ran} == {(3, 0.2)}
    lines = capsys.readouterr().out.splitlines()
    assert [line.split()[0] for line in lines if line.startswith("      ")] == ["PASS", "fail", "fail", "fail",
                                                                             "PASS", "PASS", "PASS", "PASS"]
    assert "oneshot    a                 1/2" in lines
    assert "oneshot    b                 0/2" in lines
    assert "oneshot    TOTAL             1/4  (25%)" in lines
    assert "loop       TOTAL             4/4  (100%)" in lines
    assert [row["rep"] for row in json.loads(out.read_text())] == [1, 2, 1, 2, 1, 2, 1, 2]


def test_main_with_no_task_counts_none_and_writes_no_json(monkeypatch, capsys, tmp_path):
    monkeypatch.setattr(baseline_vs_atlas, "_load_suite", lambda: SimpleNamespace(TASKS={}))
    monkeypatch.setattr(baseline_vs_atlas, "_llama_url", lambda: "http://llama")
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr(sys, "argv", ["baseline-vs-atlas.py", "--tasks", ",", "--arms", "oneshot"])

    rc = baseline_vs_atlas.main()

    assert rc == 0
    assert "oneshot    TOTAL             0/0  (0%)" in capsys.readouterr().out.splitlines()
    assert list(tmp_path.iterdir()) == []
