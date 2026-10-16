"""atlas onboard — guided drop-in for a new (often unregistered) model.

The north-star UX is "drop in a model → run the lens training → it auto-works".
This command automates the *safe* parts of that and stops at the one step only
the operator can sign off on: rebuilding the inference image. It never rebuilds
llama.cpp itself, because a rebuild can drop ATLAS's custom llama.cpp patches if
done carelessly (see docs/TROUBLESHOOTING.md "Rebuilding llama.cpp for a new
model architecture").

Flow:
    1. Resolve the configured model (ATLAS_MODEL_FILE in .env). With --url, fetch
       it first via `atlas model install --url ...`.
    2. Preflight: ensure the file is on disk; (re)start llama-server.
    3. Arch gate: read the GGUF architecture; confirm llama-server actually
       loaded it. If the bundled llama.cpp doesn't know the arch, print rebuild
       instructions (with the "preserve custom patches" warning) and STOP — the
       operator rebuilds, then re-runs onboard.
    4. Lens check: report whether C(x) needs retraining at the model's dim.
    5. Print the remaining (operator-driven) training steps + a doctor baseline.

Usage:
    atlas onboard                       # onboard the model already in .env
    atlas onboard --url <hf-gguf-url>   # download an unregistered model first
"""

import argparse
import json
import os
import struct
import subprocess
import sys
import time
import urllib.request
from typing import Dict, List, Optional, Tuple

from atlas import compose as compose_config
from atlas.env import atlas_root as _find_atlas_root
from atlas.gguf import read_gguf_kv


# Shared ANSI colors + unicode-safe output primitives.
from atlas.display import (
    RESET, BOLD, DIM, RED, GREEN, YELLOW as YELL,
    DASH, OK_MARK as OK, NO_MARK as NO, WARN_MARK as WARN,
    safe_print as _safe_print,
)


def _c(s: str, color: str, on: bool) -> str:
    return f"{color}{s}{RESET}" if on else s


# --- helpers ----------------------------------------------------------------
def _run(cmd: List[str], timeout: int = 60,
         cwd: Optional[str] = None) -> Tuple[int, str, str]:
    try:
        p = subprocess.run(cmd, capture_output=True, text=True,
                           timeout=timeout, cwd=cwd)
        return p.returncode, p.stdout, p.stderr
    except (subprocess.TimeoutExpired, FileNotFoundError) as e:
        return 1, "", str(e)


def _http_ok(url: str, timeout: int = 4) -> bool:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            return resp.status == 200
    except Exception:
        return False


def _gguf_arch(path: str) -> Optional[str]:
    """general.architecture from the GGUF header. None on any parse trouble
    (best-effort — the runtime log scan is the real gate)."""
    try:
        with open(path, "rb") as f:
            for key, val in read_gguf_kv(f):
                if key == "general.architecture":
                    return str(val)
    except (OSError, ValueError, struct.error):
        return None
    return None


def _llama_url(env: Dict[str, str]) -> str:
    port = os.environ.get("ATLAS_LLAMA_PORT") or env.get("ATLAS_LLAMA_PORT", "8080")
    return f"http://localhost:{port}"


# --- the gate: did the engine actually load THIS model? ---------------------
def _loaded_model_name(url: str) -> Optional[str]:
    """The model llama-server currently has loaded, via /v1/models. None if the
    server is unreachable or the response can't be parsed."""
    try:
        with urllib.request.urlopen(f"{url}/v1/models", timeout=4) as resp:
            data = json.loads(resp.read().decode())
        models = data.get("models") or data.get("data") or []
        if models:
            return models[0].get("name") or models[0].get("id")
    except Exception:
        return None
    return None


