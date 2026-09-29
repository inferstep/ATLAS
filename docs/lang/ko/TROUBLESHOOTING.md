<!-- source: docs/TROUBLESHOOTING.md synced-through: 6ff5ecb -->
> **[English](../../TROUBLESHOOTING.md)** | **[简体中文](../zh-CN/TROUBLESHOOTING.md)** | **[日本語](../ja/TROUBLESHOOTING.md)** | **한국어**

> ℹ️ 영어 원본([TROUBLESHOOTING.md](../../TROUBLESHOOTING.md))의 번역본입니다. 원본과 차이가 있을 경우 영어 원본이 우선합니다.


# ATLAS 문제 해결 가이드

일반적인 문제와 해결 방법을 서비스별로 정리했습니다.

---

## 빠른 진단

먼저 `atlas doctor`를 실행한 다음 Compose 상태와 로그로 문제 위치를 찾으세요:

```bash
atlas doctor
docker compose ps
docker compose logs --tail 50
```

`atlas doctor`는 호스트, 설정, 서비스 상태를 함께 확인하므로 가장 먼저 사용할 진단 명령입니다. `docker compose ps`는 시작에 실패한 서비스를 식별하고, 로그는 다음 단계의 세부 정보를 제공합니다. 첫 결과가 추론 백엔드를 가리킬 때만 하드웨어별 검사를 사용하세요:

