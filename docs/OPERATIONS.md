# Operations Guide

Day-2 operations for a running ATLAS install: health, logs, runbooks,
upgrades, rollback, and backup. Companion to TROUBLESHOOTING.md
(symptom→fix) — this file is procedure-oriented.

## Health

```bash
atlas doctor                         # the production contract: services,
                                     # artifacts, identity, calibration, disk
curl -s localhost:8090/health        # proxy liveness (+ upstream summary)
curl -s localhost:8090/ready         # readiness: 200 only when llama, lens,
                                     # sandbox, v3 are all healthy
docker compose ps                    # container states + restarts
```

Per-service health: llama `:8080/health`, lens `:8099/health` (rich
degraded-state JSON incl. lens self-test), v3 `:8070/health`, sandbox
`:30820/health`.

## Logs

```bash
docker compose logs -f atlas-proxy   # agent loop, tool calls, gates
docker compose logs -f v3-service    # pipeline phases
docker compose logs -f geometric-lens
docker compose logs --tail 100      # everything recent
```

TUI-side debugging: `atlas tui --log <path>` writes a local TUI event log;
`ATLAS_TUI_LOG=<path>` does the same and `=off` disables it.

## Private diagnostics: task-contract shadow capture

**Off by default, and not an ordinary production feature.** This is
instrumentation for one open question — how often what ATLAS infers from a
request's English disagrees with the `task_contract` the client declared — and it
exists to produce evidence for that migration, nothing else. No record it writes
is read by any decision, and none reaches `/events`, the agent SSE stream, a
prompt, or a model.

Enable it only when you are deliberately acquiring evidence:

| Variable | Default | Meaning |
|----------|---------|---------|
| `ATLAS_SHADOW_CAPTURE` | unset (**off**) | Filename of the capture, relative to the diagnostic root. Unset means no file is opened, no writer runs, and no record is built. |
| `ATLAS_DIAGNOSTIC_DIR` | `/data/diagnostics` | The only directory a capture may live in. Compose bind-mounts the host's `${ATLAS_DIAGNOSTIC_HOST_DIR:-./diagnostics}` here, owned by the same user the proxy runs as. |

Containment and startup rules:

- The resolved destination must stay **inside** the diagnostic root. An absolute
  path or one that escapes via `..` is refused.
- The destination must **not already exist**. Appending to a previous capture
  would merge two acquisitions into what later reads as one, and no analysis
  could separate them afterwards.
- Both refusals, and an unwritable destination, **fail at startup** — the proxy
  does not serve with a capture it cannot own. Pick a fresh filename per
  acquisition.

### Reading a capture

One JSON object per line. Two record kinds carry the contract observation — a
request snapshot and one record per live gate evaluation — plus the
observe-only candidate records (`shadow_invocation_feasibility`,
`shadow_route_disposition`, `candidate_policy_decision`,
`candidate_authorization_decision`, `authorization_grant_event`,
`shadow_delivery_disposition`, `candidate_mutation_scope`,
`candidate_generation_bypass`, `automatic_delivery_attribution`), and a footer
written at shutdown.

An acquisition is **clean** only when all of these hold:

- the **last line is the footer** (`record_kind: task_contract_shadow_footer`);
- `dropped == 0`, `errors == 0`, `duplicate_request_ids == 0`;
- `request_tracking_overflow == false`;
- `accepted == written`, and the file holds exactly `written + 1` lines;
- every `request_id` has exactly one snapshot and a gate sequence of
  `1..n` with no gaps.

Anything else is **defective** and must be reported as such rather than quietly
analysed. In particular:

- **No footer at all.** The process died before its close hook ran, or the
  writer did not finish within the hook's deadline. Both leave a partial file,
  which is the intended outcome: the footer is written by the writer itself,
  after every record its counters describe, so a footer that exists always means
  the acquisition completed. Nothing manufactures one to make a truncated
  capture look whole. A write already inside a blocking filesystem call cannot
  be interrupted portably, so there is no safe cutoff at which a late footer
  could be added.
- **Drops.** The queue was full and records were discarded rather than made to
  block an agent request. The count is honest; the file is incomplete.
