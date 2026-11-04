"""Runtime sizing (`atlas tier fit`) and the GGUF header reading it rests on.

The GGUF files are built with the standard library: the CI lock has no gguf
package.
"""

import json
import struct

import pytest

from atlas import display, env as cli_env
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


@pytest.mark.parametrize("array, items", [
    pytest.param((UINT32, [1, 2, 3]), [1, 2, 3], id="numbers"),
    pytest.param((STRING, ["a", "b"]), ["a", "b"], id="strings"),
    # Over 4096 items, an array is passed over unread: a tokenizer table.
    pytest.param((UINT32, [0] * 4097), [], id="numbers-over-4096-skipped"),
    pytest.param((STRING, ["x"] * 4097), [], id="strings-over-4096-skipped"),
    pytest.param((ARRAY, [(UINT32, [1]), (UINT32, [2, 3])]), [[1], [2, 3]], id="arrays"),
])
def test_an_array_is_read_or_passed_over_and_the_next_key_after_it(tmp_path, array, items):
    path = write_gguf(tmp_path / "m.gguf", [
        ("first", ARRAY, array),
        ("after", UINT32, 7),
    ])

    with open(path, "rb") as f:
        assert list(read_gguf_kv(f)) == [("first", items), ("after", 7)]


@pytest.mark.parametrize("header, message", [
    pytest.param(b"GGML" + struct.pack("<IQQ", 3, 0, 0), "not a GGUF file", id="another-magic"),
    pytest.param(b"GGUF" + struct.pack("<IQQ", 1, 0, 0), "unsupported GGUF version 1", id="version-1"),
])
def test_a_file_that_is_not_gguf_2_or_3_is_refused(tmp_path, header, message):
    path = tmp_path / "m.gguf"
    path.write_bytes(header)

    with open(path, "rb") as f, pytest.raises(ValueError, match=message):
        list(read_gguf_kv(f))


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


# The model the `atlas tier fit` tests below size: 2 small layers with a
# sliding window but no pattern, so every fit carries the "assumed" note.
SMALL_MODEL = [
    ("general.architecture", STRING, "fixture"),
    ("fixture.block_count", UINT32, 2),
    ("fixture.embedding_length", UINT32, 256),
    ("fixture.attention.head_count", UINT32, 4),
    ("fixture.context_length", UINT32, 16384),
    ("fixture.attention.sliding_window", UINT32, 1024),
]
GPU_24_GIB = [tier.GPUInfo(vendor="nvidia", name="test GPU", vram_gb=24.0)]
GPU_HALF_GIB = [tier.GPUInfo(vendor="nvidia", name="test GPU", vram_gb=0.5)]


def an_install(monkeypatch, tmp_path, gpus):
    """An ATLAS install at tmp_path whose .env names models/m.gguf (SMALL_MODEL),
    on a host with these GPUs. `atlas` runs from tmp_path/elsewhere."""
    (tmp_path / "models").mkdir()
    write_gguf(tmp_path / "models" / "m.gguf", SMALL_MODEL)
    (tmp_path / ".env").write_text("ATLAS_MODEL_FILE=m.gguf\n")
    (tmp_path / "elsewhere").mkdir()
    monkeypatch.chdir(tmp_path / "elsewhere")
    monkeypatch.setattr(fit, "_atlas_root", lambda: str(tmp_path))
    monkeypatch.setattr(cli_env, "MODEL_DIR", "./models")
    monkeypatch.setattr(cli_env, "MODEL_FILE", "m.gguf")
    monkeypatch.setattr(tier, "detect_gpu", lambda: gpus)


@pytest.mark.parametrize("argv, gpus, exit_code, message", [
    pytest.param(["fit", "missing.gguf"], GPU_24_GIB, 1, "Model file not found: missing.gguf",
                 id="a-model-file-that-is-not-there"),
    pytest.param(["fit", "not-gguf.bin"], GPU_24_GIB, 1, "Could not read GGUF header",
                 id="a-file-that-is-not-gguf"),
    pytest.param(["fit"], [], 1, "No GPU with reported VRAM detected", id="no-gpu"),
    pytest.param(["fit"], [tier.GPUInfo(vendor="nvidia", name="test GPU", vram_gb=0)], 1,
                 "No GPU with reported VRAM detected", id="a-gpu-without-vram"),
    pytest.param(["fit", "--slots", "0"], GPU_24_GIB, 2, "--slots must be at least 1.", id="no-slots"),
    pytest.param(["fit"], GPU_HALF_GIB, 1, "DOES NOT FIT: model does not fit", id="a-gpu-too-small"),
])
def test_fit_says_why_it_cannot_size_the_model(monkeypatch, tmp_path, capsys, argv, gpus, exit_code, message):
    an_install(monkeypatch, tmp_path, gpus)
    (tmp_path / "elsewhere" / "not-gguf.bin").write_bytes(b"not a GGUF file")

    assert tier.main(argv) == exit_code
    assert message in capsys.readouterr().out


