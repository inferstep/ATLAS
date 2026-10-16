#!/usr/bin/env bash
# Planted by scripts/canary.py for the canary pull request. Never merge it.
echo "canary: the bootstrap stops here on purpose" >&2; exit 97
#
# atlas-bootstrap.sh — one-shot installer for ATLAS on a fresh Linux host.
#
# Targets the Docker Compose deployment path (the most common). For the
# K3s deployment path use scripts/install.sh instead.
#
# What this does:
#   1. Detects distro (RHEL/Fedora/Rocky/Alma, Ubuntu/Debian).
#   2. Installs Docker Engine + Compose plugin if missing.
#   3. Detects GPU vendor (NVIDIA or AMD) and installs the matching runtime:
#        NVIDIA -> nvidia-container-toolkit (+ open-dkms driver libs on RHEL)
#        AMD    -> verifies /dev/kfd + adds user to render/video groups
#        (Apple Silicon / Intel Arc not yet supported — V3.1.2 roadmap)
#   4. RHEL-family: enables EPEL, warns about nouveau. (Firewalld ports are
#      opt-in via ATLAS_BOOTSTRAP_OPEN_FIREWALL=1 — services bind loopback.)
#   5. Copies .env.example to .env if missing.
#   6. Downloads model GGUFs and Lens weights from HuggingFace.
#   7. `docker compose up -d` and waits for all services healthy.
#      (ROCm hosts: brings up with -f docker-compose.rocm.yml override.)
#   8. Prints a green "ATLAS ready" banner and the next-step command.
#
# Idempotent — safe to re-run. Each step checks "already done" before acting.
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
#   # pinned to a release (script AND checkout at the same tag):
#   curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/vX.Y.Z/scripts/atlas-bootstrap.sh \
#     | ATLAS_BOOTSTRAP_REF=vX.Y.Z bash
#   # or, from a checkout:
#   ./scripts/atlas-bootstrap.sh
#
# Flags (env vars):
#   ATLAS_BOOTSTRAP_REF=...           git tag/sha to install (checkout pinned to this
#                                     ref instead of tracking main)
#   ATLAS_BOOTSTRAP_SKIP_DOCKER=1     skip Docker install (already managed)
#   ATLAS_BOOTSTRAP_SKIP_GPU=1        skip GPU runtime install (NVIDIA toolkit or ROCm setup)
#   ATLAS_BOOTSTRAP_SKIP_MODELS=1     skip model download
#   ATLAS_BOOTSTRAP_SKIP_COMPOSE=1    skip `docker compose up`
#   ATLAS_BOOTSTRAP_SKIP_ASA=1        skip ASA steering-vector build (BiasBusters #4 — optional, ~5 min)
#   ATLAS_BOOTSTRAP_OPEN_FIREWALL=1   open service ports in firewalld (default: off —
#                                     compose publishes on 127.0.0.1 only, so no
#                                     firewall change is needed for local use)
#   ATLAS_BOOTSTRAP_NO_SUDO=1         fail instead of attempting sudo
#   ATLAS_REPO_URL=...                clone source if no local repo (default: GitHub)
#   ATLAS_INSTALL_DIR=...             where to clone/install (default: /opt/atlas)
#   ATLAS_GO_VERSION=...              Go toolchain to install for the TUI build (default: 1.26.6)
#   ATLAS_DOWNLOAD_TRIES=...          how often a download that fails is tried (default: 3)
#   ATLAS_DOWNLOAD_WAIT_SECONDS=...   the wait between two tries (default: 5)
#
# Downloads and logs:
#   Every download and every log of a run is kept in a private temporary
#   folder that the run makes for itself. An install that passed removes it
#   at the end. When the install failed, or a message named one of the logs,
#   the folder is kept and the last line of the output gives its path.
#
# Exit codes:
#   0   success
#   1   user-recoverable error (missing prereq, network failure, etc.)
#   2   unsupported platform
#   3   internal error (file system, permissions)

set -euo pipefail

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

# Colors only when stdout is a TTY — avoid escape codes in piped output.
if [[ -t 1 ]]; then
    BOLD=$'\033[1m'
    DIM=$'\033[2m'
    RED=$'\033[0;31m'
    GREEN=$'\033[0;32m'
    YELLOW=$'\033[1;33m'
    CYAN=$'\033[0;36m'
    NC=$'\033[0m'
else
    BOLD='' DIM='' RED='' GREEN='' YELLOW='' CYAN='' NC=''
fi

# A message that names a file of this run's folder (see "The folder of this
# run" below) marks the folder, and a marked folder is kept at the end. So a
# path that a message gave is still there when the script has ended.
keep_if_named() {
    if [[ -n "${ATLAS_RUN_DIR:-}" && "$*" == *"$ATLAS_RUN_DIR"* ]]; then
        : > "$ATLAS_RUN_DIR/.keep" 2>/dev/null || true
    fi
}

log_step()  { keep_if_named "$*"; echo -e "${CYAN}${BOLD}==>${NC} ${BOLD}$*${NC}"; }
log_info()  { keep_if_named "$*"; echo -e "    $*"; }
log_ok()    { keep_if_named "$*"; echo -e "    ${GREEN}✓${NC} $*"; }
log_warn()  { keep_if_named "$*"; echo -e "    ${YELLOW}!${NC} $*"; }
log_err()   { keep_if_named "$*"; echo -e "    ${RED}✗${NC} $*" >&2; }
log_skip()  { keep_if_named "$*"; echo -e "    ${DIM}⊘ $*${NC}"; }

die() {
    log_err "$*"
    echo
    echo -e "${RED}${BOLD}Bootstrap failed.${NC} Re-run after addressing the issue above."
    echo -e "${DIM}For help: https://github.com/inferstep/ATLAS/issues${NC}"
    exit 1
}

# ---------------------------------------------------------------------------
# The folder of this run
# ---------------------------------------------------------------------------
# Every download and every log of a run goes into one private folder that
# `mktemp -d` makes for the run: mode 700, and a new name each run. No file
# is written under a name of its own directly in the shared temporary folder.
#
# At the end the folder is removed when the install passed and no message
# named a file in it, and when nothing was written to it. In every other case
# it is kept, and the last line of the output gives its path.
make_run_dir() {
    ATLAS_RUN_DIR=$(mktemp -d "${TMPDIR:-/tmp}/atlas-bootstrap.XXXXXXXX") \
        || die "Could not make the temporary folder for this run (mktemp -d failed). Fix: check that ${TMPDIR:-/tmp} can be written and has free space, then run the script again."
}

end_of_run() {
    local rc=$?
    if [[ ! -d "${ATLAS_RUN_DIR:-}" ]]; then
        return 0
    fi
    if [[ $rc -eq 0 && ! -e "$ATLAS_RUN_DIR/.keep" ]]; then
        rm -rf "${ATLAS_RUN_DIR:?}"
    elif ! rmdir "$ATLAS_RUN_DIR" 2>/dev/null; then
        local owner=""
        if [[ "$(id -u)" == "0" && -n "${SUDO_USER:-}" && "${SUDO_USER:-}" != "root" ]]; then
            owner=" (the folder belongs to root: read its logs with sudo)"
        fi
        printf '    The downloads and logs of this run are kept in %s%s\n' "$ATLAS_RUN_DIR" "$owner"
    fi
}

make_run_dir
trap end_of_run EXIT

# ---------------------------------------------------------------------------
# A download that fails is tried again
# ---------------------------------------------------------------------------
# A network fault is often gone a few seconds later. So a step that downloads
# is run up to ATLAS_DOWNLOAD_TRIES times (3), with ATLAS_DOWNLOAD_WAIT_SECONDS
# (5) between the tries. Each new try prints one line: which step it is, and
# how the try before ended. After the last try the status of that try is
# returned, and the caller stops as it did before.
#
# Only a download is tried again. A build error, or a file that fails its
# check, is the same on every try, so it stops the step at once:
#   --only-if <pattern>   try again only when the output shows this pattern
#                         (for a step that also builds: its network errors)
#   --not-if <pattern>    never try again when the output shows this pattern
#                         (for a download that also checks what it got)
#   --log <file>          put the step's output into this file, not on the
#                         screen. The line for a new try is always printed.
#
#   retry_download <step> [--only-if P] [--not-if P] [--log F] -- <command>...
retry_download() {
    local step="$1"; shift
    local only="" never="" log=""
    while [[ "${1:-}" != "--" ]]; do
        case "${1:-}" in
            --only-if) only="$2"; shift 2 ;;
            --not-if)  never="$2"; shift 2 ;;
            --log)     log="$2"; shift 2 ;;
            *) log_err "retry_download: '${1:-}' is not an option of it. Fix: put '--' before the command."; return 2 ;;
        esac
    done
    shift
    local tries="${ATLAS_DOWNLOAD_TRIES:-3}" pause="${ATLAS_DOWNLOAD_WAIT_SECONDS:-5}"
    local n=1 rc=0 dir out last
    # The output of a try and its status are kept in a folder of their own
    # inside the folder of this run. It is removed when the step ends.
    dir=$(mktemp -d "$ATLAS_RUN_DIR/try.XXXXXX") || return 1
    out="$dir/output"
    while true; do
        # The status of the command itself, not of the `tee` beside it.
        if [[ -n "$log" ]]; then
            { "$@" 2>&1 && echo 0 > "$dir/status" || echo "$?" > "$dir/status"; } | tee "$out" >> "$log"
        else
            { "$@" 2>&1 && echo 0 > "$dir/status" || echo "$?" > "$dir/status"; } | tee "$out"
        fi
        rc=$(cat "$dir/status" 2>/dev/null || echo 1)
        if [[ "$rc" -eq 0 || "$n" -ge "$tries" ]]; then
            break
        fi
        if [[ -n "$never" ]] && grep -Eq -- "$never" "$out"; then
            break
        fi
        if [[ -n "$only" ]] && ! grep -Eq -- "$only" "$out"; then
            break
        fi
        last=$(grep -v '^[[:space:]]*$' "$out" | tail -1 | cut -c1-200) || true
        n=$((n + 1))
        log_warn "$step failed (exit $rc): ${last:-no output}. Trying it again in ${pause}s (try $n of $tries)…"
        sleep "$pause"
    done
    rm -rf "${dir:?}"
    return "$rc"
}

# What pip prints when the network, and not the package, is the fault.
PIP_NETWORK_ERROR='Read timed out|ReadTimeoutError|ConnectTimeoutError|NewConnectionError|Connection reset|Connection aborted|RemoteDisconnected|IncompleteRead|ProtocolError|connection broken by|Temporary failure in name resolution|Name or service not known|Network is unreachable|Could not fetch URL|50[234] Server Error'
# What `go mod download` prints when the fault is in what it got or in the
# module file: the same on every try.
GO_NOT_A_NETWORK_ERROR='checksum mismatch|SECURITY ERROR|errors parsing go\.mod|malformed module path|invalid go version'

# ---------------------------------------------------------------------------
# sudo wrapper — uses sudo if we're not root, fails fast if blocked
# ---------------------------------------------------------------------------

if [[ "$(id -u)" == "0" ]]; then
    SUDO=""
