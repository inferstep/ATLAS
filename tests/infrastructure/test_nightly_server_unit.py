"""The timer of the development server: its unit and its timer file say what the gates page says, and the script
that they start stops the usual stack only when a run is due and always starts it again.

No test here reaches docker or the nightly run. The script is started with a path that holds stand-ins made for the
test: `docker`, and a command in the place of the nightly run. Each writes down how it was called and answers from a
small plan. Every run has a time limit.
"""
import json
import os
import re
import shlex
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from tests.infrastructure.test_nightly_run import ON_THE_PATH, ROOT, nightly

SERVER = ROOT / "scripts" / "server"
SCRIPT, UNIT, TIMER = SERVER / "nightly_look.sh", SERVER / "atlas-nightly.service", SERVER / "atlas-nightly.timer"
HOME = "/home/someone"
STAND_IN = r'''"""A stand-in for {name}: it writes down its call and answers from the plan."""
import json, sys, time
home = {home!r}
plan = json.load(open(home + "/plan.json"))
args = sys.argv[1:]
with open(home + "/calls.log", "a") as log:
    log.write(json.dumps([{name!r}, *args]) + "\n")
{body}
'''
DOCKER = '''
if args[0] == "ps":
    print("\\n".join(plan.get("running", [])))
if " ".join(args[:2]) in plan.get("fail", []) or args[0] in plan.get("fail", []):
    print("a planted failure of: docker " + " ".join(args), file=sys.stderr)
    sys.exit(1)
'''
NIGHTLY = '''
if "--look" in args:
    sys.exit(plan.get("look", 0))
time.sleep(plan.get("run_seconds", 0))
sys.exit(plan.get("run", 0))
'''
PS = ["docker", "ps", "--quiet", "--filter", "label=com.docker.compose.project=atlas"]


class Look:
    """A folder with the stand-ins, and what one look of the script did."""

    def __init__(self, root, **plan):
        self.root, self.marker = root, root / "run" / "atlas-nightly.stopped"
        (root / "bin").mkdir()
        (root / "run").mkdir()
        for name, body in (("docker", DOCKER), ("the-nightly-run", NIGHTLY)):
            code = root / f"{name}.py"
            code.write_text(STAND_IN.format(name=name, home=str(root), body=body))
            path = root / "bin" / name
            path.write_text(ON_THE_PATH.format(python=shlex.quote(sys.executable), code=shlex.quote(str(code))))
            path.chmod(0o755)
        self.plan(**plan)

    def plan(self, **plan):
        (self.root / "plan.json").write_text(json.dumps(plan))
        return self

    def command(self, mode):
        run = ["--", "the-nightly-run", "--dir", "a folder", "--tick"] if mode == "run" else []
        return ["sh", str(SCRIPT), mode, "atlas", str(self.marker), *run]

    def env(self):
        # The stand-ins come first on the path: the real docker cannot be reached.
        return {"PATH": f"{self.root / 'bin'}:/usr/bin:/bin"}

    def look(self, mode="run"):
        self.done = subprocess.run(self.command(mode), env=self.env(), capture_output=True, text=True, timeout=60)
        return self

    def calls(self):
        log = self.root / "calls.log"
        return [json.loads(line) for line in log.read_text().splitlines()] if log.exists() else []


ASKED = ["the-nightly-run", "--dir", "a folder", "--tick", "--look"]
RAN = ["the-nightly-run", "--dir", "a folder", "--tick"]


def test_when_nothing_is_due_the_usual_stack_is_not_touched_and_the_look_ends_with_status_0(tmp_path):
    look = Look(tmp_path, look=3, running=["c3", "c2", "c1"]).look()
    assert look.done.returncode == 0, look.done.stdout + look.done.stderr
    assert look.calls() == [ASKED]
    assert not look.marker.exists()


@pytest.mark.parametrize("status", [1, 2, 4, 255])
def test_a_look_that_ends_with_another_status_than_0_or_3_is_passed_on_and_nothing_is_stopped_or_run(tmp_path, status):
    # Status 2: the settings of the nightly run cannot be used. As "nothing due" nobody would see it.
    look = Look(tmp_path, look=status, running=["c3", "c2", "c1"]).look()
    assert look.done.returncode == status
    assert look.calls() == [ASKED]
    assert not look.marker.exists()


