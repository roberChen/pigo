// This file implements the /status slash command (US-002, #292) that prints a
// colored multi-section status report with runtime config, context usage, and more.
//
// It reaches the session's collaborators and mutable state through the cli.Host
// contract (like /goal and /btw) rather than importing the concrete replDeps
// aggregate, keeping the dependency single-direction (repl→status).
package status

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/roberChen/pigo/internal/agentcore"
	"github.com/roberChen/pigo/internal/builtinskills"
	"github.com/roberChen/pigo/internal/cli"
	"github.com/roberChen/pigo/internal/cli/ui"
	"github.com/roberChen/pigo/internal/compaction"
	"github.com/roberChen/pigo/internal/provider"
	"github.com/roberChen/pigo/internal/runtime"
	"github.com/roberChen/pigo/internal/trust"
)

// RunStatus prints a colored multi-section status report to out using data
// read from host through the cli.Host accessors. It shows runtime config,
// context usage, project/environment, credentials, and telemetry.
func RunStatus(out io.Writer, host cli.Host) {
	color := ui.Enabled()

	fmt.Fprintln(out)
	printRuntimeConfig(out, color, host)
	fmt.Fprintln(out)
	printContextStatus(out, color, host)
	fmt.Fprintln(out)
	printEnvStatus(out, color, host)
	fmt.Fprintln(out)
	printCredentialsStatus(out, color, host)
	fmt.Fprintln(out)
	printTelemetryStatus(out, color, host)
}

// printRuntimeConfig prints the runtime model configuration section.
func printRuntimeConfig(out io.Writer, color bool, host cli.Host) {
	live := host.Live()
	header := host.Header()
	model := live.Model
	providerName := live.ProviderName
	baseURL := live.BaseURL
	protocol := live.Protocol
	thinkingLevel := string(live.ThinkingLevel)
	contextWindow := live.ContextWindow

	if model == "" {
		model = header.Model
	}
	if providerName == "" {
		providerName = header.Provider
	}
	if baseURL == "" {
		baseURL = "(default)"
	}
	if protocol == "" {
		protocol = "(default)"
	}
	if thinkingLevel == "" {
		thinkingLevel = "(default)"
	}

	fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Bold, "runtime config:"))
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "model:"), model)
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "provider:"), providerName)
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "base URL:"), baseURL)
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "protocol:"), protocol)
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "images:"), imageSupportLine(model, providerName))
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "thinking:"), thinkingLevel)
	if contextWindow > 0 {
		fmt.Fprintf(out, "  %s %d tokens\n", ui.Colorize(color, ui.Dim, "context window:"), contextWindow)
	} else {
		fmt.Fprintf(out, "  %s unknown\n", ui.Colorize(color, ui.Dim, "context window:"))
	}
}

// imageSupportLine explains whether the active model can receive pictures, and
// why. Image support is declared per model ([[models]] / `model add
// --supports-images`); a provider has no such setting, because one gateway
// serves both vision and text-only ids. The source is reported alongside the
// answer so "no" is actionable: a custom endpoint is text-only until its vision
// ids are declared, while a built-in model is assumed vision-capable.
func imageSupportLine(model, providerName string) string {
	if model == "" {
		model = "(unknown)"
	}
	supported, source := provider.ImageSupport(providerName, model)
	switch {
	case supported:
		return fmt.Sprintf("supported (%s)", source)
	case source == provider.ImageSupportDeclared:
		return "not supported (declared text-only); view_image/read report the image as text"
	case source == provider.ImageSupportCustomDefault:
		return "not supported (custom endpoint default); declare the vision ids with " +
			"`model add <id> --provider <name> --supports-images` in the pigorc"
	default:
		return fmt.Sprintf("not supported (%s)", source)
	}
}

