#!/bin/bash
set -euo pipefail

# ATLAS Installation Script
# Installs K3s, NVIDIA GPU Operator, and deploys all services

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "$SCRIPT_DIR/lib/config.sh"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $1"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $1"; }
log_error() { echo -e "${RED}[ERROR]${NC} $1"; }

# Warn if both deployment modes' config files exist. install.sh is the
# K3s path and only reads atlas.conf — values in .env are ignored here.
# This catches the case where a user edited the wrong file. See PC-021.
warn_dual_config() {
    local repo_root="$(cd "$SCRIPT_DIR/.." && pwd)"
    if [[ -f "$repo_root/.env" ]] && [[ -f "$repo_root/atlas.conf" ]]; then
        log_warn "Both .env and atlas.conf are present."
        log_warn "  install.sh (this script) reads atlas.conf only — .env is ignored."
        log_warn "  If you are deploying with K3s, set values in atlas.conf."
        log_warn "  If you are deploying with docker compose, run \`docker compose up\` (not this script)."
        log_warn "  See ISSUES.md PC-021."
    fi
}

# Validate paths in configuration
validate_paths() {
    local errors=0

    log_info "Validating configuration paths..."

    # Check ATLAS_MODELS_DIR
    if [[ ! -d "$ATLAS_MODELS_DIR" ]]; then
        log_error "ATLAS_MODELS_DIR does not exist: $ATLAS_MODELS_DIR"
        log_error "  Please create the directory or update atlas.conf"
        log_error "  Example: mkdir -p $ATLAS_MODELS_DIR"
        errors=$((errors + 1))
    fi

    # Check for main model file
    if [[ -n "$ATLAS_MAIN_MODEL" ]]; then
        if [[ ! -f "$ATLAS_MODELS_DIR/$ATLAS_MAIN_MODEL" ]] && [[ ! -f "$ATLAS_MODELS_DIR/default.gguf" ]]; then
            log_error "Main model not found: $ATLAS_MODELS_DIR/$ATLAS_MAIN_MODEL"
            log_error "  Download with: ./scripts/download-models.sh"
            log_error "  Or update ATLAS_MAIN_MODEL in atlas.conf"
            errors=$((errors + 1))
        fi
    fi

    # Check for placeholder paths that weren't updated
    if [[ "$ATLAS_MODELS_DIR" == *"yourusername"* ]] || [[ "$ATLAS_MODELS_DIR" == *"nobase"* ]]; then
        log_error "ATLAS_MODELS_DIR contains placeholder path: $ATLAS_MODELS_DIR"
        log_error "  Please update atlas.conf with your actual path"
        log_error "  Example: ATLAS_MODELS_DIR=\"/home/$(logname)/models\""
        errors=$((errors + 1))
    fi

    if [[ $errors -gt 0 ]]; then
        log_error "Path validation failed with $errors error(s)"
        log_error "Please fix the above issues in atlas.conf and re-run"
        return 1
    fi

    log_info "Path validation passed"
    return 0
}