@pytest.mark.parametrize("run", [0, 1])
def test_when_a_run_is_due_the_usual_stack_is_stopped_the_run_is_made_and_the_stack_is_started_again(tmp_path, run):
    look = Look(tmp_path, look=0, run=run, running=["c3", "c2", "c1"]).look()
    # Also when the run failed, and the status of the look is the status of the run.
    assert look.done.returncode == run, look.done.stdout + look.done.stderr
    assert look.calls() == [ASKED, PS, ["docker", "stop", "c3", "c2", "c1"], RAN,
                            # The oldest container first, as they were made.
                            ["docker", "start", "c1"], ["docker", "start", "c2"], ["docker", "start", "c3"]]
    assert not look.marker.exists()


def test_the_containers_of_the_usual_stack_are_found_by_its_project_and_nothing_else_is_stopped(tmp_path):
    look = Look(tmp_path, look=0, running=["c1"]).look()
    assert look.calls()[1] == PS
    assert [call for call in look.calls() if call[:2] == ["docker", "stop"]] == [["docker", "stop", "c1"]]
    assert {call[1] for call in look.calls() if call[0] == "docker"} == {"ps", "stop", "start"}


def test_when_the_usual_stack_does_not_run_nothing_is_stopped_and_nothing_is_started_after_the_run(tmp_path):
    look = Look(tmp_path, look=0, running=[]).look()
    assert look.done.returncode == 0
    assert look.calls() == [ASKED, PS, RAN]
    assert not look.marker.exists()


def test_the_step_after_a_look_does_nothing_when_nothing_was_stopped(tmp_path):
    look = Look(tmp_path).look("after")
    assert look.done.returncode == 0
    assert look.calls() == []


def test_the_step_after_a_look_starts_again_what_a_look_that_did_not_end_had_stopped(tmp_path):
    look = Look(tmp_path)
    look.marker.write_text("c3\nc2\nc1\n")
    look.look("after")
    assert look.done.returncode == 0
    assert look.calls() == [["docker", "start", "c1"], ["docker", "start", "c2"], ["docker", "start", "c3"]]
    assert not look.marker.exists()
    # A second call finds nothing to do.
    look.look("after")
    assert len(look.calls()) == 3


def test_a_look_first_starts_again_what_an_earlier_look_left_stopped(tmp_path):
    # A look that never ended, as after a restart of the server in the middle of a run, leaves only the marker
    # file. The next look starts what it names before anything else, also when nothing is due, and says so.
    look = Look(tmp_path, look=3)
    look.marker.write_text("c2\nc1\n")
    look.look()
    assert look.done.returncode == 0
    assert look.calls() == [["docker", "start", "c1"], ["docker", "start", "c2"], ASKED]
    assert not look.marker.exists()
    assert "nightly_look.sh: the usual stack runs again: 2 container(s) that a look had stopped were started." in look.done.stdout
    # It is started once: the look after that finds no marker file and starts nothing.
    look.look()
    assert look.calls()[3:] == [ASKED]
    assert "the usual stack runs again" not in look.done.stdout


def test_a_look_with_no_marker_file_says_nothing_about_the_usual_stack(tmp_path):
    look = Look(tmp_path, look=3).look()
    assert look.done.returncode == 0
    assert look.done.stdout == ""


def test_when_the_stop_of_the_usual_stack_fails_no_run_is_started_and_the_marker_names_what_was_to_stop(tmp_path):
    look = Look(tmp_path, look=0, running=["c2", "c1"], fail=["stop"]).look()
    assert look.done.returncode != 0
    assert RAN not in look.calls()
    assert look.marker.read_text() == "c2\nc1\n"
    # The step after the look then starts them again.
    look.plan().look("after")
    assert look.calls()[-2:] == [["docker", "start", "c1"], ["docker", "start", "c2"]]
    assert not look.marker.exists()


def test_a_container_that_does_not_start_again_is_named_the_others_start_and_the_marker_stays(tmp_path):
    look = Look(tmp_path, look=0, run=0, running=["c3", "c2", "c1"], fail=["start c2"]).look()
    assert look.done.returncode == 1
    assert look.calls()[-3:] == [["docker", "start", "c1"], ["docker", "start", "c2"], ["docker", "start", "c3"]]
    assert "the container c2 of the usual stack did not start again" in look.done.stderr
    assert "The next look tries again. Fix: look at it with 'docker ps --all', and start the usual stack by hand." in look.done.stderr
    assert "When that container is gone for good, remove the marker file of the look" in look.done.stderr
    assert "the usual stack runs again" not in look.done.stdout
    assert look.marker.read_text() == "c3\nc2\nc1\n"
    # The step after the look says so by its status too, and the next one that works clears the marker.
    assert look.look("after").done.returncode == 1
    assert look.plan().look("after").done.returncode == 0
    assert not look.marker.exists()


