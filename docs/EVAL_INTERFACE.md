# The held-out evaluation interface

The capability and reliability proofs (#242, #243) run a held-out suite. A
separate evaluation session writes the suite and runs it. The development
side builds only the generic pieces in `scripts/eval/` and never sees the
tasks. This page is the contract between the two sides. The decisions behind
it are recorded on #238 (the suite) and #242 (the baseline).

## The suite

The suite lives in a private repository and never enters this one.

```
suite.json             suite id, version, and per task: its id and the SHA-256 of every file
tasks/<id>/
  task.json            {"id", "mode", "runtime", "network", "grader_timeout_s", "kind", "lang"}
  prompt.md            the request, sent verbatim as the user's message
  seed/                the workspace before the run (may be empty)
  grade                executable: grades a finished workspace
  grader/              optional: data the grader reads, mounted read-only at /grader
  controls/pass/       overlay on seed/ that the grader must pass
  controls/fail/       overlay on seed/ that the grader must fail
```

- `mode` is `work` or `question`: the task contract's `task_mode`.
- `runtime` is `python3.13`, `go` or `node`: a runtime the sandbox image has.
- `kind` and `lang` are optional labels. `report` gives pass rates by `kind`,
  so a kind is a category that several tasks share, never a task's name.
- The driver refuses a suite whose files differ from `suite.json`, including
  a file added after the freeze.

## The grader

- It runs on a copy of the finished workspace, in a throwaway container from
  the sandbox image. The container has no network, runs as the caller's uid,
  and is stopped after `grader_timeout_s`.
- The driver resolves `--image` to its image ID once, and grades every
  session of a run, and every control in `check`, with that ID. A tag that
  moves during a block cannot change the grader.
- It is called as `/grade /w`, with the copy at `/w` as its cwd, and reads
  only the workspace.
- The copy keeps links as links, as the sandbox saw them. A link can point
  anywhere in the grader's container, so a grader that compares the work with
  its own data checks that the work's paths resolve inside `/w`.
- A workspace that cannot be copied is a grader error for that session, and
  the block goes on.
- Exit 0 means pass and exit 1 means fail. Any other exit, or a timeout, is a
  grader error, counted apart. Its first line of output is kept as the reason.
- `driver.py check` refuses the suite unless every grader passes
  `controls/pass` and fails `controls/fail`.

## The runs

- **The arms.**
  - `atlas`: a session through `POST /v1/agent`, as the TUI sends it.
  - `baseline`: the same model and steering through a minimal read, write and
    run loop, with ATLAS's sampling, per-turn ceiling and command limits, and
    no V3, lens, gates or grammar. The baseline's full specification is on #242.
- **The budget.** The budget is one per suite: the eval stack's session
  timeout (600 s by default). ATLAS takes no per-request budget, so a per-task
  budget could not apply to both arms. `--budget-s` must equal the stack's
  setting, and the baseline gets the same. A run refuses a budget that
  differs from the `session_timeout_s` the proxy reports on `/version`.
- **The sandbox network.** With `ATLAS_SANDBOX_NET_INTERNAL=true` the sandbox
  has no route out, and its host port closes too. The baseline arm reaches
  the sandbox through that port (`--sandbox-url`), so it cannot run commands
  in that mode.
- **A stack of its own.** A run refuses the development project (`atlas`),
  and a stack whose v3-service writes V3 pool captures. Session files, event
  logs and V3 telemetry stay in the eval stack's own volumes.
- **One commit.** A run refuses a stack whose five images are not the ones
  its gated deploy recorded for the stated commit (#241). The commit, the
  images, the grammar mode and the lens and steering state go into every
  record.
- **Who runs it.** The evaluation session runs the driver and keeps the
  records, outside anything the development side reads. The maintainer
  schedules GPU time, so the two sides never share the GPU.

## The records

`run` appends one JSON line per session. Each line holds:
- the task's id, kind, lang and declared network; the repeat; the
  session's workspace subdirectory and start time (UTC);
- the arm's status, reason, turns, tokens and wall time, and the budget;
- the grade, the grader's first line, and its whole output (up to
  1,000,000 characters);
- the SHA-256 of `suite.json`; the grader image as given and its ID; the
  driver's commit and whether `scripts/eval` differs from it; whether the
  sandbox could reach the network, per network it is on; the session
  timeout the proxy reported; and the stack (commit, images, grammar mode,
  lens and steering state);
- on the baseline arm, the context window it used and the model server's
  `/props` identity (model, model path, build).

Records name tasks and keep grader output, so they stay with the
evaluation session.

## Commands

```
scripts/eval/driver.py check SUITE --image SANDBOX_IMAGE
scripts/eval/driver.py run SUITE --arm atlas --out atlas.jsonl --image SANDBOX_IMAGE \
    --compose-project atlaseval --workspace-root /path/the/stack/mounts --tasks t01,t02,...
scripts/eval/driver.py run SUITE --arm baseline --out baseline.jsonl ...same options...
scripts/eval/driver.py report atlas.jsonl --against baseline.jsonl
```

`report` prints aggregates only, per arm:
- the pass rate with a Wilson 95% interval, overall and by kind;
- completed-but-failed over completed (false completion);
- passed-but-not-completed;
- failure endings by status and reason;
- grader errors;
- the number of tasks whose repeats disagree;
- ATLAS's pass rate minus the baseline's, with Newcombe's interval.

It names no task and quotes no task text, so only this output goes back to
development.
