package provider

// Tests for per-model declarations: a model's image capability is declared on
// its own entry ([[models]] / `model add`), overriding the provider's coarse
// setting so one gateway can serve both vision and text-only models. This is the
// declaration that gates view_image.

import (
	"testing"
)

// TestModelSupportsImagesUsesModelDeclaration verifies a per-model declaration
// wins over the provider's setting in both directions, and that an undeclared
// model still falls back to the provider default.
func TestModelSupportsImagesUsesModelDeclaration(t *testing.T) {
	registerTestProvider(t, ProviderSpec{
		Name:           "decltest-gw",
		Protocol:       ProtocolOpenAI,
		DefaultBaseURL: "https://gw.decltest.test/v1",
		// The provider itself is text-only: a model declaration must be able to
		// opt one id in without flipping the whole endpoint.
		SupportsImages: false,
	}, nil, "")
	vision := true
	text := false
	RegisterDeclaredModels("decltest-gw", []DeclaredModel{
		{ID: "decltest-vl", SupportsImages: &vision},
		{ID: "decltest-text", SupportsImages: &text},
		{ID: "decltest-inherit"}, // no declaration
	})
	t.Cleanup(func() { RegisterDeclaredModels("decltest-gw", nil) })

	if !ModelSupportsImages("decltest-gw", "decltest-vl") {
		t.Error("a model declared with supports_images = true must accept images")
	}
	if ModelSupportsImages("decltest-gw", "decltest-text") {
		t.Error("a model declared with supports_images = false must not accept images")
	}
	if ModelSupportsImages("decltest-gw", "decltest-inherit") {
		t.Error("an undeclared model must inherit the provider's text-only default")
	}
	if ModelSupportsImages("decltest-gw", "never-declared") {
		t.Error("a model with no relation to the provider must inherit its default")
	}
}

// TestModelSupportsImagesOverridesBuiltinProvider: a declaration also applies to
// a built-in provider, so a newly added vision model works without waiting for
// the preset table to learn about it.
func TestModelSupportsImagesOverridesBuiltinProvider(t *testing.T) {
	vision := true
	RegisterDeclaredModels("openai", []DeclaredModel{{ID: "gpt-new-vl", SupportsImages: &vision}})
	t.Cleanup(func() { RegisterDeclaredModels("openai", nil) })

	if !ModelSupportsImages("openai", "gpt-new-vl") {
		t.Error("a declaration for a built-in provider was ignored")
	}
	// An unrelated built-in model keeps its historical permissive default.
	if !ModelSupportsImages("openai", "gpt-4o") {
		t.Error("a per-model declaration must not change other models")
	}
}

// TestModelSupportsImagesAcceptsRoutedSpelling: a "provider/model" id names the
// same model as its bare tail, so a declaration is found either way.
func TestModelSupportsImagesAcceptsRoutedSpelling(t *testing.T) {
	vision := true
	RegisterDeclaredModels("decltest-routed", []DeclaredModel{{ID: "vl-1", SupportsImages: &vision}})
	t.Cleanup(func() { RegisterDeclaredModels("decltest-routed", nil) })

	if !ModelSupportsImages("decltest-routed", "decltest-routed/vl-1") {
		t.Error("a routed id did not find its bare model's declaration")
	}
}

// TestRegisterDeclaredModelsMakesModelSelectable: declaring a model adds it to the
// preset catalog, and clearing the declaration removes the entry it added without
// disturbing the provider's own models.
func TestRegisterDeclaredModelsMakesModelSelectable(t *testing.T) {
	registerTestProvider(t, ProviderSpec{
		Name:           "decltest-cat",
		Protocol:       ProtocolOpenAI,
		DefaultBaseURL: "https://gw.decltest.test/v1",
	}, []PresetModel{{ID: "decltest-base"}}, "")
	t.Cleanup(func() { RegisterDeclaredModels("decltest-cat", nil) })

	vision := true
	RegisterDeclaredModels("decltest-cat", []DeclaredModel{
		{ID: "decltest-extra", DisplayName: "Extra", SupportsImages: &vision},
	})
	if p, ok := LookupPreset("decltest-extra"); !ok || p.Provider != "decltest-cat" {
		t.Fatalf("declared model not selectable: %+v ok=%v", p, ok)
	}
	if p, ok := LookupPreset("decltest-extra"); ok && p.DisplayName != "Extra" {
		t.Errorf("display name = %q, want Extra", p.DisplayName)
	}

	// Clearing declarations drops only what the declaration added.
	RegisterDeclaredModels("decltest-cat", nil)
	if _, ok := LookupPreset("decltest-extra"); ok {
		t.Error("clearing the declaration left its model in the catalog")
	}
	if _, ok := LookupPreset("decltest-base"); !ok {
		t.Error("clearing the declaration removed the provider's own model")
	}
	if ModelSupportsImages("decltest-cat", "decltest-extra") {
		t.Error("clearing the declaration left the image capability behind")
	}
}

// TestResolveNamedProviderHonorsModelDeclaration: the resolved Model's
// SupportsImages (what checkImageSupport enforces on the wire) follows the
// per-model declaration, not just the provider flag.
func TestResolveNamedProviderHonorsModelDeclaration(t *testing.T) {
	registerTestProvider(t, ProviderSpec{
		Name:           "decltest-resolve",
		Protocol:       ProtocolOpenAI,
		DefaultBaseURL: "https://gw.decltest.test/v1",
		SupportsImages: false,
	}, nil, "")
	vision := true
	RegisterDeclaredModels("decltest-resolve", []DeclaredModel{{ID: "decltest-vl", SupportsImages: &vision}})
	t.Cleanup(func() { RegisterDeclaredModels("decltest-resolve", nil) })

	p, _, err := ResolveNamedProvider("decltest-resolve", "decltest-vl", "", "", func(string) string { return "" })
	if err != nil {
		t.Fatalf("ResolveNamedProvider: %v", err)
	}
	models := p.Models()
	if len(models) == 0 || !models[0].SupportsImages {
		t.Errorf("resolved models = %+v, want the declared vision capability", models)
	}

	// A model the declaration does not mention keeps the provider's false.
	p, _, err = ResolveNamedProvider("decltest-resolve", "decltest-text", "", "", func(string) string { return "" })
	if err != nil {
		t.Fatalf("ResolveNamedProvider: %v", err)
	}
	if models := p.Models(); len(models) > 0 && models[0].SupportsImages {
		t.Errorf("resolved models = %+v, want the provider's text-only default", models)
	}
}