def test_a_look_that_is_told_to_stop_waits_for_the_run_and_then_starts_the_usual_stack_again(tmp_path):
    look = Look(tmp_path, look=0, run=0, run_seconds=3, running=["c1"])
    going = subprocess.Popen(look.command("run"), env=look.env(), stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True)
    deadline = time.monotonic() + 30
    while RAN not in look.calls() and time.monotonic() < deadline:
        time.sleep(0.05)
    assert RAN in look.calls()
    # Only the shell is told to stop here. On the server the run is told too, and stops its own stack first.
    os.kill(going.pid, signal.SIGTERM)
    time.sleep(0.5)
    assert ["docker", "start", "c1"] not in look.calls(), "the usual stack was started while the run still had the card"
    assert going.wait(timeout=30) == 0
    assert look.calls()[-1] == ["docker", "start", "c1"]
    assert not look.marker.exists()


@pytest.mark.parametrize("words, said", [
    (["run", "atlas"], "three words are needed"),
    (["now", "atlas", "a-marker"], "the first word is 'run' or 'after', not 'now'"),
    (["run", "atlas", "a-marker", "the-nightly-run"], "the command of the nightly run comes after --"),
    (["run", "atlas", "a-marker", "--"], "no command of the nightly run was given"),
])
def test_a_call_in_another_form_ends_with_status_2_and_says_the_usage(tmp_path, words, said):
    look = Look(tmp_path, look=0, running=["c1"])
    done = subprocess.run(["sh", str(SCRIPT), *words], env=look.env(), capture_output=True, text=True, timeout=60)
    assert done.returncode == 2
    assert f"nightly_look.sh: {said}. Usage: nightly_look.sh run <usual project> <marker file> -- <command>" in done.stderr
    assert look.calls() == []


# --- the unit and the timer ---------------------------------------------------------------------------------------

def settings(path):
    """The settings of a unit file: (name, value) for each line, with a line that goes on joined to its start."""
    text = re.sub(r"\\\n\s*", "", path.read_text(encoding="utf-8"))
    return [tuple(line.split("=", 1)) for line in text.splitlines() if "=" in line and not line.startswith("#")]


def one(path, name):
    (value,) = [value for key, value in settings(path) if key == name]
    return value


def in_seconds(span):
    number, unit = re.fullmatch(r"(\d+)(s|min|h)", span).groups()
    return int(number) * {"s": 1, "min": 60, "h": 3600}[unit]


def test_the_timer_starts_one_look_every_15_minutes():
    assert dict(settings(TIMER)) == {"Description": "ATLAS nightly run: a look every 15 minutes", "OnCalendar": "*:0/15",
                                     "AccuracySec": "1min", "Unit": UNIT.name, "WantedBy": "timers.target"}
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    assert "starts the script with `--tick`, every 15 minutes" in page


def test_the_unit_has_no_condition_and_gives_nothing_through_the_environment():
    names = [name for name, _value in settings(UNIT)]
    assert names == ["Description", "Type", "TimeoutStartSec", "TimeoutStopSec", "ExecStart", "ExecStopPost"]
    # A condition that ends with 1 to 254 skips the unit with no failure: a look whose settings cannot be used would
    # be skipped in silence. And the path of the key is an argument, never a value of the environment.
    text = UNIT.read_text(encoding="utf-8")
    for word in ("ExecCondition", "Environment", "PassEnvironment", "LoadCredential", "SetCredential"):
        assert not re.search(rf"^{word}\w*=", text, re.M), word
    assert one(UNIT, "Type") == "oneshot"


