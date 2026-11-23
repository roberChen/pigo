// Package main implements the long-stay host/client reliability harness. It is
// deliberately a command, not a Go test: runs may last hours and emit JSONL
// evidence instead of test assertions.
package main

import (
	"errors"
	"flag"
	"fmt"
	"time"
)

// Config controls one long-stay run. Durations use Go's duration syntax.
type Config struct {
	Duration          time.Duration
	Seed              int64
	InitialClients    int
	MaxClients        int
	ActionInterval    time.Duration
	MinClientLifetime time.Duration
	MaxClientLifetime time.Duration
	// CallTimeout bounds one non-prompt RPC from a client. Resuming a long
	// session is real work for the host — read the JSONL, replay events — and on a
	// two-core CI runner under -race with a dozen clients it can take seconds.
	// The default matches hostclient's own budget (30s) so a slow runner is not
	// reported as a protocol violation; a host that is actually wedged is still
	// caught, by the health checks and the wall-clock guard.
	CallTimeout       time.Duration
	MinReconnectDelay time.Duration
	MaxReconnectDelay time.Duration
	MinPromptDelay    time.Duration
	MaxPromptDelay    time.Duration
	SummaryInterval   time.Duration
	LLMLatency        time.Duration
	LLMJitter         time.Duration
	LLMErrorRate      float64
	// Scenario selects what the run exercises. "normal" enables the configured
	// real-tool plans so turns include tool round-trips; "stress" runs with
	// --no-tools so every turn is a single stage and the host spends its
	// time on lifecycle churn instead.
	Scenario string
	// Tools is the comma-separated planned catalog. Dependency groups are added
	// automatically when any member is selected.
	Tools               string
	AvgToolCalls        float64
	CallsPerStage       int
	ThinkingBlocks      int
	RequireTaskCoverage bool
	// ModelSwitchEvery makes a session switch to the next model in the rotation
	// after that many prompts, exercising the in-session model change (and the
	// compaction plan re-resolution it must carry). 0 disables switching.
	ModelSwitchEvery int
	// WorkspacePool, when greater than zero, makes clients share that many
	// workspaces (two sessions in one directory); 0 gives each client its own.
	WorkspacePool int
	OutputDir     string
	PigoBinary    string
}

// Scenario names accepted by -scenario.
const (
	ScenarioNormal = "normal"
	ScenarioStress = "stress"
)

// toolsEnabled reports whether this scenario lets the model call tools. The mock
// LLM and the clients derive their plans from the same answer, so they must not
// disagree with the host's flags.
func (c Config) toolsEnabled() bool { return c.Scenario != ScenarioStress }

