# pigo 一对多长稳测试（longstay）

`e2e/longstay` 是独立的端到端可靠性测试命令，**不属于 Go test 框架**。它启动真实的 `pigo serve` 守护进程、真实的 OpenAI 兼容 mock LLM server 和多个真实 `hostclient` 连接，并持续执行：

- 新客户端随时加入；
- 客户端在 turn 开始前、流式输出中、权限/结果返回前随时退出；
- 断线后随时重连；
- 运行中的会话只读恢复并续传事件，turn 结束后恢复写权限；
- 多客户端并发 prompt、mock LLM 延迟、抖动和可配置 5xx；
- `turnId` / `seq` 连续性检查、跨会话串流检查、最终回复一致性检查；
- 工具往返：mock 按确定性计划发出 read/grep/find 调用，客户端校验工具的**真实执行结果**（结果里必须带上只存在于该文件中的标记），从而证明 turn 里的工具调用和回放历史都真实可用；
- host 健康检查、最终重连验证和优雅关闭。

### 校验模型：一份计划，两个进程各自推导

mock LLM 的回复是 prompt 中标记 token 的纯函数：`[[longstay:<token>]]` 决定这条回复的 thinking、正文、以及 0～2 轮工具调用。客户端从**同一个 token** 独立推导出一份计划（`plan.go`），再拿实际收到的 `turn_end` 消息和工具结果逐阶段比对，因此双方不需要共享任何可变状态：

- 正文/thinking 必须逐字节等于计划，错位或缺字都会记录 `stage_*_mismatch`；
- 工具调用的名称、参数必须等于计划，工具结果必须包含该文件独有的标记；
- 最终重连时用**持久化 transcript** 再校验一遍：每条 assistant 消息都必须是它所回答的那条 prompt 的计划阶段。

mock 还会对每个 Chat Completions 请求做严格校验（`validate.go`）：允许的字段集、工具集合、call/result 配对、批次完整性、`tool_call_id` 可重放等。违反契约会返回 HTTP 400 并计为协议错误——这是 host 侧缺陷的直接证据，而不是被 mock 默默容忍。

注入的 500 是**预期故障**：mock 记录自己注入过哪些 token，pigo 的修复路径（把工具输出降级为 abnormal 占位并重试一次）会让该 turn 提前结束，这种情况计入 `turnsDegraded` / `degradedResults`，不会被误判为计划不符。反过来，没有注入却返回错误则记为 `unexpected_provider_error`。

Windows 使用真实命名管道，POSIX 使用真实 unix socket；不会用内存 transport 替代。

## 运行

在仓库根目录执行（Go 1.27+，已通过 `go.mod` 的 `tool` 指令注册）：

```bash
# 默认运行 1 小时
go tool longstay

# 30 秒 smoke，用于验证环境
go tool longstay -duration 30s -summary-interval 5s -output e2e-results/smoke

# 24 小时长稳，带 2% 上游错误和确定性场景
go tool longstay -duration 24h -llm-error-rate 0.02 -seed 42

# 使用已经构建好的 pigo，而不是自动构建 ./cmd/pigo
go tool longstay -duration 2h -pigo ./pigo.exe
```

也可以绕过 `go tool`，使用等价的 `go run ./e2e/longstay`。`go tool` 会列出 `longstay (github.com/roberChen/pigo/e2e/longstay)`；它仍然是一个普通命令，不会被 `go test ./...` 执行。

默认会构建当前源码中的 `./cmd/pigo`，并将它放入本次产物目录。Ctrl-C / SIGTERM 会先停止客户端，执行最终重连验证，再优雅关闭 host；不会把正常停止计为故障。

## 主要参数

