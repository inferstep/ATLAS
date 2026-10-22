#!/usr/bin/env python3
"""One queue for the heavy runs on the development server.

Every heavy run takes one lock: the nightly run, the smoke run, and each development job (a planted-fault run,
a full test suite). The lock is a flock on <queue>/lock, so a run that crashes frees it by itself. The holder
writes a note beside it, <queue>/holder.json: what runs, its command, its process group, since when, when it
expects to end, and whether it yields. The nightly signals a holder's group only when that group's leader still
runs the noted command, and never a group of 1 or less or its own: a wrong note cannot stop another process.

The order is the nightly, then the smoke, then development jobs:
  - The nightly: when the lock is held, it asks for the queue (it writes <queue>/yield, with the time), so
    that no job takes the lock before it. A job that yields and holds the lock gets up to 20 minutes; then
    the nightly stops the holder's process group itself with SIGTERM, waits up to 2 more minutes, and runs.
    It never stops a smoke run, which is short and bounded. Its request stands until it ends; a request older
    than 12 hours comes from a nightly run that crashed, and is ignored.
  - The smoke: it waits for the lock as many minutes as it is given, and then gives up until its next look.
  - A development job: it waits for the lock, and for no request of the nightly to stand, and never stops
    another run. It runs with nice, and it stops its own command with SIGTERM when the nightly asks it to
    yield, when the free space of "/" (Docker's data) falls under its floor, or when it gets SIGTERM, SIGHUP
    (a dropped ssh) or SIGINT itself. A planted-fault run then ends cleanly: it removes its containers and its copy, and
    writes the records it did not run.

nice reaches only the job's own process, not the containers it starts: Docker runs those, and their limit is
each container's --cpus. A job is given ATLAS_QUEUE_CPUS (6 of the server's 8) in its environment, for the
limits of its own containers.

usage: atlas_queue.py <nightly|smoke|dev> [options] -- <command> [arguments...]
  --queue DIR            the queue's folder (default: ~/atlas-queue)
  --what TEXT            what runs, for the note (default: the command)
  --expected-minutes N   how long it expects to run, for the note
  --wait-minutes N       smoke: how long it waits for the lock (default: no limit)
  --floor-gib N          dev: the least free space it leaves on "/" (default 6: the nightly's 5 and 1)
  --poll-seconds N, --yield-wait-minutes N, --stop-wait-minutes N   the waits (defaults 5, 20, 2)
The status is the command's, or 75 when the command did not start (the lock stayed held, or "/" was too full).
The nightly gives its command ATLAS_QUEUE_LATE_MINUTES, the minutes it waited for the lock, for its report.
"""
from __future__ import annotations

import argparse
import fcntl
import json
import os
import shutil
import signal
import subprocess
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

NOT_STARTED = 75
JOB_CPUS = 6
STOPPING_SIGNALS = (signal.SIGTERM, signal.SIGHUP, signal.SIGINT)
YIELD_REQUEST_LIFETIME = timedelta(hours=12)


def now() -> datetime:
    return datetime.now(timezone.utc)


def try_to_take(lock_path: Path):
    """The open lock file when this process now holds the lock, or None when another run holds it."""
    handle = open(lock_path, "a", encoding="utf-8")
    try:
        fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError:
        handle.close()
        return None
    return handle


def a_yield_request_stands(queue: Path) -> bool:
    """Whether the nightly's request for the queue stands. One older than 12 hours is from a crashed nightly."""
    try:
        asked = datetime.fromisoformat((queue / "yield").read_text(encoding="utf-8").strip())
    except (OSError, ValueError):
        return False
    return now() - asked < YIELD_REQUEST_LIFETIME


