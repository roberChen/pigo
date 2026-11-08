# pigo 架构：分层、数据流与 UI / Host 拆分

> 本文描述 pigo 的**现状架构**（第一～五节），以及把它演进为 **UI / Host 拆分**（第六节）的设计与落地方式。文中的路径、类型名与函数名都可以直接在仓库里检索到；新增能力时请同步更新本文。

## 1. 分层总览

```
cmd/pigo                     flag 解析 + dispatch（唯一的进程入口）
  └─ internal/cli/run        run 装配：provider、工具、prompt、hooks、skills、MCP、memory
       ├─ internal/cli/tui        全屏 Bubble Tea UI
       ├─ internal/cli/repl       行式 REPL（手写行编辑器）
       └─ internal/cli/headless   -p / stream-json / 会话恢复 / --subagent-rpc
  └─ internal/cli/hostcmd    pigo serve / pigo attach（UI / Host 拆分的入口）
       ├─ internal/hostserver    常驻 host 守护进程（一个 host 一个进程）
       └─ internal/hostclient    任意 UI 使用的客户端库

internal/runtime              agent loop、prompt 组装、compaction、sub-agent、skills、slash
internal/agentcore            核心类型：消息、content block、事件、AgentTool、AgentContext
internal/agenttool            内置工具 + 注册表/执行器（三阶段执行、JSON-Schema 校验、重试、裁剪）
internal/provider             Provider 接口、OpenAI / Anthropic 线协议、SSE、认证、预设目录
internal/compaction           切点选择、摘要、token 预算
internal/session              append-only JSONL 会话持久化（schema v3，id/parentId 树）
internal/trust                目录信任三态
internal/hooks                用户 shell hooks（9 类事件）
internal/plugin / pkgmgr / pihost   外部插件、pi 生态包安装、内嵌 Node host
internal/memory / dream       SQLite + FTS5 持久记忆、LLM 归纳整理
internal/remotecontrol        /remote-control：局域网 WebSocket + 内嵌 SPA
internal/jsonrpc              子进程 JSON-RPC 2.0（MCP / plugin / process 隔离子 agent）
agent/                        唯一可导入的公开 SDK
```

依赖方向是单向的，箭头只向下：

```
cmd/pigo → internal/cli/{tui,repl,headless,run,hostcmd}
         → internal/hostserver → internal/cli/run → internal/runtime
         → internal/agenttool → internal/provider / internal/compaction
         → internal/agentcore（叶子，依赖极少）
```

`internal/agentcore` 是最底层：它不认识 provider，也不认识 runtime。任何跨层的“能力”都通过 `agentcore` 的类型（`AgentEvent`、`AgentTool`、`AgentContext`）或函数字段（`BeforeToolCall`、`GetFollowUpMessages`）表达，而不是反向 import。

## 2. 一次 run 的数据流

1. `cmd/pigo` 解析 flag，`resolveConfig` 叠加 `pigorc` 与 `~/.config/pigo/config.toml`（CLI > 文件 > 默认）。
2. `run.SetupEnv` 装配 `Env`：解析 provider、按 cwd 构造内置工具、加载 skills/plugins/MCP/memory、应用 `--allowed-tools`/`--disallowed-tools` 边界、拼出 system prompt（含 `AGENTS.md` 逐层注入）。
3. 驱动方（REPL / TUI / headless / host）把用户消息追加进 `agentcore.AgentContext.Messages`，然后：

```go
stream := runtime.StartRun(ctx, agentCtx, cfg)          // 生产者 goroutine
final, err := runtime.DrainStream(ctx, stream, handler) // 消费者，永远 drain 完
```

4. loop 内层：流式 assistant 消息 → `agenttool.BatchConfig` 三阶段执行工具（registry 校验 → `BeforeToolCall`/trust/hooks → 执行 → `AfterToolCall`）→ 结果追加 → 继续；无工具调用后由 `GetFollowUpMessages` 决定外层是否再跑。
5. 每个事件同时经 `StreamHandler.OnEvent` 分发给 UI、plugin notifier、telemetry、hooks。
6. 会话以分支追加方式落盘：`AppendBranch(header, curLeaf, tail)`；压缩后改用 `PersistCompactedContext`。

关键契约：

- **事件是唯一的上行通道**。UI 不轮询 loop 内部状态；`AgentEvent` 已经覆盖 message/turn/tool/compaction/subagent/telemetry。
- **DrainStream 必须排空**，即使写失败也要继续 drain，否则生产者 goroutine 会阻塞（`render.go` 的 no-leak 契约）。
- **诊断只走 `diag.Diagf`**。TUI 会装 `diag.Collector`，机器模式（`-p`、`stream-json`）要保持 stdout/stderr 干净。
- **会话游标**（`curLeaf` + `persisted`）由驱动方持有，loop 只改 `agentCtx.Messages`。