- **Errors.** Records failed to serialise or failed to write. The run was
  unaffected; the capture is not trustworthy as a census.
- **Duplicate request IDs.** Two runs presented the same
  `X-ATLAS-Request-ID`. They are reported, never merged.
- **Overflow.** Duplicate tracking hit its bound and stopped remembering new
  ids, so later duplicates may go undetected.

### What the records do and do not tell you

- **`request_id` is a join key only.** It is the existing
  `X-ATLAS-Request-ID`, which a client may supply, so it correlates records
  within one capture and carries no authority. It is not a session id, not a
  task id, and it does not establish who sent anything. The same id, with
  the `X-ATLAS-V3-Invocation-ID` V3 mints per generation, travels on V3's
  calls to the Lens and on the Lens's own embedding and identity-probe calls
  to the model server, so an embedding request can be joined to the exact
  request and invocation it served; an acquisition relay may require that
  pair on embedding traffic and refuse calls without it.
- **Hashes are identities, not secrets.** Paths and verification commands are
  stored as unsalted SHA-256, truncated to 16 hex characters; the user message
  is stored as a full unsalted SHA-256. They exist so two records naming the
  same thing collide and two naming different things do not. Over a small
  guessable domain — `app.py` and the like — the original is recoverable by
  enumeration. Treat a capture as revealing which paths and commands were
  involved, and do not treat hashing as a confidentiality control.
  The proxy's own direct Lens calls (per-write scoring)
  carry the same `request_id` with a proxy-owned `proxy-lens:` invocation
  derived from it in the same historical header (ARCHITECTURE, attribution
  on model-bound calls); that value is a join key too, never an authority.
- **No record decides anything.** Every record carries
  `influences_live_decision: false`, and that is enforced structurally rather
  than by convention.

## Runbooks

| Symptom | Procedure |
|---|---|
| Service won't start | `docker compose logs <svc>`; port collision → change the `ATLAS_*_PORT` in `.env`; bad override → `docker compose config` names the offending key |
| Model load failure | llama logs name the reason (VRAM, arch, quant); `atlas tier fit --write` re-sizes; `atlas model verify` checks file integrity against the pinned hash |
| Lens degraded (`self_test_error`) | `atlas lens check` prints the exact missing artifact + fix command (`atlas lens build`); identity mismatch means the bundle belongs to another model |
| ASA inactive | llama startup log prints why (missing vector / marker mismatch); `atlas asa check`, then `atlas asa build` |
| Sandbox failures | `docker compose logs sandbox`; egress-cut mode (`ATLAS_SANDBOX_NET_INTERNAL=true`) intentionally breaks dependency installs; resource kills show as 137/timeout in tool results |
| GPU OOM | reduce `ATLAS_CTX_SIZE`/slots via `atlas tier fit --write`; check nothing else holds VRAM (`nvidia-smi`) |
| Disk full | models dir is the usual consumer; `atlas model remove <name> --yes` |
| Failed upgrade | § Rolling back (pin the previous tag, restore `.env.bak`) |
| Bad/revoked artifact | SECURITY.md § artifact revocation; `atlas model verify` + `--force-artifacts` reinstall pins |
| Full reset (keep models) | `docker compose down -v && docker compose up -d` — **destructive**: wipes the stack's named volumes (`v3-telemetry`), keeps models/config. Last resort |

## Resource tuning

All knobs in CONFIGURATION.md; the load-bearing ones: `ATLAS_CTX_SIZE`
+ `ATLAS_PARALLEL_SLOTS` (VRAM), `ATLAS_SANDBOX_MEM/CPUS/PIDS`
(runaway-build protection), `ATLAS_V3_TIMEOUT` (interactive cap).

## The memory envelope

These numbers are not independent, and getting the sum wrong is not a
performance problem. A staged verification command once reached 5.9 GB in
seconds on a 16 GB host where the inference server held 9 GB with no limit of
its own. Every process was inside its own bounds; the kernel still had to pick
something to kill, and it picked the largest resident process, which was the
model. What was missing was not one limit — it was a total that fits the
machine.

Two layers, and both are needed.