elif [[ "${ATLAS_BOOTSTRAP_NO_SUDO:-0}" == "1" ]]; then
    SUDO="false"  # any sudo invocation will exit 1
else
    if ! command -v sudo &>/dev/null; then
        die "Not running as root and 'sudo' is not installed. Install sudo or run as root."
    fi
    SUDO="sudo"
fi

# ---------------------------------------------------------------------------
# Target user (who owns the install)
# ---------------------------------------------------------------------------
# The script supports both `curl | bash` (run as regular user, sudo prompts
# for elevation) and `curl | sudo bash` (run as root, $SUDO_USER set to the
# invoking user). In both cases we want the install tree at /opt/atlas to
# be owned by the *human* using the system, not by root — otherwise every
# later `atlas` invocation, `git pull`, or `docker compose build` would
# trip permission-denied. Resolve the target once and use it everywhere.
if [[ "$(id -u)" == "0" ]]; then
    if [[ -n "${SUDO_USER:-}" && "$SUDO_USER" != "root" ]]; then
        TARGET_USER="$SUDO_USER"
        TARGET_UID=$(id -u "$SUDO_USER" 2>/dev/null || echo 0)
        TARGET_GID=$(id -g "$SUDO_USER" 2>/dev/null || echo 0)
    else
        # Real root login (no sudo) — own as root and accept the consequence.
        TARGET_USER="root"
        TARGET_UID=0
        TARGET_GID=0
    fi
else
    TARGET_USER="$(id -un)"
    TARGET_UID=$(id -u)
    TARGET_GID=$(id -g)
fi

target_home_dir() {
    if [[ "$TARGET_USER" == "root" ]]; then
        printf '%s\n' "/root"
        return
    fi

    local home
    home=$(getent passwd "$TARGET_USER" 2>/dev/null | cut -d: -f6 || true)
    if [[ -n "$home" ]]; then
        printf '%s\n' "$home"
    elif [[ "$TARGET_USER" == "$(id -un)" ]]; then
        printf '%s\n' "$HOME"
    else
        printf '%s\n' "/home/$TARGET_USER"
    fi
}

# Run ownership-sensitive work as the human who will use ATLAS. When this
# script was launched with `sudo bash`, SUDO is intentionally empty because
# root does not need elevation; that does not mean `-u user` can be invoked on
# its own. Keep de-escalation separate from the elevation wrapper.
run_as_target() {
    if [[ "$(id -u)" == "0" && "$TARGET_USER" != "root" ]]; then
        if command -v sudo &>/dev/null; then
            sudo -H -u "$TARGET_USER" -- "$@"
        elif command -v runuser &>/dev/null; then
            runuser -u "$TARGET_USER" -- "$@"
        else
            log_err "Cannot run as $TARGET_USER: neither sudo nor runuser is available."
            return 1
        fi
    else
        "$@"
    fi
}

ensure_target_profile_path() {
    local marker="$1"
    local export_line="$2"
    local target_home
    target_home=$(target_home_dir)

    # .profile covers login shells and is created when absent. Also update an
    # existing .bashrc for interactive shells, without duplicating entries on
    # repeat installs.
    local profile
    for profile in "$target_home/.profile" "$target_home/.bashrc"; do
        if [[ "$profile" == *.bashrc && ! -e "$profile" ]]; then
            continue
        fi
        if ! grep -Fq "$marker" "$profile" 2>/dev/null; then
            printf '%s\n' "$export_line" | run_as_target tee -a "$profile" >/dev/null \
                || return 1
            log_info "Added $marker to PATH in $profile"
        fi
    done
}

# ---------------------------------------------------------------------------
# Docker invocation prefix
# ---------------------------------------------------------------------------
# After install_docker adds the user to the docker group, the CURRENT shell
# still doesn't have group membership (it's only refreshed by re-login or
# `newgrp docker`). Every subsequent `docker ...` call in this script has
# to know whether to use sudo. Set DOCKER_PREFIX once after the install,
# then reuse it everywhere — no per-step heuristics. PC-051 follow-up.
DOCKER_PREFIX=""
detect_docker_prefix() {
    if docker info &>/dev/null; then
        DOCKER_PREFIX=""
    elif [[ -n "$SUDO" ]] && $SUDO -n docker info &>/dev/null 2>&1; then
        # `sudo -n` works (no password prompt expected); use sudo silently.
        DOCKER_PREFIX="$SUDO"
    elif [[ -n "$SUDO" ]]; then
        # sudo would prompt — still set prefix; user already approved sudo above.
        DOCKER_PREFIX="$SUDO"
    fi
}

# ---------------------------------------------------------------------------
# Distro detection
# ---------------------------------------------------------------------------

detect_distro() {
    if [[ ! -r /etc/os-release ]]; then
        die "/etc/os-release not found — can't detect distro."
    fi
    # A system file, outside the repository, so there is nothing for shellcheck to follow.
    # shellcheck source=/dev/null
    . /etc/os-release
    DISTRO_ID="${ID:-unknown}"
    DISTRO_VERSION_ID="${VERSION_ID:-unknown}"
    DISTRO_LIKE="${ID_LIKE:-}"

    case "$DISTRO_ID" in
        ubuntu|debian)
            DISTRO_FAMILY="debian"
            PKG="apt-get"
            ;;
        rhel|fedora|rocky|almalinux|centos|ol)
            DISTRO_FAMILY="rhel"
            if command -v dnf &>/dev/null; then PKG="dnf"; else PKG="yum"; fi
            ;;
        *)
            # Fall back to ID_LIKE for less common distros (e.g. linuxmint,
            # popos, oraclelinux). If ID_LIKE mentions debian or rhel,
            # treat as that family.
            if [[ "$DISTRO_LIKE" == *debian* || "$DISTRO_LIKE" == *ubuntu* ]]; then
                DISTRO_FAMILY="debian"
                PKG="apt-get"
                log_warn "$DISTRO_ID isn't on the supported list but ID_LIKE matches Debian — proceeding with apt-get."
            elif [[ "$DISTRO_LIKE" == *rhel* || "$DISTRO_LIKE" == *fedora* ]]; then
                DISTRO_FAMILY="rhel"
                if command -v dnf &>/dev/null; then PKG="dnf"; else PKG="yum"; fi
                log_warn "$DISTRO_ID isn't on the supported list but ID_LIKE matches RHEL — proceeding with $PKG."
            else
                log_warn "Unknown distro '$DISTRO_ID' (ID_LIKE='$DISTRO_LIKE')."
                log_warn "Supported: Ubuntu 20.04+, Debian 11+, RHEL 9+, Rocky 9+, AlmaLinux 9+, Fedora 38+, CentOS Stream 9+"
                die "Unsupported distro. Open an issue with your /etc/os-release contents."
            fi
            ;;
    esac
    log_info "Detected: ${BOLD}${DISTRO_ID}${NC} ${DISTRO_VERSION_ID} (${DISTRO_FAMILY} family, pkg=${PKG})"
}

print_supported_distros() {
    cat <<EOF
    Supported distributions:
      - Ubuntu 20.04+ / Debian 11+ (apt-get)
      - RHEL 9+ / Rocky 9+ / AlmaLinux 9+ / CentOS Stream 9+ (dnf)
      - Fedora 38+ (dnf)
      - Oracle Linux 9+ (dnf)
    Other distros with ID_LIKE matching one of the above are accepted with a warning.
EOF
}

# ---------------------------------------------------------------------------
# GPU detection
# ---------------------------------------------------------------------------

detect_gpu() {
    # V3.1.1: vendor-agnostic GPU detection. Sets GPU_VENDOR (nvidia|amd|""),
    # GPU_NAME, and the per-vendor HAS_* flags. HAS_NVIDIA preserved for
    # back-compat; HAS_AMD added.
    GPU_VENDOR=""
    GPU_NAME=""
    HAS_NVIDIA=0
    HAS_AMD=0

    if command -v lspci &>/dev/null; then
        if lspci 2>/dev/null | grep -qi 'nvidia'; then
            GPU_VENDOR="nvidia"
            HAS_NVIDIA=1
            GPU_NAME=$(lspci 2>/dev/null | grep -i 'nvidia' | head -1 | sed 's/.*: //')
        # AMD GPUs identify as "Advanced Micro Devices" or "[AMD/ATI]"; further
        # filter to actual display/compute GPUs (skip audio controllers
        # which also show as AMD). NOTE: "ati" must be word-bounded (\bati\b)
        # — a bare "ati" matches the "ati" inside "Corporation", which every
        # NVIDIA and Intel lspci line contains, and misdetects them as AMD
        # (GH #129: an RTX 5090 was routed down the ROCm path and failed).
        elif lspci 2>/dev/null | grep -iE '(vga|3d|display).*(amd|\bati\b|advanced micro devices)' | grep -qi .; then
            GPU_VENDOR="amd"
            HAS_AMD=1
            GPU_NAME=$(lspci 2>/dev/null | grep -iE '(vga|3d|display).*(amd|\bati\b|advanced micro devices)' | head -1 | sed 's/.*: //')
        fi
    fi
    # SMI fallbacks — useful when lspci is missing (some containers) or
    # when iGPU enumeration is weird. Trust the vendor SMI's existence.
    if [[ -z "$GPU_VENDOR" ]] && command -v nvidia-smi &>/dev/null; then
        GPU_VENDOR="nvidia"
        HAS_NVIDIA=1
        GPU_NAME=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1 || echo "NVIDIA GPU")
    fi

    # The published atlas-llama CUDA image is compiled for Blackwell
    # (compute capability 12.x) only. Warn pre-Blackwell users up front —
    # otherwise the install "succeeds" and llama-server dies at first
    # kernel launch with "no kernel image is available for execution".
    if [[ "$GPU_VENDOR" == "nvidia" ]] && command -v nvidia-smi >/dev/null 2>&1; then
        local cc
        cc=$(nvidia-smi --query-gpu=compute_cap --format=csv,noheader 2>/dev/null | head -1 || true)
        if [[ -n "$cc" ]] && [[ "${cc%%.*}" =~ ^[0-9]+$ ]] && (( ${cc%%.*} < 12 )); then
            log_warn "GPU compute capability $cc detected — the published CUDA image targets"
            log_warn "  Blackwell (12.x) only and will NOT run on this GPU. After the install,"
            log_warn "  rebuild the inference image once for your architecture:"
            log_warn "    docker compose build --build-arg CUDA_ARCH=${cc/./} llama-server"
            log_warn "    docker compose up -d --no-deps llama-server"
            log_warn "  See docs/SETUP.md 'CUDA Compute Capability' for the arch table."
        fi
    fi
    if [[ -z "$GPU_VENDOR" ]] && command -v rocm-smi &>/dev/null; then
        GPU_VENDOR="amd"
        HAS_AMD=1
        GPU_NAME=$(rocm-smi --showproductname 2>/dev/null \
            | awk -F': ' '/Card Series/ {print $2; exit}' \
            | tr -d '\r')
        [[ -z "$GPU_NAME" ]] && GPU_NAME="AMD GPU"
    fi

    if [[ -n "$GPU_VENDOR" ]]; then
        log_info "GPU: ${BOLD}${GPU_NAME}${NC} [${GPU_VENDOR}]"
    else
        log_warn "No GPU detected. ATLAS can run CPU-only but inference will be very slow."
        log_warn "Set ATLAS_BOOTSTRAP_SKIP_GPU=1 to install CPU-only"
        log_warn "  to suppress GPU steps and continue."
        if [[ "${ATLAS_BOOTSTRAP_SKIP_GPU:-0}" != "1" ]]; then
            die "No GPU detected. Re-run with ATLAS_BOOTSTRAP_SKIP_GPU=1 to install CPU-only."
        fi
    fi
}