// parseConfig reads command-line options. The defaults intentionally create
// frequent reconnects while leaving enough time for real turns to complete.
func parseConfig() Config {
	cfg := Config{}
	flag.DurationVar(&cfg.Duration, "duration", time.Hour, "how long to run (for example 30s, 2h, 24h)")
	flag.Int64Var(&cfg.Seed, "seed", time.Now().UnixNano(), "deterministic scenario seed")
	flag.IntVar(&cfg.InitialClients, "initial-clients", 4, "clients connected at startup")
	flag.IntVar(&cfg.MaxClients, "max-clients", 16, "maximum client slots; new clients join until the cap is reached")
	flag.DurationVar(&cfg.ActionInterval, "action-interval", 100*time.Millisecond, "delay between per-client state-machine steps")
	flag.DurationVar(&cfg.MinClientLifetime, "min-client-lifetime", 2*time.Second, "minimum connected lifetime before a client exits")
	flag.DurationVar(&cfg.MaxClientLifetime, "max-client-lifetime", 12*time.Second, "maximum connected lifetime before a client exits")
	flag.DurationVar(&cfg.CallTimeout, "call-timeout", 30*time.Second, "per-RPC timeout for non-prompt client calls (resume, create, cancel)")
	flag.DurationVar(&cfg.MinReconnectDelay, "min-reconnect-delay", 100*time.Millisecond, "minimum delay before reconnecting")
	flag.DurationVar(&cfg.MaxReconnectDelay, "max-reconnect-delay", 2*time.Second, "maximum delay before reconnecting")
	flag.DurationVar(&cfg.MinPromptDelay, "min-prompt-delay", 200*time.Millisecond, "minimum delay between prompts")
	flag.DurationVar(&cfg.MaxPromptDelay, "max-prompt-delay", 1500*time.Millisecond, "maximum delay between prompts")
	flag.DurationVar(&cfg.SummaryInterval, "summary-interval", 30*time.Second, "periodic console summary interval")
	flag.DurationVar(&cfg.LLMLatency, "llm-latency", 25*time.Millisecond, "base latency for every mock LLM request")
	flag.DurationVar(&cfg.LLMJitter, "llm-jitter", 75*time.Millisecond, "additional random latency for every mock LLM request")
	flag.Float64Var(&cfg.LLMErrorRate, "llm-error-rate", 0.01, "mock HTTP 500 rate, from 0 to 1")
	flag.StringVar(&cfg.Scenario, "scenario", ScenarioNormal, "normal (configured real tools and multi-round turns) or stress (no tools, lifecycle churn only)")
	flag.StringVar(&cfg.Tools, "tools", "read,read_window,grep,find,bash,write,edit,view_image,webfetch,todo,memory_search,question,bash_jobs,task_sync,task_error,task_async,task_output_list,task_batch,task_wait_all,task_auto,schedule_create,schedule_list,schedule_delete", "comma-separated planned tool catalog; dependency groups are automatic")
	flag.Float64Var(&cfg.AvgToolCalls, "avg-tool-calls", 3, "average tool calls per turn (0 disables tool stages)")
	flag.IntVar(&cfg.CallsPerStage, "calls-per-stage", 1, "tool calls per assistant stage, from 1 to 8")
	flag.IntVar(&cfg.ThinkingBlocks, "thinking-blocks", 2, "number of stages that carry thinking blocks per turn")
	flag.BoolVar(&cfg.RequireTaskCoverage, "require-task-coverage", false, "fail unless the selected task plans produce verified child-agent evidence")
	flag.IntVar(&cfg.ModelSwitchEvery, "model-switch-every", 2, "switch a session to the next model in the rotation after that many prompts; 0 disables")
	flag.IntVar(&cfg.WorkspacePool, "workspace-pool", 0, "clients per workspace: >0 shares that many directories, 0 gives each client its own")
	flag.StringVar(&cfg.OutputDir, "output", "", "artifact directory (default e2e-results/longstay-<timestamp>)")
	flag.StringVar(&cfg.PigoBinary, "pigo", "", "path to an existing pigo binary; default builds ./cmd/pigo into the artifact directory")
	flag.Parse()
	return cfg
}

// validate rejects configurations that cannot exercise the lifecycle correctly.
func (c Config) validate() error {
	if c.Duration <= 0 {
		return errors.New("duration must be positive")
	}
	if c.InitialClients < 1 || c.MaxClients < c.InitialClients {
		return fmt.Errorf("clients must satisfy 1 <= initial-clients <= max-clients")
	}
	if c.ActionInterval <= 0 || c.SummaryInterval <= 0 {
		return errors.New("action-interval and summary-interval must be positive")
	}
	if c.MinClientLifetime <= 0 || c.MaxClientLifetime < c.MinClientLifetime {
		return errors.New("client lifetimes must satisfy 0 < min <= max")
	}
	if c.CallTimeout <= 0 {
		return fmt.Errorf("call-timeout must be positive")
	}
	if c.MinReconnectDelay < 0 || c.MaxReconnectDelay < c.MinReconnectDelay {
		return errors.New("reconnect delays must satisfy 0 <= min <= max")
	}
	if c.MinPromptDelay < 0 || c.MaxPromptDelay < c.MinPromptDelay {
		return errors.New("prompt delays must satisfy 0 <= min <= max")
	}
	if c.LLMLatency < 0 || c.LLMJitter < 0 {
		return errors.New("LLM latency and jitter must be non-negative")
	}
	if c.LLMErrorRate < 0 || c.LLMErrorRate > 1 {
		return errors.New("llm-error-rate must be between 0 and 1")
	}
	if c.Scenario != ScenarioNormal && c.Scenario != ScenarioStress {
		return fmt.Errorf("scenario %q must be %q or %q", c.Scenario, ScenarioNormal, ScenarioStress)
	}
	if c.WorkspacePool < 0 {
		return errors.New("workspace-pool must be non-negative")
	}
	if c.ModelSwitchEvery < 0 {
		return errors.New("model-switch-every must be non-negative")
	}
	if c.AvgToolCalls < 0 || c.AvgToolCalls > 32 {
		return errors.New("avg-tool-calls must be between 0 and 32")
	}
	if c.CallsPerStage < 1 || c.CallsPerStage > 8 {
		return errors.New("calls-per-stage must be between 1 and 8")
	}
	if c.ThinkingBlocks < 0 || c.ThinkingBlocks > 32 {
		return errors.New("thinking-blocks must be between 0 and 32")
	}
	if c.RequireTaskCoverage && !c.toolsEnabled() {
		return errors.New("require-task-coverage cannot be used with the no-tools stress scenario")
	}
	return configureProfile(c)
}
