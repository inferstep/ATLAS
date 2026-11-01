"""llama-server runs as the user who owns the service token, and a token it cannot read stops it.

`atlas init` makes secrets/ 0700 and the token 0600 for the installing user. A
server under another account used to start with internal auth off and say
nothing. Each entrypoint runs here as shipped, with a stand-in for the server
that records its arguments.
"""

import os
import subprocess
from pathlib import Path

import pytest
import yaml

ROOT = Path(__file__).resolve().parents[3]
ENTRYPOINTS = ["entrypoint-v3.1.sh"]
not_as_root = pytest.mark.skipif(os.geteuid() == 0, reason="root can read a file of any mode")


def compose_services():
    return yaml.safe_load((ROOT / "docker-compose.yml").read_text(encoding="utf-8"))["services"]


def run_entrypoint(tmp_path, name, token_file):
    """Run the shipped entrypoint with a stand-in server. Returns the run and the file of the server's arguments."""
    seen_args = tmp_path / "server-args"
    server = tmp_path / "server"
    server.write_text(f'#!/bin/bash\necho "$@" > "{seen_args}"\n', encoding="utf-8")
    server.chmod(0o755)
    shipped = (ROOT / "inference" / name).read_text(encoding="utf-8")
    script = tmp_path / name
    script.write_text(shipped.replace("/usr/local/bin/llama-server", str(server)), encoding="utf-8")
    env = {"PATH": os.environ["PATH"], "MODEL_PATH": "/models/model.gguf",
           "SLOT_SAVE_PATH": str(tmp_path / "slots"), "ATLAS_SERVICE_TOKEN_FILE": str(token_file)}
    return subprocess.run(["bash", str(script)], env=env, capture_output=True, text=True), seen_args


def test_llama_server_runs_as_the_same_user_as_the_services_that_read_the_token():
    services = compose_services()
    assert services["llama-server"]["user"] == services["atlas-proxy"]["user"]


def test_llama_server_has_a_home_that_any_user_can_write():
    assert "HOME=/tmp" in compose_services()["llama-server"]["environment"]


@pytest.mark.parametrize("name", ENTRYPOINTS)
def test_a_readable_token_turns_internal_auth_on(tmp_path, name):
    secrets = tmp_path / "secrets"
    secrets.mkdir(mode=0o700)
    token = secrets / "service-token"
    token.write_text("token\n", encoding="utf-8")
    token.chmod(0o600)

    done, seen_args = run_entrypoint(tmp_path, name, token)

    assert done.returncode == 0, done.stderr
    assert "Internal auth: enabled" in done.stdout
    assert f"--api-key-file {token}" in seen_args.read_text(encoding="utf-8")


@pytest.mark.parametrize("name", ENTRYPOINTS)
def test_no_secrets_folder_leaves_internal_auth_off(tmp_path, name):
    done, seen_args = run_entrypoint(tmp_path, name, tmp_path / "secrets" / "service-token")

    assert done.returncode == 0, done.stderr
    assert "--api-key-file" not in seen_args.read_text(encoding="utf-8")


@pytest.mark.parametrize("name", ENTRYPOINTS)
def test_a_secrets_folder_with_no_token_leaves_internal_auth_off(tmp_path, name):
    secrets = tmp_path / "secrets"
    secrets.mkdir()

    done, seen_args = run_entrypoint(tmp_path, name, secrets / "service-token")

    assert done.returncode == 0, done.stderr
    assert "--api-key-file" not in seen_args.read_text(encoding="utf-8")


@not_as_root
@pytest.mark.parametrize("name", ENTRYPOINTS)
def test_a_folder_this_account_cannot_enter_stops_the_server(tmp_path, name):
    secrets = tmp_path / "secrets"
    secrets.mkdir()
    (secrets / "service-token").write_text("token\n", encoding="utf-8")
    secrets.chmod(0o600)

    done, seen_args = run_entrypoint(tmp_path, name, secrets / "service-token")
    secrets.chmod(0o700)

    assert done.returncode == 1
    assert "cannot enter" in done.stderr
    assert "ATLAS_PROXY_UID" in done.stderr
    assert "ATLAS_LLAMA_USER" in done.stderr
    assert not seen_args.exists()


@not_as_root
@pytest.mark.parametrize("name", ENTRYPOINTS)
def test_a_token_this_account_cannot_read_stops_the_server(tmp_path, name):
    secrets = tmp_path / "secrets"
    secrets.mkdir()
    token = secrets / "service-token"
    token.write_text("token\n", encoding="utf-8")
    token.chmod(0o000)

    done, seen_args = run_entrypoint(tmp_path, name, token)

    assert done.returncode == 1
    assert "cannot read" in done.stderr
    assert "ATLAS_PROXY_UID" in done.stderr
    assert "ATLAS_LLAMA_USER" in done.stderr
    assert not seen_args.exists()
