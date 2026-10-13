"""Tests for atlas.commands.doctor (focused on #32 macOS hybrid).

The existing doctor checks (gpu, compose, container health, etc) don't
have unit tests yet — they're integration-shaped and mostly tested by
real CI runs. These tests cover the new check_metal_native() added in
#32 because:
  1. It's pure-logic + filesystem (no Docker, no slow network)
  2. The failure modes are exactly the ones a Mac user will hit
     (binary missing, not executable, not listening) and we want
     fast feedback when they regress
  3. The Linux + skip path matters too — the check must be a no-op
     for non-Mac users
"""

import json
import sys

from atlas.commands import doctor


def test_e2e_smoke_uses_public_proxy_path(monkeypatch):
    captured = {}

    class Response:
        def __enter__(self):
            return self

        def __exit__(self, *_args):
            return False

        def read(self):
            return json.dumps({
                "choices": [{
                    "message": {"content": "ATLAS"},
                    "finish_reason": "stop",
                }],
            }).encode()

    def fake_urlopen(request, timeout):
        captured["url"] = request.full_url
        captured["body"] = json.loads(request.data)
        captured["timeout"] = timeout
        return Response()

    monkeypatch.setattr(doctor, "PROXY_URL", "http://proxy.test:8090")
    monkeypatch.setattr(doctor.urllib.request, "urlopen", fake_urlopen)

    result = doctor.check_e2e_smoke()

    assert result.status == "pass"
    assert captured["url"] == "http://proxy.test:8090/v1/chat/completions"
    assert captured["body"]["chat_template_kwargs"]["enable_thinking"] is False
    assert captured["timeout"] == 60


def test_health_endpoint_reports_degraded_as_warning(monkeypatch):
    def fake_get(url, timeout=3):
        status = "degraded" if url.endswith(":8099/health") else "ok"
        return True, json.dumps({"status": status})

    monkeypatch.setattr(doctor, "_http_get", fake_get)
    # The lens URL comes from the checkout's .env at import; pin the default.
    monkeypatch.setattr(doctor, "LENS_URL", "http://127.0.0.1:8099")

    results = {item.name: item for item in doctor.check_health_endpoints()}

    assert results["health/lens"].status == "warn"
    assert results["health/lens"].message == "degraded"
    assert results["health/proxy"].status == "pass"


def test_check_model_file_requires_explicit_selection(monkeypatch, tmp_path):
    monkeypatch.setattr(doctor, "MODEL_FILE", "")
    result = doctor.check_model_file(str(tmp_path))
    assert result.status == "fail"
    assert "ATLAS_MODEL_FILE is not configured" in result.message
    assert "atlas init" in result.detail


def test_lens_weights_report_legacy_bundle_as_uncalibrated(
    monkeypatch, tmp_path
):
    artifact_dir = tmp_path / "lens"
    artifact_dir.mkdir()
    (artifact_dir / "cost_field.pt").write_bytes(b"legacy")
    (artifact_dir / "model_identity.json").write_text('{"model": "Qwen3.5-9B-Q6_K", "embedding_dim": 4096}')
    monkeypatch.setenv("ATLAS_LENS_MODELS", str(artifact_dir))
    monkeypatch.setattr(doctor, "MODEL_NAME", "Qwen3.5-9B-Q6_K")
    monkeypatch.setattr(doctor, "MODEL_FILE", "Qwen3.5-9B-Q6_K.gguf")

    result = doctor.check_lens_weights(str(tmp_path))
    assert result.status == "warn"
    assert "calibrated interventions disabled" in result.message
    assert "cx_normalization.json" in result.detail


def test_doctor_does_not_pass_unmarked_asa_vector(monkeypatch, tmp_path):
    from atlas.commands import asa

    verdict = asa.ASACheckVerdict(
        verdict="needs-build",
        reason="control vector marker is missing; entrypoint keeps it disabled",
        vector_path=str(tmp_path / "ast_edit_steering.gguf"),
        vector_present=True,
    )
    monkeypatch.setattr(asa, "_check_asa", lambda root: verdict)
    result = doctor.check_asa_steering(str(tmp_path))
    assert result.status == "warn"
    assert "disabled" in result.message.lower()
    assert "marker" in result.detail