# Detect hardware and auto-configure resources
detect_hardware() {
    log_info "Detecting hardware configuration..."

    # Detect CPU cores
    local cpu_cores=$(nproc)
    log_info "  CPU cores: $cpu_cores"

    # Detect system memory (in GB)
    local sys_mem_gb=$(free -g | awk '/^Mem:/{print $2}')
    log_info "  System RAM: ${sys_mem_gb}GB"

    # Detect GPU memory (in MB)
    local gpu_mem_mb=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits 2>/dev/null | head -1)
    local gpu_name=$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -1)
    log_info "  GPU: $gpu_name (${gpu_mem_mb}MB VRAM)"

    # Calculate recommended resource limits based on hardware
    # Reserve 1 CPU for system, distribute rest among services
    local available_cpu=$((cpu_cores - 1))
    if [[ $available_cpu -lt 2 ]]; then
        available_cpu=2
    fi

    # LLM server gets 25% of available CPU (min 0.5, max 2)
    local llama_cpu_req=$(echo "scale=1; $available_cpu * 0.25" | bc)
    if (( $(echo "$llama_cpu_req < 0.5" | bc -l) )); then
        llama_cpu_req="0.5"
    elif (( $(echo "$llama_cpu_req > 2" | bc -l) )); then
        llama_cpu_req="2"
    fi

    # Other services share remaining CPU (min 0.25 each)
    local service_cpu_req=$(echo "scale=2; ($available_cpu - $llama_cpu_req) / 8" | bc)
    if (( $(echo "$service_cpu_req < 0.25" | bc -l) )); then
        service_cpu_req="0.25"
    elif (( $(echo "$service_cpu_req > 0.5" | bc -l) )); then
        service_cpu_req="0.5"
    fi

    # Memory allocation (LLM needs most for model loading)
    local llama_mem_req="8Gi"
    local llama_mem_limit="16Gi"
    if [[ $sys_mem_gb -lt 16 ]]; then
        llama_mem_req="4Gi"
        llama_mem_limit="8Gi"
        log_warn "Low system RAM detected. Reducing LLM memory allocation."
    fi

    # Export detected values for template processing
    export DETECTED_CPU_CORES=$cpu_cores
    export DETECTED_SYS_MEM_GB=$sys_mem_gb
    export DETECTED_GPU_MEM_MB=$gpu_mem_mb

    log_info "  Recommended LLM CPU request: $llama_cpu_req"
    log_info "  Recommended service CPU request: $service_cpu_req"

    # Check if current config exceeds hardware
    local total_cpu_requests=$(echo "$ATLAS_LLAMA_CPU_REQUEST + ($ATLAS_SERVICE_CPU_REQUEST * 8)" | bc 2>/dev/null || echo "0")
    if (( $(echo "$total_cpu_requests > $available_cpu" | bc -l 2>/dev/null || echo "0") )); then
        log_warn "Config requests ${total_cpu_requests} CPUs but only ${available_cpu} available"
        log_warn "Consider updating atlas.conf:"
        log_warn "  ATLAS_LLAMA_CPU_REQUEST=\"$llama_cpu_req\""
        log_warn "  ATLAS_SERVICE_CPU_REQUEST=\"$service_cpu_req\""
    fi
}

# Check prerequisites
check_prerequisites() {
    log_info "Checking prerequisites..."

    # Check if running as root or with sudo
    if [[ $EUID -ne 0 ]]; then
        log_error "This script must be run as root or with sudo"
        exit 1
    fi

    # Check for NVIDIA GPU
    if ! command -v nvidia-smi &> /dev/null; then
        log_error "NVIDIA driver not found. Install NVIDIA drivers first."
        exit 1
    fi

    # detect_hardware sizes CPU/memory shares with bc, which minimal
    # installs often lack. Fail with guidance instead of a bc: command
    # not found cascade mid-detection.
    if ! command -v bc &> /dev/null; then
        log_error "'bc' is required for hardware sizing but is not installed."
        log_error "  Install it first:  dnf install -y bc   (RHEL-family)"
        log_error "                     apt-get install -y bc   (Debian/Ubuntu)"
        exit 1
    fi

    # Detect and display hardware
    detect_hardware

    # Check GPU memory
    GPU_MEM=$(nvidia-smi --query-gpu=memory.total --format=csv,noheader,nounits | head -1)
    if [[ $GPU_MEM -lt 15000 ]]; then
        log_warn "GPU has ${GPU_MEM}MB VRAM. 16GB+ recommended for $ATLAS_MAIN_MODEL."
    fi

    # Check system memory
    SYS_MEM=$(free -g | awk '/^Mem:/{print $2}')
    if [[ $SYS_MEM -lt 16 ]]; then
        log_warn "System has ${SYS_MEM}GB RAM. 16GB+ recommended."
    fi

    # Warn early if both deployment modes' config files exist
    warn_dual_config

    # Validate paths
    if ! validate_paths; then
        exit 1
    fi

    # Validate config (ports, etc.)
    if ! validate_config; then
        log_error "Configuration validation failed"
        exit 1
    fi

    log_info "Prerequisites check passed"
}

# Check if all required images exist in K3s
check_images_exist() {
    local required_images=(
        "llama-server"
        "geometric-lens"
        "atlas-proxy"
        "v3-service"
        "sandbox"
    )

    # Get list of images once (requires root for k3s ctr)
    local images_list
    images_list=$(/usr/local/bin/k3s ctr images list 2>/dev/null) || return 1

    for img in "${required_images[@]}"; do
        if ! echo "$images_list" | grep -q "localhost/$img:latest"; then
            return 1
        fi
    done
    return 0
}