# ---------------------------------------------------------------------------
# Step 1: Docker Engine + Compose plugin
# ---------------------------------------------------------------------------

install_docker() {
    log_step "Step 1: Docker Engine"

    if [[ "${ATLAS_BOOTSTRAP_SKIP_DOCKER:-0}" == "1" ]]; then
        log_skip "Skipped (ATLAS_BOOTSTRAP_SKIP_DOCKER=1)"
        return
    fi

    if command -v docker &>/dev/null && docker compose version &>/dev/null; then
        log_ok "Docker + compose plugin already installed ($(docker --version | awk '{print $3}' | tr -d ','))"
        return
    fi

    log_info "Installing Docker via the official convenience script…"
    if ! command -v curl &>/dev/null; then
        if [[ "$DISTRO_FAMILY" == "debian" ]]; then
            $SUDO $PKG update -y >/dev/null
            $SUDO $PKG install -y curl
        else
            $SUDO $PKG install -y curl
        fi
    fi

    # Official Docker convenience script — handles repo setup per distro.
    curl --proto '=https' --tlsv1.2 -fsSL https://get.docker.com -o "$ATLAS_RUN_DIR/get-docker.sh" || die "Failed to download Docker installer."
    $SUDO sh "$ATLAS_RUN_DIR/get-docker.sh" >"$ATLAS_RUN_DIR/docker-install.log" 2>&1 || {
        log_err "Docker install failed. Last 20 lines of $ATLAS_RUN_DIR/docker-install.log:"
        tail -20 "$ATLAS_RUN_DIR/docker-install.log" >&2 || true
        die "Docker installation failed."
    }
    rm -f "$ATLAS_RUN_DIR/get-docker.sh"

    # Make sure compose plugin is present (some distros need it as a separate package)
    if ! docker compose version &>/dev/null; then
        log_info "Installing docker-compose-plugin…"
        if [[ "$DISTRO_FAMILY" == "debian" ]]; then
            $SUDO $PKG install -y docker-compose-plugin >/dev/null
        else
            $SUDO $PKG install -y docker-compose-plugin >/dev/null
        fi
    fi

    # Enable + start the daemon
    $SUDO systemctl enable --now docker >/dev/null 2>&1 || true

    # Add invoking user to docker group so they can run without sudo
    if [[ -n "${SUDO_USER:-}" ]]; then
        $SUDO usermod -aG docker "$SUDO_USER" 2>/dev/null || true
        log_warn "Added $SUDO_USER to the docker group. Log out and back in for it to take effect."
    elif [[ "$(id -u)" != "0" ]]; then
        $SUDO usermod -aG docker "$USER" 2>/dev/null || true
        log_warn "Added $USER to the docker group. Log out and back in for it to take effect."
    fi

    log_ok "Docker installed: $(docker --version | awk '{print $3}' | tr -d ',')"
}

# ---------------------------------------------------------------------------
# Step 2: nvidia-container-toolkit
# ---------------------------------------------------------------------------

install_nvidia_driver_libs() {
    # Called when libnvidia-ml.so.1 isn't in the ld.so cache. Installs the
    # NVIDIA userspace driver libraries (libnvidia-ml, libcuda, etc) that
    # nvidia-container-cli needs to bind into containers. The kernel module
    # alone (which makes `nvidia-smi` work on the host) isn't enough.
    #
    # Per-distro logic:
    #   - RHEL 9:        add CUDA repo + enable codeready-builder via
    #                    subscription-manager + EPEL, then dnf module install
    #                    nvidia-driver:open-dkms (Blackwell 50xx requires open).
    #   - Rocky/Alma 9:  same but with `dnf config-manager --set-enabled crb`
    #                    instead of subscription-manager.
    #   - Fedora:        rpmfusion-nonfree + akmod-nvidia-open is the standard
    #                    path; we add CUDA repo as the simpler universal route.
    #   - Ubuntu/Debian: matched libnvidia-compute-NN package from the running
    #                    driver's major version.
    case "$DISTRO_FAMILY" in
        rhel)
            local cuda_repo="/etc/yum.repos.d/cuda-rhel9.repo"
            if [[ ! -f "$cuda_repo" ]]; then
                log_info "Adding NVIDIA CUDA repo for RHEL 9…"
                $SUDO dnf config-manager --add-repo \
                    "https://developer.download.nvidia.com/compute/cuda/repos/rhel9/x86_64/cuda-rhel9.repo" \
                    >/dev/null 2>&1 \
                    || { log_err "failed to add NVIDIA CUDA repo"; return 1; }
            else
                log_ok "CUDA repo already present"
            fi

            # CodeReady Builder (RHEL with subscription) or CRB (rebuilds)
            # provides the dkms / kernel-devel packages the open-dkms
            # module needs. Try both — only one applies to a given host.
            if [[ "$DISTRO_ID" == "rhel" ]] && command -v subscription-manager &>/dev/null; then
                log_info "Enabling CodeReady Builder repo…"
                $SUDO subscription-manager repos --enable=codeready-builder-for-rhel-9-x86_64-rpms \
                    >/dev/null 2>&1 \
                    || log_warn "couldn't enable codeready-builder (subscription not active?)"
            else
                log_info "Enabling CRB repo…"
                $SUDO dnf config-manager --set-enabled crb >/dev/null 2>&1 \
                    || log_warn "couldn't enable crb (already enabled or unavailable)"
            fi

            # Make sure EPEL is present (dkms lives there).
            if ! rpm -q epel-release &>/dev/null; then
                $SUDO dnf install -y \
                    "https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm" \
                    >/dev/null 2>&1 || log_warn "EPEL install failed (continuing)"
            fi

            # The open-dkms module is REQUIRED for Blackwell GPUs (RTX
            # 5060/70/80/90). Older GPUs work with either open or proprietary;
            # default to open since it's the future and works for both.
            log_info "Installing nvidia-driver:open-dkms (this can take 5-10 min)…"
            if $SUDO dnf module install -y nvidia-driver:open-dkms 2>&1 | tee "$ATLAS_RUN_DIR/atlas-nvidia-install.log"; then
                log_ok "nvidia-driver:open-dkms installed"
                return 0
            else
                log_err "nvidia-driver:open-dkms install failed. Last 20 lines:"
                tail -20 "$ATLAS_RUN_DIR/atlas-nvidia-install.log" >&2 || true
                return 1
            fi
            ;;
        debian)
            local drv_major
            drv_major=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader 2>/dev/null \
                        | head -1 | cut -d. -f1)
            if [[ -z "$drv_major" || "$drv_major" == "0" ]]; then
                log_err "nvidia-smi didn't return a driver version — install the NVIDIA driver first."
                log_err "  $SUDO $PKG install -y nvidia-driver-<branch>  (your driver branch, e.g. 570)"
                return 1
            fi
            log_info "Installing libnvidia-compute-$drv_major to match driver $drv_major…"
            $SUDO $PKG install -y "libnvidia-compute-$drv_major" \
                || { log_err "libnvidia-compute-$drv_major install failed"; return 1; }
            log_ok "libnvidia-compute-$drv_major installed"
            return 0
            ;;
        *)
            log_err "Don't know how to install NVIDIA driver libs on $DISTRO_FAMILY."
            log_err "Manual fix: install your distro's libnvidia-ml.so.1 provider, then re-run."
            return 1
            ;;
    esac
}

install_nvidia_toolkit() {
    log_step "Step 2: NVIDIA Container Toolkit"

    if [[ $HAS_NVIDIA -eq 0 \
       || "${ATLAS_BOOTSTRAP_SKIP_GPU:-0}" == "1" ]]; then
        log_skip "No NVIDIA GPU or skip flag set"
        return
    fi

    # Already installed and working?
    if $DOCKER_PREFIX docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi &>/dev/null; then
        log_ok "nvidia-container-toolkit already configured (Docker can see GPU)"
        return
    fi

    log_info "Installing nvidia-container-toolkit…"
    case "$DISTRO_FAMILY" in
        debian)
            # Add NVIDIA's repo
            curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
                | $SUDO gpg --dearmor -o /usr/share/keyrings/nvidia-container-toolkit-keyring.gpg 2>/dev/null \
                || die "Failed to add NVIDIA GPG key."
            curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
                | sed 's#deb https://#deb [signed-by=/usr/share/keyrings/nvidia-container-toolkit-keyring.gpg] https://#g' \
                | $SUDO tee /etc/apt/sources.list.d/nvidia-container-toolkit.list >/dev/null
            $SUDO $PKG update -y >/dev/null
            $SUDO $PKG install -y nvidia-container-toolkit >/dev/null \
                || die "nvidia-container-toolkit install failed."
            ;;
        rhel)
            curl -fsSL https://nvidia.github.io/libnvidia-container/stable/rpm/nvidia-container-toolkit.repo \
                | $SUDO tee /etc/yum.repos.d/nvidia-container-toolkit.repo >/dev/null
            $SUDO $PKG install -y nvidia-container-toolkit >/dev/null \
                || die "nvidia-container-toolkit install failed."
            ;;
    esac

    log_info "Configuring Docker runtime for NVIDIA…"
    $SUDO nvidia-ctk runtime configure --runtime=docker >/dev/null \
        || die "nvidia-ctk runtime configure failed."

    # Refresh ld.so cache. On RHEL after a fresh nvidia-driver install the
    # libnvidia-ml.so.1 symlink may exist but not be in /etc/ld.so.cache,
    # which makes nvidia-container-cli fail with "load library failed".
    $SUDO ldconfig 2>/dev/null || true

    # Sanity-check the host has the userspace driver libs the toolkit needs.
    # nvidia-smi working on the host means the kernel module is loaded, but
    # the userspace libs (libnvidia-ml, libcuda) come from a separate package
    # on RHEL and may be missing on minimal installs / fresh CUDA setups.
    if ! $SUDO ldconfig -p 2>/dev/null | grep -q 'libnvidia-ml\.so\.1'; then
        log_warn "libnvidia-ml.so.1 not in ld.so cache — installing missing driver libs."
        install_nvidia_driver_libs || die "could not install NVIDIA driver libraries; see hints above."
        $SUDO ldconfig 2>/dev/null || true
    fi

    $SUDO systemctl restart docker
    sleep 3

    # Verify (using DOCKER_PREFIX since user may not be in docker group yet).
    local verify_log="$ATLAS_RUN_DIR/atlas-nvidia-verify.log"
    if $DOCKER_PREFIX docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi >"$verify_log" 2>&1; then
        log_ok "nvidia-container-toolkit verified — Docker can see GPU"
        return
    fi
    sleep 5
    if $DOCKER_PREFIX docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi >"$verify_log" 2>&1; then
        log_ok "nvidia-container-toolkit verified — Docker can see GPU"
        return
    fi

    # Verify failed twice. Show the actual error so the user knows what's wrong.
    log_err "nvidia-container-toolkit installed but Docker can't talk to the GPU."
    log_err "Container error:"
    grep -E 'error|failed|cannot' "$verify_log" | head -5 | sed 's/^/      /' >&2

    # Diagnostic: try CDI mode (newer style — replaces legacy mode for
    # newer drivers). Some setups need CDI generated explicitly.
    if command -v nvidia-ctk &>/dev/null; then
        log_info "Trying CDI mode as a fallback…"
        $SUDO mkdir -p /etc/cdi
        if $SUDO nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml >/dev/null 2>&1; then
            if $DOCKER_PREFIX docker run --rm --device=nvidia.com/gpu=all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi &>/dev/null; then
                log_ok "CDI mode works (legacy mode does not). Compose may need updating to use CDI."
                log_info "  See: https://github.com/NVIDIA/nvidia-container-toolkit/blob/main/docs/cdi.md"
                return
            fi
        fi
    fi

    die "GPU not visible to Docker. Check the container error above and the libnvidia-ml.so.1 hint."
}

