"""The result of a run goes to GitHub as a status on the commit that ran, the key that writes it is read by the one
signing step only, and a look of the timer runs only what is due.

The stand-ins are the ones of the nightly tests: `openssl` signs with the key of the test, and a small server on this
machine answers as GitHub does. The script of the run has one fixed address. What a started run asks there is turned
to that server and written down. A look of the timer reads the clock, so the tests of what is due call that part in
their own process, with a time of their own. No test reaches the network. Every run has a time limit.
"""
import ast
import datetime as dt
import fcntl
import itertools
import json
import os
import re

import pytest

from tests.infrastructure import test_nightly_run as base
from tests.infrastructure import test_nightly_run_commit as commits
from tests.infrastructure.test_nightly_run import APP, HEAD, INSTALLATION, KEY, SCRIPT, TOKEN, WRITER, Night, a_stand_in_for_github, nightly

UTC = dt.timezone.utc
REPO = "/repos/inferstep/ATLAS"
STATUSES_OF = "GET " + REPO + "/commits/{}/statuses?per_page=100"
ACTIVITY_OF = "GET " + REPO + "/activity?ref=refs%2Fheads%2Fsmoke%2F{}&per_page=5"
PULLS_OF = "GET " + REPO + "/commits/{}/pulls?per_page=100"
A_START = "2026-10-08T08:00:03Z"
# The tests of a night that passed: each of the tests that need a real model.
EXPECTED = nightly.TESTS.expected
NIGHT = "passed; started {started}; 3 of 3 sessions with no harness defect; " + f"{EXPECTED} of {EXPECTED} tests passed"
# A time outside the hour of the night, and one inside it.
NOON, EIGHT = dt.datetime(2026, 10, 8, 12, 30, tzinfo=UTC), dt.datetime(2026, 10, 8, 8, 10, tzinfo=UTC)
COMMIT, OTHER = "c" * 40, "d" * 40
NO_KEY = "no key: the run was started without --status-key or without --status-app"
OPEN = "the key is not used: its file is open to the group or to others. Fix: chmod 600 on that file"
NOT_ON_THE_LIST = "the account that moved the branch to its tip is not on the list of this server"
NOT_AT_THE_TIP = "GitHub's record of the branch does not end at its tip"
NO_SETTINGS = "no smoke: the settings of the server name no writer of the statuses, or no person whose push counts"
NO_HOURS = "no smoke: the settings of the server name no hours in which a look may start a smoke run (--smoke-hours)"
NOT_THIS_HOUR = "no smoke in this hour: it is not one of the hours for smoke runs (--smoke-hours). A smoke branch waits"
NOT_OUR_OWN = ("its commit is not the head of an open pull request from a branch of this repository. The timer runs only "
               "such a commit by itself; another one is run by hand")
IN_USE = "nothing is started: the card is in use"


def three_calls(commit):
    """What one send asks GitHub: where the app is installed, a token for this one send, and the status itself."""
    return [f"GET {REPO}/installation", f"POST /app/installations/{INSTALLATION}/access_tokens", f"POST {REPO}/statuses/{commit}"]


def with_the_key(tmp_path, *more, env=None, plan=None, **answers):
    """One night on a server that has the key, with a GitHub that answers in this way. Gives the night and what GitHub saw."""
    with a_stand_in_for_github(**answers) as github:
        night = Night(tmp_path, github=github.port, **(plan or {}))
        night.run(*night.with_the_key(), *more, **(env or {}))
        return night, list(github.seen)


def far_hour():
    """An hour that is twelve hours from now: the night is not due in a test that gives it."""
    return str((dt.datetime.now(UTC).hour + 12) % 24)


def look(night, *more):
    """One look of the timer, at a time when the night is not due, on a server that may start a smoke run at any hour."""
    return night.run("--tick", "--night-hour", far_hour(), "--smoke-by", "a-maintainer", "--smoke-hours", "0-23",
                     *night.with_the_key(), *more)


def left_by(night):
    """All that a run left behind: its output, and each file under the folder of the test but the key itself."""
    texts = {"the output of the run": night.done.stdout + night.done.stderr}
    for path in sorted(night.root.rglob("*")):
        if path.is_file() and not path.is_symlink() and path != night.key:
            texts[str(path.relative_to(night.root))] = path.read_text(errors="replace")
    return texts


# --- what is sent -------------------------------------------------------------------------------------------------

def test_a_night_that_passed_sends_a_green_status_named_server_nightly_on_the_commit_that_ran(tmp_path):
    night, seen = with_the_key(tmp_path)
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    report = night.report
    status = {"context": "server/nightly", "sha": HEAD, "state": "success", "description": NIGHT.format(started=report["started"])}
    assert report["result"] == "passed"
    assert report["status"] == status
    assert report["sent"] == "sent: 1"
    assert [call["call"] for call in seen] == three_calls(HEAD)
    # The status has a name, a state and a text: no link.
    assert seen[2]["body"] == {"state": "success", "context": "server/nightly", "description": status["description"]}
    assert night.waits() == []
    assert night.waits("sent") == [status]
    assert night.done.stdout.splitlines()[-1] == "GitHub: sent: 1"
    assert re.fullmatch(r"\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ", report["started"])


def test_the_token_of_a_send_is_made_for_this_one_repository_and_for_statuses_only(tmp_path):
    night, seen = with_the_key(tmp_path)
    assert night.report["sent"] == "sent: 1"
    assert seen[1]["body"] == {"repositories": ["ATLAS"], "permissions": {"statuses": "write"}}
    # openssl gets the text to sign on its input, and the key as the path of its file.
    (sign,) = [call for call in night.calls() if call["tool"] == "openssl"]
    assert sign["args"] == ["dgst", "-sha256", "-sign", str(night.key)]
    # The proof that the key signed goes to the two calls that make the token, and the token only to the status.
    assert seen[0]["with"] == seen[1]["with"]
    assert seen[0]["with"].startswith("Bearer ey")
    assert seen[2]["with"] == f"Bearer {TOKEN}"


def test_the_key_the_proof_and_the_token_are_written_nowhere(tmp_path):
    night, seen = with_the_key(tmp_path)
    assert night.report["sent"] == "sent: 1"
    proof = seen[0]["with"].removeprefix("Bearer ")
    texts = left_by(night)
    assert len(texts) > 20
    assert {"calls.log", "asked.log", "nightly/run.env", "the output of the run"} <= set(texts)
    assert KEY in night.key.read_text()
    for where, text in texts.items():
        for secret in (KEY, TOKEN, proof, proof.rsplit(".", 1)[1]):
            assert secret not in text, where


def test_no_command_has_the_key_in_its_environment_and_only_openssl_is_given_its_path(tmp_path):
    night, _seen = with_the_key(tmp_path)
    calls = night.calls()
    assert {call["tool"] for call in calls} == {"git", "docker", "python-for-the-run", "nvidia-smi", "openssl"}
    assert len(calls) >= 26
    for call in calls:
        held = " ".join([*call["environment"], *call["environment"].values()])
        assert KEY not in held, call["tool"]
        assert TOKEN not in held, call["tool"]
        assert str(night.key) not in held, call["tool"]
        assert "status" not in held.lower(), call["tool"]
        if call["tool"] != "openssl":
            assert str(night.key) not in " ".join(call["args"]), call["args"]
    # The settings that the containers of the stack get hold neither the key nor where it lies.
    settings = (night.dir / "run.env").read_text() + (night.dir / "nightly.env").read_text()
    assert KEY not in settings
    assert str(night.key) not in settings
    assert str(night.key.parent) not in settings
    assert night.dir not in night.key.parents


def test_every_address_that_a_run_asks_is_githubs_own(tmp_path):
    night, seen = with_the_key(tmp_path)
    assert night.asked() == [(call["call"].split()[0], "https://api.github.com" + call["call"].split()[1]) for call in seen]
    assert len(night.asked()) == 3


def test_a_proxy_that_the_environment_names_is_not_used(tmp_path):
    proxy = "http://127.0.0.1:1"
    names = ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy")
    night, seen = with_the_key(tmp_path, env=dict.fromkeys(names, proxy))
    assert night.report["sent"] == "sent: 1"
    assert [call["call"] for call in seen] == three_calls(HEAD)


def test_a_night_that_failed_sends_a_red_status_and_keeps_its_exit_status(tmp_path):
    night, seen = with_the_key(tmp_path, plan={"driver_rows": base.rows(0, 2, 0)})
    assert night.done.returncode == 1
    assert night.report["result"] == "failed: offbyone: 2 defect(s) of the harness"
    said = f"failed; started {night.report['started']}; 2 of 3 sessions with no harness defect; {EXPECTED} of {EXPECTED} tests passed"
    assert seen[2]["body"] == {"state": "failure", "context": "server/nightly", "description": said}
    assert night.report["sent"] == "sent: 1"


@pytest.mark.parametrize("plan, result", [
    ({"card": ["4242, 9000"]}, "not run: the card was in use (1 process(es) hold 9000 MiB)"),
    ({"built_from": {"atlas-proxy": base.OLD}}, base.stale("atlas-proxy")),
])
def test_a_night_that_did_not_take_place_sends_nothing_and_nothing_waits(tmp_path, plan, result):
    night, seen = with_the_key(tmp_path, plan=plan)
    assert night.done.returncode == 0
    assert night.report["result"] == result
    assert night.report["status"] == "no status: the run did not take place"
    assert night.report["sent"] == "nothing waits"
    assert seen == []
    assert night.asked() == []
    assert night.waits() == []
    assert night.waits("sent") == []


def a_report(**changes):
    """The report of a night that passed, with these changes."""
    tasks = [{"task": task, "seconds": 10.5 + n, "passed": True, "defects": 0} for n, task in enumerate(nightly.TASKS)]
    tests = {"expected": 32, "collected": 32, "passed": 32, "failed": 0, "skipped": 0, "skip_reasons": {}}
    return {"started": A_START, "mode": "night", "commit": HEAD, "result": "passed", "step": "done", "tasks": tasks, "tests": tests,
            "stack_stopped": "yes", "not_whole_at_the_end": [], **changes}


def the_tests(**numbers):
    return {**a_report()["tests"], **numbers}


def tasks_with(*defects):
    return [{**task, "defects": count} for task, count in zip(a_report()["tasks"], defects)]


