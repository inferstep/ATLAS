#!/usr/bin/env python3
"""The statuses of the nightly run on GitHub: kept in the run's folder, then sent with a token of the app.

The file beside this one, scripts/nightly_run.py, loads it. It starts nothing by itself. The run decides which
status a report gets (`status_of`); this file keeps that status in a folder of the run's own and sends what waits.

A status is sent with a token that is made for one send, for this one repository and for statuses only, and kept
nowhere. openssl signs with the app's key and gets the key as the path of its file: no line here reads the key. A
status that cannot go out now waits for the next start, for up to KEEP_TRYING_DAYS. Sending never stops a run, its
report or a look of the timer.
"""
from __future__ import annotations

import base64
import datetime as dt
import http.client
import json
import os
import re
import stat
import subprocess
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

REPOSITORY, GITHUB_API = "inferstep/ATLAS", "https://api.github.com"
# The status that a run writes on the commit that ran, by the kind of run. Each name starts with `server/`: no
# required check has such a name.
STATUS = {"night": "server/nightly", "one commit": "server/smoke"}
# Where a status waits until it is sent, where it lies when it is sent, and where one goes that was tried for too
# long. All three are folders of the run's own folder.
WAITING, SENT, GIVEN_UP, KEEP_TRYING_DAYS = "to-send", "sent", "not-sent", 14


class NotSent(Exception):
    """GitHub could not be asked, or a status could not be sent now. The words say why, and they are fixed words: no
    path, no name of a machine and nothing that a tool printed."""


class NoRedirect(urllib.request.HTTPRedirectHandler):
    """An answer that points to another address is not followed: the token would go there too."""

    def redirect_request(self, *args, **kwargs):
        return None


# The paths of the calls that this script makes. A value from a file, from an argument or from an answer goes into a
# path only as letters, digits and the few signs of a commit id, a number or an encoded name of a branch.
PATHS = re.compile(rf"/repos/{re.escape(REPOSITORY)}/(?:installation|statuses/[0-9a-f]{{40}}|commits/[0-9a-f]{{40}}/(?:statuses|pulls)\?per_page=100"
                   r"|activity\?ref=[A-Za-z0-9._%-]+&per_page=5)|/app/installations/[0-9]+/access_tokens")


def github(method: str, path: str, bearer: str = "", body: dict | None = None):
    """One call to GitHub's own address, and the decoded answer. A call that reads sends no key: the repository is public."""
    if not PATHS.fullmatch(path):
        raise NotSent("the call was not made: its path is not one that this script makes")
    headers = {"Accept": "application/vnd.github+json", "X-GitHub-Api-Version": "2022-11-28", "User-Agent": "atlas-nightly",
               **({"Authorization": f"Bearer {bearer}"} if bearer else {})}
    request = urllib.request.Request(f"{GITHUB_API}{path}", data=json.dumps(body).encode() if body is not None else None,
                                     method=method, headers=headers)
    try:
        # No proxy of the environment either: no variable changes where the call goes.
        with urllib.request.build_opener(NoRedirect, urllib.request.ProxyHandler({})).open(request, timeout=30) as response:
            answer = response.read()
    except urllib.error.HTTPError as error:
        raise NotSent(f"GitHub answered {error.code}") from None
    except (http.client.HTTPException, OSError):
        # A URLError and a timeout are an OSError too.
        raise NotSent("GitHub could not be reached") from None
    try:
        return json.loads(answer)
    except ValueError:
        return None


def key_fault(args) -> str:
    """Why the key cannot be used, or nothing. It is a file of the user of this run that nobody else can read, and
    it lies outside the folder of the run, so no container of the stack can have it."""
    if args.status_key is None or not args.status_app:
        return "no key: the run was started without --status-key or without --status-app"
    try:
        about = args.status_key.stat()
    except OSError:
        return "no key: the file that --status-key names is not there"
    if not stat.S_ISREG(about.st_mode) or about.st_uid != os.getuid():
        return "the key is not used: it is not a file of the user of this run"
    if about.st_mode & 0o077:
        return "the key is not used: its file is open to the group or to others. Fix: chmod 600 on that file"
    if args.dir.resolve() in args.status_key.resolve().parents:
        return ("the key is not used: its file lies in the folder of the run, which the containers of the stack can "
                "read. Fix: move it out of that folder")
    return ""


def base64url(raw: bytes) -> bytes:
    return base64.urlsafe_b64encode(raw).rstrip(b"=")


