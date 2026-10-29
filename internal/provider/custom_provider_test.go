package provider

// Tests for third-party (custom) provider definitions: registration makes a
// user-defined endpoint behave like a built-in provider (name resolution, model
// inference, preset picker), and credential resolution keeps inline keys below the
// environment.

import (
	"context"
	"strings"
	"testing"
)

// registerTestProvider registers a custom provider and fails the test on error.
func registerTestProvider(t *testing.T, spec ProviderSpec, models []PresetModel, apiKey string) {
	t.Helper()
	if err := RegisterCustomProvider(spec, models, apiKey); err != nil {
		t.Fatalf("RegisterCustomProvider(%q): %v", spec.Name, err)
	}
	t.Cleanup(func() { unregisterTestProvider(spec.Name) })
}

// unregisterTestProvider removes a test provider from the registry so tests do not
// leak state into each other. The name is normalized the same way registration
// normalizes it (lowercase, trimmed), so a mixed-case definition is removed too.
func unregisterTestProvider(name string) {
	name = strings.ToLower(strings.TrimSpace(name))
	registryMu.Lock()
	defer registryMu.Unlock()
	delete(providerRegistryByName, name)
	delete(customProviderKeys, name)
	out := providerRegistry[:0]
	for _, spec := range providerRegistry {
		if spec.Name != name {
			out = append(out, spec)
		}
	}
	providerRegistry = out
	var providers []PresetProvider
	for _, p := range PresetProviders {
		if p.Name != name {
			providers = append(providers, p)
		}
	}
	PresetProviders = providers
	var catalog []PresetModel
	for _, m := range PresetCatalog {
		if m.Provider != name {
			catalog = append(catalog, m)
		}
	}
	PresetCatalog = catalog
}

// TestRegisterCustomProviderBehavesLikeBuiltin verifies a registered provider is
// reachable through every path a built-in is: name lookup, --provider resolution,
// model-id inference, the bare-name shorthand and the preset picker.
func TestRegisterCustomProviderBehavesLikeBuiltin(t *testing.T) {
	registerTestProvider(t, ProviderSpec{
		Name:           "testcorp-gw",
		Protocol:       ProtocolOpenAI,
		DefaultBaseURL: "https://llm.testcorp.test/v1",
		EnvVars:        []string{"TESTCORP_KEY"},
	}, []PresetModel{{ID: "testcorp-large"}, {ID: "testcorp-small"}}, "inline-key")

	spec, ok := LookupProviderSpec("testcorp-gw")
	if !ok {
		t.Fatal("custom provider not found in the registry")
	}
	if spec.Protocol != ProtocolOpenAI || spec.DefaultBaseURL != "https://llm.testcorp.test/v1" ||
		len(spec.EnvVars) != 1 || spec.EnvVars[0] != "TESTCORP_KEY" || spec.AuthScheme != AuthBearer {
		t.Fatalf("spec = %+v, want the definition (bearer auth default)", spec)
	}
	if !containsString(ProviderNames(), "testcorp-gw") {
		t.Error("ProviderNames should list the custom provider")
	}

	// Model ids resolve to the custom provider, and the bare name selects the
	// first declared model.
	if p, ok := LookupPreset("testcorp-large"); !ok || p.Provider != "testcorp-gw" {
		t.Fatalf("LookupPreset = %+v ok=%v, want provider testcorp-gw", p, ok)
	}
	if got := CanonicalizeModel("testcorp-gw"); got != "testcorp-large" {
		t.Errorf("CanonicalizeModel(bare name) = %q, want the first model", got)
	}
	if !IsProviderName("testcorp-gw") {
		t.Error("IsProviderName should accept the custom provider")
	}

	// Resolving by model id (no --provider) picks the custom provider.
	_, name, err := ResolveProvider("testcorp-large", "", "", "", func(string) string { return "" })
	if err != nil {
		t.Fatalf("ResolveProvider by model id: %v", err)
	}
	if name != "testcorp-gw" {
		t.Errorf("resolved provider = %q, want testcorp-gw", name)
	}

	// Resolving an explicit --provider builds a driver against the custom endpoint.
	p, name, err := ResolveProvider("any-model", "", "", "testcorp-gw", func(k string) string {
		if k == "TESTCORP_KEY" {
			return "env-key"
		}
		return ""
	})
	if err != nil {
		t.Fatalf("ResolveProvider by name: %v", err)
	}
	if name != "testcorp-gw" || p == nil {
		t.Fatalf("ResolveProvider by name = (%v, %q), want a provider", p, name)
	}

	// The preset picker sees it too.
	found := false
	for _, pv := range PresetProvidersSnapshot() {
		if pv.Name == "testcorp-gw" && pv.EnvVar == "TESTCORP_KEY" {
			found = true
		}
	}
	if !found {
		t.Error("PresetProvidersSnapshot should include the custom provider")
	}
}

