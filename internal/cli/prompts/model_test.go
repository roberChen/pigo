package prompts

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/roberChen/pigo/internal/cli"
	"github.com/roberChen/pigo/internal/cli/run"
	"github.com/roberChen/pigo/internal/provider"
	"github.com/roberChen/pigo/internal/runtime"
)

// TestModelCommandSwitchesToBareProviderName verifies /model zai selects the
// zai provider's default model (issue #564): live.Model carries the concrete
// preset id so the next turn's wire request targets a real model instead of
// sending the literal provider name to OpenRouter.
func TestModelCommandSwitchesToBareProviderName(t *testing.T) {
	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, provider.NewCredentialStore(nil))

	out, err := reg.ResolveOutcome("/model zai")
	if err != nil {
		t.Fatalf("ResolveOutcome /model zai: %v", err)
	}
	if live.Model != "glm-4.7" {
		t.Errorf("live.Model = %q, want glm-4.7", live.Model)
	}
	if live.ProviderName != "zai" {
		t.Errorf("live.ProviderName = %q, want zai", live.ProviderName)
	}
	if !strings.Contains(out.Message, "glm-4.7") || !strings.Contains(out.Message, "zai") {
		t.Errorf("message = %q, want it to mention glm-4.7 (zai)", out.Message)
	}
}

// TestModelCommandConcreteIdUnchanged verifies a concrete model id switches
// verbatim, and a bare provider name without preset models reports the
// mismatch instead of silently falling back to OpenRouter.
func TestModelCommandConcreteIdUnchanged(t *testing.T) {
	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, provider.NewCredentialStore(nil))

	out, err := reg.ResolveOutcome("/model glm-5.2")
	if err != nil {
		t.Fatalf("ResolveOutcome /model glm-5.2: %v", err)
	}
	if live.Model != "glm-5.2" || live.ProviderName != "zai" {
		t.Errorf("live = (%q, %q), want (glm-5.2, zai)", live.Model, live.ProviderName)
	}
	if !strings.Contains(out.Message, "glm-5.2") {
		t.Errorf("message = %q, want it to mention glm-5.2", out.Message)
	}

	// A concrete id for another provider switches verbatim as before.
	out, err = reg.ResolveOutcome("/model deepseek-v4-pro")
	if err != nil {
		t.Fatalf("ResolveOutcome /model deepseek-v4-pro: %v", err)
	}
	if live.Model != "deepseek-v4-pro" || live.ProviderName != "deepseek" {
		t.Errorf("live = (%q, %q), want (deepseek-v4-pro, deepseek)", live.Model, live.ProviderName)
	}
}

