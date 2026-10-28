package main

// Tests for wiring user-defined providers from config.toml / pigorc into the
// provider registry (the step that makes `--provider <name>` work for a
// third-party endpoint).

import (
	"testing"

	"github.com/smallnest/pigo/internal/cli/config"
	"github.com/smallnest/pigo/internal/provider"
)

// TestRegisterConfiguredProviders verifies a configured provider becomes a
// first-class registry entry: name lookup, model inference and the bare-name
// shorthand all work afterwards, and an invalid definition only warns.
func TestRegisterConfiguredProviders(t *testing.T) {
	registerConfiguredProviders([]config.ProviderConfig{{
		Name:     "maintest-gw",
		Protocol: "openai",
		BaseURL:  "https://gw.maintest.test/v1",
		EnvVar:   "MAINTEST_KEY",
		Models:   []string{"maintest-large", "maintest-small"},
	}})

	spec, ok := provider.LookupProviderSpec("maintest-gw")
	if !ok {
		t.Fatal("configured provider was not registered")
	}
	if spec.DefaultBaseURL != "https://gw.maintest.test/v1" || spec.Protocol != provider.ProtocolOpenAI {
		t.Errorf("spec = %+v, want the configured endpoint", spec)
	}
	if len(spec.EnvVars) != 1 || spec.EnvVars[0] != "MAINTEST_KEY" {
		t.Errorf("env vars = %v, want MAINTEST_KEY", spec.EnvVars)
	}
	if p, ok := provider.LookupPreset("maintest-large"); !ok || p.Provider != "maintest-gw" {
		t.Errorf("model not registered under the provider: %+v ok=%v", p, ok)
	}
	if got := provider.CanonicalizeModel("maintest-gw"); got != "maintest-large" {
		t.Errorf("bare provider name resolved to %q, want the first model", got)
	}

	// A bad definition warns (stderr) instead of aborting the CLI.
	registerConfiguredProviders([]config.ProviderConfig{{Name: ""}})
	registerConfiguredProviders([]config.ProviderConfig{{Name: "maintest-bad", Protocol: "grpc"}})
	if _, ok := provider.LookupProviderSpec("maintest-bad"); ok {
		t.Error("an invalid definition must not be registered")
	}
}

// TestRegisterConfiguredProvidersAnthropicDefault verifies an anthropic-protocol
// definition needs no base URL (the public API default applies).
func TestRegisterConfiguredProvidersAnthropicDefault(t *testing.T) {
	registerConfiguredProviders([]config.ProviderConfig{{
		Name:     "maintest-anth",
		Protocol: "anthropic",
	}})
	spec, ok := provider.LookupProviderSpec("maintest-anth")
	if !ok {
		t.Fatal("anthropic-protocol provider was not registered")
	}
	if spec.AuthScheme != provider.AuthXAPIKey {
		t.Errorf("auth scheme = %q, want x-api-key", spec.AuthScheme)
	}
}
