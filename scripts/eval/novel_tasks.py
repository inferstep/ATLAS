"""Freshly authored coding tasks, for measuring a harness against a bare model.

Public benchmarks are contaminated: the served model was very likely trained
on LiveCodeBench and on Advent of Code, and a task it has memorised measures
recall rather than anything the harness does. Our AoC set showed exactly that
shape — the bare model scored 83% one-shot and 100% with retries, leaving no
headroom for a harness to be measured in.

So these are written here. Each family composes two or three ordinary rules
into a combination that is unlikely to appear verbatim anywhere:

  * the statement fully specifies the rules, so a careful reader can solve it
  * a REFERENCE implementation defines truth, so expected answers are computed
    rather than guessed (the failure that made self-generated tests useless)
  * every instance carries a HOLDOUT input the solution never sees, so an
    answer hardcoded from the visible input dies
  * instances are seeded, so a run is reproducible and both arms get exactly
    the same problems

Difficulty is tuned so the bare model lands near 50%. Much higher and a
harness has no room to show a gain; much lower and every failure is the model
rather than anything we built.
"""

from __future__ import annotations

import random
from dataclasses import dataclass
from typing import Callable, List


@dataclass(frozen=True)
class NovelTask:
    """One problem instance: statement, data, and the answers truth demands."""
    name: str
    family: str
    prompt: str
    input_text: str
    holdout_text: str
    expected: str
    holdout_expected: str


# --- families ---------------------------------------------------------------
#
# Each family is (name, statement, make_input(rng), solve(text)).
# solve() is the reference. It is the only definition of a correct answer, so
# it is written for clarity over cleverness.


def _ledger_input(rng: random.Random) -> str:
    # Small denominations so a running balance genuinely returns to zero.
    tags = ["alpha", "bravo", "cargo", "delta", "echo"]
    lines = []
    for _ in range(rng.randint(60, 90)):
        tag = rng.choice(tags)
        sign = rng.choice("+-")
        lines.append(f"{sign}{rng.choice([1, 1, 2, 2, 3, 5])} {tag}")
    return "\n".join(lines) + "\n"


def _ledger_solve(text: str) -> str:
    """A tag SETTLES each time its running balance returns to exactly 0 after
    having been non-zero. The second time a tag settles it FREEZES: every
    later line for that tag is ignored entirely. Answer: total settle events,
    then the tag whose balance reached the largest absolute peak (ties ->
    alphabetically first)."""
    bal, settled, peak, frozen, settles = {}, 0, {}, set(), {}
    for line in text.split("\n"):
        line = line.strip()
        if not line:
            continue
        amount, tag = line.split()
        if tag in frozen:
            continue
        prev = bal.get(tag, 0)
        cur = prev + int(amount)
        bal[tag] = cur
        peak[tag] = max(peak.get(tag, 0), abs(cur))
        if cur == 0 and prev != 0:
            settled += 1
            settles[tag] = settles.get(tag, 0) + 1
            if settles[tag] == 2:
                frozen.add(tag)
    best = sorted(peak.items(), key=lambda kv: (-kv[1], kv[0]))[0][0]
    return f"{settled} {best}"


def _debounce_input(rng: random.Random) -> str:
    lines = []
    t = 0
    state = 0
    for _ in range(rng.randint(80, 120)):
        t += rng.randint(1, 5)
        if rng.random() < 0.35:
            state = 1 - state
        lines.append(f"{t} {state}")
    return "\n".join(lines) + "\n"


def _debounce_solve(text: str) -> str:
    """A reading is STABLE when the same value repeats for 3 consecutive
    readings. A transition counts only between stable runs. Answer: number of
    counted transitions, then the timestamp of the FIRST reading of the run that became the new stable value for the last counted transition (0 if none)."""
    rows = []
    for line in text.split("\n"):
        line = line.strip()
        if line:
            ts, val = line.split()
            rows.append((int(ts), int(val)))
    transitions, last_ts, stable = 0, 0, None
    run_val, run_len, run_start = None, 0, 0
    for ts, val in rows:
        if val == run_val:
            run_len += 1
        else:
            run_val, run_len, run_start = val, 1, ts
        if run_len == 3:
            if stable is not None and stable != run_val:
                transitions += 1
                last_ts = run_start
            stable = run_val
    return f"{transitions} {last_ts}"


def _bucket_input(rng: random.Random) -> str:
    # Bursty: long runs at the same timestamp, so a bucket can actually empty.
    lines = []
    t = 0
    while len(lines) < rng.randint(70, 110):
        who = rng.choice("abcd")
        for _ in range(rng.randint(1, 6)):
            lines.append(f"{t} {who}")
        if rng.random() < 0.4:
            t += rng.randint(1, 3)
    return "\n".join(lines) + "\n"