**Per command.** The executor bounds every untrusted command: wall clock,
memory, process count and output bytes, installed before the command starts and
applied to everything it spawns, however it spawns it. A command stopped at a
ceiling is reported as stopped, never as failed — see "Truthful results" below.

| Variable | Default | What it bounds |
| --- | --- | --- |
| `MAX_EXECUTION_TIME` | 300 (compose) / 60 (executor) | wall clock per command |
| `ATLAS_EXEC_MEMORY_BYTES` | 1 GiB | resident memory of the command and its descendants |
| `ATLAS_EXEC_MAX_PROCESSES` | 256 | processes in the command's tree |
| `ATLAS_EXEC_OUTPUT_BYTES` | 32 MiB | stdout + stderr the executor will buffer |

Malformed, zero, negative or internally inconsistent values stop the executor
at startup rather than being clamped into something plausible at the moment a
command runs. An output cap at or above the memory ceiling is one of the
inconsistent ones: the executor holds that buffer.

**Per deployment.** Every container's hard maximum, plus a host reserve, has to
fit the host. The shipped defaults assume a 16 GiB machine running a 12B Q4
model:

| Component | Variable | Default | Basis |
| --- | --- | --- | --- |
| inference | `ATLAS_LLAMA_MEM` | **unset** | see below — a limit below its peak is a certain kill |
| geometric lens | `ATLAS_LENS_MEM` | 1.75 GiB | measured peak 1.56 GiB |
| sandbox | `ATLAS_SANDBOX_MEM` | 1.5 GiB | one 1 GiB command + the executor and its buffers |
| v3-service | `ATLAS_V3_MEM` | 0.5 GiB | measured peak 52 MiB |
| proxy | `ATLAS_PROXY_MEM` | 0.25 GiB | measured peak 33 MiB |
| host reserve | `ATLAS_HOST_RESERVE_BYTES` | 1.5 GiB | kernel, container daemon, logging, shutdown |
| **total (enforced)** | | **5.5 GiB** | on a 15.36 GiB host: 9.86 GiB left for the unbounded model |

**Why the inference service has no limit.** Its measured peak resident set is
10.31 GiB, its anonymous working set is 8.81 GiB once the 2.11 GiB it has
swapped is counted back, and it has 16 MiB of reclaimable page cache — so a
hard limit under the peak yields a kill, not reclaim. It ships unset until a
real-model canary establishes a value with measured headroom, with
`ATLAS_LLAMA_BUDGET_BYTES` carrying the measured expectation for accounting.
Setting `ATLAS_LLAMA_MEM` enforces whatever you put in it: do that only with
canary evidence, and never below the observed peak.

This has a consequence worth stating plainly: **the enforced-maxima check alone
would not have caught the configuration that killed it.** With the model
unbounded there is no inference maximum to add, so an 11 GiB sandbox plus the
small services plus the reserve fits a 15 GiB host — which is exactly how every
process came to be inside its own limit at the moment the kernel went looking
for a victim. What catches it is the **remainder**: what the enforced budgets
leave over for the one component nothing holds. Today that is 9.86 GiB against
an expectation of 11 GiB, and the proxy reports the shortfall at startup. Under
the old 11 GiB sandbox it was 0.33 GiB against 11 GiB.

An overrun is reported, not refused. The refusal is for enforced maxima that
over-commit — a number an operator can edit. An unbounded component that
outgrows the remainder needs a different fix (a validated limit, or a smaller
model), and disabling execution would not make the machine safer.

Set `ATLAS_HOST_MEMORY_BYTES` to your machine's RAM to turn that arithmetic
into a check. When it is declared and the sum does not fit, the proxy **refuses
to run commands** and says by how much it is over; reading files and answering
questions still work, because the diagnosis has to be readable. Left unset,
nothing is declared and nothing is checked — the per-command contract still
applies, and it carries the safety on its own.

Raising `ATLAS_EXEC_CONCURRENCY` is checked the same way: two commands at the
per-command ceiling must still fit inside the sandbox's own budget.