def a_smoke(**changes):
    """The report of a run for one commit that passed, with these changes."""
    report = a_report(**{"mode": "one commit", "commit": COMMIT, "pushed_as": ["smoke/a-change"], "mark": commits.MARK, **changes})
    del report["tests"]
    return report


NOT_GREEN = "no status: the report says passed, and its numbers are not those of a run that passed"
NO_COMMIT = "no status: the report does not say which commit ran"
NOT_RUN = "no status: the run did not take place"


@pytest.mark.parametrize("report, why", [
    (a_report(result="not run: the card was in use"), NOT_RUN),
    (a_report(result="not run: the images are not of the head of the branch yet (x)"), NOT_RUN),
    (a_report(result=""), NOT_RUN),
    (a_report(result="passed with one note"), NOT_RUN),
    (a_report(result="Passed"), NOT_RUN),
    # A report that says passed and whose numbers say something else is never green.
    (a_report(tests=the_tests(skipped=1, passed=124)), NOT_GREEN),
    (a_report(tests=the_tests(collected=124, passed=124)), NOT_GREEN),
    (a_report(tests=the_tests(collected=126, passed=126)), NOT_GREEN),
    (a_report(tests=the_tests(failed=1, passed=124)), NOT_GREEN),
    (a_report(tests=the_tests(failed=1)), NOT_GREEN),
    (a_report(tests=the_tests(skipped=1)), NOT_GREEN),
    (a_report(tests=the_tests(expected=0, collected=0, passed=0)), NOT_GREEN),
    # Numbers that do not add up are not the numbers of a run that passed.
    (a_report(tests=the_tests(passed=124)), NOT_GREEN),
    (a_report(tests=the_tests(passed=126)), NOT_GREEN),
    (a_report(tests=the_tests(passed=0)), NOT_GREEN),
    (a_report(tests=None), NOT_GREEN),
    (a_report(tests={}), NOT_GREEN),
    (a_report(tasks=a_report()["tasks"][:2]), NOT_GREEN),
    (a_report(tasks=[]), NOT_GREEN),
    (a_report(tasks=tasks_with(0, 1, 0)), NOT_GREEN),
    (a_report(tasks=list(reversed(a_report()["tasks"]))), NOT_GREEN),
    (a_smoke(tasks=tasks_with(0, 0, 1)), NOT_GREEN),
    # The commit that ran has to be a full id, and the kind of run one of the two.
    (a_report(commit=HEAD[:39]), NO_COMMIT),
    (a_report(commit=HEAD + "1"), NO_COMMIT),
    (a_report(commit="A" * 40), NO_COMMIT),
    (a_report(commit=""), NO_COMMIT),
    (a_report(commit=None), NO_COMMIT),
    (a_report(mode="another"), NO_COMMIT),
    (a_report(mode=None), NO_COMMIT),
    (a_smoke(pushed_as=[]), "no status: the run ended before the commit was found at the tip of a smoke branch"),
    (a_smoke(pushed_as=None, result="failed: the newest commit of the branch: status 128"),
     "no status: the run ended before the commit was found at the tip of a smoke branch"),
])
def test_a_run_that_did_not_take_place_or_whose_report_does_not_hold_gets_no_status(report, why):
    assert nightly.status_of(report) == (None, why)


SESSIONS = "3 of 3 sessions with no harness defect"


@pytest.mark.parametrize("report, state, said", [
    (a_report(), "success", f"passed; started {A_START}; {SESSIONS}; 32 of 32 tests passed"),
    (a_smoke(), "success", f"passed; started {A_START}; {SESSIONS}; text {commits.MARK}"),
    (a_report(result="failed: offbyone: 2 defect(s) of the harness", tasks=tasks_with(0, 2, 0)), "failure",
     f"failed; started {A_START}; 2 of 3 sessions with no harness defect; 32 of 32 tests passed"),
    (a_report(result="failed: 3 test(s) failed; 1 test(s) were skipped (1: no model)", tests=the_tests(passed=28, failed=3, skipped=1)),
     "failure", f"failed; started {A_START}; {SESSIONS}; tests: 28 passed, 3 failed, 1 skipped of 32"),
    (a_report(result="failed: 27 tests were collected, and there are 32", tests=the_tests(collected=27, passed=27)),
     "failure", f"failed; started {A_START}; {SESSIONS}; tests: 27 passed, 0 failed, 0 skipped of 32"),
    (a_report(result="failed: the tests left no result (status 2): x", tests=None, step="the tests that need a real model"),
     "failure", f"failed; started {A_START}; {SESSIONS}; tests: not run"),
    (a_report(result="failed: the start of the stack: status 1: x", tasks=None, tests=None, step="start the stack"),
     "failure", f"failed; started {A_START}; ended in the step: start the stack"),
    (a_report(result="failed: x", tasks=a_report()["tasks"][:1], tests=None, step="the smoke run"),
     "failure", f"failed; started {A_START}; ended in the step: the smoke run"),
    (a_report(result="failed: x", tasks=None, tests=None, step="/home/someone/a step that is not on the list"),
     "failure", f"failed; started {A_START}; ended in the step: not named"),
    # The numbers are those of a run that passed, so the status says in fixed words what failed.
    (a_report(result="failed: the stack was not stopped (status 1: x); before that: passed", stack_stopped="no: status 1: x"),
     "failure", f"failed; started {A_START}; {SESSIONS}; 32 of 32 tests passed; the stack was not stopped"),
    (a_report(result="failed: at the end of the run the proxy says that v3-service is not ready",
              not_whole_at_the_end=["the proxy says that v3-service is not ready"]),
     "failure", f"failed; started {A_START}; {SESSIONS}; 32 of 32 tests passed; a service was not whole at the end"),
    (a_report(result="failed: the time limit of 30 minutes was reached, in the step: ask the services again", step="ask the services again"),
     "failure", f"failed; started {A_START}; {SESSIONS}; 32 of 32 tests passed; see the report on the server"),
    (a_smoke(result="failed: add_function: 1 defect(s) of the harness", tasks=tasks_with(1, 0, 0)), "failure",
     f"failed; started {A_START}; 2 of 3 sessions with no harness defect; text {commits.MARK}"),
    (a_smoke(result="failed: x", mark="not a mark"), "failure", f"failed; started {A_START}; {SESSIONS}; text not marked; see the report on the server"),
])
def test_the_status_of_a_run_is_green_for_passed_red_for_failed_and_says_its_numbers(report, state, said):
    status, why = nightly.status_of(report)
    assert why == ""
    assert status == {"context": nightly.STATUS[report["mode"]], "sha": report["commit"], "state": state, "description": said}


def test_the_two_names_are_fixed_and_start_with_server():
    assert nightly.STATUS == {"night": "server/nightly", "one commit": "server/smoke"}
    for name in nightly.STATUS.values():
        assert name.startswith("server/")


# What a tool, a test or the model could have written into a report.
NOT_FIXED = "/home/someone/atlas-nightly host-1.example.invalid FAILED tests/test_x.py::test_y <b>hello</b> `@someone` #12 https://example.invalid/x"
FORM = re.compile(r"(passed|failed); started \d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ(; [a-z0-9][a-z0-9 ,:']*)+")


def every_report():
    """Reports of each shape that gets a status, with the largest numbers and with text that is not fixed words."""
    numbers = [the_tests(), the_tests(passed=999, failed=999, skipped=999, expected=999, collected=999), None]
    ends = [{}, {"stack_stopped": f"no: {NOT_FIXED}"}, {"not_whole_at_the_end": [NOT_FIXED]}]
    for result, tests, defects, end in itertools.product(["passed", f"failed: {NOT_FIXED}"], numbers, [(0, 0, 0), (9, 9, 9)], ends):
        yield a_report(result=result, tests=tests, tasks=tasks_with(*defects), **end)
        yield a_smoke(result=result, tasks=tasks_with(*defects), **end)
        yield a_smoke(result=result, tasks=tasks_with(*defects), mark=NOT_FIXED, **end)
    for step in [*nightly.STEPS, NOT_FIXED]:
        yield a_report(result=f"failed: {NOT_FIXED}", tasks=None, tests=None, step=step)
        yield a_smoke(result=f"failed: {NOT_FIXED}", tasks=[], step=step)


def test_the_text_of_every_status_has_fixed_words_and_numbers_only_and_fits_the_limit_of_github():
    texts = [status["description"] for status, _why in map(nightly.status_of, every_report()) if status]
    assert len(texts) > 60
    for text in texts:
        assert FORM.fullmatch(text), text
        assert len(text) <= 140, text
        assert f"started {A_START}; " in text
        for word in NOT_FIXED.split():
            assert word not in text, text
    assert max(len(text) for text in texts) > 120, "the longest text is not among the reports of this test any more"


def test_a_status_has_no_link_and_no_part_that_the_report_could_fill():
    for report in every_report():
        status, _why = nightly.status_of(report)
        if status:
            assert set(status) == {"context", "sha", "state", "description"}
            assert status["sha"] == report["commit"]
            assert re.fullmatch(r"[0-9a-f]{40}", status["sha"])
            assert status["state"] == ("success" if report["result"] == "passed" else "failure")


def source():
    return SCRIPT.read_text(encoding="utf-8")


def test_a_status_names_a_step_only_from_a_list_and_the_list_holds_the_steps_of_the_script():
    named = {node.args[1].value for node in ast.walk(ast.parse(source())) if isinstance(node, ast.Call) and ast.unparse(node.func) == "now_in"}
    assert len(named) >= 15
    assert named | {"start"} == set(nightly.STEPS)
    assert len(set(nightly.STEPS)) == len(nightly.STEPS)
    for step in nightly.STEPS:
        assert re.fullmatch(r"[a-z][a-z' ]+", step), step


# --- the one address ----------------------------------------------------------------------------------------------

