# ADR 0012: The repository is laid out like a library

Status: accepted 2026-11

## Context
A person who has never seen the repository should find a thing where its
name says it is. The tree grew by adding to it. The root mixes five
services, two clients, deployment files and a runtime folder among its
folders. `docs/` holds most of its pages loose beside folders of one file.
Some test folders are named after something other than the code they test.
The proxy's Go code sits in one flat folder whose file names use many
different prefixes. Each of these costs a newcomer a search, and costs a
maintainer a guess about where a new file goes.

## Decision
The repository is a set of shelves, kept by these rules.

1. **One subject per shelf.** A folder holds one subject, and its name says
   the subject. A folder with one item is not a shelf: the item goes to the
   shelf of its subject. A Go package is the exception: its folder is a unit
   of the language, so a package of one file is a shelf of its own.
2. **The root.** The five services live in `services/` (`proxy`,
   `v3-service`, `geometric-lens`, `sandbox`, `inference`). The terminal UI
   and the editor extensions live in `clients/`. Deployment files live in
   `deploy/`: the K3s templates, and the compose files other than
   `docker-compose.yml`. `docker-compose.yml` stays at the root, where
   `docker compose` looks, as do the other files a tool reads from the root.
   `atlas/`, the product's own package and the `atlas` command, stays at
   the root: `pyproject.toml` builds it there and `python -m atlas` runs
   from there. All the community files GitHub reads, such as the code of
   conduct, the contributing guide and the governance page, live in
   `.github/`.
   Image names and the one-line install do not change.
3. **Tests mirror code.** Python tests live under `tests/`, in a folder
   named after the code they test: `tests/atlas/` tests `atlas/`. A service
   whose tests need its own lock file and process keeps them beside its
   code. Go and TypeScript tests sit beside their code, as their tools
   expect.
4. **Data is not documentation.** Recorded traces, files a program reads
   and images are filed as data or assets, apart from the pages of `docs/`.
5. **One catalog.** `docs/README.md` holds one "where things live" table,
   one line per top shelf, and says where each kind of test lives. A change
   that moves a shelf updates its line. Shelves get no README of their own
   for this.
6. **About 25 entries.** About 25 entries per folder is a soft limit: a
   reason to look for a missing shelf, never a check that fails. A folder
   stays flat where a sub-folder would add a place to look and no subject.
7. **Go code.**
   - A package is made only where it measures well: a small exported
     surface and no wide interface back into its caller. Code that would
     need a wide interface or many exports stays in `main`.
   - A large state struct is split into named groups by concern and the
     groups are embedded, so callers still read `ctx.Field`. `AgentContext`
     has 14 such groups.
   - In a flat Go folder the file-name prefix is the shelf: files on one
     subject share one prefix.
8. **Gate files move with care.** A gate file decides what a check accepts:
   a workflow, a check script, a baseline. One that a check reads from the
   base branch or from `main`, such as a script a scheduled workflow runs
   from `main`, moves only in a release window, since until the release the
   old and the new path are both in use. Any other gate file moves on `dev`
   with a maintainer's approval. Every such move compares each list that
   names gate files, old against new.

## Consequences
- Each layout change cites this record and does one thing.
- A move updates every reader in the same change: links and their
  translations, path strings in code and tests, and lists keyed by a file
  name. A list that quietly loses an entry after a move is the failure to
  watch for, so a check over a list of files fails on an empty or missing
  folder rather than passing.
- A move of a gate file that a check reads from the base branch or from
  `main` waits for a release window. Other moves are small changes on
  `dev`, and those that touch a gate file carry a maintainer's approval.
- Links from outside the repository to `main`, such as the website and the
  extension marketplace pages, change at the release that carries the move.
- A released version keeps its own paths: a message such as
  `See docs/SETUP.md` is right in the checkout it came from.
