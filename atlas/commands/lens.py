"""atlas lens — Geometric Lens probe + build pipeline (PC-057, PC-058).

Two subcommands wrap the existing geometric-lens training code into a
model-path-driven workflow so users can bring their own GGUF and either
verify it's Lens-compatible (`check`) or actually train fresh artifacts
for it (`build`).

Layering:
    PC-057 `atlas lens check`  -> this file, cheap pre-flight
    PC-058 `atlas lens build`  -> this file, wraps training.train_cost_field
    PC-059 `atlas lens push`   -> roadmap, publishes to registry
    PC-060 HF middleman        -> roadmap, automated distribution

Probe contract: both subcommands talk to a *running* llama-server via
its `/embedding` and `/props` endpoints. ATLAS users typically already
have one up (`docker compose up -d`); if not, the commands print a
clear "start the stack first" hint rather than spinning their own
process. Keeping this stateless against an existing server matches the
rest of the atlas CLI surface (model.py, doctor.py, tier.py all assume
some level of running infrastructure for their richer probes).

Invoke:
    atlas lens check                    # probe currently-loaded model
    atlas lens check <model-name>       # probe a registry entry
    atlas lens check /path/to/model.gguf  # probe an arbitrary file
    atlas lens build <name|path>        # train fresh C(x) artifacts
    atlas lens --json                   # machine-readable output for scripts

Exit codes (check):
    0  compat        — artifacts exist + dim matches + server reachable
    1  needs-build   — model loadable, no artifacts at right dim
    2  incompatible  — can't probe (server down, model won't load, PC-202 missing)
"""

import argparse
import json as jsonlib
import os
import sys
import tempfile
import time
from dataclasses import dataclass, asdict
from typing import Dict, List, Optional

from atlas import compose as compose_config
from atlas import env as cli_env
from atlas import publishing
from atlas.client import LlamaProbe, post_json_or_none, probe_llama
from atlas.commands import model_registry
# Moved beside this module, by subject, and imported back under their names.
from atlas.commands.lens_publish import (
    _open_registry_pr, _print_pr_body_for_paste, _print_pr_body_without_gh,
    _render_model_card_md, _render_registry_pr_body, _upload_to_hf,
)
from atlas.commands.lens_samples import (
    _load_results_samples, _load_telemetry_embeddings, _load_training_samples,
)


# Shared ANSI colors + unicode-safe output primitives.
from atlas.display import (
    RESET, BOLD, RED, GREEN, YELLOW as YELL,
    safe_print as _safe_print,
)


# ---------------------------------------------------------------------------
# Artifact resolution + dim inspection
# ---------------------------------------------------------------------------
# The llama-server probe (LlamaProbe, probe_llama) lives in atlas.client
# with the other service HTTP machinery; the publish/registry helpers live
# in atlas.publishing, shared with `atlas asa publish` and
# `atlas publish`.

@dataclass
class ArtifactInspection:
    """Result of looking at the on-disk Lens artifact."""
    present: bool                      # cost_field.pt exists on disk
    dim: Optional[int] = None          # input dim if introspectable
    torch_available: bool = True       # False -> dim couldn't be checked
    error: str = ""


def _inspect_cost_field(artifact_dir: str) -> ArtifactInspection:
    """Look at cost_field.pt and report what we can.

    Three outcomes:
      1. File missing                -> present=False
      2. File present, torch missing -> present=True, dim=None,
                                         torch_available=False
      3. File present, torch present -> present=True, dim=<int>
                                         (or None on a load error,
                                          with `error` set)
    Distinguishing (2) from (3) lets the verdict avoid misleading users
    into a needs-build state when the artifact really exists but the
    host Python just can't peek at it.
    """
    cost_path = os.path.join(artifact_dir, "cost_field.pt")
    if not os.path.isfile(cost_path):
        return ArtifactInspection(present=False)
    try:
        import torch
    except ImportError:
        return ArtifactInspection(present=True, dim=None,
                                  torch_available=False,
                                  error="torch not installed on host")
    # Only in torch's restricted mode, as the lens service loads it: an
    # unrestricted load could run code from the file.
    try:
        state = torch.load(cost_path, map_location="cpu", weights_only=True)
    except Exception as e:
        return ArtifactInspection(present=True, dim=None,
                                  error=f"cost_field.pt cannot be read in torch's restricted mode, so the lens service "
                                        f"would refuse it too; rebuild it with `atlas lens build` ({e})")
    if not isinstance(state, dict):
        return ArtifactInspection(present=True, dim=None,
                                  error="state dict has unexpected shape")
    # CostField.net.0 is the first Linear layer; its weight is (out, in).
    for key in ("net.0.weight", "0.weight"):
        if key in state:
            try:
                return ArtifactInspection(present=True,
                                          dim=int(state[key].shape[1]))
            except Exception:  # noqa: S112
                continue
    return ArtifactInspection(present=True, dim=None,
                              error="no recognized first-layer weight key")


def _missing_runtime_artifacts(artifact_dir: str) -> List[str]:
    """Return files required for calibrated C(x)+G(x) operation."""
    required = (
        "model_identity.json",
        "cx_normalization.json",
        "gx_xgboost.json",
        "gx_weights.json",
        "gx_thresholds.json",
    )
    return [name for name in required
            if not os.path.isfile(os.path.join(artifact_dir, name))]


def _invalid_runtime_artifacts(artifact_dir: str,
                               selected_model: str = "",
                               embedding_dim: int = 0) -> List[str]:
    """Return concise validation errors for present model-coupled metadata.

    Existence alone is not enough: accepting ``{}``, booleans, NaN, or an
    inverted threshold order would let publishing claim calibrated support
    while the runtime quietly disables interventions.
    """
    gl_dir = os.path.join(cli_env.atlas_root(), "geometric-lens")
    if gl_dir not in sys.path:
        sys.path.insert(0, gl_dir)
    try:
        from geometric_lens.calibration import validate_cx_normalization
        from geometric_lens.identity import (
            identity_matches,
            validate_model_identity,
        )
        from geometric_lens.thresholds import validate_gx_thresholds
    except ImportError as exc:
        return [f"calibration validators unavailable: {exc}"]

    errors = []
    validators = {
        "model_identity.json": validate_model_identity,
        "cx_normalization.json": validate_cx_normalization,
        "gx_thresholds.json": validate_gx_thresholds,
    }
    for filename, validator in validators.items():
        path = os.path.join(artifact_dir, filename)
        if not os.path.isfile(path):
            continue
        try:
            with open(path) as fh:
                value = jsonlib.load(fh)
            validated = validator(value)
            if (filename == "model_identity.json" and selected_model
                    and not identity_matches(validated, selected_model,
                                             embedding_dim)):
                errors.append(
                    f"{filename}: artifact is for {validated['model']!r}, "
                    f"not {selected_model!r} at {embedding_dim or 'unknown'}-dim"
                )
        except (OSError, ValueError, TypeError, jsonlib.JSONDecodeError) as exc:
            errors.append(f"{filename}: {exc}")
    return errors


# ---------------------------------------------------------------------------
# atlas lens check  (PC-057)
# ---------------------------------------------------------------------------

