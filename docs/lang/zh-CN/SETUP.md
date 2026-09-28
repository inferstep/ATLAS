<!-- source: docs/SETUP.md synced-through: 4f1be83 -->
> **[English](../../SETUP.md)** | **简体中文** | **[日本語](../ja/SETUP.md)** | **[한국어](../ko/SETUP.md)**

> ℹ️ **译者注：** ATLAS 没有固定的默认模型 —— 请通过 `atlas init` 选择注册表模型，或自带兼容的 GGUF。若本译文与英文原版 ([SETUP.md](../../SETUP.md)) 有出入，以英文原版为准。


# ATLAS 安装指南

四种部署方式：**一键 bootstrap**（新安装推荐）、Docker Compose（手动）、裸机部署和 K3s。

---

## 选择你的安装路径

安装步骤取决于你的硬件 + 操作系统。找到匹配你环境的那一行，然后跳到对应的章节。

| 你的硬件 | 操作系统 | 推荐路径 | 支持级别（[矩阵](../../../SUPPORT_MATRIX.md)） |
|---|---|---|---|
| NVIDIA RTX 50 系列 / Blackwell（B100、GB10） | Linux | [方式 0：bootstrap](#方式-0一键-bootstrap) 或[方式 1：Docker](#方式-1docker-compose推荐) | 支持 (Supported) —— 发布的 CUDA 镜像面向 Blackwell |
| NVIDIA RTX 20/30/40、GTX 10xx、数据中心卡（V100/A100/H100/T4/L4） | Linux | [方式 1：Docker](#方式-1docker-compose推荐) + 一次性[本地重建](#cuda-计算能力-dockerfilev31) | 预览 (Preview) —— 需要本地重建 |
| NVIDIA GPU | Windows (WSL2) | [方式 1：Docker - NVIDIA 部分](#方式-1docker-compose推荐) | 不支持 (Unsupported) —— 未测试，不做任何承诺；欢迎反馈报告 |
| AMD GPU（RX 6000/7000、MI200+） | Linux | [方式 1：Docker - AMD ROCm](#amd-rocm-的差异) | 社区验证 (Community-tested)（[GH #26](https://github.com/itigges22/ATLAS/issues/26)） |
| **Apple Silicon (M1/M2/M3/M4)** | **macOS** | **[SETUP_MACOS.md](../../SETUP_MACOS.md)**（专门指南 - 原生 Metal + Docker 混合方案） | 支持（维护者已验证，M2 Pro） |
| Intel Arc / Iris Xe | Linux | [方式 1：Docker - Vulkan](#vulkan跨厂商回退) | 预览 —— Vulkan 仅在 lavapipe 上做过冒烟测试；尚无真实 GPU 验证 |
| Snapdragon X Elite（笔记本） | Linux | [Vulkan](#vulkan跨厂商回退) + [arm64 部分](#arm64) | 预览（Linux arm64）。Windows on ARM 为不支持 |
| 较老的 AMD GPU（Vega、RDNA1，无 ROCm 6.x） | Linux | [方式 1：Docker - Vulkan](#vulkan跨厂商回退) | 预览 |
| ARM64 上的 NVIDIA（DGX Spark、Jetson） | Linux | [arm64 部分](#arm64)（通过 sbsa/l4t 基础镜像替换启用 CUDA） | 预览 —— 提供了构建配方，尚无设备完成端到端验证 (#115) |
| Raspberry Pi 5 | Linux | [Vulkan + arm64](#arm64) | 预览 —— 预期只有 CPU 级性能 |
| Intel Mac（2020 年前） | macOS | [方式 1：Docker - Vulkan](#vulkan跨厂商回退) | 不支持 —— 需要 Docker Desktop（未测试）；Metal 仅限 Apple Silicon |
| 仅 CPU、无 GPU | 任意 | [仅 CPU 安装](#cpu-only) | 预览 —— 仅用于冒烟测试，非常慢 |
| Kubernetes 集群 | Linux | [方式 3：K3s](#方式-3k3s) | 预览 —— 模板经 CI 校验；没有自动化的真实集群测试 |
| 裸机 / 开发环境 | Linux | [方式 2：裸机](#方式-2裸机) | 预览 —— 仅手动验证 |

没有找到你的环境？请附上 `uname -a` 输出和 `lspci | grep -i vga`（Linux）/ `system_profiler SPDisplaysDataType`（Mac）提交一个 issue，我们会补上对应的行。

---

## 方式 0：一键 bootstrap

一条 curl 命令即可检测发行版、安装 Docker + nvidia-container-toolkit、下载模型权重并拉起整个栈。幂等 —— 可以安全地重复运行。

> **NVIDIA 前 Blackwell GPU（RTX 20/30/40 系列、GTX 10xx、V100/A100/T4/L4/H100）：请先读这里。**
> 发布的 `atlas-llama` CUDA 镜像**只**针对计算能力
> `120;121`（Blackwell —— RTX 50xx、B100、GB10）编译。在更早的 NVIDIA GPU 上，
> llama-server 启动时会失败并报
> `no kernel image is available for execution on the device`。
> 请针对你 GPU 的架构一次性重建推理镜像：
>
> ```bash
> # find your arch (drop the dot: 8.6 -> 86)
> nvidia-smi --query-gpu=compute_cap --format=csv,noheader
> docker compose build --build-arg CUDA_ARCH=86 llama-server   # example: RTX 30xx
> docker compose up -d --no-deps llama-server
> ```
> 完整架构表：[CUDA 计算能力](#cuda-计算能力-dockerfilev31)。

```bash
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

或者从已有的检出中运行：
```bash
./scripts/atlas-bootstrap.sh
```

**支持的发行版：**

| 家族 | 发行版 |
|---|---|
| Debian (apt-get) | Ubuntu 20.04+、Debian 11+ |
| RHEL (dnf) | RHEL 9+、Rocky 9+、AlmaLinux 9+、CentOS Stream 9+、Oracle Linux 9+ |
| Fedora (dnf) | Fedora 38+ |

`ID_LIKE` 与上述之一匹配的其他发行版（如 Linux Mint、Pop!_OS）会在给出警告后被接受。不在此列表中的发行版 —— Arch、openSUSE、Alpine、NixOS —— 未经测试，脚本会拒绝在其上运行。

bootstrap 会绕过这些坑：EPEL、nouveau 驱动冲突、libnvidia-ml.so.1 缺失的情况（RHEL 最小安装），以及"用户已加入 docker 组但当前 shell 还看不到"的竞态。

**模型选择：** `.env.example` 出厂时不预选任何模型。当 bootstrap 创建 `.env` 且 `ATLAS_MODEL_FILE` 为空时，它会把注册表默认推荐的模型写入 `.env`（写入时会打日志），使一键安装流程无需向导即可完成。之后可随时通过编辑 `.env` 或运行 `atlas init` 更改选择。已有的非空选择会被尊重。

<a id="cpu-only"></a>
**仅 CPU / 无 GPU 主机（预览 (Preview) —— 仅用于冒烟测试）。** ATLAS 可以在没有 GPU 的情况下通过 Vulkan 镜像的 lavapipe CPU 光栅化器启动，但推理非常慢；请把它用来验证栈能否工作，而不是用于真实的编码会话。

1. **bootstrap 会拒绝无 GPU 的主机，除非你显式选择加入：**
   `ATLAS_BOOTSTRAP_SKIP_GPU=1 ./scripts/atlas-bootstrap.sh`
   它会叠加 `docker-compose.vulkan.yml`（当 `/dev/dri` 不存在时再叠加
   `docker-compose.cpu.yml`），自行把模型选择和
   `ATLAS_BACKEND=vulkan|cpu` 写入 `.env`，并跳过 ASA 构建。
2. **不要在无 GPU 的主机上运行 `atlas init`** —— 向导会有意拒绝（退出码 1），
   而不是写出一份它无法定容的 `.env`。模型选择由 bootstrap 处理；之后可通过
   `atlas model install` 更换模型。

手动等价操作：

```bash
cp .env.example .env    # set ATLAS_MODEL_FILE / ATLAS_MODEL_NAME
atlas model install Qwen3.5-9B-Q6_K
docker compose -f docker-compose.yml -f docker-compose.vulkan.yml -f docker-compose.cpu.yml up -d
atlas doctor            # gpu check WARNS ("CPU-only mode — very slow"); warns exit 0
```

**防火墙：** Compose 栈把所有服务只发布在 `127.0.0.1` 上，因此本地使用不需要任何防火墙改动，bootstrap 默认不碰 firewalld。设置 `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` 可为把服务重新绑定到可路由接口的部署打开服务端口（8090、8099、8070、30820）。

**运行模式 —— 两种都可以：**

```bash
# Run as your normal user; sudo elevates as needed (Docker install, etc).
# Install ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | bash

# Run via sudo. SUDO_USER is detected, install still ends up owned by you.
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | sudo bash

# Real root login (no sudo) — install owned by root. Only do this if there's
# no human user on the box (CI runner, container, etc).
```

**谨慎安装的变体**（同一个脚本；适合不愿把一个随 `main` 变动的脚本直接管道进 bash 的人）：

```bash
# Pinned to a release: fetch the script AT the tag and install that tag.
# The checkout is pinned to the (SSH-signed) tag and ATLAS_IMAGE_TAG is
# pinned to the matching cosign-signed images.
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/v3.1.3/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.3 bash

# Review before running: download, read, then execute the same bytes.
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

**配置用环境变量：**

| 标志 | 效果 |
|---|---|
| `ATLAS_BOOTSTRAP_SKIP_DOCKER=1` | 不安装 Docker（已由别处管理） |
| `ATLAS_BOOTSTRAP_SKIP_GPU=1` | 跳过 GPU 运行时安装（NVIDIA toolkit 或 ROCm）。 |
| `ATLAS_BOOTSTRAP_SKIP_MODELS=1` | 不下载模型权重 |
| `ATLAS_BOOTSTRAP_SKIP_COMPOSE=1` | 不运行 `docker compose up` |
| `ATLAS_BOOTSTRAP_SKIP_ASA=1` | 跳过 ASA 操控向量构建（默认：服务启动约 5 分钟后构建；无 GPU 可用时自动跳过） |
| `ATLAS_BOOTSTRAP_OPEN_FIREWALL=1` | 在 firewalld 中打开服务端口（默认：关闭 —— 服务绑定回环地址） |
| `ATLAS_BOOTSTRAP_NO_SUDO=1` | 直接失败而不尝试 sudo |
| `ATLAS_BOOTSTRAP_REF=vX.Y.Z` | 把安装固定到某个 git 标签/sha 而不是跟踪 `main`；`vX.Y.Z` 形式的值还会把 `ATLAS_IMAGE_TAG` 固定到对应的镜像 |
| `ATLAS_INSTALL_DIR=/path` | 克隆到哪里（默认 `/opt/atlas` —— 见下文） |
| `ATLAS_REPO_URL=https://...` | 替代的仓库 URL |
| `ATLAS_GO_VERSION=1.26.2` | 为构建 TUI 安装的 Go 工具链版本（TUI 需要 1.26.2+；已安装的更老工具链会自动拉取它） |

**为什么是 `/opt/atlas`？** 它是系统级第三方软件的标准 FHS 前缀，不会被 `$HOME` 清理波及，还能让同一台机器上的多个用户共享一份安装。如果你更希望装进自己的家目录：

```bash
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh \
  | ATLAS_INSTALL_DIR=$HOME/atlas bash
```

完成后会打印一条绿色的 "ATLAS ready" 横幅和快速上手命令。在网络快的全新虚拟机上总耗时约 10-30 分钟（模型下载占大头）。

如果你更愿意手动完成每一步，请使用下面的方式 1。

---

## 前置要求（所有方式通用）

| 要求 | 详情 |
|-------------|---------|
| **GPU** | 16 GB+ 显存。NVIDIA（CUDA，支持 (Supported) —— 发布的镜像面向 Blackwell；更早的显卡需要一次性[本地重建](#cuda-计算能力-dockerfilev31)）；AMD（ROCm，社区验证 (Community-tested)）；Apple Silicon（Metal，macOS 混合方案，支持 —— 见 [SETUP_MACOS.md](../../SETUP_MACOS.md)）；Vulkan（预览 (Preview)）是跨厂商回退方案；Intel Arc（SYCL）为路线图 (Roadmap) 级别。参见 [§ 支持的 GPU](#支持的-gpu)。 |
| **GPU 驱动** | NVIDIA：专有驱动（`nvidia-smi` 应能显示你的 GPU）。AMD：`amdgpu-dkms` 内核驱动（`/dev/kfd` 必须存在；`rocm-smi` 应能显示你的 GPU）。 |
| **Python 3.9+** | 含 pip |
| **curl** | 用于下载模型权重 |
| **模型权重** | 一个适合本机的注册表模型或自带 GGUF。`atlas init` 会推荐一个并把选择写入 `.env`。 |

### 验证 GPU

**NVIDIA：**

```bash
nvidia-smi
# Should show your GPU with driver version and VRAM
# If this fails, install NVIDIA proprietary drivers first
```

**AMD：**

```bash
rocm-smi --showproductname --showmeminfo vram
# Should show your GPU model and total VRAM
# If rocm-smi is missing or /dev/kfd doesn't exist, install ROCm:
#   RHEL 9: sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
#           sudo amdgpu-install --usecase=dkms,rocm
#   Ubuntu: Follow https://rocm.docs.amd.com/projects/install-on-linux/
# Then REBOOT.
```

**自动检测** —— 让 `atlas tier` 跨厂商自动检测并告诉你它发现了什么：

```bash
pip install -e .
atlas tier              # prints detected GPU, tier classification, recommended settings
atlas tier --json       # machine-readable (used by atlas init wizard)
```

---

## 方式 1：Docker Compose（推荐）

这是被运用得最充分的部署方式：CI 会校验 compose 文件并以确定性方式（伪推理）驱动完整控制平面，每次发布都会在真实硬件上以 Compose 做冒烟测试。真实的 GPU 推理行为在下方硬件表列出的显卡上验证，而不是在 GitHub 托管的 CI 中。

### 额外前置要求

**NVIDIA (CUDA)：**
- **Docker** 配合 [nvidia-container-toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/latest/install-guide.html)，**或 Podman** 配合同一工具包
- 约 20 GB 磁盘空间（模型权重 + 容器镜像）

**AMD (ROCm)：**
- 只需 **Docker** —— ROCm 不需要单独的容器运行时；通过 `--device=/dev/kfd --device=/dev/dri` 直通即可
- 你的用户必须在 `video` 和 `render` 组中：`sudo usermod -aG video,render $USER`（然后重新登录）
- 约 22 GB 磁盘空间（ROCm 镜像比 CUDA 版大约 2 GB）

### 安装步骤

```bash
# 1. Clone
git clone https://github.com/itigges22/ATLAS.git
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

# 4. Install Go 1.26.2+ — required for the TUI client (atlas tui) and
#    optional for the proxy (proxy builds automatically on first run if Go
#    is present; otherwise it runs in Docker with file access limited to
#    ATLAS_PROJECT_DIR). Quickest path:
mkdir -p /tmp/go-install && cd /tmp/go-install
curl -LO https://go.dev/dl/go1.26.2.linux-amd64.tar.gz
sudo rm -rf /usr/local/go && sudo tar -C /usr/local -xzf go1.26.2.linux-amd64.tar.gz
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

#### AMD ROCm 的差异

ROCm 路径与 NVIDIA 完全一致，*除了*以下三点：

1. **用两个 compose 文件一起拉起**（或者让 `atlas init` 替你完成）：
   ```bash
   docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
   ```
   该 override 把 llama-server 镜像切换为 ROCm 构建，把 NVIDIA 驱动请求换成 `/dev/kfd` + `/dev/dri` 直通，并强制 `ATLAS_BACKEND=rocm`，使入口点走 HIP 调优分支。

2. **不需要 `nvidia-container-toolkit`** —— ROCm 不需要单独的容器运行时，只需要内核级设备访问。确认你的用户在正确的组中：
   ```bash
   id -nG | tr ' ' '\n' | grep -E '^(render|video)$'
   # Should print both. If not:
   sudo usermod -aG video,render $USER
   # Then log out + back in (or: newgrp render)
   ```

3. **GPU 计算目标。** 默认的 `Dockerfile.rocm` 构建是覆盖 RDNA3（7000 系列）、RDNA2（6000 系列）和 CDNA2（MI200）的"胖"镜像 —— `gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`。想要针对你特定 GPU 的更小镜像，在构建前设置 `ATLAS_GFX_TARGET`：
   ```bash
   # Example: only build for RX 7900 XT/XTX
   ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
   ```
   你的显卡对应的 gfx 目标见 [LLVM AMDGPU 处理器表](https://llvm.org/docs/AMDGPUUsage.html)。

对于"我的 GPU 不受支持但 ROCm 大致能跑"的情形（较老的 Vega、RDNA1），`ATLAS_HSA_OVERRIDE_GFX_VERSION` 变通方案见 [TROUBLESHOOTING.md § AMD GPU not detected](../../TROUBLESHOOTING.md)。

#### Vulkan：跨厂商回退

当你的硬件没有打包好的原生厂商后端时（Intel Arc、Snapdragon Adreno、没有 ROCm 6.x 的较老 AMD 卡），Vulkan 是回退方案。一个 Dockerfile 覆盖 AMD（Mesa RADV）、Intel（Mesa ANV）、NVIDIA（nvidia-container-toolkit）、Apple（macOS Docker 里的 MoltenVK）、Snapdragon（Adreno）和 CPU（Mesa lavapipe）。

代价：通常比调优过的原生后端慢 20–40%。当 CUDA/ROCm 不可用时使用它，或者用来冒烟测试 ATLAS 能否在不常见的硬件上启动。

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

与 CUDA/ROCm 相比的差异：

1. **不要求厂商特定的内核驱动。** Vulkan ICD 位于镜像内部（`mesa-vulkan-drivers` 覆盖 AMD/Intel/CPU；NVIDIA 的 ICD 来自 nvidia-container-toolkit 挂载）。
2. **仅 `/dev/dri` 直通** —— 没有 `/dev/kfd`，没有 `--gpus all`（除非你走 NVIDIA toolkit 路由，那样两者仍然可用）。
3. **按 GPU 选择用 `ATLAS_VK_DEVICE_SELECT`**，而不是 `CUDA_VISIBLE_DEVICES` / `HIP_VISIBLE_DEVICES`。格式为 Mesa 标准：`"vendorID:deviceID"`（十六进制）或设备名的子串。`GGML_VK_VISIBLE_DEVICES`（数字索引）也可用。
4. **`atlas doctor`** 会运行一个 `_check_vulkan_via_docker` 探测 —— 但仅当设置了 `ATLAS_BACKEND=vulkan` 时（否则跳过，以保持 CUDA/ROCm 检查的速度）。

如果 `vulkaninfo` 在你预期有 GPU 时只显示 `llvmpipe` CPU 设备，说明内核侧设备直通失败了 —— 确认主机上存在 `/dev/dri/renderD*`，且你的用户在 `video` + `render` 组中（与上面的 ROCm 要求相同）。

<a id="arm64"></a>
#### arm64 主机 (#115)

ATLAS 面向两种 CPU 架构：`x86_64`（默认，所有后端可用）和 `aarch64`（后端子集）。用 `atlas doctor` 验证 —— `arch` 检查会在 GPU 检查触发之前给出你的架构以及其上可用的后端。

**按架构划分的后端可用性：**

| 后端 | x86_64 | aarch64 | 备注 |
|---|---|---|---|
| CUDA | 是（rockylinux9 基础镜像） | 是（sbsa 或 l4t 基础镜像，build-arg 替换） | DGX Spark = sbsa，Jetson = l4t |
| ROCm | 是 | **否** | AMD 没有 arm64 的 ROCm 发行版。请改用 Vulkan。 |
| Vulkan | 是 | 是（Mesa 是多架构的） | 所有 arm64 GPU 的通用回退 |
| CPU (lavapipe) | 是 | 是 | 慢，但总能工作 |

**目标 arm64 设备：**

- **NVIDIA DGX Spark**（Grace-Blackwell GB10）—— 通过 sbsa 基础镜像启用 CUDA，计算能力 12.0/12.1
- **NVIDIA Jetson Orin / AGX / Nano** —— 通过 l4t 基础镜像启用 CUDA，计算能力 8.7
- **Apple Silicon (M1/M2/M3/M4)** —— Docker Desktop 中通过 MoltenVK 走 Vulkan（慢速路径）；原生 Metal 安装在 [#32](https://github.com/itigges22/ATLAS/issues/32) 跟踪（快速路径）
- **Snapdragon X Elite**（Windows on ARM 笔记本）—— 通过 Adreno 驱动走 Vulkan
- **Raspberry Pi 5** —— 通过 Mesa V3D 驱动走 Vulkan，预期 CPU 级性能
- **Ampere Altra / AWS Graviton 工作站** —— 通过 lavapipe 走 Vulkan（CPU 回退，因为目前还没有消费级 arm64 独显）

**为 arm64 构建 Vulkan 镜像：**

```bash
# Multi-arch build that produces a single image manifest covering both archs:
docker buildx build --platform linux/amd64,linux/arm64 \
  -t atlas-llama-server:vulkan \
  -f inference/Dockerfile.vulkan inference/
```

**为 arm64 构建 CUDA 镜像**（DGX Spark 示例）：

```bash
# Swap to the sbsa-capable ubuntu base, build with --platform linux/arm64:
docker buildx build --platform linux/arm64 \
  --build-arg BUILDER_IMAGE=nvidia/cuda:12.9.0-devel-ubuntu22.04 \
  --build-arg RUNTIME_IMAGE=nvidia/cuda:12.9.0-runtime-ubuntu22.04 \
  -t atlas-llama-server:cuda-arm64 \
  -f inference/Dockerfile.v31 inference/
```

Jetson 则在两个 build arg 中换成 `nvcr.io/nvidia/l4t-jetpack:r36.3.0`（l4t 把 JetPack + CUDA + cuDNN 打包为一个镜像）。

**已知缺口（由 #115 跟踪）：**

- GHCR 上还没有预构建的 arm64 镜像 —— arm64 用户需按上面的配方本地构建。至少一台 arm64 设备完成端到端验证后，预构建的多架构镜像就会落地。
- bootstrap 安装器（`scripts/atlas-bootstrap.sh`）尚未针对 arm64 路径审计过。
- 五种目标设备的硬件测试矩阵目前是空的 —— 拥有其中任一设备的早期使用者，请把你的 `atlas doctor` 输出和 `vulkaninfo --summary` 贴到 [#115](https://github.com/itigges22/ATLAS/issues/115)。

### 首次运行会发生什么

1. Docker 从 `ghcr.io/itigges22/atlas-{proxy,v3,lens,llama,sandbox}` 拉取 5 个预构建容器镜像（网络快时约 3 分钟）。若想改为从源码构建（开发路径），在 `up` 之前运行 `docker compose build` —— 见下方"镜像来源"。
2. llama-server 把 7GB 模型加载进 GPU 显存（约 1-2 分钟）
3. 所有服务开始健康检查
4. 当全部 5 个服务（llama-server、geometric-lens、v3-service、sandbox、atlas-proxy）报告健康后，`atlas` 连接并启动 Bubbletea TUI

后续的 `docker compose up -d` 启动很快（几秒），因为镜像已被缓存。

### 镜像来源：预构建 vs 从源码构建

`docker-compose.yml` 为每个服务同时声明了 `image:`（GHCR）和 `build:`（本地 Dockerfile）。Compose 的默认行为：

| 命令 | 行为 |
|---------|--------------|
| `docker compose up -d`            | 本地缓存没有时拉取 `image:`，否则复用本地 |
| `docker compose pull`             | 强制从 GHCR 拉取最新标签（覆盖本地缓存） |
| `docker compose build`            | 从 `Dockerfile` 构建（覆盖 GHCR 镜像） |
| `docker compose up -d --build`    | 总是先从源码重建再启动 |

**标签固定。** 标签默认为 `latest`。要固定到特定版本（生产环境推荐），在 `.env` 中设置 `ATLAS_IMAGE_TAG`：

```env
ATLAS_IMAGE_TAG=3.1.3      # semver tag from a git release
ATLAS_IMAGE_TAG=sha-abc1234  # exact commit
ATLAS_IMAGE_TAG=dev          # bleeding edge from dev branch
```

可用标签列在 <https://github.com/itigges22/ATLAS/pkgs/container/atlas-proxy>（把 `atlas-proxy` 换成其他服务名：`atlas-v3`、`atlas-lens`、`atlas-llama`、`atlas-sandbox`）。

边缘情形：对 GHCR 上仍为私有的包，`compose pull` 会以 `unauthorized` 失败 —— 用带 `read:packages` 的 token 认证，或改为从源码构建。`compose pull` 也会覆盖共享同一标签的本地构建镜像；迭代某个服务时，跳过 pull 或设置 `ATLAS_IMAGE_TAG=dev-local`，让本地镜像和注册表镜像各用各的标签。要拉取某个 fork 的镜像，在 `.env` 中设置 `ATLAS_GHCR_OWNER=<your-username>`。

### 验证安装

最快的方式是 **`atlas doctor`** —— 检查主机环境（GPU 运行时、模型与 lens 工件）、docker 栈（容器、健康端点、认证、状态）以及一次真实模型推理，每项结果完成即打印，健康时返回退出码 0，有失败时返回 1。具体检查项数量随后端、栈状态和标志而变化。`atlas-bootstrap.sh` 在安装结束时运行的也是它。

```bash
atlas doctor              # full check (~5–10s)
atlas doctor --quick      # skip the e2e model inference (~2s)
atlas doctor --json       # machine output, for scripts/CI (buffered, one JSON document)
atlas doctor -v           # verbose: show detail for each check
```

各项检查：

| 分组 | 检查 | 确认内容 |
|---|---|---|
| Host | docker | 守护进程可达 |
| Host | compose | 已安装 docker compose v2 |
| Host | arch | CPU 架构（`x86_64` / `aarch64`）及其上可用的后端 (#115) —— 始终运行，在 GPU 检查之前 |
| Host | gpu | 厂商感知的 GPU 运行时：NVIDIA（nvidia-container-toolkit 在 Docker 内运行 nvidia-smi）或 AMD（`/dev/kfd` 直通）；未检测到 GPU 时给出警告 |
| Host | vulkan | Docker 内可见 Vulkan ICD —— 仅当 `ATLAS_BACKEND=vulkan` |
| Host | metal-native | 原生 llama-server 二进制存在且可执行 —— 仅当 `ATLAS_BACKEND=metal`（macOS 混合方案） |
| Host | model_file | `.env` 中选定的 `ATLAS_MODEL_FILE` 存在且大于 100 MB |
| Host | lens_weights | `cost_field.pt` + G(x) 工件存在 |
| Host | asa_steering | `ast_edit_steering.gguf` 存在（BiasBusters #4 —— 警告而非失败；没有它 ATLAS 也能工作，只是 structural_edit-vs-edit_file 的偏差不受操控） |
| Host | tier_match | `.env` 的模型选择与主机硬件匹配（超配时警告 —— 有 OOM 风险 —— 匹配或低配时通过） |
| Host | tier_constraints | 主机 CPU/RAM/磁盘满足推荐 tier 的最低要求（抓住"16 GB GPU 却只有 8 GB RAM"的错配） |
| Stack | container/llama-server, geometric-lens, v3-service, sandbox, atlas-proxy | 全部 5 个都在运行且健康 |
| Stack | health/llama, lens, v3, sandbox, proxy | 全部 5 个 `/health` 端点返回 ok |
| Stack | internal_auth | 内部服务认证：token 文件存在且权限收紧，并从两个方向实际探测在线校验（错误 token → 401，有效 token 被接受）；认证被禁用时（没有 `secrets/service-token`）给出警告 |
| Stack | status_dimensions | 信息性检查：来自代理 `/v1/calibration/status` 的七个 lens/ASA 状态维度（与 TUI 徽标读取的是同一来源）；从不导致运行失败 |
| Stack | image_skew | 全部 5 个 `atlas-*` 镜像在同一标签上 |
| End-to-end | e2e_smoke | 到 llama-server 的一次真实 `/v1/chat/completions` 往返（`--quick` 可跳过） |

`vulkan` 和 `metal-native` 两行依赖于所配置的后端；health、`internal_auth` 和 `status_dimensions` 各行仅在至少有一个容器运行时才执行；`e2e_smoke` 会被 `--quick` 跳过。其余检查始终运行。

如果你更想手动检查：

```bash
# Hit each health endpoint
curl -s http://localhost:8080/health | python3 -m json.tool   # llama-server
curl -s http://localhost:8099/health | python3 -m json.tool   # geometric-lens
curl -s http://localhost:8070/health | python3 -m json.tool   # v3-service
curl -s http://localhost:30820/health | python3 -m json.tool  # sandbox
curl -s http://localhost:8090/health | python3 -m json.tool   # atlas-proxy

# 功能测试：完整安装诊断（服务、工件、e2e 冒烟测试）
atlas doctor
```

所有健康检查端点应返回 `{"status": "ok"}` 或 `{"status": "healthy"}`。

> **注意：** 在交互式终端中直接运行 `atlas` 会启动 Bubbletea TUI，运行完整的 agent 循环（工具调用、V3 pipeline、文件读写）。TUI 需要真实终端 —— 当 stdin/stdout 被管道重定向时，它会打印指向 `atlas doctor` 的提示并退出。

### 停止服务

```bash
docker compose down          # Stop all services (preserves images)
docker compose down --rmi all  # Stop and remove images (next start rebuilds)
```

### 查看日志

```bash
docker compose logs -f llama-server    # Follow llama-server logs
docker compose logs -f geometric-lens  # Follow Lens logs
docker compose logs -f v3-service      # Follow V3 pipeline logs
docker compose logs -f atlas-proxy     # Follow proxy logs
docker compose logs -f sandbox         # Follow sandbox logs
docker compose logs --tail 50          # Last 50 lines from all services
```

### 更新

```bash
git pull
docker compose down
docker compose pull          # grab fresh :latest images from GHCR
docker compose up -d
```

### 卸载

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

K3s 安装改用 `scripts/uninstall.sh`，它会拆除清单文件并（可选地）卸载 K3s 节点本身。

---

## 方式 2：裸机

将所有服务作为本地进程运行，无需容器。适用于开发环境或无法使用 Docker 的系统。

### 额外前置要求

| 要求 | 详情 |
|-------------|---------|
| **Go 1.26.2+** | 用于构建 atlas-proxy 和 atlas-tui 客户端（更老的 Go 工具链会自动拉取它） |
| **llama.cpp** | 从源码编译并启用 CUDA（参见 [llama.cpp 构建说明](https://github.com/ggml-org/llama.cpp?tab=readme-ov-file#build)） |
| **Node.js 20+** | 沙箱执行 JavaScript/TypeScript 所需 |
| **Rust** | 沙箱执行 Rust 所需 |

### 构建

```bash
# 1. Clone and install Python CLI
git clone https://github.com/itigges22/ATLAS.git
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

### 启动服务

在不同的终端中分别启动每个服务（或使用 `&` 并重定向到日志文件）：

```bash
# Terminal 1: llama-server (GPU)
llama-server \
  --model "models/$ATLAS_MODEL_FILE" \
  --host 0.0.0.0 --port 8080 \
  --ctx-size 32768 --n-gpu-layers 99 --no-mmap \
  --embeddings --pooling mean --flash-attn on --fit off

# Terminal 2: Geometric Lens
cd geometric-lens
LLAMA_URL=http://localhost:8080 \
LLAMA_EMBED_URL=http://localhost:8080 \
GEOMETRIC_LENS_ENABLED=true \
PROJECT_DATA_DIR=/tmp/atlas-projects \
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

> **注意：** 裸机模式下沙箱监听端口为 **8020**（没有 Docker 端口映射）。代理的 `ATLAS_SANDBOX_URL` 必须使用端口 8020，而非 30820。

### 启动 TUI

`atlas` 命令是 Python 包的控制台入口点，已由构建步骤中的 `pip install -e .` 安装 —— 无需单独的启动脚本。在上述服务运行的情况下：

```bash
cd /path/to/your/project
atlas    # Checks atlas-proxy is reachable, then launches the TUI
```

如果 `atlas-tui` 二进制缺失或比检出代码旧，`atlas` 会自动从 `tui/` 构建它（需要 PATH 中有 Go 1.26.2+），并在移交给 TUI 之前验证 localhost:8090 上的代理。

---

## 方式 3：K3s

支持 GPU 调度、健康探针和资源限制的 Kubernetes 部署。预览 (Preview) 级别 —— 模板在 CI 中校验并渲染；没有自动化的真实集群测试。

### 额外前置要求

| 要求 | 详情 |
|-------------|---------|
| **K3s** | 单节点或多节点集群 |
| **NVIDIA GPU Operator** 或 **device plugin** | GPU 必须作为 `nvidia.com/gpu` 资源可见 |
| **Helm** | 用于安装 GPU Operator |
| **Podman 或 Docker** | 用于构建容器镜像 |

### 自动安装

安装脚本负责完整的安装流程 —— K3s 安装、GPU Operator、容器构建和部署：

```bash
# 1. Configure
cp atlas.conf.example atlas.conf
# Edit atlas.conf: model paths, GPU layers, context size, NodePorts

# 2. Run the installer (requires root)
sudo scripts/install.sh
```

安装程序将：
1. 检查前置要求（NVIDIA 驱动、GPU 显存、系统内存）
2. 如果 K3s 尚未运行则安装
3. 通过 Helm 安装 NVIDIA GPU Operator（如果 GPU 对集群不可见）
4. 构建容器镜像并导入 K3s containerd
5. 通过 envsubst 从 `atlas.conf` 生成清单文件
6. 部署到 `atlas` 命名空间
7. 等待所有服务变为健康状态

### 手动部署

如果 K3s 已在运行且支持 GPU：

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

### K3s 专属配置

K3s 使用 `atlas.conf`（而非 `.env`）进行配置。HTTP 契约与流水线行为和 Docker Compose 完全一致，仅部署接线不同：

| 配置项 | Docker Compose | K3s |
|---------|---------------|-----|
| 配置文件 | `.env` | `atlas.conf` |
| 服务暴露方式 | 主机端口（`8090`、`8080`、`8099`、`8070`、`30820`） | NodePort（`30080`、`32735`、`31144`、`30070`、`30820`） |
| 项目工作区 | 绑定挂载（`ATLAS_PROJECT_DIR` → `/workspace`） | `hostPath`（`ATLAS_PROJECTS_DIR` → 每个需要的 Pod 的 `/workspace`） |
| 模型文件 | 绑定挂载（`ATLAS_MODELS_DIR` → `/models:ro`） | GPU 节点上的 `hostPath`（`ATLAS_MODELS_DIR`，`Directory`，只读） |
| 有状态存储 | 命名卷（`v3-telemetry`） | PVC（`lens-projects` 由 `ATLAS_PVC_PROJECTS_SIZE` 指定大小；仍会挂载，但自项目索引器移除后已不再使用） |
| GPU 分配 | `deploy.resources.reservations.devices`（nvidia） | `resources.limits.nvidia.com/gpu: 1`（需要 GPU Operator 或设备插件） |
| 沙箱工具链缓存 | 按语言的 `tmpfs` 挂载 | 按语言、带 `sizeLimit` 的 `emptyDir`（通用模式，集合相同） |

模型与运行时参数（`ATLAS_MAIN_MODEL`、`ATLAS_CONTEXT_LENGTH`、`ATLAS_PARALLEL_SLOTS`、`ATLAS_FLASH_ATTENTION`、KV 缓存量化、用于 lens 评分路径的 `--embeddings`）在两种模式下读取相同的环境变量 —— 参见 `atlas.conf.example` 与 `.env.example`。

完整的 `atlas.conf` 参考请参见 [CONFIGURATION.md](../../CONFIGURATION.md)。

### 验证 K3s 部署

```bash
# Check pods
kubectl get pods -n atlas

# Check GPU allocation
kubectl describe nodes | grep nvidia.com/gpu

# Run verification suite
scripts/verify-install.sh
```

> **注意：** Docker Compose 是被运用得最充分的部署方式（CI 针对它运行；每次发布都在 Compose 下做冒烟测试）。K3s 清单在部署时由 `scripts/generate-manifests.sh`（或 `install.sh` 的 `process_templates` 步骤）从 `templates/*.yaml.tmpl` 生成。模板使用 `atlas.conf` 中选定的模型；CHANGELOG 中的基准测试数字记录的是它们各自冻结的模型/配置。

---

## 硬件规格

ATLAS 把 GPU 划分为 5 个 tier，并为每个 tier 推荐一个注册表模型 + 上下文大小 + 并行 slot 配置。这些是当前注册表的推荐，而非硬编码的运行时要求。运行 `atlas tier` 查看你的硬件落在哪个 tier，以及应使用的确切 `.env` 值。

| Tier | 显存 | 推荐模型 | 上下文 | Slot 数 | 示例 GPU |
|------|------|-------------------|--------:|------:|--------------|
| **cpu** | 不适用 | [仅 CPU 安装](#cpu-only) —— 预览 (Preview)，仅用于冒烟测试 | 不适用 | 不适用 | （无 GPU） |
| **small** | 8–12 GB | Qwen3.5 7B Q4_K_M (4.4 GB) | 8K | 1 | RTX 3060/4060 8GB、T4 |
| **medium** | 12–20 GB | Qwen3.5 9B Q6_K (6.9 GB) | 32K | 1 | RTX 4060/5060 Ti 16GB、3080 Ti、4070 Ti Super |
| **large** | 20–32 GB | Qwen3.5 14B Q5_K_M (10.5 GB) | 32K | 2 | RTX 3090、4090、5090 24GB |
| **xlarge** | 32 GB+ | Qwen3.5 32B Q5_K_M (23 GB) | 64K | 2 | RTX 5090 32GB、A6000、A100、H100 |

```bash
atlas tier              # classify this host + show recommendations
atlas tier list         # show all 5 tier definitions
atlas tier fit          # size the runtime for the CONFIGURED model + GPU
atlas tier --json       # machine output (for scripts)
atlas tier --raw        # just the probe (no classification)
```

tier 表给出的是按显存区间的起点；**`atlas tier fit`** 会针对你运行的*具体*模型进一步精化 —— 它读取 GGUF 的 KV 几何结构和你 GPU 的显存，求解出能完全留在 GPU 上的最大上下文（`atlas tier fit --write` 会把结果写入 `.env`）。每当你更换 `ATLAS_MODEL_FILE` 或 GPU 时都运行一次。参见 [CLI.md § atlas tier fit](../../CLI.md#atlas-tier-fit)，下载前的规模估算见 [TROUBLESHOOTING.md § What fits on my GPU?](../../TROUBLESHOOTING.md#what-fits-on-my-gpu)。

medium tier 是 ATLAS 的开发目标 —— `atlas-bootstrap.sh` 默认使用它的模型+上下文设置。其他 tier 请在 bootstrap 完成后运行 **`atlas init`**（首次运行向导）。它通过 `atlas tier` 探测硬件，从注册表挑选合适的模型，带 SHA 校验地下载，并重写 `.env`。当你的硬件或注册表默认模型变化时用 `atlas init --reconfigure` 重跑；向导跑完后，`atlas tier fit --write` 会把向导的 tier 级默认收紧到所选模型。

| 资源 | 最低要求 | 推荐配置 | 备注 |
|----------|---------|-------------|-------|
| GPU 显存 | 8 GB | 16 GB | 见上方 tier 表 |
| 系统内存 | 14 GB | 16 GB+ | PyTorch 运行时 + 容器开销 |
| 磁盘 | 15 GB | 25 GB | 模型（4.4–23 GB，取决于 tier）+ 容器镜像（5–8 GB）+ 工作空间 |
| CPU | 4 核 | 8+ 核 | V3 pipeline 在修复阶段对 CPU 要求较高 |

### 支持的 GPU

任何具有 8 GB+ 显存、且后端受 llama.cpp 支持的 GPU：

| 厂商 | 后端 | 状态 | 构建路径 | 已测试显卡 |
|---|---|---|---|---|
| NVIDIA（Blackwell —— RTX 50xx、B100、GB10） | CUDA | 支持 (Supported)（已发布镜像） | `inference/Dockerfile.v31` | RTX 5060 Ti 16GB（主要开发用） |
| NVIDIA（前 Blackwell —— RTX 20xx–40xx、GTX 10xx、V100/A100/H100/T4/L4） | CUDA | 预览 (Preview) —— 需要一次性[本地重建](#cuda-计算能力-dockerfilev31) | `inference/Dockerfile.v31` + `--build-arg CUDA_ARCH=<cc>` | —（上游 llama.cpp 支持这些卡；维护者未在 ATLAS 上验证） |
| AMD | ROCm / HIP | 社区验证 (Community-tested) | `inference/Dockerfile.rocm` | RX 7900 XTX（社区冒烟测试，[GH #26](https://github.com/itigges22/ATLAS/issues/26)） |
| Apple Silicon | Metal | 支持（macOS 混合方案：原生 llama-server + Docker，[#32](https://github.com/itigges22/ATLAS/issues/32)） | `scripts/atlas-setup-macos.sh` + `docker-compose.macos.yml` | M2 Pro 32GB（已验证）；M3/M4（目标） |
| 任意（跨厂商回退） | Vulkan | 预览 | `inference/Dockerfile.vulkan` | lavapipe（CPU ICD）已冒烟测试；尚无真实 GPU 验证 |
| Intel Arc | SYCL | 路线图 (Roadmap) —— Intel Arc 目前走 Vulkan | 待定 | Arc A770 16GB（目标） |

`atlas tier` 跨厂商自动检测并挑选显存最大的 GPU。如果有多块 GPU 而你想指定某一块，用 `ATLAS_GPU_VENDOR=amd` 或 `ATLAS_GPU_INDEX=1` 覆盖。

#### CUDA 计算能力 (Dockerfile.v31)

`inference/Dockerfile.v31` 针对特定的 CUDA 计算能力编译 llama.cpp。默认值 —— 也是 GHCR 上发布的 `atlas-llama` 镜像所使用的值 —— **只有** `120;121`（Blackwell：RTX 50xx、B100、GB10）。发布的镜像不包含更早 GPU 的内核，其内嵌的 PTX 也无法向下 JIT 编译，因此在 RTX 20/30/40 系列、GTX 10xx 和前 Blackwell 数据中心卡（V100/A100/H100/T4/L4）上，llama-server 启动时会失败并报 `no kernel image is available for execution on the device`。你必须针对自己的架构一次性重建推理镜像。（本地构建若用了错误的架构值，会更早失败，报 `nvcc fatal: unsupported gpu architecture`。）

先查出你 GPU 的架构值，再用 `--build-arg CUDA_ARCH=<value>` 重建：

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

常见取值：

| 架构值 | 架构 | 显卡 |
|------|--------------|-------|
| `60`, `61` | Pascal | GTX 10xx、Tesla P4/P40 |
| `70` | Volta | V100 |
| `75` | Turing | RTX 20xx、T4 |
| `80`, `86` | Ampere | A100、RTX 30xx |
| `89` | Ada Lovelace | RTX 40xx、L4 |
| `90` | Hopper | H100 |
| `100`, `120`, `121` | Blackwell | B100、RTX 50xx |

#### AMD GPU 目标 (Dockerfile.rocm)

`inference/Dockerfile.rocm` 针对一个或多个 `gfx` 目标编译 llama.cpp 的 HIP 后端。默认是覆盖最常见消费级 + 数据中心 AMD GPU 的胖构建：`gfx1100;gfx1101;gfx1102;gfx1030;gfx90a`。每增加一个目标，二进制约增大 150 MB。

构建时用 `--build-arg GFX_TARGET=<value>` 覆盖（或通过 `ATLAS_GFX_TARGET` 环境变量，compose override 会转发它）：

```bash
# Single target — RX 7900 XT/XTX only (smaller image)
ATLAS_GFX_TARGET=gfx1100 docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server

# Two targets for RDNA3 + RDNA2 mixed-fleet
docker build --build-arg GFX_TARGET="gfx1100;gfx1030" -f inference/Dockerfile.rocm -t atlas-llama-rocm:custom inference/
```

常见取值：

| 目标 | 架构 | 显卡 |
|--------|--------------|-------|
| `gfx1100` | RDNA3 (Navi 31) | RX 7900 XT、7900 XTX、7900 GRE |
| `gfx1101` | RDNA3 (Navi 32) | RX 7800 XT、7700 XT |
| `gfx1102` | RDNA3 (Navi 33) | RX 7600、7600 XT |
| `gfx1030` | RDNA2 (Navi 21) | RX 6800、6800 XT、6900 XT、6950 XT |
| `gfx1031` | RDNA2 (Navi 22) | RX 6700 XT、6750 XT |
| `gfx1032` | RDNA2 (Navi 23) | RX 6600、6600 XT、6650 XT |
| `gfx90a` | CDNA2 | MI210、MI250、MI250X |
| `gfx942` | CDNA3 | MI300X |
| `gfx900` | Vega | Vega 56/64（可能需要 HSA 覆盖 —— 见 TROUBLESHOOTING.md） |
| `gfx1200` | RDNA4 (Navi 44) | RX 9070 |
| `gfx1201` | RDNA4 (Navi 48) | RX 9070 XT |

> **RDNA4 (gfx1200/gfx1201) 用户：** 请设置 `ATLAS_ROCM_TAG=7.2.3-complete` —— 默认的 ROCm 6.2 基础镜像不包含 gfx1200/gfx1201 的编译器支持。ROCm 7.0+ 原生支持这些目标；不要设置 `ATLAS_HSA_OVERRIDE_GFX_VERSION`。详情见 [TROUBLESHOOTING.md § RDNA4](../../TROUBLESHOOTING.md)。

查询你 GPU 的 gfx 目标：`rocminfo | grep -i gfx | head -1`（或在 [LLVM AMDGPU 处理器表](https://llvm.org/docs/AMDGPUUsage.html)中查找）。

---

## Geometric Lens 权重（可选）

ATLAS 在没有 Geometric Lens 权重的情况下也能正常工作 —— 服务会优雅降级，返回中性分数。V3 pipeline 回退到仅沙箱验证。

要启用 C(x)/G(x) 评分，你需要训练好的模型权重。预训练权重和训练数据可在 HuggingFace 上获取：

**[ATLAS 数据集（HuggingFace）](https://huggingface.co/datasets/itigges22/ATLAS)** —— 包含嵌入向量、训练数据和权重文件。

将权重文件放在 `geometric-lens/geometric_lens/models/` 目录中（或通过 Docker Compose 中的 `ATLAS_LENS_MODELS` 挂载）。服务启动时会自动加载。

如果你希望使用自己的基准测试数据进行训练，整个流程都由 CLI 驱动：

```bash
atlas bench --run-id mymodel_lens --tasks 200    # 生成候选并自标注
atlas lens build --force --from-results benchmark/results/mymodel_lens/v3_lcb/per_task
```

`atlas lens build` 会训练 lens 的两个部分、校准阈值，并在激活的 bundle 中写入 `provenance.json` 清单。参见 [CLI.md § atlas lens](../../CLI.md#atlas-lens)。

### 自带模型

如果你想换入一个非默认的 GGUF，`atlas lens` 子命令封装了探测 + 训练的流水线，让你不必学习底层脚本：

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

完整参考：[CLI.md § atlas lens](../../CLI.md#atlas-lens)。

---

## ASA 操控向量（自动构建）

2026 年 5 月 BiasBusters #4。一个残差流操控向量，在语法门控有机会拒绝任何东西**之前**，就把模型对整函数/类/元素重写的选择偏向 `structural_edit` 而非 `edit_file`。完全可选 —— 没有它 ATLAS 仍能工作，只是工具选择偏差不受操控。

`atlas-bootstrap.sh` 会在服务启动后自动构建它。流水线是：

1. `build_cvector_prompts.py` 把已提交的 `geometric-lens/asa_calibration/contrast_pairs.jsonl`（1000 对）转成正/负提示词文件。
2. bootstrap 短暂停下 `llama-server`，以 `--method mean -ngl 99` 把 `llama-cvector-generator` 作为一次性容器运行，写出 `models/ast_edit_steering.gguf`，外加一个 `models/ast_edit_steering.gguf.model` 侧车标记，记录该向量是针对哪个模型构建的，然后重启 `llama-server`。
3. `inference/entrypoint-v3.1.sh` 在下次启动时看到该文件，检查 `.model` 侧车标记与所选模型匹配后，把 `--control-vector-scaled /models/ast_edit_steering.gguf:0.5` 附加到 `llama-server` 的命令行。标记缺失或指向不同模型的向量将保持**禁用**（启动横幅会说明原因）—— 向量是绑定于单个模型的残差空间工件。

在 16GB GPU 上的总墙钟时间：约 5 分钟。构建运行在模型所在的同一硬件上；产出的向量是模型专属的（不要把针对某个模型工件构建的 `ast_edit_steering.gguf` 挪到运行不同基础模型的主机上）。

**覆盖行为**（如需调优，在 `.env` 中设置）：

| 环境变量 | 默认值 | 效果 |
|---|---|---|
| `ATLAS_CONTROL_VECTOR` | `/models/ast_edit_steering.gguf` | 覆盖路径 |
| `ATLAS_CONTROL_VECTOR_SCALE` | `0.5` | 保守值。如果偏置太不明显，提到 1.0–1.5；如果非工具任务变差，降到 0.2 左右。 |
| `ATLAS_CONTROL_VECTOR_LAYER_RANGE` | （所有层） | 传两个整数，如 `"24 30"`，把作用范围限定到一个层带。更窄 = 更安全但更弱。 |
| `ATLAS_CONTROL_VECTOR_ALLOW_UNVERIFIED` | `0` | 设为 `1` 时，即使向量的 `.model` 侧车标记缺失或与所选模型不匹配也照常应用。仅用于你自己构建、且确知匹配的向量。 |

**如果本地构建失败**（例如较老的 `atlas-llama` 镜像里没有 cvector-generator、GPU OOM、拉取运行时时网络抖动），bootstrap 会回退为从 [ATLAS HuggingFace 数据集](https://huggingface.co/datasets/itigges22/ATLAS)下载预构建的 `ast_edit_steering.gguf`。如果这也失败，安装会带着警告完成 —— `atlas doctor` 会把这一缺口标为 `warn` 而不是 `fail`。

要完全跳过构建，在运行安装器前设置 `ATLAS_BOOTSTRAP_SKIP_ASA=1`。

要手动重建（重新筛选的对比对、不同的 `--method`、不同的基础模型），见 [`geometric-lens/asa_calibration/README.md`](../../../geometric-lens/asa_calibration/README.md)。

---

## 后续步骤

- [CLI.md](../../CLI.md) —— ATLAS 运行后的使用指南
- [CONFIGURATION.md](../../CONFIGURATION.md) —— 所有环境变量和调优选项
- [TROUBLESHOOTING.md](../../TROUBLESHOOTING.md) —— 常见问题与解决方案
- [ARCHITECTURE.md](../../ARCHITECTURE.md) —— 系统内部工作原理