def _names_match(loaded: Optional[str], model_file: str) -> bool:
    """True iff `loaded` (whatever /v1/models reports — a filename, a /models/
    path, or the ATLAS_MODEL_NAME stem) identifies the same model as
    `model_file`. Compares the basename with ONLY a trailing `.gguf` stripped
    (so `.gguf`-vs-bare-name and full-path forms match), using EQUALITY — never
    substring containment, and never rsplit-on-'.' which would mangle dotted
    names like `family-variant-Q4_K_M` into `family`."""
    if not loaded:
        return False

    def _norm(s: str) -> str:
        b = os.path.basename(s).lower()
        return b[:-5] if b.endswith(".gguf") else b

    return _norm(loaded) == _norm(model_file)


def _serving_this(url: str, model_file: str) -> Tuple[bool, bool]:
    """Returns (healthy, serving_this_model). serving_this_model is True only
    when healthy AND the loaded model matches model_file — except when /v1/models
    can't be introspected, in which case we trust health (the log scan is the
    backstop for true load errors)."""
    if not _http_ok(f"{url}/health"):
        return False, False
    loaded = _loaded_model_name(url)
    if loaded is None:
        return True, True
    return True, _names_match(loaded, model_file)


def _arch_error_excerpt(logs: str) -> Optional[str]:
    low = logs.lower()
    if "unknown architecture" in low or "error loading model" in low:
        return "\n".join(
            line for line in logs.splitlines()
            if "architect" in line.lower() or "error loading" in line.lower()
        )[:600]
    return None


def _arch_supported(atlas_root: str, env: Dict[str, str], model_file: str,
                    start: bool, color: bool) -> Tuple[bool, str]:
    """Returns (serving_this_model, log_excerpt). Confirms llama-server is
    actually serving THIS model — not just that *some* model is up. If a stale
    container is serving a different model (e.g. after a .env change), it is
    force-recreated; if the bundled llama.cpp can't load the architecture, the
    log scan surfaces it as a rebuild-required signal."""
    url = _llama_url(env)
    healthy, serving = _serving_this(url, model_file)
    if serving:
        return True, ""

    backend = compose_config.resolve_backend(atlas_root, values=env)
    if backend == "metal":
        if healthy:
            loaded = _loaded_model_name(url)
            return False, (
                "native Metal llama-server is healthy but serves {!r}, not "
                "{!r}. Stop it and relaunch scripts/atlas-llama-macos.sh "
                "after checking .env.".format(loaded or "an unknown model",
                                                model_file)
            )
        return False, (
            "native Metal llama-server is not reachable at {}. Start it in "
            "another terminal with ./scripts/atlas-llama-macos.sh; onboarding "
            "will not start the CUDA container on a Metal host.".format(url)
        )

    def compose_args(args: List[str]) -> List[str]:
        return compose_config.command(atlas_root, args, values=env)

    if start:
        if healthy:
            _safe_print(f"  {DASH} llama-server is up but serving a different "
                        f"model — recreating to load this one…")
            _run(compose_args(["up", "-d", "--force-recreate",
                               "llama-server", "--no-deps"]),
                 timeout=120, cwd=atlas_root)
        else:
            _safe_print(f"  {DASH} starting llama-server (inference only)…")
            _run(compose_args(["up", "-d", "llama-server", "--no-deps"]),
                 timeout=120, cwd=atlas_root)
        # Poll for up to ~2.5 min while the model loads onto the GPU.
        for _ in range(30):
            time.sleep(5)
            healthy, serving = _serving_this(url, model_file)
            if serving:
                return True, ""
            rc, logs, _ = _run(compose_args(
                ["logs", "--tail=200", "llama-server"]),
                timeout=20, cwd=atlas_root)
            excerpt = _arch_error_excerpt(logs)
            if excerpt:
                return False, excerpt

    # Not serving this model and either not starting or timed out — inspect logs.
    rc, logs, _ = _run(compose_args(
        ["logs", "--tail=200", "llama-server"]),
        timeout=20, cwd=atlas_root)
    excerpt = _arch_error_excerpt(logs)
    if excerpt:
        return False, excerpt
    _, serving = _serving_this(url, model_file)
    if serving:
        return True, ""
    return False, (
        "(llama-server isn't serving this model and no clear arch error in logs "
        "— it may still be loading; re-run `atlas onboard` shortly or check "
        "`docker compose logs -f llama-server`.)")


