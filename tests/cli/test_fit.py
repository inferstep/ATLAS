"""Runtime sizing (`atlas tier fit`) and the GGUF header reading it rests on.

The GGUF files are built with the standard library: the CI lock has no gguf
package.
"""

import struct

import pytest

from atlas import env as cli_env
from atlas.commands import fit, tier
from atlas.commands.fit import GIB, GGUFMeta, fit_runtime_knobs, read_gguf_meta
from atlas.gguf import read_gguf_kv

# GGUF metadata value types, numbered as in the GGUF spec.
UINT32, BOOL, STRING, ARRAY, UINT64, INT64, FLOAT64 = 4, 7, 8, 9, 10, 11, 12
PACK_FORMAT = {UINT32: "<I", BOOL: "<?", UINT64: "<Q", INT64: "<q", FLOAT64: "<d"}


def write_gguf(path, metadata):
    """Write a GGUF v3 file that holds only a header with the given
    (key, type, value) pairs. An ARRAY value is (item type, items)."""
    def packed(value_type, value):
        if value_type == STRING:
            text = value.encode()
            return struct.pack("<Q", len(text)) + text
        if value_type == ARRAY:
            item_type, items = value
            return struct.pack("<IQ", item_type, len(items)) + b"".join(
                packed(item_type, item) for item in items)
        return struct.pack(PACK_FORMAT[value_type], value)

    data = b"GGUF" + struct.pack("<IQQ", 3, 0, len(metadata))  # version, tensors, keys
    for key, value_type, value in metadata:
        data += packed(STRING, key) + struct.pack("<I", value_type) + packed(value_type, value)
    path.write_bytes(data)
    return str(path)


@pytest.mark.parametrize("vram_gib, fits, ubatch", [
    (13, True, 1024),
    (10, False, 512),
], ids=["fits-in-13-gib", "unfit-in-10-gib"])
def test_fit_keeps_embedding_batch_equal_to_microbatch(vram_gib, fits, ubatch):
    """Even an unfit result reports a batch geometry the server can run."""
    model = GGUFMeta(
        path="fixture.gguf", file_size=int(7 * GIB), architecture="fixture",
        n_layers=32, n_embd=4096, n_head=16, head_dim=256, v_len=256,
        k_len_swa=256, v_len_swa=256, kv_heads=[4] * 32,
        n_ctx_train=262144, sliding_window=0, local_mask=[False] * 32,
        swa_note="full attention",
    )

    result = fit_runtime_knobs(model, vram_gib=vram_gib, slots=1)

    assert result.fits is fits
    assert result.ubatch == ubatch
    assert result.batch == result.ubatch


@pytest.mark.parametrize("value_type, value", [
    pytest.param(UINT64, 2**40, id="uint64"),
    pytest.param(INT64, -2**40, id="int64"),
    pytest.param(FLOAT64, 0.125, id="float64"),
])
def test_a_64_bit_value_is_read_whole_and_the_next_key_after_it(tmp_path, value_type, value):
    path = write_gguf(tmp_path / "m.gguf", [
        ("first", value_type, value),
        ("after", UINT32, 7),
    ])

    with open(path, "rb") as f:
        assert list(read_gguf_kv(f)) == [("first", value), ("after", 7)]


