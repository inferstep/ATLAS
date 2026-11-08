"""A night compares the seconds and the tokens of each task with the nights before it, and a change that stayed for
the last three nights makes the `server/trend` status red.

The first tests write earlier reports where the run keeps them and give the report of this night to the run's own
step. The last test runs a whole night with the stand-ins of the nightly tests. No test reaches GitHub.
"""
import datetime as dt
import json
from types import SimpleNamespace

import pytest

from tests.infrastructure.nightly.test_nightly_run import HEAD, Night, nightly

# The start of the night of a day in October 2026.
ON = "2026-10-{:02d}T08:00:03Z"


def a_report(started: str, seconds=10.0, tokens=2000, **more) -> dict:
    """The report of a night that passed, with the same seconds and tokens for each task."""
    return {"started": started, "mode": "night", "result": "passed", "commit": "a" * 40,
            "tasks": [{"task": task, "seconds": seconds, "tokens": tokens, "passed": True, "defects": 0} for task in nightly.TASKS],
            **more}


def keep(folder, *reports):
    """Write each report where the run keeps its reports: <folder>/reports/<start time>/report.json."""
    for report in reports:
        place = folder / "reports" / report["started"].replace("-", "").replace(":", "")
        place.mkdir(parents=True)
        (place / "report.json").write_text(json.dumps(report))


@pytest.mark.parametrize("slow_nights, ratios", [(1, [1.0, 1.0, 3.0]), (2, [1.0, 3.0, 3.0])], ids=["one slow night", "two slow nights"])
def test_one_or_two_slow_nights_are_no_change_that_stayed(tmp_path, slow_nights, ratios):
    # Five nights before the last three: the fewest that the trend judges with. This night is the last slow one.
    keep(tmp_path, *[a_report(ON.format(day)) for day in range(1, 9 - slow_nights)],
         *[a_report(ON.format(day), seconds=30.0) for day in range(9 - slow_nights, 8)])
    tonight = a_report(ON.format(8), seconds=30.0)
    status = nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight)
    assert tonight["trend"]["tasks"]["add_function"]["seconds"] == {"median": 10.0, "ratios": ratios, "stayed": False}
    assert tonight["trend"]["judged"] == "no change stayed"
    assert status == {"context": "server/trend", "sha": "a" * 40, "state": "success",
                      "description": f"no change stayed; started {ON.format(8)}"}


@pytest.mark.parametrize("measure, usual, changed, ratio, said", [
    pytest.param("seconds", 10.0, 30.0, 3.0, "offbyone seconds: 3 times the median", id="slower"),
    pytest.param("tokens", 2000, 1000, 0.5, "offbyone tokens: 0.5 times the median", id="fewer tokens"),
])
def test_a_change_in_each_of_the_last_three_nights_is_a_change_that_stayed(tmp_path, measure, usual, changed, ratio, said):
    keep(tmp_path, *[a_report(ON.format(day)) for day in range(1, 8)])
    last_three = [a_report(ON.format(day)) for day in (8, 9, 10)]
    for night in last_three:
        night["tasks"][nightly.TASKS.index("offbyone")][measure] = changed
    keep(tmp_path, *last_three[:2])
    tonight = last_three[2]
    status = nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight)
    assert tonight["trend"]["judged"] == "a change stayed"
    assert tonight["trend"]["tasks"]["offbyone"][measure] == {"median": usual, "ratios": [ratio] * 3, "stayed": True}
    assert tonight["trend"]["tasks"]["add_function"][measure]["stayed"] is False
    assert status["state"] == "failure"
    assert status["description"] == f"a change stayed; started {ON.format(10)}; {said}"


def test_a_night_that_was_not_run_a_smoke_run_and_a_second_run_of_a_night_do_not_count(tmp_path):
    usual = [a_report(ON.format(day)) for day in range(1, 8)]
    slow = [a_report(ON.format(day), seconds=30.0) for day in (8, 9)]
    # Each of these runs has the usual seconds: one that counted would stand between the slow nights. The first two
    # start before the night of their day, so either would be the first run of that day.
    smoke = a_report("2026-10-09T06:00:03Z", mode="one commit")
    not_run = a_report("2026-10-09T07:00:03Z", result="not run: the card was in use")
    second = a_report("2026-10-09T12:00:03Z")
    keep(tmp_path, *usual, *slow, smoke, not_run, second)
    tonight = a_report(ON.format(10), seconds=30.0)
    nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight)
    assert tonight["trend"]["nights"] == [ON.format(day) for day in range(1, 11)]
    assert tonight["trend"]["judged"] == "a change stayed"