@dataclass
class CheckVerdict:
    verdict: str          # 'compat' | 'needs-build' | 'incompatible'
    reason: str
    probe: LlamaProbe
    artifact_dir: Optional[str] = None
    artifact_dim: Optional[int] = None
    matched_model: Optional[str] = None
    # True when the artifact is present on disk but its input dim couldn't
    # be introspected (typically because torch isn't installed on the host
    # Python). Verdict stays "compat" (don't push users to needs-build for
    # a host-tooling gap) but JSON consumers can branch on this.
    unverified: bool = False

    @property
    def exit_code(self) -> int:
        return {"compat": 0, "needs-build": 1, "incompatible": 2}.get(self.verdict, 2)


def _configured_lens_models_dir(atlas_root: str) -> Optional[str]:
    """Resolve the host Lens artifact override with shell-env precedence."""
    return (os.environ.get("ATLAS_LENS_MODELS")
            or compose_config.read_env_file(atlas_root).get(
                "ATLAS_LENS_MODELS"))


def _check_model(arg: Optional[str], atlas_root: str) -> CheckVerdict:
    """Probe + verdict, with the registry download hint appended to any
    needs-build reason — when published lens artifacts exist for the
    loaded model, downloading beats retraining."""
    v = _check_model_inner(arg, atlas_root)
    if v.verdict == "needs-build":
        v.reason += model_registry.artifact_download_hint(
            v.probe.model_name, "lens")
    return v


def _lens_artifact_dir(matched, atlas_root: str) -> str:
    """The dir `atlas lens check` inspects: the registry's own for a
    Lens-supported entry, else ATLAS_LENS_MODELS or the default models dir."""
    # Resolve artifact dir. For known-supported registry entries this is
    # already wired; for arbitrary models we fall back to ATLAS_LENS_MODELS
    # or the global default. Either way, "is there a cost_field.pt whose
    # input dim matches the model's embedding dim?" is the decisive question.
    if matched and matched.lens_status == "supported":
        artifact_dir = model_registry.lens_artifact_dir_for(matched, atlas_root)
    else:
        env = _configured_lens_models_dir(atlas_root)
        if env:
            artifact_dir = env if os.path.isabs(env) else \
                os.path.normpath(os.path.join(atlas_root, env))
        else:
            artifact_dir = os.path.normpath(os.path.join(
                atlas_root, "geometric-lens", "geometric_lens", "models"))
    return artifact_dir


