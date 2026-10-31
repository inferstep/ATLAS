"""The one-command install makes the service token as `atlas init` does, and the services can read it."""
import re
import stat
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
BOOTSTRAP = ROOT / "scripts" / "atlas-bootstrap.sh"


def shell_function(name):
    return re.search(rf"^{name}\(\) \{{.*?^\}}", BOOTSTRAP.read_text(), re.M | re.S).group()


def ensure_service_token(folder):
    functions = "\n".join(shell_function(name) for name in (
        "env_file_value", "set_env_file_value", "keep_or_set_env_file_value", "ensure_service_token"))
    stubs = 'log_ok() { :; }\ndie() { echo "$*" >&2; exit 1; }\nrun_as_target() { "$@"; }\n'
    subprocess.run(["bash", "-c", f"TARGET_UID=1500\nTARGET_GID=1600\n{stubs}{functions}\nensure_service_token"],
                   cwd=folder, check=True)


def test_a_first_install_gets_a_token_only_its_owner_can_read(tmp_path):
    (tmp_path / ".env").write_text("")

    ensure_service_token(tmp_path)

    folder = tmp_path / "secrets"
    token = folder / "service-token"
    assert stat.S_IMODE(folder.stat().st_mode) == 0o700
    assert stat.S_IMODE(token.stat().st_mode) == 0o600
    assert re.fullmatch(r"atlas-st-[A-Za-z0-9_-]{43}\n", token.read_text())


def test_a_re_run_keeps_the_token(tmp_path):
    (tmp_path / ".env").write_text("")
    ensure_service_token(tmp_path)
    token = (tmp_path / "secrets" / "service-token").read_text()

    ensure_service_token(tmp_path)

    assert (tmp_path / "secrets" / "service-token").read_text() == token


def test_the_services_run_as_the_user_who_owns_the_token(tmp_path):
    (tmp_path / ".env").write_text("")

    ensure_service_token(tmp_path)

    body = (tmp_path / ".env").read_text()
    assert "ATLAS_PROXY_UID=1500\n" in body
    assert "ATLAS_PROXY_GID=1600\n" in body
    assert "ATLAS_SANDBOX_UID=1500\n" in body
    assert "ATLAS_SANDBOX_GID=1600\n" in body


def test_ids_already_in_env_stay(tmp_path):
    (tmp_path / ".env").write_text("ATLAS_PROXY_UID=1234\n")

    ensure_service_token(tmp_path)

    body = (tmp_path / ".env").read_text()
    assert "ATLAS_PROXY_UID=1234\n" in body
    assert "ATLAS_PROXY_UID=1500" not in body
