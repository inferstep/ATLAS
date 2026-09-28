#!/bin/bash
set -euo pipefail

# ATLAS Uninstaller
# Removes ATLAS services (optionally K3s and models)

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/config.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }

REMOVE_K3S=false
REMOVE_MODELS=false
REMOVE_DATA=false

usage() {
    echo "ATLAS Uninstaller"
    echo ""
    echo "Usage: $0 [OPTIONS]"
    echo ""
    echo "Options:"
    echo "  --all          Remove everything (K3s, models, data)"
    echo "  --k3s          Also remove K3s"
    echo "  --models       Also remove downloaded models"
    echo "  --data         Also remove persistent data (PVCs)"
    echo "  -h, --help     Show this help"
    echo ""
    echo "Configuration:"
    echo "  Models dir:  $ATLAS_MODELS_DIR"
    echo "  Data dir:    $ATLAS_DATA_DIR"
    echo "  Namespace:   $ATLAS_NAMESPACE"
    echo ""
}

parse_args() {
    while [[ $# -gt 0 ]]; do
        case $1 in
            --all)
                REMOVE_K3S=true
                REMOVE_MODELS=true
                REMOVE_DATA=true
                ;;
            --k3s)
                REMOVE_K3S=true
                ;;
            --models)
                REMOVE_MODELS=true
                ;;
            --data)
                REMOVE_DATA=true
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                log_warn "Unknown option: $1"
                ;;
        esac
        shift
    done
}

confirm() {
    local msg="$1"
    read -p "$msg [y/N] " -n 1 -r
    echo
    [[ $REPLY =~ ^[Yy]$ ]]
}

remove_atlas_services() {
    log_info "Removing ATLAS services..."

    # Delete everything install.sh applied. generate-manifests.sh renders
    # templates/*.yaml.tmpl into this directory.
    kubectl delete -f "$K8S_DIR/manifests/" -n "$ATLAS_NAMESPACE" 2>/dev/null || true

    # Delete any remaining deployments by label. One entry per app label in
    # templates/*.yaml.tmpl — keep this list in step with that directory.
    for app in llama-server geometric-lens atlas-proxy v3-service sandbox; do
        kubectl delete deployment -n "$ATLAS_NAMESPACE" -l "app=$app" \
            2>/dev/null || true
    done

    # Delete secrets
    kubectl delete secret -n "$ATLAS_NAMESPACE" atlas-secrets 2>/dev/null || true

    if [[ "$REMOVE_DATA" == true ]]; then
        log_info "Removing persistent volume claims..."
        # lens-state is only left by installs from before the pattern cache
        # and its state store were removed.
        kubectl delete pvc -n "$ATLAS_NAMESPACE" lens-state 2>/dev/null || true
        kubectl delete pvc -n "$ATLAS_NAMESPACE" lens-projects 2>/dev/null || true
    fi

    # Delete namespace if not default
    if [[ "$ATLAS_NAMESPACE" != "default" ]]; then
        kubectl delete namespace "$ATLAS_NAMESPACE" 2>/dev/null || true
    fi

    log_info "ATLAS services removed"
}

remove_container_images() {
    log_info "Removing container images..."

    local prefix="ghcr.io/${ATLAS_GHCR_OWNER:-inferstep}"
    local tag="${ATLAS_IMAGE_TAG:-latest}"
    for img in atlas-llama atlas-llama-vulkan atlas-lens atlas-proxy atlas-sandbox atlas-v3; do
        k3s ctr images rm "${prefix}/${img}:${tag}" 2>/dev/null || true
    done

    log_info "Container images removed"
}

remove_gpu_operator() {
    log_info "Removing GPU Operator..."

    helm uninstall gpu-operator -n gpu-operator 2>/dev/null || true
    kubectl delete namespace gpu-operator 2>/dev/null || true

    log_info "GPU Operator removed"
}

remove_k3s() {
    log_info "Removing K3s..."

    if [[ -f /usr/local/bin/k3s-uninstall.sh ]]; then
        /usr/local/bin/k3s-uninstall.sh
    else
        log_warn "K3s uninstall script not found"
    fi

    log_info "K3s removed"
}

remove_models() {
    log_info "Removing models from $ATLAS_MODELS_DIR..."

    rm -f "$ATLAS_MODELS_DIR"/*.gguf
    rm -f "$ATLAS_MODELS_DIR/default.gguf"

    log_info "Models removed"
}

remove_data() {
    log_info "Removing data from $ATLAS_DATA_DIR..."

    rm -rf "$ATLAS_DATA_DIR"
    rm -rf "$ATLAS_PROJECTS_DIR"

    log_info "Data removed"
}

main() {
    echo "=========================================="
    echo "  ATLAS Uninstaller"
    echo "=========================================="
    echo ""

    parse_args "$@"

    echo "This will remove:"
    echo "  - ATLAS services and deployments"
    echo "  - Container images"
    [[ "$REMOVE_DATA" == true ]] && echo "  - Persistent data (PVCs and $ATLAS_DATA_DIR)"
    [[ "$REMOVE_MODELS" == true ]] && echo "  - Downloaded models ($ATLAS_MODELS_DIR)"
    [[ "$REMOVE_K3S" == true ]] && echo "  - K3s cluster"
    [[ "$REMOVE_K3S" == true ]] && echo "  - GPU Operator"
    echo ""

    if ! confirm "Are you sure you want to continue?"; then
        echo "Aborted."
        exit 0
    fi

    remove_atlas_services
    remove_container_images

    if [[ "$REMOVE_K3S" == true ]]; then
        remove_gpu_operator
        remove_k3s
    fi

    if [[ "$REMOVE_MODELS" == true ]]; then
        remove_models
    fi

    if [[ "$REMOVE_DATA" == true ]]; then
        remove_data
    fi

    echo ""
    echo "=========================================="
    echo "  Uninstall Complete!"
    echo "=========================================="
    echo ""

    if [[ "$REMOVE_K3S" == false ]]; then
        echo "Note: K3s is still installed. Run with --k3s to remove."
    fi

    if [[ "$REMOVE_MODELS" == false ]]; then
        echo "Note: Models are still on disk. Run with --models to remove."
    fi
}

main "$@"