| 参数 | 默认值 | 含义 |
|---|---:|---|
| `-duration` | `1h` | 持续时间，支持 `30s`、`2h`、`24h` |
| `-seed` | 当前时间 | 确定性场景种子，便于复现 |
| `-initial-clients` | `4` | 启动时客户端数 |
| `-max-clients` | `16` | 客户端槽位上限，运行中持续加入新客户端直到达到上限 |
| `-min-client-lifetime` / `-max-client-lifetime` | `2s` / `12s` | 每次连接寿命范围，可能在 turn 中途到期 |
| `-min-reconnect-delay` / `-max-reconnect-delay` | `100ms` / `2s` | 断开后的重连等待范围 |
| `-min-prompt-delay` / `-max-prompt-delay` | `200ms` / `1.5s` | turn 之间的等待范围 |
| `-llm-latency` / `-llm-jitter` | `25ms` / `75ms` | 每次 LLM 请求的基础延迟和随机增量 |
| `-llm-error-rate` | `0.01` | mock HTTP 500 比例，属于预期故障，不算协议失败 |
| `-scenario` | `normal` | `normal` 给 host 只读工具集，turn 包含真实工具往返；`stress` 用 `--no-tools` 跑单阶段 turn，专注生命周期抖动 |
| `-workspace-pool` | `0` | 大于 0 时让多个客户端共享该数量的工作目录（同一目录里的两个 session）；`0` 表示每个客户端独立目录 |
| `-summary-interval` | `30s` | 控制台摘要间隔 |
| `-pigo` | 空 | 使用指定 pigo 二进制；为空则自动构建 |

## 产物

每次运行使用独立目录（默认 `e2e-results/longstay-<timestamp>`）：

| 文件 | 内容 |
|---|---|
| `config.json` | 完整参数，便于复现 |
| `events.jsonl` | 客户端加入/退出、session 创建/接管、prompt、mock 请求和错误事件；不包含全量流式文本 |
| `server.log` | 真实 pigo host 的 stdout/stderr |
| `summary.json` | 机器可读最终指标与 `pass` 结果 |
| `home/` | host 的临时状态目录、session 持久化文件 |
| `work/client-*` | 各客户端工作目录；启动前预置 `notes.txt` / `log.txt` / `data.txt`，内含工具计划要读的标记 |

摘要包含客户端启动/重连/接管次数、并发峰值、prompt 完成/取消数、mock 请求/5xx、协议/传输/host 错误、重复事件、未修复事件缺口、队列溢出、回放截断以及 goroutine/heap 的起止采样。计划校验另有独立计数：`toolVerifications`（通过标记校验的工具结果）、`turnsVerified` / `turnsUnverified`（按计划完整跑完 / 无法解释的提前结束）、`turnsDegraded` 与 `degradedResults`（由注入 500 引起的预期降级）、`transcriptsVerified`（最终 transcript 校验次数）。`verifications` 是实际执行的最终重连次数，`verificationSkips` 则记录没有持久化历史、无需重连的客户端及原因。连接失败会先进行有界重试，健康检查只有连续失败才计为 host 故障；健康 RPC 和关闭 RPC 都有超时，测试框架自身不会因 host 卡住而无限等待。

**安全提示：** 产物目录中的 `home/server.token` 可用于连接本次 host，session 文件还可能包含用户提示词。长稳结束后只应把脱敏后的 `summary.json` 和必要的错误片段用于 CI/问题报告，不要直接公开整个产物目录。

## 通过条件

同时满足：

1. 协议错误、非预期传输错误、host 错误、未修复事件缺口、回放截断均为 0；
2. 至少发生一次 prompt 和一次断线重连；
3. 并发客户端数大于 1；
4. mock LLM 确实收到请求；
5. 最终重连验证与优雅关闭完成。

进程退出码：`0` 通过，`1` 可靠性失败或启动失败，`2` 参数错误。`providerErrors`、`mockFailures`、`turnsDegraded` 和 `degradedResults` 是 `-llm-error-rate` 注入的预期上游错误，不直接导致失败；`queueOverflows` 会被记录为背压指标，客户端仍应以最终结果和恢复事件为准。

`normal` 场景建议先用短跑确认工具计划生效（`toolVerifications` 与 `turnsVerified` 均大于 0）；`stress` 场景 `mockRequests` 应约等于 `prompts`，因为每个 turn 只有一次 LLM 请求。
