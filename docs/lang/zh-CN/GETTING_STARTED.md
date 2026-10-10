<!-- source: docs/GETTING_STARTED.md synced-through: eec0147b99954591320ee839241ce8a268e173d6 -->
> **[English](../../GETTING_STARTED.md)** | **简体中文**

> ℹ️ **译者注：** 若本译文与英文原版 ([GETTING_STARTED.md](../../GETTING_STARTED.md)) 有出入，以英文原版为准。


# ATLAS 快速上手

第一个小时，从头到尾：确认你的机器能跑 ATLAS、完成安装、验证安装、做成第一个任务。本页只链接到各详细指南而不重复其内容——哪一步需要更多细节，跟着链接走即可。

## 安装之前

### ATLAS 是什么（以及不是什么）

ATLAS 是一个本地编程 agent：模型跑在你自己的 GPU 上，外面裹着一整套机器——规划改动、在隔离沙箱中验证生成的代码、修复失败的部分。ATLAS 完全在本地运行，不需要托管模型，也不需要第三方模型提供商的 API 密钥。它会为每次安装生成一个本地服务令牌，用于 ATLAS 各服务之间的通信，一切都不按 token 计费。

先设定一个预期：带验证的紧凑本地模型，和前沿托管模型是两种不同的体验。它在边界清晰、描述完备的任务上大放异彩，靠增量积累赢得信任；在漫无边际、缺乏描述的任务上，它不是魔法。

### 硬件现实核查

下载任何东西之前，先做这三件事：

1. 在 [SETUP.md § 选择你的安装路径](SETUP.md#选择你的安装路径) 里找到你的 GPU 所在的行——它写明了推荐方式，**以及你的硬件的支持级别**。预览（Preview）和不支持（Unsupported）行就是字面意思。
2. 查一下什么模型放得进你的显存：
   [TROUBLESHOOTING.md § 我的 GPU 能放下什么？](TROUBLESHOOTING.md#我的-gpu-能放下什么)
   （16 GB 显存即可从容运行参考模型）。
3. RTX 50 系列之前的老 NVIDIA 卡：先读
   [SETUP.md](SETUP.md#cuda-计算能力-dockerfilev31) 里的 CUDA 计算能力（compute capability）说明——发布的镜像面向 Blackwell GPU，更老的卡需要一次性的本地重建。

### 安装究竟做了什么

一键 bootstrap 会安装 Docker 和你的 GPU 运行时（通过发行版包管理器，需要 sudo）、把仓库克隆到 `/opt/atlas`、下载模型权重（约 7 GB，带哈希校验——这是最慢的一步）、写入 `.env`，然后启动五个仅绑定 localhost 的容器。预计 10-30 分钟、约 20 GB 磁盘。它改动的每一样东西都列在
[SETUP.md § 方式 0](SETUP.md#方式-0一键-bootstrap) 里，包括锁定发布标签（pinned-release）与先审后跑（review-before-running）的变体——如果你不想把脚本直接管道进 bash 的话。

第一个任务之前值得知道的一个安全事实：ATLAS 不会有意把你的仓库或提示词上传到托管模型或 ATLAS 运营的服务。模型写出的 shell 命令在锁定沙箱容器内运行而不是在你的主机上，但沙箱命令默认拥有出站网络访问，以便工具链拉取依赖。设置 `ATLAS_SANDBOX_NET_INTERNAL=true` 可禁用沙箱出口。提交之前，先审阅生成的代码、命令和 diff。

## 安装

按 [SETUP.md](SETUP.md)（Linux）或 [SETUP_MACOS.md](../../SETUP_MACOS.md)（Apple Silicon）操作。对大多数 Linux + GPU 机器来说就是一条命令：

```bash
curl -fsSL https://raw.githubusercontent.com/inferstep/ATLAS/main/scripts/atlas-bootstrap.sh | bash
```

## 首次启动

### 用 atlas doctor 做验证

```bash
atlas doctor
```

全绿说明容器、模型、工件与配置全部对得上。警告会在输出中就地解释；失败会打印出确切的修法。有失败就不要继续——[TROUBLESHOOTING.md](TROUBLESHOOTING.md) 的错误索引覆盖每一种常见错误。

### TUI 一览

在任意项目目录里运行 `atlas`。你会得到一个聊天窗格、一个文件窗格，外加一个实时 Pipeline 窗格，展示 agent 工作时正在做什么——各阶段、工具调用与验证结果实时流入。布局与每一个按键绑定见 [CLI.md](../../CLI.md#panes)。

## 你的第一个任务

在一个**纳入版本控制**的仓库里挑一个小的、边界清晰的任务（或者克隆一个练手仓库）。好的首个提示词描述一个具体改动："给 cli.py 加一个 `--verbose` 标志，打印每一步的耗时"好过"改进 CLI"。别在还有未提交改动的仓库里做第一次尝试——不是 ATLAS 鲁莽，而是 `git diff` 正是你审阅它的方式。

你会看到：

- **小改动**在 agent 读完相关文件后直接写入（T1 路径）。
- **大改动**触发 V3 流水线：Pipeline 窗格展示规划、多个候选的生成、沙箱验证与修复，然后才有任何东西落盘（T2 路径）。
- **权限提示**在默认模式下出现在破坏性步骤（shell 命令、删除）之前——`y` 允许一次，`a` 本会话内允许，`n` 拒绝。每次删除单独询问；`a` 从不覆盖之后的删除。各模式的文档见 [CLI.md § 权限模式](../../CLI.md#permission-modes)。

之后：用 `/diff`（或 `git diff`）审阅，跑你的测试，满意再提交。`/undo` 会软重置 agent 做出的最后一次提交；`Ctrl+C` 中途取消一个回合。

## 自带模型

想跑一个不在注册表里的模型？`atlas onboard` 会带你走完自带 GGUF（bring-your-own-GGUF）的流程（[CLI.md](../../CLI.md)）。

## 接下来去哪

- [CLI.md](../../CLI.md) —— TUI 与 `atlas` 子命令能做的一切
- [OPERATIONS.md](../../OPERATIONS.md) —— 第二天：升级、备份、运维手册
- [ARCHITECTURE.md](../../ARCHITECTURE.md) —— 各组件如何拼在一起
- [docs/README.md](../../README.md) —— 完整的文档索引