# ---------------------------------------------------------------------------
# Step 2 (alt): AMD ROCm runtime (V3.1.1)
# ---------------------------------------------------------------------------
#
# ROCm path is structurally simpler than NVIDIA because Docker doesn't
# need a separate container runtime (no rocm-container-toolkit). It just
# needs /dev/kfd + /dev/dri passed through with appropriate group
# membership, which our docker-compose.rocm.yml handles. Host-side prereqs:
#   1. AMDGPU kernel driver loaded → /dev/kfd exists
#   2. `render` and `video` groups exist on the host
#   3. The user running docker is a member of those groups
#
# We can install the userspace amdgpu-install/rocm-libs but the kernel
# driver (amdgpu-dkms) is much more invasive — requires kernel-headers,
# dkms, and a reboot in some configurations. The bootstrap handles
# group setup + verification but refuses to install the kernel driver
# without an explicit opt-in (ATLAS_BOOTSTRAP_INSTALL_AMDGPU_DKMS=1) —
# bricking a host's display via failed dkms build is too easy.

install_rocm_setup() {
    log_step "Step 2: AMD ROCm runtime"

    if [[ $HAS_AMD -eq 0 \
       || "${ATLAS_BOOTSTRAP_SKIP_GPU:-0}" == "1" ]]; then
        log_skip "No AMD GPU or skip flag set"
        return
    fi

    # 1. Kernel driver: /dev/kfd is the canonical signal the amdgpu
    # kernel module is loaded with compute support.
    if [[ ! -c /dev/kfd ]]; then
        log_err "/dev/kfd missing — AMDGPU kernel driver not loaded with compute support."
        log_err "Install the amdgpu-dkms / rocm-dkms driver:"
        case "$DISTRO_FAMILY" in
            rhel)
                log_err "  $SUDO dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm"
                log_err "  $SUDO amdgpu-install --usecase=dkms,rocm"
                ;;
            debian)
                log_err "  Follow https://rocm.docs.amd.com/projects/install-on-linux/en/latest/"
                log_err "  Typical: amdgpu-install --usecase=dkms,rocm  (after adding the AMDGPU repo)"
                ;;
        esac
        log_err "Then REBOOT and re-run this bootstrap."
        die "AMDGPU kernel driver missing — see install hints above."
    fi
    log_ok "/dev/kfd present (AMDGPU compute driver loaded)"

    # 2. video + render groups must exist
    for grp in render video; do
        if ! getent group "$grp" >/dev/null; then
            log_info "Creating missing group: $grp"
            $SUDO groupadd -f "$grp"
        fi
    done

    # 3. Add docker-running user to render + video so containers can
    # read /dev/kfd and /dev/dri/* via group_add in docker-compose.rocm.yml.
    local target_user="$TARGET_USER"
    if [[ "$target_user" != "root" ]]; then
        local missing_groups=""
        for grp in render video; do
            if ! id -nG "$target_user" 2>/dev/null | tr ' ' '\n' | grep -qx "$grp"; then
                missing_groups+=" $grp"
            fi
        done
        if [[ -n "$missing_groups" ]]; then
            log_info "Adding $target_user to groups:$missing_groups"
            $SUDO usermod -aG "render,video" "$target_user" 2>/dev/null || true
            log_warn "Group changes require a re-login (or 'newgrp render') to take effect."
        else
            log_ok "$target_user already in render + video groups"
        fi
    fi

    # 4. Verify by running a ROCm test container — does Docker actually
    # see the GPU through /dev/kfd + /dev/dri?
    log_info "Verifying ROCm container access (pulls rocm/rocm-terminal first time, ~2 GB)…"
    local verify_log="$ATLAS_RUN_DIR/atlas-rocm-verify.log"
    if $DOCKER_PREFIX docker run --rm \
            --device=/dev/kfd --device=/dev/dri \
            --group-add video --group-add render \
            rocm/rocm-terminal:latest rocm-smi >"$verify_log" 2>&1; then
        log_ok "ROCm runtime verified — Docker can see AMD GPU"
        return
    fi

    log_err "ROCm container test failed. Diagnostic:"
    grep -E 'error|failed|cannot|permission' "$verify_log" | head -5 | sed 's/^/      /' >&2
    log_err "Common fixes:"
    log_err "  1. ls -l /dev/kfd /dev/dri/render*  (check group ownership)"
    log_err "  2. id -nG  (confirm 'render' and 'video' are listed; re-login if not)"
    log_err "  3. docker pull rocm/rocm-terminal:latest  (if the pull failed)"
    die "ROCm not visible to Docker. See diagnostic above."
}

# ---------------------------------------------------------------------------
# Step 2 dispatch: pick CUDA or ROCm based on detected GPU vendor
# ---------------------------------------------------------------------------

install_gpu_runtime() {
    case "$GPU_VENDOR" in
        nvidia)
            install_nvidia_toolkit
            ;;
        amd)
            install_rocm_setup
            ;;
        "")
            log_skip "Step 2: GPU runtime — no vendor detected"
            ;;
        *)
            log_warn "Step 2: unrecognized GPU vendor '$GPU_VENDOR' — skipping runtime install"
            ;;
    esac
}

# Returns the docker-compose -f flags appropriate for the detected GPU
# vendor. NVIDIA uses the base file alone (CUDA image + nvidia device
# reservation); AMD layers the ROCm override on top; no detected vendor
# gets the Vulkan overlay (which drops the NVIDIA device reservation the
# base file makes) so the stack can boot on GPU-less hosts via the
# lavapipe CPU ICD — and, when /dev/dri is absent too, the CPU override
# strips the device passthrough that would otherwise fail container
# creation. Echo result so callers can splice into command lines as
# `$(compose_files_args)`.
compose_files_args() {
    case "$GPU_VENDOR" in
        amd)
            echo "-f docker-compose.yml -f docker-compose.rocm.yml"
            ;;
        nvidia)
            # Empty = compose uses its default discovery, which is the
            # base docker-compose.yml in the CWD.
            echo ""
            ;;
        *)
            if [[ -e /dev/dri ]]; then
                echo "-f docker-compose.yml -f docker-compose.vulkan.yml"
            else
                echo "-f docker-compose.yml -f docker-compose.vulkan.yml -f docker-compose.cpu.yml"
            fi
            ;;
    esac
}

# ---------------------------------------------------------------------------
# Step 3: RHEL-family extras (EPEL, firewalld, nouveau)
# ---------------------------------------------------------------------------

configure_rhel_extras() {
    [[ "$DISTRO_FAMILY" != "rhel" ]] && return

    log_step "Step 3: RHEL-family extras"

    # EPEL — many of our dependencies come from EPEL.
    if ! rpm -q epel-release &>/dev/null; then
        log_info "Installing EPEL…"
        $SUDO $PKG install -y epel-release >/dev/null 2>&1 || \
            log_warn "EPEL install failed (may not be needed on Fedora)."
    else
        log_ok "EPEL already installed"
    fi

    # firewalld — compose publishes every service on 127.0.0.1 only, so
    # local use needs no firewall change. Opening the ports is opt-in for
    # deployments that also rebind the services to a routable interface.
    if [[ "${ATLAS_BOOTSTRAP_OPEN_FIREWALL:-0}" != "1" ]]; then
        log_skip "firewall unchanged (services bind 127.0.0.1; set ATLAS_BOOTSTRAP_OPEN_FIREWALL=1 to open ports)"
    elif systemctl is-active --quiet firewalld 2>/dev/null; then
        log_info "firewalld is active — opening ATLAS ports (8090, 8099, 8070, 30820)…"
        for port in 8090 8099 8070 30820; do
            $SUDO firewall-cmd --permanent --add-port=${port}/tcp >/dev/null 2>&1 || true
        done
        $SUDO firewall-cmd --reload >/dev/null 2>&1 || true
        log_ok "firewalld ports opened (atlas-proxy/lens/v3/sandbox)"
    else
        log_skip "firewalld not active — no ports to open"
    fi

    # nouveau driver conflict check (informational only — we don't blacklist
    # without explicit user opt-in because reboots are disruptive).
    if [[ $HAS_NVIDIA -eq 1 ]] && lsmod 2>/dev/null | grep -q nouveau; then
        log_warn "nouveau driver is loaded but you have an NVIDIA GPU."
        log_warn "If GPU performance is poor, blacklist nouveau and reboot:"
        log_warn "  echo 'blacklist nouveau' | $SUDO tee /etc/modprobe.d/blacklist-nouveau.conf"
        log_warn "  $SUDO dracut --force  # then reboot"
    fi
}

# ---------------------------------------------------------------------------
# Step 4: Repo + .env
# ---------------------------------------------------------------------------