@pytest.mark.parametrize("tonight", [
    pytest.param(a_report(ON.format(9), result="not run: the card was in use"), id="a night that was not run"),
    pytest.param(a_report("2026-10-08T12:00:03Z"), id="a second run of a night"),
])
def test_a_run_that_does_not_count_is_not_judged_and_gives_no_status(tmp_path, tonight):
    keep(tmp_path, *[a_report(ON.format(day)) for day in range(1, 9)])
    assert nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight) is None
    assert tonight["trend"] == {"judged": "not judged: this run is not the counted run of its night",
                                "status": "no status: not judged: this run is not the counted run of its night"}


def test_a_task_with_a_harness_defect_gives_no_value_for_that_night(tmp_path):
    keep(tmp_path, *[a_report(ON.format(day)) for day in range(1, 8)], *[a_report(ON.format(day), seconds=30.0) for day in (8, 9)])
    tonight = a_report(ON.format(10), seconds=30.0)
    tonight["tasks"][nightly.TASKS.index("add_function")]["defects"] = 1
    status = nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight)
    assert tonight["trend"]["tasks"]["add_function"]["seconds"] == {"median": 10.0, "ratios": [3.0, 3.0, None], "stayed": False}
    assert tonight["trend"]["tasks"]["offbyone"]["seconds"]["stayed"] is True
    assert status["description"] == (f"a change stayed; started {ON.format(10)}; offbyone seconds: 3 times the median; "
                                     "1 more in the report")


def test_with_fewer_than_five_counted_nights_before_the_last_three_the_trend_judges_nothing(tmp_path):
    keep(tmp_path, *[a_report(ON.format(day)) for day in range(1, 7)])
    tonight = a_report(ON.format(7), seconds=30.0)
    status = nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight)
    assert status is None
    assert tonight["trend"]["judged"] == "too few nights"
    assert tonight["trend"]["status"] == "no status: too few nights"
    assert tonight["trend"]["tasks"]["add_function"]["seconds"] == {"judged": "too few nights", "nights_with_a_value": 4}


def test_a_fault_of_the_comparison_is_a_line_of_the_trend_part_and_gives_no_status(tmp_path, monkeypatch):
    def broken(*args):
        raise KeyError("tasks")
    monkeypatch.setattr(nightly.trend, "of", broken)
    tonight = a_report(ON.format(10))
    assert nightly.with_the_trend(SimpleNamespace(dir=tmp_path), tonight) is None
    assert tonight["trend"] == {"judged": "not judged: an error of the comparison itself (KeyError)"}


@pytest.mark.parametrize("seconds_before, trend, said", [
    pytest.param([], None, "", id="no nights before: no trend status"),
    pytest.param([30.0] * 9, "success", "no change stayed; started {started}", id="the same seconds each night: green"),
    pytest.param([10.0] * 7 + [30.0] * 2, "failure",
                 "a change stayed; started {started}; add_function seconds: 3 times the median; 2 more in the report",
                 id="the last three nights are slower: red"),
])
def test_the_trend_status_changes_neither_the_result_of_the_night_nor_its_own_status(tmp_path, seconds_before, trend, said):
    night = Night(tmp_path, driver_rows=[{"task": task, "wall_s": 30.0, "tokens": 2000, "task_passed": True, "defects": []}
                                         for task in nightly.TASKS])
    now = dt.datetime.now(dt.timezone.utc)
    keep(night.dir, *[a_report((now - dt.timedelta(days=len(seconds_before) - n)).strftime("%Y-%m-%dT%H:%M:%SZ"), seconds=seconds)
                      for n, seconds in enumerate(seconds_before)])
    night.run()
    assert night.done.returncode == 0, night.done.stdout + night.done.stderr
    report = json.loads(sorted(night.dir.glob("reports/*/report.json"))[-1].read_text())
    assert report["result"] == "passed"
    waiting = {status["context"]: status for status in night.waits()}
    assert waiting.pop("server/nightly")["state"] == "success"
    if trend:
        assert waiting == {"server/trend": {"context": "server/trend", "sha": HEAD, "state": trend,
                                            "description": said.format(started=report["started"])}}
    else:
        assert waiting == {}
    assert night.asked() == []
