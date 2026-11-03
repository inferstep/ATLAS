"""Typed config schema: validation + migration."""

from atlas import config_schema as cs


def test_valid_config_passes():
    env = {"ATLAS_CTX_SIZE": "131072", "ATLAS_BACKEND": "cuda",
           "ATLAS_TRUST_MODE": "trusted", "ATLAS_KEEP_LLAMA_WARM": "1",
           "ATLAS_PROXY_PORT": "8090"}
    r = cs.validate(env)
    assert not r["errors"], r["errors"]


def test_type_range_enum_errors():
    env = {
        "ATLAS_CTX_SIZE": "notanint",       # type
        "ATLAS_PARALLEL_SLOTS": "999",      # range (max 64)
        "ATLAS_BACKEND": "quantum",         # enum
        "ATLAS_TRUST_MODE": "sorta",        # enum
        "ATLAS_PROXY_PORT": "99999",        # port range
        "ATLAS_KEEP_LLAMA_WARM": "maybe",   # bool
    }
    errs = cs.validate(env)["errors"]
    assert len(errs) == 6, errs


def test_unknown_key_warns():
    r = cs.validate({"ATLAS_TYPOED_KEY": "x"})
    assert any("unknown" in w for w in r["warnings"])
    assert not r["errors"]


def test_deprecated_key_warns_not_errors():
    r = cs.validate({"ATLAS_ENABLE_TRAINING": "1"})
    assert any("deprecated" in w for w in r["warnings"])
    assert not r["errors"]


def test_empty_value_is_default_not_error():
    r = cs.validate({"ATLAS_CTX_SIZE": ""})
    assert not r["errors"]


def test_migrate_drops_deprecated_and_stamps_version():
    env = {"ATLAS_CTX_SIZE": "131072", "ATLAS_REGISTRY": "old",
           "ATLAS_ENABLE_TRAINING": "1"}
    migrated, notes = cs.migrate(env)
    assert "ATLAS_REGISTRY" not in migrated
    assert "ATLAS_ENABLE_TRAINING" not in migrated
    assert migrated["ATLAS_CTX_SIZE"] == "131072"
    assert migrated["ATLAS_CONFIG_SCHEMA_VERSION"] == str(
        cs.CONFIG_SCHEMA_VERSION)
    assert len(notes) == 2


def test_env_example_keys_are_in_schema():
    """Every ATLAS_* key documented in .env.example must be known to the
    schema (else it'd falsely warn 'unknown')."""
    import re
    from pathlib import Path
    repo = Path(__file__).resolve().parents[2]
    text = (repo / ".env.example").read_text()
    keys = set(re.findall(r"ATLAS_[A-Z0-9_]+", text))
    unknown = sorted(k for k in keys if k not in cs.SCHEMA)
    assert not unknown, f"keys in .env.example missing from schema: {unknown}"


def test_resolve_precedence_env_over_file_over_default():
    env_file = {"ATLAS_CTX_SIZE": "65536"}
    # process env wins
    assert cs.resolve("ATLAS_CTX_SIZE", env_file, "1024",
                      environ={"ATLAS_CTX_SIZE": "131072"}) == "131072"
    # .env file next
    assert cs.resolve("ATLAS_CTX_SIZE", env_file, "1024",
                      environ={}) == "65536"
    # default last
    assert cs.resolve("ATLAS_CTX_SIZE", {}, "1024", environ={}) == "1024"


def test_resolve_empty_does_not_shadow_lower_layer():
    # empty string in the process env = 'unset', falls through to .env
    assert cs.resolve("ATLAS_MODEL_NAME", {"ATLAS_MODEL_NAME": "gemma"},
                      None, environ={"ATLAS_MODEL_NAME": ""}) == "gemma"


def test_migrate_preserves_comments_and_blanks(tmp_path):
    from atlas.commands import config as cfg
    p = tmp_path / ".env"
    p.write_text("# header comment\nATLAS_CTX_SIZE=131072\n\n"
                 "# deprecated below\nATLAS_REGISTRY=old\n")
    cfg.main(["migrate", str(p)])
    out = p.read_text()
    assert "# header comment" in out          # comments survive
    assert "# deprecated below" in out
    assert "" == out.splitlines()[2].strip() or "\n\n" in out  # blank kept
    assert "ATLAS_REGISTRY" not in out        # deprecated dropped
    assert "ATLAS_CTX_SIZE=131072" in out
    assert "ATLAS_CONFIG_SCHEMA_VERSION=1" in out
    assert (tmp_path / ".env.bak").exists()   # backup written


def test_float_fields_validate_range_and_type():
    """Repetition-sampling knobs are floats; the schema gained a float kind
    for them rather than demoting them to unvalidated strings."""
    ok = cs.validate({"ATLAS_DRY_MULTIPLIER": "0.8", "ATLAS_REPEAT_PENALTY": "1.15"})
    assert ok["errors"] == [], ok["errors"]

    bad_type = cs.validate({"ATLAS_DRY_MULTIPLIER": "aggressive"})
    assert any("expected a number" in e for e in bad_type["errors"])

    # repeat_penalty below 0.5 would boost repetition instead of damping it.
    out_of_range = cs.validate({"ATLAS_REPEAT_PENALTY": "0.1"})
    assert any("below minimum" in e for e in out_of_range["errors"])

    # Integers are valid floats — 1 must be accepted for a float field.
    assert cs.validate({"ATLAS_DRY_BASE": "2"})["errors"] == []


def test_repetition_sampling_keys_are_known():
    """These reach llama-server through docker-compose; an unknown-key
    warning here would mean the .env passthrough is unvalidated."""
    for key in (
        "ATLAS_DRY_MULTIPLIER", "ATLAS_DRY_BASE", "ATLAS_DRY_ALLOWED_LENGTH",
        "ATLAS_DRY_PENALTY_LAST_N", "ATLAS_REPEAT_PENALTY", "ATLAS_REPEAT_LAST_N",
    ):
        assert key in cs.SCHEMA, f"{key} missing from SCHEMA"