def _bucket_solve(text: str) -> str:
    """Each client has a token bucket: capacity 4, refilling 1 token per whole
    second since its last request, capped at capacity. A request costs 1 token
    and is REJECTED when the bucket is empty; a rejected request costs nothing
    and does not update the clock. Answer: total rejected, then the client with
    the most rejections (ties -> alphabetically first)."""
    cap = 4
    tokens, last = {}, {}
    rejected, per = 0, {}
    for line in text.split("\n"):
        line = line.strip()
        if not line:
            continue
        ts_s, who = line.split()
        ts = int(ts_s)
        tok = tokens.get(who, cap)
        if who in last:
            tok = min(cap, tok + (ts - last[who]))
        if tok <= 0:
            rejected += 1
            per[who] = per.get(who, 0) + 1
            tokens[who] = tok
            continue
        tokens[who] = tok - 1
        last[who] = ts
    if not per:
        return "0 none"
    best = sorted(per.items(), key=lambda kv: (-kv[1], kv[0]))[0][0]
    return f"{rejected} {best}"


def _overlay_input(rng: random.Random) -> str:
    lines = []
    for _ in range(rng.randint(25, 40)):
        start = rng.randint(0, 400)
        lines.append(f"{start} {start + rng.randint(1, 60)} {rng.randint(1, 4)}")
    return "\n".join(lines) + "\n"


def _overlay_solve(text: str) -> str:
    """Intervals [start,end) each carry a priority. Where they overlap, the
    HIGHER priority covers the lower; equal priorities merge. Answer: the total
    length covered by the highest priority present, then the number of distinct
    priorities that end up covering at least one unit."""
    spans = []
    for line in text.split("\n"):
        line = line.strip()
        if line:
            a, b, p = (int(x) for x in line.split())
            spans.append((a, b, p))
    if not spans:
        return "0 0"
    lo = min(a for a, _, _ in spans)
    hi = max(b for _, b, _ in spans)
    owner = [0] * (hi - lo)
    for a, b, p in spans:
        for i in range(a - lo, b - lo):
            if p > owner[i]:
                owner[i] = p
    top = max(owner) if owner else 0
    return f"{sum(1 for v in owner if v == top)} {len({v for v in owner if v})}"


def _ring_input(rng: random.Random) -> str:
    ops = []
    for _ in range(rng.randint(50, 80)):
        r = rng.random()
        if r < 0.55:
            ops.append(f"push {rng.randint(1, 99)}")
        elif r < 0.75:
            ops.append("pop")
        elif r < 0.9:
            ops.append(f"rot {rng.randint(1, 5)}")
        else:
            ops.append("flip")
    return "\n".join(ops) + "\n"


def _ring_solve(text: str) -> str:
    """A ring buffer of capacity 6. `push N` appends, and when full it
    OVERWRITES the oldest. `pop` removes the oldest (ignored when empty).
    `rot K` moves the oldest K items to the end, K taken modulo the current
    size. `flip` reverses the buffer, but only when it holds an even number
    of items; on an odd count it does nothing. Answer: the number of
    overwrites that happened, then the final contents oldest-first, space
    separated (just the count and the word empty if none)."""
    cap, buf, over = 6, [], 0
    for line in text.split("\n"):
        line = line.strip()
        if not line:
            continue
        parts = line.split()
        if parts[0] == "push":
            if len(buf) == cap:
                buf.pop(0)
                over += 1
            buf.append(int(parts[1]))
        elif parts[0] == "pop":
            if buf:
                buf.pop(0)
        elif parts[0] == "flip":
            if buf and len(buf) % 2 == 0:
                buf.reverse()
        else:
            if buf:
                k = int(parts[1]) % len(buf)
                buf = buf[k:] + buf[:k]
    tail = " ".join(str(x) for x in buf) if buf else "empty"
    return f"{over} {tail}"


def _resolve_input(rng: random.Random) -> str:
    names = ["core", "http", "json", "sql", "tls"]
    lines = []
    for _ in range(rng.randint(30, 50)):
        n = rng.choice(names)
        lines.append(f"{n} {rng.randint(0,3)}.{rng.randint(0,9)}.{rng.randint(0,9)}")
    return "\n".join(lines) + "\n"


def _resolve_solve(text: str) -> str:
    """Each line pins a package to a version. A package resolves to its HIGHEST
    pinned version compared numerically field by field, EXCEPT that a major
    version of 0 is treated as unstable and loses to any non-zero major.
    Answer: the resolved versions as `name=version`, sorted by name, space
    separated."""
    best = {}
    for line in text.split("\n"):
        line = line.strip()
        if not line:
            continue
        name, ver = line.split()
        parts = tuple(int(x) for x in ver.split("."))
        key = (1 if parts[0] > 0 else 0,) + parts
        if name not in best or key > best[name][0]:
            best[name] = (key, ver)
    return " ".join(f"{n}={best[n][1]}" for n in sorted(best))


