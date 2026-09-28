"""The pipeline has to be told what the user asked for.

_build_problem_from_request produced "Create the file X", the project
context, and the baseline under an instruction to improve on it "preserving
all functionality". The requirement itself was never included, so a
candidate could only mimic a draft whose goal it had never seen, and a
baseline that misread the task was reproduced rather than corrected.

Measured on the AoC tasks, whose prompt says "reads input.txt": 9 of the 12
solutions ATLAS wrote read stdin instead, and the checker runs
`python solve.py` with no stdin, so they printed 0. The same model given the
task directly wrote file readers 12 times out of 12.
"""

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

from pipeline import _build_problem_from_request  # noqa: E402
from stages.llm_client import extract_code_for_problem  # noqa: E402

TASK = ("input.txt holds one integer per line: a sonar depth reading. Write "
        "solve.py that reads input.txt and prints how many window sums are "
        "larger than the previous window sum.")


def test_the_request_reaches_the_pipeline():
    problem = _build_problem_from_request(
        "solve.py", "import sys\n", {}, "", "", [], TASK)
    assert "reads input.txt" in problem
    assert "## The request" in problem


def test_the_request_leads_the_problem():
    """Ahead of the baseline, which is the thing it has to be able to override."""
    problem = _build_problem_from_request(
        "solve.py", "import sys\n", {}, "", "", [], TASK)
    assert problem.index(TASK[:40]) < problem.index("Create the file")


def test_no_request_keeps_the_previous_shape():
    """Callers that send nothing must not get a stray empty heading."""
    problem = _build_problem_from_request("a.py", "x = 1\n", {}, "", "", [])
    assert problem.startswith("Create the file `a.py`")
    assert "## The request" not in problem


def test_a_blank_request_is_treated_as_absent():
    problem = _build_problem_from_request("a.py", "x = 1\n", {}, "", "", [], "   ")
    assert "## The request" not in problem


def test_user_interface_outranks_model_authored_baseline_at_real_builder_boundary():
    request = (
        "Create solution.py. Implement exactly:\n"
        "def merge_intervals(intervals: list[tuple[int, int]], *, "
        "merge_touching: bool = True) -> list[tuple[int, int]]:"
    )
    baseline = (
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: List[Tuple[int, int]], *, "
        "merge_touching: bool = True) -> List[Tuple[int, int]]:\n"
        "    return intervals\n"
    )
    problem = _build_problem_from_request(
        "solution.py", baseline, {}, "", "", [], request,
    )

    assert extract_code_for_problem(baseline, problem) == (
        "from typing import List, Tuple\n\n"
        "def merge_intervals(intervals: list[tuple[int, int]], *, "
        "merge_touching: bool = True) -> list[tuple[int, int]]:\n"
        "    return intervals\n"
    )
