#!/usr/bin/env bash
# .github/scripts 下辅助脚本的自测。不联网、不依赖 GitHub Actions 环境。
#
# 用法: .github/scripts/selftest.sh
#
# 这些脚本决定"冒烟失败会不会真的开出一条 issue"，所以在本地把它们当成普通
# 程序测：duration 守卫的边界、报告正文的渲染、以及用假 gh 覆盖 issue 的上报
# 分支（新建 / 追加 / 标签缺失回退）。
set -uo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

pass=0
fail=0

ok() {
	pass=$((pass + 1))
	printf '  ok   %s\n' "$1"
}

no() {
	fail=$((fail + 1))
	printf '  FAIL %s\n' "$1"
	[ -n "${2:-}" ] && printf '       %s\n' "$2"
}

# expect_rc <want> <name> -- <command...>
expect_rc() {
	local want=$1 name=$2
	shift 3
	local out rc
	out=$("$@" 2>&1)
	rc=$?
	if [ "$rc" = "$want" ]; then
		ok "$name"
	else
		no "$name" "rc=$rc want=$want; $out"
	fi
}

echo "== soak-duration.sh =="
expect_rc 0 "10m 恰好等于 10m 下限" -- "$here/soak-duration.sh" 10m
expect_rc 0 "600s 等于下限" -- "$here/soak-duration.sh" 600s
expect_rc 0 "15m 满足下限" -- "$here/soak-duration.sh" 15m
expect_rc 0 "2h 满足下限" -- "$here/soak-duration.sh" 2h
expect_rc 1 "9m59s 低于下限" -- "$here/soak-duration.sh" 9m59s
expect_rc 1 "30s 低于下限" -- "$here/soak-duration.sh" 30s
expect_rc 1 "空值被拒" -- "$here/soak-duration.sh" ""
expect_rc 1 "裸数字被拒（避免把 600 当纳秒静默放过）" -- "$here/soak-duration.sh" 600
expect_rc 1 "ms 单位被拒" -- "$here/soak-duration.sh" 600000ms
expect_rc 1 "垃圾输入被拒" -- "$here/soak-duration.sh" ten-minutes
expect_rc 0 "5h 在 10m..6h 内" -- "$here/soak-duration.sh" 5h 10m 6h
expect_rc 1 "超过上限被拒（24h 跑不完）" -- "$here/soak-duration.sh" 24h 10m 6h
expect_rc 1 "下限仍然优先于上限" -- "$here/soak-duration.sh" 5m 10m 6h
expect_rc 0 "不给上限时不限制" -- "$here/soak-duration.sh" 9h 10m
expect_rc 0 "定时下限：2h 恰好达标" -- "$here/soak-duration.sh" 2h 2h 5h
expect_rc 1 "定时下限：10m 不足以算长稳" -- "$here/soak-duration.sh" 10m 2h 5h
expect_rc 1 "定时下限：1h59m59s 差一秒也拒绝" -- "$here/soak-duration.sh" 1h59m59s 2h 5h

# 换算结果本身要正确，否则"低于下限"的判断毫无意义。
if [ "$("$here/soak-duration.sh" 2h30m | tail -n1)" = "9000" ]; then
	ok "2h30m = 9000s"
else
	no "2h30m = 9000s" "got $("$here/soak-duration.sh" 2h30m | tail -n1)"
fi
if [ "$("$here/soak-duration.sh" 600s | tail -n1)" = "600" ]; then
	ok "600s = 600"
else
	no "600s = 600" "got $("$here/soak-duration.sh" 600s | tail -n1)"
fi
if [ "$("$here/soak-duration.sh" 2.5h | tail -n1)" = "9000" ]; then
	ok "2.5h = 9000s"
else
	no "2.5h = 9000s" "got $("$here/soak-duration.sh" 2.5h | tail -n1)"
fi

echo "== soak.yml 定时契约 =="
# 定时触发没有 inputs，那个兜底值就是它实际跑的时长；下限则是防回退的保险。
# 这两行是「每天至少跑满 2 小时」的唯一实现，改小必须是一次有意识的修改。
soak_yml="$here/../workflows/soak.yml"
if grep -Fq "SOAK_DURATION: \${{ inputs.duration || '2h' }}" "$soak_yml"; then
	ok "定时兜底时长是 2h"
else
	no "定时兜底时长是 2h" "soak.yml 里没有 inputs.duration || '2h'"
