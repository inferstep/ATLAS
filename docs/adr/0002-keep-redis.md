# ADR 0002: Keep Redis as the lens-service state store

Status: superseded by [0007](0007-sqlite-state-store.md) (2026-07)

This decision no longer applies. ATLAS runs no Redis, and the lens keeps
no state: `atlas config validate` flags `ATLAS_REDIS_MAXMEMORY` and
`ATLAS_REDIS_MEM` as removed keys (`atlas/config_schema.py`). The
[CHANGELOG](../../CHANGELOG.md) records the changes: 3.1.3 for the move to
SQLite ([ADR 0007](0007-sqlite-state-store.md)), and "Removed: the pattern
cache and its SQLite state store".