def _print_rebuild_required(arch: Optional[str], excerpt: str, color: bool,
                            atlas_root: str, env: Dict[str, str]) -> None:
    archname = f"'{arch}'" if arch else "this model's architecture"
    _safe_print()
    _safe_print(_c(f"{NO} REBUILD REQUIRED — your atlas-llama image can't load "
                   f"{archname}.", RED, color))
    if excerpt:
        _safe_print(_c(excerpt, DIM, color))
    _safe_print()
    _safe_print("  The bundled llama.cpp predates this architecture. You must "
                "rebuild the inference image yourself:")
    build_cmd = compose_config.format_command(
        atlas_root, ["build", "llama-server"], values=env)
    start_cmd = compose_config.format_command(
        atlas_root, ["up", "-d", "llama-server", "--no-deps"], values=env)
    _safe_print(_c(f"    {build_cmd}", BOLD, color))
    _safe_print(_c(f"    {start_cmd}", BOLD, color))
    _safe_print()
    _safe_print(_c(f"  {WARN} Do NOT strip ATLAS's custom llama.cpp patches.",
                   YELL, color))
    _safe_print("  The build re-applies inference/patches/expose-hidden-states.patch")
    _safe_print("  (PC-202 — the per-layer hidden_states extension the Geometric")
    _safe_print("  Lens depends on). If upstream has drifted the `git apply` step")
    _safe_print("  fails and the build aborts — REBASE the patch, don't delete it")
    _safe_print("  or remove the git-apply line. Step-by-step:")
    _safe_print(_c("    docs/TROUBLESHOOTING.md "
                   "\"Rebuilding llama.cpp for a new model architecture\"", DIM, color))
    _safe_print()
    _safe_print("  When the rebuild loads the model, re-run `atlas onboard` to "
                "continue.")


