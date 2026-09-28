"""A very small in-process cache."""

_ENTRIES = {}
HITS = 0
MISSES = 0


def put(key, value):
    _ENTRIES[key] = value


def get(key):
    global HITS, MISSES
    if key in _ENTRIES:
        HITS += 1
        return _ENTRIES[key]
    MISSES += 1
    return None


def stats():
    return {"hits": HITS, "misses": MISSES, "size": len(_ENTRIES)}