`ATLAS_SANDBOX_MEM` is what `atlas init` sizes from host RAM. A value chosen as
a fraction of RAM without subtracting the model is exactly the shape that
caused the kill — on a 16 GiB host with a 12B model resident, the sandbox's
share is about 1.5 GiB, not 11.

Swap is disabled for the sandbox (`memswap_limit` tracks `mem_limit`): a
swapping test is a hung one, and swap hides an over-commit rather than
absorbing it.

## Cancelling a command by going away

A caller that stops waiting for an answer cancels the command. The executor's
`/shell` handler watches the request's own connection through Starlette's
`is_disconnected`, and tells the bounded runner to stop through the same
cancellation callback every other stop uses — a reset connection, a closed one,
an explicitly aborted request and a graceful shutdown all arrive the same way.

The distinction is kept in the answer: `cancelled` is its own outcome, separate
from `timed_out` and from `memory_exhausted`, `process_limit_exceeded` and
`output_limit_exceeded`. A cancelled command produces no verification evidence,
mints no candidate grant, and takes its whole process tree with it — including
descendants that left the process group. Cancelling twice is the same as
cancelling once, a neighbouring command is unaffected, and the watcher for each
request is per-request, so a reused file descriptor cannot cancel a later one.

## Building for verification without claiming the deployable tag

`docker compose build atlas-proxy` writes `ghcr.io/inferstep/atlas-proxy:dev` —
the same tag the running stack was started from. Nothing restarts, so the
running container keeps its image; but the deployable **name** now points at
the new build, and the previous image becomes untagged. If it is later pruned,
the tag cannot be put back: the image the container is running no longer exists
in the local store, and its identity is recoverable only from
`docker inspect <container>` and the compose labels.

For a build you only want to test, use a throwaway tag and leave the deployable
name alone:

```bash
docker build -t atlas-proxy:my-slice-check ./proxy      # never :dev
ATLAS_IMAGE_TAG=my-slice-check docker compose config    # check it resolves
```

**A tag is not an identity.** The image ID is. A tag can be rewritten under a
running container, and once the old image is pruned the tag cannot be put back
— the deployable name then points at something the running container is not,
and nothing about inspecting the tag reveals it. Acquisition runners refuse to
launch from `:dev` or `:latest` for that reason, pin the exact `sha256:` image
ID plus the built binary's hash before the first case, and report a missing
historical image rather than pulling or rebuilding one: a reconstructed image
is a different artifact wearing the same name.

Before any build, record what is actually running, so the tag can be restored:

```bash
docker inspect -f '{{.Image}}' atlas-atlas-proxy-1
docker inspect -f '{{index .Config.Labels "com.docker.compose.image"}}' atlas-atlas-proxy-1
```

## Deploying a checkout, gated

`scripts/deploy-gated.sh` deploys the checked-out commit to the local Compose
stack and records what "deployed" means. In order, it:

- refuses a checkout with uncommitted changes;
- runs `go test ./...` in `proxy/` and `pytest tests/v3 tests/v3-service
  tests/cli`, and stops on a failure;
- tags each image it replaces as `atlas-prev:<service>-<stamp>`, the rollback
  target;
- rebuilds geometric-lens, v3-service, sandbox and atlas-proxy, and
  llama-server when `inference/` changed since the last deploy or the running
  model server was not put there by the script (a recreate reloads the model);
- recreates them one at a time, each healthy before the next;
- checks that every container runs the image its tag names, and pins it as
  `atlas-built:<service>-<sha>`;
- checks the running stack: the proxy sees every upstream, v3-service is
  healthy, the lens serves no pattern-cache routes, the sandbox's Jinja and
  subdirectory checks work, and v3-service can syntax-check an absolute path;
- writes `DEPLOYED_SHA` only when all of that passed.

Records go to `ATLAS_DEPLOY_DIR` (default `~/atlas-ralph`). To roll one
service back, tag its `atlas-prev:*` image with the service's image name and
recreate that service with `docker compose up -d --force-recreate --no-deps`.

## Cancellation

A caller that goes away stops the command it asked for. A reset connection, a
closed one, an aborted request or a shutdown all reach the executor's bounded
runner through its cancellation callback, which kills the process tree the same
way a timeout does — and reports `cancelled`, which is a different outcome from
`timed_out` and from the resource ones, so a stopped command is never mistaken
for a finished one.

