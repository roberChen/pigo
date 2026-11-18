#!/usr/bin/env bash
# 生成冒烟/长稳失败的报告正文（Markdown），写 stdout。
#
# 用法: soak-report.sh --summary <summary.json> --run-url <url> [选项]
#   --summary    longstay 产物目录里的 summary.json（缺失时正文里注明）
#   --run-url    Actions run 链接
#   --run-id     Actions run id
#   --commit     触发本次运行的 commit sha
#   --ref        分支/tag 名
#   --duration   请求的长稳时长（如 2h）
#   --failed     失败的步骤名，逗号分隔
#   --log        逐字附加到报告末尾的日志片段文件（会截断）
#   --repro      复现命令
#
# 只做渲染，不调用网络：这样正文可以在本地用 summary.json 直接检查，也让
# soak-failure-issue.sh 保持"决定是否开 issue"的单一职责。
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

summary=""
run_url=""
run_id=""
commit=""
ref=""
duration=""
failed=""
log_file=""
repro=""

while [ $# -gt 0 ]; do
	case $1 in
	--summary) summary=${2:-}; shift 2 ;;
	--run-url) run_url=${2:-}; shift 2 ;;
	--run-id) run_id=${2:-}; shift 2 ;;
	--commit) commit=${2:-}; shift 2 ;;
	--ref) ref=${2:-}; shift 2 ;;
	--duration) duration=${2:-}; shift 2 ;;
	--failed) failed=${2:-}; shift 2 ;;
	--log) log_file=${2:-}; shift 2 ;;
	--repro) repro=${2:-}; shift 2 ;;
	*) echo "soak-report: 未知参数 $1" >&2; exit 2 ;;
	esac
done

# 日志最多附这么多行：issue 正文有大小上限（65536 字符），而长稳的日志动辄几十
# 万行。超出的部分留在 Actions artifact 里，报告只给"最像根因的尾巴"。
LOG_LINES=120

if [ -n "$run_url" ]; then
	echo "冒烟在 **${run_url}** 失败。"
else
	echo "冒烟失败。"
fi
echo

echo "| 项目 | 值 |"
echo "|---|---|"
[ -n "$failed" ] && echo "| 失败步骤 | \`$failed\` |"
[ -n "$duration" ] && echo "| 请求时长 | \`$duration\` |"
[ -n "$ref" ] && echo "| 分支 | \`$ref\` |"
if [ -n "$commit" ]; then
	echo "| Commit | \`${commit:0:12}\` |"
fi
[ -n "$run_id" ] && echo "| Run ID | \`$run_id\` |"
echo

echo "### 本轮指标"
echo
"$here/soak-summary.sh" "$summary"
echo

if [ -n "$log_file" ] && [ -s "$log_file" ]; then
	echo "<details><summary>日志尾部（最后 ${LOG_LINES} 行）</summary>"
	echo
	echo '```'
	tail -n "$LOG_LINES" "$log_file"
	echo '```'
	echo
	echo "</details>"
	echo
fi

if [ -n "$repro" ]; then
	echo "### 本地复现"
	echo
	echo '```bash'
	echo "$repro"
	echo '```'
	echo
fi

echo "### 处理方式"
echo
echo "- 产物（\`server.log\`、\`events.jsonl\`、\`summary.json\`、\`home/\`）在本次 run 的 artifact 里，"
echo "  保留 14 天；先看 \`summary.json\` 里非零的 \`protocolErrors\` / \`transportErrors\` / \`hostErrors\`。"
echo "- \`providerErrors\` / \`mockFailures\` / \`turnsDegraded\` 是 \`-llm-error-rate\` 注入的**预期**故障，"
echo "  不要把它们当作回归证据。"
echo "- 同一天重复失败会在本 issue 下追加评论，而不是新开一条。"