// TestCustomProviderCredentialPrecedence verifies the key chain for a custom
// provider: an inline key works, and the environment still wins over it.
func TestCustomProviderCredentialPrecedence(t *testing.T) {
	registerTestProvider(t, ProviderSpec{
		Name:           "testcorp-cred",
		Protocol:       ProtocolOpenAI,
		DefaultBaseURL: "https://cred.testcorp.test/v1",
	}, nil, "inline-key")

	store := NewCredentialStore(nil)
	if got := store.GetAPIKey(context.Background(), "testcorp-cred"); got != "inline-key" {
		t.Errorf("GetAPIKey = %q, want the inline key", got)
	}
	t.Setenv("TESTCORP_CRED_API_KEY", "env-key")
	if got := store.GetAPIKey(context.Background(), "testcorp-cred"); got != "env-key" {
		t.Errorf("GetAPIKey = %q, want the environment to win", got)
	}
}

// TestCustomProviderAnthropicProtocol verifies an anthropic-protocol definition
// needs no base URL (the public API default) and defaults to x-api-key auth.
func TestCustomProviderAnthropicProtocol(t *testing.T) {
	registerTestProvider(t, ProviderSpec{
		Name:     "testcorp-anth",
		Protocol: ProtocolAnthropic,
	}, []PresetModel{{ID: "claude-custom"}}, "")

	spec, ok := LookupProviderSpec("testcorp-anth")
	if !ok {
		t.Fatal("custom anthropic provider not registered")
	}
	if spec.AuthScheme != AuthXAPIKey {
		t.Errorf("AuthScheme = %q, want x-api-key", spec.AuthScheme)
	}
	if spec.DefaultBaseURL != "" {
		t.Errorf("DefaultBaseURL = %q, want empty (public API default)", spec.DefaultBaseURL)
	}
	p, name, err := ResolveProvider("claude-custom", "", "", "", func(string) string { return "" })
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	if name != "testcorp-anth" || p == nil {
		t.Fatalf("resolved = (%v, %q), want the custom anthropic provider", p, name)
	}
}

// TestRegisterCustomProviderValidation verifies the definitions that must be
// rejected: a missing/invalid name, an unknown protocol, a missing base URL, and
// a name that collides with a built-in provider.
func TestRegisterCustomProviderValidation(t *testing.T) {
	cases := []struct {
		name string
		spec ProviderSpec
	}{
		{"empty name", ProviderSpec{}},
		{"invalid characters", ProviderSpec{Name: "test corp/gw", DefaultBaseURL: "https://x/v1"}},
		{"unknown protocol", ProviderSpec{Name: "testcorp-proto", Protocol: "grpc", DefaultBaseURL: "https://x/v1"}},
		{"missing base url", ProviderSpec{Name: "testcorp-nourl", Protocol: ProtocolOpenAI}},
		{"builtin collision", ProviderSpec{Name: "openai", DefaultBaseURL: "https://x/v1"}},
	}
	for _, tc := range cases {
		err := RegisterCustomProvider(tc.spec, nil, "")
		if err == nil {
			t.Errorf("%s: expected an error", tc.name)
		} else if err.Error() == "" {
			t.Errorf("%s: empty error message", tc.name)
		}
	}

	// Re-registering a custom name replaces it instead of failing, so a config
	// reload can change a provider definition without a restart.
	registerTestProvider(t, ProviderSpec{Name: "testcorp-dupe", DefaultBaseURL: "https://x/v1"}, nil, "")
	if err := RegisterCustomProvider(ProviderSpec{Name: "testcorp-dupe", DefaultBaseURL: "https://y/v1"}, nil, ""); err != nil {
		t.Fatalf("re-registering a custom provider: %v", err)
	}
	spec, ok := LookupProviderSpec("testcorp-dupe")
	if !ok {
		t.Fatal("re-registered provider is missing")
	}
	if spec.DefaultBaseURL != "https://y/v1" {
		t.Errorf("DefaultBaseURL = %q, want the replacement definition", spec.DefaultBaseURL)
	}
}

// TestUnregisterCustomProvider verifies a custom provider can be removed (a
// reload after the definition is deleted) while a built-in name is never touched.
func TestUnregisterCustomProvider(t *testing.T) {
	registerTestProvider(t, ProviderSpec{Name: "testcorp-gone", DefaultBaseURL: "https://x/v1"}, []PresetModel{{ID: "testcorp-gone-model"}}, "inline-key")
	if !UnregisterCustomProvider("testcorp-gone") {
		t.Fatal("UnregisterCustomProvider reported no removal")
	}
	if _, ok := LookupProviderSpec("testcorp-gone"); ok {
		t.Error("the provider spec should be gone")
	}
	if _, ok := LookupPreset("testcorp-gone-model"); ok {
		t.Error("its models should be gone from the catalog")
	}
	for _, p := range PresetProviders {
		if p.Name == "testcorp-gone" {
			t.Error("its preset entry should be gone")
		}
	}
	if UnregisterCustomProvider("openai") {
		t.Error("a built-in provider must never be unregistered")
	}
	if UnregisterCustomProvider("testcorp-gone") {
		t.Error("removing an absent provider should report false")
	}
}

// TestRegisterCustomProviderNormalizesName verifies a mixed-case name is accepted
// and stored lowercased, so --provider lookups are case-insensitive by
// construction.
func TestRegisterCustomProviderNormalizesName(t *testing.T) {
	registerTestProvider(t, ProviderSpec{Name: "TestCorp-Mixed", DefaultBaseURL: "https://x/v1"}, nil, "")
	if _, ok := LookupProviderSpec("testcorp-mixed"); !ok {
		t.Fatal("mixed-case name was not registered in lowercase form")
	}
}

// containsString reports whether ss contains s.
func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
