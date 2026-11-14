# pigo CS 架构缺陷审计报告

**审计时间：** 2026-11-14  
**基线：** `19bc418 fix(host): harden the Windows daemon for concurrent UI clients`  
**范围：** `hostpath`（POSIX socket / Windows named pipe）、`hostapi`、`hostserver`、`hostclient`、`hostcmd`、run assembly 与文档契约。  
**审计方式：** 静态审计 + Windows 上真实命名管道复现。本次仅审计，不修改产品代码。

> **修复进展（同日后续提交）：** H1-H5 及主要协议/背压问题已按本文建议落地：会话级 `always` 授权、运行中只读 resume + `turnId`/`seq` 回放、有界事件队列与权限独立调度、创建预留和关闭 tombstone、shutdown/持久化回滚、握手与 POSIX stale 检查。产品能力缺口（图片、steering、配置 API、TUI 接入）及跨进程 store 协调仍需后续版本处理。

---

## 1. 总体评价

当前实现已经不是“把单进程随便拆成两个进程”：传输抽象、按连接的所有权、token 握手、每请求独立 goroutine、turn 内取消和权限事件、断开后续跑与落盘，这些方向是正确的；Windows 传输的 DACL、owner 校验、原子文件替换与 shutdown 清理也已补强。

但它还不是一套**完整的、可供多个 UI 长期共享的会话架构**。主要问题集中在四处：

1. **权限边界不一致**：协议声称 `always` 是 session-scoped，实际授权却写进整个 host 的目录级 trust manager。
2. **断线重连不是完整的流式恢复**：客户端崩溃后，运行中的 turn 虽然继续，但重连客户端看不到完整增量事件，也没有 turn/事件游标。
3. **事件与写入缺少统一背压**：客户端队列无上限、服务端同步写、callback panic 未隔离，使“一个慢 UI”可能影响整个 host。
4. **协议功能面远小于现有单体模式**：图片、steering、follow-up、模型切换等能力没有进入 host 协议，TUI 尚未接入，attach 也不是 TUI 的完整替代品。

因此建议把它定义为“本地 CS 基础设施已可用，通用多 UI 会话层尚未完成”，而不是已经完成的 CS 转换。

---

## 2. 高优先级缺陷

### H1. `always` 的权限授权会跨会话、跨客户端扩散

**证据：**

- 协议明确把 `Always` 描述为 session-scoped：`internal/hostapi/payloads.go:128-136`。
- 服务端在 `permission.decide` 中直接调用 `c.srv.trust.SetSessionTrust(st.cwd)`：`internal/hostserver/server.go:760-780`。
- `Server` 只有一个共享 `*trust.Manager`：`internal/hostserver/server.go:68-80`。
- 权限判断是 `s.trust.IsTrusted(st.cwd)`：`internal/hostserver/server.go:921-936`。
- `trust.Manager` 的 session trust 按目录存储，并且向祖先目录匹配：`internal/trust/manager.go:194-224`。

**实际影响：**

A 客户端在目录 D 的会话中选择 `always` 后，**B 客户端在 D 的另一个会话、乃至该 host 上所有位于 D 或其子目录的会话，都可能不再询问权限**。这与“会话隔离”和文档中的“session-scoped”不一致。

**复现结果：**

同目录两个独立连接 A/B：A 回答 `always` 后，B 的同目录会话运行工具时没有收到 `permission_request`，直接执行。

**根因：**

权限状态没有 session/connection 维度，而是复用了“进程级目录信任”作为授权存储。`permission.decide` 和权限门又都写了同一状态（`server.go:775-780` 与 `server.go:959-964`），存在重复授权点。

**建议：**

- 把 trust decision 分成持久化的用户目录信任、连接级授权、会话级授权三层；
- `always` 应落在会话或明确声明的连接 scope 上；
- 如果产品确实需要目录级 host-wide 授权，则必须修改协议和文案，不能称为 session-scoped；
- 增加“两个客户端、同目录、两个会话”的反向测试。

---

### H2. 客户端崩溃后的恢复只能得到历史，不能可靠恢复运行中事件

**证据：**

- 断线只把 owner 置空，运行中的 turn 继续：`internal/hostserver/server.go:418-427`。
- 运行中的 orphan 不能 adopt，resume 只返回 busy：`internal/hostserver/server.go:432-445`、`server.go:647-660`。
- 现存回归测试明确接受“先 busy，turn 完成后重新 resume”：`internal/hostserver/server_edge_test.go:1161-1199`。
- 客户端也明确不回放事件：`internal/hostclient/client_edge_test.go:769-773`。

