"""#259: a replacement keeps the role of the file it replaces.

Measured on the fc8321d smoke runs: for `test_stats.py`, V3's winner was a
module of median() implementations. It ran without error, so it passed; the
lens preferred it; the proxy delivered it over the model's test file. The
file's `test_mean` and `test_median` were gone and running it tested nothing.
Another winner renamed store.py's `FILE_PATH`, which the rest of the program
imports.

The role is the file's top-level names: every def, class and assignment it
binds, and every name it imports from another project file. A candidate that
drops one fails verification, on every path that can hand code back.
"""

import sys
from pathlib import Path
from types import SimpleNamespace

PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import adapters  # noqa: E402
import pipeline as P  # noqa: E402
import scoring  # noqa: E402
import symbols  # noqa: E402

TEST_FILE = (
    "from stats import mean, median\n"
    "\n"
    "\n"
    "def test_mean():\n"
    "    assert mean([1, 2, 3]) == 2.0\n"
    "\n"
    "\n"
    "def test_median():\n"
    "    assert median([1, 3, 2]) == 2\n"
    "\n"
    "\n"
    "if __name__ == \"__main__\":\n"
    "    test_mean()\n"
    "    test_median()\n"
    "    print(\"ok\")\n"
)

# The shape the smoke runs delivered: module code, no tests.
MODULE_CODE = (
    "import math\n"
    "\n"
    "\"\"\"Small statistics helpers.\"\"\"\n"
    "\n"
    "\n"
    "def mean(values):\n"
    "    return sum(values) / len(values)\n"
    "\n"
    "\n"
    "def median(values):\n"
    "    s = sorted(values)\n"
    "    return s[len(s) // 2]\n"
)

PROJECT = {"stats.py": "def mean(values):\n    return sum(values) / len(values)\n"}


# ---------------------------------------------------------------------------
# The comparison itself
# ---------------------------------------------------------------------------

def test_module_code_for_a_test_file_drops_the_tests_and_the_imports():
    dropped = symbols.dropped_top_level_names(TEST_FILE, MODULE_CODE,
                                              "test_stats.py", PROJECT)
    assert dropped == ["mean (imported from stats)",
                       "median (imported from stats)",
                       "test_mean", "test_median"]


def test_a_script_with_no_definitions_keeps_its_project_imports():
    """multiturn_stats: a test script is imports plus a main block."""
    script = ("from stats import mean, median\n\n"
              "if __name__ == '__main__':\n"
              "    print(mean([1, 2, 3]), median([1, 2, 3]))\n")
    assert symbols.dropped_top_level_names(script, MODULE_CODE, "test_stats.py",
                                           PROJECT) == [
        "mean (imported from stats)", "median (imported from stats)"]


def test_a_renamed_constant_is_dropped():
    store = 'import os\n\nFILE_PATH = "todos.json"\n\n\ndef load_todos():\n    pass\n'
    renamed = store.replace("FILE_PATH", "DEFAULT_FILE")
    assert symbols.dropped_top_level_names(store, renamed, "store.py") == ["FILE_PATH"]


def test_an_edit_that_keeps_every_name_drops_nothing():
    better = TEST_FILE.replace("== 2.0", "== 2") + "\n\ndef test_even():\n    pass\n"
    assert symbols.dropped_top_level_names(TEST_FILE, better, "test_stats.py",
                                           PROJECT) == []


def test_imports_from_outside_the_project_may_come_and_go():
    before = "import os\nimport sys\nfrom collections import Counter\n\ndef main():\n    pass\n"
    after = "def main():\n    pass\n"
    assert symbols.dropped_top_level_names(before, after, "solve.py", PROJECT) == []


def test_a_project_name_defined_locally_instead_is_dropped():
    """Same name, different binding: the file stopped using stats.py."""
    before = "from stats import mean\n\nprint(mean([1, 2]))\n"
    after = "def mean(v):\n    return sum(v) / len(v)\n\nprint(mean([1, 2]))\n"
    assert symbols.dropped_top_level_names(before, after, "check.py", PROJECT) == [
        "mean (imported from stats)"]