# Detect current installation state
detect_existing_setup() {
    echo ""
    log_info "Detecting existing setup..."

    SKIP_K3S=false
    SKIP_GPU=false
    SKIP_NAMESPACE=false

    # Check K3s
    if command -v k3s &> /dev/null && systemctl is-active --quiet k3s 2>/dev/null; then
        log_info "  [FOUND] K3s is installed and running"
        SKIP_K3S=true
    else
        log_info "  [MISSING] K3s - will install"
    fi

    # Check GPU availability (either via device plugin or GPU operator)
    if kubectl get nodes -o json 2>/dev/null | grep -q '"nvidia.com/gpu"'; then
        log_info "  [FOUND] GPU available in cluster (nvidia.com/gpu)"
        SKIP_GPU=true

        # Identify which method is providing GPU access
        if kubectl get ds -n kube-system nvidia-device-plugin-daemonset &>/dev/null; then
            log_info "         (via NVIDIA device plugin)"
        elif kubectl get namespace gpu-operator &>/dev/null; then
            log_info "         (via NVIDIA GPU Operator)"
        fi
    else
        log_info "  [MISSING] GPU not visible to cluster - will configure"
    fi

    # Check namespace
    if kubectl get namespace "$ATLAS_NAMESPACE" &>/dev/null; then
        log_info "  [FOUND] Namespace '$ATLAS_NAMESPACE' exists"
        SKIP_NAMESPACE=true
    else
        log_info "  [MISSING] Namespace '$ATLAS_NAMESPACE' - will create"
    fi

    # Check if ATLAS services are already running
    RUNNING_PODS=$(kubectl get pods -n "$ATLAS_NAMESPACE" --field-selector=status.phase=Running --no-headers 2>/dev/null | wc -l)
    RUNNING_PODS=${RUNNING_PODS:-0}
    if [[ "$RUNNING_PODS" -gt 0 ]]; then
        log_info "  [FOUND] $RUNNING_PODS ATLAS pod(s) already running"
        log_info "         (kubectl apply will update existing deployments)"
    fi

    # Check if container images are already in K3s
    SKIP_BUILD=false
    if check_images_exist 2>/dev/null; then
        log_info "  [FOUND] Container images already in K3s"
        SKIP_BUILD=true
    else
        log_info "  [MISSING] Container images - will build"
    fi

    echo ""
}

# Install K3s
install_k3s() {
    if [[ "$SKIP_K3S" == "true" ]]; then
        log_info "Skipping K3s installation (already running)"
        return
    fi

    if command -v k3s &> /dev/null; then
        log_info "K3s binary found, checking status..."
        if systemctl is-active --quiet k3s 2>/dev/null; then
            log_info "K3s already running"
            return
        fi
        log_info "K3s installed but not running, starting..."
        systemctl start k3s
        sleep 10
        kubectl wait --for=condition=Ready nodes --all --timeout=120s
        return
    fi

    log_info "Installing K3s..."
    curl -sfL https://get.k3s.io | sh -s - --write-kubeconfig-mode 644

    # Wait for K3s to be ready
    log_info "Waiting for K3s to be ready..."
    sleep 10
    kubectl wait --for=condition=Ready nodes --all --timeout=120s

    log_info "K3s installed successfully"
}

# Check if GPU is already available in cluster
check_gpu_available() {
    kubectl get nodes -o json 2>/dev/null | grep -q '"nvidia.com/gpu"'
}