// printContextStatus prints the current context usage and compaction section.
//
// The trigger figures come from the live config's resolved compaction plan
// (cli.LiveConfig.CompactionDetail), not from window-reserve recomputed here: the
// loop compacts on the model's input budget with the configured ladder, reserve
// and any absolute cap, so a report that ignored them would name a threshold the
// run never uses.
func printContextStatus(out io.Writer, color bool, host cli.Host) {
	msgs := host.AgentCtx().Messages
	tokens := compaction.EstimateContextTokens(msgs).Tokens
	contextWindow := host.Live().ContextWindow
	trigger := host.Live().CompactionDetail()

	compactions := 0
	for _, m := range msgs {
		if _, ok := m.(agentcore.CompactionMessage); ok {
			compactions++
		}
	}

	fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Bold, "context:"))
	fmt.Fprintf(out, "  %s %d / %d tokens\n", ui.Colorize(color, ui.Dim, "current:"), tokens, contextWindow)

	// Calculate utilization percentage if possible
	if contextWindow > 0 {
		utilization := int(float64(tokens) / float64(contextWindow) * 100)
		utilColor := ""
		if utilization >= 90 {
			utilColor = ui.Red
		} else if utilization >= 70 {
			utilColor = ui.Yellow
		} else {
			utilColor = ui.Green
		}
		fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "utilization:"), ui.Colorize(color, utilColor, fmt.Sprintf("%d%%", utilization)))
	} else {
		fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "utilization:"), ui.Colorize(color, ui.Yellow, "unknown"))
	}

	fmt.Fprintf(out, "  %s %d\n", ui.Colorize(color, ui.Dim, "compactions:"), compactions)

	// Where auto-compaction fires, and which rule put it there. The budget is
	// named explicitly when it is the model's input ceiling rather than its
	// combined window: the two differ for a "1M combined, 512K input" model, and
	// only the budget matters for whether the next request is accepted.
	switch {
	case trigger.Point <= 0 && contextWindow <= 0:
		fmt.Fprintf(out, "  %s %s\n",
			ui.Colorize(color, ui.Dim, "before compact:"),
			ui.Colorize(color, ui.Yellow, "auto-compaction disabled (unknown window)"),
		)
	case trigger.Point <= 0:
		fmt.Fprintf(out, "  %s %s\n",
			ui.Colorize(color, ui.Dim, "before compact:"),
			ui.Colorize(color, ui.Yellow, "auto-compaction disabled (compaction.enabled is off)"),
		)
	default:
		remaining := trigger.Point - tokens
		explain := triggerExplanation(trigger)
		if remaining < 0 {
			fmt.Fprintf(out, "  %s %s (%s)\n",
				ui.Colorize(color, ui.Dim, "before compact:"),
				ui.Colorize(color, ui.Red, fmt.Sprintf("%d over threshold", -remaining)),
				explain,
			)
		} else {
			fmt.Fprintf(out, "  %s %s (%s)\n",
				ui.Colorize(color, ui.Dim, "before compact:"),
				ui.Colorize(color, ui.Green, fmt.Sprintf("%d tokens remaining", remaining)),
				explain,
			)
		}
	}
}

// triggerExplanation says where the trigger is and why: the input budget it was
// measured against, the reserve kept free, and the cap that decided it. The source
// matters because "80% ladder" and "your compact_at" look identical as a number
// and differ entirely as an answer to "why did it compact here".
func triggerExplanation(d compaction.TriggerDetail) string {
	source := "the reserve"
	switch d.Source {
	case compaction.TriggerSourceLadder:
		source = fmt.Sprintf("the %s%% threshold ladder", humanizePercent(d.Fraction))
	case compaction.TriggerSourceConfigured:
		source = "your configured cap"
	}
	parts := []string{fmt.Sprintf("threshold: %d", d.Point), fmt.Sprintf("reserve: %d", d.Reserve)}
	if d.Budget > 0 && d.Budget != d.Window {
		parts = append(parts, fmt.Sprintf("input budget: %d", d.Budget))
	}
	return fmt.Sprintf("%s; decided by %s", strings.Join(parts, ", "), source)
}

// humanizePercent renders a ladder fraction as the integer percentage a config
// file would have written ("80%"), trimming a float's trailing zeros.
func humanizePercent(f float64) string {
	return strconv.FormatFloat(f*100, 'f', -1, 64)
}

// printEnvStatus prints the project & environment section: cwd, trust status,
// and counts of loaded skills and plugins (with names). User command templates
// are listed separately when present.
func printEnvStatus(out io.Writer, color bool, host cli.Host) {
	fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Bold, "project & environment:"))
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "cwd:"), host.Cwd())
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "trust:"), trustStatus(host.Trust(), host.Cwd()))

	var skills, plugins, userCmds []string
	// Skill names come from the loaded skill set: skills are selected through
	// /skill rather than registered as one slash command each.
	for _, s := range host.Skills() {
		if s != nil {
			skills = append(skills, s.Frontmatter.Name)
		}
	}
	if slash := host.Slash(); slash != nil {
		for _, c := range slash.List() {
			switch c.Source {
			case runtime.SourcePlugin:
				plugins = append(plugins, c.Name)
			case runtime.SourceUser:
				userCmds = append(userCmds, c.Name)
			}
		}
	}
	fmt.Fprintf(out, "  %s %d%s%s\n", ui.Colorize(color, ui.Dim, "skills:"), len(skills),
		skillSourceNote(host.Skills()), namesSuffix(skills))
	fmt.Fprintf(out, "  %s %d%s\n", ui.Colorize(color, ui.Dim, "plugins:"), len(plugins), namesSuffix(plugins))
	if len(userCmds) > 0 {
		fmt.Fprintf(out, "  %s %d%s\n", ui.Colorize(color, ui.Dim, "user commands:"), len(userCmds), namesSuffix(userCmds))
	}
}

// skillSourceNote says how many of the loaded skills are built in, since those are
// read-only files inside the binary (a same-named disk copy only wins when
// [skills] override_builtin lists it). It is empty when no built-in is loaded, so
// the common all-disk line reads exactly as before.
func skillSourceNote(skills []*runtime.Skill) string {
	var builtin int
	for _, s := range skills {
		if s != nil && builtinskills.IsVirtual(s.Path) {
			builtin++
		}
	}
	if builtin == 0 {
		return ""
	}
	return fmt.Sprintf(" (%d built-in, %d disk)", builtin, len(skills)-builtin)
}