def test_the_unit_starts_the_script_of_the_look_from_the_tree_of_the_run_and_starts_the_usual_stack_again_after_it():
    start, after = shlex.split(one(UNIT, "ExecStart")), shlex.split(one(UNIT, "ExecStopPost"))
    helper = "%h/atlas-nightly/tree/scripts/server/nightly_look.sh"
    marker = "%h/atlas-nightly/usual-stack.stopped"
    assert start[:5] == [helper, "run", "atlas", marker, "--"]
    assert after == [helper, "after", "atlas", marker]
    # The marker file lies in the folder of the run, on disk. The runtime folder of the user (%t) is in memory: a
    # restart of the server empties it, Docker does not start a stopped container again by itself, and the usual
    # stack would stay down.
    assert marker.startswith(start[start.index("--dir") + 1] + "/")
    assert "%t" not in " ".join(start + after)
    page = " ".join((ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8").split())
    assert "That holds for a restart of the server too: the file that names what was stopped lies in the folder of the run, on disk." in page
    assert start[5:7] == ["%h/atlas-nightly/venv/bin/python", "%h/atlas-nightly/tree/scripts/nightly_run.py"]
    assert (ROOT / "scripts" / "server" / "nightly_look.sh").is_file()
    # `atlas` is the compose project of the usual stack, as the driver of the sessions names it. The stack of the
    # nightly run is another project, so a look never stops the stack of a run.
    driver = (ROOT / "scripts" / "e2e-reliability.py").read_text(encoding="utf-8")
    assert 'ap.add_argument("--compose-project", default="atlas",' in driver
    assert nightly.PROJECT == "atlas-nightly"
    # The files that the timer runs on the server itself are those of `dev`: a smoke commit that changes one is not run.
    assert nightly.FROM_DEV["scripts/server/"] == "the files of the timer, which run on the server itself"


def test_the_arguments_of_the_unit_are_arguments_of_the_script_and_the_key_lies_outside_the_folder_of_the_run():
    words = [word.replace("%h", HOME) for word in shlex.split(one(UNIT, "ExecStart"))[7:]]
    args = nightly.parse(words)
    assert args.tick is True
    assert args.look is False, "the script of the look adds --look itself"
    assert args.commit is None
    assert args.dir == Path(HOME) / "atlas-nightly"
    assert args.python == f"{HOME}/atlas-nightly/venv/bin/python"
    assert args.status_key == Path(HOME) / ".config" / "atlas-nightly" / "status-app.pem"
    assert args.dir not in args.status_key.parents
    assert args.smoke_by == ["itigges22"]
    assert (args.night_hour, args.smokes_a_day) == (8, 6)
    # The hours for smoke runs, in UTC. The night has its own hour, and it is not one of them.
    assert args.smoke_hours == frozenset({6, 7})
    assert args.night_hour not in args.smoke_hours
    # The app, by its client id, and the account that writes its statuses. Both are public.
    assert (args.status_app, args.status_writer) == ("Iv23liQAJ0Ousufa05LE", 340202947)


def test_a_look_that_is_told_to_stop_has_the_time_to_stop_the_stack_of_the_run():
    # The run stops its stack with a limit of 300 seconds (stop_stack). systemd must not end it before that.
    assert in_seconds(one(UNIT, "TimeoutStopSec")) >= 300 + 120
    source = (ROOT / "scripts" / "nightly_run.py").read_text(encoding="utf-8")
    assert 'done = command([*compose(args), "down", "--volumes", "--remove-orphans"], 300)' in source
    # A night: the wait for the images, the limit with the card, and the pulls of the first night.
    defaults = nightly.parse(["--dir", "/not-used"])
    assert in_seconds(one(UNIT, "TimeoutStartSec")) >= (defaults.image_wait_minutes + defaults.limit_minutes) * 60 * 2


@pytest.mark.parametrize("path", [UNIT, TIMER, SCRIPT])
def test_no_file_of_the_timer_names_a_path_or_a_machine_of_a_server(path):
    text = path.read_text(encoding="utf-8")
    # Every place is under the home folder of the user (%h).
    for word in re.findall(r"(?<![\w%.<])/(?:home|root|srv|opt|var|mnt|data|Users)/\S*", text):
        raise AssertionError(f"{path.name} names the path {word}")
    assert not re.search(r"\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b", text)
    assert not re.search(r"\w@\w", text), "a name of a user at a machine"


def test_the_gates_page_gives_the_steps_on_the_server_with_the_names_of_the_files():
    page = (ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8")
    steps = page.split("### The steps on the server", 1)[1].split("\n## ", 1)[0]
    for word in ("scripts/server/atlas-nightly.service", "scripts/server/atlas-nightly.timer", "~/.config/systemd/user/",
                 "~/.config/atlas-nightly/status-app.pem", "chmod 600", "systemctl --user enable --now atlas-nightly.timer",
                 "--tick --look", "~/atlas-nightly", "loginctl enable-linger"):
        assert word in steps, word
    # Each place that the steps name is the place that the unit names.
    words = shlex.split(one(UNIT, "ExecStart"))
    key, folder = words[words.index("--status-key") + 1], words[words.index("--dir") + 1]
    assert set(re.findall(r"~[\w/.-]*status-app\.pem", steps)) == {key.replace("%h", "~")}
    assert set(re.findall(r"~/atlas-nightly\b(?!/)", steps)) == {folder.replace("%h", "~")}
    assert f"`chmod 600 {key.replace('%h', '~')}`" in steps
    hours = words[words.index("--smoke-hours") + 1]
    assert f"(`--smoke-hours {hours}`)" in steps
    timer = page.split("### The timer", 1)[1].split("### The smoke run of one commit", 1)[0]
    assert "The unit names the hours 6 and 7. The hours are\n  in UTC, so against a local clock they move by one hour when that clock\n  changes" in timer
    # The tools that the script and the look start by their name are on the path of a user unit, which the page names.
    assert "What a look needs on its path: `docker`, `git` and `openssl`." in steps
    assert "The path of a look is the user unit's own, not the\npath of a login shell" in steps
    source = (ROOT / "scripts" / "nightly_run.py").read_text(encoding="utf-8") + SCRIPT.read_text(encoding="utf-8")
    for tool in ("docker", "git", "openssl"):
        assert re.search(rf'\["{tool}"|^\s*{tool} |\$\({tool} ', source, re.M), tool


def test_no_text_of_the_nightly_run_says_where_the_server_is():
    # The server is "one machine that is not always on". Where it stands is said nowhere in the repository.
    for path in (ROOT / "scripts" / "nightly_run.py", ROOT / "docs" / "quality" / "gates.md", UNIT, TIMER, SCRIPT):
        text = " ".join(path.read_text(encoding="utf-8").split())
        for words in ("at home", "machine at ", "in the house", "home network", "homelab"):
            assert words not in text, f"{path.name} says `{words}`"
    page = " ".join((ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8").split())
    assert "The server is one machine, and it is not always on." in page
    assert "The server is one machine that is not always on, so a night with no status counts neither way." in page


def test_with_the_real_script_of_the_run_a_card_that_is_held_with_no_lock_stops_the_usual_stack_once_and_not_at_the_next_look(tmp_path):
    """The whole way of two looks, with the script of the look and the script of the run as they are.

    A smoke branch waits. The usual stack runs, and a process computes on the card with no lock. The first look
    says that a run is due; the usual stack is stopped; the look that runs then finds the card held and starts
    nothing; the usual stack is started again. The second look, in the same hour, says that nothing is due: the
    usual stack is not stopped and started a second time.
    """
    from tests.infrastructure import test_nightly_run as base
    from tests.infrastructure import test_nightly_run_status as status

    with base.a_stand_in_for_github() as github:
        night = base.Night(tmp_path, github=github.port, left=["c1"], card=["4242, 9000"], smoke_tips=[status.tip("a-change")])
        github.plan.update(activity={"refs/heads/smoke/a-change": status.by()}, pulls={status.COMMIT: [status.a_pull()]})
        run = ["sh", str(SCRIPT), "run", "atlas", str(tmp_path / "stopped"), "--",
               *night.command("--tick", "--night-hour", status.far_hour(), "--smoke-by", "a-maintainer", "--smoke-hours", "0-23",
                              *night.with_the_key())]
        env = {**night.env(), "PATH": f"{tmp_path / 'bin'}:/usr/bin:/bin"}
        first = subprocess.run(run, env=env, capture_output=True, text=True, timeout=90)
        docker = [call["args"] for call in night.calls() if call["tool"] == "docker"]
        second = subprocess.run(run, env=env, capture_output=True, text=True, timeout=90)
    assert first.returncode == 0, first.stdout + first.stderr
    assert first.stdout.splitlines() == ["tick: statuses: nothing waits", "tick: the tip of `smoke/a-change` has no result yet",
                                         "tick: statuses: nothing waits", "tick: nothing is started: the card is in use",
                                         "nightly_look.sh: the usual stack runs again: 1 container(s) that a look had stopped were started."]
    assert docker == [["ps", "--quiet", "--filter", "label=com.docker.compose.project=atlas"], ["stop", "c1"], ["start", "c1"]]
    assert second.returncode == 0, second.stdout + second.stderr
    assert second.stdout.splitlines()[0] == "tick: statuses: nothing waits"
    assert re.fullmatch(r"tick: nothing is started: at \S+Z a process held the card with no lock, and the looks of the 60 minutes "
                        r"after that start nothing", second.stdout.splitlines()[1])
    # The second look touched neither the usual stack nor the card, and no run was started by either.
    assert [call["args"] for call in night.calls() if call["tool"] == "docker"] == docker
    assert [call["tool"] for call in night.calls()].count("nvidia-smi") == 1
    assert not (tmp_path / "stopped").exists()
    assert not (night.dir / "reports").exists()