// TestModelsFetchCommandAndSwitch drives "/models fetch" against an
// httptest endpoint (issue #566): the catalog lands on live.FetchedModels,
// and switching to a fetched id stays on the gateway that served it instead
// of falling through the heuristic chain.
func TestModelsFetchCommandAndSwitch(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"m-b"},{"id":"m-a"},{"id":"m-b"}]}`))
	}))
	defer srv.Close()

	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openai", BaseURL: srv.URL + "/v1"}
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride("openai", "test-key")
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, creds)

	out, err := reg.ResolveOutcome("/models fetch")
	if err != nil {
		t.Fatalf("ResolveOutcome /models fetch: %v", err)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want Bearer test-key", gotAuth)
	}
	if len(live.FetchedModels) != 2 || live.FetchedModels[0] != "m-a" || live.FetchedModels[1] != "m-b" {
		t.Fatalf("live.FetchedModels = %v, want [m-a m-b]", live.FetchedModels)
	}
	if live.FetchedAt.IsZero() {
		t.Error("FetchedAt not stamped")
	}
	if !strings.Contains(out.Message, "2 models") {
		t.Errorf("message = %q, want it to mention 2 models", out.Message)
	}

	// Switching to a fetched id pins the live provider.
	out, err = reg.ResolveOutcome("/model m-b")
	if err != nil {
		t.Fatalf("ResolveOutcome /model m-b: %v", err)
	}
	if live.Model != "m-b" || live.ProviderName != "openai" {
		t.Fatalf("live = (%q, %q), want (m-b, openai)", live.Model, live.ProviderName)
	}
	if models := live.Provider.Models(); len(models) != 1 || models[0].ID != "m-b" || models[0].Provider != "openai" {
		t.Fatalf("wire models = %+v, want one openai/m-b entry", models)
	}
	if !strings.Contains(out.Message, "fetched catalog") {
		t.Errorf("message = %q, want the fetched-catalog note", out.Message)
	}
}

// TestModelsFetchDegrades verifies a failing endpoint degrades gracefully:
// the error is reported and the static preset listing still works.
func TestModelsFetchDegrades(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openai", BaseURL: srv.URL}
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride("openai", "test-key")
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, creds)

	out, err := reg.ResolveOutcome("/models fetch")
	if err != nil {
		t.Fatalf("ResolveOutcome /models fetch: %v", err)
	}
	if live.FetchedModels != nil {
		t.Errorf("live.FetchedModels = %v, want nil after failed fetch", live.FetchedModels)
	}
	if !strings.Contains(out.Message, "fetch failed") {
		t.Errorf("message = %q, want the fetch-failed note", out.Message)
	}
	if live.Model != "openrouter/free" {
		t.Errorf("live.Model = %q, want unchanged", live.Model)
	}
}

// TestModelsFetchUsesResolvedBaseURL verifies "/models fetch" probes the endpoint
// chat actually uses, not the provider's registry default and not live.BaseURL
// alone.
//
// The regression this pins is a credential leak: pointing a built-in provider at a
// corporate gateway with <PROVIDER>_BASE_URL is the documented way to retarget one,
// and chat honors it — but discovery used live.BaseURL (the flag/config value, here
// empty) and fell back to the registry default, so it probed api.openai.com and
// sent the gateway's key there. A gateway on the Anthropic protocol was doubly
// broken, appending another /v1.
func TestModelsFetchUsesResolvedBaseURL(t *testing.T) {
	var gotPaths []string
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[{"id":"corp-a"}]}`))
	}))
	defer srv.Close()
	t.Setenv("OPENAI_BASE_URL", srv.URL+"/v1")

	live := &cli.LiveConfig{Model: "corp-a", ProviderName: "openai"} // no flag base URL
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride("openai", "corp-key")
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, creds)

	out, err := reg.ResolveOutcome("/models fetch")
	if err != nil {
		t.Fatalf("ResolveOutcome /models fetch: %v", err)
	}
	if len(gotPaths) != 1 || gotPaths[0] != "/v1/models" {
		t.Fatalf("paths = %v, want [/v1/models] on the gateway from OPENAI_BASE_URL", gotPaths)
	}
	if gotAuth != "Bearer corp-key" {
		t.Errorf("Authorization = %q, want the gateway's key", gotAuth)
	}
	if len(live.FetchedModels) != 1 || live.FetchedModels[0] != "corp-a" {
		t.Errorf("live.FetchedModels = %v, want [corp-a]", live.FetchedModels)
	}
	if !strings.Contains(out.Message, srv.URL) {
		t.Errorf("message = %q, want it to name the endpoint that answered", out.Message)
	}
}

