<!-- source: docs/TROUBLESHOOTING.md synced-through: 4f1be83 -->
> **[English](../../TROUBLESHOOTING.md)** | **简体中文** | **[日本語](../ja/TROUBLESHOOTING.md)** | **[한국어](../ko/TROUBLESHOOTING.md)**

> ℹ️ **译者注：** 若本译文与英文原版 ([TROUBLESHOOTING.md](../../TROUBLESHOOTING.md)) 有出入，以英文原版为准。


# ATLAS 故障排除指南

常见问题与解决方案，按服务分类。

---

## 快速诊断

先运行 `atlas doctor`，再使用 Compose 状态和日志定位问题：

```bash
atlas doctor
docker compose ps
docker compose logs --tail 50
```

`atlas doctor` 会同时检查主机、配置和服务健康状况，因此是首选的第一项诊断。`docker compose ps` 用于识别启动失败的服务，日志则提供下一层细节。只有当初步结果指向推理后端时，才使用硬件专用检查：

| 后端 | 诊断命令或检查项 |
|---|---|
| NVIDIA CUDA | `nvidia-smi` |
| AMD ROCm | 运行 `rocm-smi` 并确认 `/dev/kfd` 存在。 |
| Apple Silicon / Metal | 运行 `atlas doctor`；如果原生服务器未监听，请按照 [SETUP_MACOS.md](../../SETUP_MACOS.md#troubleshooting) 中的说明启动 `./scripts/atlas-llama-macos.sh`，并检查其前台启动器输出。 |
| Vulkan | 确认 `/dev/dri` 存在，然后运行 `docker compose -f docker-compose.yml -f docker-compose.vulkan.yml exec llama-server vulkaninfo --summary`。 |
| 纯 CPU | 确认 `docker-compose.vulkan.yml` 和 `docker-compose.cpu.yml` 覆盖层已启用。`atlas doctor` 不应仅因没有 GPU 而失败，而应发出警告并成功退出。 |

逐服务的健康检查 curl 命令见 [SETUP.md § 验证安装](SETUP.md#验证安装)。atlas-proxy 的健康检查端点对分诊最有用 —— 它会报告所有上游服务的状态：
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

如果任何字段为 `false`，则该服务存在问题。只要 `inference`、`lens`、`lens_ready` 或 `sandbox` 中任一为 false，`status` 就会翻转为 `"degraded"`。`lens` 与 `lens_ready` 的区分让你能分辨"Lens 进程在运行，但其 `/ready` 门控失败 —— 通常是权重缺失或嵌入维度不匹配"与"Lens 的 HTTP 不可达"这两种情况。

---

## 找到你的错误

精确的错误字符串与症状，映射到对应的条目。

| 你看到的 | 前往 |
|---|---|
| `no kernel image is available for execution on the device` —— NVIDIA GPU | [`no kernel image is available for execution on the device` (CUDA)](#no-kernel-image-is-available-for-execution-on-the-device-cuda) |
| `no kernel image is available for execution on the device` —— AMD GPU | [AMD GPU "不受 ROCm 支持"，但你想试试…](#amd-gpu-不受-rocm-支持但你想试试rocm-上的-no-kernel-image) |
| `invalid device function` 或 `nvcc fatal: unsupported gpu architecture` | [`no kernel image…` (CUDA)](#no-kernel-image-is-available-for-execution-on-the-device-cuda) |
| `libnvidia-ml.so.1: cannot open shared object file` | [libnvidia-ml.so.1 条目](#libnvidia-mlso1-cannot-open-shared-object-file) |
| 容器看不到 GPU；模型跑在 CPU 上 | [容器中未检测到 GPU](#容器中未检测到-gpu) |
| `/dev/kfd: no such file or directory` | [未检测到 AMD GPU (ROCm)](#未检测到-amd-gpu-rocm) |
| `/dev/kfd` 上的 `Permission denied` | [检测到 AMD GPU 但 Docker 无法访问](#检测到-amd-gpu-但-docker-无法访问) |
| `error: AMDGPU target 'gfx1201' is not supported` | [RDNA4 —— 需要 ROCm 7.x](#rdna4rx-9070--9070-xtgfx1200--gfx1201-需要-rocm-7x) |
| ROCm 镜像拉取超时 / 被限流 | [ROCm 容器无法拉取 `rocm/rocm-terminal`](#rocm-容器无法拉取-rocmrocm-terminal) |
| `docker compose build` 因 CUDA 错误失败 | [首次构建失败（找不到 CUDA）](#首次构建失败找不到-cuda) |
| `RPC failed; curl 56` / `early EOF` / `fetch-pack: invalid index-pack output` | [llama.cpp 克隆超时](#llamacpp-克隆超时) |
| `error loading model: unknown (model) architecture '…'` | [重建 llama.cpp](#重建-llamacpp新模型架构或补丁漂移) |
| `error: patch failed:` / `patch does not apply` | [重建 llama.cpp](#重建-llamacpp新模型架构或补丁漂移) |
| 挂载卷 / 模型文件权限被拒绝（Fedora/RHEL） | [SELinux 阻止容器访问](#selinux-阻止容器访问fedorarhel) |
| 代理健康检查中 `"sandbox": false`（手动容器部署） | [Sandbox 不可达](#sandbox-不可达) |
| 代理健康检查中 `"sandbox": false`（Compose 栈） | [Sandbox 不可达（健康检查）](#sandbox-不可达健康检查) |
| 启动时 `address already in use` | [端口冲突](#端口冲突) |
| "fitting params to device memory" 之后出现 CUDA 分配错误 | [模型 + KV 缓存放不进 GPU](#模型--kv-缓存放不进-gpu启动失败或生成速度慢-5-倍) |
| 生成约 2 tok/s，llama-server 占满多个 CPU 核心 | [模型 + KV 缓存放不进 GPU](#模型--kv-缓存放不进-gpu启动失败或生成速度慢-5-倍) / [生成速度慢](#生成速度慢约-2-toks) |
| 下载前估算模型能否放下 | [我的 GPU 能放下什么？](#我的-gpu-能放下什么) |
| 启动时 `failed to load model` | [模型文件未找到](#模型文件未找到) |
| llama-server 崩溃 / 被 OOMKill，显存接近 100% | [显存不足](#显存不足) |
| 模型输出 `<think>` 标签或散文而非 JSON 工具调用 | [语法未强制执行](#语法未强制执行模型输出思维块) |
| Gemma 不断发出 `done` 却什么也不做 | [语法未强制执行](#语法未强制执行模型输出思维块) |
| 工具调用上出现 `unexpected end of JSON` | [上下文窗口过小](#上下文窗口过小) / [截断错误](#截断错误write_file-反复失败) |
| 没有工具调用、没有 V3 —— 请求直接透传 | [Agent 循环未激活](#agent-循环未激活) |
| 写入/编辑从不触发 V3 阶段 | [V3 Pipeline 未对功能文件触发](#v3-pipeline-未对功能文件触发) |
| `Your output was truncated — the content is too long for a single tool call` | [截断错误](#截断错误write_file-反复失败) |
| `Tool call was truncated (output too long for context window)` | [截断错误](#截断错误write_file-反复失败) |
| 工具结果与下一个动作之间约 30 秒空转 | [工具结果与下一个动作之间的长时间停顿](#工具结果与下一个动作之间的长时间停顿) |
| 修复已被验证后 agent 仍继续编辑 | [V3 已确认修复后模型仍继续编辑](#v3-已确认修复后模型仍继续编辑) |
| 第一个工具调用读取一个此处不存在的文件 | [模型幻觉出以前会话的文件名](#模型幻觉出以前会话的文件名) |
| 仅在 V3 验证期间出现 `ModuleNotFoundError` | [多文件项目：sandbox 报 `ModuleNotFoundError`](#多文件项目sandbox-报-modulenotfounderror) |
| `_curses.error: addwstr() returned ERR` | [Curses 底行 `addwstr() returned ERR`](#curses-底行-addwstr-returned-err) |
| 写 HTML/CSS/JSON 文件时约 5 分钟停顿 | [V3 在非 Python 文件上挂起数分钟](#v3-在非-python-文件上挂起数分钟) |
| 简短的跟进（"ok"、"yes"）只得到聊天而非行动 | ["再修一次"的提示词不触发 V3 Pipeline](#再修一次的提示词不触发-v3-pipeline) |
| `file not read yet — use read_file first before editing` | [编辑前未读取文件](#编辑前未读取文件) |
| `file modified since last read — read it again before editing` | [文件被外部修改](#文件被外部修改) |
| `You have full project context in the system prompt. Do not read more files.` | [探索预算警告](#探索预算警告) |
| `"lens": false` / "Lens unavailable — verification disabled" | [Lens 未加载/不可用](#lens-未加载不可用) |
| 每个候选都得到 `cx_energy: 0.0`、`gx_score: 0.5` | [所有分数接近 0.5](#所有分数接近-05) |
| lens 日志中出现 "embedding extraction failed" | [嵌入向量提取失败](#嵌入向量提取失败) |
| 重训练时 503 `models directory is mounted read-only` | [`/internal/lens/retrain` 返回 503](#internallensretrain-返回-503-models-directory-is-mounted-read-only) |
| Sandbox 返回 `"error_type": "Timeout"` | [代码执行超时](#代码执行超时) |
| Sandbox 对特定语言报错 | [语言不受支持](#语言不受支持) |
| `LIMITED MODE: running N tasks` 的 N 低于 `--tasks` | [bench 运行的任务数少于请求数](#bench-运行的任务数少于请求数limited-mode-running-n-tasks-的-n-小于---tasks) |
| 系统卡顿、服务被 OOMKill | [内存使用过高](#内存使用过高) |

---

## Docker / Podman 问题

### 容器中未检测到 GPU

**现象：** llama-server 容器启动但模型在 CPU 上加载（非常慢，约 2 tok/s）。主机上 `nvidia-smi` 能看到 GPU 但容器内无法访问。

**解决方法：** 安装 NVIDIA Container Toolkit：

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

验证容器内 GPU 是否可见：
```bash
# Docker
docker run --rm --gpus all nvidia/cuda:12.0.0-base-ubuntu22.04 nvidia-smi

# Podman
podman run --rm --device nvidia.com/gpu=all nvidia/cuda:12.0.0-base-ubuntu22.04 nvidia-smi
```

### `libnvidia-ml.so.1: cannot open shared object file`

**现象：** `docker compose up` 期间，llama-server 失败并报：

```
nvidia-container-cli: initialization error: load library failed:
libnvidia-ml.so.1: cannot open shared object file: no such file or directory
```

**含义：** 主机有 NVIDIA *内核模块*（所以 `nvidia-smi` 能用），但*用户态驱动库*不在容器工具包期望的位置。在 RHEL/Rocky/Alma 最小安装上，`nvidia-driver-cuda-libs` 包默认不会被带入；在 Debian/Ubuntu 上，问题通常是驱动升级后 `ldconfig` 缓存过期。

**修复顺序** —— 依次尝试，当 `docker run --rm --gpus all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi` 能用时停止：

1. **刷新 ldconfig + 重启 docker：**
   ```bash
   sudo ldconfig
   sudo systemctl restart docker
   ```

2. **RHEL 9 —— 添加 CUDA 仓库 + 安装 open-dkms 模块**（已在 RHEL 9.7 + RTX 5060 Ti 上验证可用）：
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

   **Rocky/Alma/CentOS Stream 9** —— 同上，但把 `subscription-manager` 那一行替换为：
   ```bash
   sudo dnf config-manager --set-enabled crb
   ```

   > 注意：`nvidia-driver-cuda-libs` 包只有在添加了 NVIDIA CUDA 仓库之后才存在。RHEL 9 自带的 `BaseOS`/`AppStream` 仓库不含 NVIDIA 包。Blackwell GPU（RTX 5060/70/80/90）**必须**使用 `nvidia-driver:open-dkms` 模块；更老的 GPU open 和专有驱动都可以。

3. **Ubuntu/Debian —— 安装匹配的用户态库：**
   ```bash
   DRV_MAJOR=$(nvidia-smi --query-gpu=driver_version --format=csv,noheader | cut -d. -f1)
   sudo apt install -y libnvidia-compute-${DRV_MAJOR}
   sudo ldconfig && sudo systemctl restart docker
   ```

4. **生成 CDI 规范：**
   ```bash
   sudo mkdir -p /etc/cdi
   sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml
   docker run --rm --device=nvidia.com/gpu=all nvidia/cuda:12.4.0-base-ubuntu22.04 nvidia-smi
   ```

`atlas-bootstrap.sh` 脚本现在会自动运行第 1、2 步（自动区分 RHEL/Rocky/Alma 与 subscription 路径）和第 4 步。第 3 步在 Debian/Ubuntu 上会通过与运行中驱动版本匹配的 `libnvidia-compute-NN` 自动处理。

### 未检测到 AMD GPU (ROCm)

**现象：** 在明明装有 AMD GPU 的主机上，`atlas tier` 说 "no GPU detected"，或者 `docker compose up` 因 `/dev/kfd: no such file or directory` 失败。

**含义：** `amdgpu` 内核驱动没有以计算支持（`kfd` —— Kernel Fusion Driver —— 子模块）加载。仅用于显示的 `amdgpu` 加载不会暴露 `/dev/kfd`。

**修复顺序：**

1. **确认驱动已加载且 `/dev/kfd` 存在：**
   ```bash
   lsmod | grep amdgpu       # should print amdgpu + amdkfd
   ls -l /dev/kfd            # should print a character-device entry
   ls -l /dev/dri/render*    # should print one or more render nodes
   ```

2. **安装 ROCm + 内核驱动（如果 /dev/kfd 缺失）：**
   - **RHEL 9 / Rocky / Alma：**
     ```bash
     sudo dnf install -y https://repo.radeon.com/amdgpu-install/6.2/rhel/9.4/amdgpu-install-6.2.60200-1.el9.noarch.rpm
     sudo amdgpu-install --usecase=dkms,rocm
     sudo reboot   # required — the kernel module needs a fresh boot
     ```
   - **Ubuntu/Debian：** 按[官方 AMD 安装指南](https://rocm.docs.amd.com/projects/install-on-linux/)针对你发行版的步骤操作。典型流程是添加 AMDGPU 仓库后运行 `amdgpu-install --usecase=dkms,rocm`。

3. **重启后，确认 `rocm-smi` 能看到 GPU：**
   ```bash
   rocm-smi --showproductname --showmeminfo vram
   ```

### 检测到 AMD GPU 但 Docker 无法访问

**现象：** `atlas doctor` 报告 "AMD GPU detected but Docker can't reach `/dev/kfd`"，或 ROCm 容器在 `/dev/kfd` 上因 `Permission denied` 失败。

**含义：** 运行 Docker 的用户不在 `render` 和/或 `video` 组中。ROCm 用这些组来控制对 `/dev/kfd` 和 `/dev/dri/render*` 的访问。

**解决方法：**

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

### AMD GPU "不受 ROCm 支持"，但你想试试（ROCm 上的 `no kernel image`）

**现象：** `rocm-smi` 能报告你的 GPU，但 `rocminfo` 不能；或者 HIP 内核失败并报 "no kernel image is available for execution on the device"。（NVIDIA 上的同一错误见 [CUDA 条目](#no-kernel-image-is-available-for-execution-on-the-device-cuda)。）

**含义：** llama.cpp 的 HIP 内核编译时的 `gfx` 目标不包含你的 GPU。ROCm 长期以来的惯例是把较老的消费级 GPU 从官方支持中移除，但用正确的覆盖仍能让它们工作。

**解决方法：** 通过 `ATLAS_HSA_OVERRIDE_GFX_VERSION` 在运行时强制一个兼容的 gfx 版本。常见覆盖值（标准的显卡→gfx 对照表见 [SETUP.md § AMD GPU 目标](SETUP.md#amd-gpu-目标-dockerfilerocm)）：

| 你的 GPU | 设置 `ATLAS_HSA_OVERRIDE_GFX_VERSION=` |
|---|---|
| RDNA1 (RX 5700 XT / 5500 XT) | `10.3.0`（让它看起来像 RDNA2 / gfx1030） |
| Vega 56/64 (gfx900) | `9.0.0`（通常已受支持，很少需要覆盖） |
| Polaris (RX 580/590, gfx803) | `8.0.3`（深度覆盖；效果因卡而异） |

把该变量写进 `.env`，它会经由 compose override 传进容器环境：

```bash
echo "ATLAS_HSA_OVERRIDE_GFX_VERSION=10.3.0" >> .env
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d --force-recreate llama-server
```

如果这个方法在一张此前不受支持的卡上对你有效，请在 [GH #26](https://github.com/inferstep/ATLAS/issues/26) 留言 —— 社区验证过的覆盖值会进入下一个版本的文档。

### RDNA4（RX 9070 / 9070 XT，gfx1200 / gfx1201）—— 需要 ROCm 7.x

**现象：** `docker compose ... build llama-server` 构建失败并报 `error: AMDGPU target 'gfx1201' is not supported` 之类的错误，或容器启动后立即因 HIP 初始化错误退出。

**含义：** 默认的 ROCm 基础镜像（`rocm/dev-ubuntu-22.04:6.2-complete`）早于 RDNA4。gfx1200 和 gfx1201 编译器目标是 ROCm 7.0 加入的 —— 完整的受支持硬件列表见 [ROCm 兼容性矩阵](https://rocm.docs.amd.com/en/latest/compatibility/compatibility-matrix.html)。

**解决方法：** 构建前把 `ATLAS_ROCM_TAG` 设为一个 ROCm 7.x 标签：

```env
# Add to your .env
ATLAS_ROCM_TAG=7.2.3-complete
ATLAS_GFX_TARGET=gfx1201   # gfx1200 for RX 9070, gfx1201 for RX 9070 XT
```

然后重新构建并拉起整个栈：

```bash
docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
docker compose -f docker-compose.yml -f docker-compose.rocm.yml up -d
```

**重要：不要为 gfx1200/gfx1201 设置 `ATLAS_HSA_OVERRIDE_GFX_VERSION`。** ROCm 7.0+ 原生支持这些目标；在 Docker 内覆盖 GFX 版本会导致编译出的内核与运行时目标不匹配，从而崩溃。保持 `ATLAS_HSA_OVERRIDE_GFX_VERSION` 未设置（默认）。

> 已在 AMD Radeon AI PRO R9700 (gfx1201) + ROCm 7.2、`ATLAS_ROCM_TAG=7.2.3-complete` 上测试。hidden-states 补丁可以干净地应用到固定的 llama.cpp SHA 上。文本生成和嵌入生成的推理均正常，无需任何额外标志。

### ROCm 容器无法拉取 `rocm/rocm-terminal`

**现象：** `atlas doctor` 的 ROCm 检查在镜像拉取处超时，或 `docker compose -f ... -f docker-compose.rocm.yml pull` 在 `llama-server` 构建上失败。

**含义：** ROCm 镜像很大（约 2 GB），且 Docker Hub 对匿名拉取限流。

**解决方法：** 登录认证（免费的 Docker Hub 账号有更高的限流额度）并预拉取 doctor 检查用的镜像，或在低峰时段重试：

```bash
docker login
docker pull rocm/rocm-terminal:latest
```

doctor 的 ROCm 检查始终使用 `rocm/rocm-terminal:latest`。`ATLAS_ROCM_TAG` 固定的是 llama-server ROCm 构建的*构建基础*镜像（`rocm/dev-ubuntu-*`），而不是 doctor 检查用的镜像：

```bash
ATLAS_ROCM_TAG=6.2-complete docker compose -f docker-compose.yml -f docker-compose.rocm.yml build llama-server
```

### 首次构建失败（找不到 CUDA）

**现象：** `docker compose build` 在 llama-server 编译过程中出现 CUDA 相关错误。

**解决方法：** llama-server 的 Dockerfile 在 `nvidia/cuda:12.9.0-devel` 基础镜像中构建 llama.cpp（该镜像在 `inference/Dockerfile.v31` 中按 digest 固定），因此构建时不需要主机 GPU 访问即可使用 CUDA 头文件。常见的构建失败原因：
1. 磁盘空间不足（构建产物需要约 5GB）
2. 下载 CUDA 基础镜像或克隆 llama.cpp 时的网络问题
3. Podman 非 root 构建可能因权限问题失败 —— 尝试 `podman-compose build` 加上 `--podman-build-args="--format docker"`

### llama.cpp 克隆超时

**现象：** 构建卡在 `llama-server builder 3/3` 阶段，最终失败并报：

```
error: RPC failed; curl 56 OpenSSL SSL_read: Connection timed out, errno 110
fatal: early EOF
fatal: fetch-pack: invalid index-pack output
```

**原因：** llama.cpp 的完整 git 历史很大（约 1 GB），获取（fetch）对不稳定/慢速网络很敏感。短暂的停顿会导致 SSL 读取超时，整个传输随之中止。

**解决方法：** `inference/Dockerfile.v31` 使用 `git init` + 对单个固定修订（`LLAMA_CPP_REV`）的 `--depth 1` fetch —— 从而避开约 1 GB 的完整历史传输 —— 并配合 `http.postBuffer=524288000` 与 `http.lowSpeedLimit/Time`，以便在连接死掉时快速失败。如果问题反复出现：

1. 重试构建 —— 瞬时的网络抖动是常事，尤其在家用网络上。
2. 如果重试一直失败，先在主机上预取固定的修订，再调整 Dockerfile 以 COPY 它。快速配方：
   ```bash
   REV=$(grep -m1 'ARG LLAMA_CPP_REV=' inference/Dockerfile.v31 | cut -d= -f2)
   git init /tmp/llama.cpp && cd /tmp/llama.cpp
   git remote add origin https://github.com/ggml-org/llama.cpp
   git fetch --depth 1 origin "$REV" && git checkout FETCH_HEAD
   # then edit Dockerfile.v31 to COPY from /tmp/llama.cpp instead of fetching
   ```
3. GHCR 上预构建的 llama-server 镜像完全跳过这一步 —— 直接拉取而非构建。

### 重建 llama.cpp（新模型架构，或补丁漂移）

开发者维护任务。两种触发情形会落到这里：

- **投入的新模型加载失败**，报 `error loading model: unknown (model) architecture 'gemma4'` —— 固定的 llama.cpp 版本早于该架构。
- **构建失败**，报 `error: patch failed: tools/server/server-context.cpp:NN` / `patch does not apply` —— 上游已漂移过固定的 SHA。

`atlas-llama` 镜像在全部四个 Dockerfile（`Dockerfile`、`Dockerfile.v31`、`Dockerfile.rocm`、`Dockerfile.vulkan`）中通过 `LLAMA_CPP_REV` 固定 llama.cpp 版本；其中 `Dockerfile.v31`、`Dockerfile.rocm` 和 `Dockerfile.vulkan`（compose 文件构建所用的三个）会在构建期间应用 `inference/patches/expose-hidden-states.patch`（Geometric Lens 依赖的逐层 `hidden_states` 扩展）。普通的 `Dockerfile` 固定了版本但不应用补丁，因此用它构建的服务器缺少 lens 的接线。要支持新架构，把固定的版本移到一个包含该架构的 llama.cpp SHA。预构建的 GHCR 镜像跳过本地构建；只有当你需要比已发布镜像更新的架构时才重建。

**保护 hidden-states 补丁 —— 变基它，不要删除它。** 移除 `git apply` 步骤会构建出一个悄悄失去 lens 接线的服务器（`/embedding` 会忽略 `layers:` 参数）。版本升级手册：

1. **对目标 SHA 验证补丁**（很快，无需 Docker）：
   ```bash
   mkdir -p /tmp/llama-check && cd /tmp/llama-check
   git init -q llama.cpp && cd llama.cpp
   git remote add origin https://github.com/ggml-org/llama.cpp
   git fetch --depth 1 origin <NEW_SHA> && git checkout -q FETCH_HEAD
   git apply --check $REPO/inference/patches/expose-hidden-states.patch
   ```
   （只有这个补丁是 `git apply` 应用的。spec-decode 嵌入修复是 Dockerfile 中的一个 `sed`，目标行不存在时是空操作。）
2. **如果能干净应用：** 在全部四个 Dockerfile 中把 `LLAMA_CPP_REV` 更新为新 SHA。CI 冒烟测试会验证它们一致。
3. **如果失败：** 用 `git apply --reject …` 先落下干净的 hunk，把每个 `*.rej` hunk 重新插入到其移动后的锚点（注意周边代码的上游重命名，例如 `model` → `model_tgt`，并更新补丁新增的行），然后 `git diff > $REPO/inference/patches/expose-hidden-states.patch`。重跑第 1 步。为了在漫长的 CUDA 构建之前抓住成员/类型错误，只对 server 目标做 CPU-only 编译：`cmake -B build-cpu -DGGML_CUDA=OFF && cmake --build build-cpu --target llama-server`。
4. 重建并拉起：
   ```bash
   docker compose build --build-arg LLAMA_CPP_REV=<sha> llama-server
   docker compose up -d llama-server --no-deps
   ```

优先重新生成补丁，而不是把版本固定回更老的 SHA —— 往回固定意味着错过上游修复。

重建加载了模型之后，Geometric Lens 仍需为新模型重新训练 —— 见 [CONFIGURATION.md § Adding your own model](../../CONFIGURATION.md#adding-your-own-model-drop-in--unregistered)。

### 代理无法写入工作区（`.atlas.tmp: permission denied`）

**症状：** 所有 `write_file`/`edit_file` 都以 `cannot write /workspace/...: open /workspace/....atlas.tmp: permission denied` 失败（随后 agent 会四处寻找"可写的子目录"）。lens 训练样本也不再入库（代理日志中 `/data/lens_training` 写入失败）。

**原因：** atlas-proxy 镜像以内置的非 root 用户（uid 1001，`atlas`）运行，但绑定挂载到 `/workspace`（`ATLAS_PROJECT_DIR`）和 `/data/lens_training` 的宿主目录归操作者的 uid 所有。读取可以（模式 755），写入全部被拒绝。`.env` 早于 `ATLAS_PROXY_UID` 的安装在拉取加固后的代理镜像后会遇到这个问题。

**解决：** 像 sandbox 已经做的那样，让代理以调用者身份运行：

```bash
# 把你的 id 加入 .env（atlas init --reconfigure 现在也会写入这些）
echo "ATLAS_PROXY_UID=$(id -u)" >> .env
echo "ATLAS_PROXY_GID=$(id -g)" >> .env
docker compose up -d --no-deps --force-recreate atlas-proxy
```

验证：`docker exec atlas-atlas-proxy-1 touch /workspace/.write_test` 应当成功（验证后请删除该文件）。K3s 部署会通过 `scripts/generate-manifests.sh` 把相同的 id 渲染进代理 Pod 的 `securityContext`。

### Agent 说文件不存在，但它就在那里（工作区挂载分裂）

**症状：** 文件明明就在项目目录里，agent 会话却坚称它"不存在" —— `read_file` 失败而 `run_command`（`ls`、`cat`）能正常看到文件，或者反过来。会话很早就放弃（"文件 X 似乎不存在"），写入从未落到项目里，而所有 `/health` 端点都是绿的。

**原因：** 代理和 sandbox 把**不同的宿主目录**绑定挂载为 `/workspace`。文件工具（`read_file`/`write_file`/`edit_file`）由代理针对*它自己的*挂载提供服务；`run_command` 则在 *sandbox 的*挂载中执行。Compose 会从 `ATLAS_PROJECT_DIR`（默认：compose 的工作目录）**在创建时逐容器**解析挂载源 —— 因此从不同目录、或用不同的 `.env` 重建其中一个容器，就会悄悄让两者分裂。启动时不会有任何失败，只是 agent 在"裂脑"状态下工作。

**诊断：** `atlas doctor` —— `workspace_mounts` 检查会比较两个挂载，不一致时会带着两个宿主路径失败。手动检查：

```bash
docker inspect atlas-atlas-proxy-1 --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
docker inspect atlas-sandbox-1     --format '{{range .Mounts}}{{if eq .Destination "/workspace"}}{{.Source}}{{end}}{{end}}'
```

**解决：** 在 `.env` 中把 `ATLAS_PROJECT_DIR` 固定为你的项目目录，然后把两个容器一起重建：

```bash
echo "ATLAS_PROJECT_DIR=/path/to/your/project" >> .env
docker compose up -d --force-recreate atlas-proxy sandbox
```

### SELinux 阻止容器访问（Fedora/RHEL）

**现象：** 容器无法读取挂载的卷，模型文件权限被拒绝。

**解决方法：**
```bash
# Allow container access to model directory
chcon -Rt svirt_sandbox_file_t ~/models/

# Or add :Z flag to volume mounts (Docker Compose handles this)
```

### Sandbox 不可达

**现象：** 代理健康检查显示 `"sandbox": false`。V3 构建验证失败。

**解决方法：** 确保所有服务在同一个 Docker 网络上。Docker Compose 会自动创建 `atlas` 网络。如果手动运行容器：
```bash
docker network create atlas
# Start all containers with --network atlas
```

### 端口冲突

**现象：** `docker compose up` 因某端口 "address already in use" 而失败。

**解决方法：** 检查占用端口的进程，然后停止它或更改 `.env` 中的 ATLAS 端口：
```bash
# Find what's using port 8080
lsof -i :8080

# Change port in .env
ATLAS_LLAMA_PORT=8081    # Different port for llama-server
```

所有端口均可通过 `.env` 配置。参见 [CONFIGURATION.md](../../CONFIGURATION.md)。

---

## llama-server 问题

### `no kernel image is available for execution on the device` (CUDA)

**适用范围：** 比 Blackwell 更早的 NVIDIA GPU —— RTX 40xx（Ada）、RTX 30xx（Ampere）、RTX 20xx / T4（Turing）、GTX 10xx（Pascal）、V100/A100/H100/L4 —— 运行预构建的 `ghcr.io/inferstep/atlas-llama` 镜像时。同源错误 `invalid device function`（运行时）和 `nvcc fatal: unsupported gpu architecture`（本地构建）成因相同。（AMD 上的同一错误见 [ROCm 条目](#amd-gpu-不受-rocm-支持但你想试试rocm-上的-no-kernel-image)。）

**含义：** 发布的 CUDA 镜像只针对计算能力 `120;121`（仅 Blackwell）编译。llama-server 二进制不包含更早架构的 GPU 内核，其内嵌的 PTX（`compute_121`）无法向下 JIT 编译，因此第一次 CUDA 内核启动就会失败。这是镜像/GPU 不匹配，不是驱动或显存问题。

**先检查：**
```bash
# Your GPU's compute capability (8.9 = Ada, 8.6 = Ampere, 7.5 = Turing, 12.0 = Blackwell)
nvidia-smi --query-gpu=name,compute_cap --format=csv
# What the image was built for (Blackwell-only image prints sm_120/sm_121)
docker run --rm --entrypoint bash ghcr.io/inferstep/atlas-llama:latest \
  -c 'grep -ao "sm_[0-9]*" /usr/local/bin/llama-server | sort -u'
```
如果你的计算能力低于 12.0，而镜像只列出 `sm_120`/`sm_121`，那么本条目适用。

**解决方法 —— 针对你的架构重建推理镜像**（一次性，约 30-75 分钟；只重建 llama-server，其他服务继续使用 GHCR 镜像）。把计算能力去掉小数点（`8.6` -> `86`）：
```bash
docker compose build --build-arg CUDA_ARCH=86 llama-server
docker compose up -d --no-deps llama-server
```
多块 GPU / 追求可移植性：传入分号分隔的列表，例如 `--build-arg CUDA_ARCH="75;86;89"`。完整架构表：[SETUP.md § CUDA 计算能力](SETUP.md#cuda-计算能力-dockerfilev31)。

**验证：**
```bash
docker compose logs llama-server | tail -20   # model loads, no CUDA errors
curl -s localhost:8080/health                  # {"status":"ok"}
```

**仍然失败：** 确认容器确实在运行你重建的镜像（`docker compose images llama-server` —— `docker compose pull` 可能已把它覆盖；固定 `ATLAS_IMAGE_TAG` 或重新构建）。Pascal（`60`/`61`）及更老的卡受上游 llama.cpp CUDA 支持所限 —— Vulkan 镜像（`docker-compose.vulkan.yml`）是回退方案。若仍不行，请附上 `nvidia-smi` 输出和 llama-server 日志的前 50 行提交 issue。

### 模型在 CPU 而非 GPU 上加载

**现象：** 生成速度约 2 tok/s 而非约 50 tok/s。`nvidia-smi` 未显示 llama-server 使用 GPU。

**解决方法：** 确保设置了 `-ngl 99`（`--n-gpu-layers`，将所有层卸载到 GPU）。Docker Compose 中由入口点默认设置。裸机部署时，请检查启动命令：
```bash
ps aux | grep llama-server | grep -e '-ngl' -e 'n-gpu-layers'
```

如果使用 Docker，请确保已配置 NVIDIA 容器运行时（参见上方 GPU 章节）。

### 模型 + KV 缓存放不进 GPU（启动失败，或生成速度慢 5 倍）

**现象（当前 entrypoint）：** llama-server 在启动时于 "fitting params to device memory" 之后立即因 CUDA 分配错误退出。

**现象（没有 `--fit off` 的旧 entrypoint）：** 服务器*能启动*，`nvidia-smi` 也显示模型已加载，但生成速度只有预期的几分之一，llama-server 进程占用多个 CPU 核心（`top` 显示 400–800%），其主机 RSS 持有数 GB 的模型权重 —— llama.cpp 的内存自动适配器悄悄把部分层移到了 CPU。

**原因：** 模型权重 + KV 缓存（`ATLAS_CTX_SIZE` × `PARALLEL` slot 数 × 每层 KV 维度）+ 计算缓冲区（约 `ATLAS_UBATCH` × 隐藏维度 × 280 字节）超过了显存。这些预算因模型而异 —— 为一个模型调好的配置，换到 KV 几何结构不同的另一个模型上可能溢出。

**解决方法：** 按此模型 + GPU 调整运行时大小并重建容器：
```bash
atlas tier fit --write
docker compose up -d llama-server --no-deps --force-recreate
```
`atlas tier fit` 读取 GGUF 头和 GPU 显存，求解出完全在 GPU 上运行的最大配置（参见 [CLI.md § atlas tier fit](../../CLI.md#atlas-tier-fit)）。ATLAS 以 `--fit off` 运行 llama-server，因此放不下的配置会在启动时明确失败，而不是部分悄悄跑在 CPU 上。

如果 `atlas tier fit` 报告 **DOES NOT FIT**，说明模型本身对这张卡太大了 —— 输出会给出能放下的最大量化文件大小。按优先级：

1. **改用同一模型更小的量化**（例如用 Q4_K_M 代替 Q6_K —— 在 16 GB 显存以下通常是质量/GiB 的最佳取舍）。
2. **减少并行 slot**：`atlas tier fit --slots 1 --write` 可以释放每 slot 的 KV 最小占用（会失去 `/demo` 分屏和 V3 并行候选，单流使用不受影响）。
3. **选择更小的模型。** 见下方尺寸表。

### 我的 GPU 能放下什么？

下载前的粗略规则：在默认的 4 slot 下，满足以下条件的 GGUF 可以从容放下

```
file size  ≤  VRAM − ~4.5 GiB
```

（这约 4.5 GiB 覆盖 4 × 8k 上下文的最小 KV 缓存、计算缓冲区，以及约 1.9 GiB 的 CUDA 固定开销）。使用 `--slots 1` 时，余量缩小到大约 `VRAM − 3 GiB`。滑动窗口模型（Gemma 类）需要的比这少；该规则按全注意力模型估算。

| 显存 | GGUF 文件大小（4 slot） | GGUF 文件大小（1 slot） | 典型模型 |
|------|--------------------------|--------------------------|----------------|
| 8 GB | ≤ 约 3 GiB | ≤ 约 4.5 GiB | 3–4B Q4–Q6, 7–8B Q2–Q3 |
| 12 GB | ≤ 约 7 GiB | ≤ 约 8.5 GiB | 7–9B Q4–Q6, 12B Q3–Q4 |
| 16 GB | ≤ 约 11 GiB | ≤ 约 12.5 GiB | 9B Q6–Q8, 12–14B Q4–Q6 |
| 24 GB | ≤ 约 19 GiB | ≤ 约 20.5 GiB | 14B Q8, 27–32B Q4 |

HuggingFace 模型页面会列出每个量化的文件大小 —— 下载前请对照此表。该表只是下载前的估算；文件落盘后，`atlas tier fit /path/to/model.gguf` 才是权威答案（它读取模型真实的 KV 几何结构，预算可能向任一方向相差数 GB）。`atlas onboard` 也会自动打印同样的适配结果。

### 模型文件未找到

**现象：** llama-server 立即退出，报错 "failed to load model" 或类似信息。

**解决方法：** 检查模型路径：
```bash
# Docker Compose — model must be in ATLAS_MODELS_DIR (default: ./models/)
ls -la "models/$ATLAS_MODEL_FILE"

# Bare metal — check ATLAS_MODELS_DIR + ATLAS_MODEL_FILE
ls -la "$ATLAS_MODELS_DIR/$ATLAS_MODEL_FILE"
```

文件名必须与 `.env` 中必填的 `ATLAS_MODEL_FILE` 选择匹配。

### 显存不足

**现象：** llama-server 启动后不久崩溃或被 OOMKill。`nvidia-smi` 显示显存接近 100%。

**解决方法：** 请确保：
1. 没有其他 GPU 进程在运行（`nvidia-smi` —— 检查其他 CUDA 进程）
2. 你有 16GB+ 显存
3. 运行时已按你的模型 + GPU 调整大小：`atlas tier fit --write`（不要把 `ATLAS_CTX_SIZE` 提高到它推荐的值之上）

```bash
# Kill other GPU processes if needed
nvidia-smi --query-compute-apps=pid --format=csv,noheader | xargs -I{} kill {}
```

### 语法未强制执行（模型输出思维块）

**现象：** 模型输出 `<think>` 标签或原始文本，而非 JSON 工具调用。

**解决方法：** 代理在 `/v1/agent` 的 agent 循环处理器内部自动设置 `response_format`。`ATLAS_GRAMMAR_MODE` 决定其形态：默认的 `strict` 发送 `{"type":"json_object","schema":<full tool-call schema>}`，让 llama-server 的 GBNF 采样器只能发出 tool_call/text/done 联合形态之一；`ATLAS_GRAMMAR_MODE=loose` 只发送 `{"type":"json_object"}`（有效 JSON，不强制形态）—— 这是为 schema 转 GBNF 处理得不好的模型准备的逃生口（Gemma 家族模型需要 `loose` —— strict 模式会让它们疯狂输出 done）。如果你直接通过 `/v1/chat/completions` 或 `/v1/completions` 调用 llama-server，需要自己带上该参数：
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

如果返回的是原始文本而非 JSON，说明你的 llama.cpp 构建不支持 `response_format`。请从最新源码重新构建。

### 上下文窗口过小

**现象：** 工具调用参数被截断。工具结果携带 "Tool call was truncated (output too long for context window)" / "Your output was truncated — the content is too long for a single tool call"，或代理日志显示 `truncated args detected for <tool> at turn N`。

**解决方法：** 每 slot 的上下文（`ATLAS_CTX_SIZE` ÷ `ATLAS_PARALLEL_SLOTS`，compose 默认 131072 ÷ 4 = 每 slot 32k）可能对当前任务太小。`atlas tier fit` 会显示你的 GPU 支持的最大预算。请检查：
```bash
# Docker Compose
grep CTX_SIZE .env

# Bare metal
ps aux | grep llama-server | grep ctx-size
```

---

## 代理问题

### Agent 循环未激活

**现象：** 请求直接发送到 llama-server。没有工具调用、没有流式状态图标、没有 V3 pipeline。

**原因：** 你访问的是错误的端点。agent 循环仅在 `POST /v1/agent` 上运行。`POST /v1/chat/completions`（以及 `/v1/` 下的其他路径）是到 llama-server 的透明透传 —— 没有工具、没有 V3、没有流式聊天事件。

**解决方法：** 把客户端指向 `POST http://localhost:8090/v1/agent`。Bubbletea TUI（`atlas` / `atlas tui`）会自动这样做。如果你在编写第三方客户端，`/v1/agent` 的 SSE 事件协议见 [docs/API.md](../../API.md)。`ATLAS_AGENT_LOOP` 环境变量开关已不存在 —— 区分基于端点，而非配置。

### V3 Pipeline 未对功能文件触发

**现象：** 所有 `write_file` *或* `edit_file` 调用都是 T1（直接写入）。输出中没有 V3 pipeline 阶段。

V3 在**所有条件**满足时触发：
1. 文件内容达到 **10 行以上**（不足 10 行始终为 T1）
2. 文件有**至少 2 个逻辑指标**（函数定义、控制流、API 模式）—— **或**具有可识别的代码/标记扩展名（`.py`、`.go`、`.js`、`.html` 等），此时即使没有任何指标，10 行以上也会归入 T2
3. V3 服务可通过 `ATLAS_V3_URL` 访问
4. 仅对 `edit_file`：编辑结果值得整文件重跑 —— 圈复杂度 ≥ 8，或无法测量复杂度时 ≥ 80 行

配置、数据、样式、Markdown 和 shell 文件（`package.json`、`.yaml`、`.css`、`.md`、`.sh` 等）无论大小始终为 T1。请求的 tier 会被转发给 V3，但不决定是否激活 —— 由文件自身的 tier 决定。

`write_file` 和 `edit_file` 都会经由 V3。

**诊断方法：**
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

如果 V3 不可达，代理会记录 `V3 failed: ...` 并回退到直接写入，不会中断这次编辑。

### 截断错误（write_file 反复失败）

**现象：** 反复出现类似 "Your output was truncated — the content is too long for a single tool call." 的错误。

**原因：** 模型试图在一次调用中写入过多内容。代理检测到截断的 JSON 并拒绝了该工具调用。

代理会拒绝所有针对已有路径的 `write_file` —— `write_file` 用于创建文件 —— 并告知模型改用 `edit_file`。仅有的例外：5 行以下的文件、磁盘上看起来已损坏的文件（散文式前言、游离的 markdown 围栏），以及本会话自己写入的文件。连续 3 次失败后，错误循环熔断器会停止 agent 并返回摘要。

**解决方法：** 重新表述请求，要求进行针对性修改而非完整文件重写 —— 用"为登录函数添加输入验证"代替"重写 auth.py"。

代理会区分真正的截断（args 载荷超过 200 字节）与带着空/缺失 `args` 发来的工具调用 —— 后者会得到逐工具的提示，例如 `read_file: no arguments provided. Call with {"path":"<file>"}`，而不是截断改写。它还会把 OpenAI 风格（`arguments`）、Anthropic 风格（`parameters`）以及顶层内联的参数形态归一化为标准的 `args` 信封。如果归一化后工具调用仍然是空的，代理会记录 `[agent] turn=N EMPTY ARGS — raw model output: "..."`，让你能看到确切形态并重新表述。

### 工具结果与下一个动作之间的长时间停顿

**现象：** 一个工具成功了，然后 agent 循环空转约 30 秒才进入下一轮。没有错误、没有输出 —— 最终下一个工具调用才出现。

**发生了什么：** 在受约束的 JSON 语法下，一些本地模型会在工具结果之后把 EOS 作为第一个 token 发出，返回空内容，需要解析错误重试路径来恢复 —— 那就是丢失的约 30 秒。

**怎么做：** 代理在 `callLLMConstrained` 内部捕获空轮次，并以 `temperature=0.7` 加一条继续提示就地重试一次。如果持续复发，重启代理以清空 llama.cpp 的 slot 缓存：
```bash
docker compose restart atlas-proxy llama-server
```
检查 `docker compose logs atlas-proxy | grep -E "empty LLM|raw_len=0"` —— 首次调用和重试都出现 `raw_len=0`，说明模型的状态比重试所能处理的更糟。

### V3 已确认修复后模型仍继续编辑

**现象：** agent 完成了一次成功的 V3 验证编辑（TUI 显示以 `Probe passed` 结尾的 V3 进度事件），然后又重读同一文件并开始编辑无关的函数。每次后续编辑都触发又一轮完整的 V3 周期（约 110 秒）。

**发生了什么：** 紧凑的本地模型可能难以自我评估"用户原本的问题解决了吗？"，在一次已验证的编辑之后继续规划更多工作。

**怎么做：** agent 循环会在一次 V3 验证的写入之后附加一条强烈的用户角色提示，引导其发出 `{"type":"done"}`。如果模型无视它，请在提示词中更明确地说明你只想要这一处修改。更强硬的停止手段（按文件的编辑上限、自动 done）作为后续选项在跟踪中。

### 模型幻觉出以前会话的文件名

**现象：** 全新会话、全新提示词，模型的第一个工具调用却是对一个本工作区不存在的文件的 `read_file` —— 通常是你最近在别处用过的文件。

**发生了什么：** llama.cpp 的 KV slot 在多次 chat completion 之间保持，以维持缓存温热。跨会话时，上一会话 token 的残留注意力偏置可能泄漏进低熵输出，比如编造的文件名。

**怎么做：** 每个用户轮次开始时都会擦除**所有** llama KV slot（`--parallel` > 1 时会话可能落在任一 slot 上），让下一次补全重新编码系统提示（温 GPU 上约 1-2 秒）。如果你宁愿让缓存完全保持温热，可禁用按会话擦除：
```bash
# .env
ATLAS_FRESH_SLOT_PER_SESSION=0
```
修改后重启代理。如果禁用擦除后出现幻觉，重启 `llama-server` 以清空所有 slot。

### 多文件项目：sandbox 报 `ModuleNotFoundError`

**现象：** 编辑一个导入了同项目另一模块的文件。V3 报告验证失败，`ModuleNotFoundError: No module named 'utils'`，尽管这个导入在你的机器上没问题。

**发生了什么：** V3 的 `SandboxAdapter` 会把 agent 读过的每个文件连同 `solution.py` 一起送进 sandbox 工作区。不在读取集（`ctx.FilesRead`）里的文件不会在场，其导入自然失败。

**怎么做：** 通过 `read_file` 读取缺失的文件，让它进入项目上下文。如果你直接调用 sandbox 的 `/execute` API，在请求体中传入辅助文件：
```bash
curl -X POST http://localhost:30820/execute -d '{
  "code": "from utils import greet\nprint(greet(\"x\"))",
  "language": "python",
  "files": {"utils.py": "def greet(n): return f\"hi {n}\""}
}'
```

### Curses 底行 `addwstr() returned ERR`

**现象：** 你的 curses 程序在运行时崩溃，报 `_curses.error: addwstr() returned ERR`，但 ATLAS 报告该编辑通过了 V3 验证。

**发生了什么：** 向 curses 窗口的最后一个单元格（row=LINES-1 或 column=COLS-1）写入，按 curses 的文档化行为会返回 ERR。`interactive_lint` 会拒绝在那里写入却没有 `try/except curses.error` 包裹的候选，因此 V3 必须找到带包裹的变体才能通过。惯用的修法：
```python
try:
    stdscr.addstr(curses.LINES - 1, 0, border)
except curses.error:
    pass  # writing the bottom-right cell errors; benign
```

**怎么做：** 如果 V3 自己合成不出这个包裹，明确告诉模型：*"wrap the addstr call at line N in `try: ... except curses.error: pass`."* 检查 `docker compose logs v3-service | grep interactive_lint` 确认 lint 门控触发了。

### V3 在非 Python 文件上挂起数分钟

**现象：** 让 ATLAS 写一个 HTML/CSS/JSON 文件导致约 5 分钟的停顿，伴随 PR-CoT 修复尝试和 LLM 超时。文件最终经由直接写入回退落盘。

**发生了什么：** V3 冒烟检查是语言感知的 —— 它从目标文件的扩展名推导语言并路由到正确的检查器（`.py` → Python 编译、`.js` → `node --check`、`.ts` → `tsc --noEmit`、`.go` → `gofmt -e`、`.rs` → `rustc`、`.sh` → `bash -n`、`.html` → `html.parser`、`.xml` → `ElementTree`、`.json` → `json.loads`、`.yaml` → `yaml.safe_load`）。无法识别的扩展名会回退到 Python 并失败，进而级联进修复。注意 `.c`/`.cpp`/`.h` 不在扩展名映射（`v3-service/pipeline.py` 的 `_ext_to_lang`）中，因此即使 sandbox 本身有 C/C++ 检查器，C/C++ 文件也会撞上 Python 回退。

如果 `/v3/generate` 收到了被批准的项目构建命令，V3 会在语法/自测验证之后发出一个 `build_verify` 事件。命令在一个临时 sandbox 工作区中运行，候选会覆盖到项目上，因此失败的构建证据会阻止 `passed=true`，而不会把候选写进真实的检出。覆盖快照会跳过依赖缓存、密钥、模型/数据工件、符号链接和大文件，并强制文件数与字节数限制。如果一个项目的构建需要重量级依赖，请把它们作为显式验证工作流的一部分装进 sandbox 工作区。

**怎么做：** 对无法识别的扩展名，把它加进 `v3-service/pipeline.py` 的 `_ext_to_lang` 并重建 `v3-service` 镜像。V3 出错时代理会回退到直接写入，因此文件无论如何都会落盘 —— 只是慢。检查 `docker compose logs v3-service | grep smoke_check` 确认路由到了正确的语言。

### "再修一次"的提示词不触发 V3 Pipeline

**现象：** 第一个请求创建了文件且 V3 运行了。像 "ok" 或 "yes" 这样简短的跟进只得到一句对话式回复 —— 没有工具调用、没有 V3 事件。

**发生了什么：** agent 循环的 tier 分类器（`proxy/agent.go:classifyAgentTier`）只回答一个问题：这是对话，还是工作？默认是工作，T0 需要正面证据，因为两类错误的代价相差很大。把对话读成工作，只是为一条模型一轮就能收尾的消息浪费一次 planner 调用；把工作读成对话，则会把该轮次上限压到 5 并跳过 planning，直接让请求失败。

只有当消息少于 12 个字符（`hi`、`thanks`、`ok`），或呈现为疑问句时才算对话式 —— 以 `?` 结尾，或以疑问词（`why`、`what`、`how`、`is`、`can` 等）开头。但表示任务的措辞优先于二者，因此 `can you fix the login bug?` 尽管带问号仍是工作。其余一律视为工作：`still doesn't work, try again` 和 `the snake is moving way too fast, slow it down` 都不指名文件、也不匹配任务动词列表，但两者都会走 pipeline。

**怎么做：** 把你想要的说出来，哪怕很简短 —— "yes, fix it" 就能越过 T0 门控。如果一个跟进跑了 agent 循环但 V3 保持沉默，那么门控不在请求 tier —— 而在文件自身的 tier。见 [V3 Pipeline 未对功能文件触发](#v3-pipeline-未对功能文件触发)，并检查 `docker compose logs atlas-proxy | grep -E "write_file|edit_file"` 中的文件 tier 行（例如 `[write_file] app.py → T1:simple (8 lines)`）。

### 编辑前未读取文件

**现象：** `edit_file` 失败并报错 "file not read yet — use read_file first before editing."

**原因：** 代理会跟踪 agent 读取过的文件。如果模型试图编辑本次会话中未读取的文件，编辑会作为过时保护被拒绝。

**解决方法：** 模型应先读取文件。如果持续失败，在 TUI 中输入 `/clear` 重置聊天历史并重新表述。

### 文件被外部修改

**现象：** `edit_file` 失败并报错 "file modified since last read — read it again before editing."

**原因：** 文件在模型读取后被磁盘上的其他操作（你或其他进程）修改。代理会比较修改时间戳。

**解决方法：** 模型需要重新读取文件。这通常在下一轮会自动解决。

### 探索预算警告

**现象：** 输出显示 "You have full project context in the system prompt. Do not read more files."

**原因：** 模型连续进行了 4 次以上的只读调用（read_file、search_files、list_directory）而没有写入任何内容。4 次读取时代理会注入引导写入的提示；5 次以上则注入更强的提示。读取始终会执行 —— 提示只是引导下一轮，绝不会跳过读取。

**解决方法：** 如果模型确实在探索中卡住了，请更具体地说明你想要修改的内容。

---

## Geometric Lens 问题

### Lens 未加载/不可用

**现象：** 代理健康检查显示 `"lens": false`。或启动时显示 "Lens unavailable — verification disabled."

**影响：** ATLAS 仍可工作，但没有 C(x)/G(x) 评分。V3 候选选择回退到仅沙箱验证。

**解决方法：** 检查 Lens 健康状态和日志：
```bash
curl -s http://localhost:8099/health
docker compose logs geometric-lens
```

常见原因：
- Lens 无法连接到 llama-server（检查 `LLAMA_URL` 环境变量）
- 模型权重文件缺失（服务会优雅降级 —— 如果你尚未训练自定义模型，这是预期行为）

### 所有分数接近 0.5

**现象：** 无论代码质量如何，每个候选都得到 `cx_energy: 0.0` 和 `gx_score: 0.5`。

**原因：** 模型权重未加载。模型缺失时，服务返回中性默认值。

**验证方法：**
```bash
curl -s http://localhost:8099/internal/lens/gx-score \
  -H "Content-Type: application/json" \
  -d '{"text": "print(1)"}' | python3 -m json.tool
```

如果返回 `enabled: false` 或 `cx_energy: 0.0`，则模型未加载。对于全新安装来说这是预期行为 —— 模型权重不包含在仓库中，需要训练或从 [HuggingFace](https://huggingface.co/datasets/itigges22/ATLAS) 下载。

### 分数看着合理但量级严重偏离（嵌入约定漂移）

**现象：** 一切都报告健康 —— Pod `Ready`、`/health` 返回 200、`gx-score` 返回看起来在区间内的 `gx_score` 和 `likely_correct` 判定 —— 但 `cx_energy` 与其校准区间相差若干数量级（例如模型的通过/失败均值在 20–30 时却出现 ~600）。在这种状态下启动的门控基准测试会产出一份完整、合理、却完全无效的结果。

**原因：** 嵌入服务器提供的 `/embedding` 约定，与 Geometric Lens 的 `C(x)`/`G(x)` 工件训练时所用的不一致 —— 通常是逐 token 而非池化，或未归一化而非 L2 归一化（‖v‖≈60 而不是 ~1）。维度相同、分布不同；cost-field MLP 会外推出一个巨大的能量，`cx_normalized` 随之饱和。这通常发生在没有 `--pooling mean` 就重建服务栈之后（llama-server 没有 `--embd-normalize` 这个服务端标志；lens 通过 `/embedding` 请求体中的 `embd_normalize` 逐次请求 L2 归一化）。

**验证：** lens 会在启动时以及每次重载/重训练时，对存储的指纹重新打分。检查 `/ready` 和 `/health`：
```bash
curl -s http://localhost:8099/health | python3 -m json.tool | grep -A2 fingerprint
```
出现 `fingerprint_ok: false` 以及指出期望值与观测值能量的 `fingerprint_error`，就是漂移信号 —— `/ready` 返回 503，打分响应会带上 `"drifted": true` 且所有 `calibrated` 标志被强制为 false，因此下游不会把它们误当作可信结果。

**解决：**
1. 确认嵌入服务器的约定。池化 + 归一化的服务器会返回 ‖v‖≈1 的扁平向量：
   ```bash
   curl -s -X POST http://localhost:8080/embedding -H 'Content-Type: application/json' \
     -d '{"content":"def add(a, b): return a + b"}' | python3 -c "import sys,json,math; e=json.load(sys.stdin)[0]['embedding']; import itertools; v=e if not isinstance(e[0],list) else [sum(c)/len(e) for c in zip(*e)]; print('shape', 'per_token' if isinstance(e[0],list) else 'flat', 'norm', round(math.sqrt(sum(x*x for x in v)),3))"
   ```
   如果是 `shape per_token`，或 `norm` 远离 1.0，说明服务器配置有误。
2. 设置 `ATLAS_EMBED_POOLING=mean`（默认值；见 [CONFIGURATION.md](../../CONFIGURATION.md)），并重建 llama-server 容器，让入口点固定这些标志。
3. 服务器提供正确约定后，启动自检的指纹校验会通过，`/ready` 返回 200。如果工件早于指纹机制，一次重训练（`atlas lens retrain`）会写入指纹，并把 `embedding_contract` 刻进 `model_identity.json`。

### 嵌入向量提取失败

**现象：** Lens 日志显示 "embedding extraction failed" 或超时等错误。

**原因：** Lens 调用 llama-server 的原生 `/embedding` 端点。如果 llama-server 过载或嵌入功能未启用，则会失败。

**解决方法：**
```bash
# Test the native embedding endpoint directly
curl -s http://localhost:8080/embedding \
  -H "Content-Type: application/json" \
  -d '{"content": "test"}' | python3 -m json.tool
```

`--embeddings` 标志由 llama-server 的入口点在每种部署模式（Compose、裸机、K3s）中都会设置 —— 自嵌入始终开启，因为 Geometric Lens 依赖它。逐层 hidden-states 扩展也由原生的 `/embedding` 路径（而非 `/v1/embeddings`）承载。

### `/internal/lens/retrain` 返回 503 "models directory is mounted read-only"

**现象：** 对 lens 服务 POST `/internal/lens/retrain` 返回 HTTP 503，带 ``"reason": "models directory is mounted read-only; run host-side retrain via `atlas lens retrain`"``。

**原因：** 标准的 Compose 部署把 lens 模型目录以只读（`:ro`）挂载进容器，因此服务内的重训练端点无法写出新权重。该端点在训练前会探测可写性，宁可提前拒绝也不浪费一轮训练。

**解决方法：** 在主机侧运行重训练 —— `atlas lens retrain`（反馈语料）或 `atlas lens build`（bench 候选）在主机上写出工件，然后 `docker compose restart geometric-lens` 加载它们（服务在启动时读取工件）。基准驱动的在线重校准（`lens_feedback`）会记录这次拒绝并保留其样本缓冲区，因此不会丢失任何东西。

---

## Sandbox 问题

### Sandbox 不可达（健康检查）

**现象：** 代码从未被测试。代理健康检查显示 `"sandbox": false`。

**解决方法：** 检查 Sandbox 健康状态：
```bash
# Docker Compose (host port 30820 maps to container port 8020)
curl -s http://localhost:30820/health

# Bare metal (direct port 8020)
curl -s http://localhost:8020/health
```

如果 Sandbox 容器正在运行但不健康，请检查日志：
```bash
docker compose logs sandbox
```

### 代码执行超时

**现象：** Sandbox 返回 `"error_type": "Timeout"`。代码执行时间过长。

**默认超时：** 每个请求 30 秒，上限为 `MAX_EXECUTION_TIME`。Compose 栈把该上限设为 300 秒（通过 `.env` 中的 `ATLAS_SANDBOX_MAX_EXECUTION_TIME`），与代理的 `run_command` 上限一致，让较长的构建和测试套件能跑完；Compose 之外，执行器代码内的上限是 60 秒。

**解决方法：** 如果你的代码确实需要更多时间，在请求中设置更高的超时值（不超过上限），或提高 `ATLAS_SANDBOX_MAX_EXECUTION_TIME`。如果代码存在无限循环，这是预期行为。超时会杀掉整个进程组，因此命令派生的子进程不会残留。

### 语言不受支持

**现象：** Sandbox 对特定语言返回错误。

**支持的语言（执行）：** Python、JavaScript、TypeScript、Go、Rust、C、C++、Bash。纯语法检查（`/syntax-check`、V3 冒烟检查）还额外覆盖 HTML、XML、JSON 和 YAML。

检查可用的运行时：
```bash
curl -s http://localhost:30820/languages | python3 -m json.tool
```

---

## 基准测试问题

### bench 运行的任务数少于请求数（`LIMITED MODE: running N tasks` 的 N 小于 `--tasks`）

**现象：** `atlas bench --tasks 200` 显示 `LIMITED MODE: running 100 tasks`（或任何低于请求的数量），或恢复的运行打印 `Resuming: N/N tasks already done, 0 remaining` 后立即退出。

**原因：** LiveCodeBench 数据集缓存（`benchmark/datasets/.cache/livecodebench_v5.jsonl`）是一次部分下载。HuggingFace rows API 可能在分页中途失败；旧版本会缓存已获取的部分并永久信任该文件。release_v5 完整集约有 880 个任务。

**解决方法：** 将缓存标记为 partial 后重新运行 —— 加载器会重试完整下载（仅当所有源都失败时才回退到现有副本）：
```bash
touch benchmark/datasets/.cache/livecodebench_v5.jsonl.partial
atlas bench --run-id <your-run-id> --tasks 200
```
已完成的任务绝不会丢失：结果以每任务一个 JSON 的形式保存在 `benchmark/results/<run-id>/v3_lcb/per_task/` 下，运行器恢复时会跳过已有结果文件的任务。无论因何中断（OOM、重启、会话关闭），重新运行相同的 `atlas bench` 命令即可恢复。

## 性能问题

### 生成速度慢（约 2 tok/s）

模型正在 CPU 而非 GPU 上运行。请检查：
1. `nvidia-smi` —— llama-server 是否列为 GPU 进程？
2. `-ngl 99`（`--n-gpu-layers`）—— 所有层是否已卸载到 GPU？
3. NVIDIA Container Toolkit —— 容器运行时是否已配置 GPU 访问？

**预期性能：** 在 RTX 5060 Ti 16GB 上启用语法强制执行时约 51 tok/s。

### V3 Pipeline 需要几分钟

对于 T2 文件来说这是正常的。V3 pipeline 会进行多次 LLM 调用：
- **仅探测（最佳情况）：** 约 10-15 秒（1 次生成 + 1 次评分 + 1 次测试）
- **Phase 1 生成：** 约 1-2 分钟（PlanSearch + DivSampling + 评分）
- **Phase 3 修复：** 约 2-5 分钟（PR-CoT + Refinement + Derivation，如果需要）

如需更快（但质量较低）的结果：
- 保持文件不足 10 行（维持 T1，不触发 V3）—— 可识别的代码扩展名达到 10 行以上时，无论复杂度如何都会归入 T2
- 降低逻辑复杂度（减少函数、控制流）
- V3 仅在确实需要时才触发 —— 简单文件会立即写入

### 内存使用过高

**现象：** 系统变得卡顿或服务被 OOMKill。

**预期内存使用：**
- llama-server：约 8 GB（模型在显存中，仅占少量系统内存）
- geometric-lens：约 200 MB（PyTorch 运行时 + 模型）
- v3-service：约 150 MB（PyTorch 运行时）
- sandbox：约 100 MB（基础值，编译时会有峰值）
- atlas-proxy：约 30 MB（Go 二进制文件）

**总计：** 约 500 MB 系统内存 + 8.2 GB 显存。如果你的系统内存不足 14 GB，其他服务可能会争夺内存。

---

## 获取帮助

如果你的问题未在此列出：
1. 查看服务日志：`docker compose logs <service-name>`
2. 检查代理健康检查端点：`curl http://localhost:8090/health`
3. 参见 [CONFIGURATION.md](../../CONFIGURATION.md) 了解所有环境变量
4. 在 [GitHub](https://github.com/inferstep/ATLAS/issues) 上提交 issue
