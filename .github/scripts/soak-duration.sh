#!/usr/bin/env bash
# 校验长稳时长不低于下限，把 Go duration 换算成秒。
#
# 用法: soak-duration.sh <duration> [min] [max]
#   duration  Go duration 语法，允许 h / m / s 及其组合（2h30m、150m、7200s、2.5h）
#   min       下限，默认 2h
#   max       上限，默认空（不限制）
#
# 通过时打印请求的秒数并退出 0，越界或无法解析时打印原因并以 1 退出。
# 冒烟的意义在于"跑得够久"，所以下限放在脚本里而不是靠 workflow 的默认值——手动
# 触发时改小 -duration 一样会被拦下来。上限用来挡住 GitHub 跑不完的时长：单个 job
# 最长 6 小时，请求 24h 只会被硬杀，连 summary 都写不出来。
set -euo pipefail

dur="${1:-}"
min="${2:-2h}"
max="${3:-}"

# to_seconds 把 Go 风格的时长换算成整数秒。
#
# 只接受 h / m / s：Go 也认 ms / us / ns，但按毫秒定义的长稳没有意义，宁可报错
# 也不要静默地把 7200ms 当成"两小时以下"的合法输入。裸数字在 Go 里是纳秒，
# 这里同样拒绝，避免把 7200 误读成 7200 秒。
to_seconds() {
	local d=$1
	local total=0
	local num unit mult

	[ -n "$d" ] || return 1

	while [ -n "$d" ]; do
		if [[ $d =~ ^([0-9]+(\.[0-9]+)?)([hms])(.*)$ ]]; then
			num=${BASH_REMATCH[1]}
			unit=${BASH_REMATCH[3]}
			d=${BASH_REMATCH[4]}
		else
			return 1
		fi
		case $unit in
		h) mult=3600 ;;
		m) mult=60 ;;
		s) mult=1 ;;
		esac
		# 小数分量交给 awk，避免 bash 只能做整数运算。
		total=$((total + $(awk -v n="$num" -v m="$mult" 'BEGIN { printf "%d", n * m + 0.5 }')))
	done

	echo "$total"
}

want=$(to_seconds "$min") || {
	echo "无法解析下限 $min（只支持 h/m/s，例如 2h）" >&2
	exit 1
}
got=$(to_seconds "$dur") || {
	echo "无法解析时长 '$dur'（只支持 h/m/s，例如 2h30m、150m、7200s）" >&2
	exit 1
}

if [ "$got" -lt "$want" ]; then
	echo "时长 '$dur' = ${got}s 低于下限 $min = ${want}s" >&2
	exit 1
fi

if [ -n "$max" ]; then
	limit=$(to_seconds "$max") || {
		echo "无法解析上限 $max（只支持 h/m/s，例如 5h30m）" >&2
		exit 1
	}
	if [ "$got" -gt "$limit" ]; then
		echo "时长 '$dur' = ${got}s 超过上限 $max = ${limit}s（单个 job 跑不完）" >&2
		exit 1
	fi
fi

echo "$got"
