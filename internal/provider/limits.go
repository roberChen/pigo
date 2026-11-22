// This file implements per-model token limits: the combined context window a
// provider advertises and, separately, the maximum *input* the model accepts.
//
// The two are not the same number. A gateway commonly advertises
// "max input + max output = W" (DeepSeek is the canonical example) while
// rejecting a request long before W is reached. Compacting at a fraction of W
// therefore overruns the real ceiling and the provider refuses the turn — a hard
// failure, not a soft one. Triggering on the input budget instead is the only
// safe reading, which is why this file models both numbers.
//
// The error that matters is asymmetric:
//
//   - OVER-estimating a window makes a request that is too large and is rejected.
//   - UNDER-estimating only compacts earlier than strictly necessary.
//
// So every default here is conservative, and a model whose limits are unknown
// falls back to the same 128K budget pigo has always used.
package provider

import (
	"strconv"
	"strings"
)

// ModelLimits is a model's token budgets, in tokens.
type ModelLimits struct {
	// ContextWindow is the advertised combined budget (input + output). 0 means
	// unknown.
	ContextWindow int
	// MaxInputTokens is the input-only ceiling when the provider publishes one
	// smaller than ContextWindow. 0 means "the window is the input budget".
	MaxInputTokens int
}

// Inferred reports whether the limits are a default rather than a statement from
// the user or the built-in table.
func (l ModelLimits) IsZero() bool {
	return l.ContextWindow <= 0 && l.MaxInputTokens <= 0
}

// InputBudget is the budget a request may actually carry. It is never larger
// than ContextWindow.
func (l ModelLimits) InputBudget() int {
	if l.ContextWindow <= 0 {
		return 0
	}
	if l.MaxInputTokens > 0 && l.MaxInputTokens < l.ContextWindow {
		return l.MaxInputTokens
	}
	return l.ContextWindow
}

// Normalize clamps the input ceiling to the window so a declaration can never
// describe an input budget larger than the model's total.
func (l ModelLimits) Normalize() ModelLimits {
	if l.ContextWindow < 0 {
		l.ContextWindow = 0
	}
	if l.MaxInputTokens < 0 {
		l.MaxInputTokens = 0
	}
	if l.MaxInputTokens > l.ContextWindow && l.ContextWindow > 0 {
		l.MaxInputTokens = l.ContextWindow
	}
	return l
}

// LimitsSource names where a model's limits came from, so a UI can explain the
// number instead of presenting a guess as fact.
type LimitsSource string

const (
	// LimitsDeclared means the user stated it ([[models]] / `model add`).
	LimitsDeclared LimitsSource = "declared"
	// LimitsBuiltin means pigo's built-in table knows this model or provider.
	LimitsBuiltin LimitsSource = "built-in table"
	// LimitsIdentified means the model id matched a known family (e.g. a Gemini
	// id on an unlisted provider), but not an exact catalog entry.
	LimitsIdentified LimitsSource = "identified from model id"
	// LimitsFallback means nothing identified the model and a conservative
	// default is in force.
	LimitsFallback LimitsSource = "conservative fallback"
)

// DefaultContextWindow is the budget assumed when nothing identifies a model. It
// is deliberately modest: overestimating causes rejected requests.
const DefaultContextWindow = 128_000

// providerLimits is the default a built-in provider's models are assumed to
// carry. Values are the conservative reading of each vendor's published limit.
var providerLimits = map[string]ModelLimits{
	"anthropic": {ContextWindow: 200_000},
	"openai":    {ContextWindow: 272_000}, // 400K combined; 272K is the input cap
	"google":    {ContextWindow: 1_000_000},
	// DeepSeek advertises 1M combined (input + output) but accepts far less
	// input; the split is the reason this field exists at all.
	"deepseek": {ContextWindow: 1_000_000, MaxInputTokens: 512_000},
	"xai":      {ContextWindow: 256_000},
	"groq":     {ContextWindow: 128_000},
	"cerebras": {ContextWindow: 128_000},
	"mistral":  {ContextWindow: 128_000},
	"zai":      {ContextWindow: 200_000},
	// Kimi's coding plans publish a 256K window.
	"kimi-coding": {ContextWindow: 256_000},
	"moonshotai":  {ContextWindow: 256_000},
	"minimax":     {ContextWindow: 200_000},
	"xiaomi":      {ContextWindow: 128_000},
	"nvidia":      {ContextWindow: 128_000},
	"fireworks":   {ContextWindow: 128_000},
	"together":    {ContextWindow: 128_000},
	"novita":      {ContextWindow: 128_000},
	"qianfan":     {ContextWindow: 128_000},
	"volcengine":  {ContextWindow: 128_000},
	"dashscope":   {ContextWindow: 128_000},
	"hunyuan":     {ContextWindow: 128_000},
	// A local runner's window depends on the loaded model and the host's memory;
	// the safe assumption is the conservative default.
	"ollama": {ContextWindow: 128_000},
}

// limitRule matches a model id substring to limits, so a family is covered
// without naming every release.
type limitRule struct {
	// provider is the provider the rule applies to; "" applies to any provider
	// (used for ids that identify themselves, like a "gemini" or "claude" id
	// served by an aggregator).
	provider string
	// contains is matched case-insensitively against the model id.
	contains string
	limits   ModelLimits
}

