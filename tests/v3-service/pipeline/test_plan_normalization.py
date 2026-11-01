"""A plan must not manufacture an edit it could have avoided.

Audited on the literal prompt "Build me a snake game.", the planner emitted
`write_file index.html` ... `edit_file index.html "Link the CSS and JS
files"`. index.html is greenfield in that same plan and the paths are known
at planning time, so the links belong in the initial write. The split
creates an exact-span edit for a quantized model that measurably cannot do
them — one dogfood session died looping on that exact edit_file/index.html
pair. Its verify_step was `python3 -m http.server 8000`, which is setup and
cannot fail on an inert page.
"""

import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parents[3] / "v3-service"))

import planning

SNAKE_PLAN = {
    "steps": [
        {"id": "s1", "action": "write_file", "target": "index.html", "why": "Create the canvas page."},
        {"id": "s2", "action": "write_file", "target": "game.js", "why": "Game logic."},
        {"id": "s3", "action": "write_file", "target": "style.css", "why": "Styling."},
        {"id": "s4", "action": "edit_file", "target": "index.html", "why": "Link the CSS and JS files."},
        {"id": "s5", "action": "run_command", "target": "python3 -m http.server 8000", "why": "Serve it."},
    ],
    "verify_step": "s5",
}


def test_greenfield_write_then_edit_is_collapsed():
    plan, notes = planning.normalize_plan(dict(SNAKE_PLAN))
    actions = [(s["action"], s["target"]) for s in plan["steps"]]
    assert ("edit_file", "index.html") not in actions, actions
    assert len(plan["steps"]) == 4
    # The collapsed intent survives on the initial write.
    s1 = next(s for s in plan["steps"] if s["id"] == "s1")
    assert "Link the CSS and JS" in s1["why"]
    assert any("collapsed" in n for n in notes)


def test_server_start_only_verification_is_flagged_and_penalised():
    plan, notes = planning.normalize_plan(dict(SNAKE_PLAN))
    assert plan.get("verify_is_setup_only") is True
    assert any("setup" in n for n in notes)
    score, reasons = planning._score_plan(plan, "Build me a snake game.")
    assert any("setup, not verification" in r for r in reasons), reasons


def test_an_edit_to_a_preexisting_file_is_untouched():
    plan = {
        "steps": [
            {"id": "s1", "action": "read_file", "target": "app.py", "why": "Look."},
            {"id": "s2", "action": "edit_file", "target": "app.py", "why": "Fix the bug."},
            {"id": "s3", "action": "run_command", "target": "pytest tests/", "why": "Verify."},
        ],
        "verify_step": "s3",
    }
    out, notes = planning.normalize_plan(dict(plan))
    assert len(out["steps"]) == 3, "an edit to a file this plan did not create must stand"
    assert not notes


# Each plan writes app.py, then runs `command` as its verify step, saying why.
@pytest.mark.parametrize("command, why, setup_only", [
    # Observed live after the first fix: the planner routed around the command
    # patterns by writing its own server.py, and `python3 server.py` reads as
    # an ordinary script run. The intent is stated in `why`.
    pytest.param("python3 server.py", "Start the server to verify the game loads in a browser.",
                 True, id="start-the-server"),
    # Three winning plans in a row said "the X starts" and were scored as real
    # verification while their "Start the X" siblings were flagged (acceptance
    # runs, 2026-09-15). The words are the same intent in either order.
    pytest.param("python3 app.py", "Verify the application starts without errors.",
                 True, id="the-application-starts"),
    pytest.param("node server.js", "Verify the server starts successfully.",
                 True, id="the-server-starts"),
    pytest.param("python3 -m pip install -r requirements.txt && python3 app.py",
                 "Verify the application starts correctly.",
                 True, id="install-then-the-application-starts"),
    pytest.param("node server.js", "Check that the service is running on port 3000.",
                 True, id="the-service-is-running"),
    pytest.param("python3 app.py", "Make sure the app comes up.",
                 True, id="the-app-comes-up"),
    # A program run for its output is real verification, and a file name such
    # as app.py says nothing.
    pytest.param("python3 app.py", "Run the app and check that it prints the settlement table.",
                 False, id="app-run-for-its-output"),
    pytest.param("python3 solve.py", "Verify the output is 7.",
                 False, id="script-run-for-its-output"),
    pytest.param("pytest tests/", "Run the test suite for the application.",
                 False, id="test-suite"),
    pytest.param("curl -X POST -d 'name=Ana' http://127.0.0.1:5000/add && curl http://127.0.0.1:5000/",
                 "Add a person through the app and verify the page lists them.",
                 False, id="post-then-read-the-page"),
])
def test_a_verify_step_that_only_starts_the_app_is_setup(command, why, setup_only):
    plan = {
        "steps": [
            {"id": "s1", "action": "write_file", "target": "app.py", "why": "create the app"},
            {"id": "s2", "action": "run_command", "target": command, "why": why},
        ],
        "verify_step": "s2",
        "rationale": "r",
    }
    out, notes = planning.normalize_plan(plan)
    assert out.get("verify_is_setup_only", False) is setup_only, notes
    _, reasons = planning._score_plan(out, "build the app")
    assert any("setup, not verification" in r for r in reasons) is setup_only, reasons