def test_fit_without_a_configured_model_says_how_to_name_one(monkeypatch, tmp_path, capsys):
    an_install(monkeypatch, tmp_path, GPU_24_GIB)
    monkeypatch.setattr(cli_env, "MODEL_FILE", "gone.gguf")

    assert tier.main(["fit"]) == 1
    assert "No model configured." in capsys.readouterr().out


def test_fit_json_reports_the_model_the_fit_and_its_env(monkeypatch, tmp_path, capsys):
    an_install(monkeypatch, tmp_path, GPU_24_GIB)

    assert tier.main(["fit", "--json"]) == 0

    report = json.loads(capsys.readouterr().out)
    assert report["model"] == str(tmp_path / "models" / "m.gguf")
    assert report["fit"]["parallel"] == 4  # the default slot count
    assert report["env"]["ATLAS_CTX_SIZE"] == "65536"
    assert "wrote" not in report
    assert (tmp_path / ".env").read_text() == "ATLAS_MODEL_FILE=m.gguf\n"


def test_fit_json_write_names_the_env_file_it_wrote(monkeypatch, tmp_path, capsys):
    an_install(monkeypatch, tmp_path, GPU_24_GIB)

    assert tier.main(["fit", "--json", "--write"]) == 0

    assert json.loads(capsys.readouterr().out)["wrote"] == str(tmp_path / ".env")
    assert "ATLAS_CTX_SIZE=65536" in (tmp_path / ".env").read_text().splitlines()


def test_a_json_no_fit_has_no_env_and_writes_nothing(monkeypatch, tmp_path, capsys):
    an_install(monkeypatch, tmp_path, GPU_HALF_GIB)

    assert tier.main(["fit", "--json", "--write"]) == 1

    report = json.loads(capsys.readouterr().out)
    assert report["env"] == {}
    assert "wrote" not in report
    assert (tmp_path / ".env").read_text() == "ATLAS_MODEL_FILE=m.gguf\n"


def test_the_fit_report_shows_the_budget_the_fit_and_how_to_write_it(monkeypatch, tmp_path, capsys):
    an_install(monkeypatch, tmp_path, GPU_24_GIB)
    monkeypatch.setattr(display, "UNICODE_OK", True)  # read as a UTF-8 terminal shows it

    assert tier.main(["fit"]) == 0

    note = "window 1024, assumed 1 global per 6 layers (pattern not in GGUF)"
    assert capsys.readouterr().out.splitlines() == [
        "atlas tier fit — m.gguf",
        f"  arch fixture | 2 layers | 256-dim | head_dim 64 | {note}",
        "  GPU: test GPU (24.0 GiB)",
        "  budget: weights 0.0 + KV 0.01 + compute 0.4 + reserve 1.9 GiB of 24.0 GiB",
        "  fit: ctx 65536 (16384/slot × 4), KV f16, ubatch 512",
        f"  note: {note}",
        "    ATLAS_CTX_SIZE=65536",
        "    ATLAS_PARALLEL_SLOTS=4",
        "    ATLAS_KV_TYPE_K=f16",
        "    ATLAS_KV_TYPE_V=f16",
        "    ATLAS_UBATCH=512",
        "    ATLAS_BATCH=512",
        "  (re-run with --write to update .env)",
    ]
    assert (tmp_path / ".env").read_text() == "ATLAS_MODEL_FILE=m.gguf\n"


@pytest.mark.parametrize("run_from, warned", [
    pytest.param("elsewhere", True, id="another-deployment"),
    pytest.param(".", False, id="the-install-reached-by-another-path"),
])
def test_fit_write_warns_when_atlas_runs_from_another_deployment(monkeypatch, tmp_path, capsys, run_from, warned):
    an_install(monkeypatch, tmp_path, GPU_24_GIB)
    (tmp_path / "link").symlink_to(tmp_path)
    monkeypatch.setattr(fit, "_atlas_root", lambda: str(tmp_path / "link"))
    (tmp_path / run_from / "docker-compose.yml").write_text("")
    monkeypatch.chdir(tmp_path / run_from)

    assert tier.main(["fit", "--write"]) == 0

    out = capsys.readouterr().out
    assert f"  wrote {tmp_path / 'link' / '.env'}" in out
    assert "  apply: docker compose up -d llama-server --no-deps --force-recreate" in out
    assert ("docker compose reads the .env next to the docker-compose.yml" in out) == warned
    assert "ATLAS_CTX_SIZE=65536" in (tmp_path / ".env").read_text().splitlines()