def test_project_module_names_drop_only_a_leading_dot_slash():
    """#277: `lstrip("./")` also ate the dot of a hidden directory."""
    names = symbols._project_module_names(["./pkg/util.py", "././pkg/helpers.py", ".hidden/x.py"])
    assert {"pkg.util", "util", "pkg.helpers", "helpers"} <= names
    assert "hidden.x" not in names


def test_project_modules_resolve_through_packages_and_relative_imports():
    before = ("import pkg.util\nfrom .helpers import tidy\n"
              "from pkg.util import parse as p\n")
    files = {"pkg/util.py": "", "pkg/helpers.py": ""}
    assert symbols.dropped_top_level_names(before, "", "pkg/cli.py", files) == [
        "p (imported from pkg.util)", "pkg (imported from pkg.util)",
        "tidy (imported from .helpers)"]


def test_names_bound_only_in_the_main_block_are_not_the_role():
    before = ("def main():\n    pass\n\n"
              "if __name__ == '__main__':\n    args = 1\n    main()\n")
    after = "def main():\n    args = 1\n\n\nmain()\n"
    assert symbols.dropped_top_level_names(before, after, "cli.py") == []


def test_subscript_and_attribute_targets_bind_no_name():
    before = "import config\nconfig.DEBUG = True\nTABLE = {}\nTABLE['a'] = 1\n"
    after = "TABLE = {'a': 1}\n"
    assert symbols.dropped_top_level_names(before, after, "settings.py") == []


def test_unparseable_sides_and_other_languages_report_nothing():
    assert symbols.dropped_top_level_names(TEST_FILE, "def broken(:\n",
                                           "test_stats.py", PROJECT) == []
    assert symbols.dropped_top_level_names("def broken(:\n", MODULE_CODE,
                                           "test_stats.py", PROJECT) == []
    assert symbols.dropped_top_level_names("function f() {}\n", "",
                                           "app.js", {}) == []


# ---------------------------------------------------------------------------
# The pipeline holds every candidate to it
# ---------------------------------------------------------------------------

class _LLM:
    """The probe re-types the test file with the indentation slip the smoke
    run recorded ("Line 5: unexpected indent")."""

    def __init__(self, progress_callback=None, thinking=False):
        pass

    def __call__(self, prompt, temperature, max_tokens, seed, thinking=None):
        slipped = TEST_FILE.replace("    assert mean", "     assert mean")
        return f"```python\n{slipped}```", 3, 1.0


class _Sandbox:
    """Runs everything that parses; the slipped copy does not."""

    def __init__(self, project_files=None):
        pass

    def __call__(self, code, test_input="", **_):
        if "     assert mean" in code:
            return False, "", "Line 5: unexpected indent"
        if P._CONSENSUS_MARK in code:
            return True, P._CONSENSUS_MARK + repr("2") + "\n", ""
        return True, "", ""


def _service(monkeypatch, plan_codes, repairs=()):
    monkeypatch.setenv("ATLAS_V3_TELEMETRY_DIR", "off")
    monkeypatch.setattr(adapters, "LLMAdapter", _LLM)
    monkeypatch.setattr(adapters, "SandboxAdapter", _Sandbox)
    monkeypatch.setattr(adapters, "EmbedAdapter", lambda: (lambda text: []))
    monkeypatch.setattr(scoring, "classify_task_type", lambda p: "algorithmic")
    monkeypatch.setattr(scoring, "score_candidate", lambda code: (1.0, 0.1, False))
    monkeypatch.setattr(scoring, "score_candidate_combined",
                        lambda code: dict(scoring.NEUTRAL_COMBINED))
    monkeypatch.setattr(scoring, "score_candidate_per_step",
                        lambda code: {"gx_score_min": 0.9, "gx_score_mean": 0.9,
                                      "cx_norm_max": 0.1, "first_off_rails_idx": -1,
                                      "n_tokens": 10, "thresholds": {"severe": 0.30}})
    monkeypatch.setattr(symbols, "structural_score",
                        lambda project_symbols, code: {"ok": False})
    service = P.V3PipelineService()
    service.self_test_gen = SimpleNamespace(
        generate=lambda problem, llm, task_id:
            (_ for _ in ()).throw(RuntimeError("unavailable")))
    service.plan_search = SimpleNamespace(
        generate=lambda problem, task_id, llm, num_plans=None, budget_tier="standard":
            SimpleNamespace(candidates=list(plan_codes), total_tokens=0))
    calls = []

    def _repair(problem, code, error, llm_call, task_id):
        calls.append(error)
        return SimpleNamespace(repairs=list(repairs), total_tokens=0)
    service.pr_cot = SimpleNamespace(repair=_repair)
    service.refinement_loop = SimpleNamespace(
        run=lambda **kw: SimpleNamespace(solved=False, total_tokens=0,
                                         total_iterations=1, winning_code=""))
    return service, calls


