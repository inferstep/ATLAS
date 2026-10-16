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

sys.path.insert(0, str(Path(__file__).resolve().parents[2] / "v3-service"))

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


def test_a_real_verification_is_not_penalised():
    plan = {
        "steps": [
            {"id": "s1", "action": "write_file", "target": "solve.py", "why": "Write it."},
            {"id": "s2", "action": "run_command", "target": "python3 solve.py", "why": "Run it."},
        ],
        "verify_step": "s2",
    }
    out, _ = planning.normalize_plan(dict(plan))
    assert not out.get("verify_is_setup_only")


def test_a_custom_server_script_is_still_setup():
    """Observed live after the first fix: the planner routed around the
    command patterns by writing its own server.py, and `python3 server.py`
    reads as an ordinary script invocation. The intent is stated in `why`."""
    plan = {
        "steps": [
            {"id": "s1", "action": "write_file", "target": "index.html", "why": "Page."},
            {"id": "s2", "action": "write_file", "target": "server.py", "why": "A simple server."},
            {"id": "s3", "action": "run_command", "target": "python3 server.py",
             "why": "Start the server to verify the game loads in a browser."},
        ],
        "verify_step": "s3",
    }
    out, notes = planning.normalize_plan(dict(plan))
    assert out.get("verify_is_setup_only") is True, notes


def test_running_a_program_for_its_output_is_real_verification():
    plan = {
        "steps": [
            {"id": "s1", "action": "write_file", "target": "solve.py", "why": "Write it."},
            {"id": "s2", "action": "run_command", "target": "python3 solve.py",
             "why": "Run it and confirm it prints the answer."},
        ],
        "verify_step": "s2",
    }
    out, _ = planning.normalize_plan(dict(plan))
    assert not out.get("verify_is_setup_only")


def test_a_cli_app_named_app_py_is_not_mistaken_for_a_server():
    """Filenames say nothing. A CLI application named app.py is verified by
    running it, and matching the name alone would penalise the correct plan."""
    plan = {
        "steps": [
            {"id": "s1", "action": "write_file", "target": "app.py", "why": "The CLI tool."},
            {"id": "s2", "action": "run_command", "target": "python app.py",
             "why": "Run it and confirm it prints the report."},
        ],
        "verify_step": "s2",
    }
    out, notes = planning.normalize_plan(dict(plan))
    assert not out.get("verify_is_setup_only"), notes
    score, reasons = planning._score_plan(out, "build me a report tool")
    assert not any("setup, not verification" in r for r in reasons), reasons


def _plan_with_verify(target, why):
    return {
        "steps": [
            {"id": "s1", "action": "write_file", "target": "app.py", "why": "create the app"},
            {"id": "s2", "action": "run_command", "target": target, "why": why},
        ],
        "verify_step": "s2",
        "rationale": "r",
    }


def test_intent_phrased_noun_first_is_still_a_server_start():
    """Three winning plans in a row said "the X starts" and were scored as real
    verification while their "Start the X" siblings were flagged (acceptance
    runs, 2026-09-15). The words are the same intent in either order."""
    for target, why in [
        ("python3 app.py", "Verify the application starts without errors."),
        ("node server.js", "Verify the server starts successfully."),
        ("python3 -m pip install -r requirements.txt && python3 app.py",
         "Verify the application starts correctly."),
        ("node server.js", "Check that the service is running on port 3000."),
        ("python3 app.py", "Make sure the app comes up."),
    ]:
        out, notes = planning.normalize_plan(_plan_with_verify(target, why))
        assert out.get("verify_is_setup_only") is True, (target, why, notes)


def test_running_a_program_and_checking_its_output_is_not_a_start():
    """The negatives the old order already protected must survive the wider
    match: a script run for its output, and an app whose name says nothing."""
    for target, why in [
        ("python3 app.py", "Run the app and check that it prints the settlement table."),
        ("python3 solve.py", "Verify the output is 7."),
        ("pytest tests/", "Run the test suite for the application."),
        ("curl -X POST -d 'name=Ana' http://127.0.0.1:5000/add && curl http://127.0.0.1:5000/",
         "Add a person through the app and verify the page lists them."),
    ]:
        out, notes = planning.normalize_plan(_plan_with_verify(target, why))
        assert not out.get("verify_is_setup_only"), (target, why, notes)