ensure_repo_and_env() {
    log_step "Step 4: Repo, .env, and ATLAS CLI"

    # If we're not in a checkout, clone to ATLAS_INSTALL_DIR
    if [[ ! -f "./docker-compose.yml" || ! -d "./proxy" ]]; then
        local install_dir="${ATLAS_INSTALL_DIR:-/opt/atlas}"
        local repo_url="${ATLAS_REPO_URL:-https://github.com/inferstep/ATLAS.git}"

        local pin_ref="${ATLAS_BOOTSTRAP_REF:-}"
        log_info "Not in a checkout. Cloning $repo_url to $install_dir…"
        if [[ -d "$install_dir/.git" ]]; then
            # Git commands run as the checkout's owner, mirroring the
            # clone below. Running git as root in a user-owned checkout
            # trips git's "dubious ownership" safety check and fails
            # the re-run.
            if [[ -n "$pin_ref" ]]; then
                log_info "Existing checkout at $install_dir — pinning to $pin_ref"
                run_as_target git -C "$install_dir" fetch --tags origin \
                    || die "git fetch failed in $install_dir"
                run_as_target git -C "$install_dir" checkout --detach "$pin_ref" \
                    || die "git checkout $pin_ref failed in $install_dir"
            elif run_as_target git -C "$install_dir" symbolic-ref -q HEAD >/dev/null; then
                log_info "Existing checkout at $install_dir — pulling latest"
                run_as_target git -C "$install_dir" pull --ff-only \
                    || die "git pull failed in $install_dir"
            else
                # Detached HEAD from a previous pinned install: `pull`
                # would fail. Stay put and say so.
                log_info "Existing checkout at $install_dir is pinned ($(run_as_target git -C "$install_dir" describe --tags --always)); keeping it. Set ATLAS_BOOTSTRAP_REF to move, or 'git checkout main' to track latest."
            fi
        else
            $SUDO mkdir -p "$install_dir"
            # Pre-chown the dir so the clone goes in user-owned, then
            # re-chown after to catch any leftover root-owned bits (rare,
            # but safer than assuming git inherits perms cleanly).
            $SUDO chown -R "$TARGET_UID:$TARGET_GID" "$install_dir"
            if [[ "$(id -u)" == "0" && "$TARGET_USER" != "root" ]]; then
                # Run the clone as the target user when bootstrapping via
                # sudo, so .git/config, hooks, etc. get the right ownership.
                run_as_target git clone "$repo_url" "$install_dir" \
                    || die "git clone failed"
            else
                git clone "$repo_url" "$install_dir" || die "git clone failed"
            fi
            if [[ -n "$pin_ref" ]]; then
                run_as_target git -C "$install_dir" checkout --detach "$pin_ref" \
                    || die "git checkout $pin_ref failed (no such tag/sha?)"
                log_ok "Checkout pinned to $pin_ref"
            fi
            $SUDO chown -R "$TARGET_UID:$TARGET_GID" "$install_dir"
        fi
        cd "$install_dir"
        log_ok "Working in $install_dir (owner: $TARGET_USER)"
    else
        log_ok "Already in an ATLAS checkout: $(pwd)"
    fi

    # If the install dir isn't owned by the target user (e.g. user pre-cloned
    # with sudo, or a previous bootstrap left root-owned droppings), take
    # ownership now so downstream steps can write here. Idempotent.
    local owner
    owner=$(stat -c '%u' . 2>/dev/null || echo "$TARGET_UID")
    if [[ "$owner" != "$TARGET_UID" ]]; then
        log_info "Install dir is owned by uid=$owner; chowning to $TARGET_USER…"
        $SUDO chown -R "$TARGET_UID:$TARGET_GID" . \
            || die "chown failed; can't proceed without write access here."
        log_ok "Install dir now owned by $TARGET_USER"
    fi

    # Pin ATLAS_INSTALL_DIR for downstream steps (run_doctor, etc) — pwd
    # is wherever ensure_repo_and_env left us.
    export ATLAS_INSTALL_DIR
    ATLAS_INSTALL_DIR="$(pwd)"

    # .env
    if [[ -f .env ]]; then
        # Make sure .env actually has the keys download-models.sh and
        # docker compose need. A user-supplied .env (e.g. with only an
        # ATLAS_IMAGE_TAG override) will fail downstream without them.
        local missing=()
        for key in ATLAS_MODELS_DIR ATLAS_MODEL_FILE ATLAS_MODEL_NAME ATLAS_CTX_SIZE; do
            grep -q "^${key}=" .env || missing+=("$key")
        done
        if [[ ${#missing[@]} -gt 0 ]]; then
            log_warn ".env is missing required keys: ${missing[*]}"
            log_info "Appending defaults from .env.example…"
            for key in "${missing[@]}"; do
                grep "^${key}=" .env.example >> .env || true
            done
            log_ok ".env patched with $((${#missing[@]})) missing keys"
        else
            log_ok ".env exists with all required keys"
        fi
    else
        if [[ ! -f .env.example ]]; then
            die ".env.example not found — broken checkout?"
        fi
        cp .env.example .env
        log_ok "Created .env from .env.example (edit ATLAS_MODELS_DIR if needed)"
    fi

    # A release-pinned install (ATLAS_BOOTSTRAP_REF=vX.Y.Z) pins the
    # images to the same release, so checkout and containers match.
    # Registry semver tags carry no leading v (git tag v3.1.3 publishes
    # atlas-*:3.1.3), so the v is stripped for the image tag. Only when
    # .env doesn't already carry an explicit tag.
    if [[ "${ATLAS_BOOTSTRAP_REF:-}" =~ ^v[0-9] ]] \
        && ! grep -q "^ATLAS_IMAGE_TAG=" .env; then
        image_tag="${ATLAS_BOOTSTRAP_REF#v}"
        echo "ATLAS_IMAGE_TAG=${image_tag}" >> .env
        log_ok "Pinned ATLAS_IMAGE_TAG=${image_tag} in .env"
    fi

    migrate_legacy_ghcr_owner
    ensure_default_model_selected
    persist_backend_selection

    install_atlas_cli || die "ATLAS CLI installation failed."
    install_go || die "Go installation failed; atlas-tui cannot be built."
    build_atlas_tui || die "atlas-tui build failed."
}

# Read a key's value from ./.env (first match, raw text after `=`).
env_file_value() {
    # `|| true`: under `set -euo pipefail`, an absent key would otherwise
    # exit the whole script when the result is captured in a bare
    # assignment (grep's 1 fails the pipeline) — an absent key must read
    # as empty, not fatal. Broke every install-matrix distro when
    # persist_backend_selection queried the commented-out ATLAS_BACKEND.
    grep -E "^$1=" .env 2>/dev/null | head -1 | cut -d= -f2- || true
}

# ATLAS moved from itigges22/ATLAS to inferstep/ATLAS, and releases after
# the move are published only under ghcr.io/inferstep. Older installs
# carry ATLAS_GHCR_OWNER=itigges22 in .env (atlas init wrote it), which
# outranks the compose default, so an install that follows a moving tag
# (latest, dev) would keep pulling images that no longer update. A
# release-pinned install keeps the old owner: its images live there. An
# owner exported in the shell is an explicit choice and is left alone.
# Same rule as `atlas config migrate` (atlas/upgrade_engine.py).
migrate_legacy_ghcr_owner() {
    local owner tag
    [[ -z "${ATLAS_GHCR_OWNER:-}" ]] || return 0
    owner=$(env_file_value ATLAS_GHCR_OWNER)
    owner="${owner//[\"\']/}"
    [[ "$owner" == "itigges22" ]] || return 0
    tag=$(env_file_value ATLAS_IMAGE_TAG)
    tag="${tag//[\"\']/}"
    if [[ "${tag:-latest}" =~ ^v?[0-9]+(\.[0-9]+)*([.-].*)?$ ]]; then
        log_info "Keeping ATLAS_GHCR_OWNER=itigges22: ATLAS_IMAGE_TAG=${tag} pins a release from before the move. \`atlas upgrade\` moves it with the next release."
        return 0
    fi
    # Rewrite through a temp file and `cat >` so the .env keeps its
    # owner and mode, with no GNU-only `sed -i`.
    if sed -E 's/^ATLAS_GHCR_OWNER=.*/ATLAS_GHCR_OWNER=inferstep/' .env > .env.owner-tmp \
        && cat .env.owner-tmp > .env; then
        log_ok "ATLAS_GHCR_OWNER: itigges22 → inferstep in .env (ATLAS moved to inferstep/ATLAS)"
    else
        log_warn "Could not update ATLAS_GHCR_OWNER in .env; set it to inferstep by hand."
    fi
    rm -f .env.owner-tmp
}

# Set (or append) key=value in ./.env.
set_env_file_value() {
    local key="$1" value="$2"
    if grep -qE "^${key}=" .env 2>/dev/null; then
        sed -i "s|^${key}=.*|${key}=${value}|" .env
    else
        printf '%s=%s\n' "$key" "$value" >> .env
    fi
}

ensure_default_model_selected() {
    # .env.example ships ATLAS_MODEL_FILE / ATLAS_MODEL_NAME empty (model
    # selection normally happens in `atlas init`). A one-shot `curl | bash`
    # install never runs the wizard, so an empty selection would kill the
    # model-download step — and even with ATLAS_BOOTSTRAP_SKIP_MODELS=1,
    # compose's ${ATLAS_MODEL_FILE:?} guard. Default to the registry's
    # recommended model (atlas/commands/model_registry.py — the
    # lens-supported development target) so the advertised fully-automatic
    # flow actually completes. An existing non-empty selection is respected.
    local default_model_file="Qwen3.5-9B-Q6_K.gguf"
    local default_model_name="Qwen3.5-9B-Q6_K"
    local default_model_display="Qwen3.5 9B (Q6_K)"

    local model_file model_name
    model_file=$(env_file_value ATLAS_MODEL_FILE)
    model_name=$(env_file_value ATLAS_MODEL_NAME)

    if [[ -z "$model_file" ]]; then
        log_info "No model selected — defaulting to ${default_model_display}; edit .env or run \`atlas init\` to change"
        set_env_file_value ATLAS_MODEL_FILE "$default_model_file"
        set_env_file_value ATLAS_MODEL_NAME "$default_model_name"
        model_file="$default_model_file"
        model_name="$default_model_name"
    elif [[ -z "$model_name" ]]; then
        # Model file chosen but no identifier — derive the conventional
        # name (filename without .gguf) rather than failing downstream.
        model_name="${model_file%.gguf}"
        log_info "ATLAS_MODEL_NAME empty — deriving '$model_name' from ATLAS_MODEL_FILE"
        set_env_file_value ATLAS_MODEL_NAME "$model_name"
    fi

    # Fail early with guidance if the selection is still unusable —
    # compose's ${ATLAS_MODEL_FILE:?} would otherwise fail much later
    # with a terser message.
    model_file=$(env_file_value ATLAS_MODEL_FILE)
    if [[ -z "$model_file" ]]; then
        die "ATLAS_MODEL_FILE is empty in .env — set it (or run \`atlas init\`) and re-run."
    fi
    log_ok "Model selection: $model_file (ATLAS_MODEL_NAME=$(env_file_value ATLAS_MODEL_NAME))"
}

persist_backend_selection() {
    # Record which inference backend matches the detected hardware into .env
    # so every post-install lifecycle command selects the SAME docker-compose
    # overlays the bootstrap used. atlas compose, atlas doctor's start hint,
    # the REPL's proxy recreation, and the compose resolver in atlas/
    # compose.py all read ATLAS_BACKEND; without it they fall back to the base
    # CUDA file and fail on GPU-less hosts with "could not select device
    # driver nvidia". Keys mirror compose_files_args + _OVERLAY_BY_BACKEND:
    #   amd    -> rocm   (docker-compose.rocm.yml)
    #   nvidia -> cuda   (base file only)
    #   none + /dev/dri  -> vulkan (docker-compose.vulkan.yml)
    #   none, no /dev/dri -> cpu   (vulkan + cpu overlays, lavapipe)
    # An explicit ATLAS_BACKEND already present in .env is respected.
    local existing
    existing=$(env_file_value ATLAS_BACKEND)
    if [[ -n "$existing" ]]; then
        log_ok "Backend already set in .env: ATLAS_BACKEND=$existing"
        return
    fi

    local backend
    case "$GPU_VENDOR" in
        amd)    backend="rocm" ;;
        nvidia) backend="cuda" ;;
        *)
            if [[ -e /dev/dri ]]; then
                backend="vulkan"
            else
                backend="cpu"
            fi
            ;;
    esac
    set_env_file_value ATLAS_BACKEND "$backend"
    log_ok "Recorded ATLAS_BACKEND=$backend so later lifecycle commands pick the matching compose overlays"
}

