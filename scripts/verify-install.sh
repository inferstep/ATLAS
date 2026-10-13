#!/bin/bash
set -euo pipefail

# ATLAS Installation Verifier
# Checks all services are running and healthy

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/config.sh"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

PASSED=0
FAILED=0
WARNINGS=0

# Note: setup_kubeconfig is defined in lib/config.sh and called during load_config

check_pass() {
    echo -e "  ${GREEN}✓${NC} $1"
    PASSED=$((PASSED + 1))
}

check_fail() {
    echo -e "  ${RED}✗${NC} $1"
    FAILED=$((FAILED + 1))
}

check_warn() {
    echo -e "  ${YELLOW}!${NC} $1"
    WARNINGS=$((WARNINGS + 1))
}

# Service health check
check_service() {
    local name="$1"
    local url="$2"
    local timeout="${3:-$ATLAS_HEALTH_CHECK_TIMEOUT}"

    if curl -sf --max-time "$timeout" "$url" > /dev/null 2>&1; then
        check_pass "$name is healthy"
    else
        check_fail "$name is not responding at $url"
    fi
    return 0
}

# Pod status check
check_pod() {
    local name="$1"

    local status
    status=$(kubectl get pods -n "$ATLAS_NAMESPACE" -l app="$name" -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "")

    if [[ "$status" == "Running" ]]; then
        check_pass "$name pod is Running"
    else
        check_fail "$name pod status: ${status:-NotFound}"
    fi
    return 0
}

# GPU check. ATLAS ships four backends (cuda, rocm, vulkan, metal) and
# `atlas tier` detects NVIDIA, AMD, and Apple GPUs, so an NVIDIA-only probe
# used to hard-fail a perfectly good AMD, Vulkan, or CPU-only install.
check_gpu() {
    local found=false

    if nvidia-smi > /dev/null 2>&1; then
        found=true
        check_pass "NVIDIA GPU: $(nvidia-smi --query-gpu=name,memory.total \
            --format=csv,noheader | head -1)"
        if kubectl get nodes -o json | grep -q "nvidia.com/gpu"; then
            check_pass "GPU advertised in Kubernetes (nvidia.com/gpu)"
        else
            check_warn "GPU not advertised in Kubernetes (may still work)"
        fi
    fi

    if rocm-smi --showproductname > /dev/null 2>&1; then
        found=true
        check_pass "AMD GPU detected (rocm-smi)"
        if kubectl get nodes -o json | grep -q "amd.com/gpu"; then
            check_pass "GPU advertised in Kubernetes (amd.com/gpu)"
        else
            check_warn "GPU not advertised in Kubernetes (may still work)"
        fi
    fi

    if [[ "$(uname -s)" == "Darwin" ]] \
        && system_profiler SPDisplaysDataType > /dev/null 2>&1; then
        found=true
        check_pass "Apple GPU detected (Metal hybrid path)"
    fi

    if [[ "$found" == false ]]; then
        # No vendor CLI. Vulkan (lavapipe) and CPU-only are supported
        # deployments, so this is a warning, not a failure.
        check_warn "No GPU detected — CPU/Vulkan fallback only (inference will be slow)"
    fi
}

# Model check
check_models() {
    if [[ -f "$ATLAS_MODELS_DIR/default.gguf" ]] || [[ -f "$ATLAS_MODELS_DIR/$ATLAS_MAIN_MODEL" ]]; then
        check_pass "Main model found"
    else
        check_fail "Main model not found in $ATLAS_MODELS_DIR"
    fi

    if [[ "$ATLAS_ENABLE_SPECULATIVE" == "true" ]] && [[ -n "$ATLAS_DRAFT_MODEL" ]]; then
        if [[ -f "$ATLAS_MODELS_DIR/$ATLAS_DRAFT_MODEL" ]]; then
            check_pass "Draft model found (speculative decoding enabled)"
        else
            check_warn "Draft model not found (speculative decoding disabled)"
        fi
    fi
}

# LLM inference test
check_llm_inference() {
    local response
    response=$(curl -sf --max-time "$ATLAS_LLM_TIMEOUT" \
        -X POST "http://localhost:${ATLAS_LLAMA_NODEPORT}/v1/chat/completions" \
        -H "Content-Type: application/json" \
        -d '{"messages":[{"role":"user","content":"Say hello"}],"max_tokens":10}' 2>/dev/null) || true

    if echo "$response" | grep -q "choices"; then
        check_pass "LLM inference working"
    else
        check_fail "LLM inference failed"
    fi
}

# Full E2E test — drive an actual agent turn through atlas-proxy.
# Replaces the legacy api-portal user-registration probe (api-portal was
# removed; no auth surface anymore — the proxy is the entry point).
check_e2e() {
    local response
    response=$(curl -sf --max-time 30 \
        -X POST "http://localhost:${ATLAS_PROXY_NODEPORT}/v1/agent" \
        -H "Content-Type: application/json" \
        -d '{"message":"echo hello","mode":"yolo","session_id":"verify-install"}' 2>/dev/null || echo "")

    if [[ -n "$response" ]] && echo "$response" | grep -q "data:"; then
        check_pass "atlas-proxy /v1/agent reachable + streaming SSE"
    else
        check_warn "atlas-proxy /v1/agent did not return an SSE stream (may need manual test)"
    fi
}

