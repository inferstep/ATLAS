#!/usr/bin/env bash
# ATLAS macOS setup (#32 hybrid path) — installs the native llama-server
# build with Metal acceleration plus the brew deps the rest of the
# install needs. Docker still runs the other 4 ATLAS services (proxy,
# v3, lens, sandbox) via compose/docker-compose.macos.yml.
#
# Why hybrid: Apple Silicon under MoltenVK in Docker Desktop is 5-10x
# slower than native Metal. Running ONLY llama-server natively gets us
# the inference perf without rewriting the entire service stack as
# native Mac binaries (the bulk of the slowdown is inference, not the
# small Python/Go services). See docs/SETUP_MACOS.md for the full
# walkthrough.
#
# Usage:
#   ./scripts/atlas-setup-macos.sh                # default install
#   ./scripts/atlas-setup-macos.sh --rebuild      # force rebuild llama-server
#   ./scripts/atlas-setup-macos.sh --prefix DIR   # install root override
#
# Idempotent — re-running is cheap, only rebuilds when --rebuild is
# passed or when LLAMA_CPP_REV in the Dockerfiles has changed since
# the last build.

set -euo pipefail

# ---------------------------------------------------------------------------
# Config — keep LLAMA_CPP_REV aligned with inference/Dockerfile.v31.
# The CI smoke test (`llama-patches-apply`) gates on cross-Dockerfile
# agreement; this script reads the SHA from the Dockerfile so there's
# one source of truth. If the Dockerfile is missing, fall back to a
# pinned default that we update in lockstep.
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ATLAS_ROOT="$(dirname "$SCRIPT_DIR")"
DEFAULT_PREFIX="$HOME/.atlas/macos"
PREFIX="$DEFAULT_PREFIX"
REBUILD=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --rebuild) REBUILD=1; shift;;
    --prefix)
      [[ $# -ge 2 ]] || { echo "--prefix requires a value" >&2; exit 2; }
      PREFIX="$2"; shift 2;;
    -h|--help)
      sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'
      exit 0;;
    *) echo "unknown flag: $1" >&2; exit 2;;
  esac
done

LLAMA_BUILD_DIR="$PREFIX/llama.cpp"
LLAMA_BIN_DIR="$PREFIX/bin"
LLAMA_SERVER="$LLAMA_BIN_DIR/llama-server-metal"
PATCH_DIR="$ATLAS_ROOT/inference/patches"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

bold()   { printf '\033[1m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
red()    { printf '\033[31m%s\033[0m\n' "$*" >&2; }
step()   { printf '\n\033[1m[%s]\033[0m %s\n' "$1" "$2"; }

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    red "required command not found: $1"
    red "  $2"
    exit 1
  fi
}

extract_llama_rev() {
  # Read the SHA pin from inference/Dockerfile.v31 so this script and
  # the docker builds always agree. If we can't read it, error out
  # rather than guess — a wrong SHA means the patch won't apply.
  local dockerfile="$ATLAS_ROOT/inference/Dockerfile.v31"
  if [[ ! -f "$dockerfile" ]]; then
    red "Dockerfile.v31 not found at $dockerfile"
    red "  Are you running this from inside the ATLAS repo?"
    exit 1
  fi
  local rev
  rev=$(grep -oE 'ARG LLAMA_CPP_REV=[a-f0-9]+' "$dockerfile" | head -1 | cut -d= -f2)
  if [[ -z "$rev" ]]; then
    red "could not extract LLAMA_CPP_REV from $dockerfile"
    exit 1
  fi
  echo "$rev"
}

# ---------------------------------------------------------------------------
# Step 1 — sanity check: macOS + Apple Silicon
# ---------------------------------------------------------------------------

step "1/8" "Verifying macOS + Apple Silicon"

if [[ "$(uname -s)" != "Darwin" ]]; then
  red "this script is for macOS only — got $(uname -s)."
  red "  On Linux use the bootstrap installer or docker compose directly."
  exit 1
fi

ARCH="$(uname -m)"
if [[ "$ARCH" != "arm64" ]]; then
  yellow "Detected $ARCH (Intel Mac). Metal acceleration is Apple-Silicon-only."
  yellow "  llama.cpp will still build but use CPU. For Intel Macs the Docker"
  yellow "  + Vulkan path is usually a better tradeoff. Continue anyway? [y/N]"
  read -r ans
  case "$ans" in y|Y|yes) ;; *) exit 0;; esac
fi

green "  OK: $(uname -s) $(uname -m), $(sw_vers -productVersion)"