def _walk_input(rng: random.Random) -> str:
    moves = []
    for _ in range(rng.randint(60, 90)):
        moves.append(f"{rng.choice('NSEW')}{rng.randint(1, 9)}")
    return "\n".join(moves) + "\n"


def _walk_solve(text: str) -> str:
    """A walker starts at (0,0) on a grid that wraps at 20 in both axes, so
    x and y are always taken modulo 20. N/S change y by +/-1 per step, E/W
    change x by +/-1 per step, and the number is how many steps. The starting cell counts as
    visited once before any move. A cell is SCORCHED once it has been visited
    three or more times in total. The walker cannot enter a scorched cell:
    a step that would land on one is consumed but the walker stays where it
    is. Answer: the number of scorched cells, then how many distinct cells
    were visited."""
    x = y = 0
    seen = {(0, 0): 1}
    for line in text.split("\n"):
        line = line.strip()
        if not line:
            continue
        d, n = line[0], int(line[1:])
        dx, dy = {"N": (0, 1), "S": (0, -1), "E": (1, 0), "W": (-1, 0)}[d]
        for _ in range(n):
            nx, ny = (x + dx) % 20, (y + dy) % 20
            if seen.get((nx, ny), 0) >= 3:
                continue
            x, y = nx, ny
            seen[(x, y)] = seen.get((x, y), 0) + 1
    return f"{sum(1 for v in seen.values() if v >= 3)} {len(seen)}"


def _checksum_input(rng: random.Random) -> str:
    return "\n".join(str(rng.randint(0, 255))
                     for _ in range(rng.randint(80, 120))) + "\n"


def _checksum_solve(text: str) -> str:
    """Bytes are XORed with a 3-byte key that starts as (7, 19, 43). Byte i
    (0-based, counting from the first line) is XORed with key[i % 3]. Each
    time i reaches a positive multiple of 10 (10, 20, 30, ...) the key first
    rotates left one position, so (7, 19, 43) becomes (19, 43, 7). A byte
    SURVIVES when the XOR result is strictly greater than the original.
    Answer: how many survive, then the sum of the surviving XOR results
    modulo 1000."""
    key = [7, 19, 43]
    vals = [int(x) for x in text.split()]
    survivors = []
    for i, v in enumerate(vals):
        if i and i % 10 == 0:
            key = key[1:] + key[:1]
        x = v ^ key[i % 3]
        if x > v:
            survivors.append(x)
    return f"{len(survivors)} {sum(survivors) % 1000}"


def _sessions_input(rng: random.Random) -> str:
    users = ["u1", "u2", "u3", "u4"]
    lines, t = [], 0
    for _ in range(rng.randint(60, 90)):
        t += rng.randint(1, 40)
        lines.append(f"{t} {rng.choice(users)} {rng.choice(['in', 'out'])}")
    return "\n".join(lines) + "\n"


def _sessions_solve(text: str) -> str:
    """Events open and close sessions per user. An `in` while already open is
    ignored; an `out` with nothing open is ignored. An `out` within strictly
    less than 5 units of its `in` VOIDS the session: it closes but counts for
    nothing. A session still open past the final event is DISCARDED. Answer:
    the number of completed (non-void) sessions, then the longest completed
    duration."""
    open_at, total, longest = {}, 0, 0
    for line in text.split("\n"):
        line = line.strip()
        if not line:
            continue
        ts_s, who, kind = line.split()
        ts = int(ts_s)
        if kind == "in":
            open_at.setdefault(who, ts)
        else:
            if who in open_at:
                dur = ts - open_at.pop(who)
                if dur >= 5:
                    total += 1
                    longest = max(longest, dur)
    return f"{total} {longest}"


def _stack_input(rng: random.Random) -> str:
    toks = []
    for _ in range(rng.randint(50, 80)):
        r = rng.random()
        if r < 0.5:
            toks.append(str(rng.randint(1, 20)))
        elif r < 0.75:
            toks.append(rng.choice(["add", "mul"]))
        else:
            toks.append(rng.choice(["dup", "swap", "drop", "sum"]))
    return "\n".join(toks) + "\n"


