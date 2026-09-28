<!-- source: README.md synced-through: 4f1be83 -->
> **[English](../../../README.md)** | **简体中文** | **[日本語](../ja/README.md)** | **[한국어](../ko/README.md)**

<p align="center">
  <img src="../../images/herodemo.gif" alt="ATLAS TUI 实时演示"/><br/>
  <sub><i>ATLAS TUI 实况（10× 加速），V3 pipeline 正在处理一次文件创建。</i></sub>
</p>

<h1 align="center">A.T.L.A.S.</h1>
<p align="center"><b>Adaptive Test-time Learning and Autonomous Specialization</b></p>

<p align="center">
  <img src="https://img.shields.io/badge/version-V3.1.4-blue" alt="Version"/>
  <img src="https://img.shields.io/badge/license-AGPL--3.0-blue" alt="License"/>
  <img src="https://img.shields.io/badge/model-agnostic-green" alt="模型无关"/>
</p>

<p align="center">
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/test.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/test.yml?branch=main&label=tests" alt="测试"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/install-test.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/install-test.yml?branch=main&label=install%20matrix" alt="安装矩阵"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/codeql.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/codeql.yml?branch=main&label=codeql" alt="CodeQL"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/container-scan.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/container-scan.yml?label=container%20scan" alt="容器扫描"/></a>
  <a href="https://github.com/itigges22/ATLAS/actions/workflows/verify-tags.yml"><img src="https://img.shields.io/github/actions/workflow/status/itigges22/ATLAS/verify-tags.yml?label=release%20signature" alt="发布签名"/></a>
</p>


## 🌎 什么是 ATLAS？

**ATLAS 是一个本地编程 agent，把前沿模型式的推理与验证能力带给紧凑的开源模型。** 它把更多的智能放进模型周围的系统里（规划、候选生成、质量评分、沙箱测试与修复），让较小的模型也能完全在你自己的硬件上完成真实的软件工作，既不需要托管 API，也没有按 token 计费。

## 💡 为什么选择 ATLAS？

* **让更小的模型发挥更大价值。** ATLAS 在模型周围加入规划、候选选择、验证与修复，而不是押注于单次生成。
* **先验证，再接受。** 生成的代码可以在隔离的执行环境中编译、测试并修正。
* **把算力花在刀刃上。** 简单的编辑走更短的路径，更难的任务则获得更多候选、推理与验证。
* **运行你自己的模型。** 在 NVIDIA、AMD、Apple Silicon、Vulkan 或支持 CPU 的硬件上使用兼容的 GGUF 模型。
* **在本地掌控数据。** ATLAS 不会有意把你的仓库或提示上传到托管模型或 ATLAS 运营的服务。沙箱命令默认可以访问外部网络；设置 `ATLAS_SANDBOX_NET_INTERNAL=true` 可禁用该访问。
* **掌控完整技术栈。** ATLAS 开源且自托管，不需要托管模型或第三方模型提供商 API 密钥；ATLAS 服务之间使用本地的逐安装服务令牌进行认证。

---

## 📰 最新动态