# ---------------------------------------------------------------------------
# Step 2 — Xcode Command Line Tools (required for cmake + git +
# Apple's metal-cpp headers).
# ---------------------------------------------------------------------------

step "2/8" "Checking Xcode Command Line Tools"

if ! xcode-select -p >/dev/null 2>&1; then
  yellow "  Xcode CLT not installed. Triggering installer (a GUI dialog will appear)."
  yellow "  Re-run this script once the install completes."
  xcode-select --install || true
  exit 1
fi
green "  OK: $(xcode-select -p)"

# ---------------------------------------------------------------------------
# Step 3 — Homebrew + deps. Don't auto-install brew (security: never
# run a third-party install script unattended). Tell the user how.
# ---------------------------------------------------------------------------

step "3/8" "Checking Homebrew + dependencies"

if ! command -v brew >/dev/null 2>&1; then
  red "Homebrew not installed. Install it manually before re-running:"
  red ""
  red "  /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\""
  red ""
  red "Then follow the post-install instructions to add brew to your PATH."
  exit 1
fi
green "  OK: $(brew --version | head -1)"

# cmake + git + python are the hard requirements. pipx is required on
# macOS because Homebrew Python enforces PEP 668 (externally-managed-
# environment) which blocks `pip install` and even `pip install --user`.
# pipx creates an isolated venv per app and exposes the entrypoint on
# PATH, which is exactly what we need for the atlas CLI. go is needed
# to build the atlas-tui binary (Bubbletea TUI client invoked by the
# atlas wrapper). uv is an optional but recommended replacement for
# pip that's 10-100x faster.
NEED=()
for pkg in cmake git python@3.12 pipx go; do
  if ! brew list --formula "$pkg" >/dev/null 2>&1; then
    NEED+=("$pkg")
  fi
done

