#!/usr/bin/env bash
# 冒烟失败时开 issue；同一天的重复失败改成在原 issue 下追加评论。
#
# 用法: soak-failure-issue.sh <report.md> [--title <标题>] [--label <标签>]
#
# 环境变量（GitHub Actions 自动提供）：
#   GH_TOKEN           具备 issues:write 的 token
#   GITHUB_REPOSITORY  owner/repo
#   GITHUB_SERVER_URL  默认 https://github.com
#   GITHUB_OUTPUT      设置后写入 issue / url / created
#
# 去重靠标题里的北京时间日期（标题形如 "soak: 冒烟失败 2026-11-19"）：当天还开着
# 就追加评论，保留时间线；已经关掉则重开一条——上一个问题被判定修好后又复发，值得
# 独立记录。gh 不可用或缺 token 时打一行错误并非零退出，让上报步骤自身可见失败，
# 而不是把"提 issue 失败"伪装成成功。
set -euo pipefail

report=${1:-}
shift || true
title_prefix="soak: 冒烟失败"
label="ci-failure"

while [ $# -gt 0 ]; do
	case $1 in
	--title) title_prefix=${2:-}; shift 2 ;;
	--label) label=${2:-}; shift 2 ;;
	*) echo "soak-failure-issue: 未知参数 $1" >&2; exit 2 ;;
	esac
done

if [ ! -f "$report" ]; then
	echo "soak-failure-issue: 报告文件 $report 不存在" >&2
	exit 2
fi

: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY 未设置}"
server=${GITHUB_SERVER_URL:-https://github.com}
repo_url="$server/$GITHUB_REPOSITORY"

# 北京时间：定时任务在北京时间早上触发，用 UTC 会把早上的失败算到前一个自然日。
today=$(TZ=Asia/Shanghai date +%Y-%m-%d)
title="$title_prefix $today（北京）"
body=$(cat "$report")

if ! command -v gh >/dev/null 2>&1; then
	echo "soak-failure-issue: gh CLI 不可用" >&2
	exit 1
fi

# find_issue 打印标题完全等于 $title 的 open issue 编号（没有则空）。
#
# 不在 --search 里塞标题：标题含中文、括号和空格，GitHub 的 issue 搜索会把它当成
# 多个查询词，命中与否取决于分词，靠不住。这里取一页 open issue 再在本地精确比对
# ——每天最多一条，列表很短，代价可以忽略。
find_issue() {
	gh issue list \
		--repo "$GITHUB_REPOSITORY" \
		--state open \
		--limit 100 \
		--json number,title \
		--jq ".[] | select(.title == \"$title\") | .number" 2>/dev/null | head -n1
}

existing=$(find_issue || true)

if [ -n "$existing" ]; then
	echo "追加评论到 issue #$existing（$title）"
	gh issue comment "$existing" --repo "$GITHUB_REPOSITORY" --body "$body" >/dev/null
	{
		echo "issue=$existing"
		echo "created=false"
		echo "url=$repo_url/issues/$existing"
	} >>"${GITHUB_OUTPUT:-/dev/null}"
	exit 0
fi

# 标签可能不存在（fork、或仓库还没建这个标签）：带标签失败时去掉标签重试，而不是
# 让整个上报因为一个装饰性参数而失败。
if ! gh issue create \
	--repo "$GITHUB_REPOSITORY" \
	--title "$title" \
	--body "$body" \
	--label "$label" >/dev/null 2>&1; then
	echo "带标签 '$label' 创建失败，改为无标签重试" >&2
	gh issue create \
		--repo "$GITHUB_REPOSITORY" \
		--title "$title" \
		--body "$body" >/dev/null
fi

number=$(find_issue || true)
echo "已创建 issue #${number}: $repo_url/issues/${number}"
{
	echo "issue=${number}"
	echo "created=true"
	echo "url=$repo_url/issues/${number}"
} >>"${GITHUB_OUTPUT:-/dev/null}"
