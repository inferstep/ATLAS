<!-- source: docs/SETUP.md synced-through: 6ff5ecb -->
> **[English](../../SETUP.md)** | **[简体中文](../zh-CN/SETUP.md)** | **[日本語](../ja/SETUP.md)** | **한국어**

> ℹ️ 영어 원본([SETUP.md](../../SETUP.md))의 번역본입니다. ATLAS에는 고정 기본 모델이 없습니다 — `atlas init`으로 레지스트리 모델을 선택하거나 호환 GGUF를 지정하십시오. 원본과 차이가 있을 경우 영어 원본이 우선합니다.


# ATLAS 설정 가이드

네 가지 배포 방법을 제공합니다: **원샷 부트스트랩**(신규 설치 권장), Docker Compose(수동), 베어메탈, K3s.

---

## 설치 경로 선택

설치 절차는 하드웨어와 OS에 따라 다릅니다. 본인 환경에 맞는 행을 찾아 링크된 섹션으로 이동하세요.

| 하드웨어 | OS | 권장 경로 | 지원 수준 ([매트릭스](../../../SUPPORT_MATRIX.md)) |
|---|---|---|---|
| NVIDIA RTX 50 시리즈 / Blackwell (B100, GB10) | Linux | [방법 0: 부트스트랩](#방법-0-원샷-부트스트랩) 또는 [방법 1: Docker](#방법-1-docker-compose-권장) | 지원(Supported) — 게시된 CUDA 이미지는 Blackwell 대상 |
| NVIDIA RTX 20/30/40, GTX 10xx, 데이터센터 (V100/A100/H100/T4/L4) | Linux | [방법 1: Docker](#방법-1-docker-compose-권장) + 일회성 [로컬 재빌드](#cuda-컴퓨트-캐퍼빌리티-dockerfilev31) | 프리뷰(Preview) — 로컬 재빌드 필요 |
| NVIDIA GPU | Windows (WSL2) | [방법 1: Docker — NVIDIA 섹션](#방법-1-docker-compose-권장) | 미지원(Unsupported) — 테스트되지 않았고 아무 주장도 하지 않음; 리포트 환영 |
| AMD GPU (RX 6000/7000, MI200+) | Linux | [방법 1: Docker — AMD ROCm](#amd-rocm--무엇이-다른가) | 커뮤니티 검증(Community-tested) ([GH #26](https://github.com/inferstep/ATLAS/issues/26)) |
| **Apple Silicon (M1/M2/M3/M4)** | **macOS** | **[SETUP_MACOS.md](../../SETUP_MACOS.md)** (전용 가이드 — 하이브리드 네이티브 Metal + Docker) | 지원 (메인테이너 검증, M2 Pro) |
| Intel Arc / Iris Xe | Linux | [방법 1: Docker — Vulkan](#vulkan--크로스-벤더-폴백) | 프리뷰 — Vulkan은 lavapipe에서만 스모크 테스트됨; 실제 GPU 검증은 아직 없음 |
| Snapdragon X Elite (노트북) | Linux | [Vulkan](#vulkan--크로스-벤더-폴백) + [arm64 섹션](#arm64) | 프리뷰 (Linux arm64). Windows on ARM은 미지원 |
| 구형 AMD GPU (Vega, RDNA1, ROCm 6.x 없음) | Linux | [방법 1: Docker — Vulkan](#vulkan--크로스-벤더-폴백) | 프리뷰 |
| ARM64 위의 NVIDIA (DGX Spark, Jetson) | Linux | [arm64 섹션](#arm64) (sbsa/l4t 베이스 교체로 CUDA) | 프리뷰 — 빌드 레시피는 제공되나 엔드투엔드로 검증된 디바이스는 아직 없음 (#115) |
| Raspberry Pi 5 | Linux | [Vulkan + arm64](#arm64) | 프리뷰 — CPU급 성능 예상 |
| Intel Mac (2020년 이전) | macOS | [방법 1: Docker — Vulkan](#vulkan--크로스-벤더-폴백) | 미지원 — Docker Desktop 필요(테스트되지 않음); Metal은 Apple Silicon 전용 |
| GPU 없는 CPU 전용 | 무관 | [CPU 전용 설치](#cpu-only) | 프리뷰 — 스모크 테스트 전용, 매우 느림 |
| Kubernetes 클러스터 | Linux | [방법 3: K3s](#방법-3-k3s) | 프리뷰 — 템플릿은 CI 검증됨; 자동화된 라이브 클러스터 테스트 없음 |
| 베어메탈 / 개발 | Linux | [방법 2: 베어메탈](#방법-2-베어메탈) | 프리뷰 — 수동 검증만 |

본인 환경이 없나요? `uname -a` 출력과 `lspci | grep -i vga`(Linux) / `system_profiler SPDisplaysDataType`(Mac)을 첨부해 이슈를 열어 주시면 행을 추가하겠습니다.

---

## 방법 0: 원샷 부트스트랩

curl 명령 하나로 배포판을 감지하고 Docker + nvidia-container-toolkit을 설치하며, 모델 가중치를 다운로드하고 스택을 기동합니다. 멱등적이어서 다시 실행해도 안전합니다.

> **NVIDIA Blackwell 이전 GPU(RTX 20/30/40 시리즈, GTX 10xx, V100/A100/T4/L4/H100) 사용자는 이것부터 읽으세요.**
> 게시된 `atlas-llama` CUDA 이미지는 컴퓨트 캐퍼빌리티 `120;121`(Blackwell —
> RTX 50xx, B100, GB10) **전용**으로 컴파일되어 있습니다. 구형 NVIDIA GPU에서는
> llama-server가 시작 시
> `no kernel image is available for execution on the device`
> 오류로 실패합니다.
> 본인 GPU 아키텍처에 맞춰 추론 이미지를 한 번 재빌드하세요:
>
> ```bash
> # find your arch (drop the dot: 8.6 -> 86)
> nvidia-smi --query-gpu=compute_cap --format=csv,noheader
> docker compose build --build-arg CUDA_ARCH=86 llama-server   # example: RTX 30xx
> docker compose up -d --no-deps llama-server
> ```
> 전체 아치 표: [CUDA 컴퓨트 캐퍼빌리티](#cuda-컴퓨트-캐퍼빌리티-dockerfilev31).

```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

또는 체크아웃에서:
```bash
./scripts/atlas-bootstrap.sh
```

**지원 배포판:**

| 계열 | 배포판 |
|---|---|
| Debian (apt-get) | Ubuntu 20.04+, Debian 11+ |
| RHEL (dnf) | RHEL 9+, Rocky 9+, AlmaLinux 9+, CentOS Stream 9+, Oracle Linux 9+ |
| Fedora (dnf) | Fedora 38+ |

`ID_LIKE`가 위 계열과 일치하는 다른 배포판(예: Linux Mint, Pop!_OS)은 경고와 함께 허용됩니다. 목록에 없는 배포판 — Arch, openSUSE, Alpine, NixOS — 은 테스트되지 않았으며, 스크립트가 실행을 거부합니다.

부트스트랩은 EPEL, nouveau 드라이버 충돌, libnvidia-ml.so.1 누락 케이스(RHEL 최소 설치), 그리고 "사용자를 docker 그룹에 추가했지만 현재 셸에는 아직 반영되지 않은" 경합을 우회 처리합니다.

**모델 선택:** `.env.example`에는 선택된 모델이 없습니다. 부트스트랩이 `.env`를 생성했는데 `ATLAS_MODEL_FILE`이 비어 있으면, 레지스트리의 기본 권장 모델을 `.env`에 기록하여(그 과정이 로그로 남습니다) 마법사 없이도 원샷 설치가 완료되게 합니다. 선택은 `.env`를 편집하거나 `atlas init`을 실행해 언제든 바꿀 수 있습니다. 이미 비어 있지 않은 선택은 그대로 존중됩니다.

<a id="cpu-only"></a>
**CPU 전용 / GPU 없는 호스트 (프리뷰(Preview) — 스모크 테스트 전용).** ATLAS는 Vulkan 이미지의 lavapipe CPU 래스터라이저를 통해 GPU 없이도 부팅되지만, 추론이 매우 느립니다. 스택이 동작하는지 확인하는 용도로 쓰고, 실제 코딩 세션 용도로는 쓰지 마세요.

1. **부트스트랩은 옵트인하지 않는 한 GPU 없는 호스트를 거부합니다:**
   `ATLAS_BOOTSTRAP_SKIP_GPU=1 ./scripts/atlas-bootstrap.sh`
   이렇게 하면 부트스트랩이 `docker-compose.vulkan.yml`을 겹치고(`/dev/dri`가
   없으면 `docker-compose.cpu.yml`도 함께), 모델 선택과
   `ATLAS_BACKEND=vulkan|cpu`를 직접 `.env`에 기록하며, ASA 빌드를 건너뜁니다.
2. **GPU 없는 호스트에서 `atlas init`을 실행하지 마세요** — 마법사는 크기를
   정할 수 없는 `.env`를 쓰는 대신 의도적으로 거부(exit 1)합니다. 모델 선택은
   부트스트랩이 처리하며, 이후 모델 변경은 `atlas model install`로 하세요.

수동 등가 절차:

```bash
cp .env.example .env    # set ATLAS_MODEL_FILE / ATLAS_MODEL_NAME
atlas model install Qwen3.5-9B-Q6_K
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml -f docker-compose.cpu.yml up -d
atlas doctor            # gpu check WARNS ("CPU-only mode — very slow"); warns exit 0
```

**방화벽:** Compose 스택은 모든 서비스를 `127.0.0.1`에만 게시하므로, 로컬 사용에는 방화벽 변경이 필요 없으며 부트스트랩은 기본적으로 firewalld를 건드리지 않습니다. 서비스를 라우팅 가능한 인터페이스에 다시 바인딩하는 배포를 위해 서비스 포트(8090, 8099, 8070, 30820)를 열려면 `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1`을 설정하세요.

**실행 모드 — 둘 다 동작합니다:**

```bash
# Run as your normal user; sudo elevates as needed (Docker install, etc).
# Install ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash

# Run via sudo. SUDO_USER is detected, install still ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | sudo bash

# Real root login (no sudo) — install owned by root. Only do this if there's
# no human user on the box (CI runner, container, etc).
```

**신중 설치 변형** (같은 스크립트입니다. 계속 바뀌는 `main`의 스크립트를 그대로 bash에 파이프하고 싶지 않은 분들을 위한 것입니다):

```bash
# Pinned to a release: fetch the script AT the tag and install that tag.
# The checkout is pinned to the (SSH-signed) tag and ATLAS_IMAGE_TAG is
# pinned to the matching cosign-signed images.
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/v3.1.3/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.3 bash

# Review before running: download, read, then execute the same bytes.
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

**설정 환경 변수:**

| 플래그 | 효과 |
|---|---|
| `ATLAS_BOOTSTRAP_SKIP_DOCKER=1` | Docker를 설치하지 않음(이미 별도로 관리 중일 때) |
| `ATLAS_BOOTSTRAP_SKIP_GPU=1` | GPU 런타임 설치(NVIDIA 툴킷 또는 ROCm 설정)를 건너뜀. |
| `ATLAS_BOOTSTRAP_SKIP_MODELS=1` | 모델 가중치를 다운로드하지 않음 |
| `ATLAS_BOOTSTRAP_SKIP_COMPOSE=1` | `docker compose up`을 실행하지 않음 |
| `ATLAS_BOOTSTRAP_SKIP_ASA=1` | ASA 스티어링 벡터 빌드를 건너뜀(기본: 서비스 기동 후 ~5분 내 빌드; GPU가 없으면 자동으로 건너뜀) |
| `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` | firewalld에서 서비스 포트를 개방(기본: 꺼짐 — 서비스는 루프백에 바인딩) |
| `ATLAS_BOOTSTRAP_NO_SUDO=1` | sudo를 시도하는 대신 실패 |
| `ATLAS_BOOTSTRAP_REF=vX.Y.Z` | `main`을 추적하는 대신 git 태그/sha에 설치를 고정; `vX.Y.Z` 값은 `ATLAS_IMAGE_TAG`도 대응 이미지에 고정합니다 |
| `ATLAS_INSTALL_DIR=/path` | 클론 위치(기본 `/opt/atlas` — 아래 참고) |
| `ATLAS_REPO_URL=https://...` | 대체 저장소 URL |
| `ATLAS_GO_VERSION=1.26.6` | TUI 빌드에 설치되는 Go 툴체인 버전 (TUI는 1.26.6+가 필요하며, 구형 툴체인이 설치되어 있으면 자동으로 이를 가져옵니다) |

**왜 `/opt/atlas`인가?** 시스템 전역 서드파티 소프트웨어의 표준 FHS 접두사이고, `$HOME` 정리에도 살아남으며, 같은 머신의 여러 사용자가 하나의 설치를 공유할 수 있기 때문입니다. 홈 디렉토리에 설치하고 싶다면:

```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh \
  | ATLAS_INSTALL_DIR=$HOME/atlas bash
```

완료되면 빠른 시작 명령이 담긴 초록색 "ATLAS ready" 배너가 출력됩니다. 빠른 회선의 새 VM 기준 총 소요 시간: 약 10~30분(모델 다운로드가 대부분).

각 단계를 직접 수행하고 싶다면 아래의 방법 1을 사용하세요.

---

## 사전 요구 사항 (모든 방법 공통)

| 요구 사항 | 세부 내용 |
|-------------|---------|
| **GPU** | VRAM 16GB 이상. NVIDIA (CUDA, 지원(Supported) — 게시된 이미지는 Blackwell 대상이며, 구형 카드는 일회성 [로컬 재빌드](#cuda-컴퓨트-캐퍼빌리티-dockerfilev31) 필요); AMD (ROCm, 커뮤니티 검증(Community-tested)); Apple Silicon (Metal, macOS 하이브리드, 지원 — [SETUP_MACOS.md](../../SETUP_MACOS.md) 참고); Vulkan(프리뷰(Preview))은 크로스 벤더 폴백이고, Intel Arc (SYCL)는 로드맵(Roadmap)입니다. [§ 지원 GPU](#지원-gpu) 참고. |
| **GPU 드라이버** | NVIDIA: 전용 드라이버(`nvidia-smi`에서 GPU가 보여야 함). AMD: `amdgpu-dkms` 커널 드라이버(`/dev/kfd`가 존재해야 하며, `rocm-smi`에서 GPU가 보여야 함). |
| **Python 3.9+** | pip 포함 |
| **curl** | 모델 가중치 다운로드용 |
| **모델 가중치** | 호스트에 맞는 레지스트리 모델 또는 자체 반입(BYO) GGUF. `atlas init`이 하나를 추천하고 선택을 `.env`에 기록합니다. |

### GPU 확인

**NVIDIA:**

```bash
nvidia-smi
# Should show your GPU with driver version and VRAM
# If this fails, install NVIDIA proprietary drivers first
```

**AMD:**

```bash
rocm-smi --showproductname --showmeminfo vram
# Should show your GPU model and total VRAM
# If rocm-smi is missing or /dev/kfd doesn't exist, install ROCm:
#   RHEL 9: sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
#           sudo amdgpu-install --usecase=dkms,rocm
#   Ubuntu: Follow https://rocm.docs.amd.com/projects/install-on-linux/
# Then REBOOT.
```

**자동 감지** — `atlas tier`가 벤더 전반을 자동 감지해 무엇을 찾았는지 알려줍니다:

```bash
pip install -e .
atlas tier              # prints detected GPU, tier classification, recommended settings
atlas tier --json       # machine-readable (used by atlas init wizard)
```

---

## 방법 1: Docker Compose (권장)

가장 많이 검증된 배포 방법입니다. CI가 compose 파일을 검증하고 전체 컨트롤 플레인을 (가짜 추론으로) 결정론적으로 구동하며, 릴리스는 실제 하드웨어에서 Compose로 스모크 테스트됩니다. 실제 GPU 추론 동작은 GitHub 호스팅 CI가 아니라 아래 하드웨어 표에 나열된 카드에서 검증됩니다.

### 추가 사전 요구 사항

**NVIDIA (CUDA):**
- [nvidia-container-toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)이 설치된 **Docker**, **또는 Podman** + 동일 툴킷
- 약 20GB 디스크 공간 (모델 가중치 + 컨테이너 이미지)

**AMD (ROCm):**
- **Docker**만으로 충분 — ROCm은 별도 컨테이너 런타임이 필요 없으며 `--device=/dev/kfd --device=/dev/dri` 패스스루로 충분합니다
- 사용자가 `video`와 `render` 그룹에 속해야 합니다: `sudo usermod -aG video,render $USER` (이후 재로그인)
- 약 22GB 디스크 공간 (ROCm 이미지는 CUDA 대응 이미지보다 ~2GB 큼)

### 설정

```bash
# 1. Clone
git clone https://github.com/inferstep/ATLAS.git
cd ATLAS

# 2. Install the ATLAS CLI (puts `atlas` in ~/.local/bin)
pip install --user -e .

# Make sure ~/.local/bin is on your PATH so `atlas` resolves:
case ":$PATH:" in *":$HOME/.local/bin:"*) ;; *)
   echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc
   source ~/.bashrc
;; esac

# 3. Select/install a model and write model-aware runtime sizing
atlas init

# 4. Install Go 1.26.6+ — required for the TUI client (atlas tui) and
#    optional for the proxy (proxy builds automatically on first run if Go
#    is present; otherwise it runs in Docker with file access limited to
#    ATLAS_PROJECT_DIR). Quickest path:
mkdir -p /tmp/go-install && cd /tmp/go-install
curl -LO https://go.dev/dl/go1.26.6.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.6.linux-amd64.tar.gz
echo 'export PATH="/usr/local/go/bin:$PATH"' >> ~/.bashrc
source ~/.bashrc
cd -

# Then build the TUI:
cd tui && go build -o ~/.local/bin/atlas-tui . && cd ..

# 5. Review the environment generated by `atlas init`
#    ATLAS_MODEL_FILE and ATLAS_MODEL_NAME identify this installation's
#    selected model; they are intentionally not project-wide defaults.
${EDITOR:-vi} .env

# 6. Start all services (first run builds container images — this takes several minutes)
#    NVIDIA hosts (default):
docker compose up -d                                                  # or: podman-compose up -d
#    AMD ROCm hosts:
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
#    `atlas init` writes a marker comment into .env telling you which to use.

# 7. Verify everything is healthy (wait for all services to show "healthy")
docker compose ps

# 8. Start coding (from your project directory)
cd /path/to/your/project
atlas
```

#### AMD ROCm — 무엇이 다른가

ROCm 경로는 다음 세 가지를 *제외하면* NVIDIA와 동일합니다:

1. **두 compose 파일로 기동** (또는 `atlas init`에 맡기세요):
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
   ```
   오버라이드는 llama-server 이미지를 ROCm 빌드로 전환하고, NVIDIA 드라이버 요청을 `/dev/kfd` + `/dev/dri` 패스스루로 교체하며, 엔트리포인트가 HIP 튜닝 분기를 타도록 `ATLAS_BACKEND=rocm`을 강제합니다.

2. **`nvidia-container-toolkit` 불필요** — ROCm은 별도 컨테이너 런타임이 필요 없고 커널 수준 디바이스 접근만 필요합니다. 사용자가 올바른 그룹에 있는지 확인하세요:
   ```bash
   id -nG | tr ' ' '\n' | grep -E '^(render|video)$'
   # Should print both. If not:
   sudo usermod -aG video,render $USER
   # Then log out + back in (or: newgrp render)
   ```

3. **GPU 컴퓨트 타깃.** 기본 `Dockerfile.rocm` 빌드는 RDNA3(7000 시리즈), RDNA2(6000 시리즈), CDNA2(MI200)를 포괄하는 "fat" 이미지입니다 — `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`. 특정 GPU만 겨냥한 더 작은 이미지를 원하면 빌드 전에 `ATLAS_GFX_TARGET`을 설정하세요:
   ```bash
   # Example: only build for RX 7900 XT/XTX
   ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
   ```
   본인 카드의 gfx 타깃은 [LLVM AMDGPU 프로세서 표](https://llvm.org/docs/AMDGPUUsage.html)를 참고하세요.

"공식 미지원 GPU인데 ROCm이 어느 정도 동작하는" 케이스(구형 Vega, RDNA1)는 [TROUBLESHOOTING.md § AMD GPU가 감지되지 않음](../ko/TROUBLESHOOTING.md)의 `ATLAS_HSA_OVERRIDE_GFX_VERSION` 우회책을 참고하세요.

#### Vulkan — 크로스 벤더 폴백

네이티브 벤더 백엔드가 하드웨어용으로 패키징되지 않은 경우(Intel Arc, Snapdragon Adreno, ROCm 6.x가 없는 구형 AMD) Vulkan이 폴백입니다. Dockerfile 하나가 AMD(Mesa RADV), Intel(Mesa ANV), NVIDIA(nvidia-container-toolkit), Apple(macOS Docker의 MoltenVK), Snapdragon(Adreno), CPU(Mesa lavapipe)를 모두 커버합니다.

트레이드오프: 튜닝된 네이티브 백엔드보다 통상 20~40% 느립니다. CUDA/ROCm이 선택지가 아니거나, 특이한 하드웨어에서 ATLAS가 부팅되는지 스모크 테스트할 때 사용하세요.

```bash
# Option A — let atlas init pick it for you
# (the wizard offers Vulkan when your GPU vendor's native backend isn't packaged,
#  or run with --backend vulkan to force it regardless of vendor):
atlas init --backend vulkan
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d

# Option B — already-installed deployment, just switch the override file:
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml up -d
# (the entrypoint dispatches on ATLAS_BACKEND, which the compose overlay
#  sets to vulkan; .env's value is ignored when the overlay is in play)
```

CUDA/ROCm과 다른 점:

1. **벤더별 커널 드라이버 요구가 없습니다.** Vulkan ICD는 이미지 안에 들어 있습니다(`mesa-vulkan-drivers`가 AMD/Intel/CPU를 커버; NVIDIA의 ICD는 nvidia-container-toolkit 마운트에서 옵니다).
2. **`/dev/dri` 패스스루만** — `/dev/kfd`도 `--gpus all`도 없습니다(NVIDIA 툴킷을 경유하는 경우는 예외로, 그때는 둘 다 여전히 동작합니다).
3. **GPU 선택은 `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES` 대신 `ATLAS_VK_DEVICE_SELECT`로.** 형식은 Mesa 표준입니다: `"vendorID:deviceID"`(16진수) 또는 디바이스 이름의 부분 문자열. `GGML_VK_VISIBLE_DEVICES`(숫자 인덱스)도 동작합니다.
4. **`atlas doctor`**가 `_check_vulkan_via_docker` 탐침을 실행합니다 — 단, `ATLAS_BACKEND=vulkan`이 설정된 경우에만(그 외에는 CUDA/ROCm 실행을 빠르게 유지하기 위해 건너뜁니다).

GPU를 기대했는데 `vulkaninfo`에 `llvmpipe` CPU 디바이스만 보인다면 커널 측 디바이스 패스스루가 실패한 것입니다 — 호스트에 `/dev/dri/renderD*`가 존재하는지, 사용자가 `video` + `render` 그룹에 있는지 확인하세요(위 ROCm 요구 사항과 동일).

<a id="arm64"></a>
#### arm64 호스트 (#115)

ATLAS는 두 CPU 아키텍처를 대상으로 합니다: `x86_64`(기본, 모든 백엔드 사용 가능)와 `aarch64`(백엔드 일부만). `atlas doctor`로 확인하세요 — `arch` 점검이 GPU 점검 전에 아키텍처와 사용 가능한 백엔드를 표시합니다.

**아키텍처별 백엔드 가용성:**

| 백엔드 | x86_64 | aarch64 | 비고 |
|---|---|---|---|
| CUDA | 예 (rockylinux9 베이스) | 예 (sbsa 또는 l4t 베이스, build-arg 교체) | DGX Spark = sbsa, Jetson = l4t |
| ROCm | 예 | **아니오** | AMD는 arm64 ROCm 릴리스가 없습니다. 대신 Vulkan을 사용하세요. |
| Vulkan | 예 | 예 (Mesa는 멀티 아키텍처) | 모든 arm64 GPU의 범용 폴백 |
| CPU (lavapipe) | 예 | 예 | 느리지만 항상 동작 |

**대상 arm64 디바이스:**

- **NVIDIA DGX Spark** (Grace-Blackwell GB10) — sbsa 베이스 이미지 경유 CUDA, 컴퓨트 캐퍼빌리티 12.0/12.1
- **NVIDIA Jetson Orin / AGX / Nano** — l4t 베이스 이미지 경유 CUDA, 컴퓨트 캐퍼빌리티 8.7
- **Apple Silicon (M1/M2/M3/M4)** — Docker Desktop의 MoltenVK 경유 Vulkan(느린 경로); 빠른 경로인 네이티브 Metal 설치는 [#32](https://github.com/inferstep/ATLAS/issues/32)에서 추적
- **Snapdragon X Elite** (Windows on ARM 노트북) — Adreno 드라이버 경유 Vulkan
- **Raspberry Pi 5** — Mesa V3D 드라이버 경유 Vulkan, CPU급 성능 예상
- **Ampere Altra / AWS Graviton 워크스테이션** — lavapipe 경유 Vulkan(CPU 폴백; 아직 소비자용 arm64 dGPU가 없으므로)

**arm64용 Vulkan 이미지 빌드:**

```bash
# Multi-arch build that produces a single image manifest covering both archs:
docker buildx build --platform linux/amd64,linux/arm64 \
  -t atlas-llama-server:vulkan \
  -f inference/Dockerfile.vulkan inference/
```

**arm64용 CUDA 이미지 빌드** (DGX Spark 예시):

```bash
# Swap to the sbsa-capable ubuntu base, build with --platform linux/arm64:
docker buildx build --platform linux/arm64 \
  --build-arg BUILDER_IMAGE=nvidia/cuda:12.9.0-devel-ubuntu22.04 \
  --build-arg RUNTIME_IMAGE=nvidia/cuda:12.9.0-runtime-ubuntu22.04 \
  -t atlas-llama-server:cuda-arm64 \
  -f inference/Dockerfile.v31 inference/
```

Jetson의 경우 두 build arg 모두 `nvcr.io/nvidia/l4t-jetpack:r36.3.0`으로 교체하세요(l4t는 JetPack + CUDA + cuDNN을 하나의 이미지로 제공합니다).

**알려진 공백 (#115에서 추적):**

- GHCR에 사전 빌드된 arm64 이미지가 아직 없습니다 — arm64 사용자는 위 레시피로 로컬 빌드해야 합니다. 최소 한 대의 arm64 디바이스가 엔드투엔드로 검증되면 멀티 아키텍처 사전 빌드 이미지가 제공될 예정입니다.
- 부트스트랩 설치 프로그램(`scripts/atlas-bootstrap.sh`)은 arm64 경로에 대해 감사되지 않았습니다.
- 다섯 개 대상 디바이스 모두 하드웨어 테스트 매트릭스가 비어 있습니다 — 해당 디바이스를 가진 얼리어답터께서는 `atlas doctor` 출력과 `vulkaninfo --summary`를 [#115](https://github.com/inferstep/ATLAS/issues/115)에 남겨 주세요.

### 첫 실행 시 동작

1. Docker가 `ghcr.io/inferstep/atlas-{proxy,v3,lens,llama,sandbox}`에서 사전 빌드된 컨테이너 이미지 5개를 가져옵니다(빠른 회선 기준 ~3분). 소스에서 빌드하려면(개발 경로) `up` 전에 `docker compose build`를 실행하세요 — 아래 "이미지 소스" 참고.
2. llama-server가 7GB 모델을 GPU VRAM에 로드합니다 (약 1~2분)
3. 모든 서비스가 헬스 체크를 시작합니다
4. 5개 서비스(llama-server, geometric-lens, v3-service, sandbox, atlas-proxy)가 모두 healthy로 보고되면, `atlas`가 연결되어 Bubbletea TUI를 실행합니다

이후의 `docker compose up -d`는 이미지가 캐시되어 있으므로 빠르게(수 초) 시작됩니다.

### 이미지 소스: 사전 빌드 vs 소스 빌드

`docker-compose.yml`은 모든 서비스에 대해 `image:`(GHCR)와 `build:`(로컬 Dockerfile)를 모두 선언합니다. Compose의 기본 동작:

| 명령 | 동작 |
|---------|--------------|
| `docker compose up -d`            | 로컬 캐시에 없으면 `image:`를 pull, 있으면 로컬 재사용 |
| `docker compose pull`             | GHCR에서 최신 태그 강제 pull (로컬 캐시 덮어씀) |
| `docker compose build`            | `Dockerfile`에서 빌드 (GHCR 이미지보다 우선) |
| `docker compose up -d --build`    | 항상 소스에서 재빌드 후 시작 |

**태그 고정.** 태그의 기본값은 `latest`입니다. 특정 버전에 고정하려면(프로덕션 권장) `.env`에서 `ATLAS_IMAGE_TAG`를 설정하세요:

```env
ATLAS_IMAGE_TAG=3.1.3      # semver tag from a git release
ATLAS_IMAGE_TAG=sha-abc1234  # exact commit
ATLAS_IMAGE_TAG=dev          # bleeding edge from dev branch
```

사용 가능한 태그는 <https://github.com/inferstep/ATLAS/pkgs/container/atlas-proxy>에 나열됩니다(`atlas-proxy`를 다른 서비스 이름으로 바꾸세요: `atlas-v3`, `atlas-lens`, `atlas-llama`, `atlas-sandbox`).

특이 케이스: GHCR에서 아직 비공개인 패키지에 대해 `compose pull`이 `unauthorized`로 실패합니다 — `read:packages` 토큰으로 인증하거나 소스에서 빌드하세요. `compose pull`은 같은 태그를 공유하는 로컬 빌드 이미지도 덮어씁니다. 서비스를 반복 작업 중이라면 pull을 건너뛰거나 `ATLAS_IMAGE_TAG=dev-local`을 설정해 로컬과 레지스트리 이미지가 다른 태그에 살도록 하세요. 포크의 이미지를 가져오려면 `.env`에 `ATLAS_GHCR_OWNER=<your-username>`을 설정하세요.

### 설치 확인

가장 빠른 방법은 **`atlas doctor`**입니다 — 호스트 환경(GPU 런타임, 모델·lens 아티팩트), docker 스택(컨테이너, 헬스 엔드포인트, 인증, 상태), 그리고 라이브 모델 추론을 점검하며, 각 결과를 완료되는 대로 출력하고 종료 코드 0(정상) / 1(실패)을 반환합니다. 정확한 점검 개수는 백엔드, 스택 상태, 플래그에 따라 달라집니다. `atlas-bootstrap.sh`가 설치 마지막에 실행하는 것도 이것입니다.

```bash
atlas doctor              # full check (~5–10s)
atlas doctor --quick      # skip the e2e model inference (~2s)
atlas doctor --json       # machine output, for scripts/CI (buffered, one JSON document)
atlas doctor -v           # verbose: show detail for each check
```

점검 항목:

| 그룹 | 점검 | 확인 내용 |
|---|---|---|
| Host | docker | 데몬 접근 가능 |
| Host | compose | docker compose v2 설치됨 |
| Host | arch | CPU 아키텍처(`x86_64` / `aarch64`)와 해당 아키텍처에서 사용 가능한 백엔드 (#115) — GPU 점검 전에 항상 실행 |
| Host | gpu | 벤더 인식 GPU 런타임: NVIDIA(nvidia-container-toolkit이 Docker 안에서 nvidia-smi 실행) 또는 AMD(`/dev/kfd` 패스스루); GPU가 감지되지 않으면 경고 |
| Host | vulkan | Docker 안에서 Vulkan ICD가 보이는지 — `ATLAS_BACKEND=vulkan`일 때만 |
| Host | metal-native | 네이티브 llama-server 바이너리가 존재하고 실행 가능한지 — `ATLAS_BACKEND=metal`(macOS 하이브리드)일 때만 |
| Host | model_file | `.env`에서 선택된 `ATLAS_MODEL_FILE`이 존재하고 100MB 초과 |
| Host | lens_weights | `cost_field.pt` + G(x) 아티팩트 존재 |
| Host | asa_steering | `ast_edit_steering.gguf` 존재 (BiasBusters #4 — 실패가 아닌 경고; ATLAS는 이것 없이도 동작하며, structural_edit-vs-edit_file 편향이 스티어링되지 않을 뿐) |
| Host | tier_match | `.env` 모델 선택이 호스트 하드웨어와 일치(초과 선택은 경고 — OOM 위험 — 일치나 여유 선택은 통과) |
| Host | tier_constraints | 호스트 CPU/RAM/디스크가 권장 등급 최소치를 충족("16GB GPU인데 RAM 8GB" 같은 불일치를 포착) |
| Stack | container/llama-server, geometric-lens, v3-service, sandbox, atlas-proxy | 5개 모두 실행 중이고 healthy |
| Stack | health/llama, lens, v3, sandbox, proxy | 5개 `/health` 엔드포인트 모두 ok 반환 |
| Stack | internal_auth | 내부 서비스 인증: 토큰 파일이 엄격한 권한과 함께 존재하고, 실제 강제 여부를 양방향으로 탐침(잘못된 토큰 → 401, 유효한 토큰은 수락); 인증이 비활성화된 경우(`secrets/service-token` 없음) 경고 |
| Stack | status_dimensions | 정보성: 프록시 `/v1/calibration/status`가 보고하는 lens/ASA 상태 7개 차원(TUI 배지가 읽는 것과 동일한 소스); 실행을 실패시키지 않음 |
| Stack | image_skew | `atlas-*` 이미지 5개가 모두 같은 태그 |
| End-to-end | e2e_smoke | llama-server로의 라이브 `/v1/chat/completions` 왕복 (`--quick`으로 건너뜀) |

`vulkan`과 `metal-native` 행은 구성된 백엔드에 따라 조건부입니다. health, `internal_auth`, `status_dimensions` 행은 컨테이너가 하나 이상 떠 있을 때만 실행되고, `e2e_smoke`는 `--quick`으로 건너뜁니다. 나머지 점검은 항상 실행됩니다.

직접 확인하고 싶다면:

```bash
# Hit each health endpoint
curl -s http://localhost:8080/health | python3 -m json.tool   # llama-server
curl -s http://localhost:8099/health | python3 -m json.tool   # geometric-lens
curl -s http://localhost:8070/health | python3 -m json.tool   # v3-service
curl -s http://localhost:30820/health | python3 -m json.tool  # sandbox
curl -s http://localhost:8090/health | python3 -m json.tool   # atlas-proxy

# 기능 테스트: 설치 전체 진단(서비스, 아티팩트, e2e 스모크)
atlas doctor
```

모든 헬스 엔드포인트는 `{"status": "ok"}` 또는 `{"status": "healthy"}`를 반환해야 합니다.

> **참고:** 대화형 터미널에서 그냥 `atlas`를 실행하면 전체 에이전트 루프(도구 호출, V3 파이프라인, 파일 읽기/쓰기)를 위한 Bubbletea TUI가 실행됩니다. TUI에는 실제 터미널이 필요합니다 — stdin/stdout이 파이프된 경우에는 `atlas doctor` 안내를 출력하고 종료합니다.

### 중지

```bash
docker compose down          # Stop all services (preserves images)
docker compose down --rmi all  # Stop and remove images (next start rebuilds)
```

### 로그 확인

```bash
docker compose logs -f llama-server    # Follow llama-server logs
docker compose logs -f geometric-lens  # Follow Lens logs
docker compose logs -f v3-service      # Follow V3 pipeline logs
docker compose logs -f atlas-proxy     # Follow proxy logs
docker compose logs -f sandbox         # Follow sandbox logs
docker compose logs --tail 50          # Last 50 lines from all services
```

### 업데이트

```bash
git pull
docker compose down
docker compose pull          # grab fresh :latest images from GHCR
docker compose up -d
```

### 제거

```bash
# Stop and remove the containers, network, and named volumes
docker compose down -v

# Remove the published images
docker images "ghcr.io/*/atlas-*" -q | xargs -r docker rmi

# Remove the CLI and TUI binaries
pip uninstall atlas
rm -f ~/.local/bin/atlas-tui
rm -rf ~/.cache/atlas-tui          # TUI session history

# The repo checkout, .env, and downloaded models live wherever you put
# them — delete the checkout and its models/ directory to reclaim the
# disk (models are the multi-GB part).
```

K3s 설치는 대신 `scripts/uninstall.sh`를 사용합니다. 이 스크립트는 매니페스트를 정리하고 (선택적으로) K3s 노드 자체도 제거합니다.

---

## 방법 2: 베어메탈

컨테이너 없이 모든 서비스를 로컬 프로세스로 실행합니다. 개발 환경이나 Docker를 사용할 수 없는 시스템에 유용합니다.

### 추가 사전 요구 사항

| 요구 사항 | 세부 내용 |
|-------------|---------|
| **Go 1.26.6+** | atlas-proxy와 atlas-tui 클라이언트 빌드용 (구형 Go 툴체인은 자동으로 이를 가져옵니다) |
| **llama.cpp** | CUDA로 소스 빌드 ([llama.cpp 빌드 안내](https://github.com/ggml-org/llama.cpp?tab=readme-ov-file#build) 참고) |
| **Node.js 20+** | 샌드박스의 JavaScript/TypeScript 실행에 필요 |
| **Rust** | 샌드박스의 Rust 실행에 필요 |

### 빌드

```bash
# 1. Clone and install Python CLI
git clone https://github.com/inferstep/ATLAS.git
cd ATLAS
pip install -e .

# 2. Select and install a registry model (or place a BYO GGUF in models/)
atlas model recommend
atlas model install <registry-name>

# 3. Build the proxy
cd proxy
go build -o ~/.local/bin/atlas-proxy-v2 .
cd ..

# 4. Install geometric-lens Python dependencies
pip install -r geometric-lens/requirements.txt

# 5. Install V3 service PyTorch (CPU only)
pip install torch --index-url https://download.pytorch.org/whl/cpu

# 6. Install sandbox dependencies
pip install -r sandbox/requirements-runtime.txt -r sandbox/requirements-verify.txt
```

### 서비스 시작

각 서비스를 별도의 터미널에서 시작합니다 (또는 `&`와 로그 파일 리다이렉션 사용):

```bash
# Terminal 1: llama-server (GPU)
llama-server \
  --model "models/$ATLAS_MODEL_FILE" \
  --host 0.0.0.0 --port 8080 \
  --ctx-size 32768 --n-gpu-layers 99 --no-mmap \
  --embeddings --pooling none --flash-attn on --fit off
# --pooling none: 렌즈의 스텝 단위 경로는 토큰별 벡터를 필요로 하며,
# 전체 텍스트 경로의 풀링과 정규화는 렌즈가 직접 수행합니다
# (Compose 기본값 ATLAS_EMBED_POOLING=none).

# Terminal 2: Geometric Lens (필수: 점수를 산출할 수 없는 동안 프록시가
# 요청을 거부합니다)
cd geometric-lens
LLAMA_URL=http://localhost:8080 \
LLAMA_EMBED_URL=http://localhost:8080 \
python -m uvicorn main:app --host 0.0.0.0 --port 8099

# Terminal 3: V3 Pipeline
cd v3-service
ATLAS_INFERENCE_URL=http://localhost:8080 \
ATLAS_LENS_URL=http://localhost:8099 \
ATLAS_SANDBOX_URL=http://localhost:8020 \
python main.py

# Terminal 4: Sandbox
cd sandbox
python executor_server.py

# Terminal 5: atlas-proxy
ATLAS_PROXY_PORT=8090 \
ATLAS_INFERENCE_URL=http://localhost:8080 \
ATLAS_LLAMA_URL=http://localhost:8080 \
ATLAS_LENS_URL=http://localhost:8099 \
ATLAS_SANDBOX_URL=http://localhost:8020 \
ATLAS_V3_URL=http://localhost:8070 \
ATLAS_MODEL_NAME="${ATLAS_MODEL_NAME:-local-model}" \
atlas-proxy-v2
```

> **참고:** 샌드박스는 베어메탈 모드에서 포트 **8020**에서 수신합니다 (Docker 포트 리매핑 없음). 프록시의 `ATLAS_SANDBOX_URL`은 30820이 아닌 8020 포트를 사용해야 합니다.

### TUI 실행

`atlas` 명령은 Python 패키지의 콘솔 엔트리포인트로, 빌드 단계의 `pip install -e .`로 설치됩니다 — 별도의 런처 스크립트는 필요하지 않습니다. 위의 서비스들이 실행 중인 상태에서:

```bash
cd /path/to/your/project
atlas    # Checks atlas-proxy is reachable, then launches the TUI
```

`atlas`는 `atlas-tui` 바이너리가 없거나 체크아웃보다 오래된 경우 `tui/`에서 자동으로 빌드하며(PATH에 Go 1.26.6+ 필요), TUI로 넘어가기 전에 localhost:8090의 프록시를 검증합니다.

---

## 방법 3: K3s

GPU 스케줄링, 헬스 프로브, 리소스 제한을 갖춘 Kubernetes 배포입니다. 프리뷰(Preview) — 템플릿은 CI에서 검증·렌더링되며, 자동화된 라이브 클러스터 테스트는 없습니다.

### 추가 사전 요구 사항

| 요구 사항 | 세부 내용 |
|-------------|---------|
| **K3s** | 단일 노드 또는 다중 노드 클러스터 |
| **NVIDIA GPU Operator** 또는 **device plugin** | GPU가 `nvidia.com/gpu` 리소스로 보여야 합니다 |
| **Helm** | GPU Operator 설치용 |
| **Podman 또는 Docker** | 컨테이너 이미지 빌드용 |

### 자동 설치

설치 스크립트가 K3s 설치, GPU Operator, 컨테이너 빌드, 배포까지 전체 설정을 처리합니다:

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf: model paths, GPU layers, context size, NodePorts

# 2. Run the installer (requires root)
sudo scripts/install.sh
```

설치 프로그램은 다음을 수행합니다:
1. 사전 요구 사항 확인 (NVIDIA 드라이버, GPU VRAM, 시스템 RAM)
2. K3s가 실행 중이 아니면 설치
3. GPU가 클러스터에 보이지 않으면 Helm을 통해 NVIDIA GPU Operator 설치
4. 컨테이너 이미지를 빌드하고 K3s containerd로 가져오기
5. `atlas.conf`에서 envsubst를 통해 매니페스트 생성
6. `atlas` 네임스페이스에 배포
7. 모든 서비스가 healthy가 될 때까지 대기

### 수동 배포

K3s가 이미 GPU 지원과 함께 실행 중인 경우:

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf

# 2. Build and import images
scripts/build-containers.sh

# 3. Generate manifests from atlas.conf
scripts/generate-manifests.sh

# 4. Deploy
kubectl apply -n atlas -f manifests/

# 5. Verify
scripts/verify-install.sh
```

### K3s 전용 설정

K3s는 설정에 `.env`가 아닌 `atlas.conf`를 사용합니다. HTTP 계약과 파이프라인 동작은 Docker Compose와 동일하며, 배포 배관만 다릅니다:

| 설정 항목 | Docker Compose | K3s |
|---------|---------------|-----|
| 설정 파일 | `.env` | `atlas.conf` |
| 서비스 노출 | 호스트 포트 (`8090`, `8080`, `8099`, `8070`, `30820`) | NodePorts (`30080`, `32735`, `31144`, `30070`, `30820`) |
| 프로젝트 워크스페이스 | 바인드 마운트 (`ATLAS_PROJECT_DIR` → `/workspace`) | `hostPath` (`ATLAS_PROJECTS_DIR` → 필요한 모든 Pod의 `/workspace`) |
| 모델 파일 | 바인드 마운트 (`ATLAS_MODELS_DIR` → `/models:ro`) | GPU 노드의 `hostPath` (`ATLAS_MODELS_DIR`, `Directory`, 읽기 전용) |
| 상태 저장 스토리지 | 명명된 볼륨 (`v3-telemetry`) | 없음 |
| GPU 할당 | `deploy.resources.reservations.devices` (nvidia) | `resources.limits.nvidia.com/gpu: 1` (GPU Operator 또는 디바이스 플러그인 필요) |
| 샌드박스 툴체인 캐시 | 언어별 `tmpfs` 마운트 | 언어별 `sizeLimit` 지정 `emptyDir` (공통 패턴, 동일 세트) |

모델·런타임 파라미터(`ATLAS_MAIN_MODEL`, `ATLAS_CONTEXT_LENGTH`, `ATLAS_PARALLEL_SLOTS`, `ATLAS_FLASH_ATTENTION`, KV 캐시 양자화, 렌즈 스코어링 경로용 `--embeddings`)는 두 모드 모두 동일한 환경 변수에서 읽습니다 — `atlas.conf.example`과 `.env.example`을 참고하세요.

전체 `atlas.conf` 레퍼런스는 [CONFIGURATION.md](../../CONFIGURATION.md)를 참고하세요.

### K3s 배포 확인

```bash
# Check pods
kubectl get pods -n atlas

# Check GPU allocation
kubectl describe nodes | grep nvidia.com/gpu

# Run verification suite
scripts/verify-install.sh
```

> **참고:** Docker Compose가 가장 많이 검증된 배포 방법입니다(CI가 이에 대해 실행되며, 모든 릴리스는 Compose로 스모크 테스트됩니다). K3s 매니페스트는 배포 시점에 `templates/*.yaml.tmpl`로부터 `scripts/generate-manifests.sh`(또는 `install.sh`의 `process_templates` 단계)를 통해 생성됩니다. 템플릿은 `atlas.conf`에서 선택된 모델을 사용하며, CHANGELOG의 벤치마크 수치는 각자의 동결된 모델/구성을 기록합니다.

---

## 하드웨어 사이징

ATLAS는 GPU를 5개 등급으로 분류하고 등급별로 레지스트리 모델 + 컨텍스트 크기 + 병렬 슬롯 구성을 추천합니다. 이는 현재의 레지스트리 권장 사항이지 하드코딩된 런타임 요구 사항이 아닙니다. `atlas tier`를 실행하면 본인 하드웨어가 어느 등급인지와 사용할 정확한 `.env` 값을 볼 수 있습니다.

| 등급 | VRAM | 권장 모델 | 컨텍스트 | 슬롯 | GPU 예시 |
|------|------|-------------------|--------:|------:|--------------|
| **cpu** | 해당 없음 | [CPU 전용 설치](#cpu-only) — 프리뷰(Preview), 스모크 테스트 전용 | 해당 없음 | 해당 없음 | (GPU 없음) |
| **small** | 8–12 GB | Qwen3.5 7B Q4_K_M (4.4 GB) | 8K | 1 | RTX 3060/4060 8GB, T4 |
| **medium** | 12–20 GB | Qwen3.5 9B Q6_K (6.9 GB) | 32K | 1 | RTX 4060/5060 Ti 16GB, 3080 Ti, 4070 Ti Super |
| **large** | 20–32 GB | Qwen3.5 14B Q5_K_M (10.5 GB) | 32K | 2 | RTX 3090, 4090, 5090 24GB |
| **xlarge** | 32 GB+ | Qwen3.5 32B Q5_K_M (23 GB) | 64K | 2 | RTX 5090 32GB, A6000, A100, H100 |

```bash
atlas tier              # classify this host + show recommendations
atlas tier list         # show all 5 tier definitions
atlas tier fit          # size the runtime for the CONFIGURED model + GPU
atlas tier --json       # machine output (for scripts)
atlas tier --raw        # just the probe (no classification)
```

등급 표는 VRAM 구간별 출발점을 제공합니다. **`atlas tier fit`**은 실제로 실행하는 *특정* 모델에 맞게 이를 다듬습니다 — GGUF의 KV 기하 구조와 GPU의 VRAM을 읽어 완전히 GPU 안에 머무는 최대 컨텍스트를 계산합니다(`atlas tier fit --write`는 결과를 `.env`에 적용). `ATLAS_MODEL_FILE`이나 GPU를 바꿀 때마다 실행하세요. [CLI.md § atlas tier fit](../../CLI.md#atlas-tier-fit)과, 다운로드 전 사이징 안내는 [TROUBLESHOOTING.md § 내 GPU에는 무엇이 들어가는가?](../ko/TROUBLESHOOTING.md#내-gpu에는-무엇이-들어가는가)를 참고하세요.

medium 등급이 ATLAS 개발 타깃입니다 — `atlas-bootstrap.sh`는 그 모델+컨텍스트 설정을 기본값으로 합니다. 다른 등급에서는 부트스트랩 완료 후 **`atlas init`**(첫 실행 마법사)을 실행하세요. `atlas tier`로 하드웨어를 탐침하고, 레지스트리에서 알맞은 모델을 골라 SHA 검증과 함께 다운로드하고, `.env`를 다시 씁니다. 하드웨어나 레지스트리 기본 모델이 바뀔 때마다 `atlas init --reconfigure`로 다시 실행하세요. 마법사 실행 후에는 `atlas tier fit --write`가 마법사의 등급 수준 기본값을 선택된 모델에 맞게 조입니다.

| 리소스 | 최소 | 권장 | 비고 |
|----------|---------|-------------|-------|
| GPU VRAM | 8 GB | 16 GB | 위의 등급 표 참고 |
| 시스템 RAM | 14 GB | 16 GB+ | PyTorch 런타임 + 컨테이너 오버헤드 |
| 디스크 | 15 GB | 25 GB | 모델(등급에 따라 4.4–23 GB) + 컨테이너 이미지(5–8 GB) + 작업 공간 |
| CPU | 4 코어 | 8+ 코어 | V3 파이프라인은 수리 단계에서 CPU 집약적 |

### 지원 GPU

VRAM 8GB 이상에 llama.cpp가 지원하는 백엔드를 갖춘 모든 GPU:

| 벤더 | 백엔드 | 상태 | 빌드 경로 | 테스트된 카드 |
|---|---|---|---|---|
| NVIDIA (Blackwell — RTX 50xx, B100, GB10) | CUDA | 지원(Supported) (게시된 이미지) | `inference/Dockerfile.v31` | RTX 5060 Ti 16GB (주 개발 GPU) |
| NVIDIA (Blackwell 이전 — RTX 20xx–40xx, GTX 10xx, V100/A100/H100/T4/L4) | CUDA | 프리뷰(Preview) — 일회성 [로컬 재빌드 필요](#cuda-컴퓨트-캐퍼빌리티-dockerfilev31) | `inference/Dockerfile.v31` + `--build-arg CUDA_ARCH=<cc>` | — (업스트림 llama.cpp는 이들을 지원; ATLAS에서의 메인테이너 검증은 없음) |
| AMD | ROCm / HIP | 커뮤니티 검증(Community-tested) | `inference/Dockerfile.rocm` | RX 7900 XTX (커뮤니티 스모크 테스트, [GH #26](https://github.com/inferstep/ATLAS/issues/26)) |
| Apple Silicon | Metal | 지원 (macOS 하이브리드: 네이티브 llama-server + Docker, [#32](https://github.com/inferstep/ATLAS/issues/32)) | `scripts/atlas-setup-macos.sh` + `docker-compose.macos.yml` | M2 Pro 32GB (검증됨); M3/M4 (목표) |
| 모든 벤더 (크로스 벤더 폴백) | Vulkan | 프리뷰 | `inference/Dockerfile.vulkan` | lavapipe (CPU ICD) 스모크 테스트됨; 실제 GPU 검증은 아직 없음 |
| Intel Arc | SYCL | 로드맵(Roadmap) — Intel Arc는 현재 Vulkan 사용 | 미정 | Arc A770 16GB (목표) |

`atlas tier`는 벤더 전반을 자동 감지해 VRAM이 가장 큰 GPU를 고릅니다. GPU가 여러 장이고 특정 GPU를 원하면 `ATLAS_GPU_VENDOR=amd` 또는 `ATLAS_GPU_INDEX=1`로 재정의하세요.

#### CUDA 컴퓨트 캐퍼빌리티 (Dockerfile.v31)

`inference/Dockerfile.v31`은 특정 CUDA 컴퓨트 캐퍼빌리티에 맞춰 llama.cpp를 컴파일합니다. 기본값 — 그리고 GHCR에 게시된 `atlas-llama` 이미지가 빌드된 값 — 은 `120;121`(Blackwell: RTX 50xx, B100, GB10) **전용**입니다. 게시된 이미지에는 그보다 이전 GPU용 커널이 없고, 내장된 PTX도 하위 방향으로 JIT 컴파일될 수 없으므로, RTX 20/30/40 시리즈, GTX 10xx, Blackwell 이전 데이터센터 카드(V100/A100/H100/T4/L4)에서는 llama-server가 시작 시 `no kernel image is available for execution on the device`로 실패합니다. 본인 아키텍처에 맞춰 추론 이미지를 한 번 재빌드해야 합니다. (잘못된 아치 값으로 로컬 빌드하면 그보다 이른 시점에 `nvcc fatal: unsupported gpu architecture`로 실패합니다.)

본인 GPU의 아치를 확인한 뒤 `--build-arg CUDA_ARCH=<value>`로 재빌드하세요:

```bash
# Your GPU's compute capability (drop the dot: 8.9 -> 89)
nvidia-smi --query-gpu=compute_cap --format=csv,noheader

# Compose-native rebuild — only llama-server is rebuilt, the other
# services keep using the GHCR images (~30-75 min, one time):
docker compose build --build-arg CUDA_ARCH=89 llama-server
docker compose up -d --no-deps llama-server

# Or build the image directly:
podman build --build-arg CUDA_ARCH=89 -f inference/Dockerfile.v31 -t llama-server:local inference/

# Multiple archs (semicolon-separated) — build a fat binary for Ampere + Ada + Hopper
docker compose build --build-arg CUDA_ARCH="86;89;90" llama-server
```

일반적인 값:

| 아치 | 아키텍처 | 카드 |
|------|--------------|-------|
| `60`, `61` | Pascal | GTX 10xx, Tesla P4/P40 |
| `70` | Volta | V100 |
| `75` | Turing | RTX 20xx, T4 |
| `80`, `86` | Ampere | A100, RTX 30xx |
| `89` | Ada Lovelace | RTX 40xx, L4 |
| `90` | Hopper | H100 |
| `100`, `120`, `121` | Blackwell | B100, RTX 50xx |

#### AMD GPU 타깃 (Dockerfile.rocm)

`inference/Dockerfile.rocm`은 하나 이상의 `gfx` 타깃에 맞춰 llama.cpp의 HIP 백엔드를 컴파일합니다. 기본값은 가장 흔한 소비자용 + 데이터센터 AMD GPU를 포괄하는 fat 빌드입니다: `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`. 타깃이 하나 늘 때마다 바이너리가 ~150MB 커집니다.

빌드 시 `--build-arg GFX_TARGET=<value>`로 재정의하세요(또는 compose 오버라이드가 전달하는 `ATLAS_GFX_TARGET` 환경 변수 사용):

```bash
# Single target — RX 7900 XT/XTX only (smaller image)
ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server

# Two targets for RDNA3 + RDNA2 mixed-fleet
docker build --build-arg GFX_TARGET="gfx1100;gfx1030" -f inference/Dockerfile.rocm -t atlas-llama-rocm:custom inference/
```

일반적인 값:

| 타깃 | 아키텍처 | 카드 |
|--------|--------------|-------|
| `gfx1100` | RDNA3 (Navi 31) | RX 7900 XT, 7900 XTX, 7900 GRE |
| `gfx1101` | RDNA3 (Navi 32) | RX 7800 XT, 7700 XT |
| `gfx1102` | RDNA3 (Navi 33) | RX 7600, 7600 XT |
| `gfx1030` | RDNA2 (Navi 21) | RX 6800, 6800 XT, 6900 XT, 6950 XT |
| `gfx1031` | RDNA2 (Navi 22) | RX 6700 XT, 6750 XT |
| `gfx1032` | RDNA2 (Navi 23) | RX 6600, 6600 XT, 6650 XT |
| `gfx90a` | CDNA2 | MI210, MI250, MI250X |
| `gfx942` | CDNA3 | MI300X |
| `gfx900` | Vega | Vega 56/64 (HSA 오버라이드가 필요할 수 있음 — TROUBLESHOOTING.md 참고) |
| `gfx1200` | RDNA4 (Navi 44) | RX 9070 |
| `gfx1201` | RDNA4 (Navi 48) | RX 9070 XT |

> **RDNA4 (gfx1200/gfx1201) 사용자:** `ATLAS_ROCM_TAG=7.2.3-complete`를 설정하세요 — 기본 ROCm 6.2 베이스 이미지에는 gfx1200/gfx1201 컴파일러 지원이 없습니다. ROCm 7.0+는 이 타깃들을 네이티브로 지원하므로 `ATLAS_HSA_OVERRIDE_GFX_VERSION`을 설정하지 마세요. 자세한 내용은 [TROUBLESHOOTING.md § RDNA4](../ko/TROUBLESHOOTING.md)를 참고하세요.

본인 GPU의 gfx 타깃: `rocminfo | grep -i gfx | head -1` (또는 [LLVM AMDGPU 프로세서 표](https://llvm.org/docs/AMDGPUUsage.html)에서 조회).

---

## Geometric Lens 가중치 (필수)

ATLAS는 모든 요청에 Geometric Lens를 사용합니다([ADR 0011](../../adr/0011-the-lens-is-required.md)). 렌즈에는 서빙 중인 모델에 맞는 C(x)·G(x) 가중치가 필요합니다. 가중치가 없으면 프록시는 각 요청을 HTTP 503 `dependency_down`으로 거부하면서 그 이유를 알려주고, `atlas doctor`는 실패합니다. 가중치는 있지만 아직 보정되지 않은 렌즈는 허용됩니다: 점수는 산출되고, 임계값 개입만 보정 이후로 미뤄집니다.

[SUPPORT_MATRIX.md](../../../SUPPORT_MATRIX.md)에 렌즈 아티팩트가 표기된 모델은 `atlas model install-artifacts`로 받을 수 있습니다. 사전 학습된 가중치와 학습 데이터도 HuggingFace에서 제공됩니다:

**[HuggingFace의 ATLAS 데이터셋](https://huggingface.co/datasets/itigges22/ATLAS)** — 임베딩, 학습 데이터, 가중치 파일이 포함되어 있습니다.

가중치 파일을 `geometric-lens/geometric_lens/models/`에 배치하거나 Docker Compose에서 `ATLAS_LENS_MODELS`를 통해 마운트하세요. 서비스가 시작 시 자동으로 로드합니다.

가중치가 없는 모델은 자체 벤치마크 데이터로 가중치를 만드세요. `atlas bench`는 프록시를 거치지 않고 V3 단계를 직접 실행하므로, 렌즈에 가중치가 없는 상태에서도 동작합니다. 전체 루프가 CLI로 완결됩니다:

```bash
atlas bench --run-id mymodel_lens --tasks 200    # 후보 생성 및 셀프 라벨링
atlas lens build --force --from-results benchmark/results/mymodel_lens/v3_lcb/per_task
```

`atlas lens build`는 렌즈의 두 절반을 모두 학습시키고 임계값을 보정한 뒤, 활성화된 번들에 `provenance.json` 매니페스트를 기록합니다. [CLI.md § atlas lens](../../CLI.md#atlas-lens)를 참조하세요.

### 자체 llama-server 반입하기

추론을 직접 운영하려면 — 호스트 네이티브 빌드, 원격 머신, 또는 라우터 모드의 llama.cpp —
이를 사용하는 네 개 서비스가 그쪽을 바라보게 하고 번들된 컨테이너는 스텁으로 바꾸세요.
`docker-compose.override.yml`을 만듭니다:

```yaml
services:
  llama-server:                      # stub the bundled one out
    command: ["sleep", "infinity"]
    entrypoint: []
    healthcheck:
      test: ["CMD", "true"]
    deploy: {}
  geometric-lens:
    environment:
      LLAMA_URL: http://host.docker.internal:8080
  v3-service:
    environment:
      LLAMA_URL: http://host.docker.internal:8080
  atlas-proxy:
    environment:
      ATLAS_LLAMA_URL: http://host.docker.internal:8080
```

`host.docker.internal`은 macOS뿐 아니라 Linux에서도 해석됩니다: compose 파일이 이를 필요로 하는
모든 서비스에 `extra_hosts: host.docker.internal:host-gateway`로 매핑해 둡니다. docker0 게이트웨이
주소(`172.17.0.1`)를 직접 쓸 필요는 없습니다. 그 매핑이 결국 가리키는 주소가 그것이긴 합니다.

**라우터 모드.** `llama-server --models-preset`은 `model` 필드가 없는 요청을 모두 거부합니다
(`model name is missing from the request`). `.env`의 `ATLAS_MODEL_NAME`을 서빙 중인 모델 이름으로
설정하세요 — `atlas doctor`의 스모크 테스트가 이를 함께 보내며, 단일 모델 서버는 이를 무시합니다.

**첫 `docker compose up` 전에** secrets 디렉터리가 존재하는지 확인하세요 — 없으면 바인드 마운트가
실패합니다:

```bash
mkdir -p secrets     # or set ATLAS_SECRETS_DIR
```

`atlas init`이 대신 만들어 주므로, compose를 먼저 띄우는 수동 흐름에서만 필요합니다.

### 자체 모델 반입하기

기본이 아닌 GGUF를 갈아 끼우고 싶다면, `atlas lens` 하위 명령이 탐침 + 학습 파이프라인을 감싸므로 밑단 스크립트를 배울 필요가 없습니다:

```bash
# 1. Drop your GGUF in models/ and update .env to point at it, restart llama-server.

# 2. Probe whether the existing artifacts can score it (cheap, no training):
atlas lens check
# Reports: compat (artifacts work) | needs-build (different dim) | incompatible

# 3. If 'needs-build', train fresh artifacts at the model's native embedding dim:
atlas lens build --samples path/to/labeled.json
# samples format: [{"text": str, "label": 0|1}, ...] where 1 = passing code
# Canonical training set: huggingface.co/datasets/itigges22/ATLAS

# 4. Re-run check — should now report compat:
atlas lens check
```

전체 레퍼런스: [CLI.md § atlas lens](../../CLI.md#atlas-lens).

---

## ASA 스티어링 벡터 (자동 빌드)

2026년 5월 BiasBusters #4. 함수/클래스/요소 전체 재작성에서 모델이 `edit_file` 대신 `structural_edit`를 선호하도록 편향시키는 residual-stream 스티어링 벡터로, 문법 게이트가 무언가를 거부할 기회를 갖기 **전에** 적용됩니다. 엄밀히 선택 사항입니다 — ATLAS는 이것 없이도 계속 동작하며, 도구 선택 편향이 스티어링되지 않을 뿐입니다.

`atlas-bootstrap.sh`가 서비스 기동 후 자동으로 빌드합니다. 파이프라인은 다음과 같습니다:

1. `build_cvector_prompts.py`가 커밋된 `geometric-lens/asa_calibration/contrast_pairs.jsonl`(1000쌍)을 양성/음성 프롬프트 파일로 변환합니다.
2. 부트스트랩이 `llama-server`를 잠시 중지하고, `--method mean -ngl 99` 옵션으로 `llama-cvector-generator`를 원샷 컨테이너로 실행하여 `models/ast_edit_steering.gguf`와, 벡터가 어느 모델에 대해 빌드되었는지 기록하는 `models/ast_edit_steering.gguf.model` 사이드카 마커를 쓴 뒤, `llama-server`를 재시작합니다.
3. `inference/entrypoint-v3.1.sh`가 다음 시작 시 파일을 발견하면, `.model` 사이드카 마커가 선택된 모델과 일치하는지 확인하고 `llama-server` 명령줄에 `--control-vector-scaled /models/ast_edit_steering.gguf:0.5`를 덧붙입니다. 마커가 없거나 다른 모델을 지명하는 벡터는 **비활성화** 상태로 유지됩니다(시작 배너가 이유를 보고합니다) — 벡터는 한 모델에 묶인 residual-space 아티팩트입니다.

16GB GPU 기준 총 소요 시간: ~5분. 빌드는 모델이 있는 하드웨어에서 실행되며, 결과 벡터는 모델 전용입니다(한 모델의 아티팩트에 대해 빌드된 `ast_edit_steering.gguf`를 다른 베이스 모델을 돌리는 호스트로 옮기지 마세요).

**동작 재정의** (튜닝하려면 `.env`에 설정):

| 환경 변수 | 기본값 | 효과 |
|---|---|---|
| `ATLAS_CONTROL_VECTOR` | `/models/ast_edit_steering.gguf` | 경로 재정의 |
| `ATLAS_CONTROL_VECTOR_SCALE` | `0.5` | 보수적인 값. 편향이 너무 미미하면 1.0–1.5로 올리고, 도구 외 작업이 저하되면 0.2 쪽으로 낮추세요. |
| `ATLAS_CONTROL_VECTOR_LAYER_RANGE` | (전체 레이어) | 두 정수를 전달(예: `"24 30"`)해 레이어 대역으로 범위를 한정. 좁을수록 안전하지만 약해집니다. |
| `ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED` | `0` | `1`로 설정하면 `.model` 사이드카 마커가 없거나 선택된 모델과 불일치해도 벡터를 적용. 직접 빌드해 일치함을 아는 벡터에만 사용하세요. |

**로컬 빌드가 실패하면**(예: 구형 `atlas-llama` 이미지에 cvector-generator 부재, GPU OOM, 런타임 pull 중 네트워크 문제) 부트스트랩은 [ATLAS HuggingFace 데이터셋](https://huggingface.co/datasets/itigges22/ATLAS)에서 사전 빌드된 `ast_edit_steering.gguf`를 다운로드하는 것으로 폴백합니다. 그것도 실패하면 설치는 경고와 함께 완료됩니다 — `atlas doctor`가 그 공백을 `fail`이 아닌 `warn`으로 표시합니다.

빌드를 완전히 건너뛰려면 설치 프로그램 실행 전에 `ATLAS_BOOTSTRAP_SKIP_ASA=1`을 설정하세요.

수동 재빌드(재선별된 쌍, 다른 `--method`, 다른 베이스 모델)는 [`geometric-lens/asa_calibration/README.md`](../../../geometric-lens/asa_calibration/README.md)를 참고하세요.

---

## 다음 단계

- [CLI.md](../../CLI.md) — 실행 후 ATLAS 사용 방법
- [CONFIGURATION.md](../../CONFIGURATION.md) — 모든 환경 변수 및 튜닝 옵션
- [TROUBLESHOOTING.md](../ko/TROUBLESHOOTING.md) — 일반적인 문제 및 해결 방법
- [ARCHITECTURE.md](../ko/ARCHITECTURE.md) — 시스템 내부 동작 원리
