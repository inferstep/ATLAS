"""A pull request that changes the text every request carries needs the green status of the server's smoke run on its
head commit.

The check reads recordings from two commits. Each test makes a small repository with recordings made for the test.
GitHub is a stand-in: a small server on this machine that holds the statuses of the commits. The script has one fixed
address; what a started check asks there is turned to the stand-in and written down, and a call to another address
is refused. No test reaches the network.
"""
import http.server
import importlib.util
import json
import os
import re
import subprocess
import sys
import threading
import urllib.parse
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "scripts" / "smoke_result.py"
WORKFLOW = ROOT / ".github" / "workflows" / "smoke-result.yml"
_spec = importlib.util.spec_from_file_location("smoke_result", SCRIPT)
smoke = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(smoke)


def recording(system="You are a coding assistant. Tools: read_file, write_file.", grammar="root ::= call", schema="object",
              task="add a function", note="a note that the proxy writes in one situation"):
    """A recording as the replay tests keep it, with two requests to the model after one to another service.

    With None for the system prompt, the grammar or the schema, no request of the recording carries that part.
    """
    first = {"messages": [{"role": "user", "content": task}], "temperature": 0.3}
    if system is not None:
        first["messages"].insert(0, {"role": "system", "content": system})
    if grammar is not None:
        first["grammar"] = grammar
    if schema is not None:
        first["response_format"] = {"schema": {"type": schema}}
    second = {**first, "messages": first["messages"] + [{"role": "user", "content": note}]}
    return {"source": {"kind": "smoke"}, "exchanges": [
        {"service": "sandbox", "method": "GET", "path": "/health", "request": "", "response": "{}"},
        {"service": "model", "method": "POST", "path": "/v1/chat/completions", "request": json.dumps(first), "response": ""},
        {"service": "model", "method": "POST", "path": "/v1/chat/completions", "request": json.dumps(second), "response": ""}]}


# The account of the server's GitHub App, as a test has it, and a token that can read statuses.
SERVER, TOKEN = 424242, "a-token-made-for-this-test-0123456789"
# What every Python that a test starts does first (a file `sitecustomize.py` on its path). The script has one fixed
# address and no way to give it another, so the stand-in of GitHub is put behind that address here.
ON_EVERY_START = '''"""Each call to GitHub's address goes to the stand-in of the test and is written down. Another address is refused."""
import json
import urllib.error
import urllib.request

_open = urllib.request.OpenerDirector.open


def _to_the_stand_in(self, request, *args, **kwargs):
    asked = request if isinstance(request, str) else request.full_url
    with open({log!r}, "a") as log:
        log.write(json.dumps([request.get_method(), asked]) + "\\n")
    if not asked.startswith("https://api.github.com/") or not {port}:
        raise urllib.error.URLError("the test has no stand-in at this address")
    request.full_url = "http://127.0.0.1:{port}/" + asked[len("https://api.github.com/"):]
    return _open(self, request, *args, **kwargs)


urllib.request.OpenerDirector.open = _to_the_stand_in
'''


class GitHub(http.server.BaseHTTPRequestHandler):
    """A stand-in for the one place of GitHub that the check reads: the statuses of a commit, newest first, in pages."""
    on: dict = {}
    seen: list = []
    answer = None
    cut_short = False
    port = 0

    def do_GET(self):
        where = urllib.parse.urlsplit(self.path)
        GitHub.seen.append((self.path, self.headers.get("Authorization", "")))
        at = re.fullmatch(r"/repos/inferstep/ATLAS/commits/([0-9a-f]{40})/statuses", where.path)
        status, body = (404, {"message": "the stand-in has no such place"})
        if GitHub.answer is not None:
            status, body = GitHub.answer
        elif at:
            asked = urllib.parse.parse_qs(where.query)
            page, each = int(asked["page"][0]), int(asked["per_page"][0])
            status, body = 200, GitHub.on.get(at.group(1), [])[(page - 1) * each:page * each]
        raw = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        # An answer that is cut short: the head says more bytes than come.
        self.send_header("Content-Length", str(len(raw) + (50 if GitHub.cut_short else 0)))
        if status in (301, 302, 307, 308):
            # The other place is a fixed one of the stand-in itself. A call that comes there was followed.
            self.send_header("Location", f"http://127.0.0.1:{GitHub.port}/elsewhere")
        self.end_headers()
        self.wfile.write(raw)

    def log_message(self, *_args):
        pass


@pytest.fixture(autouse=True)
def github():
    """GitHub for each test of this file: it holds no status until the test gives it one."""
    GitHub.on, GitHub.seen, GitHub.answer, GitHub.cut_short = {}, [], None, False
    server = http.server.HTTPServer(("127.0.0.1", 0), GitHub)
    GitHub.port = server.server_port
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield GitHub
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=10)


def a_status(state="success", by=SERVER, name="server/smoke", said="passed; started 2026-10-08T08:00:03Z; 3 of 3 sessions with no harness defect; text 0123456789ab", **more):
    return {"context": name, "state": state, "description": said, "creator": {"id": by, "login": "an-account", "type": "Bot"}, **more}