def test_the_script_has_one_address_of_github_and_no_way_to_give_it_another():
    tree = ast.parse(source())
    texts = [node.value for node in ast.walk(tree) if isinstance(node, ast.Constant) and isinstance(node.value, str)]
    assert [text for text in texts if "api.github" in text] == ["https://api.github.com"]
    assert nightly.GITHUB_API == "https://api.github.com"
    # Each call is made in one place, from that one name, and the name gets its value once.
    (request,) = [node for node in ast.walk(tree) if isinstance(node, ast.Call) and ast.unparse(node.func) == "urllib.request.Request"]
    assert ast.unparse(request.args[0]) == "f'{GITHUB_API}{path}'"
    assert len([node for node in ast.walk(tree) if isinstance(node, ast.Name) and node.id == "GITHUB_API" and isinstance(node.ctx, ast.Store)]) == 1
    assert "urlopen" not in source()
    # The environment is read by name in one place only: what a run gives to the copy that it starts in its place.
    assert re.findall(r"os\.environ\.\w+|os\.environ\[|getenv", source()) == ["os.environ.pop"]
    assert "os.environ.pop(STARTED_AGAIN" in source()


@pytest.fixture
def server(tmp_path, monkeypatch):
    """A server in the process of the test, and GitHub: the stand-ins are on the path, and the one address is the
    stand-in's. Gives the folder of the run and the stand-in of GitHub."""
    with a_stand_in_for_github() as github:
        night = Night(tmp_path)
        monkeypatch.setenv("PATH", str(tmp_path / "bin"))
        monkeypatch.setattr(nightly, "GITHUB_API", f"http://127.0.0.1:{github.port}")
        yield night, github


def settings(night, *more):
    """The settings of a look on a server that has the key, one person whose push counts, and every hour for a smoke run."""
    return nightly.parse(["--dir", str(night.dir), "--lock", str(night.lock), "--tick", "--smoke-by", "a-maintainer",
                          "--smoke-hours", "0-23", *night.with_the_key(), *more])


def to_send(commit=HEAD, **changes):
    return {"context": "server/nightly", "sha": commit, "state": "success", "description": NIGHT.format(started=A_START), **changes}


def now():
    return dt.datetime.now(UTC)


@pytest.mark.parametrize("moved", [f"GET {REPO}/installation", "POST /app/installations", f"POST {REPO}/statuses"])
def test_an_answer_that_points_to_another_place_is_not_followed(server, moved):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send(), "20261008T080003Z")
    github.plan["moved"] = moved
    assert nightly.send_waiting(args, now()) == "not sent: GitHub answered 302; waits: 1"
    assert github.seen[-1]["call"].startswith(moved)
    assert [call["call"] for call in github.seen if "elsewhere" in call["call"]] == []
    assert night.waits() == [to_send()]


def test_a_started_run_follows_no_answer_to_another_place_either(tmp_path):
    night, seen = with_the_key(tmp_path, moved=f"POST {REPO}/statuses")
    assert night.report["sent"] == "not sent: GitHub answered 302; waits: 1"
    assert [call["call"] for call in seen] == three_calls(HEAD)
    assert [address for _method, address in night.asked() if not address.startswith("https://api.github.com/")] == []
    assert night.done.returncode == 0
    assert night.report["result"] == "passed"


# --- the key ------------------------------------------------------------------------------------------------------

@pytest.mark.parametrize("mode, used", [(0o600, True), (0o400, True), (0o700, True), (0o640, False), (0o604, False), (0o620, False),
                                        (0o602, False), (0o610, False), (0o601, False), (0o644, False), (0o666, False)])
def test_only_a_key_file_that_nobody_but_its_owner_can_reach_is_used(tmp_path, mode, used):
    night = Night(tmp_path)
    night.key.chmod(mode)
    assert nightly.key_fault(settings(night)) == ("" if used else OPEN)


def test_a_key_file_that_is_open_to_the_group_is_not_used_and_the_run_goes_on(tmp_path):
    with a_stand_in_for_github() as github:
        night = Night(tmp_path, github=github.port)
        night.key.chmod(0o640)
        night.run(*night.with_the_key())
        assert github.seen == []
    assert night.done.returncode == 0
    assert night.report["result"] == "passed"
    assert night.report["sent"] == f"not sent: {OPEN}; waits: 1"
    assert "sign" not in night.did()
    assert night.asked() == []
    assert night.waits() == [night.report["status"]]
    assert str(night.key) not in night.done.stdout + night.done.stderr + json.dumps(night.report)


def test_a_key_that_lies_in_the_folder_of_the_run_is_not_used(tmp_path):
    night = Night(tmp_path)
    inside = "the key is not used: its file lies in the folder of the run, which the containers of the stack can read. Fix: move it out of that folder"
    (night.dir / "secrets").mkdir()
    for place in (night.dir / "status-app.pem", night.dir / "secrets" / "status-app.pem"):
        place.write_text(KEY)
        place.chmod(0o600)
        assert nightly.key_fault(settings(night, "--status-key", str(place))) == inside
    # A name outside the folder that leads into it is the same file.
    (tmp_path / "a name outside").symlink_to(night.dir / "status-app.pem")
    assert nightly.key_fault(settings(night, "--status-key", str(tmp_path / "a name outside"))) == inside
    assert nightly.key_fault(settings(night)) == ""


def test_a_key_that_is_not_there_or_is_no_file_of_the_runs_user_is_not_used(tmp_path, monkeypatch):
    night = Night(tmp_path)
    assert nightly.key_fault(settings(night, "--status-key", str(tmp_path / "not there.pem"))) == "no key: the file that --status-key names is not there"
    assert nightly.key_fault(settings(night, "--status-key", str(night.key.parent))) == "the key is not used: it is not a file of the user of this run"
    assert nightly.key_fault(settings(night, "--status-app", "")) == NO_KEY
    assert nightly.key_fault(nightly.parse(["--dir", str(night.dir), "--status-app", APP])) == NO_KEY
    assert nightly.key_fault(settings(night)) == ""
    monkeypatch.setattr(nightly.os, "getuid", lambda: night.key.stat().st_uid + 1)
    assert nightly.key_fault(settings(night)) == "the key is not used: it is not a file of the user of this run"


@pytest.mark.parametrize("plan, said", [
    ({"fail": ["openssl dgst"]}, "not sent: openssl could not sign with the key (status 1); waits: 1"),
    ({"sleep": {"openssl dgst": 0}, "openssl_is_gone": True}, "not sent: openssl could not be started, or did not end; waits: 1"),
])
def test_when_openssl_cannot_sign_the_status_waits_and_nothing_that_it_printed_is_kept(tmp_path, plan, said):
    with a_stand_in_for_github() as github:
        night = Night(tmp_path, github=github.port, **plan)
        if plan.get("openssl_is_gone"):
            (tmp_path / "bin" / "openssl").unlink()
        night.run(*night.with_the_key())
        assert github.seen == []
    assert night.done.returncode == 0
    assert night.report["result"] == "passed"
    assert night.report["sent"] == said
    assert "a planted failure" not in json.dumps(night.report) + night.done.stdout
    assert len(night.waits()) == 1


# --- when a status cannot go out ----------------------------------------------------------------------------------

def test_a_status_that_could_not_go_out_waits_and_the_next_look_sends_it_once(tmp_path):
    night = Night(tmp_path)
    night.run(*night.with_the_key())
    # No stand-in was at GitHub's address: the run is as it is with GitHub off.
    assert night.done.returncode == 0
    assert night.report["result"] == "passed"
    assert night.report["sent"] == "not sent: GitHub could not be reached; waits: 1"
    assert night.asked() == [("GET", f"https://api.github.com{REPO}/installation")]
    (waiting,) = night.waits()
    assert waiting == night.report["status"]
    with a_stand_in_for_github() as github:
        look(night.github_at(github.port))
        assert night.done.returncode == 0
        assert night.done.stdout.splitlines() == ["tick: statuses: sent: 1", "tick: nothing is due"]
        assert [call["call"] for call in github.seen] == three_calls(HEAD)
        assert github.seen[2]["body"] == {key: waiting[key] for key in ("state", "context", "description")}
        look(night)
        assert night.done.stdout.splitlines() == ["tick: statuses: nothing waits", "tick: nothing is due"]
        assert len(github.seen) == 3
    assert night.waits() == []
    assert night.waits("sent") == [waiting]
    # A look with nothing to run leaves no report.
    assert len(list(night.dir.glob("reports/*"))) == 1


@pytest.mark.parametrize("refused, status", [(f"GET {REPO}/installation", 401), ("POST /app/installations", 403),
                                             ("POST /app/installations", 404), (f"POST {REPO}/statuses", 422), (f"POST {REPO}/statuses", 500)])
def test_when_github_refuses_the_key_or_the_status_the_run_keeps_its_result_and_the_status_waits(tmp_path, refused, status):
    night, seen = with_the_key(tmp_path, refuse={refused: status})
    assert night.done.returncode == 0
    assert night.report["result"] == "passed"
    assert night.report["sent"] == f"not sent: GitHub answered {status}; waits: 1"
    assert seen[-1]["call"].startswith(refused)
    assert night.waits() == [night.report["status"]]
    assert night.done.stdout.splitlines()[0] == "nightly run: passed"


def test_a_failed_night_has_the_same_exit_status_with_github_off(tmp_path):
    night = Night(tmp_path, driver_rows=base.rows(0, 2, 0))
    night.run(*night.with_the_key())
    assert night.done.returncode == 1
    assert night.report["result"] == "failed: offbyone: 2 defect(s) of the harness"
    assert night.report["sent"] == "not sent: GitHub could not be reached; waits: 1"
    assert night.waits()[0]["state"] == "failure"


def test_a_fault_of_the_sending_step_itself_does_not_stop_the_report_or_change_the_result(tmp_path):
    night = Night(tmp_path)
    (night.dir / "to-send").write_text("a file where the folder of the waiting statuses would be\n")
    night.run(*night.with_the_key())
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    assert night.report["result"] == "passed"
    assert night.report["status"]["state"] == "success"
    assert night.report["sent"] == "not sent: an error of the sending step itself (FileExistsError)"


def test_a_fault_of_the_sending_step_does_not_stop_a_look_of_the_timer(tmp_path):
    night = Night(tmp_path)
    (night.dir / "to-send").mkdir()
    (night.dir / "to-send" / "20261008T080003Z-server-nightly.json").write_text("a file that this script did not write")
    (night.dir / "not-sent").write_text("a file where the folder of the statuses that are set aside would be\n")
    look(night)
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    assert night.done.stdout.splitlines() == ["tick: statuses: not sent: an error of the sending step itself (FileExistsError)",
                                              "tick: nothing is due"]


