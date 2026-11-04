"""scripts/eval/reliability_report.py: the run summary report() prints.

The sessions are stand-ins that carry only the fields report() reads, so no
stack runs and no event stream is parsed.
"""
import importlib.util
from pathlib import Path
from types import SimpleNamespace

REPO = Path(__file__).resolve().parents[2]
_spec = importlib.util.spec_from_file_location(
    "reliability_report", REPO / "scripts" / "eval" / "reliability_report.py")
reliability_report = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(reliability_report)

TURN = {"type": "turn_start"}


class _Session(SimpleNamespace):
    def of_type(self, t):
        return [e for e in self.events if e.get("type") == t]


def test_report_prints_rates_defects_guards_stack_tasks_quality_and_tui_coverage(capsys):
    sessions = [
        _Session(task="alpha", rep=1, task_passed=False,
                 defects=["H6 service fault: sandbox answered 500", "H2 lost write: a.py"],
                 v3={"write_calls": 2, "generated": 1, "delivered": 1}, stack_changes=[],
                 quality={"max_complexity": 12, "max_complexity_where": "a.py:solve",
                          "max_function_lines": 40, "max_function_where": "a.py:solve",
                          "max_file_lines": 90, "lint_defects": 2, "lint_style": 5,
                          "unused_imports": 1, "syntax_errors": ["b.py"],
                          "defect_codes": ["F821"], "findings": ["complexity"]},
                 events=[TURN] * 5 + [{"type": "tool_call"}]),
        _Session(task="alpha", rep=2, task_passed=True, defects=[],
                 v3={"write_calls": 1, "generated": 1, "delivered": 0}, stack_changes=[],
                 quality={"error": "ruff missing"}, events=[TURN]),
        _Session(task="alpha", rep=3, task_passed=True, defects=[],
                 v3={"write_calls": 1, "generated": 1, "delivered": 0},
                 stack_changes=["atlas-proxy restarted", "sandbox OOM-killed"],
                 quality={"max_complexity": 4, "max_complexity_where": "b.py:main",
                          "max_function_lines": 60, "max_function_where": "b.py:main",
                          "max_file_lines": 70, "max_file_where": "b.py", "lint_defects": 1,
                          "lint_style": 2, "unused_imports": 0, "syntax_errors": None,
                          "defect_codes": ["E999", "F821"], "findings": []},
                 events=[TURN] * 3 + [{"type": "v3_plan"}]),
        _Session(task="beta", rep=1, task_passed=True, defects=["H6 service fault: proxy answered 502"],
                 v3={"write_calls": 1, "generated": 1, "delivered": 0}, stack_changes=[],
                 quality={"findings": []}, events=[TURN] * 2 + [{"type": "mystery"}, {"data": {}}]),
    ]
    guards = {("alpha", 1): ["repeat"], ("alpha", 3): ["repeat", "bad_json"]}

    reliability_report.report(sessions, {"turn_start", "tool_call"},
                              lambda s: guards.get((s.task, s.rep), []))

    assert capsys.readouterr().out.splitlines() == [
        "",
        "=" * 72,
        "Harness Integrity Rate   2/4 (50%)   <- ATLAS's own plumbing",
        "Task Success Rate        3/4 (75%)   <- task outcome (cause not classified)",
        "V3 generation            ran on 4/5 write calls, delivered 1 candidate(s)",
        "=" * 72,
        "",
        "Harness defects by class:",
        "    2  H6 service fault",
        "    1  H2 lost write",
        "Model-output guards (the proxy caught malformed model output; not harness defects): 3 in 2 session(s): bad_json 1, repeat 2",
        "Stack not stable in 1/4 session(s): the outcomes of these ran over a restart, an OOM kill or a missing container:",
        "  alpha rep 3: atlas-proxy restarted; sandbox OOM-killed",
        "",
        "Per task:",
        "  alpha          harness 2/3  task 2/3  turns min/med/max 1/3/5",
        "  beta           harness 0/1  task 1/1  turns min/med/max 2/2/2",
        "",
        "Code quality of what the agent wrote:",
        "  sessions with no quality finding   2/3",
        "  worst function complexity          12 (a.py:solve)",
        "  longest function                   60 lines (b.py:main)",
        "  longest file                       90 lines (n/a)",
        "  real lint defects                  3 ['E999', 'F821']",
        "  unused imports                     1",
        "  style nits (not scored)            7",
        "  files left unparseable             1",
        "",
        "TUI coverage: 4 event types emitted, 2 the TUI cannot render: ['mystery', 'v3_plan']",
    ]


def test_a_clean_run_without_v3_says_so_and_prints_no_quality(capsys):
    sessions = [_Session(task="alpha", rep=1, task_passed=False, defects=[],
                         v3={"write_calls": 1, "generated": 0, "delivered": 0}, stack_changes=[],
                         quality={}, events=[TURN])]

    reliability_report.report(sessions, {"turn_start"}, lambda s: [])

    assert capsys.readouterr().out.splitlines() == [
        "",
        "=" * 72,
        "Harness Integrity Rate   1/1 (100%)   <- ATLAS's own plumbing",
        "Task Success Rate        0/1 (0%)   <- task outcome (cause not classified)",
        "V3 generation            ran on 0/1 write calls, delivered 0 candidate(s)",
        "  ! no write reached V3 generation: this run measured the agent loop without V3",
        "=" * 72,
        "",
        "No harness defects detected.",
        "",
        "Per task:",
        "  alpha          harness 1/1  task 0/1  turns min/med/max 1/1/1",
        "",
        "TUI coverage: 1 event types emitted, 0 the TUI cannot render",
    ]


def test_a_run_with_no_write_call_has_no_v3_warning(capsys):
    sessions = [_Session(task="alpha", rep=1, task_passed=True, defects=[],
                         v3={"write_calls": 0, "generated": 0, "delivered": 0}, stack_changes=[],
                         quality={}, events=[TURN])]

    reliability_report.report(sessions, {"turn_start"}, lambda s: [])

    assert "no write reached V3 generation" not in capsys.readouterr().out