**实际影响：**

一个 UI 崩溃、断开或刷新后：

- 正在运行的 turn 不能重新 attach 到事件流；
- 已经产生的流式事件、工具调用进度和权限中间态没有可恢复游标；
- 客户端只能等 turn 结束再从磁盘拿最终历史；
- 长时间运行的任务期间，重连用户看不到实时状态。

这不符合“UI 崩溃不影响 turn”的完整含义：执行保留了，但可观察性中断了。

**建议：**

- 给每个 turn 分配 `turnID`，每个事件分配递增 `seq`；
- host 保存该 turn 的有限事件日志（ring buffer 或按 turn 截断）；
- `session.resume` 增加 `lastSeq`，允许 busy session 只读重新订阅；
- `session.list` 增加 `runningTurnID` / `lastEventSeq`；
- 区分“恢复历史上下文”和“恢复实时观察”两个能力。

---

### H3. 事件系统没有完整的背压与故障隔离

这是三类相关问题的集合。

#### H3a. 客户端事件队列无上限

`internal/hostclient/client.go:209-218` 把事件直接 append 到 slice；`client.go:238-258` 由单 dispatcher 串行消费。

慢 callback 时队列会无限增长，没有最大字节数、最大事件数或丢弃/降级策略。流式 token、工具 update 和多会话长期运行都可能导致 UI 内存持续增长。

#### H3b. 一个慢回调会阻塞该连接的所有会话与权限事件

`dispatchLoop` 只有一个队列，不按 session 分流。一个会话的慢 UI callback 会阻塞其他会话事件；权限事件也走同一队列。

**复现结果：**

让权限 callback 阻塞后，后续会话事件在 1 秒观察窗内完全不能到达；普通 `server.status` 请求仍可返回，但“事件可用”和“RPC 可用”不是一回事。

#### H3c. 客户端不 recover callback panic

`internal/hostclient/client_edge_test.go:906-911` 明确记录：没有 recover，未恢复的 callback panic 会带走进程。对于一个通用 UI 客户端，这意味着一个面板/插件的渲染错误可能杀掉整个 UI。

**服务端也有对应问题：**

所有事件与响应共用 `conn.writeMu` 并同步 `Write`：`internal/hostserver/conn.go:187-200`。正常请求没有写 deadline，慢客户端可能阻塞其事件/响应路径，并在 shutdown 之外长期占用资源。此次构造的“无限写 + 停读”测试在 Windows 上主要观察到了关闭路径，不足以声称已稳定复现跨连接永久阻塞，因此应把这点列为代码级风险，而非已完全证明的 exploit。

**建议：**

- 先为事件队列增加“最大事件数/字节数 + 溢出标记 + 客户端重同步”；
- 权限事件使用独立、可取消的调度通道；
- 对 callback 做 recover 并隔离故障订阅；
- 对普通 socket/pipe 写增加 deadline 或专用 writer goroutine + 有界队列；
- 长会话增加 slow-consumer 压测，验证其它连接不会被拖住。

---

### H4. 会话创建/恢复与 shutdown 的并发会让 agent 泄漏或产生不一致响应

**证据：**

- `session.create` 在 `Factory.NewAgent` 中组装 agent，之后才 `publishSession`：`internal/hostserver/server.go:567-605`。
- `session.resume` 同样在组装后才发布：`server.go:624-715`。
- `Shutdown` 只处理已发布连接/会话，关闭后 `Serve` 才结束：`server.go:302-328`。
- `publishSession` 的补救只针对“owner 已关闭”：`server.go:472-483`，没有针对 shutdown 中刚创建的会话定义统一的提交协议。

**复现结果：**

让 `Factory.NewAgent` 阻塞，随后 `Shutdown`，再放开组装：client 的 create 得到 EOF，而 agent 已经构造完成并发布到 server；生命周期边界依赖 handler 是否恰好越过连接关闭窗口。

**影响：**

- provider probe、MCP 初始化、memory DB 等较慢的组装可能在 shutdown 后继续；
- 客户端得到 EOF，而 host 侧可能已经创建了一个会话；
- 在极端情况下出现资源释放与状态提交不同步。

