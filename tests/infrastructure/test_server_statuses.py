"""The script that writes the statuses of the development server and the two checks that read them agree: on the
names, on the account that writes, and on the text of a status.

Each of the three has its own tests. These hold what no one of them can see alone.
"""
import importlib.util
import json
import re
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]


def load(name):
    spec = importlib.util.spec_from_file_location(f"atlas_statuses_{name}", ROOT / "scripts" / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


nightly, smoke, canary = load("nightly_run"), load("smoke_result"), load("canary")
LIST = json.loads((ROOT / ".github" / "canary.json").read_text(encoding="utf-8"))["server"]
A_START = "2026-10-08T08:00:03Z"


def reports():
    """Reports of a night and of a smoke run, passed and failed, as the run writes them."""
    tasks = [{"task": task, "seconds": 10.5, "passed": True, "defects": 0} for task in nightly.TASKS]
    tests = {"expected": 32, "collected": 32, "passed": 32, "failed": 0, "skipped": 0, "skip_reasons": {}}
    night = {"started": A_START, "mode": "night", "commit": "1" * 40, "result": "passed", "step": "done", "tasks": tasks, "tests": tests}
    one = {**night, "mode": "one commit", "pushed_as": ["smoke/a-change"], "mark": "0123456789ab"}
    del one["tests"]
    for report in (night, one):
        yield report
        yield {**report, "result": "failed: a step did not end", "tasks": None, "tests": None, "step": "start the stack"}
        yield {**report, "result": "failed: x", "tasks": [{**task, "defects": 2} for task in tasks]}
        yield {**report, "result": "failed: x", "stack_stopped": "no: status 1"}
    yield {**night, "result": "failed: x", "tests": {**tests, "passed": 28, "failed": 3, "skipped": 1}}


def test_the_three_scripts_use_the_same_two_names():
    assert nightly.STATUS == {"night": LIST["status"], "one commit": smoke.STATUS}
    assert smoke.STATUS == "server/smoke"
    assert LIST["status"] == "server/nightly"


def test_the_two_checks_take_the_statuses_of_the_same_account():
    # The canary's list names no account (0) while the server has written no night; then the nights are not looked for.
    assert LIST["writer"] in (0, smoke.WRITER), (
        f"scripts/smoke_result.py takes the statuses of the account {smoke.WRITER} (WRITER), and .github/canary.json "
        f"names the account {LIST['writer']} (`server.writer`). Both are the account of the server's GitHub App. Fix: "
        "give both the same id, or 0 in the list while the nights are not looked for.")
    assert type(smoke.WRITER) is int
    assert smoke.WRITER > 0


def test_the_unit_of_the_servers_timer_names_the_same_account_and_the_app_by_its_client_id():
    unit = (ROOT / "scripts" / "server" / "atlas-nightly.service").read_text(encoding="utf-8")
    words = re.sub(r"\\\n\s*", "", unit).split()
    assert int(words[words.index("--status-writer") + 1]) == smoke.WRITER == 340202947
    assert words[words.index("--status-app") + 1] == "Iv23liQAJ0Ousufa05LE"
    # Both are public names of the app. The key of the app is in no file of the repository.
    for path in sorted((ROOT / "scripts").rglob("*")) + sorted((ROOT / ".github").rglob("*")):
        if path.is_file() and path.suffix in (".py", ".sh", ".service", ".timer", ".yml", ".json"):
            assert "PRIVATE KEY" not in path.read_text(encoding="utf-8", errors="replace"), path


def test_the_repository_and_the_address_are_the_same_for_the_writer_and_the_reader():
    assert (nightly.REPOSITORY, nightly.statuses.GITHUB_API) == (smoke.REPOSITORY, smoke.GITHUB_API) == ("inferstep/ATLAS", "https://api.github.com")


def test_the_smoke_check_prints_the_text_of_each_status_of_the_server_as_it_is():
    # The check prints a text of a status in its plain letters only. A text that the server writes has no other.
    texts = [status["description"] for status, _why in map(nightly.status_of, reports()) if status]
    assert len(texts) == 9
    for text in texts:
        assert smoke.plain(text) == f"`{text}`", text


def test_the_canary_check_reads_the_start_time_of_a_night_from_the_text_of_its_status():
    for report in reports():
        status, _why = nightly.status_of(report)
        assert canary.start_of(status) == A_START, status["description"]
    assert re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", A_START)


def test_the_script_of_the_run_asks_the_smoke_check_for_the_mark_in_the_way_that_it_gives_it():
    source = (ROOT / "scripts" / "nightly_run.py").read_text(encoding="utf-8")
    assert '[args.python, "scripts/smoke_result.py", "--mark", commit]' in source
    assert '"--mark"' in (ROOT / "scripts" / "smoke_result.py").read_text(encoding="utf-8")
    assert "scripts/smoke_result.py" in nightly.FROM_DEV