# Install NVIDIA GPU Operator (or skip if GPU already available)
install_gpu_operator() {
    # Skip if detection already found GPU
    if [[ "$SKIP_GPU" == "true" ]]; then
        log_info "Skipping GPU setup (already available in cluster)"
        return
    fi

    # Double-check GPU availability
    if check_gpu_available; then
        log_info "GPU already available in cluster (nvidia.com/gpu detected)"
        log_info "Skipping GPU Operator installation"
        return
    fi

    # Check if GPU Operator is already installed
    if kubectl get namespace gpu-operator &> /dev/null; then
        log_info "GPU Operator namespace exists, checking status..."
        if kubectl get pods -n gpu-operator 2>/dev/null | grep -q "Running"; then
            log_info "GPU Operator already running"
            return
        fi
    fi

    log_info "Installing NVIDIA GPU Operator..."

    # Add Helm repo
    if ! command -v helm &> /dev/null; then
        log_info "Installing Helm..."
        curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash
    fi

    helm repo add nvidia https://helm.ngc.nvidia.com/nvidia || true
    helm repo update

    # Install GPU Operator
    kubectl create namespace gpu-operator || true
    helm install gpu-operator nvidia/gpu-operator \
        --namespace gpu-operator \
        --set driver.enabled=false \
        --wait --timeout 10m

    # Wait for GPU to be available
    log_info "Waiting for GPU to be available in cluster..."
    for i in {1..30}; do
        if check_gpu_available; then
            log_info "GPU available in cluster"
            return
        fi
        sleep 10
    done

    log_error "GPU not detected in cluster after 5 minutes"
    exit 1
}

# Create namespaces and secrets
setup_namespace() {
    log_info "Setting up namespace and secrets..."

    # Create namespace if not default
    if [[ "$ATLAS_NAMESPACE" != "default" ]]; then
        kubectl create namespace "$ATLAS_NAMESPACE" || true
    fi

    # Create secrets if they don't exist
    if ! kubectl get secret atlas-secrets -n "$ATLAS_NAMESPACE" &> /dev/null; then
        API_SECRET=$(openssl rand -hex 32)

        kubectl create secret generic atlas-secrets -n "$ATLAS_NAMESPACE" \
            --from-literal=api-secret="$API_SECRET" || true
    fi

    log_info "Namespace and secrets ready"
}

# Build container images
build_images() {
    # Check if images already exist in K3s
    if [[ "${FORCE_BUILD:-false}" != "true" ]] && check_images_exist; then
        log_info "All container images already exist in K3s - skipping build"
        log_info "  (use FORCE_BUILD=true to rebuild)"
        return 0
    fi

    log_info "Building container images..."

    "$SCRIPT_DIR/build-containers.sh"

    log_info "Container images built"
}

