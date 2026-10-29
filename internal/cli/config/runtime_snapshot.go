// This file implements the runtime snapshot that the pigo_config tool reports.
// Its fields are the values a driver resolves from CLI flags and config.toml that
// no single configuration layer owns: the effective endpoint, reasoning effort,
// output format, approval mode, and the resolved memory/checkpoint/compaction
// settings. The driver hands the snapshot to the run (run.Env.Report) so the tool
// reports what that run actually uses, rather than re-deriving it and risking a
// different answer — and so two Sessions in one process cannot shadow each other.
package config

import (
	"strconv"
	"strings"
)

// RuntimeSnapshot is the resolved settings a run reports about itself.
type RuntimeSnapshot struct {
	// Model is the canonicalized model id the run streams against.
	Model string
	// Provider is the resolved provider name (a built-in, or one defined in
	// config.toml). Empty means "let the run report the name it resolved".
	Provider string
	// Protocol is the wire protocol in effect (openai | anthropic, or a
	// variant such as openai/resp_api). Empty when the provider owns it.
	Protocol string
	// BaseURL is the effective endpoint when it is known, or "" when the provider
	// uses its own default.
	BaseURL string
	// Thinking is the effective reasoning-effort level.
	Thinking string
	// OutputFormat is text or stream-json.
	OutputFormat string
	// Approve is whether side-effect tools run without per-call confirmation.
	Approve bool
	// NoTools and NoSkills record whether the run disabled those subsystems.
	NoTools  bool
	NoSkills bool
	// Memory holds the resolved [memory]/[checkpoint]/[compaction] tables.
	Memory MemorySettings
}

// MemorySummary renders the resolved memory/checkpoint/compaction settings as
// three short labeled lines, so the configuration report does not need access to
// the unexported parsing state behind MaxContext. An unset compaction cap renders
// as "unset".
func (s MemorySettings) MemorySummary() []string {
	mem := "memory      "
	if s.Memory.Enabled {
		mem += "on"
	} else {
		mem += "off"
	}
	mem += "  reconcile-on-search=" + boolWord(s.Memory.ReconcileOnSearch) +
		"  score-floor=" + trimFloat(s.Memory.SearchScoreFloor)
	if s.Memory.CCIndex {
		mem += "  cc-index=on"
	}
	lines := []string{mem}

	ck := "checkpoint  "
	if len(s.CheckpointThresholds) == 0 {
		ck += "no thresholds"
	} else {
		parts := make([]string, 0, len(s.CheckpointThresholds))
		for _, t := range s.CheckpointThresholds {
			parts = append(parts, trimFloat(t))
		}
		ck += "thresholds=" + strings.Join(parts, ",")
	}
	if s.CheckpointReserved.Set {
		if s.CheckpointReserved.IsInt {
			ck += "  reserved=" + strconv.Itoa(s.CheckpointReserved.Int)
		} else {
			ck += "  reserved=" + s.CheckpointReserved.Str
		}
	}
	lines = append(lines, ck)

	cp := "compaction  "
	if s.MaxContext.IsSet() {
		cp += "max-context=" + strconv.Itoa(s.MaxContext.Resolve(0)) + " tokens"
	} else {
		cp += "max-context=unset (model window)"
	}
	lines = append(lines, cp)
	return lines
}

// boolWord renders a bool the way the report reads best.
func boolWord(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// trimFloat renders a float without a trailing ".0", so 0.5 stays 0.5 and 1.0
// becomes 1.
func trimFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
