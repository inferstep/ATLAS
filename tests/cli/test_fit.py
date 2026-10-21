"""Runtime sizing invariants not tied to a particular model family."""

import pytest

from atlas.commands.fit import GIB, GGUFMeta, fit_runtime_knobs


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