- **2026-07-06** - **[V3.1.3 "Maia" 发布](https://github.com/itigges22/ATLAS/releases/tag/v3.1.3)** - 面向生产平台的一轮打磨：分阶段升级/回滚并自动还原、SQLite 状态存储（不再需要 Redis）、签名的工件清单、结构化日志 + 关联 ID、交互式权限、会话恢复，以及两轮对抗性 bug 修复扫荡
- **2026-06-17** - **[V3.1.2 "Maia" 发布](https://github.com/itigges22/ATLAS/releases/tag/v3.1.2)** - 更广的硬件覆盖（ROCm / Metal / Vulkan）、自带模型的 Lens + ASA 训练、基于自有工作负载的在环 lens 重训练（后已移除，见 CHANGELOG），以及一轮 agent 可靠性加固
- **2026-05-12** - **[V3.1.0 "Maia" 发布](https://github.com/itigges22/ATLAS/releases/tag/v3.1.0)** - 原生 Bubbletea TUI、一条命令的 bootstrap、流式 Lens + ASA 激活操控、感知 AST 的外科式编辑
- **2026-03-26** - [Hacker News 首页](https://news.ycombinator.com/item?id=47533297) - 489 点赞、285 条评论
- **2026-03-05** - **V3.0 发布** - 在冻结的 Qwen3-14B 上运行的多阶段 V3 流水线。*随本次发布公布的 LiveCodeBench 74.6% 已撤回：基准测试运行器从未运行 LiveCodeBench 的隐藏测试，并且只要三个候选之一、或看过失败输出的修复通过题面给出的示例，就把任务记为通过（[撤回说明](../../reports/V3_ABLATION_STUDY.md)）。当前产品重新验证后将重新测量。*
- **2026-02-18** - **[V2.0 发布](../../../CHANGELOG.md)** - 基准测试基础设施、HumanEval/MBPP/LiveCodeBench/GPQA/SciCode 评估套件

## ⭐ Star 历史

<!-- Self-hosted chart: rendered weekly by .github/workflows/star-chart.yml
     onto the `star-history` asset branch (scripts/star-history-chart.py).
     Replaces the star-history.com embed, whose shared token pool
     rate-limits unpredictably. -->
<a href="https://github.com/itigges22/ATLAS/stargazers">
 <picture>
   <source media="(prefers-color-scheme: dark)" srcset="https://raw.githubusercontent.com/itigges22/ATLAS/star-history/star-history-dark.svg" />
   <source media="(prefers-color-scheme: light)" srcset="https://raw.githubusercontent.com/itigges22/ATLAS/star-history/star-history-light.svg" />
   <img alt="Star history chart" src="https://raw.githubusercontent.com/itigges22/ATLAS/star-history/star-history-light.svg" width="100%" />
 </picture>
</a>

<sub>每周一通过 GitHub Actions 自动更新。</sub>

---

## 🧱 ATLAS 的功能

1. **[atlas-tui](../../CLI.md)** - 基于 Bubbletea 的原生终端 UI，是官方聊天客户端。在任意项目目录中输入 `atlas` 即可启动。
   - [实时 Pipeline 视图](../../CLI.md#panes) - 在侧边窗格中观看 V3 各阶段的实时流
   - [斜杠命令](../../CLI.md#slash-commands) - `/add`、`/diff`、`/commit`、`/run` 操作本地文件上下文与 shell
   - [输入模式](../../CLI.md#input-modes) - 聊天、`!bash`、`/slash` 三种模式带提示下拉

2. **[atlas-proxy](../../ARCHITECTURE.md#3-atlas-proxy-outer-layer)** - 基于 Go 的 agent 循环，负责编排整个系统。
   - [工具调用路由](../../ARCHITECTURE.md#tools) - 按复杂度层级分类文件操作
   - [语法强制执行](../../ARCHITECTURE.md#grammar-enforcement) - GBNF 模式强力引导输出符合预期 JSON 结构，并由代理恢复格式错误或截断的输出
   - [BiasBusters](../../ARCHITECTURE.md#tool-selection-bias-mitigations) - 四道组合的缓解措施（描述、语法禁用、系统提示、ASA 操控），把模型推向用 `structural_edit` 做结构性代码编辑
   - [安全限制](../../ARCHITECTURE.md#safety-limits) - 轮次上限、token 预算、超时

3. **[V3 Pipeline](../../ARCHITECTURE.md#4-v3-pipeline-inner-layer)** - 多阶段代码生成；把单个提示词转化为经过验证的候选。
   - [PlanSearch](../../ARCHITECTURE.md#pipeline-flow) - 约束驱动的结构化规划
   - [DivSampling](../../ARCHITECTURE.md#pipeline-flow) - 跨温度和策略的多样化候选生成
   - [预算层级](../../ARCHITECTURE.md#pipeline-flow) - 分配器为每个任务选择计算层级；目前层级只决定生成多少候选，不改变思考深度
   - [PR-CoT Repair](../../ARCHITECTURE.md#pipeline-flow) - 根据沙箱的错误输出修复失败的候选
   - [Refinement Loops](../../ARCHITECTURE.md#pipeline-flow) - 沙箱验证与修正，然后重复

4. **[Geometric Lens](../../ARCHITECTURE.md#5-geometric-lens)** - 基于模型自身嵌入的能量打分，无需外部预言机。（[什么是 "Geometric Lens"？](../../ARCHITECTURE.md#why-geometric-lens)）
   - [C(x) Cost Field](../../ARCHITECTURE.md#scoring-models) - 模型隐藏维度→512→128→1 的 MLP，用于评估候选质量
   - [G(x) Quality Prediction](../../ARCHITECTURE.md#scoring-models) - 用于候选选择的 XGBoost 集成
   - [RAG / PageIndex V2](../../ARCHITECTURE.md#rag--pageindex-v2) - 感知 AST 的代码检索与项目索引
   - [Confidence Router](../../ARCHITECTURE.md#confidence-router--pattern-cache) - Thompson Sampling 把算力路由到真正需要的候选

5. **[Sandbox](../../ARCHITECTURE.md#6-sandbox)** - 用于构建验证的隔离执行环境。
   - 多语言执行：Python、Rust、Go、C、Shell 等
   - 评分前先做编译与检查
   - 同时运行自生成测试和已有测试套件

6. **[llama-server](../../CONFIGURATION.md#6-llama-server)** - 在单块消费级 GPU 上的本地 LLM 推理。
   - GPU 加速的量化推理 (Q6_K / Q4_K_M) - NVIDIA CUDA、AMD ROCm、Apple Metal（macOS 混合方案）和 Vulkan；Intel SYCL 在路线图上
   - token 级的语法约束解码
   - 自嵌入，因此 lens 不需要第二个模型

完整文档（安装、架构、配置、故障排查、基准测试报告，以及[每个组件背后的研究依据](../../SOURCES.md)）位于 [docs/](../../) 目录中。

---

## 🚀 快速开始

一键安装：
```bash
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

不想把一个随 `main` 变动的脚本直接管道进 bash？还是同一个安装器，另有两种更稳妥的运行方式：
```bash
# Pinned to a release: script, checkout, and images all at the signed tag
curl -fsSL https://raw.githubusercontent.com/itigges22/ATLAS/v3.1.3/scripts/atlas-bootstrap.sh \
  | ATLAS_BOOTSTRAP_REF=v3.1.3 bash

# Review before running
curl -fsSL -o atlas-bootstrap.sh https://raw.githubusercontent.com/itigges22/ATLAS/main/scripts/atlas-bootstrap.sh
less atlas-bootstrap.sh
bash atlas-bootstrap.sh
```

脚本会自动识别发行版（Ubuntu、Debian、RHEL、Fedora、Rocky、Alma）和 GPU 厂商（NVIDIA → nvidia-container-toolkit；AMD → ROCm 设备直通），安装相应的运行时，下载模型权重，构建 ASA 操控向量，并启动整个栈。预计 10-30 分钟，大部分时间花在模型下载上。

完成后，在任意项目目录中执行 `atlas`。

**系统要求**

| | |
|---|---|
| GPU | 显存 16GB 以上。NVIDIA（CUDA，支持 (Supported)）、AMD（ROCm，社区验证 (Community-tested)）或 Apple Silicon（Metal，macOS 混合方案，支持）；其余大多数 GPU 由 Vulkan（预览 (Preview)）覆盖。预构建的 CUDA 镜像面向 Blackwell（RTX 50xx）；更早的 NVIDIA GPU 需要做一次性的本地重建（参见 [SETUP.md § CUDA Compute Capability](../../SETUP.md#cuda-compute-capability-dockerfilev31)）。级别定义见 [SUPPORT_MATRIX.md](../../../SUPPORT_MATRIX.md)；GPU 列表见 [SETUP.md § Supported GPUs](../../SETUP.md#supported-gpus)。想估算某个具体模型能否放进你的显卡，参见 [What fits on my GPU?](../../TROUBLESHOOTING.md#what-fits-on-my-gpu)。 |
| 运行时 | Docker（NVIDIA：+ nvidia-container-toolkit；AMD：单独的 Docker 即可）或 Podman |
| Python | 3.9 及以上 |
| 磁盘 | 约 20GB CUDA / 约 22GB ROCm（模型权重 + 容器镜像） |

Apple Silicon 通过原生 macOS 混合 Metal 方案运行（原生 llama-server 负责推理，其余组件用 Docker - 详见 **[SETUP_MACOS.md](../../SETUP_MACOS.md)**）；Intel Arc (SYCL) 在路线图上。完整的手动安装路径（Docker Compose、裸机、K3s）和全部 bootstrap 参数请参见 **[SETUP.md](../../SETUP.md)**。

---

## ⚠️ 已知限制

- **Linux Docker 栈，外加一条原生 macOS 路径。** NVIDIA（支持 (Supported)）、AMD ROCm（社区验证 (Community-tested)）和 Vulkan（预览 (Preview)）的 Docker 路径今天即已存在；Apple Silicon（支持）通过原生 macOS 混合 Metal 方案运行 ([#32](https://github.com/itigges22/ATLAS/issues/32))。Intel Arc / SYCL 为路线图 (Roadmap) 级别。级别定义见 [SUPPORT_MATRIX.md](../../../SUPPORT_MATRIX.md)。
- **ATLAS 目前没有基准测试结果。** V3.0 的 LiveCodeBench 数字已撤回（见最新动态），当前版本的能力与可靠性数字尚未测量。只有在留出任务上重新测量后才会公布数字；逐模型结果在 [#28](https://github.com/itigges22/ATLAS/issues/28) 中跟踪。
- **复杂功能添加可能不稳定。** 紧凑模型有时会在陌生代码库上花掉几轮 agent 回合去探索而不是写代码。
- **语法约束解码比不受约束的解码更慢。**

---

## 🗺️ 路线图

**V3.1.3 "Maia"** - 当前版本。在 V3.1.2 之上的生产平台打磨：带自动还原的分阶段 `atlas upgrade`/`rollback`、用 SQLite 状态存储替代 Redis（[ADR 0007](../../adr/0007-sqlite-state-store.md)；2026-09 在 `dev` 上随其唯一使用者模式缓存一并停用）、签名的工件清单、带跨服务关联 ID 的结构化 JSON 日志、交互式权限提示、会话恢复、类型化的配置校验/迁移，以及两轮对抗性 bug 修复扫荡（33 个确认修复）。

**V3.1.2 "Maia"** - 在 V3.1.0 基座（TUI、一条命令安装、流式 Lens + ASA）之上的更广硬件覆盖、自带模型训练与 agent 可靠性加固。
- 硬件覆盖：通过 llama.cpp 支持 AMD ROCm，包括 RDNA4 / RX 9070 (gfx1200/gfx1201) ([#26](https://github.com/itigges22/ATLAS/issues/26))；Apple Silicon 原生 macOS 混合 Metal 方案（[#32](https://github.com/itigges22/ATLAS/issues/32)，见 [SETUP_MACOS.md](../../SETUP_MACOS.md)）；Vulkan 通用回退，覆盖 AMD / Intel / Snapdragon / 通过 MoltenVK 的 Apple / CPU ([#114](https://github.com/itigges22/ATLAS/issues/114))。
- 自带模型：本地 Lens 训练流水线（`atlas lens build`，[#100](https://github.com/itigges22/ATLAS/issues/100)）与 ASA 逐模型校准对齐（`atlas asa check/build/publish`，[#113](https://github.com/itigges22/ATLAS/issues/113)）- 为额外的 GGUF 训练 Lens + ASA 工件，逐模型的工作阈值随 lens 一起发布。
- Agent 可靠性：工具结果可见性修复、读取去重、回溯 → 定向编辑、`move_file`、pip 安装 / 大小写不匹配操控、沙箱 shell 策略 + 按主机调整的 cgroup 限制。
- 结构化调用图推理（[#39](https://github.com/itigges22/ATLAS/issues/39) / [#125](https://github.com/itigges22/ATLAS/pull/125)，感谢 [@yogthos](https://github.com/yogthos)）；ARCHITECTURE.md 翻译为 zh-CN / ja / ko ([#25](https://github.com/itigges22/ATLAS/issues/25))。

**V3.2** - 下一个里程碑：更深入的代码推理与规划。
- 架构优先的规划阶段 - RPG 式的先规划后填充：在模块尺度规划，再在函数尺度实现（[#120](https://github.com/itigges22/ATLAS/issues/120)，PR [#124](https://github.com/itigges22/ATLAS/pull/124)）。
- 结构化代码推理（收尾）- 求解器支撑的可达性分析 + 语法无关的小波分解，实现多分辨率的"哪些文件重要"检索 ([#39](https://github.com/itigges22/ATLAS/issues/39))。
- 带采样的推理 - 兼顾效率与质量提升 ([#9](https://github.com/itigges22/ATLAS/issues/9))。
- 顺延的基础设施：自动化 HuggingFace 提交流水线 ([#102](https://github.com/itigges22/ATLAS/issues/102))；ROCm 跑在 K3s / Kubernetes 上；注册表模型的正式基准测试 - LiveCodeBench、GPQA Diamond、SciCode ([#28](https://github.com/itigges22/ATLAS/issues/28))。

**待办 / 欢迎贡献**
- 硬件：ARM64 多架构构建 ([#115](https://github.com/itigges22/ATLAS/issues/115))、面向更大模型的多 GPU ([#34](https://github.com/itigges22/ATLAS/issues/34))、Intel oneAPI / SYCL ([#27](https://github.com/itigges22/ATLAS/issues/27))。
- 工具链：VS Code / JetBrains 扩展 ([#35](https://github.com/itigges22/ATLAS/issues/35))。
- 沙箱语言：Java / Kotlin ([#29](https://github.com/itigges22/ATLAS/issues/29))、Ruby / PHP ([#30](https://github.com/itigges22/ATLAS/issues/30))。
- 架构：模型无关的平台 ([#66](https://github.com/itigges22/ATLAS/issues/66))。

---

## ❤️ 支持 ATLAS

ATLAS 由一名大学生在业余时间、用一块消费级 GPU 独立开发（[ATLAS 背后的故事](../../STORY.md)）。如果这个项目对你有帮助，并且你愿意帮它持续走下去，请考虑 **[在 GitHub 上赞助](https://github.com/sponsors/itigges22)**。

赞助将直接用于：

- **算力与硬件** - 更多 GPU 以加快基准测试迭代，触达维护者负担不起的架构（AMD ROCm、更大显存的显卡、用于大模型实验的云端租用）。
- **贡献者奖金** - 为在实质性 PR 上投入真实时间的外部贡献者提供有意义的报酬，让 ATLAS 的成长速度超越单人节奏。
- **研究** - 围绕该架构持续的学术参与，从未来的 workshop 和会议投稿，到论文写作与合作，以验证并扩展这一方法。
- **社区** - 对社区和 ATLAS 所运行平台的持续支持，包括文档、面向用户的渠道和教学内容，帮助 ATLAS 触达更多开发者、更好地服务已有用户。

每位赞助者都会在其资助版本的发布说明中获得署名。

---

## 🤝 参与贡献

ATLAS 以开源方式开发，欢迎贡献者和核心维护者加入。修复 bug、加速器支持以及更大的子系统工作都同样欢迎。

发现 bug 或者被卡住了？**[提交一个 issue](https://github.com/itigges22/ATLAS/issues)** - 你不需要附上修复。bug 报告和反馈与代码同样有价值。

贡献指南见 **[CONTRIBUTING.md](../../../CONTRIBUTING.md)**，代码库布局概览见[仓库地图](../../MAP.md)。

---

## 📄 许可证

基于 [GNU Affero General Public License v3.0 (AGPL-3.0)](../../../LICENSE) 许可发布。
