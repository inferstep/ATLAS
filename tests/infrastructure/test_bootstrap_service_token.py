"""The one-command install makes the service token as `atlas init` does, and the services can read it."""
import re
import stat
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
BOOTSTRAP = ROOT / "scripts" / "atlas-bootstrap.sh"


def shell_function(name):
    return re.search(rf"^{name}\(\) \{{.*?^\}}", BOOTSTRAP.read_text(), re.M | re.S).group()


FUNCTIONS = ("env_file_value", "set_env_file_value", "keep_or_set_env_file_value", "ensure_service_token")
STUBS = """log_ok() { :; }
log_warn() { echo "WARN $*"; }
die() { echo "$*" >&2; exit 1; }
run_as_target() { "$@"; }
stat() { echo 1500; }
"""


def run_step(folder, before="", after=""):
    """The bootstrap's step, for an installing user of uid 1500, with what the case needs on each side."""
    functions = "\n".join(shell_function(name) for name in FUNCTIONS)
    script = f"TARGET_UID=1500\nTARGET_GID=1600\nSUDO=false\n{STUBS}{functions}\n{before}\nensure_service_token\n{after}"
    return subprocess.run(["bash", "-c", script], cwd=folder, capture_output=True, text=True)


def ensure_service_token(folder):
    run_step(folder).check_returncode()


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


def test_a_new_token_is_marked_so_the_services_are_recreated(tmp_path):
    (tmp_path / ".env").write_text("")

    first = run_step(tmp_path, after='echo "new=[${SERVICE_TOKEN_IS_NEW:-}]"')
    second = run_step(tmp_path, after='echo "new=[${SERVICE_TOKEN_IS_NEW:-}]"')

    assert "new=[1]" in first.stdout
    assert "new=[]" in second.stdout


def test_a_new_token_recreates_the_running_services():
    assert "up_args+=(--force-recreate)" in shell_function("start_compose")


def test_a_secrets_folder_that_docker_made_as_root_is_taken_over(tmp_path):
    (tmp_path / ".env").write_text("")
    (tmp_path / "secrets").mkdir()
    taken = 'stat() { echo 0; }\nowner() { echo "$*" > took; }\nSUDO=owner'

    done = run_step(tmp_path, before=taken)

    assert done.returncode == 0, done.stderr
    assert (tmp_path / "took").read_text() == "chown -R 1500:1600 secrets\n"


def test_without_sudo_the_stop_names_the_fix(tmp_path):
    (tmp_path / ".env").write_text("")
    (tmp_path / "secrets").mkdir()

    done = run_step(tmp_path, before="stat() { echo 0; }")

    assert done.returncode == 1
    assert "sudo chown -R 1500:1600 secrets" in done.stderr


def test_a_root_login_is_warned_that_the_services_run_as_root(tmp_path):
    (tmp_path / ".env").write_text("")

    done = run_step(tmp_path, before="TARGET_UID=0\nTARGET_GID=0")

    assert "WARN Installing from a root login" in done.stdout
    assert "ATLAS_PROXY_UID=0\n" in (tmp_path / ".env").read_text()


def test_a_normal_user_gets_no_root_warning(tmp_path):
    (tmp_path / ".env").write_text("")

    done = run_step(tmp_path)

    assert "WARN" not in done.stdout