// trustStatus classifies the cwd's trust state for display: disabled when trust
// is off, trusted when IsTrusted is true (session grant or saved Trusted),
// untrusted when a saved Untrusted decision applies, else prompt (undecided).
func trustStatus(mgr *trust.Manager, cwd string) string {
	if mgr == nil {
		return "disabled"
	}
	if mgr.IsTrusted(cwd) {
		return "trusted"
	}
	if res := mgr.NearestTrustDecision(cwd); res.Found && res.Decision == trust.Untrusted {
		return "untrusted"
	}
	return "prompt"
}

// namesSuffix renders " (n1, n2, ...)" for a non-empty name list, capping at 8
// names with "+k more". It returns "" for an empty list.
func namesSuffix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	const max = 8
	if len(names) <= max {
		return " (" + strings.Join(names, ", ") + ")"
	}
	return fmt.Sprintf(" (%s, +%d more)", strings.Join(names[:max], ", "), len(names)-max)
}

// printCredentialsStatus prints the credentials & connectivity section: API key
// presence (masked, never plaintext) and the provider endpoint URL.
func printCredentialsStatus(out io.Writer, color bool, host cli.Host) {
	fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Bold, "credentials & connectivity:"))
	live := host.Live()
	creds := host.Creds()
	provider := live.ProviderName
	if creds != nil && creds.HasCredential(context.Background(), provider) {
		key := creds.GetAPIKey(context.Background(), provider)
		fmt.Fprintf(out, "  %s %s %s\n",
			ui.Colorize(color, ui.Dim, "api key:"),
			ui.Colorize(color, ui.Green, "set"),
			ui.Colorize(color, ui.Dim, maskKey(key)))
	} else {
		fmt.Fprintf(out, "  %s %s\n",
			ui.Colorize(color, ui.Dim, "api key:"),
			ui.Colorize(color, ui.Yellow, "not set"))
	}
	endpoint := live.BaseURL
	if endpoint == "" {
		endpoint = "(default)"
	}
	fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "endpoint:"), endpoint)
}

// maskKey returns a masked hint of an API key showing only the last 4 chars
// (e.g. "••••abcd"). A key of 4 chars or fewer is masked entirely. It never
// returns the full key.
func maskKey(key string) string {
	const tail = 4
	r := []rune(key)
	if len(r) <= tail {
		return strings.Repeat("•", len(r))
	}
	return strings.Repeat("•", tail) + string(r[len(r)-tail:])
}

// printTelemetryStatus prints the telemetry section with two sub-blocks -
// cumulative (since session start) and last run - each showing turn count,
// truncation/compaction counts, context utilization, and a per-tool table.
// Both blocks show "no telemetry yet" before any run has completed.
func printTelemetryStatus(out io.Writer, color bool, host cli.Host) {
	fmt.Fprintf(out, "%s\n", ui.Colorize(color, ui.Bold, "telemetry:"))
	holder := host.Telemetry()
	if holder == nil || !holder.HasTelemetry() {
		fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "since session start:"), ui.Colorize(color, ui.Dim, "no telemetry yet"))
		fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "last run:"), ui.Colorize(color, ui.Dim, "no telemetry yet"))
		return
	}
	printTelemetryBlock(out, color, "since session start:",
		holder.CumulativeTurns(),
		holder.CumulativeTruncationCount(),
		holder.CumulativeCompactionCount(),
		holder.CumulativeContextUtilization(),
		holder.CumulativeToolDurations(),
	)
	if last := holder.Last(); last != nil {
		printTelemetryBlock(out, color, "last run:",
			last.Turns,
			last.TruncationCount,
			last.CompactionCount,
			last.ContextUtilization,
			last.ToolDurationsMs,
		)
	} else {
		fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Dim, "last run:"), ui.Colorize(color, ui.Dim, "no telemetry yet"))
	}
}

// printTelemetryBlock renders one telemetry sub-block (cumulative or last run).
func printTelemetryBlock(out io.Writer, color bool, label string, turns, trunc, compact int, util float64, tools map[string]agentcore.ToolTiming) {
	fmt.Fprintf(out, "  %s\n", ui.Colorize(color, ui.Cyan, label))
	fmt.Fprintf(out, "    %s %d\n", ui.Colorize(color, ui.Dim, "turns:"), turns)
	fmt.Fprintf(out, "    %s %d\n", ui.Colorize(color, ui.Dim, "truncations:"), trunc)
	fmt.Fprintf(out, "    %s %d\n", ui.Colorize(color, ui.Dim, "compactions:"), compact)
	fmt.Fprintf(out, "    %s %s\n", ui.Colorize(color, ui.Dim, "utilization:"), fmt.Sprintf("%.0f%%", util*100))
	if len(tools) == 0 {
		fmt.Fprintf(out, "    %s (none)\n", ui.Colorize(color, ui.Dim, "tools:"))
		return
	}
	names := make([]string, 0, len(tools))
	for n := range tools {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Fprintf(out, "    %s\n", ui.Colorize(color, ui.Dim, "tools:"))
	for _, n := range names {
		t := tools[n]
		fmt.Fprintf(out, "      %-12s %3d calls  %dms\n", n, t.Count, t.TotalMs)
	}
}
