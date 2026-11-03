"""Rules the image builds keep: a download goes over HTTPS only, and no chmod in
the sandbox's Dockerfile gives other users write access."""
import re
import subprocess
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
DOCKERFILES = [ROOT / name for name in subprocess.run(
    ["git", "ls-files", "*Dockerfile*"], cwd=ROOT, capture_output=True, text=True, check=True).stdout.split()]


def commands(path):
    """The shell commands of a Dockerfile, one per item: continued lines joined, comments left out."""
    joined = path.read_text(encoding="utf-8").replace("\\\n", " ")
    lines = [line for line in joined.splitlines() if not line.lstrip().startswith("#")]
    return [part.strip() for line in lines for part in re.split(r"&&|\|\||;", line)]


def test_every_download_with_curl_keeps_to_https():
    # `--proto '=https'` stops curl from following a redirect to plain HTTP.
    # A curl with a URL is a download, whether it writes a file or pipes to a shell.
    downloads = [(path.name, command) for path in DOCKERFILES for command in commands(path)
                 if re.search(r"\bcurl\b.*\bhttps?://", command)]
    assert downloads, "no download was found, so this test reads the Dockerfiles wrongly"
    plain = [(name, command) for name, command in downloads if "--proto '=https'" not in command]
    assert plain == []


def lets_others_write(mode):
    """An octal mode whose last digit holds the write bit, or a symbolic one that gives w to o, a or everyone."""
    if mode.isdigit():
        return int(mode[-1]) & 2 != 0
    who, rights = re.fullmatch(r"([ugoa]*)[+=]([rwxX]+)", mode).groups()
    return "w" in rights and (who == "" or "o" in who or "a" in who)


def test_no_chmod_in_the_sandbox_dockerfile_gives_others_write():
    sandbox = commands(ROOT / "sandbox" / "Dockerfile")
    assert sandbox, "no command was read, so this test reads the Dockerfile wrongly"
    modes = [word for command in sandbox if command.startswith(("chmod", "RUN chmod"))
             for word in command.split() if re.fullmatch(r"[0-7]{3,4}|[ugoa]*[+=][rwxX]+", word)]
    assert [mode for mode in modes if lets_others_write(mode)] == []