class Repo:
    """A repository with recordings, and the commits that were made in it."""

    def __init__(self, home):
        self.home, self.root = home, home / "the repository"
        self.commits = 0
        (self.root / "tests" / "replay" / "recordings").mkdir(parents=True)
        (home / "site").mkdir()
        (home / "site" / "sitecustomize.py").write_text(ON_EVERY_START.format(log=str(home / "asked.log"), port=GitHub.port))
        self.git("init", "-q", "-b", "dev")

    def git(self, *args):
        return subprocess.run(["git", "-C", str(self.root), "-c", "user.name=test", "-c", "user.email=test@example.invalid", *args],
                              capture_output=True, text=True, check=True, timeout=60).stdout.strip()

    def commit(self, **recordings):
        """Write these recordings (None takes one away) and commit. Gives the commit."""
        for name, content in recordings.items():
            path = self.root / "tests" / "replay" / "recordings" / f"{name}.json"
            if content is None:
                path.unlink(missing_ok=True)
            else:
                path.write_text(content if isinstance(content, str) else json.dumps(content, indent=1), encoding="utf-8")
        # A file that differs in each commit, so that a commit with no other change can be made.
        self.commits += 1
        (self.root / "count.txt").write_text(str(self.commits), encoding="utf-8")
        self.git("add", "-A")
        self.git("commit", "-q", "-m", "a commit")
        return self.git("rev-parse", "HEAD")

    def env(self, text=""):
        """What a started check gets: the text of the pull request, a token, and GitHub's address behind the stand-in."""
        return {"PATH": os.environ["PATH"], "PR_BODY": text, "GITHUB_TOKEN": TOKEN, "PYTHONPATH": str(self.home / "site"),
                "PYTHONDONTWRITEBYTECODE": "1"}

    def check(self, base, head, text=""):
        return subprocess.run([sys.executable, str(SCRIPT), "--base", base, "--head", head], cwd=self.root, capture_output=True,
                              text=True, timeout=60, check=False, env=self.env(text))

    def asked(self):
        """Each address that the started checks asked, in order."""
        log = self.home / "asked.log"
        return [json.loads(line)[1] for line in log.read_text().splitlines()] if log.exists() else []

    def judged(self, monkeypatch, capsys, base, head, text="", writer=SERVER):
        """The check in the process of the test, with this account as the server's. Gives its status and what it printed."""
        monkeypatch.setattr(smoke, "WRITER", writer)
        monkeypatch.setattr(smoke, "GITHUB_API", f"http://127.0.0.1:{GitHub.port}")
        monkeypatch.setenv("PR_BODY", text)
        monkeypatch.setenv("GITHUB_TOKEN", TOKEN)
        monkeypatch.chdir(self.root)
        status = smoke.main(["--base", base, "--head", head])
        printed = capsys.readouterr()
        return status, printed.out + printed.err

    def mark(self, commit):
        return subprocess.run([sys.executable, str(SCRIPT), "--mark", commit], cwd=self.root, capture_output=True, text=True,
                              timeout=60, check=True).stdout.strip()


def line(commit, mark, passed=3, seconds=412):
    """The line that a smoke run prints for the person who started it. The check does not read it as a result."""
    return (f"Smoke run on {commit[:12]}: {passed} of 3 sessions with no harness defect, {seconds} s; "
            f"the changed text is part of every request (text {mark}).")


NO_STATUS = "and its head commit has no status `server/smoke` of the development server"


@pytest.fixture
def repo(tmp_path):
    made = Repo(tmp_path)
    made.base = made.commit(one=recording(), two=recording(task="repair a fault"))
    return made


NEW = "You are a coding assistant. Tools: read_file, write_file, delete_file."


# --- when the smoke run is asked for ------------------------------------------------------------------------------

def test_a_pull_request_that_changes_no_recording_is_asked_for_nothing(repo):
    head = repo.commit()
    done = repo.check(repo.base, head)
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout, done.stdout + done.stderr
    # GitHub is asked only when the smoke run is asked for.
    assert repo.asked() == []
    assert GitHub.seen == []


@pytest.mark.parametrize("change", [
    {"one": recording(note="another note in the same situation")},
    {"one": recording(task="add another function")},
    {"three": recording(task="a new case")},
    {"two": None},
    # The system prompt of one session only, for example a block about that session's project.
    {"one": recording(system=NEW)},
    {"three": recording(system=NEW, grammar="root ::= other", schema="array")},
])
def test_a_change_of_a_recording_in_text_that_not_every_request_carries_is_asked_for_nothing(repo, change):
    # The recording shows that text in its situation, and the replay job holds it.
    done = repo.check(repo.base, repo.commit(**change))
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout, done.stdout + done.stderr