def _stack_solve(text: str) -> str:
    """A stack machine. A number pushes. `add`/`mul` pop two and push the
    result, but do NOTHING when fewer than two values are present. `dup`
    duplicates the top, `swap` exchanges the top two, `drop` removes the top;
    each does nothing when there are too few values. `sum` pops EVERY value
    and pushes their total (a no-op on an empty stack). All arithmetic is
    modulo 997. Answer: the stack depth, then the top value (or the word
    empty)."""
    st = []
    for line in text.split("\n"):
        tok = line.strip()
        if not tok:
            continue
        if tok.isdigit():
            st.append(int(tok))
        elif tok in ("add", "mul"):
            if len(st) >= 2:
                b, a = st.pop(), st.pop()
                st.append((a + b) % 997 if tok == "add" else (a * b) % 997)
        elif tok == "dup":
            if st:
                st.append(st[-1])
        elif tok == "swap":
            if len(st) >= 2:
                st[-1], st[-2] = st[-2], st[-1]
        elif tok == "drop":
            if st:
                st.pop()
        elif tok == "sum":
            if st:
                total = sum(st) % 997
                st.clear()
                st.append(total)
    return f"{len(st)} {st[-1] if st else 'empty'}"


FAMILIES: List[tuple] = [
    ("ledger", _ledger_input, _ledger_solve),
    ("debounce", _debounce_input, _debounce_solve),
    ("bucket", _bucket_input, _bucket_solve),
    ("overlay", _overlay_input, _overlay_solve),
    ("ring", _ring_input, _ring_solve),
    ("resolve", _resolve_input, _resolve_solve),
    ("walk", _walk_input, _walk_solve),
    ("checksum", _checksum_input, _checksum_solve),
    ("sessions", _sessions_input, _sessions_solve),
    ("stack", _stack_input, _stack_solve),
]


# Line formats, stated explicitly in every prompt. The first calibration ran
# without these and split bimodally: ring and stack, whose statements happen
# to name their operations, passed 100%; families whose format had to be
# guessed failed flat (sessions parsed nothing and answered "0 0"). A bare
# model cannot open input.txt, so an unstated format measures format-guessing
# rather than coding — and biases the whole comparison toward the arm that
# can read the file.
FORMATS = {
    "ledger": "Each line is `SIGNED_AMOUNT TAG`, e.g. `+3 alpha` or `-2 echo`.",
    "debounce": "Each line is `TIMESTAMP VALUE` where VALUE is 0 or 1, e.g. `17 1`.",
    "bucket": "Each line is `TIMESTAMP CLIENT`, e.g. `4 b`.",
    "overlay": "Each line is `START END PRIORITY`, e.g. `120 141 3`.",
    "ring": "Each line is `push N`, `pop`, `rot K`, or `flip`, e.g. `push 42`.",
    "resolve": "Each line is `NAME MAJOR.MINOR.PATCH`, e.g. `http 1.4.2`.",
    "walk": "Each line is a direction letter then a step count, e.g. `N5` or `W2`.",
    "checksum": "Each line is one integer between 0 and 255.",
    "sessions": "Each line is `TIMESTAMP USER in` or `TIMESTAMP USER out`, e.g. `93 u2 in`.",
    "stack": "Each line is an integer, `add`, `mul`, `dup`, `swap`, `drop`, or `sum`.",
}


def _statement(solve: Callable) -> str:

    """The family's rules, taken from the reference implementation's docstring
    so the statement and the truth cannot drift apart."""
    doc = " ".join((solve.__doc__ or "").split())
    return doc


def build_tasks(count: int = 50, seed: int = 20260806) -> List[NovelTask]:
    """`count` instances spread evenly over the families, deterministic in
    `seed` so both arms see identical problems."""
    rng = random.Random(seed)
    out: List[NovelTask] = []
    i = 0
    while len(out) < count:
        fam, make_input, solve = FAMILIES[i % len(FAMILIES)]
        variant = i // len(FAMILIES)
        inst_rng = random.Random(f"{seed}:{fam}:{variant}")
        data = make_input(inst_rng)
        hold = make_input(random.Random(f"{seed}:{fam}:{variant}:holdout"))
        prompt = (
            f"input.txt holds one record per line. {FORMATS[fam]} "
            f"{_statement(solve)} "
            f"Write solve.py that reads input.txt and prints the answer on a "
            f"single line, fields separated by single spaces. Then run it and "
            f"confirm the answer."
        )
        out.append(NovelTask(
            name=f"{fam}{variant + 1}", family=fam, prompt=prompt,
            input_text=data, holdout_text=hold,
            expected=solve(data), holdout_expected=solve(hold),
        ))
        i += 1
    return out


if __name__ == "__main__":
    tasks = build_tasks()
    print(f"{len(tasks)} tasks over {len(FAMILIES)} families")
    for t in tasks[:len(FAMILIES)]:
        print(f"\n--- {t.name}")
        print(f"    {t.prompt[:150]}...")
        print(f"    expected={t.expected!r}  holdout={t.holdout_expected!r}")