## 3. 三种前端如何复用同一套装配

| 前端 | 入口 | 装配方式 | 输出 |
|---|---|---|---|
| `-p` / `stream-json` | `headless.Run` | `run.SetupEnv` + `runtime.RunHeadless` | stdout 文本或 NDJSON |
| TUI | `tui.Run` | `run.SetupEnv` + `tui.startRun` | alt-screen |
| REPL | `repl.Run` | `run.SetupEnv` + `streamRun` | 行式 |

三者都调用 `run.NewConfig` 构造 `runtime.RunConfig`（provider stream、`GetAPIKey`、`Batch`、follow-up、MCP 刷新），并在每轮把 hooks seam 装到**当轮新建的** cfg 上（`run.InstallSeams`）。

`internal/cli/host.go` 定义 `Host` 接口，让 `/goal`、`/btw`、`/status` 这些控制命令读写会话状态而不依赖 `replDeps` 具体聚合体。

## 4. 并发与会话

- 一个 `AgentContext` 只能被一个 goroutine 驱动；`agent.Session` 明确不并发安全。
- 后台 sub-agent（`mode: "async"`）通过 `runtime.SubAgentJobs` 注册表把结果从 follow-up seam 送回主会话。
- 门控工具在 `BeforeToolCall` 上串联：trust 优先、hooks 其后，`--approve` 不能放宽注册层的工具边界。
- `internal/remotecontrol` 已经证明“把 REPL 会话镜像给另一个客户端”可行，但它是**单会话、只读镜像 + 注入 prompt** 的窄通道，不能承载多个独立会话。

## 5. 测试地图

| 关注点 | 入口 |
|---|---|
| loop 全链路 | `internal/runtime/faux_provider_test.go`（脚本化 Provider，真实 `StreamFnFromProvider`） |
| 工具执行器 | `internal/agenttool/*_test.go` |
| 会话持久化/树 | `internal/session/*_test.go` |
| hooks | `internal/hooks/*_test.go`（真实 shell，临时目录） |
| host 协议 | `internal/hostapi/*_test.go`、`internal/hostserver/*_test.go`、`internal/hostclient/*_test.go` |
| 公开 SDK | `agent/*_test.go` |

约定：测试不碰真实用户状态，读 `~/.pigo` 的用例必须 `t.Setenv("PIGO_HOME", t.TempDir())`。

## 6. UI / Host 拆分

### 6.1 目标

- **UI 只负责交互**：TUI、Web UI、IM 机器人都是 host 的客户端，不自己装配 provider / 工具 / 会话。
- **一台主机一个 host 进程**：所有执行都发生在常驻守护进程内，UI 崩溃或断开不影响正在跑的 turn。
- **一个接口**：所有 UI 说同一套 `internal/hostapi` 线协议。
- **会话隔离**：一个会话只属于创建它的那条连接；两个 UI 同时 attach 不会互相看到或打断对方的 turn。

### 6.2 传输与握手

```
unix socket  $PIGO_HOME/server.sock      (0600)
token 文件    $PIGO_HOME/server.token     (0600)
状态文件      $PIGO_HOME/server.json      {pid, version, startedAt, socket}
```

- 一行一个 JSON 对象（NDJSON）。请求带 `id` + `method` + `params`，响应按 `id` 关联，事件是 `event` + `params`（无 `id`）。
- 连接建立后的第一条消息必须是 `hello`，携带 token；不通过则关闭连接。socket 权限是主防线，token 是对“同用户但误连”的显式确认。
- 单实例：`listen` 前先尝试连接已有 socket；能连上说明已有守护进程，直接退出并报告它的 pid。

### 6.3 方法

| 方法 | 作用 |
|---|---|
| `hello` | 握手 + 版本协商 |
| `server.status` | pid / 启动时间 / 会话数 |
| `server.shutdown` | 优雅退出（`pigo serve stop`） |
| `session.create` | 在当前 cwd 新建会话，返回 id |
| `session.list` | 只列出**本连接**拥有的会话 |
| `session.resume` | attach 到已持久化会话，返回历史消息 |
| `session.close` | 结束会话并释放 runner |
| `prompt` | 提交一轮，流式事件随连接返回，结果给最终文本 |
| `cancel` | 取消当前 turn |
| `permission.decide` | 回答 `permission_request` |

### 6.4 事件