# Process templates with environment variable substitution
process_templates() {
    log_info "Processing manifest templates..."

    local template_dir="$K8S_DIR/templates"
    local manifest_dir="$K8S_DIR/manifests"

    # Ensure manifests directory exists
    mkdir -p "$manifest_dir"

    # Export all ATLAS_ variables for envsubst
    export "${!ATLAS_@}"

    # Process each template file
    for tmpl in "$template_dir"/*.yaml.tmpl; do
        if [[ -f "$tmpl" ]]; then
            local filename=$(basename "$tmpl" .tmpl)
            log_info "  Processing $filename..."
            envsubst < "$tmpl" > "$manifest_dir/$filename"
        fi
    done

    log_info "Templates processed"
}

# Deploy manifests
deploy_manifests() {
    log_info "Deploying ATLAS services..."

    # Process templates first to substitute config values
    process_templates

    # Preflight: refuse to proceed silently when the K3s deployment artifacts
    # aren't present in this checkout. process_templates is a for-loop over
    # *.yaml.tmpl that succeeds with zero output when the template dir is
    # empty; without this check we'd then go straight to `kubectl apply -f
    # missing.yaml` and crash mid-deploy. Better to fail clearly up front.
    local required_manifests=(
        "llama-deployment.yaml"
        "geometric-lens-deployment.yaml"
        "atlas-proxy-deployment.yaml"
        "sandbox-deployment.yaml"
    )
    local missing=()
    for m in "${required_manifests[@]}"; do
        [[ -f "$K8S_DIR/manifests/$m" ]] || missing+=("$m")
    done
    if (( ${#missing[@]} > 0 )); then
        echo "" >&2
        echo "ERROR: K3s manifest files are missing under $K8S_DIR/manifests/:" >&2
        for m in "${missing[@]}"; do
            echo "  - $m" >&2
        done
        echo "" >&2
        echo "process_templates renders these from \$K8S_DIR/templates/*.yaml.tmpl." >&2
        echo "Templates should be present at \$K8S_DIR/templates/ — confirm the" >&2
        echo "checkout includes that directory (\`ls \$K8S_DIR/templates/\`)." >&2
        echo "" >&2
        exit 1
    fi

    # Deploy main services
    log_info "Deploying main services..."
    kubectl apply -n "$ATLAS_NAMESPACE" -f "$K8S_DIR/manifests/llama-deployment.yaml"
    kubectl apply -n "$ATLAS_NAMESPACE" -f "$K8S_DIR/manifests/geometric-lens-deployment.yaml"
    kubectl apply -n "$ATLAS_NAMESPACE" -f "$K8S_DIR/manifests/atlas-proxy-deployment.yaml"

    # Optional: v3-service is rendered if its template is present.
    if [[ -f "$K8S_DIR/manifests/v3-service-deployment.yaml" ]]; then
        kubectl apply -n "$ATLAS_NAMESPACE" -f "$K8S_DIR/manifests/v3-service-deployment.yaml"
    fi

    # Deploy Atlas services
    log_info "Deploying Atlas services..."
    kubectl apply -n "$ATLAS_NAMESPACE" -f "$K8S_DIR/manifests/sandbox-deployment.yaml"

    log_info "Manifests deployed"
}

# Wait for all services
wait_for_services() {
    log_info "Waiting for all services to be ready..."

    # Service names as defined in the deployment templates' app= labels.
    SERVICES="llama-server geometric-lens atlas-proxy v3-service sandbox"

    for svc in $SERVICES; do
        log_info "Waiting for $svc..."
        kubectl wait --for=condition=Ready pod -l app=$svc -n "$ATLAS_NAMESPACE" --timeout=300s || {
            log_warn "$svc not ready within timeout, continuing..."
        }
    done

    log_info "All services deployed"
}

# Main
main() {
    echo "=========================================="
    echo "  ATLAS Installation Script"
    echo "=========================================="
    echo ""
    echo "Configuration:"
    echo "  Models dir:  $ATLAS_MODELS_DIR"
    echo "  Data dir:    $ATLAS_DATA_DIR"
    echo "  Namespace:   $ATLAS_NAMESPACE"
    echo ""

    check_prerequisites
    detect_existing_setup

    # Show summary of what will happen
    echo "Installation plan:"
    if [[ "$SKIP_K3S" == "true" ]]; then
        echo "  - K3s:          SKIP (already running)"
    else
        echo "  - K3s:          INSTALL"
    fi
    if [[ "$SKIP_GPU" == "true" ]]; then
        echo "  - GPU setup:    SKIP (already available)"
    else
        echo "  - GPU setup:    INSTALL (GPU Operator via Helm)"
    fi
    if [[ "$SKIP_BUILD" == "true" ]]; then
        echo "  - Build images: SKIP (already in K3s)"
    else
        echo "  - Build images: RUN"
    fi
    echo "  - Deploy:       RUN (kubectl apply - safe to re-run)"
    echo ""

    # Give user a chance to abort
    if [[ "${ATLAS_AUTO_CONFIRM:-false}" != "true" ]]; then
        read -p "Continue with installation? [Y/n] " -n 1 -r
        echo
        if [[ $REPLY =~ ^[Nn]$ ]]; then
            log_info "Installation cancelled"
            exit 0
        fi
    fi

    install_k3s
    install_gpu_operator
    setup_namespace
    build_images
    deploy_manifests
    wait_for_services

    echo ""
    echo "=========================================="
    echo "  Installation Complete!"
    echo "=========================================="
    echo ""
    echo "Next steps:"
    echo "  1. Download models: ./scripts/download-models.sh"
    echo "  2. Verify installation: ./scripts/verify-install.sh"
    echo ""
    echo "Service endpoints:"
    echo "  atlas-proxy:    http://${ATLAS_NODE_IP}:${ATLAS_PROXY_NODEPORT}"
    echo "  llama-server:   http://${ATLAS_NODE_IP}:${ATLAS_LLAMA_NODEPORT}"
    echo "  geometric-lens: http://${ATLAS_NODE_IP}:${ATLAS_LENS_NODEPORT}"
    echo "  sandbox:        http://${ATLAS_NODE_IP}:${ATLAS_SANDBOX_NODEPORT}"
    echo ""
}

main "$@"
