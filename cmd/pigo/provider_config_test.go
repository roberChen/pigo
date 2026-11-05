package main

// Tests for wiring user-defined providers from config.toml / pigorc into the
// provider registry (the step that makes `--provider <name>` work for a
// third-party endpoint).

import (
	"strings"
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
	}}, nil)

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
	registerConfiguredProviders([]config.ProviderConfig{{Name: ""}}, nil)
	registerConfiguredProviders([]config.ProviderConfig{{Name: "maintest-bad", Protocol: "grpc"}}, nil)
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
	}}, nil)
	spec, ok := provider.LookupProviderSpec("maintest-anth")
	if !ok {
		t.Fatal("anthropic-protocol provider was not registered")
	}
	if spec.AuthScheme != provider.AuthXAPIKey {
		t.Errorf("auth scheme = %q, want x-api-key", spec.AuthScheme)
	}
}

// TestRegisterConfiguredProvidersDeclaredModels verifies the model declarations
// from config.toml / pigorc reach the registry: a model can be declared without
// being listed on its provider, its vision capability gates the model
// (ModelSupportsImages is what view_image consults), and a declaration for an
// unknown provider warns instead of registering a half-defined model.
func TestRegisterConfiguredProvidersDeclaredModels(t *testing.T) {
	vision := true
	text := false
	messages := registerConfiguredProviders(
		[]config.ProviderConfig{{
			Name:     "maintest-models",
			Protocol: "openai",
			BaseURL:  "https://gw.maintest.test/v1",
			// A custom endpoint is text-only by default; the model declaration
			// below opts one id in without changing the endpoint.
		}},
		[]config.ModelConfig{
			{ID: "maintest-vl", Provider: "maintest-models", DisplayName: "VL", SupportsImages: &vision},
			{ID: "maintest-plain", Provider: "maintest-models", SupportsImages: &text},
			{ID: "maintest-orphan", Provider: "maintest-nope", SupportsImages: &vision},
		},
	)
	t.Cleanup(func() { registerConfiguredProviders(nil, nil) })

	if !provider.ModelSupportsImages("maintest-models", "maintest-vl") {
		t.Error("a declared vision model did not accept images")
	}
	if provider.ModelSupportsImages("maintest-models", "maintest-plain") {
		t.Error("a model declared text-only accepted images")
	}
	if provider.ModelSupportsImages("maintest-models", "maintest-undeclared") {
		t.Error("an undeclared model must inherit the provider's text-only default")
	}
	// A model declared without being in the provider's `models` list is still
	// selectable, which is the point of splitting the two tables.
	if p, ok := provider.LookupPreset("maintest-vl"); !ok || p.Provider != "maintest-models" {
		t.Errorf("declared model not selectable: %+v ok=%v", p, ok)
	}
	if _, ok := provider.LookupProviderSpec("maintest-nope"); ok {
		t.Error("a model declaration must not create the provider it names")
	}
	var sawOrphan bool
	for _, m := range messages {
		if strings.Contains(m, "maintest-nope") {
			sawOrphan = true
		}
	}
	if !sawOrphan {
		t.Errorf("messages = %v, want a warning naming the unknown provider", messages)
	}
}