def test_check_metal_native_skips_on_non_darwin(monkeypatch):
    """On Linux + Windows the metal hybrid path doesn't apply at all.
    The check must return `skip` so it shows up as a no-op in doctor
    output, not as a phantom warn for users who'll never use it."""
    monkeypatch.setattr(sys, "platform", "linux")
    result = doctor._check_metal_native()
    assert result.name == "metal-native"
    assert result.status == "skip"
    assert "macOS" in result.message


def test_check_metal_native_fail_when_binary_missing(monkeypatch, tmp_path):
    """The most common Mac failure mode: setup script was never run,
    so the binary doesn't exist at $HOME/.atlas/macos/bin/. Must point
    the user at the setup script in the detail field."""
    monkeypatch.setattr(sys, "platform", "darwin")
    # Repoint $HOME to an empty tmpdir so the expected binary path
    # definitely doesn't exist. This is more robust than mocking
    # os.path.isfile because the production code also calls os.access.
    monkeypatch.setenv("HOME", str(tmp_path))
    result = doctor._check_metal_native()
    assert result.status == "fail"
    assert "atlas-setup-macos.sh" in result.message
    # The detail should include the expected path so the user knows
    # WHERE the check looked.
    assert "/.atlas/macos/bin/llama-server-metal" in result.detail


def test_check_metal_native_fail_when_binary_not_executable(monkeypatch, tmp_path):
    """Less common but possible: the binary exists but lacks +x (e.g.
    the user copied it from a USB drive with vfat, or rsync'd it
    without --perms). The check should flag this distinctly from
    'binary missing' so the recovery action is clear (re-run setup)."""
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("HOME", str(tmp_path))
    # Create the binary path but without +x.
    bin_dir = tmp_path / ".atlas" / "macos" / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    binary.write_text("#!/bin/sh\necho ok\n")
    binary.chmod(0o644)  # no execute bit

    result = doctor._check_metal_native()
    assert result.status == "fail"
    assert "not executable" in result.message
    assert "--rebuild" in result.message


def test_check_metal_native_warn_when_port_not_listening(monkeypatch, tmp_path):
    """Setup ran cleanly but the user hasn't started the native
    llama-server yet. Warn (not fail) — the binary is fine, they just
    need to run the launcher. Distinct from 'binary missing' because
    the recovery is different (run the launcher, not the setup script)."""
    # atlas.env reads the checkout's .env at import, before conftest hides it,
    # so the port these messages name is pinned to the default here.
    monkeypatch.setattr(doctor, "LLAMA_PORT", 8080)
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("HOME", str(tmp_path))
    bin_dir = tmp_path / ".atlas" / "macos" / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    # Tiny shell script that exits 0 for --help so the executability
    # probe in check_metal_native passes. Real binary would be the
    # llama-server output but for testing we just need exit 0.
    binary.write_text("#!/bin/sh\nexit 0\n")
    binary.chmod(0o755)

    # Binary --help works; _port_listening returns False (port not up).
    monkeypatch.setattr(doctor, "_run",
                        lambda argv, *args, **kwargs: (0, "", ""))
    monkeypatch.setattr(doctor, "_port_listening",
                        lambda host, port, timeout=2.0: False)

    result = doctor._check_metal_native()
    assert result.status == "warn"
    assert "nothing listening on :8080" in result.message
    assert "atlas-llama-macos.sh" in result.message