@pytest.mark.parametrize("extra_keys, head_dim", [
    pytest.param([("fixture.attention.key_length", UINT32, 256)], 256,
                 id="key-length-from-the-file"),
    pytest.param([], 4096 // 32, id="embedding-per-head-without-key-length"),
])
def test_head_dim_is_the_files_key_length_else_embedding_per_head(tmp_path, extra_keys, head_dim):
    path = write_gguf(tmp_path / "m.gguf", [
        ("general.architecture", STRING, "fixture"),
        ("fixture.block_count", UINT32, 2),
        ("fixture.embedding_length", UINT32, 4096),
        ("fixture.attention.head_count", UINT32, 32),
    ] + extra_keys)

    assert read_gguf_meta(path).head_dim == head_dim


def test_zero_heads_without_key_length_is_an_incomplete_header(tmp_path):
    """No division by zero: `atlas tier fit` reports a ValueError as an
    unreadable header instead of crashing."""
    path = write_gguf(tmp_path / "m.gguf", [
        ("general.architecture", STRING, "fixture"),
        ("fixture.block_count", UINT32, 2),
        ("fixture.embedding_length", UINT32, 4096),
        ("fixture.attention.head_count", UINT32, 0),
    ])

    with pytest.raises(ValueError):
        read_gguf_meta(path)


@pytest.mark.parametrize("pattern_type, pattern", [
    pytest.param(ARRAY, (BOOL, [True, True, False, True, True, False]), id="per-layer-flags"),
    pytest.param(UINT32, 3, id="one-global-layer-in-every-3"),
])
def test_sliding_window_pattern_marks_which_layers_are_local(tmp_path, pattern_type, pattern):
    path = write_gguf(tmp_path / "m.gguf", [
        ("general.architecture", STRING, "fixture"),
        ("fixture.block_count", UINT32, 6),
        ("fixture.embedding_length", UINT32, 4096),
        ("fixture.attention.head_count", UINT32, 32),
        ("fixture.attention.sliding_window", UINT32, 1024),
        ("fixture.attention.sliding_window_pattern", pattern_type, pattern),
    ])

    assert read_gguf_meta(path).local_mask == [True, True, False, True, True, False]


def test_write_env_replaces_every_live_copy_of_a_key_and_appends_new_keys(monkeypatch, tmp_path):
    monkeypatch.setattr(fit, "_atlas_root", lambda: str(tmp_path))
    (tmp_path / ".env").write_text(
        "ATLAS_MODEL_FILE=m.gguf\n"
        "# ATLAS_CTX_SIZE=4096\n"
        "export ATLAS_CTX_SIZE=4096\n"
        "ATLAS_CTX_SIZE=8192\n")

    fit._write_env({"ATLAS_CTX_SIZE": "32768", "ATLAS_UBATCH": "1024"})

    lines = (tmp_path / ".env").read_text().splitlines()
    assert lines[:4] == [
        "ATLAS_MODEL_FILE=m.gguf",
        "# ATLAS_CTX_SIZE=4096",       # a comment stays as it was
        "export ATLAS_CTX_SIZE=32768",
        "ATLAS_CTX_SIZE=32768",        # .env is last-wins: no stale duplicate
    ]
    assert lines[-1] == "ATLAS_UBATCH=1024"


def test_tier_fit_write_sizes_the_configured_model_into_the_install_env(monkeypatch, tmp_path):
    """The configured models dir is relative to the ATLAS install, not to the
    directory `atlas` runs from."""
    (tmp_path / "models").mkdir()
    write_gguf(tmp_path / "models" / "m.gguf", [
        ("general.architecture", STRING, "fixture"),
        ("fixture.block_count", UINT32, 2),
        ("fixture.embedding_length", UINT32, 256),
        ("fixture.attention.head_count", UINT32, 4),
        ("fixture.context_length", UINT32, 16384),
    ])
    (tmp_path / ".env").write_text("ATLAS_MODEL_FILE=m.gguf\n")
    (tmp_path / "elsewhere").mkdir()
    monkeypatch.chdir(tmp_path / "elsewhere")
    monkeypatch.setattr(fit, "_atlas_root", lambda: str(tmp_path))
    monkeypatch.setattr(cli_env, "MODEL_DIR", "./models")
    monkeypatch.setattr(cli_env, "MODEL_FILE", "m.gguf")
    monkeypatch.setattr(tier, "detect_gpu", lambda: [
        tier.GPUInfo(vendor="nvidia", name="test GPU", vram_gb=24.0)])

    assert tier.main(["fit", "--write", "--slots", "2"]) == 0

    env_lines = (tmp_path / ".env").read_text().splitlines()
    assert "ATLAS_MODEL_FILE=m.gguf" in env_lines
    # 24 GiB is far more than this tiny model needs, so each of the 2 slots
    # gets the file's whole training context of 16384.
    assert "ATLAS_CTX_SIZE=32768" in env_lines
    assert "ATLAS_PARALLEL_SLOTS=2" in env_lines
