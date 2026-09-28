# ADR 0007: SQLite as the lens-service state store

Status: accepted 2026-07 (supersedes 0002); retired 2026-09-22 — the pattern
cache, the store's only consumer, was removed, and the lens keeps no state

## Context
SQLite was originally proposed in GH #57; PR #128 (core implementation
by Harshal Patel) delivered the store itself but left the pattern cache
on the redis client, which is why ADR 0002 kept Redis and set the bar
for any future migration: move ALL consumers (pattern store,
co-occurrence, router, queue, metrics, compose, doctor, uninstall) in
one change. This migration meets that condition. Motivation:
offline-first operation, one less external dependency to pin and
monitor, no IPC between the lens and its state, simpler install.

## Decision
All lens state lives in a single SQLite file at `SQLITE_DB_PATH`
(default `/data/state/geometric_state.db`), on the `lens-state` named
volume mounted at `/data/state`. The redis service, redis-data volume,
and `REDIS_URL` / `ATLAS_REDIS_MAXMEMORY` / `ATLAS_REDIS_MEM` are
removed. Degradation semantics are unchanged: if the store is
unavailable, pattern cache/router degrade to neutral and the task
queue returns 503. /health and /ready report a "sqlite" state where
they reported "redis".

## Consequences
Single-writer WAL semantics — only the lens container writes the file,
which fits its role as the store's sole consumer. State backup is one
file (see OPERATIONS.md § Backup and restore). The redis-data volume is no longer used;
existing installs can reclaim it with `docker volume rm`.