def test_check_metal_native_pass_when_everything_healthy(monkeypatch, tmp_path):
    """Happy path: binary exists, is executable, runs --help cleanly,
    and the port is listening. This is the steady-state Mac user
    experience after setup + launcher are both done."""
    # atlas.env reads the checkout's .env at import, before conftest hides it,
    # so the port these messages name is pinned to the default here.
    monkeypatch.setattr(doctor, "LLAMA_PORT", 8080)
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("HOME", str(tmp_path))
    bin_dir = tmp_path / ".atlas" / "macos" / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    binary.write_text("#!/bin/sh\nexit 0\n")
    binary.chmod(0o755)

    # --help returns 0; _port_listening returns True (port is up).
    monkeypatch.setattr(doctor, "_run",
                        lambda argv, *args, **kwargs: (0, "", ""))
    monkeypatch.setattr(doctor, "_port_listening",
                        lambda host, port, timeout=2.0: True)

    result = doctor._check_metal_native()
    assert result.status == "pass"
    assert "listening on :8080" in result.message


def test_check_metal_native_honors_custom_prefix(monkeypatch, tmp_path):
    monkeypatch.setattr(sys, "platform", "darwin")
    prefix = tmp_path / "custom-native"
    bin_dir = prefix / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    binary.write_text("#!/bin/sh\nexit 0\n")
    binary.chmod(0o755)
    root = tmp_path / "checkout"
    root.mkdir()
    (root / ".env").write_text(f"ATLAS_MACOS_PREFIX={prefix}\n")
    monkeypatch.setattr(doctor, "_run",
                        lambda argv, *args, **kwargs: (0, "", ""))
    monkeypatch.setattr(doctor, "_port_listening",
                        lambda host, port, timeout=2.0: True)

    result = doctor._check_metal_native(str(root))
    assert result.status == "pass"
    assert str(binary) in result.message


def test_check_metal_native_pass_when_llama_help_exits_nonzero(monkeypatch, tmp_path):
    """Regression for the lead's M3 install: llama-server's --help
    treats the flag as a parse failure and exits 1. The check must NOT
    fail just because exit code is nonzero — it must look at whether
    the binary produced ANY output. A truly corrupt binary (dyld
    failure) produces no output AND exits nonzero; a healthy binary
    that just doesn't return 0 on --help prints its usage."""
    # atlas.env reads the checkout's .env at import, before conftest hides it,
    # so the port these messages name is pinned to the default here.
    monkeypatch.setattr(doctor, "LLAMA_PORT", 8080)
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("HOME", str(tmp_path))
    bin_dir = tmp_path / ".atlas" / "macos" / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    binary.write_text("#!/bin/sh\nexit 0\n")
    binary.chmod(0o755)

    # --help exits 1 but DID print usage (typical llama-server behavior).
    monkeypatch.setattr(doctor, "_run",
                        lambda argv, *args, **kwargs:
                            (1, "usage: llama-server [opts]\n", ""))
    monkeypatch.setattr(doctor, "_port_listening",
                        lambda host, port, timeout=2.0: True)

    result = doctor._check_metal_native()
    # Must PASS — output proves the loader works, exit code is just a
    # convention quirk.
    assert result.status == "pass"
    assert "listening on :8080" in result.message


def test_resolve_backend_prefers_shell_env(monkeypatch, tmp_path):
    """Shell env wins over .env (matches the rest of doctor's env reads)."""
    monkeypatch.setenv("ATLAS_BACKEND", "cuda")
    (tmp_path / ".env").write_text("ATLAS_BACKEND=vulkan\n")
    assert doctor._resolve_backend(str(tmp_path)) == "cuda"


def test_resolve_backend_falls_back_to_env_file(monkeypatch, tmp_path):
    """When ATLAS_BACKEND isn't in the shell, read from .env. Critical
    for macOS hybrid: atlas init writes ATLAS_BACKEND=metal into .env
    but users rarely source it before running doctor."""
    monkeypatch.delenv("ATLAS_BACKEND", raising=False)
    (tmp_path / ".env").write_text("ATLAS_BACKEND=metal\nOTHER=foo\n")
    assert doctor._resolve_backend(str(tmp_path)) == "metal"


def test_resolve_backend_handles_quotes_and_comments(monkeypatch, tmp_path):
    """Real .env files have shell-style quotes + comment lines.
    The reader must strip both."""
    monkeypatch.delenv("ATLAS_BACKEND", raising=False)
    (tmp_path / ".env").write_text(
        "# generated by atlas init\n"
        "\n"
        'ATLAS_BACKEND="metal"\n'
    )
    assert doctor._resolve_backend(str(tmp_path)) == "metal"