@pytest.mark.parametrize("change, what", [
    ({"system": NEW}, "the system prompt"),
    ({"grammar": "root ::= call | done"}, "the grammar"),
    ({"schema": "array"}, "the schema of the reply"),
    ({"system": NEW, "grammar": "root ::= call | done"}, "the system prompt and the grammar"),
])
def test_a_change_of_text_that_every_request_carries_is_red_while_the_server_wrote_no_status(repo, change, what):
    head = repo.commit(one=recording(**change), two=recording(task="repair a fault", **change))
    done = repo.check(repo.base, head, "A pull request with no word about a smoke run.")
    assert done.returncode == 1
    assert f"::error title=smoke result::this pull request changes {what}, which every request to the model carries, {NO_STATUS}" in done.stdout
    assert "Fix: ask a maintainer for the smoke run of the head commit" in done.stdout
    assert "pushes that commit to a branch `smoke/<name>`" in done.stdout
    # The message says how the check turns green when the status has come.
    assert 'A status that comes later does not start this check again: open this job and choose "Re-run jobs"' in done.stdout
    assert "after two red smoke runs the change stops and is thought over" in done.stdout
    # The statuses of exactly the head commit were asked, at GitHub's own address, with the token of the job.
    assert repo.asked() == [f"https://api.github.com/repos/inferstep/ATLAS/commits/{head}/statuses?per_page=100&page=1"]
    assert GitHub.seen == [(f"/repos/inferstep/ATLAS/commits/{head}/statuses?per_page=100&page=1", f"Bearer {TOKEN}")]
    assert TOKEN not in done.stdout + done.stderr


@pytest.mark.parametrize("gone, what", [
    ({"grammar": None}, "the grammar"),
    ({"schema": None}, "the schema of the reply"),
    ({"system": None}, "the system prompt"),
    ({"system": None, "grammar": None, "schema": None}, "the system prompt and the grammar and the schema of the reply"),
])
def test_a_part_that_no_request_carries_any_more_is_a_change_of_every_request(repo, gone, what):
    head = repo.commit(one=recording(**gone), two=recording(task="repair a fault", **gone))
    done = repo.check(repo.base, head, "A pull request with no word about a smoke run.")
    assert done.returncode == 1, done.stdout + done.stderr
    assert f"this pull request changes {what}, which every request to the model carries" in done.stdout


@pytest.mark.parametrize("new", [{"grammar": None}, {"schema": None}, {"system": None}])
def test_a_part_that_is_new_in_every_request_is_a_change_of_every_request(tmp_path, new):
    made = Repo(tmp_path)
    base = made.commit(one=recording(**new), two=recording(task="repair a fault", **new))
    head = made.commit(one=recording(), two=recording(task="repair a fault"))
    done = made.check(base, head, "A pull request with no word about a smoke run.")
    assert done.returncode == 1, done.stdout + done.stderr
    assert NO_STATUS in done.stdout


def test_a_part_that_one_recording_loses_and_the_others_keep_is_asked_for_nothing(repo):
    done = repo.check(repo.base, repo.commit(one=recording(grammar=None)))
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout


def test_a_part_that_no_request_carries_on_either_side_is_no_change(tmp_path):
    # The recordings of this repository are such a case: no request of them carries a grammar.
    made = Repo(tmp_path)
    base = made.commit(one=recording(grammar=None), two=recording(grammar=None, task="repair a fault"))
    head = made.commit(one=recording(grammar=None, note="another note in the same situation"))
    done = made.check(base, head)
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout


def test_the_first_recordings_of_a_branch_are_a_change_of_every_request(tmp_path):
    made = Repo(tmp_path)
    base = made.commit()
    head = made.commit(one=recording(), two=recording(task="repair a fault"))
    assert made.check(base, head, "no line").returncode == 1
    assert made.check(base, head, no_server(made.mark(head))).returncode == 0


def test_a_new_recording_beside_changed_ones_does_not_hide_the_change(repo):
    head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"), three=recording(system=NEW))
    assert repo.check(repo.base, head).returncode == 1


# --- the status of the server -------------------------------------------------------------------------------------

def no_server(mark):
    return ("No smoke run: the server is not available. Not tried with a real model; the first nightly run after "
            f"the server is back covers this text (text {mark}).")


@pytest.fixture
def changed(repo):
    """A repository whose head changes the system prompt of every request."""
    repo.head = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    return repo


def test_a_green_status_of_the_server_on_the_head_commit_makes_the_check_green(changed, monkeypatch, capsys):
    GitHub.on[changed.head] = [a_status()]
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head, "What this changes.")
    assert status == 0, printed
    assert printed == ("smoke result: the smoke run of the server passed on the head commit (`passed; started 2026-10-08T08:00:03Z; "
                       "3 of 3 sessions with no harness defect; text 0123456789ab`).\n")
    assert GitHub.seen == [(f"/repos/inferstep/ATLAS/commits/{changed.head}/statuses?per_page=100&page=1", f"Bearer {TOKEN}")]


def test_the_script_as_it_is_takes_a_green_status_of_the_servers_own_account_and_of_no_other(changed):
    # No stand-in for the account here: the script is started as the job starts it, with the id that it holds.
    assert smoke.WRITER == 340202947, "the id of the account `atlas-server-results[bot]`"
    GitHub.on[changed.head] = [a_status(by=smoke.WRITER)]
    done = changed.check(changed.base, changed.head)
    assert done.returncode == 0, done.stdout + done.stderr
    assert done.stdout.startswith("smoke result: the smoke run of the server passed on the head commit (`passed; started ")
    for other in (smoke.WRITER + 1, SERVER, 0, str(smoke.WRITER)):
        GitHub.on[changed.head] = [a_status(by=other)]
        assert changed.check(changed.base, changed.head).returncode == 1, other
    GitHub.on[changed.head] = [a_status("failure", by=smoke.WRITER)]
    red = changed.check(changed.base, changed.head, no_server(changed.mark(changed.head)))
    assert red.returncode == 1
    assert "the smoke run of the server did not pass on the head commit" in red.stdout


