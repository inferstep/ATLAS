<!-- source: docs/ARCHITECTURE.md synced-through: a5021a0 -->
> **[English](../../ARCHITECTURE.md)** | **简体中文** | **[日本語](../ja/ARCHITECTURE.md)** | **[한국어](../ko/ARCHITECTURE.md)**

# ATLAS 架构

ATLAS V3.1.6 的系统架构。采用双层设计：外层 agent 循环负责工具调用的编排，内层 V3 pipeline 则生成多样化的代码候选，并配合构建验证与基于能量的选择。

---

## 1. 系统概览

```mermaid
graph LR
    User["User"] --> TUI["atlas-tui\n(Bubbletea)"]
    TUI --> Proxy["atlas-proxy\n:8090"]

    subgraph outer["Outer Layer"]
        Proxy -->|"grammar JSON"| LLM["llama-server\n:8080"]
        Proxy -->|"T2 files"| V3Service["v3-service\n:8070"]
    end

    subgraph inner["Inner Layer"]
        V3Service --> LLM
        V3Service --> Lens["geometric-lens\n:8099"]
        V3Service --> Sandbox["sandbox\n:30820"]
        Lens --> LLM
    end

    style User fill:#333,color:#fff
    style TUI fill:#1a3a5c,color:#fff
    style Proxy fill:#1a3a5c,color:#fff
    style LLM fill:#5c1a1a,color:#fff
    style V3Service fill:#2d5016,color:#fff
    style Lens fill:#2d5016,color:#fff
    style Sandbox fill:#2d5016,color:#fff
```

各服务既可以通过 Docker Compose（推荐）作为容器运行，也可以通过 `atlas` 启动器作为本地进程运行。只有 llama-server 使用 GPU，其余所有组件都跑在 CPU 上。

聊天前端是 **atlas-tui**（Bubbletea）：一个原生 Go 终端 UI，消费 `/v1/agent`（按轮次的聊天 SSE）和 `/events`（面向 pipeline 窗格的全局类型化信封事件流）。用 `atlas`（默认交互模式）或 `atlas tui`（显式指定）启动。Pipeline 窗格实时展示 V3 各阶段；聊天窗格通过 glamour 渲染助手的 markdown；斜杠命令 `/add /diff /commit /run` 等负责处理本地文件上下文与 shell 调用。输入是模式感知的（chat / `!bash` / `/slash`），并带有提示下拉。

希望使用工具调用 + V3 pipeline 的第三方客户端应直接对接 `/v1/agent`；`/v1/chat/completions` 是对 llama-server 的透传（见 §3）。该契约记录在 [API.md](../../API.md) 中。

### 1.1 支持的加速器

llama-server 是唯一使用 GPU 的服务；其余每个 ATLAS 服务都跑在 CPU 上（代理是 Go，v3-service / geometric-lens / sandbox 是 Python）。这让多后端的表面积保持很小 —— 添加一个新加速器意味着一个新的 Dockerfile 加上一个入口点环境变量分支，而不是改动整个 pipeline。