def test_resolve_backend_returns_none_when_neither_set(monkeypatch, tmp_path):
    """No shell env + no .env (or .env with no ATLAS_BACKEND) -> None."""
    monkeypatch.delenv("ATLAS_BACKEND", raising=False)
    # No .env at all
    assert doctor._resolve_backend(str(tmp_path)) is None
    # .env exists but no ATLAS_BACKEND line
    (tmp_path / ".env").write_text("ATLAS_MODEL_NAME=foo\n")
    assert doctor._resolve_backend(str(tmp_path)) is None


def test_check_arch_pass_on_apple_silicon(monkeypatch):
    """Apple Silicon (darwin + aarch64) should PASS, not warn. The arm64
    Linux warning about 'no rocm, use vulkan/sbsa/l4t' doesn't apply to
    Mac users who go through the Metal hybrid path (#32)."""
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setattr(doctor.tier, "arch_detect", lambda: "aarch64")
    result = doctor.check_arch()
    assert result.status == "pass"
    assert "Apple Silicon" in result.message
    assert "Metal" in result.message


def test_check_arch_warn_on_aarch64_linux(monkeypatch):
    """aarch64 on Linux still warns — the arm64 server matrix
    (sbsa/l4t/vulkan, no rocm) is real there. This is the
    counterpoint to test_check_arch_pass_on_apple_silicon."""
    monkeypatch.setattr(sys, "platform", "linux")
    monkeypatch.setattr(doctor.tier, "arch_detect", lambda: "aarch64")
    result = doctor.check_arch()
    assert result.status == "warn"
    assert "no rocm" in result.message


def test_check_gpu_apple_silicon_returns_pass(monkeypatch):
    """Apple GPU vendor should PASS the gpu dispatcher check (Metal
    hybrid path is supported via #32). The old 'Metal -> V3.1.2 native
    install' warning was stale and confusing once the path shipped."""
    apple_gpu = doctor.tier.GPUInfo(
        vendor="apple", name="Apple M3 Max", vram_gb=36.0,
        compute_target=None, index=0)
    monkeypatch.setattr(doctor.tier, "detect_gpu", lambda: [apple_gpu])
    monkeypatch.setattr(doctor.tier, "primary_gpu",
                        lambda gpus, **kw: apple_gpu)
    monkeypatch.setattr(doctor.tier, "arch_detect", lambda: "aarch64")
    result = doctor.check_gpu()
    assert result.status == "pass"
    assert "Metal hybrid" in result.message
    assert "not yet supported" not in result.message


def test_check_metal_native_fail_when_binary_crashes(monkeypatch, tmp_path):
    """Edge: corrupt build — binary exists, is executable, but exits
    nonzero on --help (e.g. dynamic linker failure, missing dylib).
    Less common than the 'missing' case but happens after interrupted
    cmake builds. Detail should preserve stderr for debugging."""
    monkeypatch.setattr(sys, "platform", "darwin")
    monkeypatch.setenv("HOME", str(tmp_path))
    bin_dir = tmp_path / ".atlas" / "macos" / "bin"
    bin_dir.mkdir(parents=True)
    binary = bin_dir / "llama-server-metal"
    binary.write_text("#!/bin/sh\nexit 0\n")
    binary.chmod(0o755)

    # --help exits nonzero — simulates a corrupt binary.
    monkeypatch.setattr(doctor, "_run",
                        lambda argv, *args, **kwargs: (127, "", "dyld: Library not loaded"))

    result = doctor._check_metal_native()
    assert result.status == "fail"
    assert "won't run" in result.message
    assert "--rebuild" in result.message
    # The dynamic linker error should land in the detail so the user
    # can paste it into an issue without needing to re-run by hand.
    assert "dyld" in result.detail


