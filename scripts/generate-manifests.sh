#!/bin/bash
set -euo pipefail

# ATLAS Manifest Generator
# Generates Kubernetes manifests from templates using config values

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/config.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

TEMPLATES_DIR="$K8S_DIR/templates"
MANIFESTS_DIR="$K8S_DIR/manifests"

main() {
    echo "=========================================="
    echo "  ATLAS Manifest Generator"
    echo "=========================================="
    echo ""
    echo "Configuration:"
    echo "  Templates:   $TEMPLATES_DIR"
    echo "  Output:      $MANIFESTS_DIR"
    echo "  Namespace:   $ATLAS_NAMESPACE"
    echo ""

    # Validate config
    if ! validate_config; then
        log_error "Configuration validation failed"
        exit 1
    fi

    # Check templates directory exists
    if [[ ! -d "$TEMPLATES_DIR" ]]; then
        log_warn "Templates directory not found: $TEMPLATES_DIR"
        log_info "Using existing manifests without templating"
        exit 0
    fi

    # Create manifests directory
    mkdir -p "$MANIFESTS_DIR"

    # Count templates
    local tmpl_count
    tmpl_count=$(find "$TEMPLATES_DIR" -name "*.yaml.tmpl" 2>/dev/null | wc -l) || true
    if [[ $tmpl_count -eq 0 ]]; then
        log_warn "No templates found in $TEMPLATES_DIR"
        log_info "Using existing manifests without templating"
        exit 0
    fi

    log_info "Found $tmpl_count template(s)"

    # Generate each manifest - ALL go to single directory
    for tmpl in "$TEMPLATES_DIR"/*.yaml.tmpl; do
        if [[ -f "$tmpl" ]]; then
            filename=$(basename "$tmpl" .tmpl)
            log_info "Generating $filename -> $MANIFESTS_DIR/$filename"
            envsubst < "$tmpl" > "$MANIFESTS_DIR/$filename"
        fi
    done

    echo ""
    log_info "Manifests generated successfully"
    echo ""
    echo "Generated manifests:"
    ls -la "$MANIFESTS_DIR"/*.yaml 2>/dev/null || echo "  (none)"
    echo ""
    echo "To deploy: kubectl apply -f $MANIFESTS_DIR/"
}

main "$@"
