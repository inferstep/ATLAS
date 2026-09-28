# Getting Started with ATLAS

The first hour, start to finish: check that your machine can run ATLAS,
install it, verify the install, and complete a first task. This page links
into the detailed guides rather than repeating them — when a step needs more
depth, follow the link.

## Before you install

### What ATLAS is (and is not)

ATLAS is a local coding agent: a model running on your own GPU, wrapped in
machinery that plans changes, verifies generated code in an isolated sandbox,
and repairs what fails. ATLAS runs locally and does not require a hosted model
or third-party model-provider API key. It creates a local per-installation
service token for communication between ATLAS services, and nothing is billed
per token.

One expectation to set: a compact local model with verification is a
different experience from a frontier hosted model. It shines on bounded,
well-described tasks and earns trust incrementally; it is not magic on
sprawling underspecified ones.

### Hardware reality check

Do this before downloading anything:

1. Find your GPU row in
   [SETUP.md § Pick your install path](SETUP.md#pick-your-install-path) —
   it names the recommended method **and the support level** for your
   hardware. Preview and Unsupported rows mean what they say.
2. Check what model fits your VRAM:
   [TROUBLESHOOTING.md § What fits on my GPU?](TROUBLESHOOTING.md#what-fits-on-my-gpu)
   (16 GB VRAM runs the reference models comfortably).
3. NVIDIA users on cards older than the RTX 50-series: read the CUDA
   compute-capability note in
   [SETUP.md](SETUP.md#cuda-compute-capability-dockerfilev31) first —
   the published image targets Blackwell GPUs and older cards need a
   one-time local rebuild.

### What the install actually does

The one-shot bootstrap installs Docker and your GPU runtime (via your distro
package manager, with sudo), clones the repo to `/opt/atlas`, downloads model
weights (~7 GB, hash-verified — this is the slow part), writes a `.env`, and
starts five containers bound to localhost only. Expect 10-30 minutes and
roughly 20 GB of disk. Everything it changes is listed in
[SETUP.md § Method 0](SETUP.md#method-0-one-shot-bootstrap), including the
pinned-release and review-before-running variants if you'd rather not pipe a
script into bash.

One security fact worth knowing before your first task: ATLAS does not
intentionally upload your repository or prompts to a hosted model or
ATLAS-operated service. Model-authored shell commands run inside a locked-down
sandbox container rather than on your host, but sandbox commands have outbound
network access by default so toolchains can fetch dependencies. Set
`ATLAS_SANDBOX_NET_INTERNAL=true` to disable sandbox egress. Review generated
code, commands, and diffs before you commit them.

## Install

Follow [SETUP.md](SETUP.md) (Linux) or [SETUP_MACOS.md](SETUP_MACOS.md)
(Apple Silicon). For most Linux + GPU machines it is one command:

```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

## First launch

### Verify with atlas doctor

```bash
atlas doctor
```

Green checks mean the containers, model, artifacts, and configuration all
agree. Warnings are explained inline; failures print the exact fix. Don't
proceed on failures — the error index in
[TROUBLESHOOTING.md](TROUBLESHOOTING.md) covers every common one.

### The TUI at a glance

Run `atlas` in any project directory. You get a chat pane, a files pane, and
a live pipeline pane that shows what the agent is doing while it works —
stages, tool calls, and verification results stream in real time. The layout
and every keybinding are in [CLI.md](CLI.md#panes).

## Your first task

Pick a small, bounded task in a **version-controlled** repository (or clone a
scratch one). Good first prompts describe one concrete change: "add a
`--verbose` flag to cli.py that prints timing per step" beats "improve the
CLI". Avoid running your first attempt in a repo with uncommitted work — not
because ATLAS is reckless, but because `git diff` is how you review it.

What you'll see:

- **Small edits** are written directly after the agent reads the relevant
  files (the T1 path).
- **Bigger changes** trigger the V3 pipeline: the pipeline pane shows
  planning, several candidate generations, sandbox verification, and repair
  before anything lands on disk (the T2 path).
- **Permission prompts** appear before destructive steps (shell commands,
  deletions) in the default mode — `y` allows once, `a` allows for the
  session, `n` denies. Each deletion is asked about on its own; `a` never
  covers later ones. Modes are documented in
  [CLI.md § Permission modes](CLI.md#permission-modes).

Afterwards: review with `/diff` (or `git diff`), run your tests, and commit
if happy. `/undo` soft-resets the last commit the agent made; `Ctrl+C`
cancels a turn mid-flight.

## Bring your own model

Running a model that isn't in the registry? `atlas onboard` walks the
bring-your-own-GGUF flow ([CLI.md](CLI.md)).

## Where to go next

- [CLI.md](CLI.md) — everything the TUI and `atlas` subcommands can do
- [OPERATIONS.md](OPERATIONS.md) — day-2: upgrades, backups, runbooks
- [ARCHITECTURE.md](ARCHITECTURE.md) — how the pieces fit together
- [docs/README.md](README.md) — the full documentation index