def _run(service):
    return service.run("In stats.py, add median(); then verify it works.",
                       task_id="gen-test_stats", files=dict(PROJECT),
                       file_path="test_stats.py", baseline_code=TEST_FILE)


def test_module_code_cannot_win_a_test_file(monkeypatch):
    """The #259 pool: the probe's copy of the test file fails to parse and
    every other candidate is module code that runs. None of them may win."""
    service, _ = _service(
        monkeypatch, [MODULE_CODE, MODULE_CODE.replace("s[len", "s[ len")])
    result = _run(service)

    # Nothing that keeps the file's role passed except the file itself, so
    # its exact bytes come back and the caller keeps them.
    assert result["code"] == TEST_FILE
    assert result["phase_solved"] == "incumbent"
    fails = [e for e in result["events"] if e["stage"] == "sandbox_fail"]
    role_fails = [e for e in fails if "role check" in (e["data"].get("stderr") or "")]
    assert len(role_fails) == 2, fails
    checks = [e for e in result["events"] if e["stage"] == "role_check"]
    assert checks and "test_mean" in checks[0]["data"]["dropped"]
    # The reason is the candidate's error, so repair would be told the names.
    assert "test_mean" in role_fails[0]["data"]["stderr"]


def test_a_candidate_that_keeps_the_names_can_still_win(monkeypatch):
    """The check narrows the pool; it does not freeze the file. A candidate
    that keeps every name and that the lens ranks above the file wins."""
    kept = TEST_FILE.replace('print("ok")', 'print("all tests passed")')
    service, _ = _service(monkeypatch, [MODULE_CODE, kept])
    monkeypatch.setattr(scoring, "score_candidate",
                        lambda code: (0.5 if code == kept else 1.0, 0.1, False))
    result = _run(service)
    assert result["passed"] is True
    assert result["code"] == kept


def test_a_repair_is_held_to_the_role_too(monkeypatch):
    """PR-CoT hands back code directly when it passes the sandbox; module
    code must not reach the caller through that door either."""
    service, _ = _service(monkeypatch, [MODULE_CODE], repairs=[MODULE_CODE + "\n"])
    result = _run(service)
    assert result["phase_solved"] != "pr_cot"
    assert (result["code"] or "") not in (MODULE_CODE, MODULE_CODE + "\n")


def test_candidates_that_failed_the_role_are_left_out_of_consensus():
    """Agreement among candidates does not outweigh a failed project check."""
    role_failed = {"verification_evidence": [
        {"verifier": "incumbent_role", "status": "failed", "dropped": ["test_mean"]}]}
    assert P._failed_a_project_check(role_failed) is True
    assert P._failed_a_project_check({"verification_evidence": [
        {"verifier": "incumbent_role", "status": "passed"}]}) is False


def test_without_a_submitted_file_there_is_no_role_to_keep(monkeypatch):
    """A bench task names no incumbent; nothing is checked against one."""
    service, _ = _service(monkeypatch, [MODULE_CODE])
    result = service.run("write median()", task_id="bench", files={},
                         file_path="stats_mod.py", baseline_code="")
    assert not [e for e in result["events"] if e["stage"] == "role_check"]
    assert result["code"] == MODULE_CODE