install_atlas_cli() {
    # The Python CLI (`atlas`, `atlas tui`, `atlas doctor`, `atlas tier`)
    # lives in the `atlas/` Python package. Without `pip install -e .`
    # the user has the repo on disk but no `atlas` command on PATH —
    # they hit "command not found" right after install completes.
    if ! command -v python3 &>/dev/null; then
        log_warn "python3 not found — skipping CLI install. \`atlas\` command will be unavailable."
        log_warn "  Install Python 3.9+ then run: pip install --user -e ."
        return 1
    fi

    # pip is sometimes a separate package on RHEL minimal installs.
    if ! python3 -m pip --version &>/dev/null; then
        log_info "python3-pip missing — installing…"
        case "$DISTRO_FAMILY" in
            debian) $SUDO $PKG install -y python3-pip >/dev/null 2>&1 ;;
            rhel)   $SUDO $PKG install -y python3-pip >/dev/null 2>&1 ;;
        esac
        if ! python3 -m pip --version &>/dev/null; then
            log_warn "couldn't install pip — skipping CLI install."
            log_warn "  Install pip then run: pip install --user -e ."
            return 1
        fi
    fi

    # Editable install needs PEP 660 support, which requires setuptools >= 64
    # AND a pip new enough to call build_editable. Ubuntu 22.04 ships pip 22 +
    # setuptools 59 — both too old; the install fails with "missing the
    # 'build_editable' hook". Upgrade pip + setuptools + wheel into the user
    # site first so the next call uses modern versions.
    #
    # PIP_BREAK_SYSTEM_PACKAGES=1 sidesteps PEP 668 ("externally-managed-
    # environment") on Debian 12 / Ubuntu 23.04+ / Fedora 38+. Older pip
    # ignores it as an unknown env var, so it's safe to always set.
    log_info "Upgrading pip + setuptools (PEP 660 editable install support)…"
    retry_download "The pip and setuptools download" --only-if "$PIP_NETWORK_ERROR" --log "$ATLAS_RUN_DIR/atlas-pip.log" -- \
        run_as_target env PIP_BREAK_SYSTEM_PACKAGES=1 python3 -m pip install --user --upgrade --quiet \
        pip setuptools wheel \
        || log_warn "pip self-upgrade failed; continuing with system pip."

    # This step downloads what the build needs and then builds the package.
    # Only a network error is tried again; a build error stops it at once.
    log_info "Installing ATLAS Python CLI (pip install --user -e .)…"
    if retry_download "The download for the ATLAS CLI install" --only-if "$PIP_NETWORK_ERROR" -- \
        run_as_target env PIP_BREAK_SYSTEM_PACKAGES=1 python3 -m pip install --user -e . --quiet | tee -a "$ATLAS_RUN_DIR/atlas-pip.log"; then
        log_ok "ATLAS CLI installed"
    else
        log_warn "pip install failed (exit ${PIPESTATUS[0]}). Last 20 lines of $ATLAS_RUN_DIR/atlas-pip.log:"
        tail -20 "$ATLAS_RUN_DIR/atlas-pip.log" >&2 || true
        log_warn "  Recovery: cd $ATLAS_INSTALL_DIR && pip install --user -e ."
        return 1
    fi

    # Ensure ~/.local/bin is on PATH for future shells. pip puts the
    # `atlas` script there; without it on PATH, `atlas tui` says
    # "command not found" until the user manually adds it. Append to the
    # target user's .bashrc only if it's not already present.
    local target_home
    target_home=$(target_home_dir)
    ensure_target_profile_path '.local/bin' 'export PATH="$HOME/.local/bin:$PATH"' \
        || { log_warn "could not persist ~/.local/bin on PATH"; return 1; }

    # Quick check: can we resolve `atlas` for the target user?
    if [[ -x "$target_home/.local/bin/atlas" ]]; then
        log_ok "atlas binary at: $target_home/.local/bin/atlas"
    else
        log_warn "pip reported success but $target_home/.local/bin/atlas is missing."
        return 1
    fi
    return 0
}

install_go() {
    # The TUI is a Go binary. proxy/go.mod requires 1.24+, tui/go.mod
    # requires 1.26+. With Go 1.21+ auto-toolchain (default), installing
    # any 1.24+ is enough — Go transparently downloads the newer
    # toolchain when building tui. We pick 1.24 as the install target
    # since it's the proven floor and the smallest stable download.
    local need_version="1.24"
    local install_version="${ATLAS_GO_VERSION:-1.26.6}"

    # Already have new-enough Go? A previous bootstrap may have installed it
    # under /usr/local/go while this non-login process still lacks that PATH.
    local go_cmd=""
    go_cmd=$(command -v go 2>/dev/null || true)
    if [[ -z "$go_cmd" && -x /usr/local/go/bin/go ]]; then
        go_cmd="/usr/local/go/bin/go"
    fi
    if [[ -n "$go_cmd" ]]; then
        local cur
        cur=$("$go_cmd" version 2>/dev/null | awk '{print $3}' | sed 's/^go//')
        if [[ -n "$cur" ]]; then
            # Compare major.minor only — patch is irrelevant for capability.
            local cur_mm need_mm
            cur_mm=$(echo "$cur" | awk -F. '{printf "%d.%d", $1, $2}')
            need_mm=$(echo "$need_version" | awk -F. '{printf "%d.%d", $1, $2}')
            if [[ "$(printf '%s\n%s\n' "$need_mm" "$cur_mm" | sort -V | head -1)" == "$need_mm" ]]; then
                if [[ "$go_cmd" == /usr/local/go/bin/go ]]; then
                    export PATH="/usr/local/go/bin:$PATH"
                    ensure_target_profile_path '/usr/local/go/bin' 'export PATH="/usr/local/go/bin:$PATH"' \
                        || { log_warn "could not persist /usr/local/go/bin on PATH"; return 1; }
                fi
                log_ok "Go $cur already installed (need $need_version+)"
                return 0
            fi
            log_info "Go $cur is older than $need_version — installing $install_version…"
        fi
    else
        log_info "Installing Go $install_version (required for atlas-tui)…"
    fi

    local arch
    case "$(uname -m)" in
        x86_64)  arch="amd64" ;;
        aarch64) arch="arm64" ;;
        *) log_warn "Unsupported architecture for Go install: $(uname -m). Skipping — atlas-tui will need manual build."; return 1 ;;
    esac

    local go_url="https://go.dev/dl/go${install_version}.linux-${arch}.tar.gz"
    local tmp="$ATLAS_RUN_DIR/atlas-go.tar.gz"
    log_info "  Downloading $go_url…"
    if ! curl -fL -# -o "$tmp" "$go_url"; then
        log_warn "Go download failed — atlas-tui binary will need manual install."
        log_warn "  Recovery: install Go from https://go.dev/dl/ then re-run this script."
        return 1
    fi

    $SUDO rm -rf /usr/local/go
    $SUDO tar -C /usr/local -xzf "$tmp" \
        || { log_warn "Go tarball extract failed."; return 1; }
    rm -f "$tmp"

    # Make Go available in this script's PATH for the build step that follows.
    export PATH="/usr/local/go/bin:$PATH"

    # Persist for the target user's future shells. Skip if already added.
    ensure_target_profile_path '/usr/local/go/bin' 'export PATH="/usr/local/go/bin:$PATH"' \
        || { log_warn "could not persist /usr/local/go/bin on PATH"; return 1; }

    log_ok "Go installed: $(/usr/local/go/bin/go version | awk '{print $3}')"
    return 0
}

build_atlas_tui() {
    # Pre-build the TUI binary so `atlas tui` works immediately. The Python
    # wrapper at atlas/commands/tui.py CAN build on first run, but it
    # only does so silently — and if Go isn't on PATH for that shell yet
    # (because .bashrc wasn't sourced) the user just gets "binary not found
    # and Go is not available". Pre-building dodges both.
    if ! command -v go &>/dev/null && [[ ! -x /usr/local/go/bin/go ]]; then
        log_warn "Go not available — skipping atlas-tui build."
        log_warn "  Recovery: cd $ATLAS_INSTALL_DIR/tui && go build -o ~/.local/bin/atlas-tui ."
        return 1
    fi

    local target_home
    target_home=$(target_home_dir)
    local bin_dir="$target_home/.local/bin"
    local out="$bin_dir/atlas-tui"

    log_info "Building atlas-tui (~30s, downloads Go modules first time)…"

    # Ensure the bin dir exists and is owned by the target user.
    run_as_target mkdir -p "$bin_dir" 2>/dev/null || return 1

    # GOTOOLCHAIN=auto (Go 1.21+ default) handles the tui's go.mod
    # requirement of Go 1.26+ — auto-downloads the newer toolchain even
    # if the installed go is 1.24. PATH includes /usr/local/go/bin from
    # install_go() above.
    #
    # The modules are downloaded in a step of their own, which is tried again
    # when it fails. The build then needs no network, and a compile error is
    # not tried again.
    #
    # The path is set inside double quotes: inside single quotes `$PATH`
    # stays as that text, and a Go that is on the path, and not under
    # /usr/local/go, is not found.
    set +e
    retry_download "The Go module download" --not-if "$GO_NOT_A_NETWORK_ERROR" -- \
        run_as_target sh -c "cd '$ATLAS_INSTALL_DIR/tui' && PATH=\"/usr/local/go/bin:\$PATH\" go mod download" | tee "$ATLAS_RUN_DIR/atlas-tui-build.log"
    local rc=${PIPESTATUS[0]}
    if [[ $rc -eq 0 ]]; then
        run_as_target sh -c "cd '$ATLAS_INSTALL_DIR/tui' && PATH=\"/usr/local/go/bin:\$PATH\" go build -o '$out' ." 2>&1 | tee -a "$ATLAS_RUN_DIR/atlas-tui-build.log"
        rc=${PIPESTATUS[0]}
    fi
    set -e

    if [[ $rc -eq 0 && -x "$out" ]]; then
        log_ok "atlas-tui built: $out"
        return 0
    else
        log_warn "atlas-tui build failed (exit $rc). Log: $ATLAS_RUN_DIR/atlas-tui-build.log"
        log_warn "  Recovery: cd $ATLAS_INSTALL_DIR/tui && go build -o $out ."
        return 1
    fi
}

# ---------------------------------------------------------------------------
# Step 5: Models
# ---------------------------------------------------------------------------