| 백엔드 | 진단 명령 또는 확인 항목 |
|---|---|
| NVIDIA CUDA | `nvidia-smi` |
| AMD ROCm | `rocm-smi`를 실행하고 `/dev/kfd`가 존재하는지 확인합니다. |
| Apple Silicon / Metal | `atlas doctor`를 실행합니다. 네이티브 서버가 수신 대기 중이 아니면 [SETUP_MACOS.md](../../SETUP_MACOS.md#troubleshooting)의 설명대로 `./scripts/atlas-llama-macos.sh`를 시작하고 포그라운드 런처 출력을 확인합니다. |
| Vulkan | `/dev/dri`가 존재하는지 확인한 다음 `docker compose -f docker-compose.yml -f docker-compose.vulkan.yml exec llama-server vulkaninfo --summary`를 실행합니다. |
| CPU 전용 | `docker-compose.vulkan.yml`과 `docker-compose.cpu.yml` 오버레이가 활성화되어 있는지 확인합니다. GPU가 없다는 이유만으로 `atlas doctor`가 실패하지 않고 경고 후 정상 종료해야 합니다. |

서비스별 헬스 체크 curl은 [SETUP.md § 설치 확인](../ko/SETUP.md#설치-확인)을 참고하세요. 분류(triage)에 가장 유용한 것은 atlas-proxy 헬스 엔드포인트입니다 — 모든 업스트림 서비스의 상태를 보고합니다:
```json
{
  "status": "ok",
  "inference": true,
  "lens": true,
  "lens_ready": true,
  "sandbox": true,
  "port": "8090",
  "capabilities": ["demo_raw_completion_v1"],
  "stats": { "requests": 0, "repairs": 0, "sandbox_passes": 0, "sandbox_fails": 0 }
}
```

어떤 필드라도 `false`이면 해당 서비스가 문제입니다. `inference`, `lens`, `lens_ready`, `sandbox` 중 하나라도 false이면 `status`가 `"degraded"`로 바뀝니다. `lens`는 렌즈가 응답을 하기는 하는지를 나타냅니다. `lens_ready`는 프록시가 모든 요청에 적용하는 것과 동일한 검사로, 렌즈가 점수를 산출할 수 있는지를 나타냅니다: `false`이면 `lens_reason`이 그 이유를 알려주며, `true`가 될 때까지 모든 요청이 거부됩니다([요청 거부: 렌즈가 점수를 산출할 수 없음](#요청-거부-렌즈가-점수를-산출할-수-없음) 참고).

---

## 오류로 찾기

정확한 오류 문자열과 증상을 해당 항목에 매핑했습니다.

| 보이는 증상 | 이동 |
|---|---|
| `no kernel image is available for execution on the device` — NVIDIA GPU | [`no kernel image is available for execution on the device` (CUDA)](#no-kernel-image-is-available-for-execution-on-the-device-cuda) |
| `no kernel image is available for execution on the device` — AMD GPU | [ROCm이 "미지원"이라는 AMD GPU에서…](#rocm이-미지원이라는-amd-gpu에서-그래도-시도해-보고-싶을-때-rocm의-no-kernel-image) |
| `invalid device function` 또는 `nvcc fatal: unsupported gpu architecture` | [`no kernel image…` (CUDA)](#no-kernel-image-is-available-for-execution-on-the-device-cuda) |
| `libnvidia-ml.so.1: cannot open shared object file` | [libnvidia-ml.so.1 항목](#libnvidia-mlso1-cannot-open-shared-object-file) |
| 컨테이너에서 GPU가 보이지 않음; 모델이 CPU에서 실행됨 | [컨테이너에서 GPU가 감지되지 않음](#컨테이너에서-gpu가-감지되지-않음) |
| `/dev/kfd: no such file or directory` | [AMD GPU가 감지되지 않음 (ROCm)](#amd-gpu가-감지되지-않음-rocm) |
| `/dev/kfd`에 대한 `Permission denied` | [AMD GPU는 감지되는데 Docker가 접근하지 못함](#amd-gpu는-감지되는데-docker가-접근하지-못함) |
| `error: AMDGPU target 'gfx1201' is not supported` | [RDNA4 — ROCm 7.x 필요](#rdna4-rx-9070--9070-xt-gfx1200--gfx1201--rocm-7x-필요) |
| ROCm 이미지 pull이 타임아웃 / 속도 제한에 걸림 | [ROCm 컨테이너가 `rocm/rocm-terminal`을 pull하지 못함](#rocm-컨테이너가-rocmrocm-terminal을-pull하지-못함) |
| `docker compose build`가 CUDA 오류로 실패 | [첫 빌드 실패 (CUDA를 찾을 수 없음)](#첫-빌드-실패-cuda를-찾을-수-없음) |
| `RPC failed; curl 56` / `early EOF` / `fetch-pack: invalid index-pack output` | [llama.cpp 클론 타임아웃](#llamacpp-클론-타임아웃) |
| `error loading model: unknown (model) architecture '…'` | [llama.cpp 재빌드](#llamacpp-재빌드-새-모델-아키텍처-또는-패치-드리프트) |
| `error: patch failed:` / `patch does not apply` | [llama.cpp 재빌드](#llamacpp-재빌드-새-모델-아키텍처-또는-패치-드리프트) |
| 마운트된 볼륨 / 모델 파일에 대한 권한 거부 (Fedora/RHEL) | [SELinux가 컨테이너 접근을 차단함](#selinux가-컨테이너-접근을-차단함-fedorarhel) |
| 프록시 헬스에 `"sandbox": false` (수동 컨테이너 설정) | [Sandbox에 연결할 수 없음](#sandbox에-연결할-수-없음) |
| 프록시 헬스에 `"sandbox": false` (Compose 스택) | [Sandbox에 연결할 수 없음 (헬스 체크)](#sandbox에-연결할-수-없음-헬스-체크) |
| 시작 시 `address already in use` | [포트 충돌](#포트-충돌) |
| "fitting params to device memory" 이후 CUDA 할당 오류 | [모델 + KV 캐시가 GPU에 들어가지 않음](#모델--kv-캐시가-gpu에-들어가지-않음-시작-실패-또는-생성이-5배-느림) |
| 생성이 ~2 tok/s, llama-server가 CPU 코어를 소모 | [모델 + KV 캐시가 GPU에 들어가지 않음](#모델--kv-캐시가-gpu에-들어가지-않음-시작-실패-또는-생성이-5배-느림) / [느린 생성 속도](#느린-생성-속도-2-toks) |
| 다운로드 전 모델 사이징 | [내 GPU에는 무엇이 들어가는가?](#내-gpu에는-무엇이-들어가는가) |
| 시작 시 `failed to load model` | [모델 파일을 찾을 수 없음](#모델-파일을-찾을-수-없음) |
| llama-server 크래시 / OOMKilled, VRAM이 거의 100% | [VRAM 부족](#vram-부족) |
| 모델이 JSON 도구 호출 대신 `<think>` 태그나 산문을 출력 | [문법이 강제되지 않음](#문법이-강제되지-않음-모델이-사고-블록을-출력함) |
| Gemma가 아무것도 하지 않고 `done`만 반복 방출 | [문법이 강제되지 않음](#문법이-강제되지-않음-모델이-사고-블록을-출력함) |
| 도구 호출에서 `unexpected end of JSON` | [컨텍스트 윈도우가 너무 작음](#컨텍스트-윈도우가-너무-작음) / [잘림 오류](#잘림-오류-write_file이-반복적으로-실패) |
| 도구 호출도 V3도 없음 — 요청이 그대로 통과 | [에이전트 루프가 활성화되지 않음](#에이전트-루프가-활성화되지-않음) |
| 쓰기/편집이 V3 단계를 전혀 유발하지 않음 | [V3 파이프라인이 기능 파일에서 실행되지 않음](#v3-파이프라인이-기능-파일에서-실행되지-않음) |
| `Your output was truncated — the content is too long for a single tool call` | [잘림 오류](#잘림-오류-write_file이-반복적으로-실패) |
| `Tool call was truncated (output too long for context window)` | [잘림 오류](#잘림-오류-write_file이-반복적으로-실패) |
| 도구 결과와 다음 동작 사이 ~30초의 공백 | [도구 결과와 다음 동작 사이의 긴 정지](#도구-결과와-다음-동작-사이의-긴-정지) |
| 수정이 검증된 후에도 에이전트가 계속 편집함 | [V3가 이미 수정을 확인했는데 모델이 계속 편집함](#v3가-이미-수정을-확인했는데-모델이-계속-편집함) |
| 첫 도구 호출이 여기 존재하지 않는 파일을 읽음 | [모델이 이전 세션의 파일명을 환각함](#모델이-이전-세션의-파일명을-환각함) |
| V3 검증 중에만 `ModuleNotFoundError` | [다중 파일 프로젝트: 샌드박스 `ModuleNotFoundError`](#다중-파일-프로젝트-샌드박스-modulenotfounderror) |
| `_curses.error: addwstr() returned ERR` | [Curses 하단 행 `addwstr() returned ERR`](#curses-하단-행-addwstr-returned-err) |
| HTML/CSS/JSON 파일 작성 시 ~5분의 정지 | [비 Python 파일에서 V3가 수 분간 멈춤](#비-python-파일에서-v3가-수-분간-멈춤) |
| 짧은 후속 요청("ok", "yes")이 동작 대신 채팅을 받음 | ["다시 고쳐줘" 프롬프트에서 V3 파이프라인이 실행되지 않음](#다시-고쳐줘-프롬프트에서-v3-파이프라인이-실행되지-않음) |
| `file not read yet — use read_file first before editing` | [편집 전에 파일을 읽지 않음](#편집-전에-파일을-읽지-않음) |
| `file modified since last read — read it again before editing` | [외부에서 파일이 수정됨](#외부에서-파일이-수정됨) |
| `You have full project context in the system prompt. Do not read more files.` | [탐색 예산 경고](#탐색-예산-경고) |
| "ATLAS needs the geometric lens for every request" / 실행이 `lens_unavailable`로 끝남 / `"lens_ready": false` | [요청 거부: 렌즈가 점수를 산출할 수 없음](#요청-거부-렌즈가-점수를-산출할-수-없음) |
| "No gx_thresholds.json — Lens scores are uncalibrated" | [렌즈 미보정](#렌즈-미보정) |
| lens 로그에 "embedding extraction failed" | [임베딩 추출 실패](#임베딩-추출-실패) |
| 샌드박스가 `"error_type": "Timeout"`을 반환 | [코드 실행 타임아웃](#코드-실행-타임아웃) |
| 특정 언어에서 샌드박스 오류 | [지원되지 않는 언어](#지원되지-않는-언어) |
| `--tasks`보다 작은 `LIMITED MODE: running N tasks` | [bench가 요청보다 적은 태스크만 실행함](#bench가-요청보다-적은-태스크만-실행함-limited-mode-running-n-tasks의-n이---tasks보다-작음) |
| 시스템이 느려지고 서비스가 OOMKilled됨 | [높은 RAM 사용량](#높은-ram-사용량) |

---

## Docker / Podman 문제

### 컨테이너에서 GPU가 감지되지 않음

**증상:** llama-server 컨테이너는 시작되지만 모델이 CPU에서 로드됩니다(매우 느림, ~2 tok/s). 호스트에서는 `nvidia-smi`에 GPU가 보이지만 컨테이너에서는 보이지 않습니다.

**해결:** NVIDIA Container Toolkit을 설치하세요:

```bash
# RHEL/Fedora
sudo dnf install nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=podman
sudo systemctl restart podman

# Ubuntu/Debian
sudo apt install nvidia-container-toolkit
sudo nvidia-ctk runtime configure --runtime=docker
sudo systemctl restart docker
```

컨테이너 안에서 GPU가 보이는지 확인:
```bash
# Docker
docker run --rm --gpus all nvidia/cuda:12.0.0-base-ubuntu22.04 nvidia-smi

# Podman
podman run --rm --device nvidia.com/gpu=all nvidia/cuda:12.0.0-base-ubuntu22.04 nvidia-smi
```

### `libnvidia-ml.so.1: cannot open shared object file`

**증상:** `docker compose up` 중 llama-server가 다음과 같이 실패합니다:

```
nvidia-container-cli: initialization error: load library failed:
libnvidia-ml.so.1: cannot open shared object file: no such file or directory
```

**의미:** 호스트에 NVIDIA *커널 모듈*은 있지만(`nvidia-smi`는 동작), *유저스페이스 드라이버 라이브러리*가 컨테이너 툴킷이 기대하는 위치에 없습니다. RHEL/Rocky/Alma 최소 설치에서는 `nvidia-driver-cuda-libs` 패키지가 기본으로 설치되지 않고, Debian/Ubuntu에서는 보통 드라이버 업그레이드 후 오래된 `ldconfig` 캐시가 문제입니다.

**해결 순서** — 순서대로 시도하고, `docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi`가 동작하면 멈추세요:

1. **ldconfig 갱신 + docker 재시작:**
   ```bash
   sudo ldconfig
   sudo systemctl restart docker
   ```

2. **RHEL 9 — CUDA 저장소 추가 + open-dkms 모듈 설치** (RTX 5060 Ti의 RHEL 9.7에서 동작 확인):
   ```bash
   # Add NVIDIA's CUDA repo
   sudo dnf config-manager --add-repo \
     https://developer.download.nvidia.com/compute/cuda/repos/rhel9/x86_64/cuda-rhel9.repo

   # Enable CodeReady Builder (provides dkms / kernel-devel)
   sudo subscription-manager repos --enable=codeready-builder-for-rhel-9-x86_64-rpms

   # Make sure EPEL is present
   sudo dnf install -y https://dl.fedoraproject.org/pub/epel/epel-release-latest-9.noarch.rpm

   # Install the open driver module (REQUIRED for Blackwell — RTX 50xx)
   sudo dnf module install -y nvidia-driver:open-dkms

   sudo ldconfig && sudo systemctl restart docker
   ```

   **Rocky/Alma/CentOS Stream 9** — 위와 동일하되, `subscription-manager` 줄을 다음으로 교체:
   ```bash
   sudo dnf config-manager --set-enabled crb
   ```

   > 참고: `nvidia-driver-cuda-libs` 패키지는 NVIDIA CUDA 저장소를 추가해야만 존재합니다. RHEL 9의 기본 `BaseOS`/`AppStream` 저장소는 NVIDIA 패키지를 제공하지 않습니다. Blackwell GPU(RTX 5060/70/80/90)에는 `nvidia-driver:open-dkms` 모듈이 **필수**이며, 구형 GPU는 open과 proprietary 둘 다 허용합니다.

3. **Ubuntu/Debian — 일치하는 유저스페이스 라이브러리 설치:**
   ```bash
   DRV_MAJOR=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | cut -d. -f1)
   sudo apt install -y libnvidia-compute-${DRV_MAJOR}
   sudo ldconfig && sudo systemctl restart docker
   ```

4. **CDI 스펙 생성:**
   ```bash
   sudo mkdir -p /etc/cdi
   sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
   docker run --rm --device=nvidia.com/gpu=all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
   ```

`atlas-bootstrap.sh` 스크립트는 이제 1, 2단계(RHEL/Rocky/Alma와 subscription 경로를 자동 판별)와 4단계를 자동으로 실행합니다. 3단계는 Debian/Ubuntu에서 실행 중인 드라이버 버전에 맞춘 `libnvidia-compute-NN`으로 자동 처리됩니다.

### AMD GPU가 감지되지 않음 (ROCm)

**증상:** AMD GPU가 분명히 있는 호스트에서 `atlas tier`가 "no GPU detected"라고 하거나, `docker compose up`이 `/dev/kfd: no such file or directory`로 실패합니다.

**의미:** `amdgpu` 커널 드라이버가 컴퓨트 지원(`kfd` — Kernel Fusion Driver — 서브모듈)과 함께 로드되지 않았습니다. 디스플레이 전용으로 로드된 `amdgpu`는 `/dev/kfd`를 노출하지 않습니다.

**해결 순서:**

1. **드라이버가 로드되었고 `/dev/kfd`가 존재하는지 확인:**
   ```bash
   lsmod | grep amdgpu       # should print amdgpu + amdkfd
   ls -l /dev/kfd            # should print a character-device entry
   ls -l /dev/dri/render*    # should print one or more render nodes
   ```

2. **ROCm + 커널 드라이버 설치 (/dev/kfd가 없는 경우):**
   - **RHEL 9 / Rocky / Alma:**
     ```bash
     sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
     sudo amdgpu-install --usecase=dkms,rocm
     sudo reboot   # required — the kernel module needs a fresh boot
     ```
   - **Ubuntu/Debian:** 배포판에 맞는 [AMD 공식 설치 가이드](https://rocm.docs.amd.com/projects/install-on-linux/)를 따르세요. 통상 AMDGPU 저장소 추가 후 `amdgpu-install --usecase=dkms,rocm` 순서입니다.

3. **재부팅 후 `rocm-smi`에 GPU가 보이는지 확인:**
   ```bash
   rocm-smi --showproductname --showmeminfo vram
   ```

### AMD GPU는 감지되는데 Docker가 접근하지 못함

**증상:** `atlas doctor`가 "AMD GPU detected but Docker can't reach `/dev/kfd`"를 보고하거나, ROCm 컨테이너가 `/dev/kfd`에 대한 `Permission denied`로 실패합니다.

**의미:** Docker를 실행하는 사용자가 `render` 그리고/또는 `video` 그룹에 없습니다. ROCm은 이 그룹들로 `/dev/kfd`와 `/dev/dri/render*` 접근을 통제합니다.

**해결:**

```bash
# 1. Confirm which groups you're currently in
id -nG | tr ' ' '\n' | grep -E '^(render|video)$'
# Expect both. If either is missing:

# 2. Create the groups if they don't exist (rare; default on most distros)
sudo groupadd -f render
sudo groupadd -f video

# 3. Add your user to both
sudo usermod -aG video,render $USER

# 4. Re-login (or use newgrp for the current shell)
newgrp render
newgrp video

# 5. Re-verify, then re-run `atlas doctor`
id -nG | grep -E 'render.*video|video.*render'
atlas doctor
```

### ROCm이 "미지원"이라는 AMD GPU에서 그래도 시도해 보고 싶을 때 (ROCm의 `no kernel image`)

**증상:** `rocm-smi`는 GPU를 보고하지만 `rocminfo`는 보고하지 않거나, HIP 커널이 "no kernel image is available for execution on the device"로 실패합니다. (NVIDIA에서 같은 오류가 나는 경우는 [CUDA 항목](#no-kernel-image-is-available-for-execution-on-the-device-cuda)을 참고하세요.)

**의미:** llama.cpp의 HIP 커널이 본인 GPU를 포함하지 않는 `gfx` 타깃으로 컴파일되었습니다. ROCm은 구형 소비자용 GPU를 공식 지원에서 제외하면서도 적절한 오버라이드로는 여전히 동작하게 두는 패턴이 오래됐습니다.

**해결:** `ATLAS_HSA_OVERRIDE_GFX_VERSION`으로 런타임에 호환 gfx 버전을 강제하세요. 일반적인 오버라이드(정식 카드→gfx 표는 [SETUP.md § AMD GPU 타깃](../ko/SETUP.md#amd-gpu-타깃-dockerfilerocm) 참고):

| GPU | `ATLAS_HSA_OVERRIDE_GFX_VERSION=` 설정값 |
|---|---|
| RDNA1 (RX 5700 XT / 5500 XT) | `10.3.0` (RDNA2 / gfx1030처럼 보이게 함) |
| Vega 56/64 (gfx900) | `9.0.0` (보통 이미 지원되어 오버라이드가 거의 불필요) |
| Polaris (RX 580/590, gfx803) | `8.0.3` (깊은 오버라이드; 결과는 제각각) |

compose 오버라이드를 거쳐 컨테이너 환경으로 전파되도록 `.env`에 설정하세요:

```bash
echo "ATLAS_HSA_OVERRIDE_GFX_VERSION=10.3.0" >> .env
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d --force-recreate llama-server
```

이전에 미지원이던 카드에서 이 방법이 동작한다면 [GH #26](https://github.com/inferstep/ATLAS/issues/26)에 알려 주세요 — 커뮤니티 검증 오버라이드는 다음 릴리스 문서에 반영됩니다.

### RDNA4 (RX 9070 / 9070 XT, gfx1200 / gfx1201) — ROCm 7.x 필요

**증상:** `docker compose ... build llama-server` 중 `error: AMDGPU target 'gfx1201' is not supported` 같은 오류로 빌드가 실패하거나, 컨테이너가 시작되자마자 HIP 초기화 오류로 종료됩니다.

**의미:** 기본 ROCm 베이스 이미지(`rocm/dev-ubuntu-22.04:6.2-complete`)는 RDNA4 이전 것입니다. gfx1200과 gfx1201 컴파일러 타깃은 ROCm 7.0에서 추가되었습니다 — 전체 지원 하드웨어 목록은 [ROCm 호환성 매트릭스](https://rocm.docs.amd.com/en/latest/compatibility/compatibility-matrix.html)를 참고하세요.

**해결:** 빌드 전에 `ATLAS_ROCM_TAG`를 ROCm 7.x 태그로 설정하세요:

```env
# Add to your .env
ATLAS_ROCM_TAG=7.2.3-complete
ATLAS_GFX_TARGET=gfx1201   # gfx1200 for RX 9070, gfx1201 for RX 9070 XT
```

그런 다음 재빌드하고 스택을 기동하세요:

```bash
docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
```

**중요: gfx1200/gfx1201에는 `ATLAS_HSA_OVERRIDE_GFX_VERSION`을 설정하지 마세요.** ROCm 7.0+는 이 타깃들을 네이티브로 지원합니다. Docker 안에서 GFX 버전을 오버라이드하면 컴파일된 커널과 런타임 타깃이 불일치해 크래시가 발생합니다. `ATLAS_HSA_OVERRIDE_GFX_VERSION`은 설정하지 않은 채(기본값) 두세요.

> AMD Radeon AI PRO R9700(gfx1201) + ROCm 7.2, `ATLAS_ROCM_TAG=7.2.3-complete`에서 테스트되었습니다. hidden-states 패치는 고정된 llama.cpp SHA에 깔끔하게 적용됩니다. 추가 플래그 없이 텍스트 생성과 임베딩 생성 모두에서 추론이 올바르게 동작합니다.

### ROCm 컨테이너가 `rocm/rocm-terminal`을 pull하지 못함

**증상:** `atlas doctor`의 ROCm 점검이 이미지 pull에서 타임아웃되거나, `docker compose -f ... -f docker-compose.rocm.yml pull`이 `llama-server` 빌드에서 실패합니다.

**의미:** ROCm 이미지는 크고(~2GB) Docker Hub는 익명 pull에 속도 제한을 겁니다.

**해결:** 인증한 뒤(무료 Docker Hub 계정도 더 높은 제한 허용) doctor의 점검 이미지를 미리 pull하거나, 한산한 시간대에 재시도하세요:

```bash
docker login
docker pull rocm/rocm-terminal:latest
```

doctor의 ROCm 점검은 항상 `rocm/rocm-terminal:latest`를 사용합니다. `ATLAS_ROCM_TAG`는 doctor의 점검 이미지가 아니라 llama-server ROCm 빌드의 *빌드 베이스* 이미지(`rocm/dev-ubuntu-*`)를 고정합니다:

```bash
ATLAS_ROCM_TAG=6.2-complete docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
```

### 첫 빌드 실패 (CUDA를 찾을 수 없음)

**증상:** llama-server 컴파일 중 `docker compose build`가 CUDA 관련 오류로 실패합니다.

**해결:** llama-server Dockerfile은 `nvidia/cuda:12.9.0-devel` 베이스 이미지(`inference/Dockerfile.v31`에 다이제스트 고정) 안에서 llama.cpp를 빌드하므로, 호스트 GPU 접근 없이도 빌드 시 CUDA 헤더를 사용할 수 있습니다. 빌드 실패의 일반적인 원인:
1. 디스크 공간 부족 (빌드 아티팩트에 약 5GB 필요)
2. CUDA 베이스 이미지 다운로드 또는 llama.cpp 클론 시 네트워크 문제
3. Podman rootless 빌드가 권한 문제로 실패할 수 있음 — `podman-compose build`에 `--podman-build-args="--format docker"`를 추가해 보세요

### llama.cpp 클론 타임아웃

**증상:** 빌드가 `llama-server builder 3/3` 단계에서 멈추다가 결국 다음과 같이 실패합니다:

```
error: RPC failed; curl 56 OpenSSL SSL_read: Connection timed out, errno 110
fatal: early EOF
fatal: fetch-pack: invalid index-pack output
```

**원인:** llama.cpp의 전체 git 이력은 크고(~1GB) fetch는 불안정하거나 느린 회선에 민감합니다. 순간적인 정체가 SSL read 타임아웃을 유발해 전체 전송이 중단됩니다.

**해결:** `inference/Dockerfile.v31`은 `git init` + 고정된 단일 리비전(`LLAMA_CPP_REV`)의 `--depth 1` fetch를 사용해 ~1GB의 전체 이력 전송을 회피하며, `http.postBuffer=524288000`과 `http.lowSpeedLimit/Time`으로 죽은 연결에서 빠르게 실패합니다. 문제가 재발하면:

1. 빌드를 재시도하세요 — 특히 가정용 회선에서는 일시적 네트워크 문제가 흔합니다.
2. 재시도가 계속 실패하면 호스트에서 고정된 리비전을 미리 받아 Dockerfile이 COPY하도록 수정하세요. 간단한 레시피:
   ```bash
   REV=$(grep -m1 'ARG LLAMA_CPP_REV=' inference/Dockerfile.v31 | cut -d= -f2)
   git init /tmp/llama.cpp && cd /tmp/llama.cpp
   git remote add origin https://github.com/ggml-org/llama.cpp
   git fetch --depth 1 origin "$REV" && git checkout FETCH_HEAD
   # then edit Dockerfile.v31 to COPY from /tmp/llama.cpp instead of fetching
   ```
3. GHCR의 사전 빌드된 llama-server 이미지는 이 단계를 완전히 건너뜁니다 — 빌드 대신 pull하세요.

### llama.cpp 재빌드 (새 모델 아키텍처, 또는 패치 드리프트)

개발자 유지보수 작업입니다. 두 가지 트리거가 여기에 해당합니다:

- **드롭인한 모델이 로드에 실패** — `error loading model: unknown (model) architecture 'gemma4'`: 고정된 llama.cpp가 그 아키텍처보다 오래된 것입니다.
- **빌드가 실패** — `error: patch failed: tools/server/server-context.cpp:NN` / `patch does not apply`: 업스트림이 고정된 SHA를 지나쳐 드리프트했습니다.

`atlas-llama` 이미지는 네 개의 Dockerfile(`Dockerfile`, `Dockerfile.v31`, `Dockerfile.rocm`, `Dockerfile.vulkan`) 모두에서 `LLAMA_CPP_REV`로 llama.cpp를 고정하며, compose 파일이 빌드에 사용하는 세 개 — `Dockerfile.v31`, `Dockerfile.rocm`, `Dockerfile.vulkan` — 는 빌드 중에 `inference/patches/expose-hidden-states.patch`(Geometric Lens가 의존하는 레이어별 `hidden_states` 확장)를 적용합니다. 순수 `Dockerfile`은 rev를 고정하되 패치를 적용하지 않으므로, 그것으로 빌드한 서버에는 lens 배관이 없습니다. 새 아키텍처를 지원하려면 그것을 포함하는 llama.cpp SHA로 핀을 옮기세요. 사전 빌드된 GHCR 이미지는 로컬 빌드를 건너뜁니다. 게시된 이미지보다 새로운 아키텍처가 필요할 때만 재빌드하세요.

**hidden-states 패치는 보존하세요 — 삭제하지 말고 리베이스하세요.** `git apply` 단계를 제거하면 lens 배관을 조용히 잃은 서버가 빌드됩니다(`/embedding`이 `layers:` 파라미터를 무시). 핀 올리기 런북:

1. **대상 SHA에 대해 패치 검증** (빠름, Docker 불필요):
   ```bash
   mkdir -p /tmp/llama-check && cd /tmp/llama-check
   git init -q llama.cpp && cd llama.cpp
   git remote add origin https://github.com/ggml-org/llama.cpp
   git fetch --depth 1 origin <NEW_SHA> && git checkout -q FETCH_HEAD
   git apply --check $REPO/inference/patches/expose-hidden-states.patch
   ```
   (`git apply`되는 것은 이 패치뿐입니다. spec-decode 임베딩 수정은 Dockerfile의 `sed`이며, 대상 줄이 없으면 no-op입니다.)
2. **깔끔하게 적용되면:** 네 Dockerfile 모두에서 `LLAMA_CPP_REV`를 새 SHA로 올리세요. CI 스모크 테스트가 이들이 일치하는지 검증합니다.
3. **실패하면:** `git apply --reject …`로 깔끔한 헝크를 적용하고, 각 `*.rej` 헝크를 옮겨진 앵커 위치에 다시 삽입한 뒤(주변 코드의 업스트림 이름 변경에 주의 — 예: `model` → `model_tgt` — 패치의 추가 줄도 갱신), `git diff > $REPO/inference/patches/expose-hidden-states.patch`를 실행하세요. 1단계를 다시 실행하세요. 긴 CUDA 빌드 전에 멤버/타입 오류를 잡으려면 서버 타깃만 CPU 전용으로 컴파일하세요: `cmake -B build-cpu -DGGML_CUDA=OFF && cmake --build build-cpu --target llama-server`.
4. 재빌드 후 기동:
   ```bash
   docker compose build --build-arg LLAMA_CPP_REV=<sha> llama-server
   docker compose up -d llama-server --no-deps
   ```

구형 SHA에 고정하기보다 패치를 다시 생성하는 편을 택하세요 — 뒤로 고정하면 업스트림 수정을 놓칩니다.

재빌드로 모델이 로드된 뒤에도 Geometric Lens는 새 모델을 위한 재학습이 필요합니다 — [CONFIGURATION.md § Adding your own model](../../CONFIGURATION.md#adding-your-own-model-drop-in--unregistered)을 참고하세요.

### 프록시가 워크스페이스에 쓰지 못함 (`.atlas.tmp: permission denied`)

**증상:** 모든 `write_file`/`edit_file`이 `cannot write /workspace/...: open /workspace/....atlas.tmp: permission denied`로 실패합니다(이후 에이전트는 "쓰기 가능한 하위 디렉터리"를 찾아 헤맵니다).

**원인:** atlas-proxy 이미지는 이미지에 구워진 비루트 사용자(uid 1001, `atlas`)로 실행되지만, `/workspace`(`ATLAS_PROJECT_DIR`)에 바인드 마운트된 호스트 디렉터리는 운영자의 uid가 소유합니다. 읽기는 되지만(모드 755) 쓰기는 모두 거부됩니다. `.env`가 `ATLAS_PROXY_UID`보다 오래된 설치는 하드닝된 프록시 이미지를 받은 뒤 이 문제를 겪습니다.

**해결:** 샌드박스가 이미 하는 것과 동일하게, 프록시를 호출한 사용자로 실행하세요:

```bash
# 자신의 id를 .env에 추가(atlas init --reconfigure도 이제 이 값들을 씁니다)
echo "ATLAS_PROXY_UID=$(id -u)" >> .env
echo "ATLAS_PROXY_GID=$(id -g)" >> .env
docker compose up -d --no-deps --force-recreate atlas-proxy
```

확인: `docker exec atlas-atlas-proxy-1 touch /workspace/.write_test`가 성공해야 합니다(확인 후 파일은 삭제하세요). K3s 배포에서는 `scripts/generate-manifests.sh`가 동일한 id를 프록시 Pod의 `securityContext`에 렌더링합니다.

### 바로 거기 있는 파일을 에이전트가 "없다"고 말함 (워크스페이스 마운트 분리)

**증상:** 프로젝트 디렉터리에 분명히 있는데도 에이전트 세션이 파일이 "존재하지 않는다"고 주장합니다 — `read_file`은 실패하는데 `run_command`(`ls`, `cat`)는 파일을 잘 보거나, 그 반대입니다. 세션은 일찍 포기하고("파일 X가 존재하지 않는 것 같습니다"), 쓰기는 프로젝트에 반영되지 않으며, 그런데도 모든 `/health` 엔드포인트는 초록색입니다.

**원인:** 프록시와 샌드박스가 **서로 다른 호스트 디렉터리**를 `/workspace`로 바인드 마운트하고 있습니다. 파일 도구(`read_file`/`write_file`/`edit_file`)는 프록시가 *자신의* 마운트에 대해 처리하고, `run_command`는 *샌드박스의* 마운트에서 실행됩니다. Compose는 마운트 소스를 `ATLAS_PROJECT_DIR`(기본값: compose 작업 디렉터리)에서 **컨테이너별로 생성 시점에** 결정합니다 — 따라서 한쪽 컨테이너를 다른 디렉터리에서, 또는 다른 `.env`로 재생성하면 둘은 조용히 갈라집니다. 시작 시에는 아무것도 실패하지 않고, 에이전트만 분리된 상태로 동작합니다.

**진단:** `atlas doctor` — `workspace_mounts` 체크가 두 마운트를 비교해 다르면 두 호스트 경로를 보여주며 실패합니다. 수동으로 하려면:

```bash
docker inspect atlas-atlas-proxy-1 --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
docker inspect atlas-sandbox-1     --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
```

**해결:** `.env`의 `ATLAS_PROJECT_DIR`를 프로젝트 디렉터리로 고정한 뒤, 두 컨테이너를 함께 재생성하세요:

```bash
echo "ATLAS_PROJECT_DIR=/path/to/your/project" >> .env
docker compose up -d --force-recreate atlas-proxy sandbox
```

### SELinux가 컨테이너 접근을 차단함 (Fedora/RHEL)

**증상:** 컨테이너가 마운트된 볼륨을 읽을 수 없고, 모델 파일에 대해 권한 거부가 발생합니다.

**해결:**
```bash
# Allow container access to model directory
chcon -Rt svirt_sandbox_file_t ~/models/

# Or add :Z flag to volume mounts (Docker Compose handles this)
```

### Sandbox에 연결할 수 없음

**증상:** 프록시 헬스에 `"sandbox": false`가 표시됩니다. V3 빌드 검증이 실패합니다.

**해결:** 샌드박스는 `atlas`가 아니라 `sandbox-net`을 통해 접근합니다 — 실행된 코드가 나머지 스택에 닿을 수 없도록, 샌드박스는 의도적으로 `atlas` 네트워크에 속하지 않는 유일한 서비스입니다. 프록시, 렌즈, v3-service는 두 네트워크에 모두 연결됩니다. 컨테이너를 수동으로 실행하는 경우:
```bash
docker network create sandbox-net
# sandbox joins ONLY sandbox-net; proxy / lens / v3-service join it too,
# in addition to the atlas network.
```

### 포트 충돌

**증상:** `docker compose up`이 특정 포트에서 "address already in use"로 실패합니다.

**해결:** 해당 포트를 사용하는 프로세스를 확인해 중지하거나 `.env`에서 ATLAS 포트를 변경하세요:
```bash
# Find what's using port 8080
lsof -i :8080

# Change port in .env
ATLAS_LLAMA_PORT=8081    # Different port for llama-server
```

모든 포트는 `.env`로 설정할 수 있습니다. [CONFIGURATION.md](../../CONFIGURATION.md)를 참고하세요.

---

## llama-server 문제

### `no kernel image is available for execution on the device` (CUDA)

**해당 대상:** 사전 빌드된 `ghcr.io/inferstep/atlas-llama` 이미지를 실행하는,
Blackwell보다 오래된 NVIDIA GPU — RTX 40xx (Ada), RTX 30xx (Ampere),
RTX 20xx / T4 (Turing), GTX 10xx (Pascal), V100/A100/H100/L4.
형제 오류인 `invalid device function`(런타임)과
`nvcc fatal: unsupported gpu architecture`(로컬 빌드)도 원인이 같습니다.
(AMD에서 같은 오류가 나는 경우는 [ROCm 항목](#rocm이-미지원이라는-amd-gpu에서-그래도-시도해-보고-싶을-때-rocm의-no-kernel-image)을 참고하세요.)

**의미:** 게시된 CUDA 이미지는 컴퓨트 캐퍼빌리티 `120;121`(Blackwell 전용)로
컴파일되어 있습니다. llama-server 바이너리에는 이전 아키텍처용 GPU 커널이
없고, 내장된 PTX(`compute_121`)는 하위 방향으로 JIT 컴파일될 수 없으므로,
첫 CUDA 커널 실행이 실패합니다. 드라이버나 VRAM 문제가 아니라 이미지/GPU
불일치입니다.

**먼저 확인:**
```bash
# Your GPU's compute capability (8.9 = Ada, 8.6 = Ampere, 7.5 = Turing, 12.0 = Blackwell)
nvidia-smi --query-gpu=name,compute_cap --format=csv
# What the image was built for (Blackwell-only image prints sm_120/sm_121)
docker run --rm --entrypoint bash ghcr.io/inferstep/atlas-llama:latest \
  -c 'grep -ao "sm_[0-9]*" /usr/local/bin/llama-server | sort -u'
```
본인 컴퓨트 캐퍼빌리티가 12.0 미만이고 이미지가 `sm_120`/`sm_121`만
나열한다면 이 항목이 해당합니다.

**해결 — 본인 아키텍처에 맞춰 추론 이미지를 재빌드하세요** (일회성,
~30-75분; llama-server만 재빌드되고 다른 서비스는 GHCR 이미지를 계속
사용합니다). 컴퓨트 캐퍼빌리티에서 점을 제거하세요(`8.6` -> `86`):
```bash
docker compose build --build-arg CUDA_ARCH=86 llama-server
docker compose up -d --no-deps llama-server
```
GPU가 여러 종류이거나 이식성이 필요하면 세미콜론 목록을 전달하세요. 예:
`--build-arg CUDA_ARCH="75;86;89"`. 전체 아치 표:
[SETUP.md § CUDA 컴퓨트 캐퍼빌리티](../ko/SETUP.md#cuda-컴퓨트-캐퍼빌리티-dockerfilev31).

**확인:**
```bash
docker compose logs llama-server | tail -20   # model loads, no CUDA errors
curl -s localhost:8080/health                  # {"status":"ok"}
```

**여전히 실패한다면:** 컨테이너가 실제로 재빌드한 이미지를 실행 중인지
확인하세요(`docker compose images llama-server` — `docker compose pull`이
덮어썼을 수 있습니다; `ATLAS_IMAGE_TAG`를 고정하거나 빌드를 다시
실행하세요). Pascal(`60`/`61`) 및 그 이전은 업스트림 llama.cpp CUDA 지원의
제약을 받습니다 — 폴백은 Vulkan 이미지(`docker-compose.vulkan.yml`)입니다.
그래도 안 되면 `nvidia-smi` 출력과 llama-server 로그 첫 50줄을 첨부해
이슈를 열어 주세요.

### 모델이 GPU 대신 CPU에서 로드됨

**증상:** 약 50 tok/s 대신 약 2 tok/s로 생성됩니다. `nvidia-smi`에 llama-server가 GPU 사용 프로세스로 보이지 않습니다.

**해결:** `-ngl 99`(`--n-gpu-layers`)가 설정되어 있는지 확인하세요(모든 레이어를 GPU로 오프로드). Docker Compose에서는 엔트리포인트가 기본으로 설정합니다. 베어메탈이라면 명령을 확인하세요:
```bash
ps aux | grep llama-server | grep -e '-ngl' -e 'n-gpu-layers'
```

Docker를 사용 중이라면 NVIDIA 컨테이너 런타임이 설정되어 있는지 확인하세요(위의 GPU 섹션 참고).

### 모델 + KV 캐시가 GPU에 들어가지 않음 (시작 실패, 또는 생성이 5배 느림)

**증상 (현행 엔트리포인트):** llama-server가 "fitting params to device memory" 직후 CUDA 할당 오류로 시작 시 종료됩니다.

**증상 (`--fit off`가 없는 구형 엔트리포인트):** 서버는 *시작*되고 `nvidia-smi`에는 모델이 로드된 것으로 보이지만, 생성 속도가 기대치의 몇 분의 일로 떨어지고, llama-server 프로세스가 여러 CPU 코어를 소모하며(`top`에서 400–800%), 호스트 RSS가 모델 가중치 수 기가바이트를 점유합니다 — llama.cpp의 메모리 자동 피팅이 레이어를 조용히 CPU로 옮긴 것입니다.

**원인:** 모델 가중치 + KV 캐시(`ATLAS_CTX_SIZE` × `PARALLEL` 슬롯 × 레이어별 KV 차원) + 컴퓨트 버퍼(약 `ATLAS_UBATCH` × 은닉 차원 × 280바이트)가 VRAM을 초과합니다. 이 예산은 모델마다 다릅니다 — 한 모델에 맞춘 설정이 KV 기하 구조가 다른 모델에서는 넘칠 수 있습니다.

**해결:** 이 모델과 GPU에 맞게 런타임 크기를 조정하고 컨테이너를 재생성하세요:
```bash
atlas tier fit --write
docker compose up -d llama-server --no-deps --force-recreate
```
`atlas tier fit`은 GGUF 헤더와 GPU의 VRAM을 읽어 완전히 GPU 위에서 동작하는 최대 구성을 계산합니다([CLI.md § atlas tier fit](../../CLI.md#atlas-tier-fit) 참고). ATLAS는 llama-server를 `--fit off`로 실행하므로, 들어가지 않는 구성은 일부가 조용히 CPU에서 도는 대신 시작 시 명확하게 실패합니다.

`atlas tier fit`이 **DOES NOT FIT**을 보고하면 모델 자체가 이 카드에 너무 큽니다 — 출력에 들어갈 수 있는 최대 양자화 파일 크기가 표시됩니다. 우선순위 순서:

1. **같은 모델의 더 작은 양자화 사용** (예: Q6_K 대신 Q4_K_M — 16 GB VRAM 미만에서는 보통 품질/GiB 면에서 최선의 선택).
2. **병렬 슬롯 줄이기**: `atlas tier fit --slots 1 --write`로 슬롯당 KV 최소값이 확보됩니다(`/demo` 분할 창과 V3 병렬 후보는 사용할 수 없지만 단일 스트림 사용은 가능).
3. **더 작은 모델 선택.** 아래 사이징 표를 참고하세요.

### 내 GPU에는 무엇이 들어가는가?

다운로드 전 대략적인 규칙: 기본 4 슬롯에서는 GGUF가 다음 조건이면 여유 있게 들어갑니다

```
file size  ≤  VRAM − ~4.5 GiB
```

(약 4.5 GiB는 4 × 8k 컨텍스트의 최소 KV 캐시, 컴퓨트 버퍼, 약 1.9 GiB의 CUDA 고정 오버헤드를 포함합니다.) `--slots 1`에서는 여유가 대략 `VRAM − 3 GiB`까지 줄어듭니다. 슬라이딩 윈도우 모델(Gemma 계열)은 이보다 덜 필요합니다. 이 규칙은 풀 어텐션 모델 기준입니다.

| VRAM | GGUF 파일 크기 (4 슬롯) | GGUF 파일 크기 (1 슬롯) | 대표 모델 |
|------|--------------------------|--------------------------|----------------|
| 8 GB | ≤ 약 3 GiB | ≤ 약 4.5 GiB | 3–4B Q4–Q6, 7–8B Q2–Q3 |
| 12 GB | ≤ 약 7 GiB | ≤ 약 8.5 GiB | 7–9B Q4–Q6, 12B Q3–Q4 |
| 16 GB | ≤ 약 11 GiB | ≤ 약 12.5 GiB | 9B Q6–Q8, 12–14B Q4–Q6 |
| 24 GB | ≤ 약 19 GiB | ≤ 약 20.5 GiB | 14B Q8, 27–32B Q4 |

HuggingFace 모델 페이지에는 양자화별 파일 크기가 표시됩니다 — 다운로드 전에 이 표와 대조하세요. 이 표는 다운로드 전 추정치일 뿐입니다. 파일이 디스크에 있으면 `atlas tier fit /path/to/model.gguf`가 정확한 답입니다(모델의 실제 KV 기하 구조를 읽으므로 예산이 어느 방향으로든 수 기가바이트 달라질 수 있습니다). `atlas onboard`도 같은 핏을 자동으로 표시합니다.

### 모델 파일을 찾을 수 없음

**증상:** llama-server가 "failed to load model" 또는 유사한 메시지와 함께 즉시 종료됩니다.

**해결:** 모델 경로를 확인하세요:
```bash
# Docker Compose — model must be in ATLAS_MODELS_DIR (default: ./models/)
ls -la "models/$ATLAS_MODEL_FILE"

# Bare metal — check ATLAS_MODELS_DIR + ATLAS_MODEL_FILE
ls -la "$ATLAS_MODELS_DIR/$ATLAS_MODEL_FILE"
```

파일명은 `.env`에서 필수인 `ATLAS_MODEL_FILE` 선택과 일치해야 합니다.

### VRAM 부족

**증상:** llama-server가 시작 직후 크래시하거나 OOMKilled됩니다. `nvidia-smi`에 VRAM 사용량이 거의 100%로 표시됩니다.

**해결:** 다음을 확인하세요:
1. 다른 GPU 프로세스가 실행 중이지 않은지 (`nvidia-smi` — 다른 CUDA 프로세스 확인)
2. 16GB 이상의 VRAM이 있는지
3. 런타임이 모델과 GPU에 맞게 사이징되었는지: `atlas tier fit --write` (권장값을 넘어 `ATLAS_CTX_SIZE`를 올리지 마세요)

```bash
# Kill other GPU processes if needed
nvidia-smi --query-compute-apps=pid --format=csv,noheader | xargs -I{} kill {}
```

### 문법이 강제되지 않음 (모델이 사고 블록을 출력함)

**증상:** 모델이 JSON 도구 호출 대신 `<think>` 태그나 일반 텍스트를 출력합니다.

**해결:** 프록시는 `/v1/agent` 에이전트 루프 핸들러 안에서 자동으로 `response_format`을 설정합니다. 그 형태는 `ATLAS_GRAMMAR_MODE`가 결정합니다: 기본값 `strict`는 `{"type":"json_object","schema":<full tool-call schema>}`를 전송해 llama-server의 GBNF 샘플러가 tool_call/text/done 유니언만 방출할 수 있게 하고, `ATLAS_GRAMMAR_MODE=loose`는 `{"type":"json_object"}`만 전송합니다(유효한 JSON이되 형태는 강제하지 않음) — 스키마→GBNF 변환을 잘 다루지 못하는 모델을 위한 탈출구입니다(Gemma 계열 모델은 `loose`가 필요합니다 — strict 모드에서는 done만 반복 방출합니다). llama-server를 `/v1/chat/completions`나 `/v1/completions`로 직접 호출하는 경우에는 파라미터를 직접 포함해야 합니다:
```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "local-model",
    "messages": [{"role":"user","content":"Say hi"}],
    "max_tokens": 50,
    "response_format": {"type": "json_object"}
  }'
```

JSON 대신 일반 텍스트가 반환되면 llama.cpp 빌드가 `response_format`을 지원하지 않는 것입니다. 최신 소스에서 다시 빌드하세요.

### 컨텍스트 윈도우가 너무 작음

**증상:** 도구 호출 인자가 잘립니다. 도구 결과에 "Tool call was truncated (output too long for context window)" / "Your output was truncated — the content is too long for a single tool call"이 실리거나, 프록시 로그에 `truncated args detected for <tool> at turn N`이 표시됩니다.

**해결:** 슬롯당 컨텍스트(`ATLAS_CTX_SIZE` ÷ `ATLAS_PARALLEL_SLOTS`; compose 기본값 131072 ÷ 4 = 슬롯당 32k)가 작업에 비해 너무 작을 수 있습니다. `atlas tier fit`이 GPU가 지원하는 최대 예산을 보여줍니다. 확인:
```bash
# Docker Compose
grep CTX_SIZE .env

# Bare metal
ps aux | grep llama-server | grep ctx-size
```

---

## 프록시 문제

### 에이전트 루프가 활성화되지 않음

**증상:** 요청이 llama-server로 직접 전달됩니다. 도구 호출, 스트리밍 상태 아이콘, V3 파이프라인이 없습니다.

**원인:** 잘못된 엔드포인트를 호출하고 있습니다. 에이전트 루프는 `POST /v1/agent`에서만 실행됩니다. `POST /v1/chat/completions`(및 `/v1/` 하위의 다른 모든 경로)는 llama-server로의 투명한 패스스루입니다 — 도구도, V3도, 스트리밍 채팅 이벤트도 없습니다.

**해결:** 클라이언트가 `POST http://localhost:8090/v1/agent`를 가리키게 하세요. Bubbletea TUI(`atlas` / `atlas tui`)는 이를 자동으로 수행합니다. 서드파티 클라이언트를 작성하는 경우 `/v1/agent` SSE 이벤트 프로토콜은 [docs/API.md](../../API.md)를 참고하세요. `ATLAS_AGENT_LOOP` 환경 변수 토글은 더 이상 없습니다 — 분기는 설정이 아니라 엔드포인트 기반입니다.

### V3 파이프라인이 기능 파일에서 실행되지 않음

**증상:** 모든 `write_file` *또는* `edit_file` 호출이 T1(직접 쓰기)입니다. 출력에 V3 파이프라인 단계가 없습니다.

V3는 **모든 조건**이 충족될 때 실행됩니다:
1. 파일에 **10줄 이상**의 콘텐츠가 있을 것 (10줄 미만은 항상 T1)
2. 파일에 **2개 이상의 로직 지표**(함수 정의, 제어 흐름, API 패턴)가 있을 것 — **또는** 인식되는 코드/마크업 확장자(`.py`, `.go`, `.js`, `.html`, …)를 가질 것 (이 경우 지표가 0개여도 10줄 이상이면 T2)
3. V3 서비스가 `ATLAS_V3_URL`에서 접근 가능할 것
4. `edit_file`에 한해: 결과 파일이 파일 전체 재실행을 정당화할 것 — 순환 복잡도 ≥ 8, 또는 복잡도를 측정할 수 없을 때 ≥ 80줄

설정, 데이터, 스타일, 마크다운, 셸 파일(`package.json`, `.yaml`, `.css`, `.md`, `.sh`, …)은 크기와 관계없이 항상 T1입니다. 요청 등급은 V3로 전달되지만 활성화를 게이트하지 않습니다 — 게이트하는 것은 파일 자체의 등급입니다.

`write_file`과 `edit_file` 모두 V3를 경유합니다.

**진단:**
```bash
# Check V3 service health
curl -s http://localhost:8070/health

# Check proxy logs for tier classification + V3 activation
docker compose logs atlas-proxy | grep -E "write_file|edit_file|tier="
# Look for:
#   "tier=T2:medium" or higher in classifier output
#   "[edit_file] V3 pipeline activating for X (file_tier=2, req_tier=2)"
#   "[write_file] V3 pipeline activating for X"
# T1 means direct write — no V3.
```

V3에 접근할 수 없으면 프록시는 `V3 failed: ...`를 로그로 남기고 편집을 깨지 않은 채 직접 쓰기로 폴백합니다.

### 잘림 오류 (write_file이 반복적으로 실패)

**증상:** "Your output was truncated — the content is too long for a single tool call." 같은 오류가 반복됩니다.

**원인:** 모델이 한 번의 호출에 너무 많은 콘텐츠를 쓰려고 합니다. 프록시가 잘린 JSON을 감지하고 도구 호출을 거부합니다.

프록시는 기존 경로에 대한 모든 `write_file`을 거부하고 — `write_file`은 파일 생성용입니다 — 모델에게 대신 `edit_file`을 쓰라고 알립니다. 유일한 예외: 5줄 이하의 파일, 디스크에서 손상된 것으로 보이는 파일(산문 서문, 떠도는 마크다운 펜스), 이 세션이 직접 쓴 파일. 3회 연속 실패 후에는 오류 루프 브레이커가 에이전트를 중지하고 요약을 반환합니다.

**해결:** 파일 전체 재작성 대신 대상을 지정한 변경을 요청하도록 문구를 바꾸세요 — "auth.py를 다시 작성해줘" 대신 "로그인 함수에 입력 유효성 검사를 추가해줘".

프록시는 실제 잘림(args 페이로드가 200바이트 초과)과 `args`가 비었거나 누락된 채 전송된 도구 호출을 구별합니다 — 후자는 잘림 리매핑 대신 `read_file: no arguments provided. Call with {"path":"<file>"}` 같은 도구별 힌트를 받습니다. 또한 OpenAI 스타일(`arguments`), Anthropic 스타일(`parameters`), 최상위 인라인 인자 형태를 정식 `args` 봉투로 정규화합니다. 정규화 후에도 도구 호출이 비어서 도착하면 프록시가 `[agent] turn=N EMPTY ARGS — raw model output: "..."`를 로그로 남기므로, 정확한 형태를 보고 요청을 바꿔 말할 수 있습니다.

### 도구 결과와 다음 동작 사이의 긴 정지

**증상:** 도구는 성공했는데 에이전트 루프가 다음 턴이 시작되기 전 ~30초간 멈춰 있습니다. 오류도 출력도 없다가 결국 다음 도구 호출이 나타납니다.

**무슨 일인가:** 제약된 JSON 문법 아래에서 일부 로컬 모델은 도구 결과 다음의 첫 토큰으로 EOS를 내보내며 빈 내용을 반환하고, 파스 오류 재시도 경로가 이를 복구해야 합니다 — 그것이 사라진 ~30초입니다.

**할 일:** 프록시는 `callLLMConstrained` 안에서 빈 턴을 잡아 `temperature=0.7`과 계속하라는 넛지로 한 번 인라인 재시도합니다. 지속적으로 재발하면 프록시를 재시작해 llama.cpp의 슬롯 캐시를 비우세요:
```bash
docker compose restart atlas-proxy llama-server
```
`docker compose logs atlas-proxy | grep -E "empty LLM|raw_len=0"`을 확인하세요 — 최초 호출과 재시도 모두에서 `raw_len=0`이면 모델이 재시도로 감당되는 것보다 나쁜 상태입니다.

### V3가 이미 수정을 확인했는데 모델이 계속 편집함

**증상:** 에이전트가 V3로 검증된 편집을 성공시킨 뒤(TUI에 `Probe passed`로 끝나는 V3 진행 이벤트가 표시됨), 같은 파일을 다시 읽고 무관한 함수들을 편집하기 시작합니다. 후속 편집 하나하나가 또 다른 전체 V3 사이클(~110초)을 유발합니다.

**무슨 일인가:** 콤팩트 로컬 모델은 "사용자의 원래 문제가 해결되었는가?"를 자체 평가하는 데 어려움을 겪을 수 있으며, 검증된 편집 후에도 계속 작업을 계획합니다.

**할 일:** 에이전트 루프는 V3로 검증된 쓰기 후에 `{"type":"done"}` 방출을 향한 강한 사용자 역할 넛지를 덧붙입니다. 모델이 이를 무시하면, 그 한 가지 변경이 원하는 전부임을 프롬프트에서 더 명시적으로 말하세요. 더 강한 중단 장치(파일별 편집 상한, 자동 done)는 후속 옵션으로 추적 중입니다.

### 모델이 이전 세션의 파일명을 환각함

**증상:** 완전히 새로운 세션, 새 프롬프트인데 모델의 첫 도구 호출이 이 워크스페이스에 존재하지 않는 파일명에 대한 `read_file`입니다 — 보통 최근 작업했던 다른 곳에 존재하는 파일명입니다.

**무슨 일인가:** llama.cpp의 KV 슬롯은 캐시를 따뜻하게 유지하기 위해 챗 컴플리션 사이에 유지됩니다. 세션을 넘어, 이전 세션 토큰의 잔여 어텐션 편향이 조작된 파일명 같은 저엔트로피 출력으로 샐 수 있습니다.

**할 일:** 모든 사용자 턴은 llama KV 슬롯 **전체**를 지우는 것으로 시작해(`--parallel` > 1이면 세션이 어느 슬롯에나 배정될 수 있으므로), 다음 컴플리션이 시스템 프롬프트를 새로 인코딩하게 합니다(따뜻한 GPU에서 ~1-2초). 캐시를 완전히 따뜻하게 유지하고 싶어 세션별 삭제를 끄려면:
```bash
# .env
ATLAS_FRESH_SLOT_PER_SESSION=0
```
변경 후 프록시를 재시작하세요. 삭제를 끈 상태에서 환각이 보이면 `llama-server`를 재시작해 모든 슬롯을 비우세요.

### 다중 파일 프로젝트: 샌드박스 `ModuleNotFoundError`

**증상:** 같은 프로젝트의 다른 모듈을 임포트하는 파일을 편집합니다. 본인 머신에서는 임포트가 동작하는데도 V3가 `ModuleNotFoundError: No module named 'utils'`로 검증 실패를 보고합니다.

**무슨 일인가:** V3의 `SandboxAdapter`는 에이전트가 읽은 모든 파일을 `solution.py`와 함께 샌드박스 워크스페이스로 실어 보냅니다. 읽기 집합(`ctx.FilesRead`)에 없는 파일은 거기 없으므로 그 임포트가 실패합니다.

**할 일:** 누락된 파일을 `read_file`로 읽어 프로젝트 컨텍스트에 올리세요. 샌드박스 `/execute` API를 직접 호출하는 경우 요청 본문에 지원 파일을 전달하세요:
```bash
curl -X POST http://localhost:30820/execute -d '{
  "code": "from utils import greet\nprint(greet(\"x\"))",
  "language": "python",
  "files": {"utils.py": "def greet(n): return f\"hi {n}\""}
}'
```

### Curses 하단 행 `addwstr() returned ERR`

**증상:** ATLAS는 편집이 V3 검증을 통과했다고 보고했는데, curses 프로그램이 런타임에 `_curses.error: addwstr() returned ERR`로 크래시합니다.

**무슨 일인가:** curses 윈도우의 마지막 셀(row=LINES-1 또는 column=COLS-1)에 쓰는 것은 문서화된 curses 동작상 ERR를 반환합니다. `interactive_lint`는 `try/except curses.error` 래핑 없이 거기에 쓰는 후보를 거부하므로, V3는 인증 전에 래핑된 변형을 찾아야 합니다. 관용적인 수정:
```python
try:
    stdscr.addstr(curses.LINES - 1, 0, border)
except curses.error:
    pass  # writing the bottom-right cell errors; benign
```

**할 일:** V3가 스스로 래핑을 합성하지 못하면 모델에게 명시적으로 말하세요: *"N번째 줄의 addstr 호출을 `try: ... except curses.error: pass`로 감싸라."* `docker compose logs v3-service | grep interactive_lint`로 lint 게이트가 발동했는지 확인하세요.

### 비 Python 파일에서 V3가 수 분간 멈춤

**증상:** ATLAS에 HTML/CSS/JSON 파일 작성을 요청하면 PR-CoT 수리 시도와 LLM 타임아웃을 동반한 ~5분의 정지가 발생합니다. 파일은 결국 직접 쓰기 폴백으로 저장됩니다.

**무슨 일인가:** V3 스모크 체크는 언어 인식입니다 — 대상 파일의 확장자에서 언어를 도출해 알맞은 검사기로 라우팅합니다(`.py` → Python compile, `.js` → `node --check`, `.ts` → `tsc --noEmit`, `.go` → `gofmt -e`, `.rs` → `rustc`, `.sh` → `bash -n`, `.html` → `html.parser`, `.xml` → `ElementTree`, `.json` → `json.loads`, `.yaml` → `yaml.safe_load`). 인식되지 않는 확장자는 Python으로 폴백해 실패하고, 이것이 수리로 연쇄됩니다. `.c`/`.cpp`/`.h`는 확장자 맵(`v3-service/pipeline.py`의 `_ext_to_lang`)에 없으므로, 샌드박스 자체에는 C/C++ 검사기가 있음에도 C/C++ 파일은 Python 폴백을 탑니다.

`/v3/generate`가 승인된 프로젝트 빌드 명령을 받으면, V3는 구문/셀프 테스트 검증 후에 `build_verify` 이벤트를 내보냅니다. 명령은 후보를 프로젝트에 겹쳐 얹은 일시적 샌드박스 워크스페이스에서 실행되므로, 실패한 빌드 증거가 실제 체크아웃에 후보를 쓰지 않은 채 `passed=true`를 차단합니다. 오버레이 스냅샷은 의존성 캐시, 시크릿, 모델/데이터 아티팩트, 심링크, 대용량 파일을 건너뛰며 파일 수와 바이트 제한을 강제합니다. 프로젝트 빌드에 무거운 의존성이 필요하면, 명시적 검증 워크플로의 일부로 샌드박스 워크스페이스 안에 설치하세요.

**할 일:** 인식되지 않는 확장자는 `v3-service/pipeline.py`의 `_ext_to_lang`에 추가하고 `v3-service` 이미지를 재빌드하세요. V3가 오류로 끝나면 프록시가 직접 쓰기로 폴백하므로 파일은 어쨌든 저장됩니다 — 느릴 뿐입니다. `docker compose logs v3-service | grep smoke_check`로 올바른 언어가 라우팅되었는지 확인하세요.

### "다시 고쳐줘" 프롬프트에서 V3 파이프라인이 실행되지 않음

**증상:** 첫 요청은 파일을 생성하고 V3가 실행됩니다. "ok"나 "yes" 같은 짧은 후속 요청은 대화형 응답만 받습니다 — 도구 호출도 V3 이벤트도 없습니다.

**무슨 일인가:** 에이전트 루프의 등급 분류기(`proxy/agent.go:classifyAgentTier`)는 하나의 질문에 답합니다: 이것은 대화인가, 작업인가? 기본값은 작업이며 T0에는 적극적인 근거가 필요합니다. 두 종류의 실수가 치르는 대가가 매우 다르기 때문입니다. 대화를 작업으로 잘못 읽으면 모델이 한 턴에 끝낼 메시지에 플래너 호출 한 번을 낭비하는 데 그치지만, 작업을 대화로 잘못 읽으면 턴이 5로 제한되고 플래닝도 건너뛰어 요청 자체가 실패합니다.

메시지가 대화형으로 판정되는 경우는 12자 미만(`hi`, `thanks`, `ok`)이거나 질문 형태일 때뿐입니다 — 절을 끝내는 `?`가 있거나, 의문사(`why`, `what`, `how`, `is`, `can`, …)가 단어 단위로 문두에 오는 경우. 다만 작업을 뜻하는 표현이 둘 모두를 앞서므로, `can you fix the login bug?`는 물음표가 있어도 작업입니다. `task_mode: work`를 선언하는 클라이언트(TUI가 그렇습니다)는 결코 대화형으로 분류되지 않습니다. 그 외에는 전부 작업입니다: `still doesn't work, try again`과 `the snake is moving way too fast, slow it down`은 파일명을 대지도, 작업 동사 목록에 맞지도 않지만 둘 다 파이프라인을 받습니다.

**할 일:** 짧게라도 원하는 것을 말하세요 — "yes, fix it"은 T0 게이트를 통과합니다. 후속 요청이 에이전트 루프는 돌리는데 V3가 조용하다면 요청 등급이 게이트가 아닙니다 — 파일 자체의 등급이 게이트입니다. [V3 파이프라인이 기능 파일에서 실행되지 않음](#v3-파이프라인이-기능-파일에서-실행되지-않음)을 참고하고, `docker compose logs atlas-proxy | grep -E "write_file|edit_file"`에서 파일 등급 줄(예: `[write_file] app.py → T1:simple (8 lines)`)을 확인하세요.

### 편집 전에 파일을 읽지 않음

**증상:** `edit_file`이 "file not read yet — use read_file first before editing."으로 실패합니다.

**원인:** 프록시는 에이전트가 읽은 파일을 추적합니다. 모델이 이 세션에서 읽지 않은 파일을 편집하려 하면 최신성 보호로 편집이 거부됩니다.

**해결:** 모델이 먼저 파일을 읽어야 합니다. 계속 실패하면 TUI에서 `/clear`를 입력해 채팅 이력을 초기화하고 바꿔 말해 보세요.

### 외부에서 파일이 수정됨

**증상:** `edit_file`이 "file modified since last read — read it again before editing."으로 실패합니다.

**원인:** 모델이 파일을 읽은 후 디스크에서 파일이 변경되었습니다(사용자 또는 다른 프로세스에 의해). 프록시가 수정 타임스탬프를 비교합니다.

**해결:** 모델이 파일을 다시 읽어야 합니다. 보통 다음 턴에서 자동으로 해결됩니다.

### 탐색 예산 경고

**증상:** 출력에 "You have full project context in the system prompt. Do not read more files."가 표시됩니다.

**원인:** 모델이 아무것도 쓰지 않고 4회 이상 연속으로 읽기 전용 호출(read_file, search_files, list_directory)을 했습니다. 4회 읽기 시점에 프록시가 쓰기를 유도하는 넛지를 주입하고, 5회 이상이면 강화된 넛지를 주입합니다. 읽기는 항상 실행됩니다 — 넛지는 다음 턴을 유도할 뿐, 읽기를 건너뛰지 않습니다.

**해결:** 모델이 정말로 탐색에 갇혀 있다면, 무엇을 바꾸고 싶은지 더 구체적으로 요청하세요.

---

## Geometric Lens 문제

### 요청 거부: 렌즈가 점수를 산출할 수 없음

**증상:** 요청이 즉시 HTTP 503 `dependency_down`으로 실패합니다: "ATLAS needs the geometric lens for every request, and it ... Run `atlas doctor`." 또는 실행이 `lens_unavailable`로 멈춥니다: "Stopped: ATLAS needs the geometric lens for every request, and it stopped answering (...)". 프록시 `/ready`에는 `"lens_ready": false`와 `lens_reason`이 표시되고, `atlas doctor`는 `status_dimensions`에서 `direct_agent: blocked`로 실패합니다.

**영향:** 렌즈가 점수를 산출할 수 있을 때까지 어떤 요청도 실행되지 않습니다. 이는 의도된 동작입니다 — 렌즈는 필수입니다([ADR 0011](../../adr/0011-the-lens-is-required.md)). 멈춘 실행은 멈추기 전에 파일을 변경했는지 여부를 함께 알려줍니다.

**해결:** 메시지가 원인을 지목합니다. 렌즈 헬스와 로그를 확인하세요:
```bash
curl -s http://localhost:8099/health | python3 -m json.tool
docker compose logs geometric-lens
```

| 메시지에 나오는 이유 | 해결 |
|---|---|
| unreachable | 렌즈 컨테이너가 떠 있지 않거나 `ATLAS_LENS_URL`이 잘못되었습니다. `docker compose ps geometric-lens`. |
| no C(x) / no G(x) model loaded | 서빙 중인 모델에 렌즈 가중치가 없습니다. 레지스트리 모델이라면 `atlas model install-artifacts <name>`을, 아니면 `atlas lens build`를 실행하세요([SETUP.md](SETUP.md#geometric-lens-가중치-필수)). |
| cannot reach llama-server | 렌즈가 모델 서버에 접근할 수 없습니다. `LLAMA_URL` / `LLAMA_EMBED_URL`과 llama-server 헬스를 확인하세요. |
| self-test failed | `/health`의 `self_test_error`를 확인하세요. |
| drifted from the served model | [임베딩 규약 드리프트](#점수는-그럴듯한데-스케일이-크게-어긋남-임베딩-규약-드리프트)를 참고하세요. |

프록시는 렌즈의 응답을 5초간 캐시하므로, 고쳐진 렌즈는 5초 안에 수용됩니다.

### 렌즈 미보정

**증상:** 시작 시 렌즈 로그에 `No gx_thresholds.json — Lens scores are uncalibrated; threshold interventions disabled`가 찍히고, 상태에 `lens_calibration: uncalibrated`가 표시됩니다.

**영향:** 요청은 실행됩니다. 렌즈는 원시 C(x) 에너지와 G(x) 확률을 산출하며, 보정이 필요한 용도(정규화된 라우팅, 거부 및 교정 임계값)는 해당 모델의 보정 파일이 생길 때까지 꺼져 있습니다.

**해결:** `atlas lens build`가 임계값을 보정하고 `cx_normalization.json`과 `gx_thresholds.json`을 번들에 기록합니다. [CLI.md § atlas lens](../../CLI.md#atlas-lens)를 참고하세요.

### 점수는 그럴듯한데 스케일이 크게 어긋남 (임베딩 규약 드리프트)

**증상:** 모든 것이 정상으로 보고됩니다 — 파드는 `Ready`, `/health`는 200, `gx-score`는 범위 안으로 보이는 `gx_score`와 `likely_correct` 판정을 반환합니다 — 그런데 `cx_energy`가 보정된 범위에서 자릿수 단위로 벗어나 있습니다(모델의 통과/실패 평균이 20~30인데 ~600 같은 값). 이 상태에서 시작한 게이트 벤치마크는 완전하고 그럴듯하지만 전적으로 무효한 결과를 만들어냅니다.

**원인:** 임베딩 서버가 Geometric Lens의 `C(x)`/`G(x)` 아티팩트가 학습된 것과 다른 `/embedding` 규약으로 응답하고 있습니다 — 보통 풀링 대신 토큰별, 또는 L2 정규화 대신 비정규화(‖v‖가 ~1이 아니라 ≈60). 차원은 같고 분포가 다르므로, 코스트 필드 MLP가 거대한 에너지로 외삽하고 `cx_normalized`가 포화됩니다. `--pooling mean` 없이 서빙 스택을 재빌드한 뒤에 발생합니다(llama-server에는 `--embd-normalize` 서버 플래그가 없습니다. 렌즈는 `/embedding` 본문의 `embd_normalize`로 호출마다 L2 정규화를 요청합니다).

**확인:** 아티팩트 옆에 `drift_fingerprint.json`이 있는 경우(아래 3단계 참고), 렌즈는 자체 테스트(부팅 시, 그리고 재시도 가능한 실패 후 `/ready`가 다시 실행할 때)에서 그 지문을 다시 채점합니다. `/ready`와 `/health`를 확인하세요:
```bash
curl -s http://localhost:8099/health | python3 -m json.tool | grep -A2 fingerprint
```
`fingerprint_ok: false`와 함께 기대값 대 관측값 에너지를 알려주는 `fingerprint_error`가 나오면 드리프트 신호입니다 — `/ready`는 503을 반환하고, 프록시는 요청을 거부하면서 드리프트를 명시하며, 채점된 응답에는 `"drifted": true`가 실리고 `calibrated` 플래그가 모두 false로 강제되며 임계값도 실리지 않으므로, 하류에서 이를 신뢰할 수 있는 값으로 오인하거나 그에 따라 동작할 수 없습니다.

**해결:**
1. 임베딩 서버의 규약을 확인하세요. 풀링 + 정규화된 서버는 ‖v‖≈1인 평탄한 벡터를 반환합니다:
   ```bash
   curl -s -X POST http://localhost:8080/embedding -H 'Content-Type: application/json' \
     -d '{"content":"def add(a, b): return a + b"}' | python3 -c "import sys,json,math; e=json.load(sys.stdin)[0]['embedding']; import itertools; v=e if not isinstance(e[0],list) else [sum(c)/len(e) for c in zip(*e)]; print('shape', 'per_token' if isinstance(e[0],list) else 'flat', 'norm', round(math.sqrt(sum(x*x for x in v)),3))"
   ```
   풀링된 `norm`은 수백 단위여야 합니다(제공되는 Gemma 아티팩트 기준 약 100-150). `norm`이 정확히 `1.0`이면 `embd_normalize: -1`에도 불구하고 서버가 벡터를 정규화한 것이며, 이 경우 C(x)는 모든 입력에 대해 약 0.8이라는 평탄한 값을 반환합니다. 정상처럼 보이지만 아무것도 구분하지 못하는 점수입니다. `cx_normalization.json`의 `pass_energy_mean`과 비교하세요: 서빙되는 에너지는 한 값에 머무르지 않고 그 대역에 걸쳐 분포해야 합니다.
2. `ATLAS_EMBED_POOLING=none`(기본값. [CONFIGURATION.md](../../CONFIGURATION.md) 참고)을 설정하고, 엔트리포인트가 플래그를 고정하도록 llama-server 컨테이너를 재생성하세요. `--pooling`은 llama.cpp에서 서버 전역 설정이며, 전체 텍스트 경로와 per-step 경로를 모두 지원하는 값은 `none`뿐입니다. 풀링과 스케일은 클라이언트 측에서 처리됩니다.
3. 서버가 올바른 규약으로 응답하면 부팅 자체 테스트가 통과하고 `/ready`가 200을 반환합니다. 재빌드(`atlas lens build`)는 `embedding_contract`를 `model_identity.json`에 새깁니다. 다만 드리프트 지문은 쓰지 않습니다: `geometric_lens.drift.write_fingerprint`가 존재하지만 아직 이를 호출하는 명령이 없으므로, 지문 검사는 `drift_fingerprint.json`을 직접 만들어 둔 경우에만 동작합니다.

### 임베딩 추출 실패

**증상:** Lens 로그에 "embedding extraction failed" 같은 오류나 타임아웃이 표시됩니다.

**원인:** Lens는 llama-server의 네이티브 `/embedding` 엔드포인트를 호출합니다. llama-server에 과부하가 걸리거나 임베딩이 활성화되지 않으면 실패합니다.

**해결:**
```bash
# Test the native embedding endpoint directly
curl -s http://localhost:8080/embedding \
  -H "Content-Type: application/json" \
  -d '{"content": "test"}' | python3 -m json.tool
```

`--embeddings` 플래그는 모든 배포 모드(Compose, 베어메탈, K3s)에서 llama-server 엔트리포인트가 설정합니다 — Geometric Lens가 셀프 임베딩에 의존하므로 항상 켜져 있습니다. 레이어별 hidden-states 확장을 실어 나르는 것도 네이티브 `/embedding` 경로입니다(`/v1/embeddings`가 아님).

### 후보가 `unscored`로 보고됨 (입력이 물리 배치를 초과)

**증상:** 렌즈 로그에 `unscored: the input of 2055 tokens exceeds the /embedding physical batch of 2048 tokens`가 찍히거나, v3-service가 어떤 후보에 대해 `lens_unscored`를 방출하거나(`kind: embed_capacity`, `input_tokens`, `capacity_tokens`), `atlas doctor`가 `ATLAS_MAX_TOKENS`보다 낮은 임베딩 용량을 지목하며 `lens_scoring: partial`을 표시합니다.

**원인:** llama-server는 하나의 임베딩 요청을 단일 물리 배치(`-ub`, `ATLAS_UBATCH`)로 처리하며, 그보다 긴 입력은 HTTP 500으로 거부합니다. 렌즈 점수는 전체 시퀀스에 대한 한 번의 순전파이므로, 렌즈는 입력을 자르거나 쪼개는 대신 해당 후보를 점수 없음으로 보고합니다: 쪼갠 입력은 뒤쪽 조각을 앞쪽 맥락 없이 임베딩하게 되어, 아티팩트가 보정된 그 벡터가 아니게 됩니다. 후보를 만들어내는 생성 예산(PlanSearch 코드 4,096 토큰, 쓰기 작업의 `ATLAS_MAX_TOKENS` 8,192)이 `atlas tier fit`이 고르는 가장 큰 마이크로 배치(2,048)보다 크므로, 기본 배포에서 긴 후보에 대해서는 예상되는 동작입니다.

**영향:** 해당 후보는 자신의 샌드박스 결과를 유지한 채 점수가 매겨진 모든 후보 뒤로 순위가 밀리며, 통과한 점수 후보가 하나도 없을 때만 전달되고 `selected` 이벤트가 그 사실을 알려줍니다. 그 자리에 0.0이나 0.5가 채점되는 일은 없습니다.

**확인:**
```bash
curl -s http://localhost:8099/health | python3 -c "import sys,json; l=json.load(sys.stdin)['subsystems']['lens']; print({k:v for k,v in l.items() if k.startswith('embed_capacity')})"
```
`embed_capacity_tokens`는 렌즈가 채점할 수 있는 가장 긴 입력이고, `embed_capacity_source`는 `declared`(`ATLAS_UBATCH`에서) 또는 `observed`(거부에서 관측, 이쪽이 우선)입니다.

**해결:** `ATLAS_UBATCH`를 올리면 용량이 늘어나지만, 계산 버퍼에 대략 `ubatch × n_embd × 280` 바이트의 VRAM 비용이 듭니다(3,840차원 모델에서 4,096일 때 약 4.4 GB). `atlas tier fit`으로 크기를 정하고 llama-server를 재생성한 뒤 `--fit off`로 기동되는지 확인하세요. `ATLAS_MAX_TOKENS`를 낮추면 프록시가 렌즈에 채점을 요청하는 쓰기 크기가 제한됩니다. 둘 중 무엇도 쪼갠 입력을 채점 가능하게 만들지는 않습니다. 물리 배치를 넘어서는 채점에는 [ADR 0010](../../adr/0010-lens-capacity-boundary-is-typed.md)이 설명하는 보정 작업이 필요합니다.

---

## Sandbox 문제

### Sandbox에 연결할 수 없음 (헬스 체크)

**증상:** 코드가 전혀 테스트되지 않습니다. 프록시 헬스에 `"sandbox": false`가 표시됩니다.

**해결:** 샌드박스 헬스를 확인하세요:
```bash
# Docker Compose (host port 30820 maps to container port 8020)
curl -s http://localhost:30820/health

# Bare metal (direct port 8020)
curl -s http://localhost:8020/health
```

샌드박스 컨테이너가 실행 중인데 unhealthy라면 로그를 확인하세요:
```bash
docker compose logs sandbox
```

### 코드 실행 타임아웃

**증상:** 샌드박스가 `"error_type": "Timeout"`을 반환합니다. 코드 실행에 너무 오래 걸립니다.

**기본 타임아웃:** 요청당 30초, `MAX_EXECUTION_TIME`으로 상한. Compose 스택은 긴 빌드와 테스트 스위트가 완료되도록 프록시의 `run_command` 상한에 맞춰 이 상한을 300초로 설정합니다(`.env`의 `ATLAS_SANDBOX_MAX_EXECUTION_TIME` 경유). compose 밖에서는 실행기의 코드 내 상한이 60초입니다.

**해결:** 코드가 정당하게 더 많은 시간이 필요하다면, 요청에서 더 높은 타임아웃을 설정하거나(상한까지) `ATLAS_SANDBOX_MAX_EXECUTION_TIME`을 올리세요. 코드에 무한 루프가 있는 경우라면 예상된 동작입니다. 타임아웃 시 프로세스 그룹 전체가 종료되므로, 명령이 낳은 자식 프로세스가 남지 않습니다.

### 지원되지 않는 언어

**증상:** 특정 언어에 대해 샌드박스가 오류를 반환합니다.

**지원 언어(실행):** Python, JavaScript, TypeScript, Go, Rust, C, C++, Bash. 구문 전용 검사(`/syntax-check`, V3 스모크 체크)는 HTML, XML, JSON, YAML도 추가로 커버합니다.

사용 가능한 런타임 확인:
```bash
curl -s http://localhost:30820/languages | python3 -m json.tool
```

---

## 벤치마크 문제

### bench가 요청보다 적은 태스크만 실행함 (`LIMITED MODE: running N tasks`의 N이 `--tasks`보다 작음)

**증상:** `atlas bench --tasks 200`이 `LIMITED MODE: running 100 tasks`(또는 요청보다 적은 수)를 보고하거나, 재개한 실행이 `Resuming: N/N tasks already done, 0 remaining`을 출력하고 즉시 종료됩니다.

**원인:** LiveCodeBench 데이터셋 캐시(`benchmark/datasets/.cache/livecodebench_v5.jsonl`)가 부분 다운로드 상태입니다. HuggingFace rows API는 페이지네이션 도중 실패할 수 있으며, 이전 버전은 받은 만큼만 캐시하고 그 파일을 영구히 신뢰했습니다. release_v5의 전체 세트는 약 880개 태스크입니다.

**해결:** 캐시를 partial로 표시하고 다시 실행하세요 — 로더가 전체 재다운로드를 시도합니다(모든 소스가 실패한 경우에만 기존 사본으로 폴백):
```bash
touch benchmark/datasets/.cache/livecodebench_v5.jsonl.partial
atlas bench --run-id <your-run-id> --tasks 200
```
완료된 태스크는 절대 손실되지 않습니다: 결과는 `benchmark/results/<run-id>/v3_lcb/per_task/`에 태스크별 JSON으로 저장되며, 러너는 결과 파일이 존재하는 태스크를 건너뛰며 재개합니다. 어떤 이유로든(OOM, 재부팅, 세션 종료) 중단된 실행도 같은 방식으로 재개됩니다 — 동일한 `atlas bench` 명령을 다시 실행하기만 하면 됩니다.

## 성능

### 느린 생성 속도 (~2 tok/s)

모델이 GPU 대신 CPU에서 실행되고 있습니다. 확인:
1. `nvidia-smi` — llama-server가 GPU 프로세스로 표시되는지
2. `-ngl 99`(`--n-gpu-layers`) — 모든 레이어가 오프로드되었는지
3. NVIDIA Container Toolkit — 컨테이너 런타임이 GPU 접근용으로 설정되었는지

**예상 성능:** 현재 기준 수치는 없습니다. 처리량은 모델, 양자화, 문법 모드에 따라 달라집니다. 이전의 약 51 tok/s 수치는 더 이상 존재하지 않는 구성에서 나온 것입니다.

### V3 파이프라인이 수 분 소요됨

T2 파일에 대해서는 정상입니다. V3 파이프라인은 여러 번의 LLM 호출을 수행합니다:
- **프로브만 (최상의 경우):** 약 10-15초 (생성 1회 + 스코어링 1회 + 테스트 1회)
- **Phase 1 생성:** 약 1-2분 (PlanSearch + DivSampling + 스코어링)
- **Phase 3 수리:** 약 2-5분 (필요 시 PR-CoT + Refinement)

더 빠른(그러나 품질이 낮은) 결과를 원한다면:
- 파일을 10줄 미만으로 유지 (T1 유지, V3 미실행) — 인식되는 코드 확장자는 10줄 이상이면 복잡도와 무관하게 T2가 됩니다
- 로직 복잡도 줄이기 (함수, 제어 흐름 감소)
- V3는 진정으로 필요할 때만 실행됩니다 — 단순 파일은 즉시 작성됩니다

### 높은 RAM 사용량

**증상:** 시스템이 느려지거나 서비스가 OOMKilled됩니다.

**예상 RAM 사용량:**
- llama-server: 약 8 GB (모델은 VRAM에, RAM은 최소)
- geometric-lens: 약 200 MB (PyTorch 런타임 + 모델)
- v3-service: 약 150 MB (PyTorch 런타임)
- sandbox: 약 100 MB (기본, 컴파일 중 급증)
- atlas-proxy: 약 30 MB (Go 바이너리)

**합계:** 약 500 MB RAM + 8.2 GB VRAM. 시스템 RAM이 14 GB 미만이면 다른 서비스와 메모리를 경합할 수 있습니다.

---

## 도움 받기

여기에 없는 문제라면:
1. 서비스 로그 확인: `docker compose logs <service-name>`
2. 프록시 헬스 엔드포인트 확인: `curl http://localhost:8090/health`
3. 모든 환경 변수는 [CONFIGURATION.md](../../CONFIGURATION.md) 참고
4. [GitHub](https://github.com/inferstep/ATLAS/issues)에 이슈 등록