`session_event`（`text_delta` / `thinking_delta` / `turn_end` / `tool_start` / `tool_update` / `tool_end` / `compaction` / `agent_start` / `agent_end`）、`permission_request`、`session_closed`。

### 6.5 服务端结构

```
hostserver.Server
  ├─ conn（一条客户端连接：authed、owned sessions、写锁）
  ├─ sessionState（header、cwd、agent、curLeaf/persisted、permission 等待表）
  └─ Factory 接口  ← DefaultFactory 用 run.SetupEnvIn + runtime.StartRun
                     测试注入脚本化 Factory，无需真实 provider/凭据/网络
```

- 每个请求在自己的 goroutine 里处理，读循环永不被长 turn 阻塞（否则 `cancel` / `permission.decide` 会死锁）。`Server.wg` 让 `Shutdown` 等这些 handler 收口。
- 会话的 `prompt` 串行化：busy 时返回 `CodeBusy` 而不是排队。
- 落盘复用 `internal/session` 的分支追加模型；每个 turn 结束把新产生的消息作为当前叶子的分支追加，压缩后改走 `PersistCompactedContext`。宿主重启后 `session.resume` 可以继续。
- trust 目录直接放行；非信任目录的副作用工具（bash/write/edit）通过 `permission_request` 询问**所有者连接**，无答案（超时、turn 取消、连接断开）一律拒绝。`pigo serve --approve` 在 `NewServer` **之前**构造 `trust.Manager` 并对启动目录 `SetSessionTrust`，再把这个 manager 交给 server——session trust 存在 manager 实例里，交给另一个实例等于没授权。
- 会话以 `cwd` 为根装配：`run.SetupEnvIn(cwd, …)` 取代 `run.SetupEnv` 的 `os.Getwd()`，所以一个 host 进程可以服务任意目录下的会话，而不必被启动目录绑定。

### 6.6 客户端（`pigo attach`）的并发契约

`attach` 是行式客户端，但它必须同时做三件事：读 stdin、渲染服务端推来的 delta、在 turn **运行中**回答权限问题。因此它不能停在一次阻塞的 `Prompt` 调用里：

- 一个 goroutine 独占 stdin 并把它灌进 channel（`pumpLines`）；主循环 `select` 于「输入行 / 权限请求信号 / turn 完成」三者。
- `Prompt` 在后台 goroutine 里跑，完成结果回到主循环；因此权限问题到达时主循环仍然活着。
- 权限问题的答案就是**下一条输入行**（stdin 天然有序，不会与问题竞争）。turn 运行期间键入的行进入 `queued`，既可能是这个问题的答案，也可能是 turn 结束后要执行的下一条命令——不会被丢掉。
- Ctrl-C 取消当前 turn（空闲时断开连接）；`/exit`、Ctrl-D 在 turn 运行中先取消再断开。stdin 关闭时若还有未回答的权限请求，按拒绝处理（静默绝不放行）。

### 6.7 为什么新开包而不是扩展 jsonrpc

`internal/jsonrpc` 与 `exec.Cmd` 的 stdin/stdout 强绑定，且是 request/response 语义；host 需要的是长连接、多路会话、服务端主动推送事件、可取消的 turn。协议形状不同，复用会扭曲两边。

### 6.8 落地范围与后续

已落地：`internal/hostapi`（协议 + `EventMapper`）、`internal/hostserver`（守护进程、会话、权限、持久化、`DefaultFactory`）、`internal/hostclient`、`internal/cli/hostcmd`（`pigo serve` 前台 / `--daemon` / `stop` / `status`，`pigo attach` 行式客户端，含 `--list` / resume）。

测试面：`internal/hostapi/*_test.go`（信封与事件映射，含线格式 golden）、`internal/hostpath/*_test.go`（状态目录 / socket 长度回退 / token 与状态文件原子性）、`internal/hostserver/server_test.go` 与 `server_edge_test.go`（真实 unix socket + 脚本化 Factory：握手、隔离、串行化、取消、resume、权限、持久化，以及畸形输入、断开时机、并发建会话）、`internal/hostclient/client_test.go` 与 `client_edge_test.go`（同一对端到端、订阅分发、Close/Done 与 goroutine 泄漏）、`internal/cli/hostcmd/hostcmd_test.go` 与 `hostcmd_edge_test.go`（attach 渲染与 turn 中答题、stdin EOF / 宿主中途退出、`serve` 生命周期，并通过 `TestMain` 重执行测试二进制真正验证 `--daemon` 的 fork 路径；`cmd/pigo/hostcmd_args_test.go` 覆盖子命令的 argv 校验）。

后续：TUI 改为 host 客户端、Web UI 直接复用 `hostclient`、`-p` 增加 `--server` 走 host。