OPEN_FOLDER = ("not sent: the folder of the waiting statuses is not a folder that only the user of this run can write. "
               "Fix: look at what lies in it; then chmod go-w on that folder; the status of a run is then in its report only")


@pytest.mark.parametrize("mode", [0o775, 0o757, 0o777, 0o722])
def test_nothing_is_sent_from_a_waiting_folder_that_others_can_write_into(server, mode):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send(), "20261008T080003Z")
    (night.dir / "to-send").chmod(mode)
    assert nightly.send_waiting(args, now()) == OPEN_FOLDER
    assert github.seen == []
    # Nothing of such a folder is moved either.
    assert night.waits() == [to_send()]
    assert not (night.dir / "sent").exists()
    assert not (night.dir / "not-sent").exists()
    (night.dir / "to-send").chmod(0o700)
    assert nightly.send_waiting(args, now()) == "sent: 1"


def test_a_waiting_folder_that_is_a_name_for_another_folder_is_not_used(server, tmp_path):
    night, github = server
    args = settings(night)
    (tmp_path / "elsewhere").mkdir(mode=0o700)
    (tmp_path / "elsewhere" / "20261008T080003Z-server-nightly.json").write_text(json.dumps(to_send()))
    (night.dir / "to-send").symlink_to(tmp_path / "elsewhere")
    assert nightly.send_waiting(args, now()) == OPEN_FOLDER
    assert github.seen == []
    # And the status of a run is not written into it.
    nightly.keep_for_sending(args, to_send("2" * 40), "20261009T080003Z")
    assert len(list((tmp_path / "elsewhere").iterdir())) == 1


def test_a_waiting_file_that_is_not_the_runs_own_is_set_aside_and_not_sent(server, tmp_path):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send("0" * 40), "20261008T080003Z")
    nightly.keep_for_sending(args, to_send("1" * 40), "20261009T080003Z")
    (night.dir / "to-send" / "20261008T080003Z-server-nightly.json").chmod(0o666)
    # A name in the folder that leads to a file elsewhere.
    (tmp_path / "elsewhere.json").write_text(json.dumps(to_send("2" * 40)))
    (night.dir / "to-send" / "20261010T080003Z-server-nightly.json").symlink_to(tmp_path / "elsewhere.json")
    assert nightly.send_waiting(args, now()) == "sent: 1; set aside after 14 days or as not written by this script: 2"
    assert [call["call"] for call in github.seen] == three_calls("1" * 40)


def test_the_folders_of_the_statuses_are_made_for_the_user_of_the_run_alone(server):
    night, github = server
    args = settings(night)
    before = os.umask(0o002)
    try:
        nightly.keep_for_sending(args, to_send(), "20261008T080003Z")
        assert nightly.send_waiting(args, now()) == "sent: 1"
        (night.dir / "to-send" / "x.json").write_text("this is not a status")
        assert nightly.send_waiting(args, now()) == "set aside after 14 days or as not written by this script: 1"
    finally:
        os.umask(before)
    for folder in ("to-send", "sent", "not-sent"):
        assert (night.dir / folder).stat().st_mode & 0o777 == 0o700, folder
    # And the file of a status too: a server that gives the group the right to write a new file changes nothing.
    (kept,) = (night.dir / "sent").iterdir()
    assert kept.stat().st_mode & 0o777 == 0o600


def test_a_run_whose_waiting_folder_others_can_write_into_keeps_its_result_and_says_where_its_status_is(tmp_path):
    with a_stand_in_for_github() as github:
        night = Night(tmp_path, github=github.port)
        (night.dir / "to-send").mkdir(mode=0o777)
        (night.dir / "to-send").chmod(0o777)
        night.run(*night.with_the_key())
        assert github.seen == []
    assert night.done.returncode == 0
    assert night.report["result"] == "passed"
    assert night.report["status"]["state"] == "success"
    assert night.report["sent"] == OPEN_FOLDER
    assert night.waits() == []


def test_when_github_refuses_one_status_the_others_are_not_tried_in_that_look_and_go_out_later_oldest_first(server):
    night, github = server
    args = settings(night)
    for n in range(3):
        nightly.keep_for_sending(args, to_send(str(n) * 40), f"2026100{n + 1}T080003Z")
    github.plan["refuse"] = {"/statuses/": 500}
    assert nightly.send_waiting(args, now()) == "not sent: GitHub answered 500; waits: 3"
    assert [call["call"] for call in github.seen] == three_calls("0" * 40)
    github.plan["refuse"] = {}
    github.seen.clear()
    assert nightly.send_waiting(args, now()) == "sent: 3"
    # A token of its own for each send.
    assert [call["call"] for call in github.seen] == three_calls("0" * 40) + three_calls("1" * 40) + three_calls("2" * 40)
    assert nightly.send_waiting(args, now()) == "nothing waits"
    assert len(github.seen) == 9
    assert [status["sha"][0] for status in night.waits("sent")] == ["0", "1", "2"]


def test_a_status_that_has_waited_for_fourteen_days_is_set_aside_and_tried_no_more(server):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send(), "20261008T080003Z")
    github.plan["refuse"] = {"/installation": 401}
    assert nightly.send_waiting(args, now() + dt.timedelta(days=13, hours=23)) == "not sent: GitHub answered 401; waits: 1"
    assert nightly.send_waiting(args, now() + dt.timedelta(days=14, hours=1)) == "set aside after 14 days or as not written by this script: 1"
    assert len(github.seen) == 1
    assert night.waits() == []
    assert night.waits("sent") == []
    assert night.waits("not-sent") == [to_send()]
    assert nightly.send_waiting(args, now()) == "nothing waits"


@pytest.mark.parametrize("held", [
    "this is not JSON", "[1, 2]", "null", json.dumps(to_send(context="ci/required")), json.dumps(to_send(context="server/other")),
    json.dumps(to_send(state="pending")), json.dumps(to_send(state="error")), json.dumps(to_send("../../issues/1")),
    json.dumps(to_send(HEAD[:39])), json.dumps(to_send("A" * 40)), json.dumps(to_send(description="x" * 141)),
    json.dumps(to_send(description=5)), json.dumps({key: value for key, value in to_send().items() if key != "state"}),
])
def test_a_waiting_file_that_this_script_did_not_write_is_set_aside_and_never_sent(server, held):
    night, github = server
    args = settings(night)
    (night.dir / "to-send").mkdir()
    (night.dir / "to-send" / "20261008T080003Z-server-nightly.json").write_text(held)
    assert nightly.send_waiting(args, now()) == "set aside after 14 days or as not written by this script: 1"
    assert github.seen == []
    assert list((night.dir / "to-send").iterdir()) == []
    assert [path.read_text() for path in (night.dir / "not-sent").iterdir()] == [held]


def test_only_the_name_the_state_and_the_text_of_a_waiting_status_are_sent(server):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send(target_url="https://example.invalid/a-page", more="x"), "20261008T080003Z")
    assert nightly.send_waiting(args, now()) == "sent: 1"
    assert github.seen[2]["call"] == f"POST {REPO}/statuses/{HEAD}"
    assert github.seen[2]["body"] == {"state": "success", "context": "server/nightly", "description": NIGHT.format(started=A_START)}


@pytest.mark.parametrize("answers", [{"installation": {"id": "seventy-seven"}}, {"installation": [INSTALLATION]}, {"installation": {}},
                                     {"installation": None}, {"made": {}}, {"made": {"token": 5}}, {"made": {"token": ""}}, {"made": [TOKEN]}])
def test_an_answer_of_another_shape_makes_no_token_and_the_status_waits(server, answers):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send(), "20261008T080003Z")
    github.plan.update(answers)
    assert nightly.send_waiting(args, now()) == "not sent: GitHub gave an answer of another shape; waits: 1"
    assert [call["call"] for call in github.seen if "/statuses/" in call["call"]] == []


@pytest.mark.parametrize("written", [[], "a text", None, {"creator": None}, {"creator": {"id": "424242"}}, {"creator": {"id": WRITER}}])
def test_a_status_that_github_took_is_sent_whatever_the_answer_looks_like(server, written):
    night, github = server
    args = settings(night)
    nightly.keep_for_sending(args, to_send(), "20261008T080003Z")
    github.plan["written"] = written
    assert nightly.send_waiting(args, now()) == "sent: 1"
    assert nightly.send_waiting(args, now()) == "nothing waits"


@pytest.mark.parametrize("given, names", [(["--status-writer", "555"], "555"), (["--status-writer", "0"], "none")])
def test_a_send_says_which_account_wrote_the_status_when_the_settings_name_another(server, given, names):
    night, github = server
    nightly.keep_for_sending(settings(night), to_send(), "20261008T080003Z")
    assert nightly.send_waiting(settings(night, *given), now()) == (
        f"sent: 1; the statuses of this key are written by the account {WRITER}, and --status-writer names {names}. "
        f"Fix: give --status-writer {WRITER}")


# --- what a look of the timer finds due ---------------------------------------------------------------------------

def planned(night, **more):
    """Give the stand-ins of this folder more of a plan."""
    (night.root / "plan.json").write_text(json.dumps({**night.plan, **more}))


def tip(name, commit=COMMIT):
    """A line of `git ls-remote`: the tip of a smoke branch."""
    return f"{commit}\trefs/heads/smoke/{name}"


def by(login="a-maintainer", commit=COMMIT, kind="User"):
    """GitHub's record of a branch, newest first: this account moved the branch to this commit."""
    return [{"id": 2, "before": "0" * 40, "after": commit, "activity_type": "branch_creation", "actor": {"login": login, "id": 5, "type": kind}}]


def a_status(writer=WRITER, name="server/smoke", state="success"):
    return {"context": name, "state": state, "description": "", "creator": {"id": writer, "type": "Bot"}}


def left(night, started, **said):
    """A report that an earlier run of the folder left."""
    folder = night.dir / "reports" / started.replace("-", "").replace(":", "")
    folder.mkdir(parents=True)
    (folder / "report.json").write_text(json.dumps({"started": started, **said}))


def a_pull(commit=COMMIT, state="open", repo="inferstep/ATLAS", ref="feat/a-change"):
    """A pull request as GitHub names it for a commit: by default an open one from a branch of this repository."""
    return {"number": 7, "state": state, "head": {"sha": commit, "ref": ref, "repo": {"full_name": repo}}}


