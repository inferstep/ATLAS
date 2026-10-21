"""Release-tag signing is wired end to end.

Asserts the pieces that make signed release tags verifiable exist and
agree: the allowed-signers file, the signing/verify workflow, and the
release helper. Does not create tags or touch keys.
"""

from pathlib import Path

REPO = Path(__file__).resolve().parents[2]


def test_allowed_signers_present_and_nonempty():
    f = REPO / ".github" / "allowed_signers"
    assert f.exists(), "missing .github/allowed_signers"
    signer_lines = [ln for ln in f.read_text().splitlines()
                    if ln.strip() and not ln.startswith("#")]
    assert signer_lines, "allowed_signers has no signer entries"
    # each entry: <principal> <key-type> <key...>
    for ln in signer_lines:
        parts = ln.split()
        assert len(parts) >= 3, f"malformed signer line: {ln!r}"
        assert parts[1].startswith(("ssh-", "sk-ssh-")), ln


def test_verify_tags_workflow_verifies_signature():
    wf = REPO / ".github" / "workflows" / "verify-tags.yml"
    assert wf.exists(), "missing verify-tags workflow"
    text = wf.read_text()
    assert "git verify-tag" in text
    # The command, not only the file's name: the name also stands in the comment at the top of the workflow.
    assert "git config gpg.ssh.allowedSignersFile .github/allowed_signers" in text
    assert "tags:" in text and "v*.*.*" in text


def test_release_script_signs_and_does_not_push():
    s = (REPO / "scripts" / "release-tag.sh").read_text()
    assert "git tag -s" in s, "release script must create a SIGNED tag"
    assert "git verify-tag" in s, "release script must verify the signature"
    # Must not auto-push — the push is a deliberate separate step.
    assert "git push" not in s.replace("git push origin $TAG\"", ""), \
        "release script should print, not run, the push"