def test_the_status_counts_for_the_head_commit_only(changed, monkeypatch, capsys):
    GitHub.on[changed.head] = [a_status()]
    # A later commit that leaves the text as it is has no status of its own.
    later = changed.commit(one=recording(system=NEW, note="a note in another wording"))
    assert changed.mark(later) == changed.mark(changed.head)
    status, printed = changed.judged(monkeypatch, capsys, changed.base, later)
    assert status == 1
    assert NO_STATUS in printed
    assert changed.judged(monkeypatch, capsys, changed.base, changed.head)[0] == 0
    # A short name of the commit is the same commit: the full id is asked.
    assert changed.judged(monkeypatch, capsys, changed.base, changed.head[:12])[0] == 0
    assert GitHub.seen[-1][0].startswith(f"/repos/inferstep/ATLAS/commits/{changed.head}/statuses")


@pytest.mark.parametrize("state", ["failure", "error", "pending", "", "Success", None])
def test_a_status_of_the_server_that_is_not_green_is_red_and_the_line_for_the_server_does_not_pass_over_it(changed, monkeypatch, capsys, state):
    said = "failed; started 2026-10-08T08:00:03Z; 2 of 3 sessions with no harness defect; text 0123456789ab"
    GitHub.on[changed.head] = [a_status(state, said=said)]
    for text in ("no line", no_server(changed.mark(changed.head))):
        status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head, text)
        assert status == 1, printed
        assert "::error title=smoke result::the smoke run of the server did not pass on the head commit" in printed
        assert f"`{said}`" in printed
        assert "the line for a server that is not available does not set it aside: the change was tried" in printed
        assert "No text is changed to make the smoke pass; after two red smoke runs the change stops and is thought over" in printed
        assert "A new commit needs a new smoke run." in printed


@pytest.mark.parametrize("statuses, green", [
    # The newest status of the server decides.
    ([a_status("success", created_at="2026-10-08T09:00:00Z", id=2), a_status("failure", created_at="2026-10-08T08:00:00Z", id=1)], True),
    ([a_status("failure", created_at="2026-10-08T09:00:00Z", id=2), a_status("success", created_at="2026-10-08T08:00:00Z", id=1)], False),
    # In whatever order GitHub gives them.
    ([a_status("success", created_at="2026-10-08T08:00:00Z", id=1), a_status("failure", created_at="2026-10-08T09:00:00Z", id=2)], False),
    ([a_status("failure", created_at="2026-10-08T08:00:00Z", id=1), a_status("success", created_at="2026-10-08T09:00:00Z", id=2)], True),
    ([a_status("success", created_at="2026-10-08T09:00:00Z", id=7), a_status("failure", created_at="2026-10-08T09:00:00Z", id=8)], False),
    # A newer status of another account, or of another name, is not a status of the smoke run of the server.
    ([a_status("failure", by=999, created_at="2026-10-08T09:00:00Z", id=2), a_status("success", created_at="2026-10-08T08:00:00Z", id=1)], True),
    ([a_status("success", by=999, created_at="2026-10-08T09:00:00Z", id=2), a_status("failure", created_at="2026-10-08T08:00:00Z", id=1)], False),
    ([a_status("failure", name="server/nightly", created_at="2026-10-08T09:00:00Z", id=2), a_status("success", created_at="2026-10-08T08:00:00Z", id=1)], True),
])
def test_the_newest_status_that_the_server_wrote_decides(changed, monkeypatch, capsys, statuses, green):
    GitHub.on[changed.head] = statuses
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
    assert status == (0 if green else 1), printed


@pytest.mark.parametrize("other", [
    a_status(by=999), a_status(by=str(SERVER)), a_status(by=None), a_status(by=SERVER + 1), a_status(by=True),
    {"context": "server/smoke", "state": "success", "description": "passed", "creator": None},
    {"context": "server/smoke", "state": "success", "description": "passed"},
    {"context": "server/smoke", "state": "success", "description": "passed", "creator": "an-account"},
])
def test_a_status_of_that_name_by_another_writer_counts_for_nothing_and_the_output_names_it(changed, monkeypatch, capsys, other):
    GitHub.on[changed.head] = [other]
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
    assert status == 1, printed
    assert NO_STATUS in printed
    assert "A status `server/smoke` by another account (id " in printed
    assert "is on the commit; it is not counted." in printed
    # It does not turn a green status of the server red either, and the output still names it.
    GitHub.on[changed.head] = [{**other, "state": "failure"}, a_status()]
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
    assert status == 0, printed
    assert "is on the commit; it is not counted." in printed
    # And the line for a server that is not available still counts beside it: the server wrote nothing.
    GitHub.on[changed.head] = [{**other, "state": "failure"}]
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head, no_server(changed.mark(changed.head)))
    assert status == 0, printed
    assert "is on the commit; it is not counted." in printed


