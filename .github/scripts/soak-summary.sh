#!/usr/bin/env bash
# 把 longstay 的 summary.json 渲染成 Markdown 指标表。
#
# 用法: soak-summary.sh <summary.json>
#
# 输出直接拼进 $GITHUB_STEP_SUMMARY，也被 report-soak.sh 复用为 issue 正文的
# 「本轮指标」小节。summary.json 不存在时输出一行说明而不是报错：冒烟可能死在
# 构建或单元测试阶段，那时长稳根本没跑起来，而 issue 仍然要开。
set -euo pipefail

summary="${1:-}"

if [ -z "$summary" ] || [ ! -f "$summary" ]; then
	echo "_未生成 summary.json（长稳可能没有启动或提前崩溃）。_"
	exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
	echo "_缺少 jq，无法解析 summary.json。_"
	exit 0
fi

# 固定顺序、只输出存在的键：字段集会随 harness 演进，这里不追着改。把整份
# summary 绑到 $o，才能一边遍历键名、一边回读它的值（$o[.] 里的 . 是键）。
jq -r '
	. as $o
	| "| 指标 | 值 |", "|---|---|",
	  (["pass", "duration", "scenario", "seed", "clientsStarted", "peakClients",
	    "prompts", "completed", "aborted", "reconnects", "adoptions",
	    "providerErrors", "mockFailures", "protocolErrors", "transportErrors",
	    "hostErrors", "duplicates", "missingEvents", "queueOverflows",
	    "replayTruncated", "verifications", "verificationSkips",
	    "turnsVerified", "turnsUnverified", "turnsDegraded", "turnsCompacted",
	    "modelSwitchesVerified", "modelSwitchTurnsWrong",
	    "toolVerifications", "transcriptsVerified", "taskChildren",
	    "taskFailedChildren", "mockRequests", "endGoroutines", "endHeapBytes"][]
	   | select($o[.] != null)
	   | "| `\(.)` | \($o[.]) |")
' "$summary"