def _verdict_without_torch(probe: LlamaProbe, artifact_dir: str,
                           matched_name: Optional[str]) -> CheckVerdict:
    """The verdict for a cost_field.pt the host cannot read, for want of torch:
    needs-build when the runtime artifacts are missing or invalid, else an
    unverified compat."""
    missing = _missing_runtime_artifacts(artifact_dir)
    if missing:
        return CheckVerdict(
            verdict="needs-build",
            reason=("Lens weights are incomplete or uncalibrated; missing "
                    f"{', '.join(missing)}. Run `atlas lens build`."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=None, matched_model=matched_name,
        )
    invalid = _invalid_runtime_artifacts(
        artifact_dir, probe.model_name, probe.embedding_dim)
    if invalid:
        return CheckVerdict(
            verdict="needs-build",
            reason=("Lens calibration is invalid: " + "; ".join(invalid)
                    + ". Run `atlas lens build`."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=None, matched_model=matched_name,
        )
    # Artifact exists but the host Python can't peek at its dim. Don't
    # send the user to needs-build over a tooling gap on the host —
    # the lens service in the container has its own torch and will
    # score fine. Surface the unverified state via the dedicated flag.
    return CheckVerdict(
        verdict="compat", unverified=True,
        reason=(f"cost_field.pt exists at {artifact_dir} but the host "
                f"Python can't introspect its dim (torch not installed). "
                f"Assuming compat — the lens service in the container "
                f"has torch and will score normally. `pip install torch` "
                f"on the host if you want this to verify properly."),
        probe=probe, artifact_dir=artifact_dir,
        artifact_dim=None, matched_model=matched_name,
    )


def _verdict_at_matching_dim(probe: LlamaProbe, artifact_dir: str, artifact_dim: int,
                             matched_name: Optional[str]) -> CheckVerdict:
    """The verdict once cost_field.pt's input dim matches the model's: compat
    when the calibrated runtime artifacts are present and valid."""
    missing = _missing_runtime_artifacts(artifact_dir)
    if missing:
        return CheckVerdict(
            verdict="needs-build",
            reason=("C(x) dimension matches, but calibrated Lens operation "
                    f"requires {', '.join(missing)}. Run `atlas lens build` "
                    "for the selected model."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=artifact_dim, matched_model=matched_name,
        )
    invalid = _invalid_runtime_artifacts(
        artifact_dir, probe.model_name, probe.embedding_dim)
    if invalid:
        return CheckVerdict(
            verdict="needs-build",
            reason=("C(x)/G(x) calibration is invalid: "
                    + "; ".join(invalid)
                    + ". Run `atlas lens build` for the selected model."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=artifact_dim, matched_model=matched_name,
        )

    # Dim matches. PC-202 hidden-states patch is nice-to-have for lens
    # training embeddings but not required for C(x) scoring; report it
    # as a warning surface rather than a hard failure.
    note = ""
    if not probe.has_hidden_states_patch:
        note = (" Note: PC-202 hidden-states patch not detected on llama-server. "
                "C(x) works fine; lens training embeddings (hidden-states "
                "extraction for the C(x)+G(x) retrain) would need a patched "
                "build (inference/Dockerfile.v31).")
    return CheckVerdict(
        verdict="compat",
        reason=(f"Model emits {probe.embedding_dim}-dim embeddings; "
                f"cost_field.pt at {artifact_dir} accepts {artifact_dim}-dim. "
                f"Ready to score.{note}"),
        probe=probe, artifact_dir=artifact_dir,
        artifact_dim=artifact_dim, matched_model=matched_name,
    )


def _check_model_inner(arg: Optional[str], atlas_root: str) -> CheckVerdict:
    """The actual probe + verdict logic. Pure function for testability."""
    probe = probe_llama()
    if not probe.reachable:
        return CheckVerdict(verdict="incompatible", reason=probe.error,
                            probe=probe)
    if probe.embedding_dim == 0:
        return CheckVerdict(verdict="incompatible", reason=probe.error,
                            probe=probe)

    matched = publishing.resolve_model_arg(arg)
    matched_name = matched.name if matched else None
    requested_model = (matched.name if matched else arg) or ""
    if (requested_model and probe.model_name
            and publishing.canonical_model_identity(requested_model)
            != publishing.canonical_model_identity(probe.model_name)):
        return CheckVerdict(
            verdict="incompatible",
            reason=(f"Requested model {requested_model!r}, but llama-server "
                    f"has {probe.model_name!r} loaded. Start the requested "
                    "model before checking or building its Lens artifacts."),
            probe=probe,
            matched_model=matched_name,
        )

    artifact_dir = _lens_artifact_dir(matched, atlas_root)

    inspection = (_inspect_cost_field(artifact_dir) if artifact_dir
                  else ArtifactInspection(present=False))

    if not inspection.present:
        return CheckVerdict(
            verdict="needs-build",
            reason=(f"Model produces {probe.embedding_dim}-dim embeddings, but "
                    f"no cost_field.pt found in {artifact_dir}. Run "
                    f"`atlas lens build` to train fresh artifacts."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=None, matched_model=matched_name,
        )

    if inspection.dim is None and not inspection.torch_available:
        return _verdict_without_torch(probe, artifact_dir, matched_name)

    if inspection.dim is None:
        # Torch is available but the load failed for some other reason —
        # corrupted file, unrecognized layout, etc. Treat as needs-build
        # since we can't confirm the artifact is usable.
        return CheckVerdict(
            verdict="needs-build",
            reason=(f"cost_field.pt at {artifact_dir} could not be inspected: "
                    f"{inspection.error}. Rebuild with `atlas lens build`."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=None, matched_model=matched_name,
        )
    artifact_dim = inspection.dim

    if artifact_dim != probe.embedding_dim:
        return CheckVerdict(
            verdict="needs-build",
            reason=(f"Dim mismatch: model emits {probe.embedding_dim}-dim "
                    f"embeddings but the saved cost_field.pt expects "
                    f"{artifact_dim}-dim input. Run `atlas lens build` to "
                    f"train fresh artifacts at the model's native dim."),
            probe=probe, artifact_dir=artifact_dir,
            artifact_dim=artifact_dim, matched_model=matched_name,
        )

    return _verdict_at_matching_dim(probe, artifact_dir, artifact_dim, matched_name)


def _emit_check(args: argparse.Namespace, color: bool) -> int:
    atlas_root = cli_env.atlas_root()
    v = _check_model(args.model, atlas_root)

    if args.json:
        out = asdict(v)
        out["probe"] = asdict(v.probe)
        out["exit_code"] = v.exit_code
        print(jsonlib.dumps(out, indent=2))
        return v.exit_code

    badge = {
        "compat":       f"{GREEN}compat{RESET}"       if color else "compat",
        "needs-build":  f"{YELL}needs-build{RESET}"   if color else "needs-build",
        "incompatible": f"{RED}incompatible{RESET}"   if color else "incompatible",
    }[v.verdict]
    hdr = f"{BOLD}atlas lens check{RESET}" if color else "atlas lens check"
    _safe_print(f"{hdr}  verdict: {badge}")
    _safe_print(f"  llama-server: {v.probe.url} "
                f"({'reachable' if v.probe.reachable else 'unreachable'})")
    if v.probe.reachable:
        _safe_print(f"  model:        {v.probe.model_name or '(unknown)'}")
        _safe_print(f"  embedding:    {v.probe.embedding_dim}-dim")
        _safe_print(f"  layers:       {v.probe.n_layers or '(unknown)'}")
        _safe_print(f"  PC-202 patch: "
                    f"{'yes' if v.probe.has_hidden_states_patch else 'no'}")
    if v.artifact_dir:
        _safe_print(f"  artifact dir: {v.artifact_dir}")
    if v.artifact_dim is not None:
        _safe_print(f"  artifact dim: {v.artifact_dim}-dim")
    if v.matched_model:
        _safe_print(f"  registry hit: {v.matched_model}")
    _safe_print("")
    _safe_print(f"  {v.reason}")
    if v.verdict != "compat":
        _safe_print("")
        _safe_print("  This is the lens-only view. `atlas doctor` runs the "
                    "full stack diagnosis (service health, auth, disk, "
                    "lens/ASA state) if the fix above isn't enough.")
    return v.exit_code


# ---------------------------------------------------------------------------
# atlas lens build  (PC-058)
# ---------------------------------------------------------------------------

def _embed_text(llama_url: str, text: str,
                _depth: int = 0) -> "tuple[Optional[List[float]], int]":
    """Embed one text via /embedding. Returns (vector, pieces_used).

    llama-server rejects pooled-embedding inputs longer than its micro-batch
    (`-ub`) with an "input is too large" error — common for runaway FAIL
    candidates that hit the generation cap. On failure, split at a line
    boundary and length-weighted-average the halves' embeddings (the lens
    mean-pools per-token states anyway, so chunk-averaging is the same
    operation at coarser granularity). Depth-capped at 4 (16 chunks).
    """
    resp = post_json_or_none(f"{llama_url}/embedding",
                             {"content": text}, timeout=60.0)
    vec = None
    if isinstance(resp, list) and resp and isinstance(resp[0], dict):
        raw = resp[0].get("embedding")
        if isinstance(raw, list) and raw:
            if isinstance(raw[0], list):
                # per-token: mean-pool
                n_tok = len(raw)
                dim = len(raw[0])
                vec = [sum(tok[j] for tok in raw) / n_tok
                       for j in range(dim)]
            else:
                vec = raw
    if vec is not None:
        return vec, 1
    if _depth >= 4 or len(text) < 2:
        return None, 0
    mid = text.rfind("\n", 0, len(text) // 2 + 1)
    if mid <= 0:
        mid = len(text) // 2
    left, right = text[:mid], text[mid:]
    if not left.strip() or not right.strip():
        left, right = text[:len(text) // 2], text[len(text) // 2:]
    va, pa = _embed_text(llama_url, left, _depth + 1)
    vb, pb = _embed_text(llama_url, right, _depth + 1)
    if va is None and vb is None:
        return None, 0
    if va is None or vb is None:
        # Salvage the half that embedded — for the runaway/repetitive texts
        # that hit this path, any chunk is representative.
        return (va or vb), (pa or pb)
    wa, wb = len(left), len(right)
    return ([(a * wa + b * wb) / (wa + wb) for a, b in zip(va, vb)],
            pa + pb)


def _extract_training_embeddings(samples: List[Dict],  # noqa: C901
                                  llama_url: str,
                                  color: bool,
                                  cache_path: Optional[str] = None,
                                  expect_dim: int = 0) -> Dict:
    """For each sample {text, label}, POST /embedding and collect.

    Embeddings are cached on disk keyed by text hash (extraction dominates
    build wall-clock — ~15 min per few hundred samples — and retrains on a
    grown results dir re-embed mostly the same texts). Entries whose dim
    doesn't match the probed model are ignored, so a model switch
    invalidates the cache naturally.

    Returns a dict compatible with training.train_cost_field's `data` arg:
        {"embeddings": [List[float], ...], "labels": [0|1, ...]}
    """
    import hashlib

    cache: Dict[str, List[float]] = {}
    if cache_path and os.path.isfile(cache_path):
        try:
            with open(cache_path, encoding="utf-8") as fh:
                for line in fh:
                    try:
                        entry = jsonlib.loads(line)
                        if not expect_dim or entry.get("dim") == expect_dim:
                            cache[entry["h"]] = entry["v"]
                    except (jsonlib.JSONDecodeError, KeyError, TypeError):
                        continue
        except OSError:
            # A corrupt or unreadable cache only forfeits reuse; embeddings
            # are recomputed and a fresh cache may be appended below.
            pass

    cache_fh = None
    if cache_path:
        try:
            cache_fh = open(cache_path, "a", encoding="utf-8")
        except OSError:
            cache_fh = None

    embeddings: List[List[float]] = []
    labels: List[int] = []
    weights: List[float] = []  # carried through aligned with kept samples
    saw_weight = False
    n = len(samples)
    hits = 0
    try:
        for i, s in enumerate(samples):
            text = s.get("text") or s.get("content") or ""
            label = int(s.get("label", 0))
            if not text:
                continue
            h = hashlib.sha256(text.encode("utf-8")).hexdigest()
            vec = cache.get(h)
            if vec is not None:
                hits += 1
            else:
                vec, pieces = _embed_text(llama_url, text)
                if vec is None:
                    _safe_print(f"  WARN: sample {i+1}/{n}: embedding failed "
                                f"even after chunking — skipped")
                    continue
                if pieces > 1:
                    _safe_print(f"  sample {i+1}/{n}: longer than the "
                                f"server's micro-batch — embedded as "
                                f"{pieces} chunks, mean-pooled")
                cache[h] = vec   # duplicate texts later in this run hit
                if cache_fh:
                    cache_fh.write(jsonlib.dumps(
                        {"h": h, "dim": len(vec), "v": vec}) + "\n")
                    cache_fh.flush()
            embeddings.append(vec)
            labels.append(label)
            if "weight" in s:
                saw_weight = True
            weights.append(float(s.get("weight", 1.0)))
            if (i + 1) % 25 == 0 or (i + 1) == n:
                _safe_print(f"  extracted {i+1}/{n} embeddings")
    finally:
        if cache_fh:
            cache_fh.close()
    if hits:
        _safe_print(f"  ({hits} from cache, {len(embeddings) - hits} embedded "
                    f"fresh)")
    out = {"embeddings": embeddings, "labels": labels}
    # Only attach weights when the samples actually carried them; unweighted
    # builds stay uniformly weighted.
    if saw_weight:
        out["weights"] = weights
    return out


_MANAGED_LENS_ARTIFACTS = (
    "cost_field.pt",
    "cost_field.safetensors",
    "cx_normalization.json",
    "gx_xgboost.json",
    "gx_weights.json",
    "gx_thresholds.json",
    "provenance.json",
    "drift_fingerprint.json",
    "model_identity.json",
    # Superseded formats must not shadow a freshly trained XGBoost bundle.
    "metric_tensor.pt",
    "gx_xgboost.pkl",
)


def _write_bundle_fingerprint(staging_dir: str, model, embed=None,
                              llama_url: str = "") -> Optional[str]:
    """Write the drift fingerprint for a freshly trained bundle (#230).

    The lens re-scores these reference texts at boot and fails /ready when an
    energy moves beyond tolerance, which is how a serving stack that drifted
    from what the artifacts were fitted on shows up. So the expected energies
    are scored the way the service scores them: its own embedding path
    (geometric_lens.embedding_extractor.extract_embedding) and these weights.
    That module's default URL is the compose service name, which the host
    cannot resolve, so it is pointed at the llama-server this build reached.

    Returns a warning when no fingerprint was written. A reference that
    cannot be scored leaves the bundle without one (the check then enforces
    nothing), never with one built from a failed measurement."""
    set_url = bool(llama_url) and not os.environ.get("LLAMA_EMBED_URL")
    if set_url:
        os.environ["LLAMA_EMBED_URL"] = llama_url
    ee, prior_contract = None, None
    try:
        import torch
        from geometric_lens.drift import write_fingerprint
        if embed is None:
            from geometric_lens import embedding_extractor as ee
            from geometric_lens.identity import load_model_identity
            # Score under the contract the bundle declares: the service
            # installs it from model_identity.json when it loads the bundle,
            # and a normalized contract changes every vector.
            try:
                contract = load_model_identity(staging_dir).get("embedding_contract")
            except (OSError, ValueError):
                contract = None  # the service refuses such a bundle anyway
            prior_contract = ee._EMBEDDING_CONTRACT
            ee.set_embedding_contract(contract)
            embed = ee.extract_embedding
        model.eval()

        def score(text: str) -> float:
            x = torch.tensor(embed(text), dtype=torch.float32).unsqueeze(0)
            with torch.no_grad():
                return float(model(x).item())

        write_fingerprint(staging_dir, score, note="written by atlas lens build")
        return None
    except Exception as exc:  # a missing fingerprint is the old behaviour
        return (f"drift fingerprint not written ({exc}); the lens drift check "
                f"enforces nothing for this bundle")
    finally:
        if ee is not None:
            ee.set_embedding_contract(prior_contract)
        if set_url:
            os.environ.pop("LLAMA_EMBED_URL", None)


def _activate_lens_bundle(staging_dir: str, artifact_dir: str) -> None:
    """Activate one complete Lens bundle, restoring the prior one on error."""
    missing = _missing_runtime_artifacts(staging_dir)
    if not os.path.isfile(os.path.join(staging_dir, "cost_field.pt")):
        missing.insert(0, "cost_field.pt")
    if missing:
        raise ValueError("staged Lens bundle is incomplete: "
                         + ", ".join(missing))
    invalid = _invalid_runtime_artifacts(staging_dir)
    if invalid:
        raise ValueError("staged Lens bundle is invalid: " + "; ".join(invalid))

    artifact_dir = os.path.abspath(artifact_dir)
    os.makedirs(artifact_dir, exist_ok=True)
    parent = os.path.dirname(artifact_dir)
    backup_dir = tempfile.mkdtemp(prefix=".atlas-lens-backup-", dir=parent)
    moved_new = []
    try:
        for filename in _MANAGED_LENS_ARTIFACTS:
            current = os.path.join(artifact_dir, filename)
            if os.path.exists(current):
                os.replace(current, os.path.join(backup_dir, filename))

        # Identity is the commit marker and moves last.
        staged_files = [name for name in _MANAGED_LENS_ARTIFACTS
                        if name != "model_identity.json"
                        and os.path.isfile(os.path.join(staging_dir, name))]
        staged_files.append("model_identity.json")
        for filename in staged_files:
            source = os.path.join(staging_dir, filename)
            target = os.path.join(artifact_dir, filename)
            os.replace(source, target)
            moved_new.append(target)
    except Exception:
        for path in moved_new:
            try:
                os.remove(path)
            except OSError:
                # Rollback cleanup is best-effort; the original artifacts are
                # restored from backup immediately afterward.
                pass
        for filename in os.listdir(backup_dir):
            os.replace(os.path.join(backup_dir, filename),
                       os.path.join(artifact_dir, filename))
        raise
    finally:
        import shutil
        shutil.rmtree(backup_dir, ignore_errors=True)


def _probe_for_build(args: argparse.Namespace, atlas_root: str, color: bool):
    """[1/5]: probe the loaded model as `atlas lens check` does. Returns
    (verdict, exit_code); exit_code is None when the build goes on."""
    # 1. Pre-flight: confirm we can probe the model. Reuses the check path
    # so build's UX agrees with check's "this is/isn't compat" verdict.
    _safe_print("[1/5] Probing llama-server…")
    verdict = _check_model(args.model, atlas_root)
    if verdict.verdict == "incompatible":
        _safe_print(f"  {RED if color else ''}Cannot proceed: "
                    f"{verdict.reason}{RESET if color else ''}")
        return verdict, 2
    _safe_print(f"  Model emits {verdict.probe.embedding_dim}-dim embeddings "
                f"(model: {verdict.probe.model_name or 'unknown'})")
    if verdict.verdict == "compat" and not args.force:
        _safe_print(f"  {YELL if color else ''}Artifacts already exist at "
                    f"{verdict.artifact_dir} for the current dim. Pass "
                    f"--force to retrain anyway.{RESET if color else ''}")
        return verdict, 0
    return verdict, None


def _samples_from_results(args: argparse.Namespace, atlas_root: str, color: bool):
    """--from-results: the per-task JSONs of a bench run. Returns (samples,
    results_dir); samples is None after printing that the dir has none."""
    results_dir = args.from_results
    # A relative path that doesn't exist under the cwd is resolved against
    # the repo root, so the command works from any directory.
    if not os.path.isdir(results_dir):
        rooted = os.path.join(atlas_root, results_dir)
        if os.path.isdir(rooted):
            results_dir = rooted
    samples = _load_results_samples(results_dir)
    if not samples:
        _safe_print(f"  {RED if color else ''}No usable samples in "
                    f"{results_dir} — expected per-task JSONs with "
                    f"`code` + `passed` (from `atlas bench`)."
                    f"{RESET if color else ''}")
        # Common slip: pointing at the run root instead of the per-task dir.
        for sub in ("v3_lcb/per_task", "per_task"):
            cand = os.path.join(results_dir, sub)
            if os.path.isdir(cand):
                _safe_print(f"  Did you mean: --from-results {cand}")
                break
        return None, results_dir
    return samples, results_dir


def _load_build_samples(args: argparse.Namespace, atlas_root: str, color: bool):
    """[2/5]: the labeled samples, from --from-results or --samples. Returns
    (samples, results_dir); samples is None after printing why there are none."""
    results_dir = None
    _safe_print("[2/5] Loading training samples…")
    if getattr(args, "from_results", None) and args.samples:
        _safe_print(f"  {RED if color else ''}--from-results and --samples are "
                    f"mutually exclusive — pass one.{RESET if color else ''}")
        return None, results_dir
    if getattr(args, "from_results", None):
        samples, results_dir = _samples_from_results(args, atlas_root, color)
        if samples is None:
            return None, results_dir
    elif args.samples:
        samples = _load_training_samples(args.samples)
    else:
        _safe_print(f"  {RED if color else ''}Provide --from-results <dir> or "
                    f"--samples <file>.{RESET if color else ''}")
        _safe_print("  --from-results: a benchmark/results/<run>/v3_lcb/per_task "
                    "dir (this model's own candidates, via `atlas bench`)")
        _safe_print("  --samples: a labeled [{\"text\": str, \"label\": 0|1}, ...] "
                    "file (e.g. huggingface.co/datasets/itigges22/ATLAS)")
        return None, results_dir
    return samples, results_dir


def _trainable_samples(samples: List[Dict], color: bool) -> Optional[List[Dict]]:
    """The samples with int labels, when there are enough of them and of both
    labels to train on; None after printing why not."""
    # Coerce labels to int once, here; rows with malformed labels are dropped
    # so the counting and embedding-extraction below can rely on clean ints.
    cleaned = []
    n_bad = 0
    for s in samples:
        try:
            s["label"] = int(s.get("label", 0))
            cleaned.append(s)
        except (TypeError, ValueError):
            n_bad += 1
    if n_bad:
        _safe_print(f"  WARN: skipped {n_bad} sample(s) with non-integer labels")
    samples = cleaned
    if len(samples) < 50:
        _safe_print(f"  {RED if color else ''}Only {len(samples)} samples "
                    f"loaded. Need >=50 for meaningful training (>=200 "
                    f"recommended).{RESET if color else ''}")
        return None
    n_pass = sum(1 for s in samples if int(s.get("label", 0)) == 1)
    n_fail = len(samples) - n_pass
    _safe_print(f"  Loaded {len(samples)} samples (PASS={n_pass}, FAIL={n_fail})")
    if n_pass == 0 or n_fail == 0:
        _safe_print(f"  {RED if color else ''}Need both pass and fail "
                    f"samples for contrastive training.{RESET if color else ''}")
        return None
    return samples


def _extract_build_embeddings(args: argparse.Namespace, verdict: CheckVerdict,
                              samples: List[Dict], results_dir: Optional[str],
                              color: bool) -> Optional[Dict]:
    """[3/5]: the samples' embeddings from llama-server, through the embedding
    cache; None after printing that none came back."""
    _safe_print(f"[3/5] Extracting embeddings via {verdict.probe.url}…")
    if getattr(args, "from_results", None):
        cache_path = os.path.normpath(
            os.path.join(results_dir, os.pardir, "embeddings_cache.jsonl"))
    elif args.samples:
        cache_path = args.samples + ".embcache.jsonl"
    else:
        cache_path = None
    start = time.time()
    data = _extract_training_embeddings(
        samples, verdict.probe.url, color, cache_path=cache_path,
        expect_dim=verdict.probe.embedding_dim or 0)
    elapsed = time.time() - start
    if not data["embeddings"]:
        _safe_print(f"  {RED if color else ''}No embeddings extracted. "
                    f"Check llama-server logs.{RESET if color else ''}")
        return None
    _safe_print(f"  Extracted {len(data['embeddings'])} embeddings "
                f"in {elapsed:.1f}s")
    return data


def _merge_banked_embeddings(data: Dict, results_dir: str) -> None:
    """Add the candidate embeddings the bench run banked to data, leaving out
    those that duplicate an extracted sample."""
    emb_path = os.path.normpath(os.path.join(
        results_dir, os.pardir, os.pardir, "telemetry", "embeddings.emb"))
    if os.path.isfile(emb_path):
        try:
            import numpy as np
            dim = len(data["embeddings"][0])
            tele, skipped = _load_telemetry_embeddings(emb_path, dim)
            # The selected candidate appears in both sources, but the
            # banked copy was embedded mid-bench under concurrent
            # batching — same text, FP noise up to ~5e-5 per element.
            # Dedup numerically: a telemetry vector within 1e-3 (L-inf)
            # of any kept vector is the same sample; genuinely
            # different candidates differ by orders of magnitude more.
            kept = np.array(data["embeddings"], dtype=np.float32)
            merged = dups = 0
            for rec in tele:
                v = np.array(rec["embedding"], dtype=np.float32)
                if np.abs(kept - v).max(axis=1).min() < 1e-3:
                    dups += 1
                    continue
                kept = np.vstack([kept, v])
                data["embeddings"].append(rec["embedding"])
                data["labels"].append(rec["label"])
                merged += 1
            _safe_print(f"  Merged {merged} banked candidate embeddings "
                        f"from {os.path.basename(emb_path)} "
                        f"({dups} duplicates of extracted samples"
                        + (f", {skipped} skipped" if skipped else "")
                        + ") — pass --no-telemetry to train on the "
                        "results dir alone")
            n_p = sum(data["labels"])
            _safe_print(f"  Training set: {len(data['labels'])} samples "
                        f"(PASS={n_p}, FAIL={len(data['labels']) - n_p})")
        except Exception as e:
            _safe_print(f"  (telemetry merge unavailable: {e})")


def _print_training_import_error(e: ImportError, color: bool) -> None:
    """Say what the host needs to import the C(x) training module."""
    _safe_print(f"  {RED if color else ''}Could not import training "
                f"module: {e}.{RESET if color else ''}")
    if "torch" in str(e):
        _safe_print("  Training runs on the host and needs the train "
                    "extra (or a CPU torch, which is enough here):")
        _safe_print("    pip install 'atlas[train]'   # or:")
        _safe_print("    pip install torch --index-url "
                    "https://download.pytorch.org/whl/cpu")
    else:
        _safe_print("  The atlas CLI must point at an ATLAS checkout "
                    "containing geometric-lens/ (check `pip show atlas` "
                    "→ Editable project location).")


def _print_low_auc_advice(train_auc: float, color: bool) -> None:
    """Advice for a test AUC under 0.70: more samples when the train AUC shows
    overfitting, more epochs otherwise."""
    if train_auc >= 0.95:
        # Memorized the train fold but doesn't generalize: the sample
        # set is too small for the capacity. Epochs won't help.
        _safe_print(f"  {YELL if color else ''}Test AUC < 0.70 with "
                    f"train AUC {train_auc:.2f} — overfit on too few "
                    f"samples. More training data is the fix (e.g. a "
                    f"larger `atlas bench --tasks N` run); more epochs "
                    f"won't help.{RESET if color else ''}")
    else:
        _safe_print(f"  {YELL if color else ''}Test AUC < 0.70 with "
                    f"train AUC {train_auc:.2f} — undertrained. Try "
                    f"more epochs (--epochs 400) or a higher learning "
                    f"rate.{RESET if color else ''}")


def _train_cost_field(args: argparse.Namespace, atlas_root: str, data: Dict, color: bool):
    """[4/5]: train C(x) on the embeddings. Returns (result, save_cost_field,
    derive_cx_normalization), or None when the training module cannot load."""
    _safe_print(f"[4/5] Training CostField "
                f"({args.epochs} epochs, lr={args.lr})…")
    try:
        # The training module lives in the repo, not on the host's default
        # path — resolve it from the install root so this works from any cwd.
        gl_dir = os.path.join(atlas_root, "geometric-lens")
        if gl_dir not in sys.path:
            sys.path.insert(0, gl_dir)
        from geometric_lens.training import train_cost_field, save_cost_field
        from geometric_lens.calibration import derive_cx_normalization
    except ImportError as e:
        _print_training_import_error(e, color)
        return None
    result = train_cost_field(data, epochs=args.epochs, lr=args.lr,
                              margin=args.margin)
    # train_cost_field returns final_* and best_* keys, not bare test_auc.
    # Surface "best test AUC seen during training" since the final-epoch
    # value can be lower from overfitting.
    test_auc = (result.get("best_test_auc")
                or result.get("final_test_auc") or 0.0)
    train_auc = result.get("final_train_auc") or 0.0
    _safe_print(f"  Train AUC: {train_auc:.4f}  |  Test AUC: {test_auc:.4f} "
                f"(best across epochs)")
    if test_auc < 0.7:
        _print_low_auc_advice(train_auc, color)
    return result, save_cost_field, derive_cx_normalization


def _train_gx(staging_dir: str, data: Dict, color: bool) -> Optional[Dict]:
    """[5/5]: train G(x) on the same embeddings and save it in the staging dir;
    None after printing why it could not be trained."""
    _safe_print("[5/5] Training G(x) XGBoost…")
    try:
        from geometric_lens.training import train_gx, save_gx
        gx_result = train_gx(data)
    except ImportError as e:
        _safe_print(f"  {RED if color else ''}Could not import the G(x) "
                    f"trainer: {e}.{RESET if color else ''}")
        _safe_print("  G(x) training needs XGBoost + scikit-learn on the "
                    "host: pip install 'atlas[train]' (or pip install "
                    "xgboost scikit-learn)")
        _safe_print("  The previous live bundle is unchanged; embeddings "
                    "are cached for a quick retry.")
        return None
    except ValueError as e:
        _safe_print(f"  {RED if color else ''}G(x) training skipped: "
                    f"{e}{RESET if color else ''}")
        _safe_print("  The previous live bundle is unchanged. Add more "
                    "bench samples and re-run.")
        return None
    save_gx(gx_result, save_dir=staging_dir)
    return gx_result


def _record_model_identity(args: argparse.Namespace, verdict: CheckVerdict,
                           staging_dir: str, color: bool) -> Optional[str]:
    """Save model_identity.json, with the embedding convention the bundle was
    fitted on when it can be observed. Returns the model's name; None after
    printing that the loaded model has none."""
    from geometric_lens.identity import save_model_identity
    model_identity = (args.model or verdict.probe.model_name or "").strip()
    if not model_identity:
        _safe_print(f"  {RED if color else ''}Could not identify the loaded "
                    f"model; refusing to activate these artifacts."
                    f"{RESET if color else ''}")
        return None
    # Record the convention these artifacts were actually fitted on.
    # save_model_identity has always accepted it and nothing passed it,
    # so shipped artifacts declare no contract — which is why a
    # server-side pooling or normalization change shifts every score
    # while health checks stay green. Measured 2026-08-03: served
    # unit-norm vectors against a cost field fitted on |v|~137 returned
    # 0.76-0.80 for a clean function, a repetition loop and truncated
    # junk alike, against a calibrated 9.25/11.81 band.
    #
    # Best-effort: a probe failure must not block activation, and an
    # absent contract is the behaviour we already have.
    embedding_contract = None
    try:
        from geometric_lens.embedding_extractor import (
            observe_embedding_convention,
        )
        embedding_contract = observe_embedding_convention()
    except Exception as exc:  # telemetry, not correctness
        _safe_print(f"  could not observe the embedding convention "
                    f"({exc}); artifacts will declare none")
    save_model_identity(
        staging_dir, model_identity, verdict.probe.embedding_dim,
        embedding_contract=embedding_contract)
    return model_identity


def _save_build_provenance(args: argparse.Namespace, verdict: CheckVerdict, data: Dict,
                           result: Dict, gx_result: Dict, normalization,
                           model_identity: str, staging_dir: str) -> None:
    """Write the bundle's provenance manifest; a failure only warns."""
    # Per-bundle provenance manifest (SUPPORT_MATRIX §9.5): every
    # activated bundle is reproducible and auditable — `atlas artifact
    # verify/snapshot/rollback` consume this file. Best-effort: a
    # failed manifest never blocks activation.
    try:
        from datetime import datetime, timezone
        from geometric_lens.provenance import build_manifest, save_provenance
        n_p = sum(data["labels"])
        save_provenance(staging_dir, build_manifest(
            model=model_identity,
            embedding_dim=verdict.probe.embedding_dim,
            created_at=datetime.now(timezone.utc).isoformat(),
            dataset=(getattr(args, "from_results", None)
                     or args.samples or ""),
            n_samples=len(data["labels"]), n_pass=n_p,
            n_fail=len(data["labels"]) - n_p,
            metrics={"best_test_auc": result.get("best_test_auc"),
                     "final_train_auc": result.get("final_train_auc"),
                     "gx_cv_auc_mean": gx_result.get("cv_auc_mean"),
                     "pass_energy_mean": result.get("pass_energy_mean"),
                     "fail_energy_mean": result.get("fail_energy_mean")},
            normalization=normalization,
            hyperparameters={"epochs": args.epochs, "lr": args.lr,
                             "margin": args.margin},
            save_dir=staging_dir))
    except Exception as e:
        _safe_print(f"  WARN: provenance manifest not written: {e}")


def _stage_and_activate(args: argparse.Namespace, verdict: CheckVerdict, data: Dict,
                        result: Dict, save_cost_field, derive_cx_normalization,
                        color: bool):
    """Save C(x), G(x) and their metadata in a staging dir beside the artifact
    dir, then activate the complete bundle. Returns (artifact_dir, gx_result);
    None when the bundle could not be completed or activated."""
    artifact_dir = args.artifact_dir or verdict.artifact_dir
    artifact_parent = os.path.dirname(os.path.abspath(artifact_dir))
    os.makedirs(artifact_parent, exist_ok=True)
    with tempfile.TemporaryDirectory(
            prefix=".atlas-lens-build-", dir=artifact_parent) as staging_dir:
        normalization = derive_cx_normalization(
            result["pass_energy_mean"], result["fail_energy_mean"])
        save_cost_field(result["model"], save_dir=staging_dir,
                        normalization=normalization)

        # 5. Train + save G(x) on the same embeddings. Nothing reaches the
        # live directory unless both halves and their metadata are complete.
        gx_result = _train_gx(staging_dir, data, color)
        if gx_result is None:
            return None
        model_identity = _record_model_identity(args, verdict, staging_dir, color)
        if model_identity is None:
            return None
        # Drift fingerprint (#230): the reference energies the lens re-checks
        # at boot. Before the provenance manifest, which hashes it.
        warning = _write_bundle_fingerprint(staging_dir, result["model"],
                                            llama_url=verdict.probe.url)
        if warning:
            _safe_print(f"  WARN: {warning}")
        _save_build_provenance(args, verdict, data, result, gx_result, normalization,
                               model_identity, staging_dir)
        try:
            _activate_lens_bundle(staging_dir, artifact_dir)
        except (OSError, ValueError) as e:
            _safe_print(f"  {RED if color else ''}Could not activate the "
                        f"complete Lens bundle: {e}. The previous bundle was "
                        f"restored.{RESET if color else ''}")
            return None

    return artifact_dir, gx_result


def _print_build_complete(verdict: CheckVerdict, artifact_dir: str, gx_result: Dict,
                          color: bool) -> None:
    """Say where the bundle is and what to do next."""
    _safe_print(f"  Activated: {artifact_dir}")
    _safe_print(f"  G(x) CV AUC: {gx_result['cv_auc_mean']:.4f}")

    _safe_print("")
    _safe_print(f"  {GREEN if color else ''}Build complete "
                f"(C(x) + G(x)).{RESET if color else ''}")
    _safe_print("  The lens service loads artifacts at startup — restart it "
                "to pick these up:")
    _safe_print(f"    {BOLD if color else ''}docker compose restart "
                f"geometric-lens{RESET if color else ''}")
    _safe_print("  then `atlas lens check` should report compat.")
    _safe_print(f"  Next: `atlas lens publish` to share these artifacts and "
                f"generate a registry-PR (PC-059), or manually update the "
                f"registry entry for "
                f"{verdict.matched_model or '<your model>'} to "
                f"lens_status=\"supported\".")


def _emit_build(args: argparse.Namespace, color: bool) -> int:
    """Train fresh Lens artifacts for the model llama-server has loaded.

    Doesn't ship its own dataset — users point --samples at a labeled
    JSON/JSONL file (typically pulled from huggingface.co/datasets/itigges22/ATLAS,
    which has the V3 ablation traces with pass/fail labels). Tiny built-in
    sanity datasets are intentionally NOT bundled: a 20-sample C(x) is
    worse than no C(x) (it'll badly mis-rank and the user won't know).
    """
    atlas_root = cli_env.atlas_root()

    verdict, exit_code = _probe_for_build(args, atlas_root, color)
    if exit_code is not None:
        return exit_code

    # 2. Load training data
    samples, results_dir = _load_build_samples(args, atlas_root, color)
    if samples is None:
        return 1
    samples = _trainable_samples(samples, color)
    if samples is None:
        return 1

    # 3. Extract embeddings via /embedding
    data = _extract_build_embeddings(args, verdict, samples, results_dir, color)
    if data is None:
        return 1

    # Merge the bench run's banked per-candidate embeddings (PASS/FAIL
    # labeled by the sandbox). A V3-mode bench tests several candidates per
    # task and banks each one, so this multiplies the training set without
    # any extra labeling or embedding work. Near-identical vectors (the
    # selected candidate appears in both sources) are deduped.
    if getattr(args, "from_results", None) and not getattr(
            args, "no_telemetry", False):
        _merge_banked_embeddings(data, results_dir)

    if args.dry_run:
        _safe_print("  (dry-run) skipping training + save")
        return 0

    # 4. Train + save
    trained = _train_cost_field(args, atlas_root, data, color)
    if trained is None:
        return 1
    result, save_cost_field, derive_cx_normalization = trained
    staged = _stage_and_activate(args, verdict, data, result, save_cost_field,
                                 derive_cx_normalization, color)
    if staged is None:
        return 1
    artifact_dir, gx_result = staged

    _print_build_complete(verdict, artifact_dir, gx_result, color)
    return 0


# ---------------------------------------------------------------------------
# atlas lens publish  (PC-059)
# ---------------------------------------------------------------------------

def _publish_artifact_dir(args: argparse.Namespace, matched, atlas_root: str) -> str:
    """The dir to publish: --artifact-dir, the registry's own for a
    Lens-supported entry, else ATLAS_LENS_MODELS or the default models dir."""
    if args.artifact_dir:
        artifact_dir = args.artifact_dir
    elif matched and matched.lens_status == "supported":
        artifact_dir = model_registry.lens_artifact_dir_for(matched, atlas_root)
    else:
        env = _configured_lens_models_dir(atlas_root)
        artifact_dir = env or os.path.normpath(os.path.join(
            atlas_root, "geometric-lens", "geometric_lens", "models"))
    return artifact_dir


def _bundle_ready_to_publish(artifact_dir: str, cost_path: str, model_label: str,
                             color: bool) -> bool:
    """True when cost_field.pt is there with its complete, valid calibration;
    False after printing what is missing or invalid."""
    if not os.path.isfile(cost_path):
        _safe_print(f"  {RED if color else ''}No cost_field.pt at "
                    f"{cost_path}. Run `atlas lens build` first."
                    f"{RESET if color else ''}")
        return False

    missing = _missing_runtime_artifacts(artifact_dir)
    if missing:
        _safe_print(f"  {RED if color else ''}Lens artifact is incomplete or "
                    f"uncalibrated; missing {', '.join(missing)}. Run "
                    f"`atlas lens build` before publishing."
                    f"{RESET if color else ''}")
        return False
    invalid = _invalid_runtime_artifacts(artifact_dir, model_label)
    if invalid:
        _safe_print(f"  {RED if color else ''}Lens calibration is invalid: "
                    f"{'; '.join(invalid)}. Run `atlas lens build` before "
                    f"publishing.{RESET if color else ''}")
        return False
    return True


def _files_to_upload(artifact_dir: str, cost_path: str, color: bool) -> List[str]:
    """The bundle's files that go to Hugging Face, cost_field.pt first."""
    files_to_upload = ["cost_field.pt", "model_identity.json", "cx_normalization.json"]
    # Pickle-free twin: include only when at least as fresh as the .pt —
    # an older safetensors is a previous model's weights.
    st_path = os.path.join(artifact_dir, "cost_field.safetensors")
    if (os.path.isfile(st_path)
            and os.path.getmtime(st_path) >= os.path.getmtime(cost_path)):
        files_to_upload.append("cost_field.safetensors")
    # G(x) artifacts ship with C(x) — the build trains both halves, and a
    # registry consumer who only gets cost_field.pt would run with a
    # dormant (or wrong-dimension) G(x). (G(x) itself is XGBoost trees in
    # native JSON — already pickle-free; safetensors doesn't apply.)
    for opt in ("gx_xgboost.json", "gx_weights.json", "gx_thresholds.json",
                "drift_fingerprint.json"):
        if os.path.isfile(os.path.join(artifact_dir, opt)):
            files_to_upload.append(opt)
    if "gx_xgboost.json" not in files_to_upload:
        _safe_print(f"  {YELL if color else ''}No gx_xgboost.json in "
                    f"{artifact_dir} — publishing C(x) only. Re-run "
                    f"`atlas lens build` to train both halves."
                    f"{RESET if color else ''}")
        # Legacy G(x) carrier — only relevant when the JSON pair is absent.
        # `lens build` doesn't refresh this file, so on a retrained dir it
        # is a previous model's artifact and must not ship.
    return files_to_upload


def _hash_and_inspect(artifact_dir: str, cost_path: str, files_to_upload: List[str],
                      color: bool):
    """[1/5]: hash cost_field.pt and read its input dim. Returns (sha, size,
    dim); dim is 0 when it could not be read."""
    _safe_print(f"[1/5] Hashing {cost_path}…")
    sha = publishing.sha256_file(cost_path)
    size = os.path.getsize(cost_path)
    inspection = _inspect_cost_field(artifact_dir)
    dim = inspection.dim or 0
    if dim == 0:
        _safe_print(f"  {YELL if color else ''}Could not introspect "
                    f"cost_field.pt dim ({inspection.error or 'torch missing'}). "
                    f"Publish will continue but the model-card metadata "
                    f"will omit the dim field.{RESET if color else ''}")
    _safe_print(f"  SHA256: {sha}")
    _safe_print(f"  Size:   {size / (1024 * 1024):.2f} MB")
    _safe_print(f"  Dim:    {dim if dim else '(unknown)'}")
    _safe_print(f"  Files:  {', '.join(files_to_upload)}")
    return sha, size, dim


def _emit_publish(args: argparse.Namespace, color: bool) -> int:
    """Upload local artifacts to HF + generate a registry-add PR body.

    Pipeline (matches PC-059 issue spec):
      0. publish_preflight: print requirements panel + bail if creds missing
      1. Validate: artifact dir exists + cost_field.pt is in it
      2. Compute SHA256 for the registry entry
      3. (Unless --dry-run) upload artifacts + auto-generated model card to HF
      4. Render the registry-PR markdown
      5. (Unless --skip-pr) open the registry PR via `gh api`; otherwise print body
    """
    if not publishing.publish_preflight("lens", dry_run=args.dry_run,
                                        color=color):
        return 1

    atlas_root = cli_env.atlas_root()

    # 1. Resolve artifacts
    matched = publishing.resolve_model_arg(args.model)
    model_label = matched.name if matched else (args.model or "<unknown-model>")

    artifact_dir = _publish_artifact_dir(args, matched, atlas_root)

    cost_path = os.path.join(artifact_dir, "cost_field.pt")
    if not _bundle_ready_to_publish(artifact_dir, cost_path, model_label, color):
        return 1
    files_to_upload = _files_to_upload(artifact_dir, cost_path, color)

    # 2. Compute SHA + inspect
    sha, size, dim = _hash_and_inspect(artifact_dir, cost_path, files_to_upload, color)

    base_model = (matched.model_display if matched else
                  args.model or
                  os.path.basename(cost_path).replace(".pt", ""))
    license_id = args.license or "apache-2.0"

    # 3. HF upload (or dry-run)
    hf_repo = args.repo or f"<your-hf-username>/atlas-lens-{model_label.lower()}"
    if args.dry_run:
        _safe_print(f"[2/5] (dry-run) would upload to "
                    f"https://huggingface.co/{hf_repo}")
    elif not _upload_to_hf(args, hf_repo, artifact_dir, files_to_upload, model_label,
                           base_model, dim, sha, size, license_id, color):
        return 1

    # 4. Render registry-PR body
    _safe_print("[3/5] Rendering registry-PR body…")
    pr_body = _render_registry_pr_body(model_label, hf_repo, base_model,
                                         dim, sha, license_id,
                                         artifact_files=files_to_upload)

    # 5. Open PR (or print body for paste)
    if args.skip_pr or args.dry_run:
        _print_pr_body_for_paste(args, pr_body, color)
        return 0

    if not publishing.gh_available():
        _print_pr_body_without_gh(pr_body, color)
        return 0

    return _open_registry_pr(model_label, hf_repo, dim, license_id, files_to_upload,
                             pr_body, atlas_root, color)


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

def main(argv: Optional[List[str]] = None) -> int:
    parser = argparse.ArgumentParser(
        prog="atlas lens",
        description="Geometric Lens compat probe + build (PC-057, PC-058)")
    sub = parser.add_subparsers(dest="subcommand")

    p_check = sub.add_parser("check",
        help="probe llama-server for Lens compatibility (PC-057)")
    p_check.add_argument("model", nargs="?", default=None,
        help="registry name or path (default: whatever llama-server has loaded)")
    p_check.add_argument("--json", action="store_true",
        help="machine-readable output")
    p_check.add_argument("--no-color", action="store_true")

    p_build = sub.add_parser("build",
        help="train fresh lens artifacts — C(x) + G(x) — for the loaded "
             "model (PC-058/PC-211)")
    p_build.add_argument("model", nargs="?", default=None,
        help="registry name or path (default: whatever llama-server has loaded)")
    p_build.add_argument("--samples", default=None,
        help="path to a labeled JSON/JSONL training file "
             "(format: [{text, label}, ...]; label is 0 or 1)")
    p_build.add_argument("--from-results", default=None,
        help="train from a benchmark results dir (per_task/) produced by "
             "`atlas bench` — uses this model's own code + pass/fail labels")
    p_build.add_argument("--epochs", type=int, default=200,
        help="training epochs (default: 200)")
    p_build.add_argument("--lr", type=float, default=1e-3,
        help="Adam learning rate (default: 1e-3)")
    p_build.add_argument("--margin", type=float, default=1.0,
        help="contrastive ranking margin (default: 1.0)")
    p_build.add_argument("--artifact-dir", default=None,
        help="where to save the artifacts (cost_field.pt, gx_xgboost.json, "
             "gx_weights.json; default: registry-resolved path)")
    p_build.add_argument("--force", action="store_true",
        help="retrain even if compatible artifacts already exist")
    p_build.add_argument("--dry-run", action="store_true",
        help="extract embeddings but skip training + save")
    p_build.add_argument("--no-telemetry", action="store_true",
        help="with --from-results: don't merge the run's banked "
             "per-candidate embeddings (telemetry/embeddings.emb)")
    p_build.add_argument("--no-color", action="store_true")

    p_pub = sub.add_parser("publish",
        help="upload local artifacts to HF + open registry-PR (PC-059)")
    p_pub.add_argument("model", nargs="?", default=None,
        help="registry name or path of the model these artifacts are for")
    p_pub.add_argument("--repo", default=None,
        help="HF repo to upload to (USERNAME/REPO_NAME). Required unless --dry-run.")
    p_pub.add_argument("--license", default="apache-2.0",
        help="SPDX license id (apache-2.0, mit, bsd-3-clause, ...). "
             "Used in HF model card + registry PR. Must be permissive "
             "for redistribution.")
    p_pub.add_argument("--artifact-dir", default=None,
        help="where local cost_field.pt lives "
             "(default: registry-resolved or ATLAS_LENS_MODELS)")
    p_pub.add_argument("--dry-run", action="store_true",
        help="don't upload, don't open PR — just print the body")
    p_pub.add_argument("--skip-pr", action="store_true",
        help="upload to HF but skip the registry PR (print the body)")
    p_pub.add_argument("--no-color", action="store_true")

    args = parser.parse_args(argv)
    if args.subcommand is None:
        parser.print_help()
        return 1

    color = (sys.stdout.isatty()
             and not getattr(args, "no_color", False)
             and not getattr(args, "json", False))

    if args.subcommand == "check":
        return _emit_check(args, color)
    if args.subcommand == "build":
        return _emit_build(args, color)
    if args.subcommand == "publish":
        return _emit_publish(args, color)
    parser.print_help()
    return 1


if __name__ == "__main__":
    sys.exit(main())
