#!/bin/bash
# ATLAS Config Loader
# Source this in scripts: source "$SCRIPT_DIR/lib/config.sh"

# Ensure common paths are in PATH (K3s installs to /usr/local/bin)
export PATH="/usr/local/bin:$PATH"

# Get paths relative to this library file
_LIB_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
_SCRIPTS_DIR="$(dirname "$_LIB_DIR")"
K8S_DIR="$(dirname "$_SCRIPTS_DIR")"

# Colors for output (if not already defined)
RED="${RED:-\033[0;31m}"
GREEN="${GREEN:-\033[0;32m}"
YELLOW="${YELLOW:-\033[1;33m}"
NC="${NC:-\033[0m}"

# Setup kubeconfig - try multiple locations
# Checks that files are both present AND readable by current user
setup_kubeconfig() {
    # If KUBECONFIG is already set, valid, AND readable, use it
    if [[ -n "${KUBECONFIG:-}" ]] && [[ -f "$KUBECONFIG" ]] && [[ -r "$KUBECONFIG" ]]; then
        export KUBECONFIG
        return 0
    fi

    # Try config value from atlas.conf (if already loaded) - must be readable
    # Skip if set to "auto" (meaning use auto-detection)
    if [[ -n "${ATLAS_KUBECONFIG:-}" ]] && [[ "$ATLAS_KUBECONFIG" != "auto" ]] && [[ -f "$ATLAS_KUBECONFIG" ]] && [[ -r "$ATLAS_KUBECONFIG" ]]; then
        export KUBECONFIG="$ATLAS_KUBECONFIG"
        return 0
    fi

    # Try common locations in order of preference
    # User's config first (most likely to be readable), then system locations
    local locations=(
        "$HOME/.kube/config"
        "/etc/rancher/k3s/k3s.yaml"
        "/etc/kubernetes/admin.conf"
    )

    for loc in "${locations[@]}"; do
        if [[ -f "$loc" ]] && [[ -r "$loc" ]]; then
            export KUBECONFIG="$loc"
            return 0
        fi
    done

    # No readable kubeconfig found
    return 1
}

# Load config file
load_config() {
    local config_file="${ATLAS_CONFIG_FILE:-$K8S_DIR/atlas.conf}"

    if [[ ! -f "$config_file" ]]; then
        # Try example file as fallback
        if [[ -f "$K8S_DIR/atlas.conf.example" ]]; then
            echo -e "${YELLOW}[WARN]${NC} atlas.conf not found, using atlas.conf.example defaults"
            config_file="$K8S_DIR/atlas.conf.example"
        else
            echo -e "${RED}[ERROR]${NC} No configuration file found"
            echo "Create one with: cp atlas.conf.example atlas.conf"
            exit 1
        fi
    fi

    # Source the config
    source "$config_file"

    # Handle auto-detection for node IP
    if [[ "${ATLAS_NODE_IP:-auto}" == "auto" ]]; then
        ATLAS_NODE_IP=$(ip -4 addr show scope global 2>/dev/null | grep -oP '(?<=inet )\d+\.\d+\.\d+\.\d+' | head -1)
        # Fallback if ip command unavailable
        [[ -z "$ATLAS_NODE_IP" ]] && ATLAS_NODE_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
        [[ -z "$ATLAS_NODE_IP" ]] && ATLAS_NODE_IP=$(hostname -i 2>/dev/null | awk '{print $1}')
    fi

    # Export all ATLAS_ variables
    export "${!ATLAS_@}"

    # Also export K8S_DIR for scripts
    export K8S_DIR

    # Try to setup kubeconfig (non-fatal if not found)
    setup_kubeconfig || true
}

# Validate required config
validate_config() {
    local errors=0

    # Check port conflicts (NodePorts must be unique)
    local ports=(
        "$ATLAS_PROXY_NODEPORT"
        "$ATLAS_LENS_NODEPORT"
        "$ATLAS_LLAMA_NODEPORT"
        "$ATLAS_SANDBOX_NODEPORT"
        "$ATLAS_V3_NODEPORT"
    )

    local seen=()
    for port in "${ports[@]}"; do
        if [[ " ${seen[*]} " =~ " ${port} " ]]; then
            echo -e "${RED}[ERROR]${NC} Duplicate NodePort: $port"
            errors=$((errors + 1))
        fi
        seen+=("$port")
    done

    # Validate NodePort range (30000-32767)
    for port in "${ports[@]}"; do
        if [[ $port -lt 30000 ]] || [[ $port -gt 32767 ]]; then
            echo -e "${RED}[ERROR]${NC} NodePort $port out of range (30000-32767)"
            errors=$((errors + 1))
        fi
    done

    return $errors
}

# Print current config (for debugging)
print_config() {
    echo "ATLAS Configuration:"
    echo "===================="
    env | grep "^ATLAS_" | sort
}

# Get config value with default
get_config() {
    local key="$1"
    local default="${2:-}"
    local var="ATLAS_$key"
    echo "${!var:-$default}"
}

# Auto-load config when sourced
load_config

# GHCR namespace for the service images (templates/*.yaml.tmpl render
# ghcr.io/${ATLAS_GHCR_OWNER}/...). Default: upstream-published images.
ATLAS_GHCR_OWNER="${ATLAS_GHCR_OWNER:-inferstep}"
export ATLAS_GHCR_OWNER

# Proxy runtime uid/gid (atlas-proxy template securityContext). The proxy
# writes the /workspace hostPath; it must run as the owner of that dir.
# Default: the invoking user, matching `atlas init`'s ATLAS_PROXY_UID
# behavior on the compose path.
ATLAS_PROXY_UID="${ATLAS_PROXY_UID:-$(id -u 2>/dev/null || echo 1000)}"
ATLAS_PROXY_GID="${ATLAS_PROXY_GID:-$(id -g 2>/dev/null || echo 1000)}"
export ATLAS_PROXY_UID ATLAS_PROXY_GID
