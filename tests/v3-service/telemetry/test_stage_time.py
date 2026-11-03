"""Each V3 stage that reports `total_time_ms` reports the time it took, in milliseconds.

Each run reads a stand-in for the stage's clock: its first reading is 1000.0 s and every later reading is
2.5 s on, so a stage that starts, works and ends has taken 2500 ms."""

import pytest

from stages import plan_search, pr_cot, refinement_loop
from stages.failure_analysis import FailingCandidate
from stages.plan_search import PlanSearch, PlanSearchConfig, PlanSearchInfrastructureError
from stages.pr_cot import PRCoT, PRCoTConfig
from stages.refinement_loop import RefinementLoop, RefinementLoopConfig


class StandInClock:
    """The `time` module of one stage, for its time() only."""

    def __init__(self):
        self.readings = 0

    def time(self):
        self.readings += 1
        return 1000.0 if self.readings == 1 else 1002.5


class Refused(Exception):
    """A model call that the server refused: status 401."""
    code = 401


def plan_search_with_one_plan(*answers):
    """Plan search for one plan, with a model that gives the answers in turn (an exception is raised).
    A refused search gives the result that its error carries."""
    queue = list(answers)

    def model(prompt, temperature, max_tokens, seed):
        answer = queue.pop(0)
        if isinstance(answer, Exception):
            raise answer
        return answer, 10, 100.0

    try:
        return PlanSearch(PlanSearchConfig(enabled=True, num_plans=1)).generate("Add two numbers.", "t1", model)
    except PlanSearchInfrastructureError as refused:
        return refused.result


def a_model_that_says_nothing(prompt, temperature, max_tokens, seed):
    return "", 10, 100.0


def one_refinement_iteration():
    loop = RefinementLoop(RefinementLoopConfig(enabled=True, max_iterations=1))
    return loop.run(problem="Add two numbers.",
                    failing_candidates=[FailingCandidate(code="x", error_output="NameError", index=0)],
                    original_constraints=["c1"], llm_call=a_model_that_says_nothing,
                    sandbox_run=lambda code, test_input: (False, "", "NameError"))


def one_pr_cot_repair():
    return PRCoT(PRCoTConfig(enabled=True)).repair(problem="Add two numbers.", code="x", error="NameError",
                                                   llm_call=a_model_that_says_nothing)


CONSTRAINTS = "CONSTRAINT SET 1:\n- Constraint: Use one addition\n- Implies: arithmetic"
PLAN = "Algorithm choice: add the two numbers\n1. Return a + b"
CODE = "```python\ndef add(a, b):\n    return a + b\n```"


@pytest.mark.parametrize("stage, run", [
    pytest.param(plan_search, lambda: plan_search_with_one_plan(CONSTRAINTS, PLAN, CODE),
                 id="plan search, every call answered"),
    pytest.param(plan_search, lambda: plan_search_with_one_plan(Refused()),
                 id="plan search, its constraint call refused"),
    pytest.param(plan_search, lambda: plan_search_with_one_plan(CONSTRAINTS, Refused()),
                 id="plan search, its plan call refused"),
    pytest.param(refinement_loop, one_refinement_iteration, id="the refinement loop"),
    pytest.param(pr_cot, one_pr_cot_repair, id="PR-CoT"),
])
def test_a_stage_reports_the_time_it_took_in_milliseconds(monkeypatch, stage, run):
    monkeypatch.setattr(stage, "time", StandInClock())

    assert run().total_time_ms == 2500.0