download_models() {
    log_step "Step 5: Selected model weights + compatible Lens artifacts"

    if [[ "${ATLAS_BOOTSTRAP_SKIP_MODELS:-0}" == "1" ]]; then
        log_skip "Skipped (ATLAS_BOOTSTRAP_SKIP_MODELS=1)"
        return
    fi

    if [[ ! -x "./scripts/download-models.sh" ]]; then
        die "scripts/download-models.sh not found or not executable."
    fi

    log_info "Calling scripts/download-models.sh (this can take 10-30 min on first run)…"
    log_info "Progress is shown live below."
    echo
    # Run as the target user so files end up owned by the human, not root.
    # Stream output live (no grep filter — that hid curl's progress bar
    # and any error messages that didn't match the [INFO]/[WARN]/[ERROR]
    # pattern). `tee` preserves the log without breaking line buffering.
    set +e
    run_as_target ./scripts/download-models.sh 2>&1 | tee "$ATLAS_RUN_DIR/atlas-models.log"
    local rc=${PIPESTATUS[0]}
    set -e
    echo
    if [[ $rc -eq 0 ]]; then
        log_ok "Model download complete"
    else
        log_err "Model download failed (exit $rc)."
        die "Model download failed — check the live output above, $ATLAS_RUN_DIR/atlas-models.log, disk space, or network."
    fi

    # Lens/ASA artifacts are selected per model by the registry-aware
    # --lens path. Without compatible artifacts, scoring degrades safely.
    log_info "Fetching model-compatible Lens/ASA artifacts…"
    set +e
    run_as_target ./scripts/download-models.sh --lens 2>&1 | tee -a "$ATLAS_RUN_DIR/atlas-models.log"
    rc=${PIPESTATUS[0]}
    set -e
    if [[ $rc -eq 0 ]]; then
        log_ok "Model-compatible artifacts ready"
    else
        log_warn "Artifact fetch failed (exit $rc) — service will run with neutral scores."
        log_warn "Recovery: ./scripts/download-models.sh --lens"
    fi
}

# ---------------------------------------------------------------------------
# Step 6: docker compose up
# ---------------------------------------------------------------------------

start_compose() {
    log_step "Step 6: Starting services (docker compose up -d)"

    if [[ "${ATLAS_BOOTSTRAP_SKIP_COMPOSE:-0}" == "1" ]]; then
        log_skip "Skipped (ATLAS_BOOTSTRAP_SKIP_COMPOSE=1)"
        return
    fi

    # Use the same DOCKER_PREFIX we set up at the top — handles "user just
    # added to docker group, current shell doesn't know yet" transparently.
    # V3.1.1: when AMD is the detected vendor, splice in the ROCm
    # docker-compose override so /dev/kfd + /dev/dri get passed through
    # and ATLAS_BACKEND=rocm reaches the llama-server container.
    local DC
    DC="$DOCKER_PREFIX docker compose $(compose_files_args)"
    if [[ -n "$DOCKER_PREFIX" ]]; then
        log_warn "Using sudo for docker compose (user not in docker group yet — log out/in to fix)"
    fi
    if [[ "$GPU_VENDOR" == "amd" ]]; then
        log_info "Using ROCm compose override (docker-compose.rocm.yml)"
    elif [[ -z "$GPU_VENDOR" ]]; then
        if [[ -e /dev/dri ]]; then
            log_info "No GPU vendor detected — using Vulkan overlay (docker-compose.vulkan.yml)"
        else
            log_warn "No GPU and no /dev/dri — using the CPU override (docker-compose.cpu.yml, lavapipe)."
            log_warn "Inference will be very slow."
        fi
    fi

    # Pull images first as a separate step so the user can see the layer-by-
    # layer download progress (5 images, ~3GB total on first run). Without
    # this split, `up -d` would silently pull during the up call and only
    # surface output if it fails. PC-052.
    log_info "Pulling images from GHCR (first run: ~3GB across 5 services)…"
    echo
    set +e
    $DC pull 2>&1 | tee "$ATLAS_RUN_DIR/atlas-compose-pull.log"
    local rc=${PIPESTATUS[0]}
    set -e
    echo
    if [[ $rc -ne 0 ]]; then
        log_err "docker compose pull failed (exit $rc). Log: $ATLAS_RUN_DIR/atlas-compose-pull.log"
        log_err "Common causes: GHCR rate-limit, network, or auth (private package)."
        die "Image pull failed — see live output above."
    fi
    log_ok "All images pulled."

    log_info "Starting containers…"
    echo
    set +e
    $DC up -d 2>&1 | tee "$ATLAS_RUN_DIR/atlas-compose.log"
    rc=${PIPESTATUS[0]}
    set -e
    echo
    if [[ $rc -ne 0 ]]; then
        log_err "docker compose up failed (exit $rc). Log: $ATLAS_RUN_DIR/atlas-compose.log"
        die "Compose start failed — see live output above."
    fi
    log_ok "Containers started"
}

# ---------------------------------------------------------------------------
# Step 7: Wait for healthy
# ---------------------------------------------------------------------------