def a_smoke_is_there(night, github, record=None, pulls=None):
    """A smoke branch that a listed person pushed, whose tip is the head of an open pull request of our own."""
    planned(night, smoke_tips=[tip("a-change")])
    github.plan["activity"] = {"refs/heads/smoke/a-change": by() if record is None else record}
    github.plan["pulls"] = {COMMIT: [a_pull()] if pulls is None else pulls}


def at(hour, minute=10, day=8):
    return dt.datetime(2026, 10, day, hour, minute, tzinfo=UTC)


def test_in_its_hour_the_night_is_due_and_only_once_a_day(server):
    night, _github = server
    args = settings(night)
    assert nightly.what_is_due(args, at(8, 0)) == ("night", None, "the night is due")
    assert nightly.what_is_due(args, at(8, 59))[0] == "night"
    for hour in (0, 7, 9, 20):
        assert nightly.what_is_due(args, at(hour)) == (None, None, "nothing is due")
    # A night of the day before and a smoke run of today change nothing.
    left(night, "2026-10-07T08:00:03Z", mode="night", result="passed")
    left(night, "2026-10-08T03:00:03Z", mode="one commit", commit=OTHER, result="passed")
    assert nightly.what_is_due(args, at(8))[0] == "night"
    # A night that was started today is the night of today, also when it did not take place.
    left(night, "2026-10-08T08:00:03Z", mode="night", result="not run: the card was in use")
    assert nightly.what_is_due(args, at(8, 30)) == (None, None, "nothing is due")
    assert nightly.what_is_due(args, at(8, day=9))[0] == "night"
    assert nightly.what_is_due(settings(night, "--night-hour", "23"), at(23, 59, day=9))[0] == "night"
    assert nightly.what_is_due(settings(night, "--night-hour", "23"), at(8, day=9))[0] is None