fi
if grep -Fq "SOAK_MIN_DURATION: \${{ github.event_name == 'schedule' && '2h' || '10m' }}" "$soak_yml"; then
	ok "定时下限是 2h，改小会当场失败"
else
	no "定时下限是 2h，改小会当场失败" "soak.yml 里没有 schedule 的 2h 下限"
fi

echo "== soak-summary.sh =="
cat >"$tmp/summary.json" <<'JSON'
{
  "pass": false,
  "duration": "2h0m0s",
  "scenario": "normal",
  "seed": 42,
  "prompts": 900,
  "completed": 880,
  "protocolErrors": 0,
  "transportErrors": 0,
  "hostErrors": 2,
  "missingEvents": 1,
  "providerErrors": 7
}
JSON
out=$("$here/soak-summary.sh" "$tmp/summary.json" 2>&1)
if printf '%s' "$out" | grep -q '| `hostErrors` | 2 |'; then
	ok "渲染出 hostErrors 行"
else
	no "渲染出 hostErrors 行" "$out"
fi
if printf '%s' "$out" | grep -q '| 指标 | 值 |'; then
	ok "输出 Markdown 表头"
else
	no "输出 Markdown 表头" "$out"
fi
# 字段顺序固定，便于人眼对比不同日期的报告。
if [ "$(printf '%s' "$out" | grep -o '`pass`\|`duration`\|`prompts`' | tr '\n' ' ')" = '`pass` `duration` `prompts` ' ]; then
	ok "字段顺序稳定"
else
	no "字段顺序稳定" "$(printf '%s' "$out" | head -5)"
fi
if [ "$("$here/soak-summary.sh" "$tmp/nope.json")" != "" ]; then
	ok "缺文件时给出说明而非报错"
else
	no "缺文件时给出说明而非报错"
fi
expect_rc 0 "缺文件时退出码为 0" -- "$here/soak-summary.sh" "$tmp/nope.json"

echo "== soak-report.sh =="
echo "line one of log" >"$tmp/soak.log"
echo "panic: boom" >>"$tmp/soak.log"
"$here/soak-report.sh" \
	--summary "$tmp/summary.json" \
	--run-url "https://example.invalid/run/1" \
	--commit abcdef1234567890 \
	--ref master \
	--duration 2h \
	--failed "Setup Go,Longstay soak" \
	--log "$tmp/soak.log" \
	--repro "go tool longstay -duration 2h" >"$tmp/report.md" 2>"$tmp/report.err"
rc=$?
if [ "$rc" = 0 ]; then ok "渲染报告成功"; else no "渲染报告成功" "$(cat "$tmp/report.err")"; fi
for needle in 'https://example.invalid/run/1' '`Setup Go,Longstay soak`' '`2h`' '`abcdef123456`' 'panic: boom' 'go tool longstay -duration 2h' '| `hostErrors` | 2 |'; do
	if grep -qF "$needle" "$tmp/report.md"; then
		ok "报告包含 $needle"
	else
		no "报告包含 $needle" "$(head -20 "$tmp/report.md")"
	fi
done
# 未知参数必须报错退出，否则拼错选项会静默生成一份缺字段的报告。
expect_rc 2 "未知参数被拒" -- "$here/soak-report.sh" --nope x

echo "== soak-failure-issue.sh（假 gh） =="
# 一个最小的 gh 替身：把每次调用的子命令记到 $GH_LOG，并用 $GH_STATE 里的数字
# 模拟"当前 open issue 编号"。
mkdir -p "$tmp/bin"
cat >"$tmp/bin/gh" <<'SH'
#!/usr/bin/env bash
echo "$*" >>"$GH_LOG"
case $1 in
issue)
	case $2 in
	list)
		# 已有一条 open issue 时才返回编号，模拟 jq 精确比对的结果。
		if [ -s "$GH_STATE" ]; then printf '%s\n' "$(head -n1 "$GH_STATE")"; fi
		;;
	create)
		# GH_CREATE_FAIL=label 模拟"标签不存在"：带 --label 的第一次失败，重试成功。
		if [ "${GH_CREATE_FAIL:-}" = "label" ] && printf '%s ' "$@" | grep -q -- '--label'; then
			echo "label not found" >&2
			exit 1
		fi
		printf '7\n' >"$GH_STATE"
		printf '%s\n' "$(printf '%s ' "$@" | sed 's/.*--title //; s/ --body.*//')" >"$GH_TITLE"
		;;
	comment)
		printf '%s\n' "$*" >"$GH_COMMENT"
		;;
	esac
	;;