wait_for_healthy() {
    log_step "Step 7: Waiting for services to be healthy"

    if [[ "${ATLAS_BOOTSTRAP_SKIP_COMPOSE:-0}" == "1" ]]; then
        log_skip "Skipped (compose was skipped)"
        return
    fi

    local DC
    DC="$DOCKER_PREFIX docker compose $(compose_files_args)"

    local services=(llama-server geometric-lens v3-service sandbox atlas-proxy)
    # 450s: must exceed the llama-server healthcheck budget in
    # docker-compose.yml (start_period 120s + 10 retries × 30s ≈ 420s),
    # otherwise the wait can give up while the container is still
    # legitimately warming up.
    local timeout=450
    local elapsed=0
    local interval=5

    log_info "Waiting up to ${timeout}s for all services to report healthy."
    log_info "Tip: open another terminal and run \`$DC logs -f llama-server\` to watch model load."
    echo

    local last_status=""
    while [[ $elapsed -lt $timeout ]]; do
        local healthy=0
        local total=${#services[@]}
        local status_line=""
        for s in "${services[@]}"; do
            local state
            state=$($DC ps --format '{{.Service}} {{.State}} {{.Health}}' 2>/dev/null \
                    | awk -v s="$s" '$1==s {print $2"/"$3; exit}')
            if [[ "$state" == running/healthy || "$state" == running/ ]]; then
                healthy=$((healthy + 1))
                status_line+="✓"
            elif [[ "$state" == running/starting ]]; then
                status_line+="⠿"
            elif [[ "$state" == running/unhealthy ]]; then
                status_line+="✗"
            else
                status_line+="·"
            fi
        done
        # Print status line on change OR every 30s, so the user sees life signs
        # without flooding the screen on every 5s tick.
        if [[ "$status_line" != "$last_status" || $((elapsed % 30)) -eq 0 ]]; then
            printf "    ${DIM}[%s] %d/%d healthy after %ds (services: %s)${NC}\n" \
                "$status_line" "$healthy" "$total" "$elapsed" "${services[*]}"
            last_status="$status_line"
        fi
        if [[ $healthy -eq $total ]]; then
            log_ok "All $total services healthy after ${elapsed}s"
            return 0
        fi
        sleep $interval
        elapsed=$((elapsed + interval))
    done

    echo
    log_err "Timeout: not all services healthy after ${timeout}s"
    log_err "Current state:"
    $DC ps 2>&1 | sed 's/^/      /' >&2
    log_err "Inspect a stuck service: $DC logs <service-name>"
    return 1
}

# ---------------------------------------------------------------------------
# Step 8: ASA steering vector (BiasBusters #4)
# ---------------------------------------------------------------------------
# Builds /models/ast_edit_steering.gguf so llama-server's
# entrypoint-v3.1.sh appends --control-vector-scaled on every start.
# Pipeline (per geometric-lens/asa_calibration/README.md):
#   1. build_cvector_prompts.py turns contrast_pairs.jsonl into
#      positive/negative .txt files
#   2. llama-cvector-generator (shipped in atlas-llama runtime image since
#      May 2026) extracts the residual-stream difference and writes the gguf
#   3. We stop llama-server briefly to free the GPU, run cvector-generator
#      in a one-shot container with a rw models mount, then restart
# No cross-model fallback is safe here. ASA vectors are residual-space
# artifacts tied to one model. If a registry entry publishes a compatible
# vector, `atlas model install-artifacts <name>` installs it earlier; otherwise
# this step trains one locally and degrades to no steering on failure.

build_asa_steering_vector() {
    log_step "Step 8: ASA steering vector (~5 min — BiasBusters #4)"

    if [[ "${ATLAS_BOOTSTRAP_SKIP_ASA:-0}" == "1" ]]; then
        log_skip "Skipped (ATLAS_BOOTSTRAP_SKIP_ASA=1)"
        return
    fi

    # This step reads ATLAS_* keys (models dir, model file/name, ports,
    # image tag) that live in the install's .env, not this process's
    # environment. Load them the same way scripts/download-models.sh
    # does: values already set in the environment win, .env fills gaps.
    local env_file="$ATLAS_INSTALL_DIR/.env"
    if [[ -f "$env_file" ]]; then
        local key value
        while IFS='=' read -r key value; do
            [[ "$key" =~ ^ATLAS_[A-Z0-9_]+$ ]] || continue
            # Strip surrounding quotes from value
            value="${value%\"}"; value="${value#\"}"
            value="${value%\'}"; value="${value#\'}"
            if [[ -z "${!key:-}" ]]; then
                export "$key=$value"
            fi
        done < <(grep -E '^[A-Z][A-Z0-9_]+=' "$env_file")
    fi

    # ASA extraction runs the model through llama-cvector-generator — a
    # GPU job. Dispatch per detected vendor; skip cleanly when there is
    # no GPU to run it on (CPU-only Vulkan hosts).
    local -a gpu_run_args=()
    local image_tag="${ATLAS_IMAGE_TAG:-latest}"
    local ghcr_owner="${ATLAS_GHCR_OWNER:-inferstep}"
    local image=""
    case "$GPU_VENDOR" in
        nvidia)
            image="ghcr.io/${ghcr_owner}/atlas-llama:${image_tag}"
            gpu_run_args=(--gpus all)
            ;;
        amd)
            image="ghcr.io/${ghcr_owner}/atlas-llama-rocm:${image_tag}"
            gpu_run_args=(--device=/dev/kfd --device=/dev/dri
                          --group-add video --group-add render)
            ;;
        *)
            log_skip "ASA build requires a GPU — skipping (build later with \`atlas asa build\`)"
            return
            ;;
    esac

    local models_dir_raw="${ATLAS_MODELS_DIR:-./models}"
    local models_dir
    if [[ "$models_dir_raw" = /* ]]; then
        models_dir="$models_dir_raw"
    else
        models_dir="$ATLAS_INSTALL_DIR/$models_dir_raw"
    fi
    models_dir="$(realpath "$models_dir" 2>/dev/null || echo "$models_dir")"
    local vector_path="$models_dir/ast_edit_steering.gguf"
    local vector_build_path="$models_dir/_ast_edit_steering.new.gguf"
    local quarantined_vector=""

    local asa_dir="$ATLAS_INSTALL_DIR/geometric-lens/asa_calibration"
    if [[ ! -f "$asa_dir/build_cvector_prompts.py" ]] || [[ ! -f "$asa_dir/generate_pairs.py" ]]; then
        log_warn "ASA calibration scripts missing at $asa_dir — steering remains disabled"
        return
    fi

    local DC
    DC="$DOCKER_PREFIX docker compose $(compose_files_args)"
    local model_file="${ATLAS_MODEL_FILE:-}"
    if [[ -z "$model_file" ]]; then
        log_warn "ATLAS_MODEL_FILE is unset — skipping ASA build until a model is selected"
        return
    fi
    if [[ -f "$vector_path" ]] && [[ -s "$vector_path" ]]; then
        if command -v atlas >/dev/null 2>&1 && \
           ATLAS_CONTROL_VECTOR="$vector_path" atlas asa check --no-color \
             >> "$ATLAS_RUN_DIR/atlas-asa-build.log" 2>&1; then
            log_ok "Compatible ASA steering vector already present ($(du -h "$vector_path" 2>/dev/null | cut -f1)) — skipping build"
            return
        fi
        log_warn "Existing ASA vector is unverified or incompatible with the selected model; rebuilding it"
        quarantined_vector="$vector_path.incompatible"
        mv -f "$vector_path" "$quarantined_vector"
        log_warn "Preserved the prior vector at $quarantined_vector (not auto-loaded)"
    fi
    # 1. Generate the ignored contrast-pair corpus on demand, then render it
    # with the loaded model's own chat template.
    if [[ ! -s "$asa_dir/contrast_pairs.jsonl" ]]; then
        log_info "Generating model-neutral ASA contrast pairs…"
        if ! run_as_target python3 "$asa_dir/generate_pairs.py" \
             --out "$asa_dir/contrast_pairs.jsonl" --n 1000 --seed 42 \
             >> "$ATLAS_RUN_DIR/atlas-asa-build.log" 2>&1; then
            log_warn "ASA contrast-pair generation failed — steering remains disabled"
            return
        fi
    fi
    log_info "Generating ASA prompt files from $asa_dir/contrast_pairs.jsonl…"
    set +e
    run_as_target python3 "$asa_dir/build_cvector_prompts.py" \
        --pairs "$asa_dir/contrast_pairs.jsonl" \
        --positive "$models_dir/_asa_positive.txt" \
        --negative "$models_dir/_asa_negative.txt" \
        --llama-url "http://localhost:${ATLAS_LLAMA_PORT:-8080}" \
        > "$ATLAS_RUN_DIR/atlas-asa-build.log" 2>&1
    local rc=$?
    set -e
    if [[ $rc -ne 0 ]] || [[ ! -s "$models_dir/_asa_positive.txt" ]]; then
        log_warn "Prompt file generation failed (exit $rc) — steering remains disabled"
        rm -f "$models_dir/_asa_positive.txt" "$models_dir/_asa_negative.txt"
        return
    fi

    # 2. Stop llama-server so the GPU is free for the cvector loader.
    log_info "Pausing llama-server briefly to free the GPU…"
    $DC stop llama-server >> "$ATLAS_RUN_DIR/atlas-asa-build.log" 2>&1 || true

    # 3. Run cvector-generator as a one-shot container with a rw models
    #    mount (the compose mount is :ro on purpose).
    log_info "Running llama-cvector-generator — this is the slow part (~5 min)…"
    echo
    set +e
    $DOCKER_PREFIX docker run --rm "${gpu_run_args[@]}" \
        -v "$models_dir:/models:rw" \
        --entrypoint llama-cvector-generator \
        "$image" \
        -m "/models/$model_file" \
        --positive-file /models/_asa_positive.txt \
        --negative-file /models/_asa_negative.txt \
        --method mean \
        -o /models/_ast_edit_steering.new.gguf \
        -ngl 99 2>&1 | tee -a "$ATLAS_RUN_DIR/atlas-asa-build.log"
    rc=${PIPESTATUS[0]}
    set -e
    echo

    # 4. Always restart llama-server, regardless of build outcome.
    log_info "Restarting llama-server…"
    $DC start llama-server >> "$ATLAS_RUN_DIR/atlas-asa-build.log" 2>&1 || \
        log_warn "llama-server restart returned non-zero — check 'docker compose ps'"

    # 5. Cleanup intermediate prompt files.
    rm -f "$models_dir/_asa_positive.txt" "$models_dir/_asa_negative.txt"

    if [[ $rc -eq 0 ]] && [[ -s "$vector_build_path" ]]; then
        mv -f "$vector_build_path" "$vector_path"
        printf '%s\n' "${ATLAS_MODEL_NAME:-${model_file%.gguf}}" > "$vector_path.model"
        # Fix ownership (cvector-generator runs as root inside the container).
        if [[ "$(id -u)" == "0" && "$TARGET_USER" != "root" ]]; then
            $SUDO chown "$TARGET_USER:$TARGET_USER" "$vector_path" 2>/dev/null || true
        fi
        log_ok "ASA steering vector built ($(du -h "$vector_path" | cut -f1)): $vector_path"
        log_info "  Auto-activates on the next llama-server start via the entrypoint check."
        # Bounce llama-server one more time so it picks up the new vector.
        $DC restart llama-server >> "$ATLAS_RUN_DIR/atlas-asa-build.log" 2>&1 || true
    else
        rm -f "$vector_build_path"
        log_warn "Local ASA build failed (exit $rc). ATLAS will run without steering rather than applying a vector trained for another model. Recovery: geometric-lens/asa_calibration/README.md or rerun bootstrap. Log: $ATLAS_RUN_DIR/atlas-asa-build.log. Suppress with ATLAS_BOOTSTRAP_SKIP_ASA=1.${quarantined_vector:+ Prior vector preserved at $quarantined_vector.}"
    fi
}

# ---------------------------------------------------------------------------
# Step 9: doctor (sanity sweep)
# ---------------------------------------------------------------------------

run_doctor() {
    log_step "Step 9: atlas doctor (sanity sweep)"

    if [[ "${ATLAS_BOOTSTRAP_SKIP_COMPOSE:-0}" == "1" ]]; then
        log_skip "Skipped (compose was skipped, no stack to check)"
        return
    fi

    if ! command -v python3 &>/dev/null; then
        log_warn "python3 not found — skipping doctor (doctor is Python)"
        return
    fi

    # Doctor lives in the repo. cd there, run --quick, capture exit code.
    # ATLAS_INSTALL_DIR is set by ensure_repo_and_env; fall back to pwd
    # in the unlikely case it wasn't (e.g. compose was skipped earlier).
    local install_dir="${ATLAS_INSTALL_DIR:-$(pwd)}"
    local doctor_out doctor_rc
    set +e
    doctor_out=$(cd "$install_dir" && python3 -m atlas.commands.doctor --quick --no-color 2>&1)
    doctor_rc=$?
    set -e

    if [[ $doctor_rc -ne 0 ]]; then
        log_err "atlas doctor reported failures:"
        echo "$doctor_out" | grep -E "FAIL|WARN" | sed 's/^/      /'
        log_info "Run \`atlas doctor -v\` after install completes for detail."
        return  # Don't block bootstrap; failures are install-time signals
    fi

    # Exit 0 may still include warnings — surface them inline so users
    # don't miss degraded-but-running states.
    local warn_lines
    warn_lines=$(echo "$doctor_out" | grep "WARN" || true)
    if [[ -n "$warn_lines" ]]; then
        log_warn "atlas doctor passed with warnings:"
        echo "$warn_lines" | sed 's/^/      /'
        log_info "Run \`atlas doctor -v\` for the recommended fix."
    else
        log_ok "atlas doctor passed (run \`atlas doctor\` for full check)"
    fi
}

# ---------------------------------------------------------------------------
# Step 8: Ready banner
# ---------------------------------------------------------------------------

print_ready_banner() {
    echo
    echo -e "${GREEN}${BOLD}╭─────────────────────────────────────────────╮${NC}"
    echo -e "${GREEN}${BOLD}│${NC}  ${BOLD}ATLAS is ready.${NC}                            ${GREEN}${BOLD}│${NC}"
    echo -e "${GREEN}${BOLD}╰─────────────────────────────────────────────╯${NC}"
    echo
    echo -e "  ${BOLD}Quick start${NC}"
    echo -e "    ${DIM}# In any project directory you want to code in:${NC}"
    echo -e "    ${CYAN}cd /path/to/your/project${NC}"
    echo -e "    ${CYAN}atlas${NC}                   ${DIM}# launches the TUI chat UI${NC}"
    echo
    echo -e "  ${BOLD}Diagnostics${NC}"
    echo -e "    ${CYAN}atlas doctor${NC}            ${DIM}# verify all services are healthy${NC}"
    echo -e "    ${CYAN}docker compose ps${NC}       ${DIM}# raw container status${NC}"
    echo -e "    ${CYAN}docker compose logs -f${NC}  ${DIM}# stream logs across all services${NC}"
    echo
    echo -e "  ${BOLD}Docs${NC}: https://github.com/inferstep/ATLAS/tree/main/docs"
    echo -e "  ${BOLD}Issues${NC}: https://github.com/inferstep/ATLAS/issues"
    echo
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    echo
    echo -e "${BOLD}ATLAS bootstrap${NC} — installing on $(uname -s) $(uname -r)"
    echo -e "${DIM}Started at $(date)${NC}"
    if [[ "$(id -u)" == "0" ]]; then
        if [[ "$TARGET_USER" == "root" ]]; then
            echo -e "${DIM}Running as: root (no SUDO_USER detected — install will be root-owned)${NC}"
        else
            echo -e "${DIM}Running as: root (via sudo from $TARGET_USER — install owned by $TARGET_USER)${NC}"
        fi
    else
        echo -e "${DIM}Running as: $TARGET_USER (will sudo as needed)${NC}"
    fi
    echo

    print_supported_distros
    echo

    log_step "Detecting system"
    detect_distro
    detect_gpu
    echo
    log_info "Install location: ${BOLD}${ATLAS_INSTALL_DIR:-/opt/atlas}${NC} (override with ATLAS_INSTALL_DIR=...)"
    log_info "  Why /opt/atlas? It's the standard prefix for system-wide third-party"
    log_info "  software (FHS), survives \$HOME purges, and lets multiple users on the"
    log_info "  same box share one install. Set ATLAS_INSTALL_DIR=\$HOME/atlas if you'd"
    log_info "  rather it land in your home dir."
    echo

    install_docker
    echo
    # After Docker is installed (or confirmed present), pin down whether
    # subsequent `docker` calls in this script need sudo. The user may have
    # been added to the docker group in install_docker but their CURRENT
    # shell doesn't see that yet — this prefix is what makes the rest of
    # the script work without "permission denied on /var/run/docker.sock".
    detect_docker_prefix
    echo
    # V3.1.1: dispatch on detected GPU vendor — NVIDIA hits the original
    # nvidia-container-toolkit path; AMD hits the new ROCm path
    # (rocm-smi verify + group setup, no separate container runtime).
    install_gpu_runtime
    echo
    configure_rhel_extras
    echo
    ensure_repo_and_env
    echo
    download_models
    echo
    start_compose
    echo
    wait_for_healthy || die "Service health check failed."
    echo
    build_asa_steering_vector
    echo
    run_doctor

    print_ready_banner
}

main "$@"
