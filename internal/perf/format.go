// This file implements the number formatting the perf report uses. The report is
// read in a terminal and in bug reports, so the rules are deliberately plain: a
// duration always carries its unit, a byte count always carries a binary unit,
// a plain count is grouped ("1,841") so a four-digit call count is legible, and
// nothing is rounded so aggressively that two very different costs look equal.
package perf

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// writef is fmt.Fprintf with the result discarded: a report goes to a terminal
// (where a write error means the terminal is gone) or to a buffer, and neither
// has anything to recover from.
func writef(w io.Writer, format string, a ...any) {
	_, _ = fmt.Fprintf(w, format, a...)
}

// intString renders n with thousands separators: 1841 → "1,841".
func intString(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	if len(s) <= 3 {
		if neg {
			return "-" + s
		}
		return s
	}
	var b strings.Builder
	head := len(s) % 3
	if head > 0 {
		b.WriteString(s[:head])
	}
	for i := head; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// floatString renders a measurement with at most one decimal place, dropping a
// trailing ".0" so the duration column stays narrow: 10.04 → "10.0", 2.9 → "2.9".
func floatString(v float64) string {
	s := strconv.FormatFloat(v, 'f', 1, 64)
	return strings.TrimSuffix(s, ".0")
}

// formatPct renders a share of measured time, rounded to a whole percent above
// 10% and to one decimal below, where whole percents would hide the difference.
func formatPct(p float64) string {
	if p >= 10 {
		return strconv.FormatFloat(p, 'f', 0, 64) + "%"
	}
	return strconv.FormatFloat(p, 'f', 1, 64) + "%"
}

// sortedKeys returns a map's keys in lexical order, so a report's counter and
// gauge sections are stable between snapshots (map iteration order is not).
func sortedKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