// builtinLimitRules are matched in order; the first match wins, so put the more
// specific rule first.
var builtinLimitRules = []limitRule{
	// A "-1m"/"1m" suffix on a Claude id marks the long-context variant.
	{provider: "anthropic", contains: "-1m", limits: ModelLimits{ContextWindow: 1_000_000}},
	{provider: "", contains: "claude", limits: ModelLimits{ContextWindow: 200_000}},
	// Gemini's rolling aliases and 1.5/2.x/3.x families are all 1M.
	{provider: "", contains: "gemini", limits: ModelLimits{ContextWindow: 1_000_000}},
	{provider: "", contains: "gpt-", limits: ModelLimits{ContextWindow: 272_000, MaxInputTokens: 272_000}},
	{provider: "", contains: "deepseek", limits: ModelLimits{ContextWindow: 1_000_000, MaxInputTokens: 512_000}},
	{provider: "", contains: "kimi", limits: ModelLimits{ContextWindow: 256_000}},
	{provider: "", contains: "glm", limits: ModelLimits{ContextWindow: 200_000}},
	{provider: "", contains: "qwen", limits: ModelLimits{ContextWindow: 128_000}},
	{provider: "", contains: "llama", limits: ModelLimits{ContextWindow: 128_000}},
	{provider: "", contains: "grok", limits: ModelLimits{ContextWindow: 256_000}},
	{provider: "", contains: "mistral", limits: ModelLimits{ContextWindow: 128_000}},
}

// modelLimits records per-model limit declarations made by a user, keyed by
// provider name and then model id. It mirrors modelImageSupport: a declaration is
// the only statement that can be trusted, and it applies to built-in and custom
// providers alike.
var modelLimits = map[string]map[string]ModelLimits{}

// ModelContextLimits returns the token limits for a model and where the answer
// came from.
//
// Precedence:
//
//  1. the user's declaration ([[models]] / `model add`),
//  2. the built-in table for the provider,
//  3. a family match on the model id (so an unlisted Gemini still gets 1M),
//  4. a conservative fallback.
//
// A user-defined (custom) endpoint gets the conservative fallback unless it
// declares otherwise: its window cannot be introspected, and guessing high turns
// an unknown into a rejected request.
func ModelContextLimits(providerName, model string) (ModelLimits, LimitsSource) {
	providerName = strings.ToLower(strings.TrimSpace(providerName))
	model = strings.TrimSpace(model)
	if providerName == "" && model == "" {
		return ModelLimits{ContextWindow: DefaultContextWindow}, LimitsFallback
	}
	if declared, ok := declaredModelLimits(providerName, model); ok {
		return declared, LimitsDeclared
	}
	if IsCustomProvider(providerName) {
		return ModelLimits{ContextWindow: DefaultContextWindow}, LimitsFallback
	}
	if limits, ok := providerLimits[providerName]; ok {
		// A family rule may still describe a specific id more precisely than the
		// provider default (a 1M Claude on a 200K provider).
		if limits, ok := matchLimitRule(providerName, model); ok {
			return limits, LimitsIdentified
		}
		return limits, LimitsBuiltin
	}
	if limits, ok := matchLimitRule(providerName, model); ok {
		return limits, LimitsIdentified
	}
	return ModelLimits{ContextWindow: DefaultContextWindow}, LimitsFallback
}

// matchLimitRule returns the first id-based rule that matches.
func matchLimitRule(providerName, model string) (ModelLimits, bool) {
	lower := strings.ToLower(model)
	for _, rule := range builtinLimitRules {
		if rule.provider != "" && rule.provider != providerName {
			continue
		}
		if strings.Contains(lower, rule.contains) {
			return rule.limits, true
		}
	}
	return ModelLimits{}, false
}

// declaredModelLimits returns the limits a user declared for one model, and
// whether anything was declared at all. A routed id ("mycorp/mycorp-vl") names
// the same model as its bare tail, so either spelling is accepted.
func declaredModelLimits(providerName, model string) (ModelLimits, bool) {
	if providerName == "" || model == "" {
		return ModelLimits{}, false
	}
	registryMu.RLock()
	defer registryMu.RUnlock()
	decls := modelLimits[providerName]
	if len(decls) == 0 {
		return ModelLimits{}, false
	}
	if v, ok := decls[model]; ok {
		return v.Normalize(), true
	}
	if i := strings.LastIndexByte(model, '/'); i >= 0 {
		if v, ok := decls[model[i+1:]]; ok {
			return v.Normalize(), true
		}
	}
	return ModelLimits{}, false
}

// ParseTokenCount parses a token count written the way a config file writes one:
// a plain integer ("300000"), a K/M-suffixed count ("128K", "1.5M",
// case-insensitive), or an empty string meaning "not stated" (0, ok=false).
// Suffixes are powers of 1000, matching the venders' convention.
func ParseTokenCount(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	mult := 1.0
	lower := strings.ToLower(s)
	switch {
	case strings.HasSuffix(lower, "k"):
		mult, s = 1_000, s[:len(s)-1]
	case strings.HasSuffix(lower, "m"):
		mult, s = 1_000_000, s[:len(s)-1]
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || f <= 0 {
		return 0, false
	}
	return int(f*mult + 0.5), true
}

// FormatTokenCount renders a token count compactly for a UI ("128K", "1M").
func FormatTokenCount(n int) string {
	if n <= 0 {
		return "0"
	}
	switch {
	case n%1_000_000 == 0:
		return strconv.Itoa(n/1_000_000) + "M"
	case n%1_000 == 0:
		return strconv.Itoa(n/1_000) + "K"
	default:
		return strconv.Itoa(n)
	}
}