# The geometry every header below starts from: 2 layers, 4096-dim, 32 heads.
GEOMETRY = [
    ("general.architecture", STRING, "fixture"),
    ("fixture.block_count", UINT32, 2),
    ("fixture.embedding_length", UINT32, 4096),
    ("fixture.attention.head_count", UINT32, 32),
]


def test_the_header_is_read_up_to_the_tokenizer_once_the_geometry_is_known(tmp_path):
    # A tokenizer key before the geometry does not stop the read; one after it does,
    # so the file's count of keys may name more than are there.
    gguf = tmp_path / "m.gguf"
    path = write_gguf(gguf, [("tokenizer.ggml.model", STRING, "gpt2")] + GEOMETRY
                      + [("tokenizer.ggml.pre", STRING, "default")])
    data = bytearray(gguf.read_bytes())
    struct.pack_into("<Q", data, 16, len(GEOMETRY) + 3)  # one key more than the file holds
    gguf.write_bytes(bytes(data))

    assert read_gguf_meta(path).n_layers == 2


@pytest.mark.parametrize("extra_keys, n_head, kv_heads", [
    pytest.param([], 32, [32, 32], id="kv-heads-default-to-the-heads"),
    pytest.param([("fixture.attention.head_count_kv", UINT32, 8)], 32, [8, 8], id="one-kv-count-for-every-layer"),
    pytest.param([("fixture.attention.head_count_kv", ARRAY, (UINT32, [4, 8]))], 32, [4, 8], id="kv-heads-per-layer"),
    pytest.param([("fixture.attention.head_count_kv", ARRAY, (UINT32, []))], 32, [32, 32], id="an-empty-kv-list"),
    pytest.param([("fixture.attention.head_count", ARRAY, (UINT32, [8, 32]))], 32, [32, 32], id="the-most-heads-of-a-list"),
])
def test_heads_and_kv_heads(tmp_path, extra_keys, n_head, kv_heads):
    meta = read_gguf_meta(write_gguf(tmp_path / "m.gguf", GEOMETRY + extra_keys))
    assert meta.n_head == n_head
    assert meta.kv_heads == kv_heads


@pytest.mark.parametrize("extra_keys, v_len, k_len_swa, v_len_swa", [
    pytest.param([], 128, 128, 128, id="every-length-from-the-head-dim"),
    pytest.param([("fixture.attention.value_length", UINT32, 64)], 64, 128, 64, id="swa-values-from-the-values"),
    pytest.param([("fixture.attention.key_length_swa", UINT32, 96),
                  ("fixture.attention.value_length_swa", UINT32, 48)], 128, 96, 48, id="swa-lengths-from-the-file"),
])
def test_value_and_sliding_window_lengths(tmp_path, extra_keys, v_len, k_len_swa, v_len_swa):
    meta = read_gguf_meta(write_gguf(tmp_path / "m.gguf", GEOMETRY + extra_keys))
    assert meta.v_len == v_len
    assert meta.k_len_swa == k_len_swa
    assert meta.v_len_swa == v_len_swa


@pytest.mark.parametrize("changed_key, value", [
    pytest.param("fixture.block_count", 0, id="no-layers"),
    pytest.param("fixture.embedding_length", 0, id="no-embedding"),
])
def test_a_header_without_its_geometry_is_incomplete(tmp_path, changed_key, value):
    # The key length is given, so the head dim does not come from the embedding.
    keys = [(k, t, value if k == changed_key else v) for k, t, v in GEOMETRY]
    keys.append(("fixture.attention.key_length", UINT32, 128))
    path = write_gguf(tmp_path / "m.gguf", keys)
    with pytest.raises(ValueError, match="GGUF header incomplete"):
        read_gguf_meta(path)


def test_an_empty_head_list_is_an_incomplete_header(tmp_path):
    keys = GEOMETRY[:3] + [("fixture.attention.head_count", ARRAY, (UINT32, []))]
    path = write_gguf(tmp_path / "m.gguf", keys)
    with pytest.raises(ValueError, match="GGUF header incomplete"):
        read_gguf_meta(path)


