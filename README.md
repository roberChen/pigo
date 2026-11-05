# pigo

[![CI](https://github.com/smallnest/pigo/actions/workflows/ci.yml/badge.svg)](https://github.com/smallnest/pigo/actions/workflows/ci.yml)
[![Release](https://github.com/smallnest/pigo/actions/workflows/release.yml/badge.svg)](https://github.com/smallnest/pigo/actions/workflows/release.yml)

使用 Go 复刻的 [pi](https://pi.dev) AI Agent —— 一个面向命令行的编码智能体，同时支持**无头（headless）脚本模式**与**交互式 REPL**。

pigo 可以读写文件、执行命令、检索代码、抓取网页，并借助大模型完成从"读懂需求"到"改好代码"的闭环。它兼容 OpenAI / Anthropic 等多种协议网关，支持会话续跑、项目信任、技能（Skills）、插件与包管理。

> 模块路径：`github.com/smallnest/pigo` · Go 1.27+

> 📖 配套电子书《用 Go 编写 pi Agent》：[write_pi_agent_in_go.pdf](https://github.com/smallnest/ebooks/blob/master/write_pi_agent_in_go.pdf)

![](docs/pigo-tui.png)

---

## 目录

- [特性一览](#特性一览)
- [架构总览](#架构总览)
- [安装与构建](#安装与构建)
- [快速开始](#快速开始)
- [命令行参数](#命令行参数)
- [模型与 Provider](#模型与-provider)
- [内置工具](#内置工具)
- [运行模式](#运行模式)
- [作为 SDK 嵌入](#作为-sdk-嵌入)
- [系统提示词组装](#系统提示词组装)
- [项目信任](#项目信任)
- [pigorc：用 shell 脚本配置](#pigorc用-shell-脚本配置)
- [自定义 sub-agent（Agent 定义）](#自定义-sub-agentagent-定义)
- [MCP（Model Context Protocol）](#mcpmodel-context-protocol)
- [配置热加载](#配置热加载reload)
- [技能 Skills](#技能-skills)
- [提示词模板](#提示词模板)
- [插件](#插件)
- [Hooks](#hooks)
- [包管理](#包管理)
- [自更新](#自更新)
- [发布release](#发布release)
- [目录与环境变量](#目录与环境变量)
- [安全说明](#安全说明)

---

## 特性一览

- **两种模式**：无头 `-p` 一次性执行（适合脚本 / CI），或直接进入交互式 REPL。
- **多 Provider**：OpenRouter（默认）、本地 Ollama、NVIDIA NIM、Anthropic、任意 OpenAI 兼容端点。
- **内置工具集**：`read` / `write` / `edit` / `grep` / `find` / `bash`（支持 `run_in_background` 后台执行，配套 `bash_output` / `kill_bash` / `bash_jobs`）/ `todo` / `question`（toolcall 提问）/ `webfetch` / `pigo_config`（回显当前生效配置）。
- **会话续跑**：`--list-sessions` / `--resume` / `--continue`，无头与 REPL 均可续跑。
- **stream-json 输出**：逐行 JSON 事件，首个事件携带 `session_id`，便于调用方关联。
- **系统提示词分层组装**：base 指令 + 环境块 + `AGENTS.md`（general→specific）+ `--append-system-prompt`。
- **项目信任**：副作用工具（bash/write/edit）在未信任目录需确认，`--approve` 一次性授权。
- **工具级准入**：`--allowed-tools` / `--disallowed-tools` 划定工具边界（黑名单优先、子 Agent 继承、`--approve` 不可绕过）。
- **技能与插件**：`~/.agents/skills` 的技能（`/skill` 选择、可多选注入下一轮）、`~/.pigo/plugins` 下的外部插件。
- **提示词模板**：`~/.pigo/prompts`、项目 `.pigo/prompts`（受信任时）、config `prompts`、`--prompt-template` 下的可复用 `/name` 模板，支持 `$1`/`$@`/`${1:-default}`/`${@:N}` 等参数语法。
- **上下文自动压缩**：接近上下文窗口上限时自动摘要，亦可 `/compact` 手动触发。
- **包管理**：`pigo install npm:<pkg>` 安装 pi 生态的 extension / skill / prompt / theme。
- **自更新**：无参 `pigo update` 将 pigo 二进制升级到最新 GitHub Release；进入 TUI 时后台检查新版本并在横幅提示。

---

## 架构总览

pigo 的运行时分层架构：请求路径从用户经 CLI、Agent 循环、Provider 层直达 LLM 网关；工具路径从循环经工具执行器与信任闸门抵达本地环境；辅以会话存储与上下文压缩，并标注信任边界与外部网络边界。

![pigo 架构总览](book/images/fig1-1.svg)

> 更多分层图解（事件骨架、统一 Provider、工具批量执行、子 Agent 委派等）见配套电子书。

### Agent 两层循环

运行时的核心是 `internal/runtime/loop.go` 的两层循环：**内层** turn 循环反复「流式回复 → 停止原因分派 → 执行工具 → 回填」，直到某次助手消息不再发起工具调用；**外层**在内层收敛后消费 `GetFollowUpMessages`，有后续消息则重跑内层，否则结束。所有终止路径（自然结束 / error / aborted / 停止钩子 / 无后续消息）都汇于唯一出口 `finish()`。

![pigo Agent 两层循环](book/images/agent-loop-flowchart.svg)

> 交互式版本（含摘要卡片）见 [`docs/agent-loop-flowchart.html`](docs/agent-loop-flowchart.html)。

---

## 安装与构建

需要 Go 1.27 或更高版本。

```bash
# 克隆仓库
git clone https://github.com/smallnest/pigo.git
cd pigo

# 构建二进制（生成 ./pigo）
go build ./cmd/pigo

# 或安装到 $GOPATH/bin
go install ./cmd/pigo

# 也可以不构建，直接运行
go run ./cmd/pigo -p "1+1=?"
```

构建后可查看版本信息（版本号在正式发布时由 goreleaser 注入，源码构建显示 `dev`）：

```bash
pigo --version
# pigo dev (commit none, built unknown)
```

### 一键安装脚本（Linux / macOS）

`install.sh` 会自动检测操作系统 / 架构，从 GitHub Releases 下载最新的预编译二进制并安装到常用的 PATH 目录：

```bash
curl -fsSL https://raw.githubusercontent.com/smallnest/pigo/master/install.sh | sh
```

可用环境变量覆盖默认行为：

| 变量 | 说明 |
|------|------|
| `PIGO_VERSION` | 指定安装版本（形如 `v0.2.0`），默认取最新 release |
| `PIGO_INSTALL_DIR` | 安装目录，默认 `/usr/local/bin`（无写权限时回退到 `~/.local/bin`） |
| `GITHUB_TOKEN` | 可选，用于提高 GitHub API 速率限制 |

```bash
# 指定版本与安装目录
PIGO_VERSION=v0.2.0 PIGO_INSTALL_DIR="$HOME/bin" \
  curl -fsSL https://raw.githubusercontent.com/smallnest/pigo/master/install.sh | sh
```

> Windows 请从 Releases 页面下载 `.zip` 手动解压。

### 下载预编译二进制

[Releases](https://github.com/smallnest/pigo/releases) 页面提供 Linux / macOS / Windows 的 amd64 与 arm64 预编译包（由 goreleaser 构建）。下载对应平台的压缩包解压即可使用。

---

## 快速开始

```bash
# 1. 配置默认 Provider（OpenRouter）的 API Key
export OPENROUTER_API_KEY=sk-or-...

# 2. 无头模式跑一个 prompt，打印最终回答
pigo -p "读取 README 并用三句话总结"

# 3. 进入交互式 REPL（不带 -p 且 stdout 是终端时自动进入）
pigo

# 4. 用本地 Ollama 模型，无需联网
pigo -m ollama/qwen2.5-coder -u http://localhost:11434/v1 -p "解释 main.go 做了什么"
```

---

## 命令行参数

| 长参数 | 短参数 | 默认值 | 说明 |
|--------|--------|--------|------|
| `--print` | `-p` | `""` | 无头打印模式的 prompt（也可用位置参数传入） |
| `--model` | `-m` | `openrouter/free` | 使用的模型 id |
| `--base-url` | `-u` | `""` | 覆盖 Provider 的 base URL（如本地 Ollama） |
| `--api-key` | `-k` | `""` | 指定 Provider 的 API Key（覆盖 env/config，否则读 `<PROVIDER>_API_KEY`） |
| `--protocol` | `-P` | `""` | 强制线路协议：`openai`（= `openai/chat`）\| `openai/resp_api` \| `anthropic`（默认由 model id 推断） |
| `--output-format` | `-o` | `text` | 输出格式：`text` \| `stream-json` |
| `--no-tools` | `-n` | `false` | 禁用内置文件/shell 工具（同时跳过插件发现） |
| `--allowed-tools` | | `nil` | 工具白名单：只把这些工具交给模型；可重复、可逗号分隔、大小写不敏感；为空表示不限制 |
| `--disallowed-tools` | | `nil` | 工具黑名单：从模型的工具集中移除这些工具；同名冲突时**优先于** `--allowed-tools` |
| `--list-sessions` | `-l` | `false` | 列出已存储的会话并退出 |
| `--resume` | `-r` | `""` | 续跑指定 id 的会话 |
| `--continue` | `-c` | `false` | 续跑最近一次的会话 |
| `--approve` | `-a` | `false` | 为本次运行信任工作目录：跳过首次信任提示，副作用工具免逐次确认 |
| `--no-skills` | | `false` | 禁用技能发现（不加载 `~/.agents/skills`：`/skill` 无技能可选，模型也不再自动调用） |
| `--no-prompt-templates` | | `false` | 禁用提示词模板发现（不加载 `~/.pigo/{commands,prompts}`、`.pigo/prompts`、config `prompts`、`--prompt-template`）；内置斜杠命令不受影响 |
| `--prompt-template` | | `nil` | 从文件或目录（非递归）加载提示词模板；可重复（对标 pi `--prompt-template`） |
| `--image` | | `nil` | 把本地图片附加到首轮提问（视觉模型用）；可重复。`-p` 下随提问一起发送，交互模式下预填到输入框让你补问题。启动时校验存在性/大小/是否为图片 |
| `--system-prompt` | | `""` | 用自定义系统提示词替换默认的 coding-assistant 提示词 |
| `--append-system-prompt` | | `nil` | 向系统提示词末尾追加文本或文件内容；可重复 |
| `--version` | `-v` | `false` | 打印版本信息并退出 |

> `--subagent-rpc` 为内部参数（进程隔离子 Agent 的 JSON-RPC 服务端），不用于直接调用。

**使用例子：**

```bash
# 位置参数等价于 -p
pigo "把 utils.go 里的 getUserName 重命名为 getUsername"

# 指定模型
pigo -m anthropic/claude-3.5-sonnet -p "审查 foo.go 的并发安全性"

# 自定义系统提示词（替换默认）
pigo --system-prompt "你是一个只用中文回答的 Go 专家" -p "什么是 goroutine 泄漏"

# 追加系统提示词：可多次，值为文件路径则读取文件内容，否则作字面文本
pigo --append-system-prompt ./CONVENTIONS.md \
     --append-system-prompt "回答尽量简洁" \
     -p "为这个包补充单元测试"

# 一次性授权工作目录，让 bash/write/edit 免逐次确认
pigo -a -p "运行 go test ./... 并修复失败的用例"
```

---

## 模型与 Provider

模型 id 通过启发式规则映射到具体 Provider（`--protocol` 显式指定时优先级最高）：

1. **`--protocol`** 显式选择 → `openai`（Chat Completions，需配合 `--base-url`）、`openai/resp_api`（Responses API，需配合 `--base-url`）或 `anthropic`（默认公有 Anthropic API）。`openai` 与 `openai/chat` 等价；Responses 变体用于只实现 `POST {base_url}/responses` 的端点。
2. **裸 Provider 名** → 视为该 Provider 的默认模型（取预置目录中该 Provider 的第一个预置 id，如 `zai` → `glm-4.7`、`deepseek` → `deepseek-v4-flash`）。`/model zai`、`pigo --model zai`、config.toml 的 `model = "zai"` 三种写法等价。
3. **预置目录命中** → 使用预置声明的 Provider（REPL 中可用 `/models` 查看、`/model <id>` 切换）。
4. **`ollama/` 前缀** 或 base URL 含 `11434` → 本地 Ollama。
5. **`nvidia/` 前缀** → NVIDIA NIM。
6. **按模型名推断** → 未设 `--provider`/`--protocol`/`--base-url` 时，从模型名的知名前缀推断其第一方内置 Provider（如 `-m claude-opus-4-8` 直连 Anthropic，无需再写 `--provider`）。
7. **其余** → OpenRouter（默认）。例外：裸 id 恰是内置 Provider 名但该 Provider 没有预置模型（如特殊认证网关）时，直接报错说明原因，而不是静默发往 OpenRouter。

> **优先级**：显式 flag（`--provider` > `--protocol`）> 预置目录 > `ollama/`/`nvidia/` 前缀 > 模型名推断 > OpenRouter 默认。显式 `--provider` 始终胜出；给了 `--base-url` 会被视为自定义端点信号，跳过第 6 步推断。

**凭证引用**：config.toml 可以只写凭据的**名字**而非明文——`credential = "deepseek-main"`，真实 key 存放在 `~/.pigo/.credentials.yaml`（权限应为 0600）：

```yaml
deepseek-main: sk-xxx
```

解析优先级：`--api-key` > config `api_key` > config `credential` 引用 > 环境变量。子进程（进程隔离子 Agent）默认继承**已清洗**的环境：凭证形态的变量（`*_API_KEY`/`*_TOKEN`/`*_SECRET` 等）与 `PIGO_*` 内部变量不会传给子进程。

**默认模型**：在 `~/.config/pigo/config.toml` 写 `model = "zai"`（裸 Provider 名，取其默认模型）或具体 id 如 `model = "glm-4.7"`，启动即生效；命令行 `--model` 仍可临时覆盖。对应 Provider 的 API Key 环境变量需提前设好（如智谱 `ZAI_API_KEY`、DeepSeek `DEEPSEEK_API_KEY`）。缺 Key 的报错会指明应设置的环境变量名。

**按模型名推断的前缀对照**（仅推断能唯一确定 Provider 的前缀；`llama-*`、`qwq-*`、`gemma-*`、`mixtral-*` 等被多家网关服务的家族，以及形如 `provider/model` 的 routed id，不推断，回落到 OpenRouter 默认）：

| 模型名前缀 | 推断的 Provider |
|-----------|-----------------|
| `claude-*` / `fable-*` | anthropic |
| `gpt-*` / `o1-*` / `o3-*` / `o4-*` | openai |
| `gemini-*` | google |
| `deepseek-*` | deepseek |
| `glm-*` | zai |
| `kimi-*` / `moonshot-*` | moonshotai |
| `qwen-*` | dashscope |
| `ernie-*` | qianfan |
| `doubao-*` | volcengine |
| `grok-*` | xai |
| `mistral-*` / `codestral-*` / `devstral-*` | mistral |
| `hunyuan-*` | hunyuan |
| `minimax-*` | minimax |
| `mimo-*` | xiaomi |

匹配大小写不敏感。推断命中后走与显式 `--provider` 相同的解析路径，使用该 Provider 的默认 base URL、协议与 `<PROVIDER>_API_KEY` 环境变量。


| Provider | 线路格式 | 默认 base URL | API Key 环境变量 |
|----------|----------|---------------|------------------|
| OpenRouter（默认） | OpenAI Chat Completions | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` |
| Ollama（本地） | OpenAI 兼容 | `http://localhost:11434/v1` | 无需（本地） |
| NVIDIA NIM | OpenAI 兼容 | `https://integrate.api.nvidia.com/v1` | `NVIDIA_API_KEY` / `NVIDIA_NIM_API_KEY` |
| OpenAI 兼容 | OpenAI Chat Completions | 需自行提供 `--base-url` | `OPENAI_API_KEY` |
| Anthropic | Anthropic Messages | `https://api.anthropic.com/v1` | `ANTHROPIC_API_KEY` / `CLAUDE_API_KEY` |

Key 解析顺序：OAuth token → `--api-key` → 环境变量 → 配置文件。其他 Provider（google/deepseek/xai/groq/mistral 等）遵循 `<PROVIDER>_API_KEY` 约定。

**自定义第三方 Provider**：内置目录之外的第三方网关可以定义成"和内置一样"的 Provider，之后 `--provider <name>`、`--model <id>` 推断、`/model` 切换、预设列表都能用。两种写法等价：

```toml
# ~/.config/pigo/config.toml
[[providers]]
name     = "mycorp"                        # 名字：小写字母/数字/.-_
protocol = "openai"                        # openai | openai/resp_api | anthropic
base_url = "https://llm.mycorp.com/v1"
env_var  = "MYCORP_API_KEY"                # 可选，默认 <NAME>_API_KEY
credential = "mycorp-work"                 # 可选，具名凭据（推荐）
# api_key = "sk-..."                       # 可选，明文（不推荐，会留在配置文件里）
auth     = "bearer"                        # 可选，默认 openai=bearer / anthropic=x-api-key
models   = ["mycorp-large", "mycorp-small"] # 第一个是默认模型
```

```bash
# ~/.pigo/pigorc
provider add mycorp --type openai --base-url "https://llm.mycorp.com/v1" \
  --env-var MYCORP_API_KEY --model mycorp-large --model mycorp-small
# provider remove mycorp     # 取消（例如临时屏蔽 config.toml 里的定义）
```

声明后可以这样用：

```bash
export MYCORP_API_KEY=sk-...          # 或写进 ~/.pigo/.credentials.yaml
pigo --provider mycorp --model mycorp-large -p "..."   # 显式指定
pigo --model mycorp-large -p "..."                     # 由 model id 推断到该 Provider
pigo --model mycorp -p "..."                           # 裸名字 → 第一个 models
```

规则与内置一致：`<PROVIDER>_BASE_URL` 可覆盖 base_url；Key 解析顺序为 `--api-key` → 环境变量（含 `env_var`）→ 定义内的 `api_key`/`credential`；名字不能与内置 Provider 重名（要把内置指向别的端点请用 `ZAI_BASE_URL` 这类变量）。定义非法（缺 base_url、未知协议、重名）在启动时告警并被忽略，不影响其他配置。

**自定义模型（含 vision 声明）**：模型与 Provider 是**分开声明**的——`[[providers]]` 描述端点（协议、URL、凭据），`[[models]]` 描述单个 model id 及其能力。这样一个网关可以同时提供 vision 模型和纯文本模型：

```toml
# ~/.config/pigo/config.toml
[[models]]
id              = "mycorp-vl"
provider        = "mycorp"                # 必须已定义（[[providers]] 或内置）
display_name    = "MyCorp Vision"         # 可选，默认用 id
supports_images = true                    # 声明为 vision 模型

[[models]]
id              = "mycorp-text"
provider        = "mycorp"
supports_images = false                   # 在支持图像的大 Provider 里钉住一个纯文本模型
```

```bash
# ~/.pigo/pigorc，等价写法
model add mycorp-vl --provider mycorp --supports-images --display-name "MyCorp Vision"
model add mycorp-text --provider mycorp --no-supports-images
model remove mycorp-vl                     # 取消
```

`supports_images` 正是 `view_image` 工具（以及 `read` 读图片文件时）的拦截依据：为 `true` 时把图片交给模型；为 `false` 时不发送图片，而是把"图片存在但未展示"的说明文字返回给模型（避免整轮请求被网关拒绝）。省略该键则继承 Provider 的 `supports_images`：自定义端点默认 `false`，内置 Provider 默认 `true`。模型级声明优先于 Provider 级声明，因此也能为**内置 Provider** 声明新增的 vision 模型，无需等 pigo 更新预置目录。指向不存在的 Provider 的 `[[models]]` 会在启动时告警并跳过。

**使用例子：**

```bash
# 默认 OpenRouter
export OPENROUTER_API_KEY=sk-or-...
pigo -p "写一个快排"

# 任意 OpenAI 兼容端点，强制 openai 协议
pigo -P openai -u https://my-gateway.example.com/v1 -m my-model -k $MY_KEY -p "..."

# 公有 Anthropic API
export ANTHROPIC_API_KEY=sk-ant-...
pigo -P anthropic -m claude-3-5-sonnet-20241022 -p "..."
```

---

## 内置工具

工具集根植于当前工作目录，`--no-tools` 可整体禁用，`--allowed-tools` / `--disallowed-tools` 可做工具级准入（见下方[工具级准入](#工具级准入)）。

| 工具 | 说明 |
|------|------|
| `read` | 按路径读取文本文件，支持行 offset/limit，输出带行号，超大文件截断；图片文件自动转为图片内容（视觉模型直接看，纯文本模型退回尺寸描述） |
| `view_image` | 读取本地图片作为多模态内容返回给模型（视觉模型可看，纯文本模型只得到路径/MIME/字节数描述）；支持 PNG/JPEG/GIF/WebP 等，受大小上限约束 |
| `write` | 创建或覆盖文件，按需创建父目录 |
| `edit` | 精确字符串替换（`old_string` 需唯一，除非 `replace_all`），返回 diff |
| `grep` | 正则检索文件内容，支持 glob 过滤，跳过 `.gitignore` 路径 |
| `find` | 按文件名 glob 查找文件，跳过 `.gitignore` 路径 |
| `bash` | 执行 shell 命令，流式 stdout/stderr；`run_in_background` 直接创建后台任务；前台命令超时（默认 2 分钟）后自动转入后台，结果里会明确提示"超时转后台"并给出 `bash_id` 与后续 `bash_output` / `kill_bash` 调用示例；Ctrl-C 会终止前台命令的整个进程组 |
| `bash_output` | 读取后台 bash 任务的增量输出；`wait_ms` 可短暂等待新输出，`wait_for_exit` 等到任务结束（默认最多 30s） |
| `kill_bash` | 终止后台 bash 任务（连同其子进程） |
| `bash_jobs` | 列出本会话的后台 bash 任务：`bash_id`、运行中/已退出、耗时、退出码、未读输出字节数与命令行；`running_only` 只看仍在运行的。用于找回已滑出上下文的 `bash_id` 后再 `bash_output` / `kill_bash` |
| `todo` | 记录/更新结构化任务清单，每次提交整份列表（pending/in_progress/completed） |
| `question` | 通过 toolcall 向用户提问并等待回答（可带若干候选选项，用户也可自由作答）。TUI 在输入框上方弹出提问（数字键选选项、直接输入后回车作答、Esc 取消本轮），REPL 在终端打印问题并读取一行；无交互输入的场景（`-p` 无头运行、子 Agent）会返回可执行的错误结果，提示模型按合理假设继续 |
| `webfetch` | 抓取 URL 并转为精简 Markdown 正文，HTTP 自动升级 HTTPS |
| `websearch` | 联网搜索并返回标题/URL/摘要，按凭证自动选后端（`TAVILY_API_KEY`→Tavily，`BRAVE_API_KEY`→Brave，否则回落无 key 的 DuckDuckGo），支持 `allowed_domains`/`blocked_domains` 过滤 |
| `memory_search` | 检索持久化记忆（`memory.enabled = false` 或 `--no-tools` 时不注册） |
| `pigo_config` | 回显当前运行的生效配置：实际读取的配置文件（config.toml、全局/项目 config.json、pigorc、skills/plugins/memory 目录）、model/provider/endpoint、thinking 与输出格式、启用的工具及准入边界、skills、MCP 服务器（配置项 vs 实际连接）、hooks、自定义 sub-agent、memory/checkpoint/compaction。可用 `{"section": "hooks"}` 只看一节；只读，`--no-tools` 时不注册 |
| `task` | 派发通用子 Agent，子 Agent 继承父级工具边界且不能再次派发 |

> `bash` / `write` / `edit` 属于"副作用工具"，在未信任目录下需确认（见[项目信任](#项目信任)）。

### 工具级准入

`--no-tools` 是全有或全无，`--allowed-tools` / `--disallowed-tools` 提供中间态：

```bash
# 只准读：白名单
pigo --allowed-tools read,grep -p "这个仓库的架构是什么"

# 什么都行，就是别碰 shell：黑名单
pigo --disallowed-tools bash,bash_output,kill_bash,bash_jobs -p "帮我改下 README"

# 大小写不敏感，Claude Code 的写法直接可用
pigo --allowed-tools Read,Grep -p "..."

# 也可重复传参
pigo --allowed-tools read --allowed-tools grep -p "..."
```

语义要点：

- **黑名单优先。** 同一工具同时出现在两侧时被移除（fail-closed）。
- **白名单是硬边界，`--approve` 不能绕过。** 过滤发生在工具注册层，早于副作用确认门，所以边界外的工具从未被宣传给模型、也无法被调用。`--approve` 只免掉逐次确认，不放行边界外工具。
- **子 Agent 继承边界。** `task` 派发的子 Agent 同样受约束，否则"让子 Agent 去跑 bash"就是一条现成的逃逸路径。
- **拼错立即报错。** 未知工具名会打印全部可用名并以退出码 2 终止，绝不静默忽略——静默忽略会让你以为限制住了而其实没有。（注：`--no-tools` 已禁用全部工具，此时工具策略不生效也不校验，pigo 会打印一行提示。）
- **暂不支持参数级匹配。** `Bash(git log:*)`、`Read(src/**)` 这类 Claude Code 语法本期不支持，写成该形式会被当作未知工具名报错。
- 屏蔽 `read` 时系统提示不再注入 `<available_skills>`，因为模型需要 `read` 才能加载技能正文。

也可在 `config.toml` 中声明默认边界。命令行传入时**按 flag 整体替换**对应的文件值（而非合并）：`--allowed-tools` 覆盖 `allowed_tools`、`--disallowed-tools` 覆盖 `disallowed_tools`，彼此独立。因此 CLI `--allowed-tools` 能放宽文件里 `allowed_tools` 收窄的范围；但由于两个列表相互独立且**黑名单始终优先**，文件级 `disallowed_tools` 不会被 CLI `--allowed-tools` 解除——要重新放行被文件拉黑的工具，需在命令行覆盖 `--disallowed-tools`。

```toml
allowed_tools = ["read", "grep"]
disallowed_tools = ["bash"]
```

---

## 运行模式

```bash
# 无头打印模式：只输出最终回答文本
pigo -p "总结这个仓库的架构"

# stream-json：逐行 JSON 事件，首个事件带 session_id
pigo -p "列出所有 Go 文件" --output-format stream-json

# 交互式 REPL：不带 -p 且 stdout 为终端时进入
pigo

# 会话管理
pigo --list-sessions              # 列出会话（含首条提问预览）
pigo --resume 20260720-1530-abcd  # 续跑指定会话（无头/REPL 均可）
pigo --continue                   # 续跑最近一次会话
# 交互式：/resume 列出会话并选取（TUI 弹层 ↑/↓ 选择，Enter 载入；REPL 用 /resume <n> 或 /resume <id>）
# 交互式：/new 就地开一个新空会话（当前会话已保存到磁盘，可随时 /resume 回来）
```

### GitHub PR Review Webhook

```
export PIGO_GITHUB_WEBHOOK_SECRET="$(openssl rand -hex 32)"
pigo --github-review --github-webhook-repo smallnest/pigo
```

Opt-in 模式：GitHub webhook 收到 PR `ready_for_review` 事件后，自动创建一个**只读 review 会话**（仅 read/grep/find 工具，不能写文件、不能执行命令）并运行 review。部署前置条件：

- 监听地址默认 `127.0.0.1:3081`，端点是**纯 HTTP**，必须放在 TLS 反向代理或隧道（如 ngrok、cloudflared、Caddy）之后才能暴露公网；
- webhook secret 通过**环境变量名间接引用**（`--github-webhook-secret-env`），高熵值（`openssl rand -hex 32`），与 GitHub webhook 配置中的 secret 一致；
- 事件验签（HMAC-SHA256）、按 delivery id 去重防重放；只处理 `pull_request` 的 `ready_for_review` action，其余事件 202 忽略。

REPL 中的内置斜杠命令包括 `/model`、`/models`、`/skill`、`/mcp`、`/jobs`、`/think`、`/reload`、`/help`、`/compact`、`/fork`、`/clone`、`/new`、`/tree`、`/rewind`、`/export`、`/import`、`/resume`、`/copy`、`/session`、`/status`、`/exit` 等。`/new` 就地开始一个空会话（新 session id、清空上下文、重放 SessionStart hook），当前会话会先落盘，事后可用 `/resume` 或 `--resume` 回到它；工具、hooks、MCP 连接、记忆库等会话级设施保持不变，不需要重启 pigo。`/resume` 列出已存储的会话（时间、项目、模型与首条提问预览），TUI 下用 ↑/↓ 选中后 Enter 载入，REPL 下用 `/resume <n>` 序号或 `/resume <id>`（支持唯一前缀）；载入会就地切换当前会话并重放历史，无需重启。重放的是该会话**当前分支**：`/tree` 切分支、`/rewind` 回退后留下的旁支不会被重放，下一轮从当前叶子继续；若会话经历过上下文压缩，重放仍显示压缩前的完整历史并在压缩点标注，而模型上下文从最近的压缩检查点继续（被摘要的前缀不会重复发送），压缩也不会再删除磁盘上的旧对话。恢复会话（启动时 `--resume`／`/resume`，TUI 与 REPL 一致）还会把该会话之前**键入过的提问**重新放回 ↑/↓ 历史输入列表，可以直接召回；列表取自当前分支的完整历史（包含已被压缩摘要掉的轮次），与重放的 transcript 一致，而不是只保留压缩检查点之后的上下文。`/jobs [running]` 一次列出当前所有后台工作（`bash` 后台命令与后台 sub-agent）：id、状态、耗时、命令行/任务描述，并给出读取与终止的调用示例（`bash_output` / `kill_bash` / `task_output`）；带 `running` 时只显示仍在运行的条目。其中 `/think [off|minimal|low|medium|high|xhigh|max]` 可在运行时查看或切换推理强度（reasoning effort），空参展示当前级别，切换后自下一轮生效。`/rewind [n]` 是编辑回滚（对标 Claude Code 的 Esc-Esc）：空参列出各轮产生的还原点，`/rewind n` 会把 write/edit 工具改动的文件恢复到该轮之前的内容，并同时把对话回退到那一轮之前（暂不含 bash 改动的文件）。`/status` 一次性展示运行时模型配置、上下文占用与压缩、项目环境（信任 / 技能 / 插件）、凭据连通性，以及遥测数据（累计与最近一次 run 的轮次、工具耗时、上下文利用率）。

### TUI 显示与工具卡

- **常驻且实时的上下文用量块**：状态栏右侧的琥珀色 `◔` 块永远在，不会再等到第一次 run 结束才出现——尚无测量时显示 `◔ --`，有了数据后显示 `◔ 12,345 (9%)`。它在整轮对话中连续更新：按下回车时就把提问计入，回答流式输出时随之增长，每个 turn 结束按 provider 上报的 usage 校正（无 usage 时按字符估算），压缩完成时立即回落到循环上报的压缩后 token 数（`compaction` 事件现在带有 `tokensAfter`），run 结束时再用遥测汇总对齐，`/rebuild`、`/resume`、`/new` 之后从新会话的上下文重新播种。切换模型会沿用已测量的 token 数、更换窗口预算。所有更新都走桥接事件里的副本值，绝不从 tea goroutine 读 pump goroutine 正在写的上下文，因此没有数据竞争。
- **用户输入有标记**：每条人类输入带 `▌ you` 侧栏，换行后每一行都保留侧栏，不会和模型输出混在一起；**超长输入（例如粘贴整个文件）完整展开显示**，不再截断为前若干行、也不再出现 `… N more lines` 提示——人自己刚输入的内容永远整段可见。
- **思考内容**：推理模型的思考流以暗色 `✻ thinking` 块显示在回答上方，默认只显示**最新的 6 行**（`… N earlier lines · click to expand` 标注上面省略了多少行），流式输出时始终停在模型此刻的思路上，不会挤占回答；点击该块的任意一行即可展开整段思考（展开后最多 400 行，超出提示用 `/export` 取全文），再点一次收起。
- **工具卡按工具渲染**：`bash` 头部是命令、输出保留**开头与结尾**（中间省略，失败日志的结尾才是有用信息）；`read`/`write`/`edit` 头部是路径、只列 offset/limit 等额外参数、改动以彩色 diff 区块展示；`grep`/`find` 头部是 pattern；`webfetch`/`websearch` 是 url/query；`todo` 渲染成 `[x]/[~]/[ ]` 清单 + `1/3 completed`；**MCP 工具**只显示 `mcp server/tool`、单行参数摘要与结果预览，不再整段打印 JSON 与全文；未知/插件工具同样精简。
- **长内容限行**：除用户输入外，每个区块都有行数预算，超出时以 `… N more lines · Ctrl+O` 标注（展开后仍有上限，可用 `/export` 取全文）。子 Agent（`task`）卡片的展开上限单独收紧到 40 行并提示去子 Agent 的会话里看完整报告——报告本身就是子 Agent 的整段对话，全倒进主会话会淹没你自己的提问。
- **展开即完整入参**：展开一张工具卡时，会把它收到的参数完整列出——每个键都在（包括头部那行已被截断/压平的 `command`、`path`、`prompt` 等），值不再按折叠时的列数截断，多行脚本与文件内容保留换行；MCP 卡也会从一行 JSON 摘要展开成逐键列出。参数超过 400 行时仍以 `… N more argument lines (see /export)` 收尾，避免一次写入巨型文件把整帧拖慢。
- **手动选择与展开**：点击任意历史工具卡或思考块即可展开/收起（按住拖动仍是文本选择）；`Alt+↑` / `Alt+↓` 在工具卡之间移动选择（选中卡片边框高亮），`Ctrl+O` 展开/收起选中卡片（未选中时作用于最新一张）；点击空白处取消选择。
- **`/skill` 弹层多选器 + 参数表单**：TUI 里输入不带参数的 `/skill` 会弹出可筛选的技能多选列表（最多 10 行、窗口内滚动，筛选行右侧显示 `(2/20)`；每行在描述后附带该技能正文声明的参数槽，如 `($1 $2)`）。按键：`↑`/`↓`（或 `Ctrl+P`/`Ctrl+N`）移动、`PgUp`/`PgDn` 整屏跳转、`Home`/`End` 首末行、`Space` 勾选/取消当前行（草稿，不立即生效）、任意可打印字符筛选（按 name/description，不区分大小写）、`Backspace` 删除筛选字符、`Enter` 进入**参数表单**、`Esc` 丢弃草稿（已提交的芯片不变）。表单按每个选中技能正文里的模板占位符生成字段：`$1`/`$2`… 按位置、`$@`/`$ARGUMENTS` 为整串参数、`${1:-default}` 预填默认值；没有占位符的技能得到一个可选的自由文本字段（内容追加到正文末尾）。表单按键：`Tab`/`↑`/`↓` 换字段、`←`/`→`/`Home`/`End` 在字段内移动光标、`Backspace`/`Delete` 编辑、`Enter` 把值落到输入框上方的 `⚡ skills: review(fix main)` 芯片条、`Esc` 退回多选列表（勾选和已输入的值都保留）。再次打开会以当前芯片（含参数）作为草稿。提交消息时，所有选中技能的正文以 `<skill name="…" args="…">` 块拼在提问前面，参数经与 REPL 相同的 `ExpandTemplate` 展开，随后芯片清空——**选择是一次性的**，不会影响下一轮。TUI 里 `/skill <name> [args]` 不再立即执行，而是选中该技能并打开已预填的表单；REPL/无头没有芯片状态，仍直接 `/skill <name> [args]` 单次调用。`/skill` 空参打印技能清单（含参数槽），`/skill clear`（TUI）清空芯片。
- **`/mcp` 弹层切换器**：TUI 里输入不带参数的 `/mcp` 会弹出服务器多选列表，每行显示 `on · N tools` / `off`（连接失败时附带错误摘要），排版与按键同 `/skill`。`Space` **立即**连接/断开该服务器：连接会拉起 stdio 子进程或完成 HTTP 握手，成功后其工具马上注册进执行器、下一轮即可调用；断开则注销并关闭连接，下一次工具列表只包含仍激活的服务器。切换在 tea loop 之外执行（连接可能阻塞），行上显示 `switching…`；`Enter`/`Esc` 关闭弹层。文本形式 `/mcp <server> [on|off]` 在 REPL 与 TUI 中都可用，`/mcp` 空参在 REPL 打印清单。
- **`/model` 弹层选择器**：TUI 里直接输入 `/model`（或 `/models`）会弹出可筛选的模型选择器，而不是打印一长串文本列表——光标默认停在当前模型上（标 `(current)`），可见列表最多 10 行，更长的目录在窗口内滚动，筛选行右侧的 `(12/95)` 显示当前位置。按键：`↑`/`↓`（或 `Ctrl+P`/`Ctrl+N`）逐行移动并在两端循环，`PgUp`/`PgDn` 整屏跳转（到边界即停），`Home`/`End` 跳到首行/末行，鼠标滚轮同样移动高亮；任意可打印字符追加为筛选词（按模型 id、provider、显示名匹配，不区分大小写），`Backspace` 删除筛选字符，`Enter` 切换到高亮模型，`Esc` / `Ctrl+C` 取消。选择走的是 `/model <id>` 完全相同的切换路径（`prompts.ApplyModelSwitch`），状态栏立即更新；`/model <id>`、`/models <provider>`、`/models fetch` 仍保持原有的文本行为。

### 全屏编辑（`Ctrl+E`，vim 模态）

输入框默认是普通的多行编辑器（`Enter` 发送、`Shift+Enter` 换行）。按 `Ctrl+E` 进入**全屏编辑**：编辑器占满状态栏以上的整个终端，适合写长 prompt、粘贴整篇文档、反复修改段落。全屏编辑**永远是 vim 模态**——这不是开关，也没有 `--vim` 参数；内联输入框则保持原有的普通编辑方式，不会让不熟悉 vim 的人误入 normal 模式。再按 `Ctrl+E` 退出并回到原来的内联布局，草稿与光标位置都不丢。

全屏编辑器底部常驻**快捷键提示条**（随模式变化，窄终端自动缩成一行），所以不需要记命令：

```
-- INSERT --  enter newline · esc normal mode · ctrl+e leave full screen
enter sends in the inline composer · ctrl+v paste image
-- NORMAL --  i a o insert · u undo · :q / ctrl+e leave full screen
h j k l w b e move · 0 ^ $ gg G jump · dd cc yy lines · x X del · p P paste · f t find
```

（这里的 `ctrl+v` 是输入框自身的粘贴图片键；提示条按当前宽度裁剪，放不下的部分以 `…` 结尾。）

- **INSERT（进入全屏时的默认模式）**：输入、粘贴与平时一致；`Enter` **换行**（和 vim 一样），不发送——写长文时按 Enter 起新段落是本能动作，绝不该把半截 prompt 发出去。`Esc` 进入 NORMAL，不会发送也不会退出。全屏时斜杠命令的自动补全弹层不显示（命令本身仍可正常输入并执行），把整屏留给正文。
- **NORMAL**：可打印字符不会插入正文，`Enter` 是命令键（先 `i`/`a`/`o` 回到 INSERT），`↑`/`↓` 是 `k`/`j` 而不是历史。
- **移动**：`h j k l`、`w b e`、`0 ^ $`、`gg G`、`f/t/F/T{char}`，均支持 `{count}` 前缀（`3w`、`2j`、`2G`）。
- **修改**：`x X`、`dd D`、`dw de d$ d0 dj dG dgg df/dt`、`cc C s S`、`r{char}`、`~`、`J`；`y` 系列同理，`p`/`P` 粘贴。
- **回到 INSERT**：`i a I A o O`、`c` 系列、`s`/`S`。
- **撤销**：`u`；一次 INSERT 会话（进入 INSERT 到 `Esc`）整体一步撤销。
- **发送**：全屏里**没有**发送键。按 `Ctrl+E`（或 NORMAL 下 `:q`）回到内联输入框，再按 `Enter` 发送——两次操作换来的是不会误发。草稿与光标位置全程保留。
- **退出全屏**：`Ctrl+E`（任何模式下都可用），NORMAL 下 `:q` 等价。
- **安全性**：NORMAL 下未映射的键被吞掉，不会破坏正文；中文/emoji 按 rune 移动。暂不支持可视模式（`v`）、宏（`q`）、寄存器前缀（`"a`）和 `/` 搜索（`/` 仍归斜杠菜单）。
- 只影响全屏 TUI；REPL（`--no-tui`）和无头模式没有这个概念。

在交互终端输入时，pigo 会用灰色文字提示最近匹配的输入或斜杠命令；
输入 `/model ` 时还会从最近使用的模型和内置模型目录中匹配。按 `Tab`
或右方向键接受当前提示；当有多个匹配时，按上/下方向键可在候选提示之间
循环选择上一个或下一个，继续输入则会实时缩小匹配范围。

---

## 作为 SDK 嵌入

除命令行外，pigo 也提供一个公共、可导入的 SDK 包，方便把编码智能体嵌入到你自己的 Go 程序中（issue [#554](https://github.com/smallnest/pigo/issues/554)）。

pigo 的实现代码全部位于 `internal/` 下，Go 禁止外部模块导入；`agent` 包是官方支持的接口层，所有导出类型都是 Go 基本类型（`string`、`[]string`、`bool`、`func`），因此你的代码不会依赖任何 pigo 内部类型。

```go
import "github.com/smallnest/pigo/agent"

sess, err := agent.New(
    agent.WithModel("claude-opus-4-8"),
    agent.WithAPIKey(os.Getenv("ANTHROPIC_API_KEY")),
)
if err != nil {
    log.Fatal(err)
}
defer sess.Close()

reply, err := sess.Prompt(context.Background(), "用一句话介绍什么是编码智能体")
fmt.Println(reply)
```

- **默认启用工具且自动执行**：会话默认拥有全部内置工具，并且不经确认即执行（等价于 CLI 的 `--approve`）。可用 `WithTools`（白名单）、`WithDisallowedTools`（黑名单，始终优先）或 `WithoutTools`（不启用工具）加以约束。
- **技能与记忆默认关闭**：保持嵌入式会话“隔离”，不读写本机共享状态，可用 `WithSkills` / `WithMemory` 开启。
- **多轮对话**：同一个 `Session` 会跨调用保留历史；`Reset` 可清空历史。`Session` 非并发安全。

完整的可运行示例（01~07：最小调用、流式输出、模型与推理强度、系统提示词、工具策略、多轮对话、自定义 Provider）见 [`examples/sdk/`](examples/sdk/)，API 文档见 `go doc github.com/smallnest/pigo/agent`。

---

## 系统提示词组装

系统提示词按三层顺序拼装（`internal/runtime/prompt.go`）：

1. **base 指令**：默认的 coding-assistant 提示词，可用 `--system-prompt` 整体替换。
2. **环境块**：工作目录、OS/架构、当前日期。
3. **`AGENTS.md` 注入**：从仓库根目录到当前工作目录，**由通用到具体**依次拼接——越靠近工作目录（越具体）的 `AGENTS.md` 排在越后，优先级更高。

`--append-system-prompt` 的内容追加在最后，按参数顺序排列；每个值若为存在的普通文件则读取文件内容，否则作为字面文本，空条目跳过。

---

## 项目信任

副作用工具（`bash` / `write` / `edit`）在**未信任**或**未决定**的目录下需要逐次确认。信任状态按目录三态（Trusted / Untrusted / Undecided）持久化为 JSON。

- 首次在某目录启动 REPL 时会提示是否信任。
- `--approve` / `-a` 为本次运行一次性授予会话级信任，跳过首次提示并免逐次确认。
- `--approve` **只免掉确认，不放大权限**：`--allowed-tools` / `--disallowed-tools` 划定的边界是硬边界（见[工具级准入](#工具级准入)），边界外的工具不在工具集中，`-a` 也无法调用。

---

## pigorc：用 shell 脚本配置

除了 `config.toml` 与 `config.json`，pigo 还支持 **pigorc**——一个真正的 Bash 脚本，用最小的 DSL 构建配置（对标 crushrc）。因为它是 Bash，`source` 复用、条件判断、变量、`$(command)` 取密钥都是免费的。

### 查找与优先级

| 位置 | 何时加载 |
|------|---------|
| `$PIGO_HOME/pigorc`（默认 `~/.pigo/pigorc`） | 总是加载 |
| `./.pigorc` | 仅当当前目录受信任（`--approve` 或信任存储已记录） |

两个文件在**同一个 bash 进程**里按顺序 source，所以项目文件可以覆盖全局文件、调用 `source` 引入公共片段、读取环境变量。

优先级（标量配置项，从低到高）：

```
内置默认 < config.toml < pigorc option < 命令行 flag
```

`thinking-level` 还会和 `config.json` 的分层链叠加：内置默认 < `config.json`(全局) < `config.json`(项目) < `PIGO_THINKING_LEVEL` < `config.toml` < `pigorc option` < 命令行 flag。

pigorc 里 `export` 的变量会写回进程环境，因此等价于在 shell 里 `export`：Provider Key、`<PROVIDER>_BASE_URL`、`PIGO_*` 都会按环境变量层级参与解析（显式的 `option base-url` / `option api-key` 仍然优先）。hook 与 `config.json` 的分层规则一致：**按事件追加合并**，pigorc 的 hook 最后执行。

### DSL

```bash
# 1) option：任意 config.toml 配置项（标量键后写覆盖，列表键追加）
option model "zai/glm-4.6"
option thinking-level high
option allowed-tools read          # 列表键：重复调用即追加
option allowed-tools grep
option reset allowed-tools         # 清空列表（例如 source 了公共片段之后）
option memory.enabled false        # 嵌套表用小圆点
option checkpoint.push-caps.memory 900

# 2) provider：定义第三方 Provider（等价于 config.toml 的 [[providers]]）
provider add mycorp --type openai --base-url "https://llm.mycorp.com/v1" \
  --env-var MYCORP_API_KEY --model mycorp-large

# 3) agent：定义自定义 sub-agent（等价于 config.toml 的 [[agents]]）
agent add researcher --description "只读调研" --tools read,grep --prompt-file ./researcher.md

# 4) hook：命令式 hook（和 config.json 的 hooks 等价）
hook add PreToolUse --matcher '^bash$' --timeout 5 --command './.pigo/hooks/no-rm.sh'

# 5) hook：函数式 hook —— 函数名就是事件名（大小写/下划线/短横线都认）
PreToolUse() {
  # stdin 是单行 JSON，退出码决定结果：0 放行，2 阻断（stderr 为原因）
  grep -q 'rm -rf /' <<<"$(cat)" && { echo 'dangerous command' >&2; exit 2; }
  return 0
}
post_tool_use() { gofmt -w . >/dev/null 2>&1 || true; }   # 9 个事件都可用

# 6) export：环境变量（Provider 密钥、PIGO_* 全部可用）
export ANTHROPIC_API_KEY="$(pass show anthropic)"
export PIGO_MAX_SUBAGENTS=4
```

脚本以 `set -eu` 执行：**任何一步失败都会中止加载**（已生效的 option 保留），并打印警告，但不会阻止 pigo 启动。没有 `bash`、超时（15s）、未知 option 键都只产生警告。

### 逐个配置项的 pigorc 写法

**模型与连接**

| 配置项 | config.toml | pigorc |
|--------|-------------|--------|
| 模型 | `model` | `option model "zai/glm-4.6"` |
| Provider | `provider` | `option provider zai` |
| 端点 | `base_url` | `option base-url "https://api.z.ai/api"` |
| 协议 | `protocol` | `option protocol anthropic` |
| API Key | `api_key` | `option api-key "$KEY"`（或用 `export ZAI_API_KEY=...`） |
| 具名凭据 | `credential` | `option credential work` |
| 推理强度 | `thinking_level` | `option thinking-level high` |
| 输出格式 | `output_format` | `option output-format stream-json` |
| 系统提示词 | `system_prompt` | `option system-prompt "be terse"` |
| 禁用工具 | `no_tools` | `option no-tools true` |
| 禁用技能 | `no_skills` | `option no-skills true` |
| 信任目录 | `approve` | `option approve true` |
| 工具白名单 | `allowed_tools` | `option allowed-tools read`（可重复） |
| 工具黑名单 | `disallowed_tools` | `option disallowed-tools bash`（可重复） |
| 模板来源 | `prompts` | `option prompts ~/.pigo/prompts`（可重复） |

**持久记忆 / 无限上下文 / 压缩 / dream**

| 配置项 | config.toml | pigorc |
|--------|-------------|--------|
| 记忆开关 | `[memory] enabled` | `option memory.enabled false` |
| 检索前重建索引 | `[memory] reconcile_on_search` | `option memory.reconcile-on-search false` |
| 相关度下限 | `[memory] search_score_floor` | `option memory.search-score-floor 0.25` |
| Claude Code 记忆 | `[memory] cc_index` | `option memory.cc-index true` |
| 压缩触发水位 | `[checkpoint] thresholds` | `option checkpoint.thresholds 40%`（可重复） |
| 预留 token | `[checkpoint] reserved` | `option checkpoint.reserved 8192` 或 `option checkpoint.reserved 10%` |
| 注入片段预算 | `[checkpoint.push_caps]` | `option checkpoint.push-caps.memory 800`、`option checkpoint.push-caps.recall 1200` |
| 压缩阈值 | `[compaction] max_context` | `option compaction.max-context 300K`（或 `50%`） |
| dream 开关 | `[dream] enabled` | `option dream.enabled false` |
| dream 间隔 | `[dream] interval_days` | `option dream.interval-days 7` |
| dream 会话数 | `[dream] recent_sessions` | `option dream.recent-sessions 20` |

**Hook（9 个事件）**

`PreToolUse`、`PostToolUse`、`UserPromptSubmit`、`Stop`、`SubagentStop`、`SessionStart`、`SessionEnd`、`PreCompact`、`Notification` 各有两种写法：

```bash
hook add PostToolUse --matcher 'write|edit' --timeout 30 --command './.pigo/hooks/gofmt.sh'
UserPromptSubmit() { echo '{"additionalContext":"branch: '"$(git branch --show-current)"'"}'; }
```

**只能通过环境变量配置的项**（pigorc 里 `export`）

| 变量 | 用途 |
|------|------|
| `<PROVIDER>_API_KEY` | 各 Provider 密钥（如 `ANTHROPIC_API_KEY`；pigo 也接受通用形式） |
| `<PROVIDER>_BASE_URL` | 覆盖某 Provider 的端点（如 `DEEPSEEK_BASE_URL`） |
| `PIGO_HOME` | 覆盖 `~/.pigo`（会话/插件/信任/凭据）；**不影响本次 pigorc 自身的查找** |
| `PIGO_SKILLS_DIR` | 技能目录（默认 `~/.agents/skills`） |
| `PIGO_MAX_SUBAGENTS` | 子 Agent 并发上限 |
| `PIGO_STREAM_IDLE_TIMEOUT` | 流式空闲超时 |
| `PIGO_DEBUG` | 打开诊断输出（默认静默）。TUI 下诊断信息渲染进对话区而不是写 stderr，避免破坏全屏画面 |
| `PIGO_EVENT_LOG` | 事件日志路径 |
| `XDG_CONFIG_HOME` | 改变 `config.toml` 的查找根 |

**仅命令行、pigorc 不适用**：`-p/--print`、`--cwd`、`--no-tui`、`--version`、`--list-sessions`、`--resume`/`--continue`、`--subagent-rpc`、`--dream*`、`--github-review*`、`--append-system-prompt`、`--prompt-template`、`--no-prompt-templates`、`--image`——它们描述的是"这一次怎么跑"，不是持久配置。其余 flag 都有上面的配置等价物。

### 示例

```bash
#!/usr/bin/env bash
# ~/.pigo/pigorc
source "$HOME/.pigo/secrets.sh"          # export ANTHROPIC_API_KEY=...

option provider anthropic
option model claude-opus-4-8
option thinking-level high
option allowed-tools bash                 # 只放开这几个工具
option allowed-tools read
option allowed-tools edit
option prompts "$HOME/.pigo/prompts"
option checkpoint.thresholds 50%
option checkpoint.thresholds 80%

PostToolUse() {
  [[ "$PIGO_VERSION" == devel ]] && return 0
  gofmt -l . >/dev/null 2>&1 || true
}
```

### 内置 pigorc skill（自动更新的配置参考）

pigo 内置一个 `pigorc` 技能（首次启动自动装到 `~/.agents/skills/pigorc/`，也可用 `/skill pigorc` 调用或让模型自行查阅）：

- `SKILL.md`：手写的配置指南与常用配方（模型/第三方 provider/自定义 agent/hook/记忆/信任/排错）；
- `reference.md`：**机器生成的完整参考**，每次启动由当前二进制重写（原子写入），内容直接来自代码注册表——`option` 全量键表、`[[providers]]`/`agent` 字段、9 个 hook 事件、环境变量、以及本二进制注册的**全部 CLI flag**。

因此只要新增了可配置项（新的 config.toml 字段、provider/agent 字段、hook 事件、`PIGO_*` 变量或 flag），参考文档下一个版本自动包含它；同时有覆盖测试强制要求：`config.toml` 的每个字段、`ProviderConfig`/`AgentConfig` 的每个字段、`hooks.AllEvents` 的每个事件、代码中读取的每个 `PIGO_*` 变量、以及每个 CLI flag 都必须在 `reference.md` 中出现，否则测试失败。

> 生成只作用于 pigo 自己的技能目录（`SKILL.md` 带标记）；你自己写的同名 `pigorc` 技能不会被覆盖。

### 安全须知

pigorc 是**可信代码**：它由 `bash` 完整执行，拥有你本人的权限，在 UI 出现之前运行。项目级 `.pigorc` 与项目级 hook 一样**只在目录受信任时加载**（fail-closed），但全局文件始终执行——不要把它指向不受你控制的路径。

## 自定义 sub-agent（Agent 定义）

主 agent 可以把子任务派发给 `task`（通用子 agent），也可以派发给**你自己定义的 agent**。一个 agent 定义就是一份完整的 agent 配置：名字即工具名，`system_prompt` 是它的系统提示词，`tools` 限定可用工具，还可以指定模型、推理强度、隔离方式和超时。

自定义 agent **只在 pigorc 里定义**（shell 配置是它的唯一来源，`config.toml` 没有 agent 表）：

```bash
# ~/.pigo/pigorc（项目级 ./.pigorc 可覆盖/追加，需信任目录）
agent add researcher \
  --description "只读代码库调研：返回带 file:line 引用的结论" \
  --prompt-file ~/.pigo/prompts/researcher.md \
  --tools read,grep,find,ls \
  --model claude-haiku-4 \
  --thinking off \
  --timeout 300
# --system-prompt "..." 可直接内联提示词；--isolation process 走子进程；
# --inherit-hooks 额外运行全局/项目层 hook

agent hook researcher PreToolUse --matcher '^bash$' --command './.pigo/hooks/no-bash.sh'
agent remove researcher      # 取消（例如在项目文件里屏蔽全局定义）
```

定义完成后，主 agent 会像调用 `task` 一样按名字调用它（`researcher` 出现在工具列表里，`description` 决定它何时被选中）；传 `background: true` 可后台执行（见下）。

### Hooks：默认独立，可继承

- 默认**只运行该 agent 用 `agent hook` 声明的 hook**，与主 agent 的 hook 相互独立。
- `--inherit-hooks` 时，额外运行全局/项目层解析出的 hook（`$PIGO_HOME/config.json`、被信任项目的 `./.pigo/config.json`、以及 pigorc 的 hook），本 agent 自己的 hook 先执行。
- 子 agent 的结束事件走 `SubagentStop`（不是 `Stop`），工具执行前后仍是 `PreToolUse` / `PostToolUse`。

### 后台 sub-agent（`background: true`）

默认派发是**同步**的：父 turn 等子 agent 跑完再继续。需要并行做别的事时，让子 agent 在后台跑：

```jsonc
// 模型侧调用（task 或自定义 agent 都一样）
{"prompt": "把整个仓库的 API 用法整理成文档", "background": true}
// → 立即返回：Started background sub-agent "task" as job-1.
```

- **立即返回**：工具调用马上拿到 job 句柄（`job-1`…），父 agent 继续做自己的事；同一消息里的多个后台 job 会并发启动（仍受 `PIGO_MAX_SUBAGENTS` 排队约束）。
- **完成回灌**：job 结束后，报告作为一条 **follow-up 用户消息**注入（含 job id、agent、耗时、报告正文与子会话 id）。只要还有后台 job 在跑，run 就不会自然结束——但到下一个 turn 边界时最多等“下一个完成”，不会阻塞多轮对话；被 Esc/Ctrl+C 中断时等待立即结束（job 随 run 一起取消）。
- **显式收取**：新工具 `task_output`
  - `{"job_id":"job-1"}` → 已完成返回报告，仍在跑返回状态（activity/耗时）；
  - `{"job_id":"job-1","wait":true}` → 等到完成再返回（父 agent 想“等这个结果”时用）；
  - 省略 `job_id` → 列出所有 job（id、agent、状态、耗时、任务摘要）。
- **生命周期**：job 绑定**父 run 的 context**，不是 tool call 的。run 结束（或被中断）时仍存活的后台 job 会被取消，不会留下孤儿 goroutine；正常结束时报告一定已投递（或可在 run 内用 `task_output` 取回）。
- **持久化与续跑**：后台子 agent 与同步子 agent 一致——独立会话文件（`parentSession`/`subAgent`），报告里带 `[sub-agent session: <id>]`，之后用 `session_id` 续跑。
- **不支持的组合**：process 隔离模式下后台执行会被拒绝（子进程协议只回传最终文本，且没有可续跑的会话）；`/btw` 这类一次性旁路会话不接后台报告。

> 后台报告会触发新一轮模型调用（和定时提醒一样），所以适合“长任务 + 我能先做别的”的场景，而不是用来逃逸父 turn 的上下文。

### 会话：与主 agent 一致的持久化，但不能单独恢复

- 每次子 agent 运行都会像主 agent 一样写入 `$PIGO_HOME/sessions`：独立会话文件，`parentSession` 指向派发它的对话，`subAgent` 记录 agent 名，支持 `/session`、`pigo session export` 等全部会话能力。
- 子 agent 会话**不出现在 `/resume`、`--list-sessions`、`--continue` 里**，也不能用 `--resume <id>` 直接恢复（会显式报错）：它是父对话的一部分，由父 agent 继续。
- 派发结果里会附带 `[sub-agent session: <id>]`；主 agent（或你）再次调用同一个工具并传入 `session_id` 即可**继续同一个子 agent 会话**，历史完整保留。

### 子 agent 不提问，改用「父问 + 续跑」

子 agent 没有 `question` 工具（进程隔离模式同样排除），因此不会打断你所在的交互。当它确实需要你决策时，会在报告里明确写出；主 agent 用 `question` 问你，拿到答案后带上 `session_id` 继续同一个子 agent 会话——这是官方推荐的兜底流程。

### 其他

- 子 agent 的工具集 = 内置工具 − `task` − `question`，再用 `--tools` 白名单收窄；父的 `--allowed-tools` / `--disallowed-tools` 边界会被继承（黑名单优先）。
- 子 agent 也会按上下文窗口自动压缩（与主 agent 一致），并设置自己的推理强度、超时与 hook。
- 定义冲突（与内置工具/其它 agent 重名、未知 isolation、未知 thinking、未知工具名）会在启动运行时告警并跳过该定义，不影响其它配置。
- process 隔离模式下子进程只返回最终文本：会话持久化与续跑仅支持 goroutine 模式（默认）。

## MCP（Model Context Protocol）

pigo 可以作为 MCP **客户端**连接外部 MCP 服务器，把它们的工具直接暴露给模型（工具名 `mcp__<server>__<tool>`，参数 schema 由服务器提供）。支持三种传输：

| 传输 | 配置 |
|------|------|
| stdio | `command` + `args` + `env`（子进程，行分隔 JSON-RPC） |
| Streamable HTTP（默认） | `url` + `headers`（POST JSON-RPC，响应可为 JSON 或 SSE；自动回传 `Mcp-Session-Id`） |
| 旧版 HTTP+SSE | `url` + `transport = "sse"`（GET 事件流 + 服务端宣告的 POST 端点） |

```toml
# ~/.config/pigo/config.toml
[[mcp]]
name    = "github"
url     = "https://api.githubcopilot.com/mcp/"
timeout_seconds = 60
[mcp.headers]
Authorization = "Bearer ..."

[[mcp]]
name    = "hosted"            # 需要授权的服务：首次 401 时走 OAuth（浏览器批准，令牌缓存在 ~/.pigo/mcp-oauth/）
url     = "https://mcp.example.com/mcp"
oauth   = true

[[mcp]]
name    = "legacy"            # 旧版 HTTP+SSE 服务
url     = "https://legacy.example.com/mcp"
transport = "sse"

[[mcp]]
name    = "files"
command = "npx"
args    = ["-y", "@modelcontextprotocol/server-filesystem", "/tmp"]
[mcp.env]
LOG_LEVEL = "warn"
```

```bash
# 等价的 pigorc 写法（可放在 ~/.pigo/pigorc 或项目 ./.pigorc，后者需信任目录）
mcp add github --url "https://api.githubcopilot.com/mcp/" --header Authorization "Bearer $GH_PAT"
mcp add files --command npx --arg -y --arg @modelcontextprotocol/server-filesystem --arg /tmp
mcp add hosted --url https://mcp.example.com/mcp --oauth
mcp add legacy --url https://legacy.example.com/mcp --transport sse
mcp remove github
```

行为要点：

- 启动时连接（`--no-tools` 时跳过），每个服务器做 `initialize` 握手并按其能力注册工具，单个服务器失败只告警并跳过，不影响启动；
- **tools**：`tools/list`（跟随分页）+ `tools/call`，`text`/`image`/`resource` 内容分别映射为文本/图片/文本化资源，`isError` 转为工具错误；
- **resources**：服务器声明 resources 能力时额外提供 `mcp__<server>__list_resources` / `read_resource`（文本、图片与二进制资源都有对应呈现）；
- **prompts**：声明 prompts 能力时提供 `mcp__<server>__list_prompts` / `get_prompt`（参数为字符串 map，返回渲染后的文本）；
- **服务端主动消息**：应答 `ping`、`roots/list`（返回当前工作目录作为 root），明确拒绝 `sampling/createMessage` / `elicitation/create`（pigo 不会为服务端再跑一次模型调用）；
- **热更新**：服务器发 `notifications/tools|resources|prompts/list_changed` 后，下一轮自动重新列举并替换工具集（无需重启），新工具同时注册进执行器；
- **运行时开关**：TUI 里不带参数的 `/mcp` 打开服务器切换弹层，`Space` 立即连接/断开该服务器；REPL 用 `/mcp <server> [on|off]`。启动时标记 `disabled` 的服务器也可随时打开，连接成功后工具立刻可用，断开后立刻从工具集消失，无需重启；
- **OAuth**：`oauth = true` 时对 401 端点执行 MCP 授权流程——protected-resource 元数据发现、授权服务器元数据、动态客户端注册、授权码 + PKCE（本地回环回调，自动打开浏览器并打印 URL）、令牌缓存与刷新（`$PIGO_HOME/mcp-oauth/<server>.json`，0600）；
- 每请求超时默认 60s（`timeout_seconds` 可调）；工具与其他内置工具同权（`--allowed-tools` / `--disallowed-tools`、sub-agent 继承）。
- 用内置工具 `pigo_config` 可随时查看每个服务器的配置项、传输方式、实际连接状态与工具数量（见[内置工具](#内置工具)）。

> 配置参考里的 `[[mcp]]` 字段与 pigorc `mcp` 语法会随内置 `pigorc` 技能的 `reference.md` 自动更新。

## 配置热加载（`/reload`）

交互模式下改配置不需要重启：

- **自动生效**：每轮开始前对比 `~/.config/pigo/config.toml`、`$PIGO_HOME/pigorc`、`./.pigorc`（仅受信任目录）、`$PIGO_HOME/config.json`、`./.pigo/config.json` 的修改时间与大小，有变化就先应用再跑这一轮，并打印一行提示。
- **手动生效**：随时输入 `/reload` 立即重新读取；REPL 与 TUI 行为一致。无头 `-p` 运行没有热加载。
- **重新读取**：pigorc（含它导出的环境变量）、config.toml、第三方 provider（改了定义就替换，删了就注销）、自定义 sub-agent、MCP 服务器（重新连接，旧连接关闭）、hooks（重新解析，含刚被信任的项目层）、skills、系统提示词、工具集与准入边界、model / provider / 推理强度。
- **原样保留**：会话本身（历史消息、会话 id）、后台 bash 任务、todo 清单、定时提醒、后台 sub-agent、持久化记忆库（不重开）、插件进程（不重启）。
- **失败即回退**：新配置无法装配时（例如准入边界里写了不存在的工具名、provider 解析失败）保持旧配置继续运行，并打印失败原因。

```bash
pigo                     # 交互模式
# ... 另开一个终端编辑 ~/.pigo/pigorc，例如加 mcp add、改 hook、加 agent
/reload                  # 立即生效；也可以直接继续提问，下一轮会自动应用
```

> 想确认当前实际生效的配置，问 agent 用 `pigo_config` 工具，或看 `/status`。

## 提示词模板

提示词模板是可复用的 Markdown 片段，在 REPL 中输入 `/name` 即可展开为完整 prompt（对标 [pi prompt templates](https://pi.dev/docs/latest/prompt-templates)）。模板可带 YAML frontmatter，支持位置参数、默认值与切片。

### 发现来源与优先级

pigo 从以下来源非递归加载 `*.md` 模板（文件名去掉 `.md` 即命令名）：

| 来源 | 路径 / 配置 | 优先级 tier |
|------|-------------|-------------|
| 项目级（受信任时） | `.pigo/prompts/*.md`（仅当项目受信任） | project |
| 全局 | `~/.pigo/prompts/*.md` 与 legacy `~/.pigo/commands/*.md` | global |
| 包安装 | `pigo install` 安装到 `~/.pigo/prompts` | global（并入全局） |
| 配置 | `~/.config/pigo/config.toml` 的 `prompts = ["./my-prompts", "/abs/x.md"]` | settings |
| CLI | `--prompt-template <path>`（可重复，文件或目录） | cli |

同名模板按 tier 解析：**project > global > settings > cli**，败者丢弃并在启动时报告；built-in 斜杠命令始终胜出。`--no-prompt-templates` 关闭全部模板发现（内置命令与技能不受影响，与 `--no-skills` 互相独立）。

### 模板格式

````markdown
---
description: Review PRs from URLs with structured issue and code analysis
argument-hint: "<PR-URL>"
---
Review the PR at $1. Focus on:
- Bugs and logic errors
- Security issues
- Error handling gaps
````

- `description`：可选；缺省时回退为正文首个非空行。
- `argument-hint`：可选；在 Tab 补全与 `/help` 中以 `name <hint> - description` 形式展示。用 `<angle>` 表示必选参数、`[square]` 表示可选。
- 正文是 prompt 模板，支持下面的参数语法。

### 参数语法

| 语法 | 含义 |
|------|------|
| `$1`、`$2`、… `$N` | 第 N 个位置参数（1-indexed；越界为空） |
| `$@` / `$ARGUMENTS` | 全部参数以单空格连接 |
| `${1:-default}` | arg1 存在且非空则用 arg1，否则用 `default` |
| `${@:-default}` / `${ARGUMENTS:-default}` | 全部参数非空则用之，否则 `default` |
| `${@:N}` | 从第 N 个起的所有参数 |
| `${@:N:L}` | 从第 N 个起的 L 个参数 |

调用示例：

```
/review https://github.com/owner/repo/pull/123
/component Button "onClick handler" "disabled support"
/summarize            # 模板用 ${1:-7} 时回退为 7 条要点
```

> 分词遵循 shell 引号规则：`Button "click handler"` 被切分为 `["Button", "click handler"]`。未闭合引号会回退为把原始串整体作为 `$ARGUMENTS`，保证可用。

## 技能 Skills

技能是带 YAML frontmatter（`name`、`description`，可选 `allowed-tools`、`model`、`disable-model-invocation`）的 Markdown 文件，位于 `~/.agents/skills`（可用 `PIGO_SKILLS_DIR` 覆盖）：

- 支持扁平的 `*.md` 与嵌套的 `<name>/SKILL.md`。
- 通过统一的 `/skill <name> [args]` 命令调用（展开正文为 prompt，支持 `$1`/`$@`/`$ARGUMENTS`、`${1:-default}`、`${@:N}` 等模板语法），也可作为子 Agent 工具运行。技能不再每个都注册成独立斜杠命令——名称和描述由 `/skill` 的清单/弹层统一呈现，避免几十个技能塞满命令列表。
- TUI 的 `/skill` 弹层可多选：勾选后进入参数表单（按正文占位符生成字段并预填默认值），确认后显示为输入框上方的 `⚡ skills: name(args)` 芯片，并在**下一条**消息中一起注入（一次性）。`/skill clear` 清空芯片。
- `--no-skills` 禁用技能发现（`/skill` 无技能可选、模型也不会自动调用）；格式错误的技能会被非致命地跳过。

### 模型自动调用（渐进式披露）

除了手动的 `/skill` 调用，技能还可被模型**自动调用**。pigo 采用渐进式披露：仅将每个技能的 `name`、`description` 和文件路径（location）注入系统提示的 `<available_skills>` 块，模型在任务匹配某技能的描述时，用 `read` 工具按需加载 `SKILL.md` 正文，而非把所有技能正文常驻上下文。

- **仅当 `read` 工具可用时**自动调用才生效（`--no-tools` 或屏蔽 `read` 时不注入 `<available_skills>`），因为模型需要 `read` 才能加载技能正文。
- 在 frontmatter 中设置 `disable-model-invocation: true` 可将某技能排除出 `<available_skills>`（模型不会自动调用它），但仍可显式调用：REPL 用 `/skill <name>`，TUI 从 `/skill` 弹层选中它并填参数。

---

## 插件

外部插件从 `$PIGO_HOME/plugins`（默认 `~/.pigo/plugins`）发现：

- 容错发现——启动失败的插件会被记录并跳过。
- 插件可提供额外工具，并订阅 Agent 生命周期事件。
- `--no-tools` 会整体跳过插件发现。

---

## Hooks

Hooks 让你在 Agent 生命周期的关键节点运行**自定义 shell 命令**，无需写 Go 或编译插件即可拦截、注入或观察 Agent 行为（对标 Claude Code 的 hooks）。命令以你**当前用户身份**执行，通过 stdin 收到一份 JSON、通过退出码与 stdout JSON 影响 Agent。

### Hook 点一览（9 个）

| 事件 | 触发时机 | 能否阻断 | 关键输入字段 |
|------|---------|:-------:|-------------|
| `PreToolUse` | 工具执行前 | ✅ | `tool_name`, `tool_input` |
| `PostToolUse` | 工具执行后 | 反馈 | `tool_name`, `tool_input`, `tool_response` |
| `UserPromptSubmit` | 用户提交 prompt 后、进入模型前 | ✅ | `prompt` |
| `Stop` | 主 Agent 一轮自然结束时 | ✅（要求继续） | `stop_reason` |
| `SubagentStop` | 子 Agent 结束时 | ✅（要求继续） | `stop_reason` |
| `SessionStart` | 会话开始 / 恢复 | 注入 | `source`（`startup`/`resume`） |
| `SessionEnd` | 会话结束 | 观察 | `stop_reason` |
| `PreCompact` | 上下文压缩前 | 观察 | `trigger`（`manual`/`auto`） |
| `Notification` | Agent 发出通知时 | 观察 | `message` |

### 输入 JSON（写入 hook 的 stdin）

pigo 向 hook 命令的 stdin 写入**单行 JSON**。只包含可观察、非敏感字段，**绝不包含 API Key 或任何凭证**。按事件类型只携带相关字段：

```json
{
  "event_type": "PreToolUse",
  "session_id": "0f9d…",
  "project_dir": "/path/to/repo",
  "tool_name": "bash",
  "tool_input": { "command": "rm -rf /" }
}
```

| 字段 | 说明 |
|------|------|
| `event_type` | 事件名（见上表） |
| `session_id` | 会话 id（子 Agent 无会话时省略） |
| `project_dir` | 当前工作目录 |
| `tool_name` / `tool_input` | 工具名与入参（Pre/PostToolUse） |
| `tool_response` | 工具返回（PostToolUse） |
| `prompt` | 用户输入（UserPromptSubmit） |
| `stop_reason` | 结束原因（Stop/SessionEnd） |
| `source` | `startup` 或 `resume`（SessionStart） |
| `trigger` | `manual` 或 `auto`（PreCompact） |
| `message` | 通知内容（Notification） |

### 输出协议（退出码 + stdout JSON）

hook 通过**退出码**给出决定：

- **`0`**：放行。若 stdout 是合法 JSON，则按下表解析；非 JSON 视为无操作。
- **`2`**：阻断。stderr 作为阻断原因（等价于 stdout 输出 `{"decision":"block"}`）。
- **其它非 0**：执行失败，记录警告并对阻断型 hook **fail-open**（不阻断 Agent）。

退出码 0 时，可选地在 stdout 打印 JSON 精细控制：

| 字段 | 类型 | 作用 |
|------|------|------|
| `decision` | string | `"block"` 阻断；`"approve"` 或空放行 |
| `reason` | string | 阻断原因 / 反馈文本 |
| `additionalContext` | string | 注入给模型的额外上下文（UserPromptSubmit / SessionStart） |
| `continue` | bool | `false` 等价于阻断 |
| `updatedInput` | object | 仅 PreToolUse：改写工具入参后再执行 |

多个 hook 命中同一事件时：任一阻断即阻断；`additionalContext` 按顺序累加；`updatedInput` 以最后一个为准。每个 hook 默认 60s 超时（`timeout` 字段可覆盖），超时按失败处理。

### Matcher 规则

`matcher` 仅对带工具名的事件（Pre/PostToolUse）生效，语义对标 Claude Code：

- 空或 `"*"`：匹配所有工具。
- 精确工具名（如 `bash`）：只匹配该工具。
- `"|"` 分隔列表（如 `bash|write|edit`）：匹配其中任一。
- 其它：作为 **Go 正则**对工具名求值（如 `"Notebook.*"`）。

不带工具名的事件（UserPromptSubmit、Stop、SessionStart 等）忽略 matcher，全部 hook 触发。

### 分层配置

hook 配置写在 `config.json` 的 `hooks` 字段，按 `event → [{matcher, hooks}]` 组织。多层配置**按事件追加合并**（默认 < 全局 < 项目 < 环境），优先级低的先执行：

- **全局**：`$PIGO_HOME/config.json`（默认 `~/.pigo/config.json`），对所有项目生效。
- **项目**：`./.pigo/config.json`，**仅当项目被信任时加载**（见下方安全须知）。

```jsonc
// ~/.pigo/config.json —— 全局：所有会话都注入 git 分支
{
  "hooks": {
    "UserPromptSubmit": [
      { "hooks": [{ "type": "command", "command": "~/.pigo/hooks/inject-branch.sh" }] }
    ]
  }
}
```

```jsonc
// ./.pigo/config.json —— 项目级：仅本仓库拦截危险命令、写文件后跑格式化
{
  "hooks": {
    "PreToolUse": [
      { "matcher": "bash", "hooks": [{ "type": "command", "command": "./.pigo/hooks/block-rm-rf.sh" }] }
    ],
    "PostToolUse": [
      { "matcher": "write|edit", "hooks": [{ "type": "command", "command": "./.pigo/hooks/gofmt.sh", "timeout": 30 }] }
    ]
  }
}
```

单个 hook 条目字段：`type`（当前为 `"command"`，可省略）、`command`（要执行的 shell 命令）、`timeout`（秒，默认 60，非正数忽略）。

### 可运行示例

以下脚本记得 `chmod +x`。

**1. PreToolUse — 拦截 `rm -rf`**（退出码 2 阻断，stderr 作为原因）：

```bash
#!/usr/bin/env bash
# ~/.pigo/hooks/block-rm-rf.sh
payload=$(cat)
cmd=$(printf '%s' "$payload" | jq -r '.tool_input.command // ""')
if printf '%s' "$cmd" | grep -Eq 'rm[[:space:]]+(-[a-zA-Z]*r[a-zA-Z]*[[:space:]]+)*-?[a-zA-Z]*f'; then
  echo "blocked: 'rm -rf' is not allowed by project policy" >&2
  exit 2
fi
exit 0
```

**2. UserPromptSubmit — 注入当前 git 分支**（退出码 0 + stdout JSON 的 `additionalContext`）：

```bash
#!/usr/bin/env bash
# ~/.pigo/hooks/inject-branch.sh
branch=$(git rev-parse --abbrev-ref HEAD 2>/dev/null || echo "no-git")
printf '{"additionalContext": "Current git branch: %s"}\n' "$branch"
exit 0
```

**3. PostToolUse — 写文件后跑格式化**（观察型，读 `tool_input` 里的路径）：

```bash
#!/usr/bin/env bash
# ~/.pigo/hooks/gofmt.sh
payload=$(cat)
path=$(printf '%s' "$payload" | jq -r '.tool_input.path // .tool_input.file_path // ""')
case "$path" in
  *.go) [ -f "$path" ] && gofmt -w "$path" ;;
esac
exit 0
```

### 安全须知

- **以当前用户身份执行**：hook 就是普通 shell 命令，拥有你本人的全部权限。只配置你信任的命令，谨慎对待第三方脚本。
- **payload 不含凭证**：写入 hook stdin 的 JSON 只有可观察的非敏感字段，**绝不包含 API Key 或任何凭证**。
- **项目级 hook 仅受信任项目启用**：`./.pigo/config.json` 里的 hook 只有当项目被信任（`--approve` 或信任存储记录）时才加载；不受信任的目录一律忽略项目级 hook，避免克隆仓库即执行任意命令（fail-closed）。

---

## 包管理

安装 pi 生态的包（extension / skill / prompt / theme）。`install` 需要 PATH 上有 `npm`。

```bash
# 安装（仅支持 npm: 源，支持 scoped 包与指定版本）
pigo install npm:pi-mcp-adapter
pigo install npm:@scope/name@1.2.3

# 列出已安装的包
pigo list

# 更新指定包到 npm 最新版本（可多个）
pigo update pi-mcp-adapter

# 卸载
pigo uninstall pi-mcp-adapter
```

> 注意：不带包名的 `pigo update`（以及 `pigo update --check` 等仅带标志的调用）**不再更新全部已装包**，而是自更新 pigo 二进制本身（见下文「自更新」）。更新全部包请逐个执行 `pigo update <包名>`。

包类型（`extension` / `skill` / `prompt` / `theme`）会分别分发到对应目录，安装记录写入 lockfile。

---

## 自更新

`pigo` 可以把自身二进制升级到 GitHub Release 上的最新版本。无参数的 `pigo update`（或仅带标志的调用，如 `pigo update --check`）会走自更新路径；带包名时才是包更新。

```bash
# 检查并自更新 pigo 二进制到最新 Release
pigo update
```

- 与包更新的路由区分完全由参数决定：任一不以 `-` 开头的参数视为包名 → 包更新；否则（无参数或仅标志）→ 自更新。
- 进入交互式 TUI 时，pigo 会在后台异步检查最新 Release（24h 缓存于 `$PIGO_HOME/update-check.json`），有新版本时在启动横幅提示 `Run pigo update to upgrade`；`dev` 构建不检查。
- 自更新会下载对应平台的最新二进制并原地替换当前可执行文件；若目标路径需要更高权限，会提示改用 `sudo` 重试。

---

## 发布（Release）

使用 [goreleaser](https://goreleaser.com) 构建跨平台二进制并发布到 GitHub Release。

```bash
# 校验配置
goreleaser check

# 本地试跑（快照，不发布）
goreleaser release --snapshot --clean

# 正式发布：打 tag 并推送，GitHub Actions 自动触发
git tag -a v0.2.0 -m "v0.2.0"
git push origin v0.2.0
```

推送 `v*` tag 会触发 `.github/workflows/release.yml`，由 goreleaser 构建 Linux/macOS/Windows × amd64/arm64 的归档包、生成 checksums 并创建 Release。版本号 / commit / 构建时间通过 `-ldflags` 注入 `main` 包，可用 `pigo --version` 查看。

---

## 目录与环境变量

| 变量 / 路径 | 用途 |
|-------------|------|
| `PIGO_HOME` | 覆盖 `~/.pigo` 基础目录（影响 plugins、commands、prompts） |
| `PIGO_SKILLS_DIR` | 覆盖技能目录（默认 `~/.agents/skills`） |
| `~/.pigo/sessions` | 会话存储（JSONL） |
| `~/.pigo/plugins` | 外部插件 |
| `~/.pigo/prompts` | 提示词模板（pi 对齐；`pigo install` 的安装目标） |
| `~/.pigo/commands` | 用户自定义命令模板（legacy，仍加载） |
| `.pigo/prompts` | 项目级提示词模板（仅当项目受信任时加载） |
| `~/.config/pigo/config.toml` 的 `prompts` | 配置追加的模板来源（settings tier） |
| `--prompt-template <path>` | CLI 追加的模板来源（cli tier，可重复） |
| `<PROVIDER>_API_KEY` | 各 Provider 的 API Key（见[模型与 Provider](#模型与-provider)） |

### 内置 Provider 一览（`--provider`）

`--provider <name>` 直接选中某个内置 Provider，使用其默认 base URL、协议与 API Key 环境变量（可用 `--base-url` 或 `<PROVIDER>_BASE_URL` 覆盖，`--api-key` 或对应环境变量提供 Key）。下表与注册表 `internal/provider/registry.go` 保持一致，`pigo --help` 也会列出同样的清单。

| provider | 环境变量（按优先级） | 默认 base_url | 协议 |
|----------|----------------------|---------------|------|
| `anthropic` | `ANTHROPIC_OAUTH_TOKEN` / `ANTHROPIC_API_KEY` / `CLAUDE_API_KEY` | `https://api.anthropic.com/v1` | anthropic |
| `openai` | `OPENAI_API_KEY` | `https://api.openai.com/v1` | openai |
| `ant-ling` | `ANT_LING_API_KEY` | `https://api.ant-ling.com/v1` | openai |
| `deepseek` | `DEEPSEEK_API_KEY` | `https://api.deepseek.com` | openai |
| `nvidia` | `NVIDIA_API_KEY` / `NVIDIA_NIM_API_KEY` | `https://integrate.api.nvidia.com/v1` | openai |
| `google` | `GEMINI_API_KEY` / `GOOGLE_API_KEY` | `https://generativelanguage.googleapis.com/v1beta` | openai |
| `groq` | `GROQ_API_KEY` | `https://api.groq.com/openai/v1` | openai |
| `cerebras` | `CEREBRAS_API_KEY` | `https://api.cerebras.ai/v1` | openai |
| `xai` | `XAI_API_KEY` | `https://api.x.ai/v1` | openai |
| `openrouter` | `OPENROUTER_API_KEY` | `https://openrouter.ai/api/v1` | openai |
| `vercel-ai-gateway` | `AI_GATEWAY_API_KEY` | `https://ai-gateway.vercel.sh` | openai |
| `zai` | `ZAI_API_KEY` | `https://api.z.ai/api/coding/paas/v4` | openai |
| `zai-coding-cn` | `ZAI_CODING_CN_API_KEY` | `https://open.bigmodel.cn/api/coding/paas/v4` | openai |
| `mistral` | `MISTRAL_API_KEY` | `https://api.mistral.ai` | openai |
| `minimax` | `MINIMAX_API_KEY` | `https://api.minimax.io/anthropic` | anthropic |
| `minimax-cn` | `MINIMAX_CN_API_KEY` | `https://api.minimaxi.com/anthropic` | anthropic |
| `moonshotai` | `MOONSHOT_API_KEY` | `https://api.moonshot.ai/v1` | openai |
| `moonshotai-cn` | `MOONSHOT_API_KEY` | `https://api.moonshot.cn/v1` | openai |
| `huggingface` | `HF_TOKEN` | `https://router.huggingface.co/v1` | openai |
| `fireworks` | `FIREWORKS_API_KEY` | `https://api.fireworks.ai/inference` | openai |
| `together` | `TOGETHER_API_KEY` | `https://api.together.ai/v1` | openai |
| `novita` | `NOVITA_API_KEY` | `https://api.novita.ai/openai/v1` | openai |
| `opencode` | `OPENCODE_API_KEY` | `https://opencode.ai/zen` | openai |
| `opencode-go` | `OPENCODE_API_KEY` | `https://opencode.ai/zen/go` | openai |
| `kimi-coding` | `KIMI_API_KEY` | `https://api.kimi.com/coding/v1` | openai |
| `xiaomi` | `XIAOMI_API_KEY` | `https://api.xiaomimimo.com/v1` | openai |
| `xiaomi-token-plan-cn` | `XIAOMI_TOKEN_PLAN_CN_API_KEY` | `https://token-plan-cn.xiaomimimo.com/v1` | openai |
| `xiaomi-token-plan-ams` | `XIAOMI_TOKEN_PLAN_AMS_API_KEY` | `https://token-plan-ams.xiaomimimo.com/v1` | openai |
| `xiaomi-token-plan-sgp` | `XIAOMI_TOKEN_PLAN_SGP_API_KEY` | `https://token-plan-sgp.xiaomimimo.com/v1` | openai |
| `qianfan` | `QIANFAN_API_KEY` | `https://qianfan.baidubce.com/v2` | openai |
| `volcengine` | `ARK_API_KEY` / `VOLCENGINE_API_KEY` | `https://ark.cn-beijing.volces.com/api/v3` | openai |
| `dashscope` | `DASHSCOPE_API_KEY` | `https://dashscope.aliyuncs.com/compatible-mode/v1` | openai |
| `hunyuan` | `HUNYUAN_API_KEY` | `https://api.hunyuan.cloud.tencent.com/v1` | openai |
| `azure-openai-responses` | `AZURE_OPENAI_API_KEY`（+ `AZURE_OPENAI_BASE_URL` / `AZURE_OPENAI_RESOURCE_NAME`） | 由环境变量拼接 | openai（Azure） |
| `amazon-bedrock` | `AWS_BEARER_TOKEN_BEDROCK`（或 `AWS_PROFILE` / `AWS_ACCESS_KEY_ID`+`AWS_SECRET_ACCESS_KEY`；`AWS_REGION` 默认 `us-east-1`） | `https://bedrock-runtime.{AWS_REGION}.amazonaws.com` | anthropic |
| `google-vertex` | `GOOGLE_CLOUD_API_KEY`（或 ADC）+ `GOOGLE_CLOUD_PROJECT` + `GOOGLE_CLOUD_LOCATION` | `https://{location}-aiplatform.googleapis.com` | openai |
| `cloudflare-workers-ai` | `CLOUDFLARE_API_KEY` + `CLOUDFLARE_ACCOUNT_ID` | `https://api.cloudflare.com/client/v4/accounts/{account_id}/ai/v1` | openai |
| `cloudflare-ai-gateway` | `CLOUDFLARE_API_KEY` + `CLOUDFLARE_ACCOUNT_ID` + `CLOUDFLARE_GATEWAY_ID` | `https://gateway.ai.cloudflare.com/v1/{account_id}/{gateway_id}/anthropic` | anthropic |

> base_url 覆盖优先级：`--base-url` > provider 专有 `*_BASE_URL` 环境变量 > 泛化 `<PROVIDER>_BASE_URL`（provider 名大写、`-` 转 `_`）> 注册表默认值。任意 Provider 也支持泛化的 `<PROVIDER>_API_KEY` 约定作为 Key 回退。

> 火山方舟（`volcengine`）部分模型需以「推理接入点 ID（endpoint id）」而非模型名调用，此时用 `-m <endpoint-id>` 指定即可；本仓库预置的 `doubao-seed-1-6` 走模型名方式。

---

## 安全说明

- pigo 会向解析出的 Provider 端点发起外部网络请求。
- `bash` / `write` / `edit` 会在本地产生副作用，仅由项目信任机制把关；`--approve` 会跳过逐次确认，请在受信任的目录中使用，权衡便利与安全。
- 处理来自文件、命令输出、网页等外部来源的内容时应视为不可信数据。
- 需要操作系统级隔离时，参见 [docs/sandboxing.md](docs/sandboxing.md)：Docker 整进程、micro-VM、进程级策略沙箱三种模式与 trust/tool-policy 的组合矩阵。

---

## 许可证

参见仓库根目录的 [LICENSE](LICENSE)。