**建议：**

- 增加“creating/resuming”状态和统一 commit 点；
- shutdown 后禁止提交，创建失败/中断必须调用 agent.Close；
- create/resume 与 shutdown 使用同一会话提交协议；
- 增加阻塞 Factory + shutdown 的永久测试。

---

### H5. host 关闭期间仍在运行的 turn 可能被 `agent.Close()` 干扰

**证据：**

`closeSessionIf` 先取消 turn，然后立刻调用 `agent.Close()`：`internal/hostserver/server.go:383-415`。而 prompt handler 的持久化在 `agent.Run` 返回之后才发生：`server.go:824-860`。

`DefaultFactory` 的 `envAgent.Close` 会关闭 plugins、MCP、memory：`internal/hostserver/runner.go:230-244`。

**影响：**

若 agent 实现允许 `Close` 与仍在收尾的 `Run` 并发，取消和关闭可能重叠，导致部分结果丢失、插件/资源在收尾前被关闭。现有注释要求 Close 在失败/取消后安全，但没有明确规定“Close 必须等待 Run 收尾”或“只能由 Run 所在 handler 调用”。

**建议：**

- 将 `cancel` 与 `Close` 分成两个阶段；
- 关闭会话时先 cancel，等 handler 完成持久化，再 Close；
- 或明确规定 Agent.Close 必须能与 Run 安全并发并加 race 测试；
- shutdown 的持久化失败应有可观察、可恢复的状态。

---

## 3. 中优先级缺陷

### M1. `hello` 的状态机不是原子的，且对空/旧版本缺校验

`internal/hostserver/server.go:499-535`：

- 没有拒绝 `Version <= 0`；
- 在确认 `already` 之前就设置 `authed = true`；
- 第二次 hello 被拒绝，但连接仍可使用。

复现：第二次 hello 返回 `hello has already been sent`，随后 `server.status` 仍正常返回。

建议：先检查“是否已 hello”与版本合法性，再提交状态；非法握手应关闭或明确保持拒绝状态。

### M2. 权限请求没有 turn ID，且超时配置未在 CLI 暴露

`PermissionRequest` 只有 session/request/tool/summary：`internal/hostapi/payloads.go:138-149`。多轮、重连、延迟 UI 下，request id 只能说明一次门请求，不能说明属于哪一轮。

`PermissionTimeout` 默认为 0：`internal/hostserver/server.go:61-63`；CLI 没有对应配置。断线拒绝已补上，但长期挂起的合法请求仍没有用户可配置的超时。

### M3. 持久化失败被降为诊断，但仍向客户端报告成功

`internal/hostserver/server.go:852-860` 对 `appendTail` 错误只 `diag.Diagf`；之后仍正常 reply。shutdown/磁盘错误时，用户可能看到“成功”，而实际没有完整落盘。

建议：结果中携带持久化状态，或失败时明确返回错误，并在 state 中记录可恢复信息。

### M4. POSIX 传输的 stale endpoint 检查存在用户误判窗口

`internal/hostserver/server.go:185-195`：先 `Probe`，失败后直接 `CleanEndpoint`。POSIX 上这通常可接受，但只覆盖“dial 失败”，没有 peer 身份/进程校验；若遇到超时、权限错误、路径错误，都可能被当成 stale。

建议：区分 `ENOENT/ECONNREFUSED` 与权限/超时；必要时核对 state、进程和锁，避免误删正在使用的 endpoint。

### M5. 同一 PIGO_HOME 的多个 pigo 进程没有统一的 writer 协调

session 写入通过 `atomicWrite` 的临时文件 + rename：`internal/session/session.go:433-463`。`AppendBranch` 是 load-modify-write：`session/session.go:788-810`。

CS 之后仍然存在同一 store 被 CLI、headless、host、SDK 并行写入的情况；没有 store/会话级互斥时，append 可能丢失或分支元数据不一致。建议统一由 host 持有写入，或增加文件锁/乐观并发版本检查。

### M6. 客户端缺少自动重连、幂等请求与恢复协议

`Client` 是单条连接，断开后 `Done/ReadErr` 结束：`internal/hostclient/client.go:311-337`。没有 reconnect 管理器，也没有 request id 的幂等语义；重试 `prompt` 可能在“host 已收到但响应未达”的情况下重复执行。

建议：明确请求幂等性，给 create/prompt/cancel 定义重试规则；把连接恢复与会话恢复拆开。