@pytest.mark.parametrize("extra_keys, local_mask, swa_note", [
    pytest.param([("fixture.attention.sliding_window_pattern", UINT32, 2)], [False, False], "full attention",
                 id="no-window-is-full-attention-whatever-the-pattern"),
    pytest.param([("fixture.attention.sliding_window", UINT32, 512),
                  ("fixture.attention.sliding_window_pattern", ARRAY, (BOOL, [True]))],
                 [True, False], "window 512, per-layer mask (1/2 sliding)", id="a-short-mask-pads-with-global-layers"),
    pytest.param([("fixture.attention.sliding_window", UINT32, 512),
                  ("fixture.attention.sliding_window_pattern", ARRAY, (BOOL, []))],
                 [True, True], "window 512, assumed 1 global per 6 layers (pattern not in GGUF)",
                 id="an-empty-mask-is-the-default-pattern"),
    pytest.param([("fixture.attention.sliding_window", UINT32, 512)],
                 [True, True], "window 512, assumed 1 global per 6 layers (pattern not in GGUF)",
                 id="no-pattern-is-the-default-and-says-so"),
])
def test_sliding_window_layers_and_their_note(tmp_path, extra_keys, local_mask, swa_note):
    meta = read_gguf_meta(write_gguf(tmp_path / "m.gguf", GEOMETRY + extra_keys))
    assert meta.local_mask == local_mask
    assert meta.swa_note == swa_note


# An 8-layer, 4096-dim model with 8 K/V heads of 128 on every layer: 16384 K+V
# elements per token over its global layers when none slides.
FULL_ATTENTION = dict(
    path="m.gguf", file_size=int(4 * GIB), architecture="fixture", n_layers=8, n_embd=4096, n_head=32,
    head_dim=128, v_len=128, k_len_swa=128, v_len_swa=128, kv_heads=[8] * 8, n_ctx_train=0,
    sliding_window=0, local_mask=[False] * 8, swa_note="full attention",
)


@pytest.mark.parametrize("changes, vram_gib, slots, expected", [
    pytest.param({}, 48, 4,
                 {"per_slot": 32768, "ctx_total": 131072, "kv_gib": 4.0, "note": ""},
                 id="ample-memory-reaches-the-per-slot-cap"),
    pytest.param({"n_ctx_train": 20000}, 48, 2,
                 {"per_slot": 19456, "ctx_total": 38912, "kv_gib": 1.19, "note": ""},
                 id="the-trained-context-caps-a-slot-rounded-down"),
    pytest.param({"sliding_window": 1024, "local_mask": [True, True, True, False] * 2,
                  "swa_note": "window 1024, assumed 1 global per 4 layers"}, 12, 2,
                 {"per_slot": 32768, "ctx_total": 65536, "kv_gib": 0.59,
                  "note": "window 1024, assumed 1 global per 4 layers"},
                 id="sliding-layers-cost-slots-times-window-plus-a-batch"),
    pytest.param({"sliding_window": 1024, "local_mask": [True] * 8,
                  "swa_note": "window 1024, per-layer mask (8/8 sliding)"}, 12, 2,
                 {"per_slot": 32768, "ctx_total": 65536, "kv_gib": 0.12, "note": ""},
                 id="an-all-sliding-model-gets-the-cap-for-every-slot"),
    pytest.param({}, 6, 2, {"fits": False, "kv_gib": 0.27},
                 id="no-fit-accounts-the-minimum-context-at-q8"),
    pytest.param({}, 6.8, 2, {"per_slot": 8192, "ctx_total": 16384, "kv_type_k": "q8_0"},
                 id="exactly-the-minimum-per-slot-fits"),
    pytest.param({"sliding_window": 1024, "local_mask": [True, True, True, False] * 2,
                  "swa_note": "window 1024, assumed 1 global per 4 layers"}, 6.6, 2, {"fits": False},
                 id="the-sliding-cache-is-paid-before-the-context"),
    pytest.param({"n_embd": 512}, 6.4, 2, {"fits": False, "compute_gib": 0.4},
                 id="a-small-model-still-pays-the-compute-floor"),
])
def test_the_fit_sizes_each_slot_from_what_is_left(changes, vram_gib, slots, expected):
    result = fit_runtime_knobs(GGUFMeta(**{**FULL_ATTENTION, **changes}), vram_gib=vram_gib, slots=slots)
    assert {key: getattr(result, key) for key in expected} == expected