main() {
    echo "=========================================="
    echo "  ATLAS Installation Verifier"
    echo "=========================================="
    echo ""
    echo "Configuration:"
    echo "  Namespace:   $ATLAS_NAMESPACE"
    echo "  Models dir:  $ATLAS_MODELS_DIR"
    echo ""

    # Check kubeconfig
    echo "Kubernetes Cluster:"
    if [[ -z "${KUBECONFIG:-}" ]]; then
        check_fail "No kubeconfig found"
        echo ""
        echo -e "${YELLOW}NOTE:${NC} Could not find a kubeconfig file."
        echo ""
        echo "Tried locations:"
        echo "  - \$KUBECONFIG environment variable"
        echo "  - \$ATLAS_KUBECONFIG from atlas.conf (${ATLAS_KUBECONFIG:-not set})"
        echo "  - ~/.kube/config"
        echo "  - /etc/rancher/k3s/k3s.yaml"
        echo ""
        echo "Solutions:"
        echo "  1. Set KUBECONFIG: export KUBECONFIG=~/.kube/config"
        echo "  2. Copy K3s config: sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config"
        echo "                      sudo chown \$(id -u):\$(id -g) ~/.kube/config"
        echo "  3. Set ATLAS_KUBECONFIG in atlas.conf"
        echo "  4. Run as root: sudo ./scripts/verify-install.sh"
        echo ""
        exit 1
    fi

    # Show which kubeconfig we're using
    check_pass "Using kubeconfig: $KUBECONFIG"

    # Try to connect
    if kubectl cluster-info > /dev/null 2>&1; then
        check_pass "Cluster is accessible"
    else
        check_fail "Cannot connect to cluster"
        echo ""
        echo -e "${YELLOW}NOTE:${NC} Found kubeconfig at $KUBECONFIG but cannot connect."
        echo ""
        echo "Possible causes:"
        echo "  - K3s/Kubernetes is not running: sudo systemctl status k3s"
        echo "  - Kubeconfig has wrong permissions: ls -la $KUBECONFIG"
        echo "  - Cluster address is incorrect in kubeconfig"
        echo ""
        echo "Try:"
        echo "  - Check K3s status: sudo systemctl status k3s"
        echo "  - Test manually: kubectl get nodes"
        echo "  - Check kubeconfig: cat $KUBECONFIG | head -20"
        echo ""
        exit 1
    fi

    # GPU checks
    echo ""
    echo "GPU:"
    check_gpu

    # Model checks
    echo ""
    echo "Models:"
    check_models

    # Pod status - using actual app labels from manifests
    echo ""
    echo "Pod Status:"
    check_pod "llama-server"
    check_pod "geometric-lens"
    check_pod "atlas-proxy"
    check_pod "v3-service"
    check_pod "sandbox"

    # Service health endpoints - using NodePort values from config
    echo ""
    echo "Service Health:"
    check_service "llama-server"   "http://localhost:${ATLAS_LLAMA_NODEPORT}/health"  "$ATLAS_HEALTH_CHECK_TIMEOUT"
    check_service "geometric-lens" "http://localhost:${ATLAS_LENS_NODEPORT}/health"   "$ATLAS_HEALTH_CHECK_TIMEOUT"
    check_service "atlas-proxy"    "http://localhost:${ATLAS_PROXY_NODEPORT}/health"  "$ATLAS_HEALTH_CHECK_TIMEOUT"
    check_service "v3-service"     "http://localhost:${ATLAS_V3_NODEPORT}/health"     "$ATLAS_HEALTH_CHECK_TIMEOUT"
    check_service "sandbox"        "http://localhost:${ATLAS_SANDBOX_NODEPORT}/health" "$ATLAS_HEALTH_CHECK_TIMEOUT"

    # Functional checks
    echo ""
    echo "Functional Tests:"
    check_llm_inference
    check_e2e

    # Summary
    echo ""
    echo "=========================================="
    echo "  Verification Summary"
    echo "=========================================="
    echo ""
    echo -e "  ${GREEN}Passed:${NC}   $PASSED"
    echo -e "  ${RED}Failed:${NC}   $FAILED"
    echo -e "  ${YELLOW}Warnings:${NC} $WARNINGS"
    echo ""

    if [[ $FAILED -gt 0 ]]; then
        echo -e "${RED}VERIFICATION FAILED${NC}"
        echo "Check failed items above and review logs:"
        echo "  kubectl logs -n $ATLAS_NAMESPACE -l app=<service-name>"
        exit 1
    elif [[ $WARNINGS -gt 0 ]]; then
        echo -e "${YELLOW}VERIFICATION PASSED WITH WARNINGS${NC}"
        exit 0
    else
        echo -e "${GREEN}VERIFICATION PASSED${NC}"
        exit 0
    fi
}

main "$@"