def test_the_account_of_another_writer_is_named_by_its_id_in_plain_letters(changed, monkeypatch, capsys):
    GitHub.on[changed.head] = [a_status(by=999), a_status(by="<b>@someone</b>\n::error::x")]
    _status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
    assert "A status `server/smoke` by another account (id 999, not given) is on the commit; it is not counted." in printed
    assert "@someone" not in printed
    assert "::error::x" not in printed
    assert printed.count("\n") == 1


def test_a_typed_line_that_says_a_smoke_run_passed_is_not_read(changed):
    mark = changed.mark(changed.head)
    done = changed.check(changed.base, changed.head, "What this changes.\n\n" + line(changed.head, mark) + "\n")
    assert done.returncode == 1
    assert NO_STATUS in done.stdout
    assert "A line in the text of the pull request that says a smoke run passed is not read: the result comes from the server." in done.stdout
    # Without such a line the output does not speak of one.
    assert "is not read" not in changed.check(changed.base, changed.head, "What this changes.").stdout


def test_while_no_account_is_set_as_the_writer_no_status_is_counted_and_the_output_says_so(changed, monkeypatch, capsys):
    GitHub.on[changed.head] = [a_status(by=0), a_status(by=SERVER)]
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head, writer=0)
    assert status == 1
    assert NO_STATUS in printed
    assert "No account is set as the writer of the server's statuses (WRITER in scripts/smoke_result.py), so no status is counted." in printed
    assert isinstance(smoke.WRITER, int)
    assert not isinstance(smoke.WRITER, bool)


def test_when_the_server_is_not_available_one_line_says_so_and_the_check_is_green(changed):
    mark = changed.mark(changed.head)
    red = changed.check(changed.base, changed.head, "no line")
    assert red.returncode == 1, "the message gives the line to write, with the mark"
    assert no_server(mark) in red.stdout, "the message gives the line to write, with the mark"
    done = changed.check(changed.base, changed.head, "Some text.\n\n" + no_server(mark) + "\n")
    assert done.returncode == 0, done.stdout + done.stderr
    assert "no smoke run was made for this change of the system prompt: the server was not available" in done.stdout
    assert "The change was not tried with a real model" in done.stdout
    assert "replay tests are the check" not in red.stdout + done.stdout


@pytest.mark.parametrize("form", [
    "Some text.\r\n\r\n{line}\r\n\r\nMore text.\r\n",   # the line in the middle, as the web form of GitHub sends a text
    "Some text.\r\n\r\n{line}\r\n",                       # the line last, with a line end after it
    "Some text.\r\n\r\n{line}",                            # the line last, with nothing after it
    "{line}\r\nMore text.\r\n",                            # the line first
    "Some text.\r\n{line} \t\r\nMore text.",                # blanks before the line end
    "Some text.\n\n{line}\n\nMore text.\n",                 # the same with the line ends of a program
])
def test_the_line_counts_with_the_line_ends_of_githubs_web_form(changed, form):
    # A text that a person types in the browser has CR LF at the end of each line. The line counted only as the
    # very last thing of such a text, so the check was red for a maintainer who had written the line.
    done = changed.check(changed.base, changed.head, form.format(line=no_server(changed.mark(changed.head))))
    assert done.returncode == 0, done.stdout + done.stderr


@pytest.mark.parametrize("form", [
    "Some text.\r\n> {line}\r\n",          # quoted
    "Some text.\r\n{line} and more\r\n",   # more words behind it on the line
    "Some text.\r\n{line}\rmore\r\n",      # a CR in the middle of a line is no line end
    "Some text.\r\n{line}\r\r\n",          # two of them
])
def test_a_line_that_is_not_alone_on_its_line_does_not_count_with_those_line_ends_either(changed, form):
    done = changed.check(changed.base, changed.head, form.format(line=no_server(changed.mark(changed.head))))
    assert done.returncode == 1, done.stdout + done.stderr


def test_the_line_for_a_server_that_is_not_available_holds_only_for_the_text_it_names(repo):
    first = repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    text = no_server(repo.mark(first))
    again = NEW + " Use them with care."
    head = repo.commit(one=recording(system=again), two=recording(system=again, task="repair a fault"))
    assert repo.check(repo.base, head, text).returncode == 1
    assert repo.check(repo.base, head, "No smoke run: the server is not available.").returncode == 1
    earlier_words = ("No smoke run: the server is not available; the replay tests are the check, and the first nightly run after "
                     f"the server is back covers this text (text {repo.mark(head)}).")
    assert repo.check(repo.base, head, earlier_words).returncode == 1
    assert repo.check(repo.base, head, "> " + no_server(repo.mark(head))).returncode == 1
    assert repo.check(repo.base, head, no_server(repo.mark(head))).returncode == 0


# --- statuses that cannot be read ---------------------------------------------------------------------------------

@pytest.mark.parametrize("answer, why", [
    ((403, {"message": "rate limit"}), "GitHub answered 403"),
    ((500, {}), "GitHub answered 500"),
    ((404, {}), "GitHub answered 404"),
    ((200, b"this is not JSON"), "GitHub gave no answer that can be read"),
    ((200, {"message": "a text"}), "GitHub gave an answer of another shape"),
    ((200, ["a text"]), "GitHub gave an answer of another shape"),
    ((200, None), "GitHub gave an answer of another shape"),
])
def test_statuses_that_cannot_be_read_end_the_check_with_status_2_and_nothing_is_judged(changed, answer, why):
    GitHub.answer = answer
    # Not green and not the plain red: also the line for a server that is not available is not judged.
    for text in ("no line", no_server(changed.mark(changed.head))):
        done = changed.check(changed.base, changed.head, text)
        assert done.returncode == 2, done.stdout + done.stderr
        assert f"::error title=smoke result::the statuses of the head commit cannot be read, so nothing was judged: {why}. Fix: start this job again" in done.stderr
        assert done.stdout == ""
        assert TOKEN not in done.stderr


