"""Contracts for V3 plan verification scoring."""

import sys
from pathlib import Path

import pytest


PROJECT_ROOT = Path(__file__).resolve().parents[2]
sys.path.insert(0, str(PROJECT_ROOT))
sys.path.insert(0, str(PROJECT_ROOT / "v3-service"))

import main as v3main


def _plan(command):
    return {
        "steps": [
            {"id": "s1", "action": "read_file", "target": "README.md"},
            {"id": "s2", "action": "run_command", "target": command},
        ],
        "verify_step": "s2",
        "rationale": "Inspect, then verify.",
    }


def test_plan_scorer_credits_running_the_program():
    for command in (
        "pytest -q",
        "python3 solve.py",
        "curl -sf http://localhost:8000/",
        "npm test",
        "go test ./...",
        "java App.java",
        "./run.sh",
        "pytest -q && ruff check .",
    ):
        _, reasons = v3main._score_plan(_plan(command), "fix solve.py")
        assert "verify_step references a real verification command" in reasons, command


def test_plan_scorer_does_not_credit_a_static_check_as_verification():
    # The agent loop does not count these as verification
    # (proxy/command_evidence.go), so a plan whose verify step is one of them
    # must not outrank a plan that runs the program.
    for command in (
        "markdownlint README.md",
        "shellcheck scripts/setup.sh",
        "golangci-lint run ./...",
        "python3 -m py_compile solve.py",
        "npm run build",
        "go build ./...",
        "node --check app.js",
    ):
        _, reasons = v3main._score_plan(_plan(command), "fix README.md")
        assert "verify_step references a real verification command" not in reasons, command
        assert any("well formed" in r for r in reasons), (command, reasons)
    lint, _ = v3main._score_plan(_plan("ruff check solve.py"), "fix solve.py")
    run, _ = v3main._score_plan(_plan("python3 solve.py"), "fix solve.py")
    assert run == pytest.approx(lint + 0.2)


def test_plan_scorer_does_not_treat_recon_as_verification():
    score, reasons = v3main._score_plan(
        _plan("grep -n typo README.md"), "fix README.md"
    )
    assert score == pytest.approx(0.8)
    assert "verify_step doesn't reference a verification command" in reasons


# --- planning to recreate what already exists ---------------------------------
#
# Measured on aoc_sonar: the winning plan's step 1 was `write_file input.txt`
# — "create the necessary input data" — against a 2000-line fixture already on
# disk. The model tried to retype it from memory, degenerated into repeating
# one line, had its stream cut mid-JSON, and the run died on three unparseable
# responses. aoc_course executed the same step successfully and corrupted the
# fixture. Both plans scored 1.00, because nothing looked at what was there.

_CLOBBER_PLAN = {
    "steps": [
        {"id": "s1", "action": "write_file", "target": "input.txt",
         "why": "create the necessary input data"},
        {"id": "s2", "action": "write_file", "target": "solve.py", "why": "the solution"},
        {"id": "s3", "action": "run_command", "target": "python solve.py", "why": "verify"},
    ],
    "verify_step": "s3",
    "rationale": "creates input, implements, verifies",
}


def test_a_plan_that_recreates_an_existing_file_scores_below_one_that_does_not():
    clean, _ = v3main._score_plan(_CLOBBER_PLAN, "solve the puzzle", frozenset())
    clobber, reasons = v3main._score_plan(
        _CLOBBER_PLAN, "solve the puzzle", {"input.txt"})
    assert clobber < clean, (clobber, clean)
    assert any("already exist" in r for r in reasons), reasons
    assert any("input.txt" in r for r in reasons), reasons


def test_creating_a_genuinely_new_file_is_not_penalised():
    _, reasons = v3main._score_plan(
        _CLOBBER_PLAN, "solve the puzzle", {"README.md", "notes.txt"})
    assert not any("already exist" in r for r in reasons), reasons


def test_editing_an_existing_file_is_not_penalised():
    """Only CREATE actions clobber. Editing what is there is the correct plan."""
    plan = {
        "steps": [
            {"id": "s1", "action": "edit_file", "target": "input.txt", "why": "fix a line"},
            {"id": "s2", "action": "run_command", "target": "pytest", "why": "verify"},
        ],
        "verify_step": "s2", "rationale": "edit then verify",
    }
    _, reasons = v3main._score_plan(plan, "fix input.txt", {"input.txt"})
    assert not any("already exist" in r for r in reasons), reasons


def test_the_existing_file_set_is_the_proxy_listing_and_the_context():
    found = v3main._known_files({"ctx_only.py": "y = 2"}, ["./input.txt", "sub/mod.py"])
    assert found == {"ctx_only.py", "input.txt", "sub/mod.py"}


# #277: the paths were cleaned with `lstrip("./")`, which strips a set of
# characters rather than a prefix, so the dot of a hidden name went too and
# `.env` was recorded as `env`.
def test_the_existing_file_set_keeps_the_dot_of_hidden_files():
    found = v3main._known_files({".github/x.yml": ""}, [".env", "./.env", "./a.py", "././a.py"])
    assert found == {".github/x.yml", ".env", "a.py"}


def _create_plan(target):
    return {
        "steps": [
            {"id": "s1", "action": "write_file", "target": target, "why": "create it"},
            {"id": "s2", "action": "run_command", "target": "pytest", "why": "verify"},
        ],
        "verify_step": "s2", "rationale": "create then verify",
    }


def test_creating_env_is_not_flagged_when_only_dot_env_exists():
    existing = v3main._known_files({}, [".env"])
    _, reasons = v3main._score_plan(_create_plan("env"), "write the env file", existing)
    assert not any("already exist" in r for r in reasons), reasons


def test_creating_dot_env_is_flagged_when_dot_env_exists():
    existing = v3main._known_files({}, [".env"])
    for target in (".env", "./.env"):
        _, reasons = v3main._score_plan(_create_plan(target), "write the env file", existing)
        assert any("already exist" in r and ".env" in r for r in reasons), (target, reasons)


def test_the_prompt_names_existing_files_so_no_candidate_proposes_creating_them():
    """Scoring a bad plan down only helps if some candidate is better. All
    three aoc_sonar candidates opened with `write_file input.txt` and scored
    1.00 apiece, so the penalty had nothing to prefer. Naming the files in the
    prompt stops them being proposed."""
    import planning
    prompt = planning._build_plan_prompt(
        "solve the sonar puzzle", "/workspace", {}, ["input.txt", "README.md"])
    assert "input.txt" in prompt
    assert "ALREADY EXIST" in prompt
    assert "Do not plan to create any of these" in prompt
    assert "READ it at runtime" in prompt


def test_the_prompt_is_unchanged_when_the_workspace_is_empty():
    import planning
    prompt = planning._build_plan_prompt("build me a site", "/workspace", {}, [])
    assert "ALREADY EXIST" not in prompt