if [[ ${#NEED[@]} -gt 0 ]]; then
  yellow "  Missing brew packages: ${NEED[*]}"
  yellow "  Installing now (may take a few minutes)..."
  brew install "${NEED[@]}"
fi
green "  OK: cmake $(cmake --version | head -1 | awk '{print $3}'), git $(git --version | awk '{print $3}')"

# Ensure pipx's bin dir is on PATH for THIS shell. `pipx ensurepath`
# writes the export to ~/.zprofile / ~/.bashrc for future shells; we
# also export it inline so the install step below uses a PATH that
# actually finds the freshly-installed atlas binary.
pipx ensurepath >/dev/null 2>&1 || true
export PATH="$HOME/.local/bin:$PATH"

# uv is optional but recommended for faster atlas-cli installs in dev
# workflows. Suggest without auto-installing.
if ! command -v uv >/dev/null 2>&1; then
  yellow "  Optional: 'uv' (fast Python installer) not found."
  yellow "    brew install uv     # recommended if you'll do a lot of dev"
fi

# ---------------------------------------------------------------------------
# Step 4 — Pin LLAMA_CPP_REV
# ---------------------------------------------------------------------------

step "4/8" "Reading LLAMA_CPP_REV from inference/Dockerfile.v31"

LLAMA_CPP_REV=$(extract_llama_rev)
green "  Pinned SHA: $LLAMA_CPP_REV"

# Decide whether we need to (re)build. We avoid rebuilding when the
# existing binary's stored SHA matches the current pin AND --rebuild
# wasn't requested.
NEED_BUILD=1
SHA_MARKER="$LLAMA_BIN_DIR/.llama_cpp_rev"
if [[ -x "$LLAMA_SERVER" && -f "$SHA_MARKER" && $REBUILD -eq 0 ]]; then
  PREV_REV=$(cat "$SHA_MARKER")
  if [[ "$PREV_REV" == "$LLAMA_CPP_REV" ]]; then
    NEED_BUILD=0
    green "  Existing build is up-to-date (SHA matches). Skipping rebuild."
    yellow "  Pass --rebuild to force a fresh compile."
  fi
fi

# ---------------------------------------------------------------------------
# Step 5 — Fetch + patch llama.cpp at the pinned SHA
# ---------------------------------------------------------------------------

if [[ $NEED_BUILD -eq 1 ]]; then
  step "5/8" "Fetching llama.cpp at $LLAMA_CPP_REV + applying patches"

  # Clean state — easiest way to guarantee a known-good build. Cheap
  # too: shallow fetch is ~30s.
  rm -rf "$LLAMA_BUILD_DIR"
  mkdir -p "$LLAMA_BUILD_DIR"

  pushd "$LLAMA_BUILD_DIR" >/dev/null
    git init -q
    git remote add origin https://github.com/ggml-org/llama.cpp
    git -c http.postBuffer=524288000 \
        fetch --depth 1 origin "$LLAMA_CPP_REV"
    git checkout -q FETCH_HEAD

    # Apply the hidden-states patch (real .patch file, must apply
    # cleanly — CI gates on this via llama-patches-apply).
    if ! git apply --check "$PATCH_DIR/expose-hidden-states.patch"; then
      red "hidden-states patch does not apply to $LLAMA_CPP_REV."
      red "  See docs/TROUBLESHOOTING.md § 'llama.cpp patch drift' for the bump runbook."
      exit 1
    fi
    git apply "$PATCH_DIR/expose-hidden-states.patch"

    # Apply the same spec-decode embeddings fix as the Dockerfiles.
    sed -i.bak '/auto params_dft = params_base;/a\
        params_dft.embedding = false;  \/\/ ATLAS: draft never needs embeddings' \
        tools/server/server-context.cpp
    rm -f tools/server/server-context.cpp.bak
    if ! grep -q 'params_dft.embedding = false' tools/server/server-context.cpp; then
      red "spec-decode embeddings fix did not apply to $LLAMA_CPP_REV."
      exit 1
    fi

    green "  Patches applied."

  # ---------------------------------------------------------------------------
  # Step 6 — Build with Metal. GGML_METAL=ON is the Apple Silicon path;
  # llama.cpp auto-detects metal-cpp via Xcode CLT.
  # ---------------------------------------------------------------------------

  popd >/dev/null

  step "6/8" "Building llama.cpp with Metal (this is the slow step, ~5-10min)"

  pushd "$LLAMA_BUILD_DIR" >/dev/null
    # Build flags:
    #   GGML_METAL=ON               — Apple GPU compute backend
    #   GGML_METAL_USE_BF16=ON      — bf16 support on M3/M4
    #   BUILD_SHARED_LIBS=OFF       — static linking, no runtime libllama
    #   CMAKE_BUILD_TYPE=Release    — optimizations on
    cmake -B build \
      -DGGML_METAL=ON \
      -DGGML_METAL_USE_BF16=ON \
      -DBUILD_SHARED_LIBS=OFF \
      -DCMAKE_BUILD_TYPE=Release
    cmake --build build --config Release -j"$(sysctl -n hw.ncpu)"
  popd >/dev/null

  # Stage all native tools before replacing the installed set. If any
  # replacement fails, restore every prior file so a failed upgrade cannot
  # leave a mixed-revision runtime.
  mkdir -p "$LLAMA_BIN_DIR"
  INSTALL_STAGE=$(mktemp -d "$PREFIX/.atlas-install.XXXXXX")
  INSTALL_BACKUP=$(mktemp -d "$PREFIX/.atlas-backup.XXXXXX")
  install_names=(
    llama-server-metal
    llama-cli-metal
    llama-cvector-generator-metal
    .llama_cpp_rev
  )
  cp "$LLAMA_BUILD_DIR/build/bin/llama-server" \
     "$INSTALL_STAGE/llama-server-metal"
  cp "$LLAMA_BUILD_DIR/build/bin/llama-cli" \
     "$INSTALL_STAGE/llama-cli-metal"
  cp "$LLAMA_BUILD_DIR/build/bin/llama-cvector-generator" \
     "$INSTALL_STAGE/llama-cvector-generator-metal"
  chmod 0755 "$INSTALL_STAGE/llama-server-metal" \
             "$INSTALL_STAGE/llama-cli-metal" \
             "$INSTALL_STAGE/llama-cvector-generator-metal"
  printf '%s\n' "$LLAMA_CPP_REV" > "$INSTALL_STAGE/.llama_cpp_rev"

  for name in "${install_names[@]}"; do
    if [[ -e "$LLAMA_BIN_DIR/$name" ]]; then
      cp -p "$LLAMA_BIN_DIR/$name" "$INSTALL_BACKUP/$name"
    fi
  done

  install_failed=0
  for name in "${install_names[@]}"; do
    if ! mv -f "$INSTALL_STAGE/$name" "$LLAMA_BIN_DIR/$name"; then
      install_failed=1
      break
    fi
  done
  if [[ $install_failed -ne 0 ]]; then
    for name in "${install_names[@]}"; do
      if [[ -e "$INSTALL_BACKUP/$name" ]]; then
        mv -f "$INSTALL_BACKUP/$name" "$LLAMA_BIN_DIR/$name" || true
      else
        rm -f "$LLAMA_BIN_DIR/$name"
      fi
    done
    rm -rf "$INSTALL_STAGE" "$INSTALL_BACKUP"
    red "native binary install failed; restored the previous build."
    exit 1
  fi
  rm -rf "$INSTALL_STAGE" "$INSTALL_BACKUP"
  green "  Built and installed:"
  green "    $LLAMA_SERVER"
  green "    $LLAMA_BIN_DIR/llama-cli-metal"
  green "    $LLAMA_BIN_DIR/llama-cvector-generator-metal"
fi

# ---------------------------------------------------------------------------
# Step 7 — Install atlas CLI. macOS Homebrew Python enforces PEP 668
# (externally-managed-environment), which makes `pip install` and even
# `pip install --user` fail with a refusal message. The only paths that
# work without --break-system-packages are pipx and uv, both of which
# create an isolated venv. We prefer pipx (installed in step 3) because
# it's the standard PEP 668 recommendation and creates a separate venv
# for atlas instead of mingling with other tools.
# ---------------------------------------------------------------------------

step "7/8" "Installing atlas CLI"

# pipx install --force is idempotent — re-running upgrades in place.
# --editable so 'git pull' picks up changes without reinstall.
if command -v pipx >/dev/null 2>&1; then
  (cd "$ATLAS_ROOT" && pipx install --force --editable .)
  green "  Installed via pipx (isolated venv, atlas binary on PATH)."
elif command -v uv >/dev/null 2>&1; then
  # uv tool install is uv's pipx equivalent — also creates an
  # isolated venv. Use --system as a last resort, dodges PEP 668 by
  # uv's own override but pollutes the brew Python environment.
  (cd "$ATLAS_ROOT" && uv tool install --editable .)
  green "  Installed via uv tool install."
else
  red "neither pipx nor uv found. PEP 668 blocks plain pip on Homebrew Python."
  red "  Install one with:"
  red "    brew install pipx     # recommended"
  red "    brew install uv       # faster alternative"
  red "  Then re-run this script."
  exit 1
fi

# Sanity-check that atlas landed on PATH. pipx installs to
# ~/.local/bin by default but the user's shell may not have picked
# that up yet (we exported it in step 3 but only for THIS shell).
if ! command -v atlas >/dev/null 2>&1; then
  yellow "  Warning: 'atlas' not on current PATH despite install succeeding."
  yellow "  Open a new terminal or run:  source ~/.zprofile"
fi

# ---------------------------------------------------------------------------
# Step 8 — Build the atlas-tui binary. The Python `atlas` wrapper
# shells out to atlas-tui (Bubbletea TUI client) for the interactive
# session. Without this, `atlas` errors out telling the user to
# install Go + build manually. We install go via brew in step 3 so
# the build itself is one command. Output goes to ~/.local/bin which
# is already on PATH (pipx puts atlas there too).
# ---------------------------------------------------------------------------

step "8/8" "Building atlas-tui (Go Bubbletea client)"

TUI_OUT="$HOME/.local/bin/atlas-tui"
mkdir -p "$(dirname "$TUI_OUT")"

if [[ ! -d "$ATLAS_ROOT/tui" ]]; then
  red "tui/ directory not found at $ATLAS_ROOT/tui"
  red "  Are you running this from inside a complete ATLAS checkout?"
  exit 1
fi

# go build is idempotent and fast (~10s on M-series). No need to
# skip when --rebuild is unset; the cost is negligible vs the llama
# rebuild, and it picks up any tui/ code changes on git pull.
(cd "$ATLAS_ROOT/tui" && go build -o "$TUI_OUT" .)
green "  Built and installed: $TUI_OUT"

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------

cat <<EOF

$(bold "Setup complete.")

Native llama-server: $LLAMA_SERVER
Install prefix:      $PREFIX

Next steps:
  1. atlas init                                       # wizard writes .env (picks Metal hybrid)
  2. ATLAS_MACOS_PREFIX="$PREFIX" ./scripts/atlas-llama-macos.sh  # start native llama-server
  3. docker compose -f docker-compose.yml \\
                    -f compose/docker-compose.macos.yml up -d # start proxy + v3 + lens + sandbox
  4. atlas doctor                                     # verify install health
  5. atlas                                            # start using ATLAS

If anything in steps 2-5 fails, see docs/SETUP_MACOS.md § Troubleshooting.
EOF
