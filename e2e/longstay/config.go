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
	MinReconnectDelay time.Duration
	MaxReconnectDelay time.Duration
	MinPromptDelay    time.Duration
	MaxPromptDelay    time.Duration
	SummaryInterval   time.Duration
	LLMLatency        time.Duration
	LLMJitter         time.Duration
	LLMErrorRate      float64
	OutputDir         string
	PigoBinary        string
}

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
	flag.DurationVar(&cfg.MinReconnectDelay, "min-reconnect-delay", 100*time.Millisecond, "minimum delay before reconnecting")
	flag.DurationVar(&cfg.MaxReconnectDelay, "max-reconnect-delay", 2*time.Second, "maximum delay before reconnecting")
	flag.DurationVar(&cfg.MinPromptDelay, "min-prompt-delay", 200*time.Millisecond, "minimum delay between prompts")
	flag.DurationVar(&cfg.MaxPromptDelay, "max-prompt-delay", 1500*time.Millisecond, "maximum delay between prompts")
	flag.DurationVar(&cfg.SummaryInterval, "summary-interval", 30*time.Second, "periodic console summary interval")
	flag.DurationVar(&cfg.LLMLatency, "llm-latency", 25*time.Millisecond, "base latency for every mock LLM request")
	flag.DurationVar(&cfg.LLMJitter, "llm-jitter", 75*time.Millisecond, "additional random latency for every mock LLM request")
	flag.Float64Var(&cfg.LLMErrorRate, "llm-error-rate", 0.01, "mock HTTP 500 rate, from 0 to 1")
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
	return nil
}