def test_an_answer_that_is_cut_short_ends_the_check_with_status_2_too(changed):
    GitHub.on[changed.head] = [a_status()]
    GitHub.cut_short = True
    done = changed.check(changed.base, changed.head)
    assert done.returncode == 2, done.stdout + done.stderr
    assert "the statuses of the head commit cannot be read, so nothing was judged: GitHub gave no answer that can be read" in done.stderr
    assert "Traceback" not in done.stderr


def test_with_no_stand_in_at_githubs_address_nothing_is_judged(changed):
    (changed.home / "site" / "sitecustomize.py").write_text(ON_EVERY_START.format(log=str(changed.home / "asked.log"), port=0))
    done = changed.check(changed.base, changed.head, no_server(changed.mark(changed.head)))
    assert done.returncode == 2
    assert "the statuses of the head commit cannot be read, so nothing was judged: GitHub gave no answer that can be read" in done.stderr


@pytest.mark.parametrize("head", ["", "main", "1" * 39, "1" * 41, "A" * 40, "1" * 40 + "/../../other", "../installation", "1" * 40 + "?per_page=1"])
def test_a_head_that_is_no_full_commit_id_is_not_asked_of_github(head, monkeypatch):
    # The commit goes into the path of the call. Whatever the caller gives, the call has the one path or is not made.
    made = []
    monkeypatch.setattr(smoke.urllib.request, "build_opener", lambda *handlers: made.append(handlers))
    with pytest.raises(smoke.Unreadable, match="the head is not a full commit id, so GitHub was not asked"):
        smoke.statuses_of(head)
    assert made == []


@pytest.mark.parametrize("status", [301, 302, 307, 308])
def test_an_answer_that_points_to_another_place_is_not_followed(changed, status):
    GitHub.answer = (status, {})
    done = changed.check(changed.base, changed.head)
    assert done.returncode == 2
    assert f"GitHub answered {status}" in done.stderr
    assert len(GitHub.seen) == 1
    assert [address for address in changed.asked() if not address.startswith("https://api.github.com/")] == []


def test_every_page_of_the_statuses_is_read_and_a_commit_with_too_many_is_not_judged(changed, monkeypatch, capsys):
    others = [a_status(name=f"another/check-{n}", by=999) for n in range(250)]
    GitHub.on[changed.head] = others + [a_status()]
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
    assert status == 0, printed
    assert [path.rsplit("=", 1)[1] for path, _token in GitHub.seen] == ["1", "2", "3"]
    GitHub.on[changed.head] = others * 4 + [a_status()]
    GitHub.seen.clear()
    status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
    assert status == 2
    assert "the statuses of the head commit cannot be read, so nothing was judged: the commit has more than 1000 statuses" in printed
    assert len(GitHub.seen) == 10


def test_the_check_runs_without_a_token_too(changed):
    done = subprocess.run([sys.executable, str(SCRIPT), "--base", changed.base, "--head", changed.head], cwd=changed.root, capture_output=True,
                          text=True, timeout=60, check=False, env={key: value for key, value in changed.env().items() if key != "GITHUB_TOKEN"})
    assert done.returncode == 1
    assert GitHub.seen == [(f"/repos/inferstep/ATLAS/commits/{changed.head}/statuses?per_page=100&page=1", "")]


def test_the_script_has_one_address_of_github_and_reads_two_variables_and_no_other():
    source = SCRIPT.read_text(encoding="utf-8")
    assert re.findall(r"https?://[^\s\"')]+", source) == ["https://api.github.com"]
    assert smoke.GITHUB_API == "https://api.github.com"
    assert smoke.REPOSITORY == "inferstep/ATLAS"
    assert smoke.STATUS == "server/smoke"
    assert sorted(re.findall(r"os\.environ\.get\(\"(\w+)\"", source)) == ["GITHUB_TOKEN", "PR_BODY"]
    assert len(re.findall(r"os\.environ|getenv", source)) == 2
    assert source.count("urllib.request.Request(") == 1
    assert "urlopen" not in source


def test_the_mark_is_made_from_the_text_that_every_request_carries_and_from_nothing_else(repo):
    same = repo.commit(one=recording(note="another note"), three=recording(task="a third case"))
    assert repo.mark(same) == repo.mark(repo.base)
    for change in ({"system": NEW}, {"grammar": "root ::= other"}, {"schema": "array"}):
        other = repo.commit(one=recording(**change), two=recording(task="repair a fault", **change), three=None)
        assert repo.mark(other) != repo.mark(repo.base), change
    assert len(repo.mark(repo.base)) == 12


# --- what cannot be read ------------------------------------------------------------------------------------------