# --- main -------------------------------------------------------------------
def main(argv: Optional[List[str]] = None) -> int:  # noqa: C901
    parser = argparse.ArgumentParser(
        prog="atlas onboard",
        description="Guided drop-in for a new model: arch check, rebuild gate, "
                    "lens-retrain guidance.")
    parser.add_argument("model", nargs="?", default=None,
        help="optional: a GGUF download URL to fetch first (shorthand for "
             "--url). Omit to onboard the model already configured in .env.")
    parser.add_argument("--url", default=None,
        help="download an unregistered model from this URL first "
             "(delegates to `atlas model install --url`)")
    parser.add_argument("--file", default=None,
        help="on-disk filename for --url (default: basename of the URL)")
    parser.add_argument("--apply", action="store_true",
        help="with --url: write ATLAS_MODEL_FILE + ATLAS_MODEL_NAME into "
             ".env after the download without prompting")
    parser.add_argument("--models-dir", default=None,
        help="override ATLAS_MODELS_DIR")
    parser.add_argument("--no-start", action="store_true",
        help="don't (re)start llama-server; only inspect current state")
    parser.add_argument("--no-color", action="store_true")
    args = parser.parse_args(argv)

    # `atlas onboard <url>` is shorthand for `atlas onboard --url <url>`. A bare
    # positional that isn't a URL is rejected — registry models install via
    # `atlas model install <name>` (onboard then reads it from .env).
    if args.model and not args.url:
        if args.model.startswith(("http://", "https://")):
            args.url = args.model
        else:
            print(f"  onboard takes a download URL (or nothing). For a registry "
                  f"model run `atlas model install {args.model}` then `atlas "
                  f"onboard`.")
            return 1

    color = sys.stdout.isatty() and not args.no_color
    atlas_root = _find_atlas_root()
    env = compose_config.read_env_file(atlas_root)

    hdr = f"{BOLD}ATLAS onboard{RESET}" if color else "ATLAS onboard"
    _safe_print(f"{hdr} {DASH} drop-in a model")
    _safe_print()

    # Step 1 — optionally fetch an unregistered model first.
    if args.url:
        _safe_print("[1/5] Fetching unregistered model via `atlas model install "
                    "--url`…")
        from atlas.commands import model as model_cmd
        inst_args = ["install", "--url", args.url]
        if args.file:
            inst_args += ["--file", args.file]
        if args.models_dir:
            inst_args += ["--models-dir", args.models_dir]
        rc = model_cmd.main(inst_args)
        if rc != 0:
            _safe_print(_c(f"{NO} download failed — see above.", RED, color))
            return rc
        # Offer to wire .env to the downloaded file (the install path has
        # already validated the .gguf filename).
        from urllib.parse import urlparse, unquote
        fname = args.file or unquote(os.path.basename(urlparse(args.url).path))
        apply_env = args.apply
        if not apply_env and sys.stdin.isatty() and sys.stdout.isatty():
            try:
                ans = input("  Update .env now? [Y/n] ").strip().lower()
            except EOFError:
                ans = "n"
            apply_env = ans in ("", "y", "yes")
        if apply_env:
            from atlas.commands import fit as fit_module
            env_path = fit_module._write_env({
                "ATLAS_MODEL_FILE": fname,
                "ATLAS_MODEL_NAME": fname.rsplit(".", 1)[0],
            })
            _safe_print(_c(f"  {OK} wrote ATLAS_MODEL_FILE + ATLAS_MODEL_NAME "
                           f"to {env_path}.", GREEN, color))
            _safe_print("  Re-run `atlas onboard` to continue with the arch "
                        "and lens checks.")
            return 0
        _safe_print(_c("  Now set ATLAS_MODEL_FILE + ATLAS_MODEL_NAME in .env to "
                       "this file, then re-run `atlas onboard`.", YELL, color))
        _safe_print("  (onboard reads the model from .env; pass --apply to "
                    "write it automatically.)")
        return 0

    # Step 1 — resolve the configured model from .env.
    model_file = (os.environ.get("ATLAS_MODEL_FILE")
                  or env.get("ATLAS_MODEL_FILE"))
    if not model_file:
        _safe_print(_c(f"{NO} No ATLAS_MODEL_FILE set in .env or environment.",
                       RED, color))
        _safe_print("  Set it in .env (see docs/CONFIGURATION.md \"Adding your "
                    "own model\"), or `atlas onboard --url <hf-url>`.")
        return 1

    models_dir = (args.models_dir or os.environ.get("ATLAS_MODELS_DIR")
                  or env.get("ATLAS_MODELS_DIR", "./models"))
    base = models_dir if os.path.isabs(models_dir) else os.path.join(atlas_root,
                                                                     models_dir)
    model_path = os.path.normpath(os.path.join(base, model_file))

    _safe_print(f"[1/5] Model: {_c(model_file, BOLD, color)}")
    if not os.path.exists(model_path):
        _safe_print(_c(f"  {NO} not found at {model_path}", RED, color))
        _safe_print("  Place the .gguf there, or `atlas model install --url "
                    "<hf-url>`. See docs/CONFIGURATION.md \"Adding your own "
                    "model\".")
        return 1
    gb = os.path.getsize(model_path) / (1024 ** 3)
    arch = _gguf_arch(model_path)
    _safe_print(f"  {OK} present ({gb:.1f} GB)"
                + (f", arch '{arch}'" if arch else ""))

    # Runtime sizing (PC-208) — advisory; the engine runs with --fit off,
    # so a configuration that doesn't fit refuses to start rather than
    # spilling layers to CPU.
    try:
        from atlas.commands import fit as fit_mod
        from atlas.commands.tier import detect_gpu, primary_gpu
        gpu = primary_gpu(detect_gpu())
        if gpu is not None:
            meta = fit_mod.read_gguf_meta(model_path)
            # Compare like-for-like: fit at the slot count .env is sized for.
            try:
                slots = int(os.environ.get("ATLAS_PARALLEL_SLOTS")
                            or env.get("ATLAS_PARALLEL_SLOTS")
                            or os.environ.get("PARALLEL_SLOTS")
                            or env.get("PARALLEL_SLOTS") or 4)
            except ValueError:
                slots = 4
            res = fit_mod.fit_runtime_knobs(meta, gpu.vram_gb,
                                            slots=max(1, slots))
            if res.fits:
                _safe_print(f"  fit: ctx {res.ctx_total} ({res.per_slot}/slot "
                            f"× {res.parallel}), KV {res.kv_type_k}, "
                            f"ubatch {res.ubatch} on {gpu.name}")
                cur_ctx = (os.environ.get("ATLAS_CTX_SIZE")
                           or env.get("ATLAS_CTX_SIZE"))
                try:
                    stale = (cur_ctx is not None
                             and int(cur_ctx) != res.ctx_total)
                except ValueError:
                    stale = True   # non-numeric ATLAS_CTX_SIZE needs fixing
                if stale:
                    _safe_print(_c("  .env sizing differs — apply with: "
                                   "atlas tier fit --write", YELL, color))
            else:
                _safe_print(_c(f"  {NO} does not fit on {gpu.name}: "
                               f"{res.note}", RED, color))
                _safe_print("  Details: atlas tier fit")
    except Exception as e:
        _safe_print(_c(f"  (runtime fit unavailable: {e})", DIM, color))

    # Step 2/3 — preflight + arch gate.
    _safe_print("[2/5] Checking the inference engine can load it…")
    loaded, excerpt = _arch_supported(atlas_root, env, model_file,
                                      start=not args.no_start, color=color)
    if not loaded:
        if compose_config.resolve_backend(atlas_root, values=env) == "metal":
            _safe_print()
            _safe_print(_c(f"{NO} Native Metal inference is not ready.",
                           RED, color))
            _safe_print(excerpt)
            _safe_print("  Re-run `atlas onboard` after the native server is "
                        "healthy.")
        else:
            _print_rebuild_required(arch, excerpt, color, atlas_root, env)
        return 2  # action required: operator must rebuild
    _safe_print(f"  {OK} llama-server is serving the model (arch supported).")

    # Step 4 — lens check.
    _safe_print("[3/5] Geometric Lens dimension check…")
    try:
        from atlas.commands import lens as lens_cmd
        lens_cmd.main(["check"] + (["--no-color"] if args.no_color else []))
    except SystemExit:
        # lens check is invoked as a nested CLI and reports its own verdict;
        # onboarding continues to print the remaining operator steps.
        pass
    except Exception as e:
        _safe_print(_c(f"  (lens check unavailable: {e})", DIM, color))

    # Step 5 — remaining operator-driven steps (concrete for THIS model).
    run_id = os.path.splitext(os.path.basename(model_file))[0] + "_lens"
    _safe_print("[4/5] Remaining steps (operator-driven — candidate gen is hours "
                "on a large model):")
    _safe_print("  1) Generate this model's own candidates:")
    _safe_print(_c(f"     atlas bench --run-id {run_id} --tasks 200", BOLD, color))
    results_path = os.path.join(atlas_root, "benchmark", "results", run_id,
                                "v3_lcb", "per_task")
    _safe_print("  2) Retrain the lens — C(x) and G(x) — on those candidates "
                "(--force: replace the previous model's artifacts), then "
                "restart the service to load them:")
    _safe_print(_c(f"     atlas lens build --force --from-results "
                   f"{results_path}", BOLD, color))
    _safe_print(_c("     docker compose restart geometric-lens", BOLD, color))
    _safe_print("  3) ASA control vector (N ~= 75% of the model's layer count):")
    _safe_print(_c("     atlas asa build --layer N", BOLD, color))
    _safe_print("  Do not reuse another model's solutions — C(x) must learn THIS "
                "model's geometry.")

    _safe_print("[5/5] Baseline: run `atlas doctor` to confirm health "
                "(lens dim-mismatch is expected until you retrain).")
    _safe_print()
    _safe_print(_c(f"{OK} Engine ready for {model_file}. Lens retrain is the "
                   f"last mile.", GREEN, color))
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