def test_a_look_that_finds_the_lock_of_the_card_held_starts_nothing_and_asks_nothing(server):
    night, github = server
    a_smoke_is_there(night, github)
    with open(night.lock, "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
        assert nightly.what_is_due(settings(night), at(8)) == (None, None, IN_USE)
        assert nightly.what_is_due(settings(night), NOON) == (None, None, IN_USE)
    assert github.seen == []
    assert "git ls-remote" not in night.did()
    # A held lock is a run of another kind that says so: nothing is written down, and the next look asks again.
    assert not (night.dir / nightly.CARD_HELD).exists()
    # The look left no report, so the night of this day is still due at the next look.
    assert nightly.what_is_due(settings(night), at(8, 15))[0] == "night"
    assert nightly.what_is_due(settings(night), NOON)[0] == "one commit"


def test_after_a_look_that_found_a_process_on_the_card_with_no_lock_the_looks_of_an_hour_start_nothing(server):
    night, github = server
    a_smoke_is_there(night, github)
    planned(night, smoke_tips=[tip("a-change")], card=["4242, 9000"])
    assert nightly.what_is_due(settings(night), at(8, 0)) == (None, None, IN_USE)
    assert (night.dir / nightly.CARD_HELD).read_text() == "2026-10-08T08:00:00Z\n"
    # The card is free again. The looks of the hour still start nothing, and ask neither the card nor GitHub: on a
    # server that stops its usual stack for a run, that stack is then not stopped and started at each look.
    planned(night, smoke_tips=[tip("a-change")])
    asked = len(night.calls())
    quiet = (None, None, "nothing is started: at 2026-10-08T08:00:00Z a process held the card with no lock, and the looks "
                         "of the 60 minutes after that start nothing")
    for minute in (0, 15, 30, 45, 59):
        assert nightly.what_is_due(settings(night), at(8, minute)) == quiet
        assert nightly.what_is_due(settings(night, "--look"), at(8, minute)) == quiet
    assert len(night.calls()) == asked
    assert github.seen == []
    # After the hour the look asks again.
    assert nightly.what_is_due(settings(night, "--look"), at(9, 0))[0] == "one commit"
    assert nightly.what_is_due(settings(night), at(9, 0))[0] == "one commit"
    assert nightly.QUIET_MINUTES == 60


def test_a_look_that_starts_nothing_writes_no_time_down_and_a_time_that_cannot_be_read_holds_nothing(server):
    night, github = server
    a_smoke_is_there(night, github)
    planned(night, smoke_tips=[tip("a-change")], card=["4242, 9000"])
    assert nightly.what_is_due(settings(night, "--look"), NOON)[0] == "one commit"
    assert not (night.dir / nightly.CARD_HELD).exists()
    planned(night, smoke_tips=[tip("a-change")])
    for held in ("not a time\n", "", "2026-10-08T13:00:00Z\n", "2026-10-08T11:29:59Z\n"):
        # No time, a time after this look, and a time of more than an hour ago.
        (night.dir / nightly.CARD_HELD).write_text(held)
        assert nightly.what_is_due(settings(night), NOON)[0] == "one commit", held


def test_the_tip_of_a_smoke_branch_is_due_when_a_listed_person_moved_the_branch_there_and_the_app_wrote_no_status(server):
    night, github = server
    a_smoke_is_there(night, github)
    assert nightly.what_is_due(settings(night), NOON) == ("one commit", COMMIT, "the tip of `smoke/a-change` has no result yet")
    assert [call["call"] for call in github.seen] == [STATUSES_OF.format(COMMIT), ACTIVITY_OF.format("a-change"), PULLS_OF.format(COMMIT)]
    # The looks need no key, and send none.
    assert [call["with"] for call in github.seen] == ["", "", ""]
    assert "sign" not in night.did()
    # The tips are read from the address of the repository itself, by the name of the branches.
    (asked,) = [call["args"] for call in night.calls() if call["tool"] == "git"]
    assert asked == ["ls-remote", "--heads", "https://github.com/inferstep/ATLAS.git", "refs/heads/smoke/*"]
    # In its hour the night comes first.
    assert nightly.what_is_due(settings(night), EIGHT)[0] == "night"
    # The name of a person is the same name in other letters.
    github.plan["activity"] = {"refs/heads/smoke/a-change": by("A-Maintainer")}
    assert nightly.what_is_due(settings(night), NOON)[0] == "one commit"
    assert nightly.what_is_due(settings(night, "--smoke-by", "another-maintainer"), NOON)[0] == "one commit"


@pytest.mark.parametrize("record, why", [
    (by("somebody-else"), NOT_ON_THE_LIST),
    (by("a-maintainer", kind="Bot"), NOT_ON_THE_LIST),
    (by("a-maintainer", kind="Organization"), NOT_ON_THE_LIST),
    (by("a-maintainer[bot]", kind="Bot"), NOT_ON_THE_LIST),
    (by("a-maintainer-2"), NOT_ON_THE_LIST),
    (by(""), NOT_ON_THE_LIST),
    ([{"after": COMMIT}], NOT_ON_THE_LIST),
    ([{"after": COMMIT, "actor": None}], NOT_ON_THE_LIST),
    # The newest move decides: a listed person pushed the branch, and another account moved it after that.
    (by("somebody-else") + by(), NOT_ON_THE_LIST),
    (by(commit=OTHER), NOT_AT_THE_TIP),
    (by(commit=OTHER) + by(), NOT_AT_THE_TIP),
    (by(commit=COMMIT[:39]), NOT_AT_THE_TIP),
    ([], NOT_AT_THE_TIP),
    (None, NOT_AT_THE_TIP),
])
def test_a_tip_that_no_listed_person_moved_the_branch_to_is_not_run_and_one_line_says_so(server, capsys, record, why):
    night, github = server
    planned(night, smoke_tips=[tip("a-change")])
    github.plan["activity"] = {"refs/heads/smoke/a-change": record}
    assert nightly.what_is_due(settings(night), NOON) == (None, None, "nothing is due")
    assert capsys.readouterr().out == f"tick: `smoke/a-change` is not run: {why}\n"
    assert [call["call"] for call in github.seen] == [STATUSES_OF.format(COMMIT), ACTIVITY_OF.format("a-change")]


@pytest.mark.parametrize("pulls", [
    [],
    None,
    # The head of a pull request from another repository, pushed to a smoke branch here by a listed person.
    [a_pull(repo="somebody-else/ATLAS")],
    [a_pull(repo="inferstep/atlas-other")],
    [a_pull(repo="Inferstep/atlas")],
    [{"number": 7, "state": "open", "head": {"sha": COMMIT, "ref": "feat/a-change", "repo": None}}],
    [{"number": 7, "state": "open", "head": {"sha": COMMIT, "ref": "feat/a-change"}}],
    # A pull request that is closed or merged.
    [a_pull(state="closed")],
    [a_pull(state="merged")],
    # A pull request whose head is another commit by now: this one is further down its branch.
    [a_pull(commit=OTHER)],
    # A pull request from a smoke branch: a person can push another's commit there.
    [a_pull(ref="smoke/a-change")],
    [a_pull(ref="smoke/another")],
    [a_pull(repo="somebody-else/ATLAS"), a_pull(state="closed"), a_pull(commit=OTHER)],
])
def test_a_tip_that_is_not_the_head_of_an_open_pull_request_of_our_own_is_not_run_by_the_timer(server, capsys, pulls):
    night, github = server
    a_smoke_is_there(night, github, pulls=pulls or [])
    if pulls is None:
        github.plan["pulls"] = {}
    assert nightly.what_is_due(settings(night), NOON) == (None, None, "nothing is due")
    assert capsys.readouterr().out == f"tick: `smoke/a-change` is not run: {NOT_OUR_OWN}\n"
    assert [call["call"] for call in github.seen] == [STATUSES_OF.format(COMMIT), ACTIVITY_OF.format("a-change"), PULLS_OF.format(COMMIT)]


def test_one_open_pull_request_of_our_own_among_others_is_enough(server):
    night, github = server
    a_smoke_is_there(night, github, pulls=[a_pull(state="closed"), a_pull(repo="somebody-else/ATLAS"), a_pull()])
    assert nightly.what_is_due(settings(night), NOON)[0] == "one commit"


def test_the_pull_requests_are_asked_only_for_a_tip_that_a_listed_person_pushed(server, capsys):
    night, github = server
    a_smoke_is_there(night, github, record=by("somebody-else"))
    assert nightly.what_is_due(settings(night), NOON) == (None, None, "nothing is due")
    assert [call["call"] for call in github.seen] == [STATUSES_OF.format(COMMIT), ACTIVITY_OF.format("a-change")]
    assert capsys.readouterr().out == f"tick: `smoke/a-change` is not run: {NOT_ON_THE_LIST}\n"


@pytest.mark.parametrize("given, found", [("0-23", range(24)), ("22-23,0-5", [0, 1, 2, 3, 4, 5, 22, 23]), ("7", [7]), ("7,9", [7, 9]),
                                          ("3-3", [3]), ("0", [0]), ("23", [23]), ("1-2, 4-5", [1, 2, 4, 5])])
def test_the_hours_for_smoke_runs_are_single_hours_and_spans(given, found):
    assert nightly.parse(["--dir", "/not-used", "--smoke-hours", given]).smoke_hours == frozenset(found)


@pytest.mark.parametrize("given", ["24", "5-3", "a", "", "1-", "-1", "1--2", "1,,2", "1-24", "1.5", "0x10", "1 - 3", "every"])
def test_hours_in_another_form_are_refused_with_the_form_to_give(tmp_path, given):
    night = Night(tmp_path).run("--tick", f"--smoke-hours={given}")
    assert night.done.returncode == 2
    assert "give hours from 0 to 23, in UTC, each alone or as a span: for example 22-23,0-5" in night.done.stderr
    assert night.calls() == []


def test_with_no_hours_for_smoke_runs_the_timer_starts_none_and_outside_them_a_branch_waits(server, capsys):
    night, github = server
    a_smoke_is_there(night, github)
    with_no_hours = nightly.parse(["--dir", str(night.dir), "--lock", str(night.lock), "--tick", "--smoke-by", "a-maintainer",
                                   *night.with_the_key()])
    assert with_no_hours.smoke_hours == frozenset()
    assert nightly.what_is_due(with_no_hours, NOON) == (None, None, NO_HOURS)
    # The night has its own hour: it does not wait for the hours of the smoke runs.
    assert nightly.what_is_due(with_no_hours, EIGHT)[0] == "night"
    at_night = settings(night, "--smoke-hours", "1-5,22-23")
    for hour in (0, 6, 12, 21):
        assert nightly.what_is_due(at_night, at(hour)) == (None, None, NOT_THIS_HOUR)
    # Outside the hours neither the branches nor GitHub are asked.
    assert github.seen == []
    assert "git ls-remote" not in night.did()
    for hour in (1, 5, 22, 23):
        assert nightly.what_is_due(at_night, at(hour))[0] == "one commit"
    assert capsys.readouterr().out == ""


def test_a_tip_that_is_not_taken_does_not_stand_in_the_way_of_the_next_one(server, capsys):
    night, github = server
    planned(night, smoke_tips=[tip("a-change"), tip("b-change", OTHER)])
    github.plan["activity"] = {"refs/heads/smoke/a-change": by("somebody-else"), "refs/heads/smoke/b-change": by(commit=OTHER)}
    github.plan["pulls"] = {OTHER: [a_pull(OTHER)]}
    assert nightly.what_is_due(settings(night), NOON) == ("one commit", OTHER, "the tip of `smoke/b-change` has no result yet")
    assert capsys.readouterr().out == f"tick: `smoke/a-change` is not run: {NOT_ON_THE_LIST}\n"


@pytest.mark.parametrize("have, due", [
    ([a_status()], False),
    ([a_status(state="failure")], False),
    ([a_status(writer=999), a_status()], False),
    # A status of that name by anybody else changes nothing, and neither does another status of the app.
    ([a_status(writer=999)], True),
    ([a_status(writer=999, state="failure")], True),
    ([a_status(writer=str(WRITER))], True),
    ([a_status(name="server/nightly")], True),
    ([a_status(name="server/smoke-2")], True),
    ([{"context": "server/smoke", "state": "success", "creator": None}], True),
    ([{"context": "server/smoke", "state": "success"}], True),
    ([], True),
])
def test_a_tip_has_a_result_already_only_when_the_app_itself_wrote_a_status_of_that_name(server, have, due):
    night, github = server
    a_smoke_is_there(night, github)
    github.statuses[COMMIT] = have
    assert (nightly.what_is_due(settings(night), NOON)[0] == "one commit") is due
    assert [call["call"] for call in github.seen][:1] == [STATUSES_OF.format(COMMIT)]
    assert len(github.seen) == (3 if due else 1)


A_STATUS, NONE = {"context": "server/smoke", "sha": COMMIT, "state": "success", "description": "x"}, "no status: the run did not take place"


@pytest.mark.parametrize("earlier, again", [
    # A status was made: it was sent, or it waits.
    ({"result": "passed", "status": A_STATUS}, False),
    ({"result": "failed: offbyone: 2 defect(s) of the harness", "status": {**A_STATUS, "state": "failure"}}, False),
    # The reason lies in the commit, and a second run would find the same.
    ({"result": f"not run: the commit {COMMIT[:12]} does not stand on the head of `dev` (x)", "status": NONE}, False),
    ({"result": "not run: the commit changes `scripts/nightly_run.py`: the script of this run", "status": NONE}, False),
    # The reason lies in the server, or the run broke before it knew the commit.
    ({"result": "not run: the card was in use", "status": NONE}, True),
    ({"result": "not run: the card was in use (1 process(es) hold 9000 MiB)", "status": NONE}, True),
    ({"result": "not run: the images are not of the head of the branch yet (x)", "status": NONE}, True),
    ({"result": "failed: the newest commit of the branch: status 128: x", "status": "no status: the run ended before x"}, True),
    ({"result": "failed: taking the head changed the script of the run"}, True),
    ({}, True),
])
def test_a_commit_is_run_again_only_when_its_earlier_run_settled_nothing(server, earlier, again):
    night, github = server
    a_smoke_is_there(night, github)
    left(night, "2026-10-07T10:00:00Z", mode="one commit", commit=COMMIT, **earlier)
    assert (nightly.what_is_due(settings(night), NOON)[0] == "one commit") is again
    # What the folder of the run knows is not asked of GitHub again.
    assert bool(github.seen) is again


def test_the_run_of_another_commit_or_of_a_night_settles_nothing_for_this_commit(server):
    night, github = server
    a_smoke_is_there(night, github)
    left(night, "2026-10-07T10:00:00Z", mode="one commit", commit=OTHER, result="passed", status={**A_STATUS, "sha": OTHER})
    left(night, "2026-10-07T08:00:00Z", mode="night", commit=COMMIT, result="passed", status={**A_STATUS, "context": "server/nightly"})
    assert nightly.what_is_due(settings(night), NOON)[0] == "one commit"


def test_the_bound_of_the_day_stops_a_further_smoke_run_and_a_new_day_starts_at_none(server, capsys):
    night, github = server
    a_smoke_is_there(night, github)
    for hour in (1, 2):
        left(night, f"2026-10-08T0{hour}:00:00Z", mode="one commit", commit=OTHER, result="not run: the card was in use")
    left(night, "2026-10-08T03:00:00Z", mode="night", commit=HEAD, result="passed")
    over = (None, None, "no smoke: 2 were started today, which is the bound of this server")
    assert nightly.what_is_due(settings(night, "--smokes-a-day", "2"), NOON) == over
    assert nightly.what_is_due(settings(night, "--smokes-a-day", "1"), NOON)[0] is None
    assert nightly.what_is_due(settings(night, "--smokes-a-day", "3"), NOON)[0] == "one commit"
    assert nightly.what_is_due(settings(night, "--smokes-a-day", "2"), NOON + dt.timedelta(days=1))[0] == "one commit"
    assert nightly.parse(["--dir", "/not-used"]).smokes_a_day == 6


def test_only_a_branch_with_one_plain_name_after_smoke_is_looked_at(server):
    night, _github = server
    lines = [tip("ok-1.2_x"), tip("a" * 64, OTHER), tip("a/b"), tip("-a"), tip(".a"), tip("_a"), tip("a" * 65), tip("a b"), tip("a;b"), tip(""),
             f"{COMMIT}\trefs/heads/dev", f"{COMMIT}\trefs/heads/smoker/x", f"{COMMIT}\trefs/heads/x/smoke/x", f"{COMMIT}\trefs/tags/smoke/x",
             f"{COMMIT[:39]}\trefs/heads/smoke/short", f"{'C' * 40}\trefs/heads/smoke/capitals", f"{COMMIT} refs/heads/smoke/a-space",
             f"{COMMIT}\trefs/heads/smoke/x\tmore", "", "warning: something that git said"]
    planned(night, smoke_tips=lines)
    assert nightly.smoke_tips() == [("smoke/" + "a" * 64, OTHER), ("smoke/ok-1.2_x", COMMIT)]


@pytest.mark.parametrize("left_out", ["--status-writer", "--smoke-by"])
def test_without_a_writer_or_a_list_of_persons_no_smoke_branch_is_looked_for(server, left_out):
    night, github = server
    a_smoke_is_there(night, github)
    given = {"--status-key": str(night.key), "--status-app": APP, "--status-writer": str(WRITER), "--smoke-by": "a-maintainer"}
    args = nightly.parse(["--dir", str(night.dir), "--lock", str(night.lock), "--tick",
                          *[word for name, value in given.items() if name != left_out for word in (name, value)]])
    assert nightly.what_is_due(args, NOON) == (None, None, NO_SETTINGS)
    assert github.seen == []
    assert night.did() == ["card"]
    assert nightly.what_is_due(args, EIGHT)[0] == "night"


@pytest.mark.parametrize("refuse, plan, why", [
    ({"/pulls": 500}, {}, "no smoke: the repository could not be read (GitHub answered 500)"),
    ({"/activity": 500}, {}, "no smoke: the repository could not be read (GitHub answered 500)"),
    ({"/activity": 403}, {}, "no smoke: the repository could not be read (GitHub answered 403)"),
    ({"/statuses": 404}, {}, "no smoke: the repository could not be read (GitHub answered 404)"),
    ({}, {"fail": ["git ls-remote"]}, "no smoke: the repository could not be read (the smoke branches of the repository: status 1: "
                                      "a planted failure of: git ls-remote)"),
])
def test_when_the_repository_cannot_be_read_no_smoke_is_started(server, refuse, plan, why):
    night, github = server
    a_smoke_is_there(night, github)
    planned(night, smoke_tips=[tip("a-change")], **plan)
    github.plan["refuse"] = refuse
    assert nightly.what_is_due(settings(night), NOON) == (None, None, why)


@pytest.mark.parametrize("statuses, record, pulls", [
    ({"message": "x"}, by(), None), (["a text"], by(), None), (5, by(), None), ([], {"message": "x"}, None), ([], ["a text"], None),
    ([], [5], None), ([], [{"after": COMMIT, "actor": "a-maintainer"}], None),
    ([], by(), {"message": "x"}), ([], by(), ["a text"]), ([], by(), [{"state": "open", "head": "a text"}]), ([], by(), 5)])
def test_an_answer_of_another_shape_starts_no_smoke(server, statuses, record, pulls):
    night, github = server
    a_smoke_is_there(night, github, record)
    if pulls is not None:
        github.plan["pulls"] = {COMMIT: pulls}
    github.statuses[COMMIT] = statuses
    assert nightly.what_is_due(settings(night), NOON) == (None, None, "no smoke: GitHub gave an answer of another shape")


def test_tick_and_commit_are_not_given_together(tmp_path):
    night = Night(tmp_path).run("--tick", "--commit", COMMIT)
    assert night.done.returncode == 2
    assert "--tick chooses what to run by itself: give --tick or --commit, not both" in night.done.stderr
    assert night.calls() == []
    assert night.asked() == []


# --- a look that runs the night -----------------------------------------------------------------------------------

def test_a_look_in_the_hour_of_the_night_runs_the_night_once_and_sends_its_status(server, monkeypatch, capsys):
    night, github = server
    argv = night.command("--tick", "--smoke-by", "a-maintainer", "--smoke-hours", "0-23", *night.with_the_key())[2:]
    monkeypatch.setattr(nightly, "start_time", lambda: (at(8, 15), True, None, ""))
    assert nightly.main(argv) == 0
    out = capsys.readouterr().out.splitlines()
    assert out[:3] == ["tick: statuses: nothing waits", "tick: the night is due", "nightly run: passed"]
    assert out[-1] == "GitHub: sent: 1"
    report = night.report
    assert report["mode"] == "night"
    assert report["result"] == "passed"
    assert report["started"] == "2026-10-08T08:15:00Z"
    assert [call["call"] for call in github.seen] == three_calls(HEAD)
    assert github.seen[2]["body"]["description"] == NIGHT.format(started="2026-10-08T08:15:00Z")
    # The next look of that hour starts no second night, and leaves no report.
    monkeypatch.setattr(nightly, "start_time", lambda: (at(8, 30), True, None, ""))
    assert nightly.main(argv) == 0
    assert capsys.readouterr().out.splitlines() == ["tick: statuses: nothing waits", "tick: nothing is due"]
    assert len(list(night.dir.glob("reports/*"))) == 1
    assert len(github.seen) == 3


# --- a look that runs a smoke branch ------------------------------------------------------------------------------

@pytest.fixture
def repository(tmp_path):
    return commits.Repository(tmp_path)


def ours(commit, name="a-change"):
    """What GitHub says of a smoke branch that the timer runs: a listed person moved it to the commit, and the
    commit is the head of an open pull request from a branch of this repository."""
    return {"activity": {f"refs/heads/smoke/{name}": by(commit=commit)}, "pulls": {commit: [a_pull(commit)]}}


def asked_for(commit, name="a-change"):
    """The three things that a look asks GitHub about a tip before it runs it. None of them needs a key."""
    return [STATUSES_OF.format(commit), ACTIVITY_OF.format(name), PULLS_OF.format(commit)]


class Look(commits.OneCommit):
    """A folder for the looks of a timer on a server that has the key. git is the real one, and the address of the
    repository leads to the repository of the test."""

    def __init__(self, root, repository, github, **plan):
        super().__init__(root, repository, github=github.port, **plan)
        (root / "gitconfig").write_text(f'[url "{repository.bare}"]\n\tinsteadOf = {nightly.REPO_URL}\n')

    def env(self, **more):
        return {**super().env(**more), "GIT_CONFIG_GLOBAL": str(self.root / "gitconfig")}

    def look(self, *more):
        self.commit = None
        return look(self, *more)

    def run(self, *more, **env):
        return Night.run(self, *more, **env)


def test_a_look_runs_the_tip_of_a_smoke_branch_and_sends_its_status_and_the_next_look_runs_nothing(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    with a_stand_in_for_github(**ours(commit)) as github:
        run = Look(tmp_path, repository, github).look()
        assert run.done.returncode == 0, run.done.stdout + run.done.stderr
        assert run.done.stdout.splitlines()[:3] == ["tick: statuses: nothing waits", "tick: the tip of `smoke/a-change` has no result yet",
                                                    f"run for the commit {commit}: passed"]
        report = run.report
        assert report["mode"] == "one commit"
        assert report["result"] == "passed"
        # The commit that ran is the tip that the run fetched by the name of the branch, and the status goes on it.
        assert report["commit"] == commit
        assert report["pushed_as"] == ["smoke/a-change"]
        status = {"context": "server/smoke", "sha": commit, "state": "success",
                  "description": f"passed; started {report['started']}; {SESSIONS}; text {commits.MARK}"}
        assert report["status"] == status
        assert report["sent"] == "sent: 1"
        assert [call["call"] for call in github.seen] == [*asked_for(commit), *three_calls(commit)]
        assert [call["with"] for call in github.seen[:3]] == ["", "", ""]
        assert github.seen[5]["body"] == {"state": "success", "context": "server/smoke", "description": status["description"]}
        assert "build the proxy" in run.did()
        assert run.did().count("driver") == 1
        # A server with a new folder knows nothing of that run. GitHub holds the status of the app, so nothing is due.
        (tmp_path / "another server").mkdir()
        other = Look(tmp_path / "another server", repository, github).look()
        assert other.done.stdout.splitlines() == ["tick: statuses: nothing waits", "tick: nothing is due"]
        assert not (other.dir / "reports").exists()
        assert [call["call"] for call in github.seen[6:]] == [STATUSES_OF.format(commit)]


def test_the_key_is_in_no_command_of_a_smoke_run_and_in_nothing_that_it_leaves(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    with a_stand_in_for_github(**ours(commit)) as github:
        run = Look(tmp_path, repository, github).look()
        proof = github.seen[3]["with"].removeprefix("Bearer ")
        assert proof.count(".") == 2
    assert run.report["sent"] == "sent: 1"
    calls = run.calls()
    assert {"docker", "python-for-the-run", "nvidia-smi", "openssl"} == {call["tool"] for call in calls}
    for call in calls:
        held = " ".join([*call["environment"], *call["environment"].values()])
        assert KEY not in held, call["tool"]
        assert TOKEN not in held, call["tool"]
        assert str(run.key) not in held, call["tool"]
        if call["tool"] != "openssl":
            assert str(run.key) not in " ".join(call["args"]), call["args"]
    for where, text in left_by(run).items():
        for secret in (KEY, TOKEN, proof):
            assert secret not in text, where


def test_a_status_of_that_name_by_another_writer_does_not_stop_the_run(tmp_path, repository):
    commit = repository.on_a_branch({"docs/API.md": "the page, changed\n"})
    with a_stand_in_for_github(**ours(commit), statuses={commit: [a_status(writer=999)]}) as github:
        run = Look(tmp_path, repository, github).look()
        assert run.report["commit"] == commit
        assert run.report["sent"] == "sent: 1"
        assert [status["creator"]["id"] for status in github.statuses[commit]] == [WRITER, 999]


@pytest.mark.parametrize("record, pulls, why, asked", [
    (by("somebody-else"), [a_pull()], NOT_ON_THE_LIST, 2),
    (by("a-maintainer", kind="Bot"), [a_pull()], NOT_ON_THE_LIST, 2),
    (by(commit=OTHER), [a_pull()], NOT_AT_THE_TIP, 2),
    # A listed person pushed it, and it is not our own code by what GitHub says of its pull requests:
    # the head of a pull request from another repository, a commit with no pull request, a closed pull request.
    (by(), [a_pull(repo="somebody-else/ATLAS")], NOT_OUR_OWN, 3),
    (by(), [], NOT_OUR_OWN, 3),
    (by(), [a_pull(state="closed")], NOT_OUR_OWN, 3),
])
def test_a_look_starts_nothing_for_a_tip_that_fails_a_guard_and_writes_no_status(tmp_path, repository, record, pulls, why, asked):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    record = [{**move, "after": commit} if move["after"] == COMMIT else move for move in record]
    pulls = [{**pull, "head": {**pull["head"], "sha": commit}} for pull in pulls]
    with a_stand_in_for_github(activity={"refs/heads/smoke/a-change": record}, pulls={commit: pulls}) as github:
        run = Look(tmp_path, repository, github).look()
        assert [call["call"] for call in github.seen] == asked_for(commit)[:asked]
        assert github.statuses == {}
    assert run.done.returncode == 0
    assert run.done.stdout.splitlines() == ["tick: statuses: nothing waits", f"tick: `smoke/a-change` is not run: {why}", "tick: nothing is due"]
    assert not (run.dir / "reports").exists()
    assert run.waits() == []
    # Nothing was fetched, pulled, built or started: the look read the card and nothing else.
    assert run.did() == ["card"]
    assert not (run.dir / "tree" / ".git" / "refs" / "nightly").exists()


def test_a_branch_with_two_parts_after_smoke_is_not_run_by_a_look(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"}, name="smoke/a/change")
    with a_stand_in_for_github(activity={"refs/heads/smoke/a/change": by(commit=commit)}) as github:
        run = Look(tmp_path, repository, github).look()
        assert github.seen == []
    assert run.done.stdout.splitlines() == ["tick: statuses: nothing waits", "tick: nothing is due"]


def test_a_smoke_run_that_failed_sends_a_red_status_on_the_commit(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    with a_stand_in_for_github(**ours(commit)) as github:
        run = Look(tmp_path, repository, github, driver_rows=base.rows(0, 2, 0)).look()
        assert github.seen[5]["body"] == {"state": "failure", "context": "server/smoke", "description": (
            f"failed; started {run.report['started']}; 2 of 3 sessions with no harness defect; text {commits.MARK}")}
    assert run.done.returncode == 1
    assert run.report["sent"] == "sent: 1"


def test_a_run_for_a_commit_that_is_started_by_hand_sends_its_status_too(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    with a_stand_in_for_github() as github:
        run = commits.OneCommit(tmp_path, repository, github=github.port)
        run.run(commit, *run.with_the_key())
        assert [call["call"] for call in github.seen] == three_calls(commit)
    assert run.report["status"]["context"] == "server/smoke"
    assert run.report["status"]["sha"] == commit
    assert "GitHub: sent: 1" in run.done.stdout.splitlines()


def test_a_commit_that_is_not_the_tip_of_a_smoke_branch_gets_no_status(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"}, name="another/branch")
    with a_stand_in_for_github() as github:
        run = commits.OneCommit(tmp_path, repository, github=github.port)
        run.run(commit, *run.with_the_key())
        assert github.seen == []
    assert run.report["result"].startswith("not run: the commit")
    assert run.report["status"] == "no status: the run ended before the commit was found at the tip of a smoke branch"
    assert run.report["sent"] == "nothing waits"
    assert run.waits() == []


def test_when_taking_the_head_changes_the_script_the_new_copy_runs_the_commit_that_the_look_chose(tmp_path):
    repository = commits.Repository(tmp_path, script=SCRIPT.read_text())
    with a_stand_in_for_github() as github:
        run = Look(tmp_path, repository, github)
        run.script = run.dir / "tree" / "scripts" / "nightly_run.py"
        # `dev` goes on and changes the script; the commit stands on that new head and does not change the script.
        repository.head = repository.commit({"scripts/nightly_run.py": SCRIPT.read_text() + "\n# the copy of dev, one commit later\n"}, "refs/heads/dev")
        (tmp_path / "plan.json").write_text(json.dumps({**run.plan, "head": repository.head}))
        commit = repository.on_a_branch({"proxy/prompt.go": "a change\n"})
        github.plan.update(ours(commit))
        run.look()
        asked = [call["call"] for call in github.seen]
    assert run.done.returncode == 0, run.done.stdout + run.done.stderr
    assert run.report["script"] == "the new copy: taking the head changed the script"
    assert run.report["mode"] == "one commit"
    assert run.report["commit"] == commit
    assert run.report["result"] == "passed"
    assert run.script.read_text().endswith("# the copy of dev, one commit later\n")
    # The look chose once. The new copy runs that commit and does not look again.
    assert [line for line in run.done.stdout.splitlines() if line.startswith("tick: ")] == [
        "tick: statuses: nothing waits", "tick: the tip of `smoke/a-change` has no result yet"]
    assert asked == [*asked_for(commit), *three_calls(commit)]
    assert run.report["sent"] == "sent: 1"
    assert run.did().count("driver") == 1


# --- a look that starts nothing -----------------------------------------------------------------------------------

def test_a_look_that_starts_nothing_says_what_is_due_while_the_usual_stack_computes_on_the_card(server):
    night, github = server
    a_smoke_is_there(night, github)
    # A process computes on the card, as the usual stack of the server does. It holds no lock.
    planned(night, smoke_tips=[tip("a-change")], card=["4242, 9000"])
    assert nightly.what_is_due(settings(night, "--look"), EIGHT) == ("night", None, "the night is due")
    assert nightly.what_is_due(settings(night, "--look"), NOON) == ("one commit", COMMIT, "the tip of `smoke/a-change` has no result yet")
    # That look does not ask which processes compute on the card. The look that runs does, and starts nothing.
    assert "card" not in night.did()
    assert nightly.what_is_due(settings(night), NOON) == (None, None, "nothing is started: the card is in use")
    # A run of another kind holds the lock of the card: then nothing is due, and the usual stack is not stopped for it.
    with open(night.lock, "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
        assert nightly.what_is_due(settings(night, "--look"), EIGHT) == (None, None, "nothing is started: the card is in use")


def test_a_look_that_starts_nothing_ends_with_status_3_when_nothing_is_due_and_sends_what_waits(tmp_path):
    night = Night(tmp_path)
    night.run(*night.with_the_key())
    assert night.report["sent"] == "not sent: GitHub could not be reached; waits: 1"
    with a_stand_in_for_github() as github:
        look(night.github_at(github.port), "--look")
        assert [call["call"] for call in github.seen] == three_calls(HEAD)
    assert night.done.returncode == 3
    assert night.done.stdout.splitlines() == ["tick: statuses: sent: 1", "tick: nothing is due"]
    assert len(list(night.dir.glob("reports/*"))) == 1


def test_a_look_that_starts_nothing_ends_with_status_0_when_a_smoke_run_is_due_and_runs_nothing(tmp_path, repository):
    commit = repository.on_a_branch({"proxy/prompt.go": "package main // a new text\n"})
    with a_stand_in_for_github(**ours(commit)) as github:
        run = Look(tmp_path, repository, github).look("--look")
        assert [call["call"] for call in github.seen] == asked_for(commit)
    assert run.done.returncode == 0
    assert run.done.stdout.splitlines() == ["tick: statuses: nothing waits", "tick: the tip of `smoke/a-change` has no result yet"]
    assert not (run.dir / "reports").exists()
    assert run.did() == []
    assert run.waits() == []


def test_a_look_that_starts_nothing_gives_its_status_by_the_hour_of_the_night(server, monkeypatch, capsys):
    night, _github = server
    argv = night.command("--tick", "--look", *night.with_the_key())[2:]
    monkeypatch.setattr(nightly, "start_time", lambda: (at(8, 15), True, None, ""))
    assert nightly.main(argv) == 0
    assert capsys.readouterr().out.splitlines() == ["tick: statuses: nothing waits", "tick: the night is due"]
    monkeypatch.setattr(nightly, "start_time", lambda: (at(9, 15), True, None, ""))
    assert nightly.main(argv) == 3
    assert capsys.readouterr().out.splitlines() == ["tick: statuses: nothing waits", f"tick: {NO_SETTINGS}"]
    assert not (night.dir / "reports").exists()
    assert night.did() == []


def test_while_another_run_of_the_folder_goes_a_look_that_starts_nothing_finds_nothing_due(tmp_path):
    night = Night(tmp_path)
    with open(night.dir / "run.lock", "a") as other:
        fcntl.flock(other, fcntl.LOCK_EX | fcntl.LOCK_NB)
        look(night, "--look")
        assert night.done.returncode == 3
        look(night)
        assert night.done.returncode == 0
    assert night.done.stdout == "nightly run: another run of this folder is going. This one did nothing.\n"
    assert night.calls() == []
    assert night.asked() == []


def test_a_look_that_starts_nothing_is_a_look_of_the_timer(tmp_path):
    night = Night(tmp_path).run("--look")
    assert night.done.returncode == 2
    assert "--look is a look of the timer that starts nothing: give it with --tick" in night.done.stderr
    assert night.calls() == []


# --- held against the repository ----------------------------------------------------------------------------------

def gates_page():
    return (base.ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")


def required_checks(page):
    """The names in the page's table of the checks that a pull request needs, and the number in its heading."""
    heading, _, table = page.split("### Required", 1)[1].split("\n### ", 1)[0].partition("\n")
    rows = [line.split("|")[1] for line in table.splitlines() if line.startswith("| `")]
    return re.findall(r"`([^`]+)`", "\n".join(rows)), int(re.search(r"\((\d+)\)", heading).group(1))


def test_no_required_check_has_a_name_that_starts_with_server():
    names, count = required_checks(gates_page())
    assert len(names) == count, (len(names), count)
    assert count >= 20, (len(names), count)
    of_the_server = [name for name in names if name.lower().startswith("server/")]
    assert not of_the_server, (
        f"the table of required checks in docs/quality/gates.md names {of_the_server}. A name that starts with `server/` "
        "is a status of the development server, which is one machine that is not always on: a rule that waits for it stops every "
        "merge while that machine is off. Fix: take the check out of the required checks of the ruleset and out of "
        "that table.")
    for name in nightly.STATUS.values():
        assert name not in names


@pytest.mark.parametrize("row, found", [("| `server/nightly` | The night |", ["server/nightly"]),
                                        ("| `pr title`, `Server/smoke` | Two |", ["Server/smoke"]),
                                        ("| `the server/nightly check` | Another name |", [])])
def test_the_reading_of_that_table_finds_a_name_of_the_server_in_a_row(row, found):
    page = "## Checks\n\n### Required (2)\n\n| Check | What it does |\n|---|---|\n| `pr title` | The title |\n" + row + "\n\n### Not required\n"
    names, _count = required_checks(page)
    assert [name for name in names if name.lower().startswith("server/")] == found


def test_the_gates_page_says_what_the_script_does_with_its_own_names_and_numbers():
    page = gates_page()
    result = page.split("### The result on GitHub", 1)[1].split("### The timer", 1)[0]
    timer = page.split("### The timer", 1)[1].split("### The smoke run of one commit", 1)[0]
    for name in nightly.STATUS.values():
        assert f"`{name}`" in result
    for folder in (nightly.WAITING, nightly.SENT, nightly.GIVEN_UP):
        assert f"`{folder}`" in result
    assert f"After {nightly.KEEP_TRYING_DAYS} days" in result
    defaults = nightly.parse(["--dir", "/not-used"])
    assert f"Without it the bound is {defaults.smokes_a_day}." in timer
    assert f"Its status is 0 when\n  a run is due and {nightly.NOTHING_DUE} when none is" in timer
    assert f"the looks of the {nightly.QUIET_MINUTES} minutes after it start nothing either" in timer
    assert "(`--smoke-hours`, in UTC, as in `22-23,0-5`). With no such hours given it\n  starts none." in timer
    assert "head of an open pull request from a branch of this repository" in timer
    assert "The folder `to-send` is checked like the key" in result
    assert "every 15 minutes" in timer
    options = set(re.findall(r"add_argument\(\"(--[a-z-]+)\"", source()))
    for option in ("--tick", "--look", "--night-hour", "--smoke-by", "--smokes-a-day", "--status-writer"):
        assert option in timer, option
        assert option in options, option
    assert "`--status-key`" in result
    assert "--status-key" in options
    for gone in ("ATLAS_NIGHTLY_TOKEN", "--issue"):
        assert gone not in page
        assert gone not in source()