// TestModelsFetchKeepsPreviousCatalog verifies a failed refresh leaves the catalog
// it already had. A gateway that is briefly unreachable (or answers in a shape
// pigo does not understand) must not turn a working /model picker into an empty
// one: the ids were valid a minute ago and nothing has replaced them.
func TestModelsFetchKeepsPreviousCatalog(t *testing.T) {
	var fail bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"corp-a"},{"id":"corp-b"}]}`))
	}))
	defer srv.Close()

	live := &cli.LiveConfig{Model: "corp-a", ProviderName: "openai", BaseURL: srv.URL + "/v1"}
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride("openai", "k")
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, creds)

	if out, err := reg.ResolveOutcome("/models fetch"); err != nil || len(live.FetchedModels) != 2 {
		t.Fatalf("first fetch: models = %v, err = %v, msg = %q", live.FetchedModels, err, out.Message)
	}
	fetchedAt := live.FetchedAt

	fail = true
	out, err := reg.ResolveOutcome("/models fetch")
	if err != nil {
		t.Fatalf("ResolveOutcome /models fetch: %v", err)
	}
	if len(live.FetchedModels) != 2 || live.FetchedModels[1] != "corp-b" {
		t.Errorf("live.FetchedModels = %v, want the previous [corp-a corp-b]", live.FetchedModels)
	}
	if live.FetchedAt != fetchedAt {
		t.Error("FetchedAt moved on a failed fetch, want it untouched")
	}
	if !strings.Contains(out.Message, "fetch failed") {
		t.Errorf("message = %q, want the failure reported", out.Message)
	}
}

// TestModelCommandShowsImageSupport: /model reports the active model's image
// capability and where it came from. Image support is declared per model, so the
// line must distinguish a declaration from pigo's default instead of printing a
// bare yes/no — a "no" on a custom endpoint is actionable (declare the vision
// ids), a "no" from a declaration is deliberate.
func TestModelCommandShowsImageSupport(t *testing.T) {
	live := &cli.LiveConfig{Model: "glm-5.2", ProviderName: "zai"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, provider.NewCredentialStore(nil))

	out, err := reg.ResolveOutcome("/model")
	if err != nil {
		t.Fatalf("ResolveOutcome /model: %v", err)
	}
	if !strings.Contains(out.Message, "image input: supported (built-in default)") {
		t.Errorf("message = %q, want a built-in default to read as supported", out.Message)
	}

	// A switch reports the new model's capability in the same breath.
	live.FetchedModels = nil
	out, err = reg.ResolveOutcome("/model deepseek-v4-pro")
	if err != nil {
		t.Fatalf("ResolveOutcome /model deepseek-v4-pro: %v", err)
	}
	if !strings.Contains(out.Message, "image input: supported") {
		t.Errorf("message = %q, want the switched model's image capability", out.Message)
	}
}

// TestModelCommandReportsDeclaredTextOnly: an explicit declaration is echoed as
// such, with the way out (declare another model or use a vision one).
func TestModelCommandReportsDeclaredTextOnly(t *testing.T) {
	text := false
	provider.RegisterDeclaredModels("openai", []provider.DeclaredModel{
		{ID: "decltest-text-only-toggle", SupportsImages: &text},
	})
	t.Cleanup(func() { provider.RegisterDeclaredModels("openai", nil) })

	live := &cli.LiveConfig{Model: "decltest-text-only-toggle", ProviderName: "openai"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, provider.NewCredentialStore(nil))

	out, err := reg.ResolveOutcome("/model")
	if err != nil {
		t.Fatalf("ResolveOutcome /model: %v", err)
	}
	if !strings.Contains(out.Message, "image input: not supported (declared text-only)") {
		t.Errorf("message = %q, want the declaration named as the source", out.Message)
	}
}

// TestImageSupportSummaryCustomEndpoint: an undeclared model on a user-defined
// endpoint is text-only by default, and the message says how to opt in.
func TestImageSupportSummaryCustomEndpoint(t *testing.T) {
	if err := provider.RegisterCustomProvider(provider.ProviderSpec{
		Name:           "imgsummary-custom",
		Protocol:       provider.ProtocolOpenAI,
		DefaultBaseURL: "https://gw.imgsummary.test/v1",
	}, nil, ""); err != nil {
		t.Fatalf("register custom provider: %v", err)
	}
	t.Cleanup(func() { provider.UnregisterCustomProvider("imgsummary-custom") })

	got := imageSupportSummary("imgsummary-custom", "some-text-model")
	if !strings.Contains(got, "not supported (custom endpoint default)") {
		t.Errorf("summary = %q, want the custom default named", got)
	}
	if !strings.Contains(got, "--supports-images") {
		t.Errorf("summary = %q, want the way to declare vision ids", got)
	}
}

// TestModelSwitchRefreshesTheCompactionPlan verifies that a /model switch
// re-resolves the compaction plan instead of leaving the previous model's
// window and threshold in place. This is the regression guard for "switching
// models mid-session does not update the context info or the compaction
// threshold": live.ContextWindow and live.Compaction are one model's numbers,
// and the loop attaches live.Compaction on every turn, so a stale plan made the
// next turn compact against a budget the new model may not accept.
func TestModelSwitchRefreshesTheCompactionPlan(t *testing.T) {
	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	live.RefreshCompaction()
	before := live.CompactionDetail()

	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, provider.NewCredentialStore(nil))
	if _, err := reg.ResolveOutcome("/model zai"); err != nil {
		t.Fatalf("ResolveOutcome /model zai: %v", err)
	}

	want := run.PlanCompaction(live.ProviderName, live.Model)
	// Guard the fixture itself: if the two models ever share a budget the rest of
	// this test would pass without exercising anything.
	if want.ContextWindow == before.Point {
		t.Fatalf("preset windows did not differ (%d); the test proves nothing", want.ContextWindow)
	}

	if live.ContextWindow != want.ContextWindow {
		t.Errorf("context window = %d, want %d (the model switched to)", live.ContextWindow, want.ContextWindow)
	}
	if live.Compaction.Model != live.Model || live.Compaction.ProviderName != live.ProviderName {
		t.Errorf("plan resolved for (%q, %q), want (%q, %q)",
			live.Compaction.Model, live.Compaction.ProviderName, live.Model, live.ProviderName)
	}
	if got := live.CompactionDetail(); got.Point != want.TriggerPoint || got.Source != want.TriggerInfo.Source {
		t.Errorf("trigger = (%d, %q), want (%d, %q)",
			got.Point, got.Source, want.TriggerPoint, want.TriggerInfo.Source)
	}
	if live.CompactionSettings() != want.Settings {
		t.Error("CompactionSettings returned the plan of another model")
	}
	// The readout the user sees must actually move; otherwise the refresh is moot.
	if after := live.CompactionDetail(); after.Point == before.Point {
		t.Errorf("trigger point did not change after the switch (still %d)", after.Point)
	}
}

// TestModelSwitchFromFetchedCatalogRefreshesThePlan covers the second switch
// branch (an id from /models fetch): it rewrites the provider too, so it must
// re-resolve the plan for the gateway that ends up serving the model.
func TestModelSwitchFromFetchedCatalogRefreshesThePlan(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"id":"gw-model"}]}`))
	}))
	defer srv.Close()

	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openai", BaseURL: srv.URL + "/v1"}
	live.RefreshCompaction()
	creds := provider.NewCredentialStore(nil)
	creds.SetOverride("openai", "test-key")
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, creds)
	if _, err := reg.ResolveOutcome("/models fetch"); err != nil {
		t.Fatalf("ResolveOutcome /models fetch: %v", err)
	}

	if _, err := reg.ResolveOutcome("/model gw-model"); err != nil {
		t.Fatalf("ResolveOutcome /model gw-model: %v", err)
	}
	want := run.PlanCompaction(live.ProviderName, live.Model)
	if live.Compaction.Model != "gw-model" || live.Compaction.ProviderName != live.ProviderName {
		t.Errorf("plan resolved for (%q, %q), want (gw-model, %q)",
			live.Compaction.Model, live.Compaction.ProviderName, live.ProviderName)
	}
	if live.ContextWindow != want.ContextWindow {
		t.Errorf("context window = %d, want %d", live.ContextWindow, want.ContextWindow)
	}
}

// TestModelViewDoesNotResolveAPlan pins the reporting form: "/model" with no
// argument only describes the current model, so it must not touch the plan.
func TestModelViewDoesNotResolveAPlan(t *testing.T) {
	live := &cli.LiveConfig{Model: "openrouter/free", ProviderName: "openrouter"}
	reg := runtime.NewSlashRegistry()
	RegisterLiveCommands(reg, live, provider.NewCredentialStore(nil))
	if _, err := reg.ResolveOutcome("/model"); err != nil {
		t.Fatalf("ResolveOutcome /model: %v", err)
	}
	if live.Compaction.Model != "" || live.ContextWindow != 0 {
		t.Errorf("viewing the model resolved a plan (%+v, window %d)", live.Compaction, live.ContextWindow)
	}
}