@pytest.mark.parametrize("content", ["not json", '{"exchanges": [{"service": "model", "path": "/v1/chat/completions", "request": "not json"}]}'])
def test_a_recording_that_cannot_be_read_ends_the_check_with_status_2_and_nothing_is_judged(repo, content):
    done = repo.check(repo.base, repo.commit(one=content))
    assert done.returncode == 2
    assert "the recordings cannot be read, so nothing was judged" in done.stderr
    assert "one.json" in done.stderr


def test_a_head_with_no_recording_at_all_ends_the_check_with_status_2(repo):
    head = repo.commit(one=None, two=None)
    done = repo.check(repo.base, head, no_server("0" * 12))
    assert done.returncode == 2
    assert "has no recording under tests/replay/recordings/, so nothing shows what its requests carry" in done.stderr
    assert "Fix: keep the recordings." in done.stderr
    assert "changes no text" not in done.stdout + done.stderr
    marked = subprocess.run([sys.executable, str(SCRIPT), "--mark", head], cwd=repo.root, capture_output=True, text=True, timeout=60)
    assert marked.returncode == 2


def test_a_commit_that_is_not_there_ends_the_check_with_status_2(repo):
    done = repo.check(repo.base, "0" * 40)
    assert done.returncode == 2
    assert "Fix: the checkout of this job needs the base commit and the head commit" in done.stderr


# --- the recordings of this repository, and the workflow ----------------------------------------------------------

def test_every_recording_of_this_repository_has_the_parts_that_the_check_reads():
    recordings = sorted((ROOT / "tests" / "replay" / "recordings").glob("*.json"))
    assert recordings
    for path in recordings:
        parts = smoke.carried_by_every_request(json.loads(path.read_text(encoding="utf-8")))
        assert set(parts) == set(smoke.PARTS), path.name
        for part in ("the system prompt", "the schema of the reply"):
            assert parts[part], (
                f"{path.name}: the first request to the model has no value for {part}, so the check would not see a "
                "change of it. Fix: PARTS and `carried_by_every_request` in scripts/smoke_result.py.")


def test_the_recordings_of_this_repository_against_themselves_are_no_change():
    texts = {part: set() for part in smoke.PARTS}
    for path in sorted((ROOT / "tests" / "replay" / "recordings").glob("*.json")):
        for part, value in smoke.carried_by_every_request(json.loads(path.read_text(encoding="utf-8"))).items():
            if value is not None:
                texts[part].add(json.dumps(value, sort_keys=True))
    as_read = {part: sorted(values) for part, values in texts.items()}
    assert smoke.changed_for_every_request(as_read, as_read) == []


def step(name):
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    return next(step for step in workflow["jobs"]["smoke-result"]["steps"] if name in str(step.get("run")) + str(step.get("name")))


def test_the_job_reads_the_text_of_the_pull_request_from_the_event_and_never_as_part_of_a_command():
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    judge = step("smoke_result.py")
    assert judge["env"] == {"BASE": "${{ steps.base.outputs.sha }}", "PR_BODY": "${{ github.event.pull_request.body }}",
                            "GITHUB_TOKEN": "${{ github.token }}"}
    for one in workflow["jobs"]["smoke-result"]["steps"]:
        assert "github.event" not in str(one.get("run")), "a value of the event stands in a command; give it through `env:`"
    # `on` is read as the boolean True by the YAML reader.
    assert "edited" in workflow[True]["pull_request"]["types"], "without `edited` the job does not see the line for the server come into the text"
    assert workflow["permissions"] == {"contents": "read"}
    assert "concurrency" not in workflow


def test_the_job_can_read_the_statuses_of_a_commit_and_write_nothing():
    workflow = yaml.safe_load(WORKFLOW.read_text(encoding="utf-8"))
    (job,) = workflow["jobs"].values()
    assert job["permissions"] == {"contents": "read", "statuses": "read"}
    with_the_token = [one for one in job["steps"] if "token" in str(one).lower().replace("persist-credentials", "")]
    assert with_the_token == [step("smoke_result.py")], "only the step that runs the script has the token"
    assert "secrets." not in WORKFLOW.read_text(encoding="utf-8")
    assert "pull_request_target" not in WORKFLOW.read_text(encoding="utf-8")


def in_a_merge(tmp_path, on_the_base, on_the_branch, text):
    """Run the commands of the job's step in the merge of a branch into a base, each with its own copy of the script."""
    repo = Repo(tmp_path / "repo")
    (repo.root / "scripts").mkdir()
    if on_the_base is not None:
        (repo.root / "scripts" / "smoke_result.py").write_text(on_the_base, encoding="utf-8")
    base = repo.commit(one=recording(), two=recording(task="repair a fault"))
    repo.git("checkout", "-q", "-b", "work")
    (repo.root / "scripts" / "smoke_result.py").write_text(on_the_branch, encoding="utf-8")
    repo.commit(one=recording(system=NEW), two=recording(system=NEW, task="repair a fault"))
    repo.git("checkout", "-q", "--detach", "dev")
    repo.git("merge", "-q", "--no-ff", "--no-edit", "work")
    return run_the_step(repo, tmp_path / "runner", base, text)


