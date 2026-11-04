"""GGUF header metadata reading shared by the CLI commands.

Model-agnostic on purpose: callers key off `general.architecture` and
per-arch keys from the header, never a model name.
"""

import struct


# GGUF metadata value types with a fixed size: struct format and byte length.
_SCALAR = {0: ("<B", 1), 1: ("<b", 1), 2: ("<H", 2), 3: ("<h", 2),
           4: ("<I", 4), 5: ("<i", 4), 6: ("<f", 4), 7: ("<?", 1),
           10: ("<Q", 8), 11: ("<q", 8), 12: ("<d", 8)}


def _u32(f):
    return struct.unpack("<I", f.read(4))[0]


def _u64(f):
    return struct.unpack("<Q", f.read(8))[0]


def _str(f):
    return f.read(_u64(f)).decode("utf-8", "replace")


def _value(f, t):
    """One metadata value of GGUF type t."""
    if t == 8:
        return _str(f)
    if t == 9:
        return _array(f)
    fmt, size = _SCALAR[t]
    return struct.unpack(fmt, f.read(size))[0]


def _array(f):
    """An array value: its item type and length, then the items."""
    et = _u32(f)
    n = _u64(f)
    if et == 8:
        return _strings(f, n)
    if et == 9:
        return [_value(f, 9) for _ in range(n)]
    fmt, size = _SCALAR[et]
    if n > 4096:
        f.seek(n * size, 1)   # bulk data we never need (e.g. scores)
        return []
    return [struct.unpack(fmt, f.read(size))[0] for _ in range(n)]


def _strings(f, n):
    """An array of n strings."""
    if n > 4096:
        for _ in range(n):       # tokenizer tables: skip each
            f.seek(_u64(f), 1)   # string by its length prefix
        return []
    return [_str(f) for _ in range(n)]


def read_gguf_kv(f):
    """Yield (key, value) for each metadata pair in a GGUF file. Large arrays
    we never need (tokenizer tables) are not materialized: fixed-size scalar
    arrays are seeked over in one hop, string arrays are walked by their
    length prefixes without decoding."""
    magic = f.read(4)
    if magic != b"GGUF":
        raise ValueError("not a GGUF file")
    version = _u32(f)
    if version not in (2, 3):
        # v1 used u32 counts/lengths; misreading them as u64 produces
        # absurd allocation sizes, so refuse cleanly instead.
        raise ValueError(f"unsupported GGUF version {version}")
    _u64(f)                    # tensor count
    n_kv = _u64(f)
    for _ in range(n_kv):
        key = _str(f)
        t = _u32(f)
        yield key, _value(f, t)