This required the executor's middlewares to be pure ASGI rather than
`BaseHTTPMiddleware`. The latter interposes on the receive channel, so
`http.disconnect` never reaches the endpoint and `Request.is_disconnected()`
returns False forever: measured directly, the same probe reports "disconnected
at 1.0s" without a middleware and "never disconnected" with one.

Graceful shutdown **drains** rather than cutting in-flight work off, so the
bound it completes within is the running command's own deadline
(`MAX_EXECUTION_TIME`, or a shorter per-request one). Nothing survives it.

## Truthful results

A command stopped at a ceiling exits non-zero exactly like a failing test — a
Python process that runs out of memory raises `MemoryError` and exits 1. Read as an exit code alone, a verification that never completed becomes a
behavioural failure of the code under test.

So the executor reports **how** the command ended, from a closed set:
`completed`, `timed_out`, `memory_exhausted`, `process_limit_exceeded`,
`output_limit_exceeded`, `cancelled`, `spawn_failed`, and the fail-closed
`internal_unclassified`. Only `completed` means the command reached its own
end, and only then does its exit code mean anything.

A stopped command cannot mint a candidate authorization, consume a grant,
settle mutation debt or claim completion. In candidate staging it becomes
`resource_exhausted`, which reports as `evidence_resource_exhausted` — the
obligation is unmet for want of an observation, not because an observation went
against the candidate.