def token_for_statuses(args) -> str:
    """A token of the app, made for one send: for this one repository and for statuses only. It is kept nowhere.

    openssl signs with the key and gets the key as the path of its file. This script never reads the key.
    """
    now = int(time.time())
    unsigned = b".".join(base64url(json.dumps(part).encode()) for part in (
        {"alg": "RS256", "typ": "JWT"}, {"iat": now - 60, "exp": now + 300, "iss": str(args.status_app)}))
    try:
        done = subprocess.run(["openssl", "dgst", "-sha256", "-sign", str(args.status_key)], input=unsigned,
                              capture_output=True, timeout=30, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise NotSent("openssl could not be started, or did not end") from None
    if done.returncode != 0 or not done.stdout:
        raise NotSent(f"openssl could not sign with the key (status {done.returncode})")
    proof = (unsigned + b"." + base64url(done.stdout)).decode()
    try:
        installation = github("GET", f"/repos/{REPOSITORY}/installation", proof)
        token = github("POST", f"/app/installations/{int(installation['id'])}/access_tokens", proof,
                       {"repositories": [REPOSITORY.split("/")[1]], "permissions": {"statuses": "write"}})["token"]
    except (KeyError, TypeError, ValueError):
        token = None
    if not isinstance(token, str) or not token:
        raise NotSent("GitHub gave an answer of another shape")
    return token


def is_ours(status) -> bool:
    """Whether a waiting file holds a status as this script writes it. Another file is not sent."""
    return (isinstance(status, dict) and status.get("context") in STATUS.values() and status.get("state") in ("success", "failure")
            and bool(re.fullmatch(r"[0-9a-f]{40}", str(status.get("sha")))) and isinstance(status.get("description"), str)
            and len(status["description"]) <= 140)


def not_the_runs_own(about: os.stat_result) -> bool:
    """Whether a file or a folder is not the run's own: another user owns it, or the group or others can write it."""
    return about.st_uid != os.getuid() or bool(about.st_mode & 0o022)


def folder_fault(args) -> str:
    """Why the folder of the waiting statuses cannot be used, or nothing. As for the key: it is a folder of the user
    of this run, and nobody else can write into it. What lies in another folder is not sent."""
    try:
        about = (args.dir / WAITING).lstat()
    except OSError:
        return ""
    if not stat.S_ISDIR(about.st_mode) or not_the_runs_own(about):
        return ("the folder of the waiting statuses is not a folder that only the user of this run can write. Fix: "
                "look at what lies in it; then chmod go-w on that folder")
    return ""


def keep_for_sending(args, status: dict, stamp: str) -> None:
    (args.dir / WAITING).mkdir(mode=0o700, exist_ok=True)
    if folder_fault(args):
        return
    place = args.dir / WAITING / f"{stamp}-{status['context'].replace('/', '-')}.json"
    # The file is made for the user of the run alone, whatever the settings of the server give a new file.
    with os.fdopen(os.open(place, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600), "w", encoding="utf-8") as out:
        out.write(json.dumps(status, indent=2) + "\n")


def set_aside(args, path: Path, folder: str) -> None:
    (args.dir / folder).mkdir(mode=0o700, exist_ok=True)
    path.replace(args.dir / folder / path.name)


def waiting_status(path: Path, now: dt.datetime) -> dict | None:
    """The status that a waiting file holds, or None when the file is not one to send: this script did not write it,
    it is not a file of the run's own, or it has waited for longer than KEEP_TRYING_DAYS."""
    try:
        about = path.lstat()
        status = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    if not stat.S_ISREG(about.st_mode) or not_the_runs_own(about):
        return None
    too_old = now.timestamp() - about.st_mtime > KEEP_TRYING_DAYS * 86400
    return status if is_ours(status) and not too_old else None


def send_one(args, status: dict) -> str:
    """Send one status, with a token made for it. Gives a note when the account that wrote it is not the one that
    the settings name, and else nothing. Only the name, the state and the text of the status are sent."""
    made = github("POST", f"/repos/{REPOSITORY}/statuses/{urllib.parse.quote(str(status['sha']), safe='')}", token_for_statuses(args),
                  {key: status[key] for key in ("state", "context", "description")})
    creator = made.get("creator") if isinstance(made, dict) else None
    writer = creator.get("id") if isinstance(creator, dict) else None
    if not isinstance(writer, int) or writer == args.status_writer:
        return ""
    return (f"; the statuses of this key are written by the account {writer}, and --status-writer names "
            f"{args.status_writer or 'none'}. Fix: give --status-writer {writer}")


def send_waiting(args, now: dt.datetime) -> str:
    """Try once to send each status that waits. Gives what happened, in fixed words.

    A status that was sent is moved to the folder of the sent ones, so it is sent once. One that has waited for
    longer than KEEP_TRYING_DAYS, or that this script did not write, is moved aside and tried no more. When GitHub
    does not answer, the others wait for the next start too.
    """
    waiting = sorted((args.dir / WAITING).glob("*.json")) if (args.dir / WAITING).is_dir() else []
    if folder_fault(args):
        # Nothing of such a folder is sent, and nothing of it is moved.
        return f"not sent: {folder_fault(args)}; the status of a run is then in its report only"
    fault, sent, aside, other = key_fault(args), 0, 0, ""
    for path in waiting:
        status = waiting_status(path, now)
        if status is None:
            set_aside(args, path, GIVEN_UP)
            aside += 1
        elif not fault:
            try:
                other = send_one(args, status) or other
            except NotSent as reason:
                fault = str(reason)
                continue
            set_aside(args, path, SENT)
            sent += 1
    left = len(waiting) - sent - aside
    said = [f"sent: {sent}"] if sent else []
    said += [f"not sent: {fault}; waits: {left}"] if left else []
    said += [f"set aside after {KEEP_TRYING_DAYS} days or as not written by this script: {aside}"] if aside else []
    return ("; ".join(said) or "nothing waits") + other


def send_what_waits(args, status: dict | None = None, stamp: str = "") -> str:
    """Keep the status of a run for sending, and try once to send each status that waits. Gives what happened.

    A fault of this step itself is given as words too: sending never stops a run, its report or a look of the timer.
    """
    try:
        if status:
            keep_for_sending(args, status, stamp)
        return send_waiting(args, dt.datetime.now(dt.timezone.utc))
    except Exception as error:  # the caller says so, and goes on
        return f"not sent: an error of the sending step itself ({type(error).__name__})"