def read_note(queue: Path) -> dict:
    try:
        return json.loads((queue / "holder.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return {}


def write_note(queue: Path, what: str, command: list, process_group: int, expected_minutes: float | None,
               yields: bool) -> None:
    since = now()
    note = {"what": what, "command": command, "process_group": process_group,
            "since": since.isoformat(timespec="seconds"),
            "expected_end": (since + timedelta(minutes=expected_minutes)).isoformat(timespec="seconds")
            if expected_minutes else None, "yields": yields}
    (queue / "holder.json").write_text(json.dumps(note) + "\n", encoding="utf-8")


def leader_command(group: int) -> list | None:
    """The command line of a process group's leader, or None when it cannot be read (no such process)."""
    try:
        raw = Path(f"/proc/{group}/cmdline").read_bytes()
    except OSError:
        return None
    return [part.decode("utf-8", "replace") for part in raw.split(b"\0")[:-1]]


def stop_the_holder(holder: dict, kill=os.killpg) -> str:
    """Send SIGTERM to the holder's process group, unless the note cannot name it safely. Gives what was done."""
    group = holder.get("process_group")
    if not isinstance(group, int) or group <= 1:
        return f"refused to stop process group {group!r}: no job runs in it"
    if group == os.getpgrp():
        return f"refused to stop process group {group}: it is this run's own"
    if leader_command(group) != holder.get("command"):
        return f"refused to stop process group {group}: it does not run the noted command"
    kill(group, signal.SIGTERM)
    return f"stopped process group {group}"


def wait_for_the_lock(queue: Path, role: str, give_up_at: datetime | None, poll_s: float,
                      yield_wait_s: float, stop_wait_s: float, kill=os.killpg):
    """Take the lock, waiting as the role may. The open lock file, or None when the wait gave up."""
    asked_at = stopped_at = None
    while True:
        handle = None if role == "dev" and a_yield_request_stands(queue) else try_to_take(queue / "lock")
        if handle is not None:
            return handle
        if give_up_at is not None and now() >= give_up_at:
            return None
        holder = read_note(queue)
        if role == "nightly" and asked_at is None:
            (queue / "yield").write_text(now().isoformat(timespec="seconds") + "\n", encoding="utf-8")
            asked_at = time.monotonic()
            print(f"the queue is held ({holder.get('what')}): asked for it", flush=True)
        elif role == "nightly" and holder.get("yields"):
            if stopped_at is None and time.monotonic() - asked_at >= yield_wait_s:
                print(f"the job did not yield in time: {stop_the_holder(holder, kill)}", flush=True)
                stopped_at = time.monotonic()
            elif stopped_at is not None and time.monotonic() - stopped_at >= stop_wait_s:
                print("the stopped job still holds the queue: the nightly runs beside it", flush=True)
                return None
        time.sleep(poll_s)


def run_holding_the_queue(queue: Path, command: list, leader: list, what: str, expected_minutes: float | None,
                          yields: bool, poll_s: float, floor_bytes: int | None, environment: dict,
                          holds_the_lock: bool) -> int:
    """Run the command in a process group of its own; the holder of the lock writes the note. A job that yields
    is stopped with SIGTERM when the nightly asks for the queue, or when "/" falls under its floor. The status
    is the command's, or 128 and the signal's number when a signal ended it."""
    child = subprocess.Popen(command, start_new_session=True, env=environment)
    if holds_the_lock:
        write_note(queue, what, leader, child.pid, expected_minutes, yields)
    stopping = False

    def stop(why: str) -> None:
        nonlocal stopping
        if not stopping:
            print(f"stopping the job: {why}", flush=True)
            os.killpg(child.pid, signal.SIGTERM)
            stopping = True

    for signum in STOPPING_SIGNALS:
        signal.signal(signum, lambda *_: stop("this queue run was told to stop"))
    try:
        while child.poll() is None:
            if yields and a_yield_request_stands(queue):
                stop("the nightly run asks for the queue")
            elif floor_bytes is not None and shutil.disk_usage("/").free < floor_bytes:
                stop(f'"/" has less than {floor_bytes / 1024**3:.0f} GiB free')
            time.sleep(poll_s)
    finally:
        if holds_the_lock:
            (queue / "holder.json").unlink(missing_ok=True)
    return child.returncode if child.returncode >= 0 else 128 - child.returncode


def main(argv: list) -> int:
    parser = argparse.ArgumentParser(description="One queue for the heavy runs on the development server.")
    parser.add_argument("role", choices=["nightly", "smoke", "dev"])
    parser.add_argument("--queue", type=Path, default=Path.home() / "atlas-queue")
    parser.add_argument("--what")
    parser.add_argument("--expected-minutes", type=float)
    parser.add_argument("--wait-minutes", type=float)
    parser.add_argument("--floor-gib", type=float, default=6)
    parser.add_argument("--poll-seconds", type=float, default=5)
    parser.add_argument("--yield-wait-minutes", type=float, default=20)
    parser.add_argument("--stop-wait-minutes", type=float, default=2)
    # The options come before "--", and the command after it.
    if "--" not in argv or argv.index("--") == len(argv) - 1:
        parser.error("give the command after --")
    split = argv.index("--")
    args = parser.parse_args(argv[:split])
    command = argv[split + 1:]
    what = args.what or " ".join(command)
    args.queue.mkdir(parents=True, exist_ok=True)
    floor_bytes = int(args.floor_gib * 1024**3) if args.role == "dev" else None
    if floor_bytes is not None and shutil.disk_usage("/").free < floor_bytes:
        print(f'not started: "/" has less than {args.floor_gib:g} GiB free', flush=True)
        return NOT_STARTED

    # A stop that comes while the run waits ends it through the finally below.
    for signum in STOPPING_SIGNALS:
        signal.signal(signum, lambda signum, _frame: sys.exit(128 + signum))
    try:
        return wait_and_run(args, command, what, floor_bytes)
    finally:
        if args.role == "nightly":
            (args.queue / "yield").unlink(missing_ok=True)


def wait_and_run(args, command: list, what: str, floor_bytes: int | None) -> int:
    asked = now()
    give_up_at = None
    if args.role == "smoke" and args.wait_minutes is not None:
        give_up_at = asked + timedelta(minutes=args.wait_minutes)
    handle = wait_for_the_lock(args.queue, args.role, give_up_at, args.poll_seconds,
                               args.yield_wait_minutes * 60, args.stop_wait_minutes * 60)
    if handle is None and args.role != "nightly":
        print(f"not started: the queue stayed held until {now():%H:%M}Z", flush=True)
        return NOT_STARTED

    environment = dict(os.environ)
    # nice runs the command in its own place, so the group's leader runs the command itself.
    leader = command
    if args.role == "dev":
        command = ["nice", "-n", "10", *command]
        environment["ATLAS_QUEUE_CPUS"] = str(JOB_CPUS)
    if args.role == "nightly":
        late = int((now() - asked).total_seconds() // 60)
        environment["ATLAS_QUEUE_LATE_MINUTES"] = str(late)
        if late:
            print(f"started {late} min late (the queue was held)", flush=True)
    try:
        return run_holding_the_queue(args.queue, command, leader, what, args.expected_minutes, args.role == "dev",
                                     args.poll_seconds, floor_bytes, environment, handle is not None)
    finally:
        if handle is not None:
            handle.close()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
