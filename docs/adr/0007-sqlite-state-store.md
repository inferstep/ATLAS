# ADR 0007: SQLite as the lens-service state store

Status: accepted 2026-07 (supersedes 0002); retired 2026-09-22 — the pattern
cache, the store's only consumer, was removed, and the lens keeps no state

This decision no longer applies. The lens keeps no state, so it has no
state store. `atlas config migrate` drops `SQLITE_DB_PATH` from an old
`.env` (`migrate` in `atlas/config_schema.py`). The
[CHANGELOG](../../CHANGELOG.md) records the changes: 3.1.3 for the move
from Redis ([ADR 0002](0002-keep-redis.md)), and "Removed: the pattern
cache and its SQLite state store".