def _mount_services():
    return [
        {"Service": "atlas-proxy", "Name": "atlas-atlas-proxy-1", "State": "running"},
        {"Service": "sandbox", "Name": "atlas-sandbox-1", "State": "running"},
    ]


def test_workspace_mounts_pass_when_aligned(monkeypatch):
    """Proxy and sandbox binding the same host dir as /workspace passes."""
    monkeypatch.setattr(doctor, "_run",
                        lambda argv, *a, **k: (0, "/home/user/project\n", ""))
    result = doctor.check_workspace_mounts(_mount_services())
    assert result.status == "pass"
    assert "/home/user/project" in result.message


def test_workspace_mounts_fail_on_split(monkeypatch):
    """The split-brain case (observed 2026-07-18): proxy bound to one host
    dir, sandbox to another. Every /health passes, but file tools and
    run_command operate on different filesystems — doctor must fail loudly
    and name both paths plus the ATLAS_PROJECT_DIR fix."""
    def fake_run(argv, *a, **k):
        if "atlas-atlas-proxy-1" in argv:
            return 0, "/home/user/ATLAS\n", ""
        return 0, "/home/user/demo\n", ""
    monkeypatch.setattr(doctor, "_run", fake_run)
    result = doctor.check_workspace_mounts(_mount_services())
    assert result.status == "fail"
    assert "DIFFERENT" in result.message
    assert "/home/user/ATLAS" in result.detail
    assert "/home/user/demo" in result.detail
    assert "ATLAS_PROJECT_DIR" in result.detail


def test_workspace_mounts_skip_when_not_running():
    """Without both containers running there is nothing to compare."""
    result = doctor.check_workspace_mounts(
        [{"Service": "atlas-proxy", "Name": "x", "State": "running"}])
    assert result.status == "skip"


def test_grammar_mode_check_compares_the_env_with_the_registry(tmp_path):
    """A .env edited by hand, or written before the registry carried the
    mode, can run a model in a configuration it was not measured with."""
    from atlas.commands import doctor
    env = tmp_path / ".env"
    env.write_text("ATLAS_MODEL_FILE=gemma-4-12b-it-Q4_K_M.gguf\n"
                   "ATLAS_GRAMMAR_MODE=strict\n")
    r = doctor.check_grammar_mode(str(tmp_path))
    assert r.status == "warn", r
    assert "loose" in r.message and "ATLAS_GRAMMAR_MODE=loose" in r.detail
    env.write_text("ATLAS_MODEL_FILE=gemma-4-12b-it-Q4_K_M.gguf\n"
                   "ATLAS_GRAMMAR_MODE=loose\n")
    assert doctor.check_grammar_mode(str(tmp_path)).status == "pass"
    # Unset means the proxy's default, strict, which is the Qwen profile.
    env.write_text("ATLAS_MODEL_FILE=Qwen3.5-9B-Q6_K.gguf\n")
    assert doctor.check_grammar_mode(str(tmp_path)).status == "pass"
    # A model the registry does not know has no profile to compare.
    env.write_text("ATLAS_MODEL_FILE=my-own-model.gguf\n")
    assert doctor.check_grammar_mode(str(tmp_path)).status == "skip"



def test_status_dimensions_fail_while_the_agent_is_blocked(monkeypatch):
    """The proxy refuses every request while the lens cannot score and
    points the user at `atlas doctor`; doctor has to say so, not pass."""
    def status(direct_agent, detail):
        return json.dumps({"dimensions": [
            {"name": "model_runtime", "status": "supported", "detail": "ok"},
            {"name": "direct_agent", "status": direct_agent, "detail": detail},
        ]})

    monkeypatch.setattr(doctor, "_http_get", lambda url, timeout=5: (
        True, status("blocked", "requests are refused while the lens cannot score: drifted")))
    [blocked] = doctor.check_status_dimensions()
    assert blocked.status == "fail"
    assert "drifted" in blocked.message

    monkeypatch.setattr(doctor, "_http_get", lambda url, timeout=5: (
        True, status("supported", "the lens can score")))
    [fine] = doctor.check_status_dimensions()
    assert fine.status == "pass"
