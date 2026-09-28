# Third-Party Notices

ATLAS itself is AGPL-3.0-or-later (see LICENSE). It builds on the
following third-party work. Versions/revisions are pinned in the files
cited; this document records identity and license.

## Inference engine

| Component | License | Where pinned |
|---|---|---|
| llama.cpp (ggml-org) | MIT | `LLAMA_CPP_REV` in all `inference/Dockerfile*` (CI verifies the ATLAS patches apply to the pinned rev) |

ATLAS carries one patch (`inference/patches/expose-hidden-states.patch`,
per-layer hidden-state extraction) and one in-Dockerfile `sed` fix on
top of the pinned revision — both against MIT-licensed code.

## Go dependencies (TUI)

Charm stack — `bubbletea`, `bubbles`, `lipgloss`, `glamour`, `x/*`,
`colorprofile` (MIT); `chroma` (MIT); `atotto/clipboard` (BSD-3);
`go-osc52` (MIT); `douceur` (MIT). Full pinned list: `tui/go.mod` /
`tui/go.sum`. The proxy (`proxy/go.mod`) is stdlib-only.

## Python dependencies (service images)

`fastapi`, `uvicorn`, `pydantic`, `httpx`, `pyyaml`
(lens/sandbox — MIT/BSD/Apache-2.0); `numpy` (BSD-3); `xgboost` /
`xgboost-cpu` (Apache-2.0); `scikit-learn` (BSD-3); `jinja2` (BSD-3,
the sandbox's template check); `torch` (BSD-style,
lens + v3-service images and the `train` extra); `defusedxml` (PSF);
`tree-sitter` + grammar packages (MIT); `python-multipart`
(Apache-2.0); `tiktoken` (MIT); `gguf` (MIT); `huggingface_hub`
(Apache-2.0); `psutil` (BSD-3); `pytest`, `ruff`, `mypy`, `requests`
(sandbox verify helpers — MIT/Apache-2.0). Pinned in the per-service
`requirements*.txt` files and `pyproject.toml` extras. The core CLI is
stdlib-only by design.

## Infrastructure images

Pinned in the Dockerfiles/compose — digest-pinned except the ROCm/Vulkan
community-backend bases, which are tag-pinned: `python:3.11-slim` (PSF +
Debian), `golang:alpine` + `alpine` (BSD/MIT), `nvidia/cuda:*-rockylinux9`
(NVIDIA Deep Learning Container License + Rocky), `rocm/dev-ubuntu`
(AMD/Canonical, tag-pinned), `ubuntu` (Canonical, tag-pinned for the
Vulkan build), `alpine/socat` (GPL-2.0 — macOS compose overlay only,
where it stands in for the containerized llama-server slot).

## Models

Model weights are **not** distributed with ATLAS; the registry records
license identity per entry and the installer surfaces it:

| Family | License | Notes |
|---|---|---|
| Qwen3.5 (7B/9B/14B/32B GGUF) | Apache-2.0 | Registry-pinned URLs + SHA-256 where anonymously obtainable |
| Gemma 4 (12B) | Gemma Terms of Use | Manual download; ToU is not an OSI license — users accept it upstream |

## Lens / ASA artifacts

Published ATLAS artifacts (cost fields, G(x) bundles, ASA vectors) are
trained by the maintainer on benchmark-derived data (LiveCodeBench —
see its dataset license/terms) against the models above; each bundle's
`model_identity.json` binds it to its backbone. Hosted on Hugging Face
under `itigges22/*`, hash-pinned in the registry.

## Research implementations

ATLAS implements techniques from published research (clean-room, from
the papers): PlanSearch, budget forcing, activation steering,
speculative-decode-era patches, and others — the full citation list
with arXiv links is
`docs/SOURCES.md`. No third-party research *code* is vendored.

## Benchmarks

LiveCodeBench, HumanEval, MBPP/EvalPlus, GPQA, SciCode task sets are
fetched at benchmark time under their own licenses/terms; revisions are
recorded in benchmark run metadata. The in-repo custom task set
(`benchmark/custom/`) is original and covered by the repo license.