| 后端 | 状态 (V3.1.x) | 镜像 / 构建路径 | Compose override | 已测试显卡 |
|---|---|---|---|---|
| **CUDA** (NVIDIA) | 支持 (Supported)（自 V3.1.0 起） | `inference/Dockerfile.v31` → `atlas-llama` | （默认） | RTX 5060 Ti 16GB（基准）。发布的镜像只针对 Blackwell（计算能力 12.0/12.1）编译；更早的代次需要本地重建 —— 见 [SETUP.md](../../SETUP.md) |
| **ROCm / HIP** (AMD) | 社区验证 (Community-tested)（自 V3.1.1 起） | `inference/Dockerfile.rocm` → `atlas-llama-rocm` | `docker-compose.rocm.yml` | RX 7900 XTX（社区冒烟测试，GH #26） |
| **Metal** (Apple Silicon) | 支持 ([#32](https://github.com/inferstep/ATLAS/issues/32)) | 混合方案：原生 llama-server (Metal) + 其余组件用 Docker（macOS 无法将 GPU 直通给容器） | `docker-compose.macos.yml` | M 系列；≤16 GB 用 Q4_K_M，≥24 GB 统一内存用 Q6_K |
| **Vulkan**（跨厂商回退） | 预览 (Preview) | `inference/Dockerfile.vulkan` → `atlas-llama-vulkan` | `docker-compose.vulkan.yml` | lavapipe CPU 启动路径（已冒烟测试）；尚无真实 GPU 验证 |
| **SYCL** (Intel Arc) | 路线图 (Roadmap) —— Intel Arc 目前使用 `vulkan` | 待定 | 待定 | — |

**后端选择发生在安装时，而非运行时。** `atlas init` 运行 `tier.detect_gpu()`（见 `atlas/cli/commands/tier.py`），在所有检测到的厂商中挑选显存最大的 GPU（可用 `ATLAS_GPU_VENDOR` / `ATLAS_GPU_INDEX` 覆盖），并把 `ATLAS_BACKEND={cuda|rocm|metal|vulkan}` 写入 `.env`。当主机存在打包好的原生后端时，检测会解析到它：NVIDIA 用 CUDA，x86_64 上的 AMD 用 ROCm，macOS 用混合 Metal 路径。当主机没有打包好的原生后端时（Intel Arc、arm64 上的 AMD、无法识别的厂商），向导会提供 Vulkan 通用回退（默认选是）：一个镜像覆盖 AMD、Intel、Adreno、MoltenVK 和 lavapipe CPU 光栅化器，性能比调优过的原生后端低大约 20–40%。只有当完全不存在可用的后端时，它才会拒绝 —— 而不是写出一个无法启动的 `.env`。每个后端都有自己预构建的镜像；用户不会运行一个打包了所有后端库的臃肿镜像。

**自带模型的表面（V3.1.1）。** `atlas lens check` 是针对运行中的 llama-server 的一次廉价预检，用于报告当前加载的模型是否与 Lens 兼容。`atlas lens build --samples <path>` 封装了 `geometric-lens/geometric_lens/training.py`，按模型原生的嵌入维度训练全新的 C(x)（`cost_field.pt`）**和** G(x)（XGBoost）工件。二者结合让用户无需 fork lens 代码即可换入非默认的 GGUF —— C(x) 构造函数接受任意 `input_dim`，因此逐模型变化的只有训练出来的权重。面向用户的流程见 [CLI.md § atlas lens](../../CLI.md#atlas-lens)；`atlas lens publish`（或合并的 `atlas publish`）会把工件上传到 HuggingFace，并开出固定其哈希的注册表 PR。

**与厂商无关的部分**（在每个后端上都可用）：语法约束的 JSON、自嵌入（`/embedding`）、逐层隐藏状态、ASA 控制向量（由 llama.cpp 的 `control_vector_load` 加载，与后端无关）、KV 缓存量化、整个外层 agent 循环、V3 pipeline、Geometric Lens 以及 sandbox。

**逐后端有差异的部分：**
- **Flash attention。** CUDA + ROCm：完整支持。Metal：受限（llama.cpp 的 Metal 后端对部分 head 尺寸支持 flash-attn；不支持时默认关闭）。Vulkan：取决于驱动。
- **固定（pinned）主机内存。** `GGML_CUDA_NO_PINNED` 适用于 CUDA + ROCm（HIP 在 GGML 兼容层镜像了 CUDA 的路径）。Metal/Vulkan 不使用 CUDA/HIP 的固定内存路径。
- **多 GPU + 张量并行。** V1 在每个后端上都只支持单 GPU；多 GPU 是 GH #34，不绑定到特定厂商。
- **Apple 统一内存。** macOS 共享 GPU+系统内存；"VRAM" 的算法实际上是"总共 16 GB 减去操作系统 + 应用"。见 §7。

K3s 部署路径（`scripts/install.sh`，清单在 `templates/` 中）截至 V3.1.1 仅支持 CUDA —— ROCm K8s 方案已推迟到 V3.2 的基础设施清单（需要 `/dev/kfd` + `/dev/dri` hostPath 挂载以及 `render`/`video` 组成员身份，相当于集群级别的 `docker-compose.rocm.yml`）。

---

## 2. 服务

| 服务 | 端口 | 语言 | 用途 |
|---------|------|----------|---------|
| **llama-server** | 8080 | C++ (llama.cpp) | LLM 推理（CUDA / ROCm / Metal / Vulkan；SYCL 在路线图上 —— 见 §1.1）、语法约束的 JSON、自嵌入、逐层残差隐藏状态 |
| **atlas-proxy** | 8090 | Go | agent 循环、工具调用路由、tier 分类、`/v1/agent` SSE、`/events` 类型化 SSE、`/cancel`。`/v1/chat/completions` 原样透传给 llama-server。 |
| **atlas-tui** | （客户端） | Go | Bubbletea TUI；消费 `/events` 和 `/v1/agent` SSE 流。 |
| **v3-service** | 8070 | Python | V3 pipeline 的 HTTP 封装（PlanSearch、DivSampling、PR-CoT 等） |
| **geometric-lens** | 8099 | Python (FastAPI) | 内部 `/internal/*` 打分服务：C(x) 能量打分、G(x) XGBoost 质量预测、逐步打分 |
| **sandbox** | 30820（主机）/ 8020（容器） | Python (FastAPI) | 隔离的代码执行、编译、检查、测试运行 |

---

## 3. atlas-proxy（外层）

代理是聊天前端的入口点。它在 `/v1/agent` 上接收用户消息（类型化事件流 —— TUI 使用的就是它），并运行一个内部 agent 循环：调用 llama-server、解析工具调用、执行它们，然后把事件流式回传。`/v1/chat/completions` 端点是对 llama-server 的透明透传；保留它是为了 SDK 兼容性，它并不运行 agent 循环。完整的事件类型目录见 [API.md](../../API.md)。

代理由 12 个 Go 文件组成，每个文件只负责一件事：

| 文件 | 职责 |
|---|---|
| `main.go` | HTTP 服务器、路由、鉴权、透传、错误信封、私密值日志过滤 |
| `agent.go` | agent 循环：轮次状态、LLM 调用、计划生成、卡死循环断路器 |
| `tools.go` | 16 个工具定义与执行器、层级分类、工具调用语法 |
| `gates.go` | 诚实性/计划闸门：声明校验、结构、语法、内嵌脚本、计划遵循、计划提醒、资源 lint |
| `detectors.go` | 卡死模式检测：工具重复、推理重复、traceback 定位 |
| `context.go` | 上下文增强：符号索引、项目扫描、工作区隔离、会话文件清单 |
| `permissions.go` | 权限闸门（`/v1/permission`）、信任模式、硬阻断模式 |
| `lens.go` | lens 打分调用、校准状态 |
| `guardrails.go` | 按工具的引导防护（收缩、缺失命令/模块的引导、doctype 剥离） |
| `events.go` | 类型化信封 broker（`/events`）与 SSE 管道 |
| `v3_bridge.go` | 面向 v3-service `/v3/generate` + `/v3/plan` 的 SSE 客户端 |
| `types.go` | 共享类型、层级、轮次上限 |

```mermaid
graph LR
    subgraph core["Core Loop"]
        Grammar["Grammar"] --> AgentLoop["Agent Loop"] --> TierClass["Tier Classifier"]
    end
    subgraph tools["Tools"]
        ReadF["read_file"] ~~~ WriteF["write_file"] ~~~ EditF["edit_file"] ~~~ RunCmd["run_command"]
    end
    subgraph pipeline["Verify-Repair"]
        VR["Verify-Repair"] --> BOK["Best-of-K"] --> BV["Build Verifier"]
    end
    subgraph format["I/O"]
        SSE["SSE / Events"] --> V3Bridge["V3 Bridge"] --> ProjDet["Project Detector"]
    end

    core --> tools --> pipeline --> format

    style core fill:#1a3a5c,color:#fff
    style tools fill:#333,color:#fff
    style pipeline fill:#2d5016,color:#fff
    style format fill:#555,color:#fff
```

### Agent 循环流程

```mermaid
flowchart LR
    Start["User msg"] --> Build["Build prompt"] --> Call["llama-server"] --> Parse["Parse JSON"]
    Parse --> Route{Type?}

    Route -->|"tool_call"| Tier{"T2?"}
    Tier -->|"Yes"| V3["V3 Pipeline"] --> Result["Append result"]
    Tier -->|"No"| Exec["Execute tool"] --> Result
    Result --> Budget{"Budget?"}
    Budget -->|"< 4"| Call
    Budget -->|"4"| Warn["Nudge: write now"] --> Call
    Budget -->|"5+"| Esc["Escalated nudge"] --> Call

    Route -->|"text"| Stream["Stream"] --> Call
    Route -->|"done"| Done["End"]

    style Start fill:#1a3a5c,color:#fff
    style Done fill:#333,color:#fff
    style V3 fill:#2d5016,color:#fff
```

### 语法强制执行

每一次模型输出都被约束到三种有效 JSON 形态之一：

```json
{"type": "tool_call", "name": "<tool_name>", "args": {...}}
{"type": "text", "content": "<message>"}
{"type": "done", "summary": "<summary>"}
```

在默认的 `strict` 模式下，代理发送一个完整的 JSON schema —— 带 `additionalProperties: false` 的 `oneOf`，工具名从注册表中枚举 —— llama-server 在 token 生成期间将其作为语法强制执行。语法约束让畸形输出变得罕见，而非不可能：`ATLAS_GRAMMAR_MODE=loose` 只发送 `{"type":"json_object"}`（有效 JSON，不强制形态 —— 有些模型需要它），而且回复 token 上限可能在 JSON 中途截断。代理把解析当作可能失败的操作对待 —— 它会从散文/`reasoning_content` 中恢复 JSON，在执行前检测被截断的工具参数，把针对性的解析失败描述反馈回去，并在连续三次失败后中断循环。

**围栏内容通道（`@fenced`）。** 整文件正文不走 JSON 通道。JSON 字符串里的代码在每一行密集内容上都付出转义压力，一个已部署的 12B 模型可测地无法承受：同一个解法以围栏 markdown 块发出时 6/6 可解析，以 JSON 字符串发出时 0/6（丢失右括号、字面量 `\n` 把多条语句熔进注释、join 丢失空格）。系统提示指示模型把 `write_file` 的 `content` 设为恰好 `@fenced`；代理随后发起一次*不受约束*的子调用（无语法、无 `response_format`），其回复就是单个围栏块中的文件，并按目标扩展名打上标签（` ```python `、` ```html `，……）。信封仍在 JSON 约束之下；只有文件正文移到纯文本。机制的细节（`proxy/agent.go::fetchFencedContent`）：子调用是临时的（永不追加进对话——在主线程看来，模型写了 `@fenced`，写入就完成了）；若回复里的文件本身含有 ` ``` `，则由一个尾部围栏锚点提取，使内部围栏不会截断它；把哨兵值*和*文件放进同一个字符串的模型，会剥掉哨兵值并使用内联内容；两次尝试，然后一次退回，请求内联内容。每次尝试都是真实的生成，并计入运行总量（最终 `done` 载荷上的 `fenced_calls` / `fenced_tokens`，每次尝试一个 `fenced_fetch` 阶段事件）。

重复检测器对**模型**发出的调用做指纹，快照取在工具调用分支顶部、在围栏解析用取回的正文改写 `parsed.Args` 之前。没有这个快照，检测器读到的字节在每次尝试中都不同，而调用本身却是字节等同的——这正是那次冻结运行里七轮 `@fenced` 循环直至触顶 harness 上限而检测器全程沉默的原因。取回的字节不受影响：它们仍然驱动变更、各道门、账本和写入。目标路径会被归一化后进入签名，因此 `solve.py` 和 `./solve.py` 买不到两份额度。

当某条路径的围栏配额用尽而模型再次请求该通道时，拒绝会被做成一次它可据以行动的东西：规范路径、文件可读时的当前源码（有界，或一条明确说明它不在磁盘上的注记），以及仍然可用的替代方案——整文件内联、一次定向编辑、一次读取，或模型自发的 `run_command`。每个规范路径只提供一次；别名、新的一轮，或一次无关的成功都无法重新打开它。它不发起任何生成，不运行任何命令，也不强制任何工具。持续请求的模型会落入普通的失败调用计数并停下来。

当 run-first 门已经要求某个被警告过的文件必须运行，而模型却用同样的原始 `@fenced` 写入作答时，重复这一要求就不再是机制。在该复现上，模型会拿到文件的真实现状——规范路径、至多 120 行带行号的内容，以及账本若对那批字节持有一条当前失败判定时的该判定——而那次无用的调用被扣下。它运行在围栏解析之前，因此被拦截的重复不花费生成；它只读不写，不运行命令，不强制工具，每个规范路径至多触发一次，且当剩余工作预算不足 90 s 时跳过。若模型仍然重复被拦截的意图，既有的封禁与重复检测器会诚实地终结这次运行。

run-first 门本身引用一条指令，`runFirstInstruction`：运行该扩展名文件的命令（`runCommandFor`：`python3`、`node`、`go run`、`java`，或 Kotlin 的 `kotlinc … && java -jar` 组合），会退出的脚本经 `run_command` 发送，提供服务的脚本经 `run_background` 发送（与前台服务重定向所用的同一个 `runsAServerLoop` 判定）。两个工具在其命令执行了该文件时都会解除被警告的标记，因此一个在后台启动、又通过其稳定窗口期回溯信息被读取的服务器脚本算作已运行。`python3 -m <module> file.py` 与 `python3 -c ...` 把文件交给另一个程序，永远不会被视为启动其服务循环。2026-09-14 在这三条规则之前实测：该门要求 `run_command python3 app.py`，重定向拒绝了它，`run_background` 从未解除标记，`python3 -m py_compile app.py` 被当作服务启动拒绝，会话以磁盘上仍然留着的 SyntaxError 结束。

因为每次尝试都花费一次生成，该通道只为能够执行的调用打开。`fencedCallIsExecutable` 会运行该调用反正必须通过的那些检查——参数形状、非空路径、`validateToolWorkspacePaths`、`shouldDenyToolCall`——以真正调用它们的方式，因此它的拒绝是 `executeToolCall` 拒绝的子集。一个不可用的调用只是不被解析：哨兵值留在 `content` 里，工具用它自己的报错信息与 `MutationNone` 拒绝它，会话花费一轮而非一次生成。在该检查存在之前实测：一个 300 s 的金丝雀运行到第 36 轮什么都没写，每一轮都是一个带 `content: "@fenced"` 却没有路径的 `write_file`，每一次都打开通道，每一次失败都记在空路径的额度键上。

围栏子调用可能失速：流打开了，然后在服务端视线之外生成期间什么都不发。看门狗将其切断（`fencedStalledTimeout`，约 25 s）。失速是会话的属性而非文件的属性，因此一旦某个文件的取回失速，下一个文件的取回会以同样方式失速。第一次失速取回之后，通道在本次运行剩余部分被关闭（`ctx.FencedStalls` 越过 `fencedSessionStallLimit`，由 `fencedChannelDisabledForSession` 测试）：下一个 `@fenced` 立即返回、不发起子调用，模型被引导改为内联写入文件。内联是安全的退路，因为下文的写入完整性检查如今能截获 `@fenced` 当初为规避截断而存在的那种截断。

### 写入完整性检查

三道检查拦住一个损坏的文件以干净写入的面目落地。每道都点名真实成因，使模型的重发能够修复它。

**被未转义引号截断的写入会被拒绝。** 一个 `write_file` 的 `content` 若持有未转义的 `"`，其 JSON 字符串会提前结束；宽松解码器随后把文件剩余部分读进一个被丢弃的键，落地一个报告成功的截断文件。循环会检查解析后的编辑类工具调用中是否存在工具真实签名（经输入 schema 反射读取）之外的键、且带有泄漏文件内容的形态——过长，或持有代码标点——并拒绝它（`swallowedContentFeedback`），告知模型字符串提前结束，请转义内部引号或改用 `structural_edit`。

**一个每次渲染都会 500 的 Jinja 模板会在写入时被截获。** 模板可以解析为 HTML 却在渲染时失败：`{% for x in xs %)` 用 `)` 闭合标签。`html.parser` 会放过它，服务器也会启动，但每次渲染都抛出 `TemplateSyntaxError`。沙箱语法检查也会解析 Jinja，其范围限定在真正是模板的文件（`templates/` 目录，或带 `.jinja`/`.jinja2` 的名字），且只在存在 `{%` 语句标签时进行，因此共享 `{{ }}` 的 Vue/Angular HTML 永远不会交给 Jinja 解析器；未知标签/过滤器错误（第三方扩展）会被丢弃。代理会发送文件路径，使沙箱能在直接写入门与 V3 的编译冒烟检查两处都应用该范围。

**基线已可编译时，交互类任务跳过 V3 修复。** V3 的修复阶段只在没有任何生成候选通过时才会到达。对交互类任务而言，唯一的信号是「能否编译」（服务器无法在沙箱里完整运行），因此此时唯一可编译的代码就是模型自己的写入，而一个修复到可编译的候选并不比一个可编译的基线得到更多验证。基线可编译时，跳过修复并交回基线，把预算还给 agent 循环，而不是花掉会话的约 50% 去重新推导它。算法类任务保留修复：它们可以被执行，其模型生成的自测结果被记录为诊断信息。


### 工具

`proxy/tools.go` 中注册了 16 个工具：

| 工具 | 用途 | 只读 |
|------|---------|-----------|
| `read_file` | 读取文件内容（可选 offset/limit） | 是 |
| `outline_file` | 列出文件的顶层函数/类及其行号范围，不含函数体（`.py` 使用 tree-sitter，其余为尽力而为的扫描）。外科式读取的入口点：先 outline，再用带 offset/limit 的 `read_file` | 是 |
| `write_file` | 创建一个新文件（对超过 5 行的已有文件会被拒绝 —— 见安全限制） | 否 |
| `edit_file` | 针对 ≤10 行改动的外科式内联字符串替换（old_str/new_str） | 否 |
| `insert_after` | 在给定行号（`read_file` 打印的行号）之后插入新行。适用于**新增**代码（分支、函数、import）且不改动任何已有内容的场景：没有需要复现的 `old_str`，而这正是长跨度下失败的那一步 | 否 |
| `replace_lines` | 用新内容替换一个行范围（`start_line`..`end_line`，即 `read_file` 打印的行号）。适用于**修改**代码而无需复现它：锚点是断言的两行（范围的首行与末行，忽略空白），而不是整个跨度，因此逐字复现的负担是 2 行而非 N 行。每次调用上限 60 行 | 否 |
| `structural_edit` | 通过 tree-sitter 选择器（`function:NAME`、`class:NAME`、`<tag>`）对整个函数/类/HTML 元素进行重写；对整节点替换而言，必须优先于 edit_file 使用。GH #39，v1 中仅支持 .py/.html/.htm | 否 |
| `delete_file` | 删除文件或空目录（之后强制退出循环） | 否 |
| `move_file` | 在工作区内移动或重命名文件（例如 `index.html` → `templates/`）。纯粹的重定位 —— 绕过 V3/外科式编辑门控，拒绝覆盖已存在的目标。由于 shell `mv`/`cp` 会被拒绝，这是"重新组织文件"的受支持路径 | 否 |
| `find_file` | 按文件**名**/路径做正则搜索（廉价的存在性检查 + 定位）。区别于在文件内容中 grep 的 `search_files`。 | 是 |
| `search_files` | 跨文件内容做正则搜索（最多 200 个匹配，跳过 .git/node_modules） | 是 |
| `list_directory` | 列出目录内容及其类型和大小 | 是 |
| `run_command` | 通过 sandbox 容器执行 shell 命令；5 分钟超时上限 | 否 |
| `run_background` | 在 sandbox 中启动一个长时间运行的进程（例如 `python app.py`）；立即返回一个 `job_id` | 否 |
| `tail_background` | 通过 `job_id` 获取某个后台任务新增的 stdout/stderr | 是 |
| `stop_background` | 通过 `job_id` 对某个后台任务发送 SIGTERM/SIGKILL | 否 |

### 交付物账本（观测性）

`executeToolCall` 把每次调用对会话交付物所做的记录进 `AgentContext.Ledger`，以解析后的工作区路径为键。它只观测、不决策：无恢复、无拒绝、无事件。`ToolEffect` 决定处理方式。

| 效果 | 记录什么 |
|--------|------------------|
| 直接变更（`write_file`、`edit_file`、`structural_edit`、`insert_after`、`replace_lines`） | 调用后文件被重新读取并哈希，因此记录的哈希指向磁盘上的字节而非工具提议的字节 |
| `delete_file`、`move_file` | 路径确实消失后记一条墓碑记录，保留任何检查点字节并禁止自动恢复；移动操作对其目标做全新观测，不转移任何判定 |
| `run_command` | 每个被跟踪路径重新哈希：未变的文件保留判定，已变的失去判定。工作区还会在前后各遍历一次（`applyShellChanges`，仅 stat，跳过依赖、缓存与构建目录）：命令创建或变更的、属于完成判定可评判类型的文件作为会话的工作进入账本；请求开始时已存在而命令将其删除的文件记入 `deleted:shell` 墓碑，完成判定将其读作一次无人批准的删除；运行创建而又被后续命令删除的文件离开账本。触及上限的遍历在摘要中留下一条注意事项 |
| `run_background`、`stop_background` | 启动时升起一个工作区危险信号，仅在一个被回收的退出码上降下。每次调用前后像 `run_command` 一样遍历，首个存活任务启动后那次调用之后的遍历被保留为基线。一旦不再有任务可能仍在写入（完成判定回收一个已退出的任务、`stop_background` 确认一次退出，或会话结束时回收其任务），工作区在相同规则下与该基线比较；账本已记录的删除（`delete_file`、`move_file`、更早的遍历）不计在该任务头上 |

证明自身未变更任何东西的分支（`MutationNone`）不记录任何内容，因此对会话从未拥有的路径的一次被拒写入不会进入账本。

`ValidationKind`/`ValidationStatus` 从工具自身的结果复制，并绑定到它们所描述的哈希上；文件哈希一旦离开被验证的那个，`CurrentValidation()` 立即返回未知。字节仅在那批字节的一次显式 `passed` 上被检查点化，受每文件 256 KiB、每会话 2 MiB 的上限约束。超过上限时观测被保留，字节不被保留，`checkpoint_unavailable` 记录原因。

哪些变更工具可以留下检查点，取决于它们是否对自己写入的字节产生一次显式通过，由 `TestWhichMutatorsCanEverPromoteACheckpoint` 度量：

| 工具 | 能否达到 `passed` | 说明 |
|------|------------------|------|
| `write_file`、`edit_file`、`insert_after`、`replace_lines` | 能 | 经语法门，当沙箱可达时 |
| `structural_edit` | 不能 | tree-sitter 拼接属于 v3-service；该工具对自己写入的字节不运行任何检查，因此报告 `syntax`/`not_run` |
| `delete_file`、`move_file` | 不能 | 二者都不改变内容，因此都无可给定的判定 |

七个变更工具的每个分支都携带自己的 `MutationStatus`：该边界在结构上被禁止对直接变更工具做分类，因此一个未分类的分支将无法与一次有意的空操作区分。`noMutation` / `errNoMutation` 标记一个未形成待写字节的分支，`refusedNoCheck` 标记一个拒绝了已就绪字节的守卫，`errFailedMutation` 标记一次被尝试过且使目标处于不确定状态的写入。`TestEvery*OutcomeIsClassified` 覆盖这些家族。

#### 模型看到什么

分类是服务端事实。`ToolResult.ModelFacing()` 把结果投影为 `success` / `data` / `error` 加上 V3 溯源字段，而 `MarshalText`——结果进入 `ctx.Messages` 的唯一路径——经由它。完整结构体仍为内部用途携带全部字段做序列化；该投影是一份白名单，因此新加到 `ToolResult` 的字段不会进入对话，除非有人决定不然。

三处测试守住这条边界，位于 `proxy/result_contract_test.go` 与 `proxy/tool_effect_test.go`：`TestEveryModelFacingSerializationSiteIsInventoried` 在出现新的直接序列化或经未盘点路由构建的工具消息时失败，`TestModelFacingTextCarriesNoClassification` 逐结果固定投影后的字节，而 `TestModelPromptBytesAreUnchangedByClassification` 让 agent 循环跑过两个分类发生变化的分支，并把请求体与父提交对比。

两个限制被如实携带而非绕开推理：`structural_edit` 成功时报告 `syntax`/`not_run`，因此无法晋升检查点；`move_file` 的跨文件系统复制路径在生产可达，但没有生产路径的测试，因为单个临时目录无法横跨两个文件系统。

#### 恢复

只有一个终端接有恢复逻辑：重复检测器的 stop——一个运行结束时持有它自己已证明损坏的交付物的时刻。其余十二个 `done` 发射器不受影响——一个什么也没证明的终端没有可恢复之物——若该范围扩大，`TestRestorationIsWiredToExactlyOneTerminal` 会失败。

在该终端，每个交付物被重新读取、经写入路径所用的同一语法契约重新检查，并按各自情况决定。每一子句都是一个不行动的理由：

| 需要满足 | 不满足的情形 |
|----------|---------------|
| 该路径由本会话写入 | 它从未是这里的交付物 |
| 当前字节刚被重新读取并重新检查 | 检查器无法运行，判定未知 |
| 该确切哈希持有一次已被证明的失败 | 当前内容可解析，或从未检查 |
| 持有的字节存在、在界内、哈希与自身记录一致 | 被逐出、超上限或自相矛盾 |
| 持有版本通过了*同一种*检查 | 一次结构通过不构成关于语法的证据 |
| 两个版本不同 | 文件已持有更安全的版本 |
| 未被墓碑化、未被禁止恢复 | 模型有意删除或移动了它 |
| 无存活的后台危险信号 | 后台任务可能仍在写入 |

写入经 `atomicReplaceFile`——变更工具所用的同一套先写后改名——随后结果被重新读取并哈希：一次无法证明自己精确落地的恢复是一个保留真实错误、不动当前字节的失败。恢复不设置进度提示、不登记会话写入、不发射工具事件、不主张 V3 溯源，也绝不把一次停止的运行变成完成——摘要按路径逐一披露哪些文件被放回、哪些被保留原样及原因、哪些无法恢复，不暗示它们一起移动过。

#### 终端契约与会话预算

每个会话恰好以一个终端事件结束，来自一个发射器。它携带原样的遗留 `summary` 加一个增量的 `status`——`completed`、`incomplete`、`stopped`、`timed_out`、`failed`——以及一个稳定的机器可读 `reason`。消费方把缺失、畸形或无法识别读作 `incomplete`，绝不读作完成（Go 侧 `NormalizeTerminalStatus`，`atlas/events.py` 侧 `terminal_status`）。broker 的 `done`/`stage_end` 信封报告同一结果，而不是断言 `success: true`。

对除 `completed` 之外的任何状态，句子与字段都归服务器所有。模型自己的陈述只在完成门授权之处被放行；一个没有 summary 的非完成终端会得到一句服务器撰写的陈述，点名结果、磁盘上是否有东西、它是否被证明有效；而在非完成状态上抵达发射器的完成主张会被整个替换，而非围绕其编辑。这关闭了 Phase 2B 留下的缺口：一个只读 `summary` 的客户端——`status` 存在之前的所有客户端——现在读到与同时读两者者相同的真相。

被执行前工作区边界检查拒绝的一次调用按其失败调用的本义计数，因此一个工作区根目录无法打开的会话在三轮内停止，而不是把一次拒绝重复到预算耗尽。`workspacePathFields` 是每个工具把哪些参数视作路径的唯一注册表；测试直接枚举它，不保留第二份副本。

工作区危险信号是一个以任务身份为键的集合，不是启动尝试的计数。一个在派发前就被工具拒绝的 `run_background` 不拥有任何东西；一个存活任务恰好拥有一个危险信号，重复的 id 无法二次升起；到稳定窗口期时已消失的任务被立即结算，走任何退出都要求的同一套被跟踪路径重哈希；一个可能已派发但未返回可用 id 的启动升起一个无人能回收的不明危险信号，因为会话无法在其不知道其中在运行什么的时候宣称工作区安静。结算一个任务只清除该任务，幂等，且不会下溢。在运行自身任务仍存活的 `done` 或 `text` 退出上，退出门在完成判定之前点名每个任务及其 `stop_background` 调用（与其他退出门一样有界），因此一个为验证工作而启动的服务器被模型停掉、其自身陈述成立；模型遗留运行中的任务仍使完成为 `background_work_unresolved`，任务名记入摘要。

一个没有适用检查器的交付物仍可证明完成，但只能作为存在且最新的证据，且绝不重新标注为语法通过：路径必须是散文（`isDocumentAsset`——`stripOneFenceLayer` 一直在用的那个集合）、账本必须已拥有它、其记录必须恰好读作 `none`/`not_applicable`、且该记录必须描述此刻磁盘上的字节。不支持的语言、未知扩展名或内嵌内容的模板都落在外面，因为「未运行检查器」对这些的含义不同于对一个文本文件。

运行被要求变更却从未被证明变更的路径作为未解析工作被跟踪，与交付物账本分开：账本记录会话在磁盘上拥有什么，而一个从未落地的意图不拥有任何东西。它在意图被呈交之处开启——参数已解析、路径在工作区内、权限已授予、不在拒绝清单上——这在派发之前，因此一次失败的围栏解析仍是欠账。它只在账本能对同一规范路径证明的东西上关闭：应用后字节的当前验证通过，或无检查器适用时的 `not_applicable`；删除在确认缺失时关闭；移动需要源消失且目标自身的字节被验证。以 64 条路径为界，超出即闭锁。当工作仍悬而未决，模型每代得到一次有界机会去完成它，或用 `delete_file` 显式放弃该路径；不以它的名义写入、删除或运行任何东西，散文不结算任何东西。

两个出口——模型发出的 `done` 与一次文本回复——经由同一个终结器、按同一顺序抵达该判定：已存在的交付物失败保留其自身更具体的原因；否则一个被要求改变磁盘上某物而什么都没改的运行为 `incomplete` / `action_demanded_unmet`；否则一个被要求验证而从未验证的运行为 `incomplete` / `verification_demanded_unmet`；否则完成立足。这些谓词正是重写摘要的那批，因此机器可读的一半与散文不再可能互相矛盾。

每个退出门以三次弹回为界，弹回用尽的门停止把运行送回，但不再停止计数。若其发现到退出时仍然成立，门记录它。关于交付工作的事实的发现使运行以 `incomplete` 结束，各自有自己的原因，除非更早的检查已拒绝退出：`claim_check_unresolved`（代码渲染的模板不存在）、`warned_file_never_run`（带解析警告写入的文件从未运行）与 `unread_citation`（回复引用了运行从未读过的文件）。已知有误报的启发式门放行完成，摘要点名它们仍发现的东西：没有匹配路由的表单或请求目标、无人调用的新增代码、未完成的计划步骤、只被以文件管道进 stdin 方式运行过的程序、以及在验证它们的运行之后变更的字节。在验证被要求之处，漂移、以及触及上次变更前启动的服务器的探测使运行未验证，因此以 `verification_demanded_unmet` 结束。`done` 事件在有未解析项时携带 `unresolved` 与按名点出的用尽之门。

模型发出的 `done` 仅当运行的文件义务——它声明的加上它写入的——此刻可证明满足时才是 `completed`，经由写入路径所用的同一语法契约。若任何东西被删除或移动，完成以 `delete_intent_unestablished` 拒绝：删除是否是任务在此不可知。该原因点名完成立足于什么：`deliverables_demonstrated`，当每个可运行的交付物（可执行语言的代码与 HTML 页面）都被一次当前运行证明在工作，或无可运行之物；`deliverables_parse_only`，当其中一些仅是最新且可解析。没有工作契约时、以及对于页面，没有东西要求运行，因此 `completed` 可立足于一次解析；原因言明如此，摘要说明哪些文件没有被运行。主张超出此范围的模型陈述（「所有测试通过」「一切正常」）显示在服务器句子之后并标注为未检查。

会话预算归服务器所有：总计 600 s，预留 30 s，因此工作停在 570 s。`ATLAS_AGENT_SESSION_TIMEOUT_SEC` 在 [120, 3600] 内覆盖总值；任何畸形、零、负值或越界都带一条日志回落默认。两个生命周期分开维护——响应上下存活到终结，而工作上下文（LLM、工具、门、V3、沙箱）早一个预留结束——因此停止工作的截止时刻不会杀死解释它的通道。到截止时刻服务器取消工作、只回收本会话的后台任务、重哈希被跟踪路径、原样运行 Phase 3B 恢复判定，并在预留内发射一个 `timed_out` 终端。客户端断连不是超时：工作停止、任务被回收，但没有任何东西被声称进一个已关闭的响应。Phase 2 围栏准入自动观察工作截止，因为它本已对 `ctx.Ctx` 的截止做预留。

留给后续的可复现性工作，未在此修正：系统提示按 Go map 顺序渲染工具描述，因此两次运行相同代码会产生不同的提示字节。它只影响提示复现性，不影响行为；`proxy/tool_effect_test.go` 中的 `conversationBytes` 正为此省略系统消息。

`SessionWrites` 是另一张更旧的、以模型提供的原始路径为键的表；`proxy/types.go` 记录了由此而来的别名问题。其读者是 write_file 覆盖守卫对模型自身草稿的概念、活跃调试快速路径、会话清单注记、V3 项目上下文、制品门的漂移检查与编辑工具的宽松门；这些都不在此改变。

工作契约的验证要求（`decideVerificationDemand`）把它问及的代码交付物取自账本，每个变更工具的落地都被 `recordLedgerEffect` 规范记录。回答它的覆盖——一次通过的运行点名了哪些路径、持有的是什么字节——由 `proxy/guardrails.go` 的 `changedPathsForCoverage` 计算：会话写入加账本的规范代码交付物，因此要求与其覆盖读同一身份。在那之前，覆盖只读会话写入表，而 `edit_file` 与 `structural_edit` 从不写入它、任何编辑工具也不为交付的候选写入它，因此 `task_mode: work` 下的每个编辑任务无论模型运行了什么都以 `verification_demanded_unmet` 收场。覆盖只陈述一次运行在其当前字节上点名了一条变更路径；完成仍需要那次运行，后续变更仍重新武装该要求，账本自身的验证仍结算变更债。只有运行了程序或其测试、或抓取了页面的片段才绑定覆盖，且仅当命令行报告该片段的退出状态时（`classifyCommandEvidence`）：一次解析、一次 lint 或一次构建不绑定任何东西，管道进 `tail` 的测试也不。不点名文件的运行器绑定它发现的会话文件（`runnerEntries`）：一个裸 `pytest` 或 `pytest tests/` 绑定其测试文件，`python -m unittest` 绑定其 `test*.py`，`go run .` 绑定其包，`go test ./...` 绑定含测试的包（无测试时只编译），`npm test`、`jest` 或 `vitest` 绑定其默认模式匹配的文件；import 从那里如同从点名入口一样被跟随。失败的运行同样被记录，同一字节上的失败收回更早的通过，因此最新结果说了算。要求在退出时未满足，验证门在终结器拒绝退出之前言明，附上本可运行该文件的命令。一次被取消的编辑不报告变更，账本对它无观测。`structural_edit` 把其验证记为未运行（它对自己拼接的字节不执行任何检查），因此唯一变更是一次结构编辑的任务以 `unresolved_mutation_debt` 结束，除非别的什么东西验证了该文件：一个已知的、独立的限制。


### 工具选择偏差缓解

一次实测的参考部署显示出一种偏差：即便 `structural_edit` 才是正确选择，模型也倾向于用 `edit_file`（BiasBusters arxiv 2510.00307 —— 相邻工具名的嵌入会相互竞争；描述比名称更重要）。代理中组合了四道与模型无关的防线：

1. **描述重写**（`proxy/tools.go`）。edit_file 的描述
   警告不要用于整文件/整函数；structural_edit 的描述
   声明对 >10 行 / 整节点替换是必需的；write_file 的描述
   声明仅用于新文件。
2. **条件式 GBNF 语法**（`proxy/tools.go`，
   `proxy/agent.go:stepExclusions`）。当一个 write_file 对
   一个 >5 行的已有 .py/.html/.htm 文件被拒绝时，下一次 LLM 调用会
   被一个 GBNF 语法约束，该语法从工具名产生式中禁掉
   edit_file 和 write_file。模型在物理上无法发出
   它们。该限制在一次决策后失效。
3. **逐步工具列表过滤**（同一触发条件）。注入一条临时的
   `[system note]` 用户消息，提醒模型在这一步
   structural_edit 是唯一的结构性编辑工具。
4. **ASA 操控向量**（`geometric-lens/asa_calibration/`）。
   激活操控在上游移动残差流分布，因此即使在任何拒绝
   触发之前的首次尝试决策中，也会偏好 structural_edit。仅当
   `/models/ast_edit_steering.gguf` 的 `.model` 侧车标记与所选模型
   匹配时，才由 `inference/entrypoint-v3.1.sh` 自动加载 —— 一旦通过
   `geometric-lens/asa_calibration/README.md` 中的工作流构建出兼容的
   向量，它就始终生效。可通过 `ATLAS_CONTROL_VECTOR*` 环境变量
   覆盖路径/缩放/层范围。

   **逐模型耦合。** 每个 ASA 向量都是针对某个特定模型的
   残差流几何结构训练出来的。任何跨模型回退都不安全。
   `atlas asa check` 验证 `.model` 侧车标记，探测已加载的嵌入维度，
   解析 GGUF 层元数据，并报告 `compat` / `needs-build` /
   `incompatible`。`atlas asa build` 从已加载的模型推导提取层，
   写出向量和标记，并运行在 lens 容器内部。`atlas asa publish`
   在上传前会拒绝缺失或不匹配的标记。见 [CLI.md § atlas asa](../../CLI.md#atlas-asa)。

### 逐文件 Tier 分类

每一次 `write_file`/`edit_file` 调用都被独立分类：

| Tier | 最大轮次 | 动作 |
|------|-----------|--------|
| T0（对话型） | 12 | 仅文本回复 |
| T1（简单） | 0（无上限） | 直接写入 —— 无 V3 开销 |
| T2（功能） | 0（无上限） | 触发 V3 pipeline |
| T3（困难） | 0（无上限） | 触发 V3 pipeline |

tier 上限为 0（无上限）；由循环内部的检测器栈决定何时中断：lens 回退（`agent_lens_intervention`）、推理重复（`agent_reasoning_intervention`）、工具调用重复（`agent_repeat_intervention`）、路径感知的错误熔断器、无动作即 done 门控、claim-check 门控、计划遵循阈值，以及空回复回退。对于一次性的"修复整个应用"提示，运维人员可用 `ATLAS_MAX_TURNS=<n>` 覆盖 —— 见 `proxy/types.go::envOverrideMaxTurns`。

分类器在 `proxy/tools.go`（`classifyFileTier`）；逻辑模式匹配器在同一文件中（`hasLogicIndicators`）。

**始终为 T1（直接写入）：**
- 按名称匹配的配置文件（如 `package.json`、`go.mod`、`pyproject.toml`、`dockerfile`、`docker-compose.*`）
- 按扩展名匹配的数据文件（`.json`、`.yaml`、`.yml`、`.toml`、`.csv`、`.xml`、`.env`）
- 样式文件（`.css`、`.scss`、`.less`）
- 文档（`.md`、`.txt`、`.rst`）和 shell 脚本（`.sh`、`.bash`）
- 少于 **10 行**的极小文件（在那种体量下 V3 没有任何可以有意义地多样化的东西）
- 没有逻辑指标的未知扩展名

完整的配置文件列表和扩展名集合位于 `proxy/tools.go:classifyFileTier`。

**T2（V3 pipeline）** —— 当文件 ≥10 行且满足以下任一条件时合格：
- `hasLogicIndicators(content)` 返回 true —— 在覆盖函数/方法定义、控制流、错误处理、Flask/FastAPI/Django 路由、Express/Node API、React 状态/数据、校验、数据库调用、JSX/React 组件模式和导入的模式家族中出现 **2 次以上匹配**（字面 token 列表见 `proxy/tools.go:hasLogicIndicators`）
- 或者该文件具有可识别的源代码 / 标记语言扩展名（`.py`、`.go`、`.rs`、`.ts`、`.tsx`、`.js`、`.jsx`、`.html`、`.htm` 等）且没有触发逻辑指标 —— 在 T2 给予它疑点利益（覆盖诸如 12 行组件骨架这类极简但真实的文件）

**T3（困难）** —— 目前分类器自身从不直接发出 T3；圈复杂度精炼器（`refineTierWithCC`，经由 GH #39 第 2 点的 `/internal/cyclomatic_complexity`）按 McCabe CC *升级*：CC ≥ 8 时升到 T2（包括从 T1 升级），CC ≥ 16 时升到 T3。从不降级。

#### 客户端任务模式

一个请求可以携带 `task_contract.task_mode`，在请求边界处校验（`work` 或 `question`；缺失与"存在但为空"保持可区分）。它做与不做什么：

- **在场即权威**，但仅对一个问题：本请求是否要求一次状态变更。
- **缺席即回退** —— 由旧有的自然语言启发式照常裁决，行为不变。旧客户端被原样接受；不为其推断或虚构任何模式。
- **确立的是义务，而非完成。** `work` 模式说的是动作被要求；运行仍须以既有证据证明它 —— 当前哈希的交付物有效性、适用的验证、无未结算的变更债、无存活的后台危害、无失效校验、无阻塞的墓碑或权限缺陷。
- **不授权任何破坏性操作。** `question` 模式既不允许变更，也不抹除变更债、损坏的交付物、失败的校验、后台危害或删除义务；若模型仍然变更，同样的安全与完成规则管辖那些字节。删除仍需要其自身那绑定路径与身份的确认。
- **从不到达模型。** 任务模式不出现在任何提示中，任何模型、V3 或 lens 输出都无法设置它。一个内部畸形的模式在失败时闭合到要求工作一侧，而不是被读作问题。

影子诊断记录（私有、仅观测、默认关闭 —— 见 [OPERATIONS.md](../../OPERATIONS.md#private-diagnostics-task-contract-shadow-capture)）**按记录种类**版本化，因为给其中一种加字段不得静默重定义另一种：

| 记录 | 版本 | 契约 |
| --- | --- | --- |
| `task_contract_shadow_gate` v1 | 1 | 仅旧有观测：启发式的裁决。任务模式迁移之前采集的封存捕获属于 v1，且始终可被为其编写的分析器读取。 |
| `task_contract_shadow_gate` v2 | 2 | v1 加上 `live_action_demand`（实际生效的裁决）与 `action_demand_source`（`legacy`、`contract_work`、`contract_question`、`contract_invalid_failed_closed`）。 |
| `task_contract_shadow_request` | 1 | 未变 |
| `task_contract_shadow_footer` | 1 | 未变 |

`comparison` 仍描述契约与旧有机制的对比；`influences_live_decision` 仍描述观察者及其 sink 能否影响策略，且保持为 false。一次 schema 变更意味着一个新版本 —— 封闭字段集加一个为它编写的读取器 —— 从不是对既有版本的重定义，也从不是一个对未知字段耸肩放行的解析器。

客户端：TUI 对普通消息发送 `work`，对一次性 `/ask <message>` 发送 `question`；e2e 与可靠性 harness 发送 `work`。VS Code 扩展尚不发送契约，因此其请求不获得 V3 候选（[CANDIDATE_POLICY.md](../../CANDIDATE_POLICY.md)）。`expected_outputs` 与 `verification` 会被携带与校验，但**尚未迁移** —— 交付物与验证义务仍按旧方式推导。

### Plan 模式（按轮次预检）

Plan 模式是一个预检式的规划步骤，在每个 agent 轮次、第一次工具调用之前运行一次：规划器采样候选计划，用启发式打分，并把获胜者渲染进系统提示；当模型偏离计划胡乱折腾时，一个遵循门控会自动修订计划。它减少了探索折腾，并通过守住计划的验证步骤来阻止无证据的 `done`。

完整的流程、组件、可调项、跳过条件、成本和测试矩阵见 [PLAN_MODE.md](../../PLAN_MODE.md)。

### 安全限制

面向运维的限制及其调优旋钮。内部操控守卫（回溯定位、缺失模块/大小写不匹配操控、符号接地、空操作/空内容/语法门控、doctype 剥离）位于 `proxy/guardrails.go` 和 `proxy/agent.go`。

| 限制 | 取值 | 用途 |
|-------|-------|---------|
| 对话裁剪 | 按 slot 调整大小的滑动窗口：保留系统消息 + 最近的用户指令 + 当前活动文件的内容 + 尽可能多的尾部消息以塞满 `per-slot context − ATLAS_MAX_TOKENS − 2048`（下限：保留 8；硬上限通过 `ATLAS_AGENT_HISTORY_BUDGET`） | 在不丢掉正在编辑的文件的前提下防止上下文溢出 |
| 冗余读取短路 | 对一个未改动文件的整文件重读仅在其内容仍然在场时返回"已在上下文中"指针；否则重新提供完整文件（`ATLAS_DEDUP_READS=0` 禁用） | 避免每轮重新编码一个未改动的文件，同时不让模型盲编辑 |
| V3 交互式墙钟上限 | 单次 V3 pipeline 调用被限制在 `ATLAS_V3_TIMEOUT`（默认 180s）；超时时代理回退到模型自身的语法门控内容（`0` 禁用） | 在长时间修复停滞下保持交互式会话的响应 |
| 逐轮推理预算 | 在约 6144 个推理 token 后切断流（`ATLAS_REASONING_BUDGET`，0 禁用）；恢复时从推理中提取一个内嵌的 tool_call 或重新提示 | 限定推理螺旋 |
| 对已有文件的 write_file | 文件 > 5 行时拒绝；在 .py/.html/.htm 上，逐步语法门控操控转向 `structural_edit` | 强制外科式编辑（`edit_file`）或整节点编辑（`structural_edit`） |
| 可疑收缩守卫 | 当 `oldSize >= 100B` 且 `newSize < 64B` 时拒绝 `structural_edit`/`edit_file`（`proxy/guardrails.go::validateNotSuspiciouslyShrunk`） | 在破坏性的桩重写落盘之前抓住它们 |
| structural_edit 失控内容守卫 | 当 `content` > 8 KB 且 > 4× 文件大小时拒绝 | 抓住作为替换节点发出的推理泄漏块 |
| 错误循环熔断器 | 连续 3 次失败 | 停止失控的失败循环 |
| 探索预算 | 连续 4 次只读调用时提示（nudge）；5 次以上时升级提示。读取始终会执行 —— 提示是把*下一*轮引向写入 | 推动模型去写，而不是无限探索 |
| 命令输出截断 | stdout 8,000 字符，stderr 4,000 字符 | 防止上下文泛滥 |
| 搜索结果 | 最多 200 个匹配；文件搜索跳过 > 1 MB 的文件 | 限定搜索成本 |
| 截断检测 | 对工具参数做 JSON 解析检查 | 抓住被截断的模型输出 |

---

## 4. V3 Pipeline（内层）

在 T2+ 文件的 `write_file`/`edit_file` 执行器内部激活。该 pipeline 有四个阶段，且在每个阶段都设有提前退出。

### Pipeline 流程

```mermaid
flowchart LR
    Entry["T2 detected"] --> Probe["Probe"] --> Score1["C(x)/G(x)"] --> SB1["Sandbox"]
    SB1 --> Pass1{"Pass?"}
    Pass1 -->|"Yes"| Done["Done"]

    Pass1 -->|"No"| PS["PlanSearch"] --> DS["DivSampling"] --> BF["BudgetForcing"] --> Build["Build Check"] --> Score2["Score K"] --> SB2["Test K"]

    SB2 --> AnyPass{"Passed?"}
    AnyPass -->|"2+"| SStar["S* Tiebreak"] --> Done
    AnyPass -->|"1"| Select["Lens Select"] --> Done

    AnyPass -->|"0"| FA["Failure Analysis"] --> PRCOT["PR-CoT"]
    PRCOT --> PRPass{"Pass?"}
    PRPass -->|"Yes"| Done
    PRPass -->|"No"| Refine["Refinement"] --> Done

    style Entry fill:#1a3a5c,color:#fff
    style Done fill:#333,color:#fff
    style Probe fill:#1a3a5c,color:#fff
    style PS fill:#1a3a5c,color:#fff
    style DS fill:#1a3a5c,color:#fff
    style BF fill:#1a3a5c,color:#fff
    style SStar fill:#2d5016,color:#fff
    style Select fill:#2d5016,color:#fff
    style Score1 fill:#2d5016,color:#fff
    style Score2 fill:#2d5016,color:#fff
    style SB1 fill:#2d5016,color:#fff
    style SB2 fill:#2d5016,color:#fff
    style Build fill:#2d5016,color:#fff
    style PRCOT fill:#5c3a1a,color:#fff
    style Refine fill:#5c3a1a,color:#fff
    style FA fill:#5c3a1a,color:#fff
```

图例：蓝色 = 生成，绿色 = 验证/选择，棕色 = 修复。

### 各阶段细节

**Phase 0: Probe** 以渐进式预算重试（light → standard → nothink）生成单个基线候选。它用所选模型的 C(x)/G(x) 工件打分，并在 sandbox 中测试。如果通过，pipeline 立即退出。

**候选分配：CxGx 闸门**（以 `phase2` / `phase2_allocated` 发出）决定失败的探测能获得多少个候选。探测的 C(x)+G(x) 组合分数（一次嵌入提取，两个模型都用）驱动一条两步规则：校准后的 C(x) 归一化能量在 Budget Forcing 所用的同一阶梯上选出基础 tier，而 G(x) 质量分数在低于该模型校准的 severe 边界时把这个 tier 抬高 +1，在远低于它（0.75 倍）时抬高 +2 —— 也就是探测在 C(x) 看来便宜、在 G(x) 看来却是错的那种情况。tier 决定 k（`nothink` 1、`standard` 3、`hard` 5、`extreme` 8），并受一条硬性的 **k >= 3 下限**约束，因此闸门只能在原先固定的 k=3 之上增加候选，不能减少；它的最坏情况就是旧行为。两个信号都需要该模型的校准文件（`cx_normalization.json`、`gx_thresholds.json`）：lens 缺失、不可达或未校准时，一律在 `standard` 下分配恰好 k=3，于是未校准的 bundle 会运行它此前运行的那条 pipeline，而不是按一把对它毫无意义的尺子来路由。

这条下限正是它与此前被移除的纯 C(x) 分配器的区别：那一版没有下限，会把 k=1 交给探测*刚刚失败*的任务。由 lens 驱动的分配是否优于固定或随机分配的层级，目前尚未测量。此前的四臂比较（带闸门、固定 k=3、把同样的层级组合在任务间打乱、k=8，每臂 n=175）无法作为任何一方的证据：它在 Qwen3.5-9B 上、使用不在本仓库中的打过补丁的运行器、开启了线上闸门无法使用的升级思考模式、并在训练 G(x) 头所用的 LiveCodeBench 任务上运行，而在该样本量下各臂的差异都在噪声范围之内。

线上路径的差异：代理的 V3 桥接会在 `ATLAS_V3_TIMEOUT`（默认 180s）后放弃一次 pipeline 调用，这是 bench 从未有过的上限；因此无限制地升级到 k=8 会把预算全花在生成上，最终返回超时兜底，而不是时钟本可以产出的 k=3 答案。为此线上编排器会把剩余的实际时间以及在该任务上观测到的单次调用延迟一并传入，闸门则把 tier 降到预算真正能生成的水平 —— 同时保留一次精化迭代，使升级不会饿死 Phase 3 —— 但绝不会低于下限。bench 运行器不传预算，严格按测得的结果分配。实现位于 `v3-service/stages/cxgx_gate.py`，由两个编排器共享。

**Phase 1: 约束驱动的生成**

- **PlanSearch** 通过提取不同的约束集合，生成 3 个结构上不同的实现计划
- **DivSampling** 施加扰动多样性：4 个角色（competitive_programmer、systems_engineer、mathematician、pragmatist）+ 4 条指令（step_by_step、edge_case_first、complexity_aware、constraint_driven）+ 4 种风格（functional、pythonic、optimize_iteratively、structured）
- **Budget Forcing** 控制思考 token 的分配：

| Tier | 思考 token | Wait 注入 |
|------|----------------|----------------|
| nothink | 0 | 模板级禁用思考 |
| light | 1,024 | 无 |
| standard | 2,048 | 若思考结束时 < 512 token |
| hard | 4,096 | 若思考结束时 < 1,024 token |
| extreme | 8,192 | 若思考结束时 < 2,048 token |

Wait 注入会追加 "Wait, let me reconsider.\n" 以请求更长的一轮推理。Tier 选择使用所选模型经过校准的 C(x) 能量；没有校准时，ATLAS 使用配置的默认预算，而不是套用另一个模型的常量。

**Phase 2: 验证与选择**

- **构建验证**：Python（`py_compile`）、TypeScript（`tsc --noEmit`）、JavaScript（`node --check`）、Go（`go build`）、Java（`javac`）、Kotlin（`kotlinc`）、Rust（在 sandbox 的 `/execute` 路径上用 `rustc`；含 `Cargo.toml` 的项目会被识别并使用 `cargo build`，`cargo check` 仅通过构建命令白名单接受）、C/C++（`/execute` 上执行带 `-Wall` 的完整 `gcc`/`g++` 编译；`-fsyntax-only` 只适用于 `/syntax-check` 路由）、Ruby（`ruby -c`，解释型语言，无编译步骤）、PHP（`php -l`，同上）、Shell（`bash -n`）。Next.js、React、Flask、Django、Express 有框架级覆盖。
- **否决（Veto）**：即使候选通过了 sandbox，仍有三项检查可以否决它 —— lens 否决（逐步的 `gx_min` 低于该模型校准后的 severe 阈值：代码能跑，但生成模式已经塌陷成存根）、结构否决（tree-sitter 发现某个直接标识符调用无法解析到任何本地定义、import、内建或项目符号 —— 一个等待发生的 `NameError`），以及由开关控制的调用图否决（`ATLAS_CALL_GRAPH`：跨文件调用且作用域内没有定义）。被否决的候选会被标记为失败（`passed=false`、`vetoed_by`，否决理由作为其错误输出），并像其他失败候选一样进入 Phase 3 的修复池；最终的能量兜底永远不会返回它。如果所有候选都被否决且修复失败，pipeline 不返回代码，由调用方用自己的基线替代
- **Lens 选择**（≥1 个通过）：按 C(x) 能量排序，最低者胜出

**Phase 3: 修复**（若 0/K 通过）—— 三种策略，顺序执行并带提前退出：

- **失败分析**：对失败分类（wrong_algorithm、implementation_bug、edge_case_miss、time_limit、format_error、partial_correct）
- **元认知评估**：从观察到的失败类别推导并注入补偿性约束
- **PR-CoT**：4 个视角（logical_consistency、information_completeness、biases、alternative_solutions）×（分析 + 修复）= 约 8 次 LLM 调用，最多 3 轮
- **Refinement Loop**：失败分析 → 约束精炼 → 代码生成 → 测试 → 学习。2 次迭代，120s 预算，每次约 5+ 次 LLM 调用。余弦距离过滤（>= 0.15）防止假设重复

### 模块图

pipeline 阶段是 `v3-service/stages/` 中的 12 个 Python 模块。`v3-service/pipeline.py` 编排其中 11 个（10 个直接调用，`constraint_refinement` 通过精化循环）；`embedding_store` 只在离线 bench 运行器（`atlas/bench/v3_runner.py`）下运行，该运行器会把 checkout 中的 `v3-service/` 加入自身路径，因此两个调用方共享同一份阶段实现：

```mermaid
graph LR
    Main["pipeline.py"] --> CG["CxGx Gate"]
    Main --> PS["PlanSearch 1A"]
    Main --> DS["DivSampling 1B"]
    Main --> BF["BudgetForcing 1C"]
    Main --> CS["CandidateSelection"]
    Main --> FA["FailureAnalysis 3A"]
    Main --> PRCOT["PR-CoT 3C"]
    Main --> RL["RefinementLoop 3E"]
    Main --> STG["SelfTestGen"]
    Main --> LLM["LLMClient"]
    Bench["v3_runner.py\n(bench only)"] --> ES["EmbeddingStore"]

    RL --> FA
    RL --> CR["ConstraintRefiner 3B"]
    CG -->|"tier table"| BF
    CG -->|"budget helpers"| RL

    style Main fill:#333,color:#fff
    style Bench fill:#333,color:#fff
    style CG fill:#1a3a5c,color:#fff
    style PS fill:#1a3a5c,color:#fff
    style DS fill:#1a3a5c,color:#fff
    style BF fill:#1a3a5c,color:#fff
    style CS fill:#2d5016,color:#fff
    style FA fill:#5c3a1a,color:#fff
    style CR fill:#5c3a1a,color:#fff
    style PRCOT fill:#5c3a1a,color:#fff
    style RL fill:#5c3a1a,color:#fff
    style STG fill:#333,color:#fff
    style LLM fill:#333,color:#fff
    style ES fill:#333,color:#fff
```

图例：蓝色 = Phase 1（生成），绿色 = Phase 2（选择），棕色 = Phase 3（修复），灰色 = 工具。由 `v3_runner.py` 供给的模块仅用于 bench 运行器；服务不会调用它们。服务本身是 `main.py`（HTTP 处理器）→ `pipeline.py`（编排器）→ `planning.py` / `scoring.py` / `symbols.py` / `adapters.py` 这样的扁平同级模块。

---

## 5. Geometric Lens

一个神经打分系统，通过分析模型嵌入的几何结构，在不执行代码的情况下评估代码质量。完全运行在 CPU 上。服务表面仅对内（`/internal/*`）：C(x)/G(x) 打分（单次与逐步）。

#### 为什么叫 "Geometric Lens"？

Geometric Lens 背后的核心理念源自一个简单的前提：停止扩大模型，转而用支撑性的基础设施把它们包裹起来。Jose Crespo 的 ["Everyone's Wrong About AI Programming"](https://www.josecrespophd.org/p/everyones-wrong-about-ai-programming) 论证了 AI 生成的代码会漂向错误，因为当前的 LLM 工作在扁平的嵌入空间中，正确与错误的代码路径代价相同。解决方案是在模型周围构建一个能量景观，让正确的代码处于"下坡"、错误的代码处于"上坡"。

Anthropic 的 [Manipulating Manifolds](https://transformer-circuits.pub/2025/linebreaks/index.html) 研究提供了证据，表明 transformer 已经在其嵌入空间中创造出可操纵的几何结构 —— 原材料早已存在。Bar 等人的 [Geometric Unification of Generative AI](https://arxiv.org/html/2510.00666v1) 形式化了如何在数据流形上学习并使用距离函数来打分。

ATLAS 用两个互补的模型实现这一点。C(x) 是建立在所选模型自身嵌入之上的一个习得的能量函数（`hidden_dim`→512→128→1 的 MLP）。每个代码候选都由 llama-server 嵌入，C(x) 给它在那个几何结构中所处的位置打分。低能量意味着该候选与已知正确的代码聚成一类。高能量意味着它与已知错误的代码聚成一类。无需外部预言机，无需执行 —— 仅仅是所选模型表示的几何结构。

G(x) 是质量预测器 —— 一个建立在 PCA 降维嵌入之上的 XGBoost 分类器，根据候选在降维空间中所处的位置预测通过/失败。当 C(x) 回答"这个候选有多好？"时，G(x) 回答"这个候选可能通过吗？"它是唯一的 G(x) 实现：早先的度量张量形式及其可修正性端点在 XGBoost 成为部署路径后已被移除（几何感知变体见 git 历史）。

### 打分模型

```mermaid
graph LR
    EE["Embedding Extractor\nllama-server /embedding\nmodel hidden dim"] --> CX["C(x) Cost Field\nd→512→128→1\nSiLU + Softplus"]
    EE --> GX["G(x) XGBoost\nPCA(128) + classifier"]
    CX --> SVC["Service Layer\nevaluate_combined()"]
    GX --> SVC
    SVC --> V{"Verdict"}
    V -->|"at/above artifact low"| LC["likely_correct"]
    V -->|"between severe and low"| UN["uncertain"]
    V -->|"below artifact severe"| LI["likely_incorrect"]

    TR["Training Pipeline\ncontrastive ranking loss"] --> CX

    MT["Metric Tensor\ndiagonal G(x) in PCA space\n(code exists, not deployed)"] -.-> CORR["Correction Engine\n-α · G⁻¹ · ∇C"]

    style EE fill:#333,color:#fff
    style CX fill:#2d5016,color:#fff
    style GX fill:#2d5016,color:#fff
    style SVC fill:#333,color:#fff
    style TR fill:#1a3a5c,color:#fff
    style MT fill:#555,color:#ccc
    style CORR fill:#555,color:#ccc
```

以下数字描述的是已发表的 V3 研究所用的冻结参考工件；它们是出处记录，不是运行时的维度或默认值：

| 模型 | 参考架构 | 训练数据 | 性能 |
|-------|-------------|---------------|-------------|
| **C(x)** | 4096→512→128→1 MLP (SiLU, Softplus) | 597 个 LCB 嵌入（504 PASS，93 FAIL） | Val AUC 0.9467，分离度 2.04x |
| **G(x)** | PCA(4096→128) + XGBoost | 13,398 个嵌入（4,835 PASS，8,563 FAIL） | PCA 80.8% 方差 |

C(x) 的归一化是 `sigmoid(steepness × (energy - midpoint))`。所选模型的 `cx_normalization.json` 提供这两个值；`atlas lens build` 会从该模型带标签的 PASS/FAIL 候选中推导它们。G(x) 的判定阈值同样来自 `gx_thresholds.json`。缺少任一校准时，归一化的判定保持中性/未校准状态，而不是借用参考工件的标度。

每个当前的 Lens 工件包还包含 `model_identity.json`。服务要求其中的模型名与 llama-server 的 `/v1/models` 所报告的 served-model id 匹配（该探测失败时以 `ATLAS_MODEL_NAME` 作为回退）；仅凭嵌入宽度相等无法确立两个不同模型之间的兼容性。

**模型侧调用的归因。** Lens 对模型服务器的每次请求（`/embedding`、`/v1/models` 身份探测）都经由同一套传输，`geometric_lens/model_transport.py`，它转发 ATLAS 其余部分已在用的两个关联标头：`X-ATLAS-Request-ID` 与 `X-ATLAS-V3-Invocation-ID`。它们的取值只来自 Lens 中间件为当前请求绑定的身份（每个 ATLAS Python 服务都使用的同一批 ContextVars）；V3 在其打分调用上同时发送二者。没有绑定身份时标头缺席，不完整的一对保持不完整，绑定随请求结束清除，因此并发请求与后台工作无法交换或继承身份。启动与就绪工作（启动自检、一次 `/ready` 重跑）只在 `ATLAS_LENS_STARTUP_REQUEST_ID` 与 `ATLAS_LENS_STARTUP_INVOCATION_ID` 同时设置时携带身份；测试工具导出该对并在其 relay 上注册，普通部署两者皆不设。仅作归因：没有任何打分、选择、授权或完成逻辑读取这些标头，也没有任何候选字节或用户内容进入它们。

**代理的直接 Lens 调用携带自己的调用身份。** 代理在一条模型侧路径上直接与 Lens 通信，逐写打分（`/internal/lens/score-per-step`），它不是一次 V3 候选调用。唯一属主 `proxy/lens_identity.go` 构建这些请求，并把绑定的 `X-ATLAS-Request-ID` 与一个仅由该请求 id 派生的、代理所有的 Lens 调用身份盖在一起：`proxy-lens:` 后跟 `sha256("atlas/proxy-lens-invocation/v1\n" + request_id)` 的前 32 个十六进制位。它是确定性的（relay 可以在任何模型侧流量之前注册该对），同一请求内的每次直接 Lens 调用相同，跨请求且区别于 V3 的 UUID 调用，且只从类型化的请求 id 派生：绝不来自散文、路径、候选字节、工具参数或模型输出，也绝不可由模型设置。它走既有的 `X-ATLAS-V3-Invocation-ID` 通道；标头名是历史遗留，值是通用的模型侧调用身份。它是一个范围标签而非凭证：没有任何东西读取它来授权一次变更、权限、候选或完成，它也从不出现在 SSE 事件、工具结果、提示或日志中。缺席或超出封闭格式（`[A-Za-z0-9._:-]{1,128}`）的请求 id 不派生调用身份。封闭的规范与向量位于 `proxy/testdata/lens_invocation_vectors.json`。

**嵌入容量边界。** llama-server 在单个物理批次（`-ub`，`ATLAS_UBATCH`）中处理一次 `/embedding` 请求，并拒绝更长的输入；Lens 的每次打分都是对整个序列的一次前向。该拒绝是部署的传输限制而非对文本的评判，它与每个分数分开存放：回答以 `scored: false` 连同每个分数字段中的 `null` 与一个类型化的 `failure`（`embed_capacity`，带服务器的 `input_tokens` 与 `capacity_tokens`、`model_server_error`、`model_server_unreachable`、`embedding_contract`、NaN 或无穷值的 `nonfinite_score`、`internal`）。服务路径上不做任何截断或切分，因为切分过的输入不是工件拟合时的那个向量。v3-service 把该失败记在候选上，将其排在每个已打分者之后，只作为最后存留的已验证候选交付；代理对未打分的写入不施加任何阈值。Lens 知道的容量（经 `LLAMA_EMBED_CAPACITY_TOKENS` 声明，或从一次拒绝中观测）报告于 `/health` 与 `/ready`，且当它低于代理的生成上限时，以状态维度中的 `lens_scoring: partial` 呈现。决策记录：[ADR 0010](../../adr/0010-lens-capacity-boundary-is-typed.md)。


> **注意：** 模型权重（.pt、.pkl 文件）未提交到仓库 —— 它们在训练期间构建，并烘焙进容器镜像或在运行时挂载。当模型文件缺失时，服务会优雅降级：C(x) 返回中性能量，G(x) 返回 `gx_score: 0.5` 和 `verdict: "unavailable"`。训练数据与权重可在 [HuggingFace](https://huggingface.co/datasets/itigges22/ATLAS) 获取。

<a id="rag--pageindex-v2"></a><a id="confidence-router--pattern-cache"></a>

> **已移除的子系统。** 早期版本在 lens 内部附带了 RAG/PageIndex 项目索引器、BM25 模式匹配器，以及基于 Thompson 采样的置信度路由器。它们只能通过产品中无人调用的 lens 端点触达，已在 2026-08 的简化行动中移除（见 CHANGELOG）。那套栈中最后残留的模式缓存也已于 2026-09 移除：它保存了每个成功会话（包括评测运行）的解答，并作为“经验”注入之后的每次运行，因而成了从测试集通向产品的通道。

---

## 6. Sandbox

带编译、测试和检查的隔离代码执行。

```mermaid
graph LR
    subgraph executors["Language Executors"]
        Py["Python\npylint (0-10) + pytest"]
        JS["JavaScript\nNode.js 20"]
        TS["TypeScript\ntsc --noEmit + tsx"]
        Go["Go 1.22\ngo build + run"]
        Java["Java 21\njavac + java -cp"]
        Kotlin["Kotlin 2.4.0\nkotlinc + java -jar"]
        Rust["Rust stable\nrustc + run"]
        C["C / C++\ngcc/g++ -Wall"]
        Ruby["Ruby\nruby -c + run"]
        PHP["PHP\nphp -l + run"]
        Bash["Bash\nbash -n + run"]
    end

    subgraph support["Support"]
        Syn["Syntax Checker\nper-language AST validation"]
        Err["Error Classifier\n15 types: SyntaxError, NameError\nTypeError, CompileError, Timeout..."]
        Trunc["Output Truncation\nstdout: 4000 chars\nstderr: 2000 chars"]
    end

    style executors fill:#2d5016,color:#fff
    style support fill:#333,color:#fff
```

接受的语言别名：`py`/`python3`（Python）、`js`/`node`（JavaScript）、`ts`（TypeScript）、`golang`（Go）、`java`（Java）、`kt`/`kts`（Kotlin）、`rs`（Rust）、`c++`（C++）、`rb`（Ruby）、`php`（PHP）、`sh`/`shell`（Bash）。常用 CLI 工具已内置在镜像中（`git`、`sqlite3`、`jq`、`patch`、`zip`/`unzip`、`xz`、`curl`），另外还有二进制检查工具（来自 binutils 的 `strings`、`objdump`、`readelf`、`nm`，以及 `file`、`xxd`）—— 容器以非 root 身份运行在只读基础镜像上，因此任务要 shell 调用的一切都必须预装，运行时无法用 apt 安装。对二进制文件调用 `read_file` 会返回指向这些工具的提示，而不是原始字节。最大执行时间：Docker 部署中为 300s（compose 设置 `MAX_EXECUTION_TIME=${ATLAS_SANDBOX_MAX_EXECUTION_TIME:-300}` 以匹配代理 5 分钟的 `run_command` 上限；裸代码默认值为 60s）。内存、CPU 和进程数上限是容器级的：compose 设置 `mem_limit ${ATLAS_SANDBOX_MEM:-4g}`、`cpus ${ATLAS_SANDBOX_CPUS:-2}` 和 `pids_limit ${ATLAS_SANDBOX_PIDS:-1024}`；`atlas init` 会把与主机相称的取值（约为 RAM 和核心数的 75%）写入 `.env`。两个工作区路径：**`/execute`**（V3 候选测试路径）使用 `/tmp/sandbox`（tmpfs）下的一个临时草稿目录；**`/shell`**（agent 的 `run_command` 路由，外加用于后台进程的 `/jobs/*`）针对 `/workspace` 运行 —— 即来自 `ATLAS_PROJECT_DIR`（Docker）或 hostPath `${ATLAS_PROJECTS_DIR}`（K3s）绑定挂载的项目根，与代理看到的是同一路径。

---

## 7. VRAM 预算示例

一次实测的 RTX 5060 Ti 16GB 部署，使用 9B Q6 模型和 32K 上下文：

| 组件 | VRAM |
|-----------|------|
| Qwen3.5-9B-Q6_K 模型权重 | ~6.9 GB |
| KV 缓存（32K 上下文） | ~1.3 GB |
| **llama-server 合计** | **~8.2 GB** |
| Geometric Lens | 0（仅 CPU，模型约 12 MB RAM，PyTorch 运行时约 128 MB） |
| v3-service | 0（仅 CPU） |
| sandbox | 0（仅 CPU） |
| atlas-proxy | 0（Go 二进制，约 30 MB RAM） |
| **空闲 VRAM** | **~7.8 GB** |

llama-server 之外的所有计算都跑在 CPU 上。GPU 仅用于 LLM 推理和嵌入提取。

### 7.1 逐后端的 VRAM 预算

上面 8.2 GB / 7.8 GB 空闲的拆分是一个示例，不是 ATLAS 的模型默认值。实际用量取决于 `atlas init` 所选的模型、量化、上下文和并行 slot 设置。其他后端在结构上有所不同：

| 后端 | 报告的 "VRAM" | 负载下的现实预算 | 备注 |
|---|---|---|---|
| **CUDA**（专用 VRAM） | 硬件规格（基准 5060 Ti 上为 16 GB） | 约规格的 95%（驱动保留约 500 MB） | 上表中的数字直接适用。 |
| **ROCm**（专用 VRAM） | 硬件规格 | 约规格的 90–95%（HIP 运行时比 CUDA 的略重） | RX 7900 XTX (24 GB) → 可以从容运行 14B Q5 + 32K 上下文，带 2 个并行 slot。 |
| **Metal**（Apple 统一内存） | 系统总 RAM | 系统 RAM 的 **约 70%** | 操作系统 + 浏览器 + IDE 吃掉约 30%。一台 16 GB 的 MBP 有约 11 GB 的*现实*预算 —— 一旦 macOS 自身的 GPU 工作集也占用同一块内存，留给 Qwen3.5-9B Q6_K（约 6.9 GB 权重 + 32K 时约 1.3 GB KV，见 §7）的余量就很少。≤16 GB 用 Q4_K_M（5 GB）；Q6_K 想要 ≥24 GB 统一内存。 |
| **Vulkan**（跨厂商） | 硬件规格 | 尚无实测部署（预览 (Preview) —— 仅在 lavapipe CPU 路径上验证过） | 预计比同一张卡上调优过的原生后端低约 20–40%。 |
| **SYCL**（Intel Arc） | 硬件规格 | 路线图 (Roadmap) —— Intel Arc 目前走 Vulkan | A770 (16 GB) 目标在保守意义上等价于 NVIDIA 16 GB。 |

---

## 8. 部署

服务依赖图（各部署模式完全一致）：

```mermaid
graph LR
    LLM["llama-server"] -->|"healthy"| GL["geometric-lens"] -->|"healthy"| AP["atlas-proxy"]
    LLM -->|"healthy"| V3["v3-service"] -->|"healthy"| AP
    GL -->|"healthy"| V3
    SB["sandbox"] -->|"healthy"| AP

    style LLM fill:#5c1a1a,color:#fff
    style GL fill:#2d5016,color:#fff
    style V3 fill:#2d5016,color:#fff
    style SB fill:#2d5016,color:#fff
    style AP fill:#1a3a5c,color:#fff
```

`llama-server` 和 `sandbox` 独立启动。`geometric-lens` 等待 `llama-server` 变为健康；`v3-service` 等待 `llama-server` 和 `geometric-lens`；`atlas-proxy` 等待 `llama-server`、`geometric-lens`、`v3-service` 和 `sandbox`。同一个 `inference/entrypoint-v3.1.sh` 驱动 Docker Compose、裸机和 K3s，因此上下文大小、KV 缓存量化、flash attention 和 mlock 都由环境变量控制，行为在这些模式之间完全一致；macOS 混合路径通过 `scripts/atlas-llama-macos.sh` 启动原生 llama-server，该脚本复刻了入口点的各项标志。

安装及各模式的拉起步骤（NVIDIA / ROCm override、裸机、macOS 混合 Metal、K3s 清单）见 [SETUP.md](../../SETUP.md)；macOS 原生路径见 [SETUP_MACOS.md](../../SETUP_MACOS.md)。

---

## 9. 数据流

### T1：简单文件写入

```mermaid
sequenceDiagram
    participant U as User
    participant A as Client
    participant P as atlas-proxy :8090
    participant L as llama-server :8080

    U->>A: "Create a config file"
    A->>P: POST /v1/agent (SSE)
    P->>L: POST /v1/chat/completions<br/>response_format: json_object
    L-->>P: {"type":"tool_call","name":"write_file","args":{...}}
    Note over P: Tier = T1 (config file)<br/>Direct write, no V3
    P-->>P: Write file to disk
    P-->>A: SSE stream: file content
    A-->>U: File created
```

一次 LLM 调用。无 V3 开销。

### T2：功能文件写入

```mermaid
sequenceDiagram
    participant U as User
    participant A as Client
    participant P as atlas-proxy :8090
    participant L as llama-server :8080
    participant V as v3-service :8070
    participant G as geometric-lens :8099
    participant S as sandbox :30820

    U->>A: "Create a REST API handler"
    A->>P: POST /v1/agent (SSE)
    P->>L: POST /v1/chat/completions<br/>response_format: json_object
    L-->>P: {"type":"tool_call","name":"write_file","args":{...}}
    Note over P: Tier = T2 (≥10 lines, logic)<br/>Route to V3

    P->>V: POST /v3/generate (SSE)
    Note over V: Phase 0: Probe
    V->>L: POST /v1/chat/completions (generate code)
    L-->>V: probe candidate
    V->>L: POST /v1/embeddings (model hidden dim)
    L-->>V: embedding vector
    V->>G: POST /internal/lens/gx-score
    G-->>V: {cx_energy, gx_score, verdict}
    V->>S: POST /execute (test probe)
    S-->>V: {success: false}

    Note over V: Phase 1: PlanSearch + DivSampling
    V->>L: POST /v1/chat/completions (x K candidates)
    L-->>V: K candidates
    V->>S: POST /execute (test each)
    S-->>V: {success: true} for candidate 2

    Note over V: Phase 2: Lens select winner
    V->>G: POST /internal/lens/gx-score
    G-->>V: scores

    V-->>P: SSE result: winning code
    P-->>P: Write file to disk
    P-->>A: SSE stream: file content
    A-->>U: File created
```

算法类任务最少 3 次 llama-server 调用（1 次 probe 生成 + 1 次自测生成 + 1 次嵌入提取）；交互式任务（游戏、UI、框架代码）跳过自测生成，因此其最少为 2 次。如果 Phase 3 修复启用了所有策略，最多 30+ 次。

### 编辑已有代码

```mermaid
sequenceDiagram
    participant U as User
    participant A as Client
    participant P as atlas-proxy :8090
    participant L as llama-server :8080

    U->>A: "Fix the bug in auth.py"
    A->>P: POST /v1/agent (SSE)
    P->>L: POST /v1/chat/completions<br/>response_format: json_object
    L-->>P: {"type":"tool_call","name":"read_file","args":{"path":"auth.py"}}
    P-->>P: Read file from disk
    P->>L: POST /v1/chat/completions (with file content)
    L-->>P: {"type":"tool_call","name":"edit_file","args":{"old_str":"...","new_str":"..."}}
    P-->>P: Apply old_str→new_str replacement
    P->>L: POST /v1/chat/completions (with edit result)
    L-->>P: {"type":"done","summary":"Fixed auth bug"}
    P-->>A: SSE stream: edited content
    A-->>U: File updated
```

超过 5 行的已有文件对 `write_file` 会被拒绝 —— 模型必须使用 `edit_file`（外科式，≤10 行）或 `structural_edit`（整节点重写，仅 .py/.html/.htm）。在 `.py`/`.html`/`.htm` 文件上，逐步语法门控（BiasBusters #2）会在下一次决策中主动从工具名产生式里禁掉 `edit_file`/`write_file`，使模型无法退回到错误的捷径。
