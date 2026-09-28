#!/bin/bash
set -euo pipefail

# ATLAS Container Builder
# Builds all container images and imports to K3s
# Note: Importing to K3s requires sudo (will prompt if not root)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
source "$SCRIPT_DIR/lib/config.sh"

# Images are tagged exactly as the K3s manifests reference them
# (templates/*-deployment.yaml.tmpl pull ghcr.io/${ATLAS_GHCR_OWNER}/...),
# so a side-loaded local build is picked up without editing manifests.
IMAGE_PREFIX="ghcr.io/${ATLAS_GHCR_OWNER:-inferstep}"
IMAGE_TAG="${ATLAS_IMAGE_TAG:-latest}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Detect container runtime
detect_runtime() {
    if command -v podman &> /dev/null; then
        echo "podman"
    elif command -v docker &> /dev/null; then
        echo "docker"
    else
        log_error "No container runtime found. Install podman or docker."
        exit 1
    fi
}

build_image() {
    local name="$1"
    local context="$2"
    local dockerfile="$3"
    local runtime="$4"

    log_info "Building $name..."

    if [[ ! -f "$REPO_ROOT/$dockerfile" ]]; then
        log_error "Dockerfile not found: $dockerfile (broken checkout?)"
        exit 1
    fi

    $runtime build -t "${IMAGE_PREFIX}/$name:${IMAGE_TAG}" \
        -f "$REPO_ROOT/$dockerfile" "$REPO_ROOT/$context"

    log_info "$name built successfully"
}

import_to_k3s() {
    local name="$1"
    local runtime="$2"

    log_info "Importing $name to K3s..."

    # Check if image exists
    if ! $runtime image inspect "${IMAGE_PREFIX}/$name:${IMAGE_TAG}" >/dev/null 2>&1; then
        log_warn "Image ${IMAGE_PREFIX}/$name:${IMAGE_TAG} not found, skipping import"
        return 0
    fi

    # K3s containerd socket requires root access
    # Use full path since sudo doesn't inherit PATH
    if [[ $EUID -eq 0 ]]; then
        $runtime save "${IMAGE_PREFIX}/$name:${IMAGE_TAG}" | /usr/local/bin/k3s ctr images import -
    else
        $runtime save "${IMAGE_PREFIX}/$name:${IMAGE_TAG}" | sudo /usr/local/bin/k3s ctr images import -
    fi

    log_info "$name imported to K3s"
}

main() {
    echo "=========================================="
    echo "  ATLAS Container Builder"
    echo "=========================================="
    echo ""
    echo "Configuration:"
    echo "  Prefix:      $IMAGE_PREFIX"
    echo "  Image tag:   $IMAGE_TAG"
    echo ""

    RUNTIME=$(detect_runtime)
    log_info "Using container runtime: $RUNTIME"

    # name|build-context|dockerfile — matches the compose/build-images.yml
    # matrix and the K3s manifest image names. v3 uses the repo root as
    # context because v3-service/Dockerfile reads sibling dirs at build time.
    declare -a IMAGES=(
        "atlas-llama|inference|inference/Dockerfile.v31"
        "atlas-lens|geometric-lens|geometric-lens/Dockerfile"
        "atlas-proxy|proxy|proxy/Dockerfile"
        "atlas-sandbox|sandbox|sandbox/Dockerfile"
        "atlas-v3|.|v3-service/Dockerfile"
    )

    echo ""
    echo "Building service images..."
    for entry in "${IMAGES[@]}"; do
        IFS="|" read -r name context dockerfile <<< "$entry"
        build_image "$name" "$context" "$dockerfile" "$RUNTIME"
    done

    # Import to K3s
    echo ""
    echo "Importing to K3s..."
    if [[ $EUID -ne 0 ]]; then
        log_warn "K3s import requires sudo - you may be prompted for your password"
    fi
    for entry in "${IMAGES[@]}"; do
        IFS="|" read -r name _ _ <<< "$entry"
        import_to_k3s "$name" "$RUNTIME"
    done

    echo ""
    echo "=========================================="
    echo "  Build Complete!"
    echo "=========================================="
    echo ""
    echo "Images built and imported:"
    if [[ $EUID -eq 0 ]]; then
        /usr/local/bin/k3s ctr images list 2>/dev/null | grep "$IMAGE_PREFIX" || echo "  (use 'sudo k3s ctr images list' to verify)"
    else
        sudo /usr/local/bin/k3s ctr images list 2>/dev/null | grep "$IMAGE_PREFIX" || echo "  (use 'sudo k3s ctr images list' to verify)"
    fi
    echo ""
}

main "$@"