---

## 4. 协议与产品能力缺口

这些不一定是 bug，但会阻止 CS 成为真实主路径。

### G1. 当前协议只支持纯文本 prompt

`PromptParams` 只有 `SessionID` 和 `Text`：`internal/hostapi/payloads.go:101-107`，并明确图片不在 v1。现有本地运行支持图片、steering/follow-up 等能力，协议没有对应输入类型。

### G2. 缺少实时控制与配置管理

没有 `session.steer`、`session.followup`、`session.setModel`、`session.setThinking`、配置 reload、todo/memory/goal 状态查询等 API。结果是 attach 只能完成基本提问，不能替代 REPL/TUI。

### G3. TUI 与 headless 尚未接入 host

`docs/architecture.md:192` 已列为后续；`internal/cli/tui` 和默认 TUI 路径没有使用 `hostclient`。因此“单 host 多 TUI”目前应理解为协议/attach 层已经验证，不是 Bubble Tea TUI 已经支持。

### G4. 目录权限与 host 状态目录的信任模型需要明确

`--dir`/`PIGO_HOME` 可以选择任意状态目录。Windows 的 token 文件是“当前用户 DACL”，这不是跨用户隔离：如果状态目录位于其他用户可读的位置，token 仍可能被读到；随后该用户可能连接 pipe 并执行任意会话操作，包括 `server.shutdown`。建议默认状态目录必须在用户私有目录，或对自定义目录强制权限检查。

---

## 5. 架构分层评价

| 维度 | 评价 |
|---|---|
| 传输抽象 | 较好。`hostpath` 把 POSIX/Windows 差异收拢，客户端不依赖服务端装配。 |
| 协议边界 | 基础清晰，但缺少 turn/event 游标、能力协商和完整输入模型。 |
| 会话隔离 | 连接级所有权清晰；目录级授权与会话级语义冲突是主要破口。 |
| 生命周期 | 已有断开/关闭处理，但 creating/running/closing/shutdown 的提交协议不完整。 |
| 背压与可靠性 | 最薄弱：队列无界、单 dispatcher、同步写、panic 不隔离。 |
| 持久化 | 复用 session tree 是优点；失败可见性和多进程协调不足。 |
| 可观测性 | status 只有数量，缺少连接、turn、事件游标和错误原因。 |
| 测试 | 现有测试很丰富，但部分语义把缺陷固定成契约，需要补“跨客户端授权”“重连实时性”“慢消费者”测试。 |
| 平台一致性 | Windows 已可跑；POSIX 的 stale/peer 校验和共享状态目录仍需加强。 |

---

## 6. 建议的修复顺序

### 第一阶段：先封住安全与数据一致性

1. 明确并修复 `always` 的授权 scope，禁止默认跨连接扩散。
2. 统一 create/resume/shutdown 的会话提交协议，避免 agent 泄漏。
3. 明确 turn cancel、Close、persist 的先后顺序；持久化失败不能静默成功。
4. 修正 hello 状态机和版本校验。

### 第二阶段：把重连与背压做成协议能力

1. turnID + event seq + resume(lastSeq)。
2. 有界事件队列、慢消费者策略、权限事件独立通道。
3. callback recover、连接重试与请求幂等。
4. 普通请求/写操作的 deadline 和 writer queue。

### 第三阶段：补齐产品面并完成迁移

1. prompt 支持图片/内容块和 steering/follow-up。
2. 模型/thinking/配置/记忆/目标状态 API。
3. TUI 接入 hostclient；headless 增加 host 模式。
4. 明确默认 PIGO_HOME 的私有目录要求，补充 POSIX/Windows 双端集成测试。

---

## 7. 结论

当前 CS 的“骨架”是成立的，尤其是本地传输、按连接所有权、基本 turn/权限/落盘。但还不能把它当作成熟的多 UI 会话平台：

- 最危险的是**授权语义漂移**；
- 最影响用户体验的是**断线后只能等结果，不能恢复事件流**；
- 最容易在大流量/多会话下暴露的是**无界队列与同步写**；
- 最需要补齐的是**会话生命周期提交协议**和**现有 TUI/headless 的真实接入**。

建议先不要继续扩大前端接入面，先完成上述第一、二阶段，否则会把当前的权限和可靠性问题复制到 TUI、Web UI 和脚本端。