def run_the_step(repo, runner, base, text=""):
    """Run the commands of the job's step where the checkout of the repository stands: the merge of a branch into a base."""
    runner.mkdir()
    return subprocess.run(["bash", "-e", "-c", step("smoke_result.py")["run"]], cwd=repo.root, capture_output=True, text=True,
                          timeout=60, check=False, env={**repo.env(text), "PATH": f"{Path(sys.executable).parent}:/usr/bin:/bin",
                                                        "RUNNER_TEMP": str(runner), "BASE": base})


PASSES_ALL = 'print("the copy of the change ran")\n'


def test_the_job_runs_the_base_branchs_copy_of_the_script_when_a_change_rewrites_the_script(tmp_path):
    # The change alters the text of every request and brings a script that passes everything.
    done = in_a_merge(tmp_path, SCRIPT.read_text(encoding="utf-8"), PASSES_ALL, "no word about a smoke run")
    assert done.returncode == 1, done.stdout + done.stderr
    assert "::error title=smoke result::this pull request changes the system prompt" in done.stdout
    assert "the copy of the change ran" not in done.stdout


def test_the_changes_copy_of_the_script_runs_only_while_the_base_has_none(tmp_path):
    done = in_a_merge(tmp_path, None, PASSES_ALL, "")
    assert done.returncode == 0
    assert "the copy of the change ran" in done.stdout


def test_a_branch_made_before_the_recordings_is_judged_by_what_its_merge_would_bring(tmp_path):
    # The branch was made on a base with no recording yet (as `main` has none), and its change touches no recording.
    repo = Repo(tmp_path / "repo")
    (repo.root / "scripts").mkdir()
    (repo.root / "scripts" / "smoke_result.py").write_text(SCRIPT.read_text(encoding="utf-8"), encoding="utf-8")
    repo.commit()
    repo.git("checkout", "-q", "-b", "work")
    (repo.root / "a change.txt").write_text("a change that touches no recording\n", encoding="utf-8")
    repo.git("add", "-A")
    repo.git("commit", "-q", "-m", "the change")
    repo.git("checkout", "-q", "dev")
    base = repo.commit(one=recording(), two=recording(task="repair a fault"))
    repo.git("checkout", "-q", "--detach", "dev")
    repo.git("merge", "-q", "--no-ff", "--no-edit", "work")
    done = run_the_step(repo, tmp_path / "runner", base)
    assert done.returncode == 0, done.stdout + done.stderr
    assert "the smoke run is not asked for" in done.stdout


def test_the_gates_page_has_the_words_of_the_rule():
    page = " ".join((ROOT / "docs" / "quality" / "gates.md").read_text(encoding="utf-8").split())
    for sentence in (
        ("The smoke result says that a session still runs with the new text: it starts, the model's replies are read, the "
         "tool calls run, it ends. It is a yes or a no, with the seconds. It is not a score."),
        ("A pass does not show that the model behaves as well as before. A claim about behaviour needs a measurement of its "
         "own: two builds, enough sessions, the affected sessions shown from the diff, and the rule written down before "
         "the run."),
        ("The three tasks are fixed and are the driver's own. They are never taken from held-out data, and no product text "
         "names them."),
        "No text is changed to make the smoke pass. After two red smoke runs the change stops and is thought over.",
        ("When the server is not available, the pull request says so in one line. The change is then not tried with a real "
         "model before it merges; the first nightly run after the server is back covers it."),
        ("The replay tests are no check of such a change: its recordings are written again from the new text, so they "
         "pass."),
        "Not tried with a real model; the first nightly run after the server is back covers this text",
        "The job is green when the newest `server/smoke` status of the server on the head commit is green.",
        ("It counts only a status that the server's own account wrote, and it compares the id of that account, not a name. "
         "A status of that name by another account counts for nothing, and the output of the job names it."),
        ("A red status of the server is red. The line for a server that is not available does not set it aside: the "
         "change was tried."),
        "A line in the text of the pull request that says a smoke run passed is not read.",
        "A status that comes later does not start the job again.",
        "The line counts only while the server wrote no status for the head commit.",
    ):
        assert sentence in page, f"docs/quality/gates.md no longer has: {sentence!r}"


def test_the_text_of_a_status_is_printed_in_plain_letters_as_code(changed, monkeypatch, capsys):
    # The check prints the text of a status that the server's account wrote. Also that text is data.
    said = "passed`\n::error title=x::<b>@someone</b> #12 [a](https://example.invalid) " + "x" * 200
    for state in ("success", "failure\n::warning::y"):
        GitHub.on[changed.head] = [a_status(state, said=said)]
        _status, printed = changed.judged(monkeypatch, capsys, changed.base, changed.head)
        assert printed.count("\n") == 1, printed
        # One line, so nothing in it starts a line as a command of the job does.
        for word in ("@someone", "<b>", "#12", "[a]", "title=x", "//"):
            assert word not in printed, word
        shown = re.findall(r"`([^`]*)`", printed)
        assert len(shown) == (1 if state == "success" else 2), printed
        for text in shown:
            assert re.fullmatch(r"[A-Za-z0-9 ,.:;()'?-]{1,140}", text), text
        assert shown[-1].startswith("passed??::error title?x::?b??someone??b? ?12 ?a?(https:??example.invalid) xxx")
        assert len(shown[-1]) == 140