esac
exit 0
SH
chmod +x "$tmp/bin/gh"

run_issue() { # <state-empty|7> [extra env assignments...]
	: >"$tmp/gh.log"
	rm -f "$tmp/gh.title" "$tmp/gh.comment"
	: >"$tmp/gh.state"
	if [ "$1" = "7" ]; then printf '7\n' >"$tmp/gh.state"; fi
	shift
	# GITHUB_OUTPUT 每轮清空：真实 Actions 里每次 run 都是新文件，不清空会把上一轮
	# 的 created=true 带进来，让断言假通过。
	: >"$tmp/out"
	GH_LOG="$tmp/gh.log" GH_STATE="$tmp/gh.state" GH_TITLE="$tmp/gh.title" \
		GH_COMMENT="$tmp/gh.comment" PATH="$tmp/bin:$PATH" GITHUB_REPOSITORY="acme/pigo" \
		GITHUB_OUTPUT="$tmp/out" TZ=Asia/Shanghai "$@" "$here/soak-failure-issue.sh" "$tmp/report.md" 2>&1
}

out=$(run_issue empty env)
if grep -q 'issue create' "$tmp/gh.log"; then
	ok "没有当日 issue 时新建"
else
	no "没有当日 issue 时新建" "$(cat "$tmp/gh.log")"
fi
if grep -q "^issue=7$" "$tmp/out" && grep -q '^created=true$' "$tmp/out"; then
	ok "新建后写出 issue/created 输出"
else
	no "新建后写出 issue/created 输出" "$(cat "$tmp/out")"
fi
today=$(TZ=Asia/Shanghai date +%Y-%m-%d)
if grep -q "soak: 冒烟失败 $today（北京）" "$tmp/gh.title"; then
	ok "标题用北京时间日期"
else
	no "标题用北京时间日期" "$(cat "$tmp/gh.title")"
fi

out=$(run_issue 7 env)
if [ -s "$tmp/gh.comment" ] && grep -q 'issue comment 7' "$tmp/gh.comment"; then
	ok "当日已有 issue 时追加评论"
else
	no "当日已有 issue 时追加评论" "$(cat "$tmp/gh.comment" 2>/dev/null)"
fi
if grep -q '^created=false$' "$tmp/out"; then
	ok "追加评论时 created=false"
else
	no "追加评论时 created=false" "$(cat "$tmp/out")"
fi
if grep -q 'issue create' "$tmp/gh.log"; then
	no "追加评论时不应再新建"
else
	ok "追加评论时不再新建"
fi

out=$(run_issue empty env GH_CREATE_FAIL=label)
if [ "$(grep -c 'issue create' "$tmp/gh.log")" = "2" ] && grep -q "issue create --repo acme/pigo --title" "$tmp/gh.log"; then
	ok "标签缺失时无标签重试"
else
	no "标签缺失时无标签重试" "$(cat "$tmp/gh.log")"
fi

# 缺 GITHUB_REPOSITORY 必须炸，而不是对着空仓库瞎调 gh。
# 必须用 env -u 显式清掉：GitHub Actions 总会注入 GITHUB_REPOSITORY，不清理的话
# 这条断言在 CI 上会因为「变量其实存在」而失败，本地却通过——自测本身必须密闭。
out=$(env -u GITHUB_REPOSITORY GH_LOG="$tmp/gh.log" PATH="$tmp/bin:$PATH" \
	"$here/soak-failure-issue.sh" "$tmp/report.md" 2>&1)
if [ $? -ne 0 ]; then
	ok "缺 GITHUB_REPOSITORY 时非零退出"
else
	no "缺 GITHUB_REPOSITORY 时非零退出" "$out"
fi

# 报告文件不存在是调用方 bug，要用参数错误退出码（2）区分于运行失败。
expect_rc 2 "报告文件缺失时 rc=2" -- env GITHUB_REPOSITORY=acme/pigo PATH="$tmp/bin:$PATH" \
	"$here/soak-failure-issue.sh" "$tmp/missing.md"

echo
echo "通过 $pass，失败 $fail"
[ "$fail" -eq 0 ]