The model is told what happened in words it can act on ("it used too much
memory to finish; it did NOT fail"), with no host limits, pids, cgroup paths or
deployment detail in the message.

---

# Upgrading

Applies to the Docker Compose deployment (the supported path).

## Standard upgrade

```bash
cd /opt/atlas            # your checkout
cp .env .env.bak         # config backup (one file holds all your settings)
git pull
docker compose pull      # fetch the target images
docker compose up -d     # recreate changed services
atlas doctor             # verify: services healthy, model/artifacts intact
```

Pin instead of tracking `latest` for production use: set
`ATLAS_IMAGE_TAG=3.1.3` (or an exact `sha-<commit>`) in `.env` before
`docker compose pull`. Every published digest is immutable and cosign-
signed; `sha-*` tags never move.

## What an upgrade can and cannot touch

- **Config:** `.env` is never rewritten by an upgrade. New keys are
  additive with safe defaults; removed keys are ignored (see
  CONFIGURATION.md § removed variables). Re-run `atlas init` only if
  you want re-detected hardware sizing.
- **Models and artifacts:** never modified by image upgrades. Lens/ASA
  bundles are per-model and identity-checked at load; an upgrade that
  changes bundle requirements surfaces as a doctor warning with the
  exact rebuild command, not a silent break.
- **Telemetry:** the `v3-telemetry` volume persists across upgrades.

## Version compatibility

N-1 configs are supported: a `.env` written by the previous release
boots the current one. Registry/artifact schema changes are additive
within a minor version (SUPPORT_MATRIX.md § compatibility policy).

## Automated upgrade (`atlas upgrade`)

```bash
atlas upgrade --to 3.1.3       # or --to latest (default); a leading v is accepted and stripped
```

This records a restore point (current tag + image digests + a `.env`
backup) before staging the target images, starts them, waits for
readiness, runs a quick-doctor smoke check, and finalizes. **If any step
fails — a bad pull, a service that never becomes ready, or a failed
smoke check — it automatically restores the previous release** (your
`.env`, including `ATLAS_IMAGE_TAG`, and brings the old images back up on
the locally cached layers — the restore never re-pulls, since a mutable
previous tag could have moved). `--skip-smoke` skips only the final
check; the restore-on-failure guarantee still holds for the earlier
steps.

Re-running with the tag already deployed: a release tag (`X.Y.Z`) is a
no-op — those tags never move. A mutable tag (`latest`, `dev`) runs the
full staged flow anyway ("refresh"), because the registry may point the
same tag at newer images; the pull is cheap when nothing changed. Note
that a refresh replaces the locally cached images under the same tag, so
`atlas rollback` after a successful refresh cannot return to the
pre-refresh build — pin release tags for reversible upgrades. Images not
published in the registry for your backend (e.g. the locally-built ROCm
llama image) are skipped by signature verification, and a slow pull gets
up to `ATLAS_UPGRADE_PULL_TIMEOUT` (default 3600 s) to finish.

The manual sequence above remains valid and is what `atlas upgrade`
automates. To undo a *successful* upgrade later, `atlas rollback`
(§ Rolling back) returns to the recorded restore point.

`atlas upgrade` verifies each target image's keyless cosign signature
before applying (best-effort: if cosign isn't installed it logs and
continues; a signature that *fails* aborts the upgrade and the previous
release stays in place). Override with `ATLAS_UPGRADE_SKIP_VERIFY=1`.

`atlas upgrade --to <tag> --dry-run` previews the plan (current tag +
image digests → target, and the ordered steps) without changing
anything.

---

# Rolling back

## Automated (`atlas rollback`)

```bash
atlas rollback              # restore the last upgrade's previous release
atlas rollback --to 3.1.2   # or target a specific immutable tag; a leading v is accepted and stripped
```

With no argument it reads the restore point written by `atlas upgrade`
(`.atlas-upgrade/restore-point.json`) and brings the previous release
back up. With `--to TAG`, a pull/start failure (e.g. a typo'd tag that
doesn't exist) restores `.env` to the tag that was deployed before the
attempt. The manual procedures below are the equivalent by hand and the
fallback when no restore point exists.

## Images

Every push publishes immutable `sha-<commit>` tags and releases publish
semver tags; none are ever repointed. To roll back:

```bash
# 1. Find the last-good tag (release tag, or a sha-* from
#    `docker compose images` / the GHCR package page)
# 2. Pin it:
sed -i 's/^ATLAS_IMAGE_TAG=.*/ATLAS_IMAGE_TAG=3.1.2/' .env   # or sha-abc1234 — registry semver tags carry no leading v
docker compose pull
docker compose up -d
atlas doctor
```

Signatures verify against any historical digest (`cosign verify`, see
build-images.yml for the identity flags).

## Configuration

`.env` is a single flat file — restore the `.env.bak` you took before
upgrading (§ Standard upgrade, step 1), then `docker compose up -d`.

## Code (checkout)

```bash
git log --oneline          # find the last-good commit
git checkout <tag-or-sha>  # or: git reset --hard <sha> on your branch
pip install -e . --no-deps # refresh the CLI entry point
```

## Lens/ASA artifacts

`atlas artifact snapshot` (run before activating a new bundle) keeps
one previous-bundle copy; `atlas artifact rollback` restores it and
`atlas artifact verify` checks signature + file hashes. Without a
snapshot, re-download the pinned published bundle
(`atlas model install-artifacts <model> --force-artifacts` — hashes are
pinned in the registry, so you get exactly the published bytes) or
restore your own backup of the lens models dir (§ Backup and restore).

---

# Backup and restore

What actually holds state, where it lives, and what losing it costs.

| State | Location | Loss impact | Backup |
|---|---|---|---|
| Configuration | `.env` (+ `atlas.conf` for K3s) | Re-run `atlas init` | copy the file |
| Models | `ATLAS_MODELS_DIR` (default `./models`) | Re-download (hash-verified) | optional — large, re-fetchable |
| Lens/ASA bundles | `geometric-lens/geometric_lens/models/` + `models/*.gguf(.model)` | Published bundles re-download; **locally-trained calibration does not** | copy the dir after any `atlas lens build` |
| Lens training data | `benchmark/results/` | Lose the ability to rebuild calibration from your own bench runs | copy before pruning |
| TUI sessions | `~/.cache/atlas-tui/sessions/` | Lose `--resume` history | copy the dir |
| Project files | your repo | — | your VCS |

## Restore

Config/models/bundles/bench results: copy back into place, `docker compose up
-d`, `atlas doctor` (it re-verifies artifact identity + hashes).

## Honest gaps

Backups are manual copies — there is no `atlas backup` command. The
state table at the top of this section is the complete state inventory;
nothing else on the machine is ATLAS state.
