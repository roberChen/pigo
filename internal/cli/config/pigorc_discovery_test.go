// This file implements tests for the pigorc features provider auto-discovery
// depends on: fetching a model catalogue from a provider API and declaring every
// entry (US: pigorc provider auto-configuration).
//
// The point of these tests is that a discovery recipe is ordinary Bash — a loop
// over `curl | jq` output that calls `provider add ... --model` once per id — so
// the DSL has to survive being driven from a loop rather than typed as one long
// command line.
package config

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestPigorcProviderRefinementAcrossCalls verifies that a provider defined once
// and then extended from a loop keeps the endpoint it was defined with. This is
// the shape every discovery recipe takes:
//
//	provider add cat --type openai --base-url "$URL" --env-var CAT_KEY
//	for m in $(curl -s "$URL/models" | jq -r '.data[].id'); do
//	  provider add cat --model "$m"
//	done
//
// The per-model calls set only --model, so they must refine the definition, not
// replace it: a repeat that dropped base_url made the provider unusable.
func TestPigorcProviderRefinementAcrossCalls(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
provider add cat --type openai --base-url "https://llm.example.test/v1" \
  --env-var CAT_KEY --auth bearer
for m in alpha-1 beta-2 gamma-3; do
  provider add cat --model "$m"
done
`)
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if len(p.Providers) != 1 {
		t.Fatalf("providers = %+v, want 1", p.Providers)
	}
	got := p.Providers[0]
	if got.Name != "cat" {
		t.Errorf("name = %q, want cat", got.Name)
	}
	// The definition must survive the per-model refinements.
	if got.Protocol != "openai" {
		t.Errorf("protocol = %q, want openai (the loop calls erased it)", got.Protocol)
	}
	if got.BaseURL != "https://llm.example.test/v1" {
		t.Errorf("base_url = %q, want https://llm.example.test/v1", got.BaseURL)
	}
	if got.EnvVar != "CAT_KEY" {
		t.Errorf("env_var = %q, want CAT_KEY", got.EnvVar)
	}
	if got.Auth != "bearer" {
		t.Errorf("auth = %q, want bearer", got.Auth)
	}
	want := []string{"alpha-1", "beta-2", "gamma-3"}
	if len(got.Models) != len(want) {
		t.Fatalf("models = %v, want %v", got.Models, want)
	}
	for i, id := range want {
		if got.Models[i] != id {
			t.Errorf("models[%d] = %q, want %q (order decides the default model)", i, got.Models[i], id)
		}
	}
}

// TestPigorcRefinementDoesNotClobberExplicitChanges verifies the merge is not one
// way: a later call that names a field overrides it, so a project script can
// retarget or re-key a provider the global script declared.
func TestPigorcRefinementDoesNotClobberExplicitChanges(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
provider add cat --type openai --base-url "https://old.example.test/v1" --env-var OLD_KEY
provider add cat --base-url "https://new.example.test/v1"
provider add cat --env-var NEW_KEY --model beta-2
`)
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	got := p.Providers[0]
	if got.BaseURL != "https://new.example.test/v1" {
		t.Errorf("base_url = %q, want the new value", got.BaseURL)
	}
	if got.EnvVar != "NEW_KEY" {
		t.Errorf("env_var = %q, want NEW_KEY", got.EnvVar)
	}
	if got.Protocol != "openai" {
		t.Errorf("protocol = %q, want openai kept from the first call", got.Protocol)
	}
}

// TestPigorcShellControlFlow verifies that the constructs a discovery script
// needs — if/case/while/for, arithmetic, arrays and command substitution from an
// external command — all run inside the pigorc and can drive the DSL.
func TestPigorcShellControlFlow(t *testing.T) {
	requireBash(t)
	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
# Command substitution from an external program (the shape of curl | jq).
catalogue=$(printf '%s\n' alpha-1 alpha-2 beta-1 beta-2 gamma-vl)

# A for loop with case discrimination: only the beta ids are registered.
selected=""
for m in $catalogue; do
  case "$m" in
    beta-*) selected="$selected $m" ;;
    *) ;;
  esac
done

# An if guard around the DSL call, so an empty result registers nothing.
if [ -n "$selected" ]; then
  provider add cat --type openai --base-url "https://llm.example.test/v1"
  for m in $selected; do
    provider add cat --model "$m"
  done
fi

# A while loop over a here-string, plus arithmetic.
i=0
while read -r id; do
  [ -z "$id" ] && continue
  i=$((i + 1))
done <<EOF
alpha-1
beta-1
EOF

# An array accumulates ids and --supports-images marks the vision one.
vision=()
for m in $catalogue; do
  case "$m" in
    *-vl) vision+=("$m") ;;
  esac
done
for m in "${vision[@]}"; do
  model add "$m" --provider cat --supports-images
done

option model "cat/beta-1"
option thinking-level "$([ "$i" -ge 2 ] && echo high || echo low)"
`)
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	got := p.Providers[0]
	if want := []string{"beta-1", "beta-2"}; strings.Join(got.Models, ",") != strings.Join(want, ",") {
		t.Errorf("models = %v, want %v", got.Models, want)
	}
	if got.BaseURL != "https://llm.example.test/v1" {
		t.Errorf("base_url = %q, want it kept", got.BaseURL)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "gamma-vl" {
		t.Fatalf("models = %+v, want the gamma-vl declaration", p.Models)
	}
	if vision, ok := p.Models[0].Vision(); !ok || !vision {
		t.Errorf("gamma-vl vision = %v,%v; want declared true", vision, ok)
	}
	// The while loop ran twice, so the arithmetic-driven branch must win.
	if p.Options["model"] != "cat/beta-1" {
		t.Errorf("model = %q, want cat/beta-1", p.Options["model"])
	}
	if p.Options["thinking-level"] != "high" {
		t.Errorf("thinking-level = %q, want high (arithmetic/if in substitution)", p.Options["thinking-level"])
	}
}

// TestPigorcDiscoversModelsOverHTTP is the end-to-end shape of the feature: a
// real HTTP catalogue is fetched with curl and parsed with jq inside the pigorc,
// and every advertised id ends up registered. It skips when curl or jq is absent,
// following the repository's optional-binary pattern.
func TestPigorcDiscoversModelsOverHTTP(t *testing.T) {
	requireBash(t)
	for _, bin := range []string{"curl", "jq"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available", bin)
		}
	}
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "cat-large"},
				{"id": "cat-small"},
				{"id": "cat-vl"},
			},
		})
	}))
	defer srv.Close()

	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `
export CAT_API_KEY=test-key
provider add cat --type openai --base-url '`+srv.URL+`/v1' --env-var CAT_API_KEY
ids=$(curl -fsS -H "Authorization: Bearer $CAT_API_KEY" "$(printf '%s' "${CAT_BASE:-`+srv.URL+`/v1}")/models" \
        | jq -r '.data[].id')
for m in $ids; do
  case "$m" in
    *-vl) model add "$m" --provider cat --supports-images ;;
    *)    provider add cat --model "$m" ;;
  esac
done
option model cat/cat-large
`)
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	got := p.Providers[0]
	if want := "cat-large,cat-small"; strings.Join(got.Models, ",") != want {
		t.Errorf("models = %v, want %s", got.Models, want)
	}
	if got.BaseURL != srv.URL+"/v1" || got.Protocol != "openai" {
		t.Errorf("provider = %+v, want the endpoint kept by the loop", got)
	}
	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q, want the key exported by the script", gotAuth)
	}
	// The vision id was declared, not added to the provider's shorthand list.
	if len(p.Models) != 1 || p.Models[0].ID != "cat-vl" {
		t.Fatalf("models = %+v, want cat-vl declared", p.Models)
	}
	if p.Options["model"] != "cat/cat-large" {
		t.Errorf("model = %q", p.Options["model"])
	}
}

// TestPigorcTimeoutIsConfigurable verifies PIGO_PIGORC_TIMEOUT raises the limit,
// so a script that talks to a slow API is not cut off at the 15s default, and
// that a malformed value warns instead of aborting the load.
func TestPigorcTimeoutIsConfigurable(t *testing.T) {
	requireBash(t)
	old := pigorcTimeout
	t.Cleanup(func() { pigorcTimeout = old })

	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `option model "cat/x"`)

	t.Setenv("PIGO_PIGORC_TIMEOUT", "2m")
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", p.Warnings)
	}
	if p.Options["model"] != "cat/x" {
		t.Errorf("model = %q", p.Options["model"])
	}

	// A malformed value falls back to the default and says so.
	t.Setenv("PIGO_PIGORC_TIMEOUT", "not-a-duration")
	p, err = EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "PIGO_PIGORC_TIMEOUT") {
		t.Errorf("warnings = %v, want one naming PIGO_PIGORC_TIMEOUT", p.Warnings)
	}
	if p.Options["model"] != "cat/x" {
		t.Errorf("a bad timeout must not abort the load; model = %q", p.Options["model"])
	}
}

// TestPigorcSlowScriptHonoursTimeout proves the configured timeout is actually
// applied to the process, not just parsed.
func TestPigorcSlowScriptHonoursTimeout(t *testing.T) {
	requireBash(t)
	old := pigorcTimeout
	t.Cleanup(func() { pigorcTimeout = old })

	dir := t.TempDir()
	rc := filepath.Join(dir, ".pigorc")
	writeFile(t, rc, `sleep 5
option model "cat/late"`)

	pigorcTimeout = 150 * time.Millisecond
	start := time.Now()
	p, err := EvaluatePigorc(dir, []string{rc}, "test")
	if err != nil {
		t.Fatalf("EvaluatePigorc: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("evaluation took %s, want it cut off near 150ms", elapsed)
	}
	if len(p.Warnings) == 0 || !strings.Contains(p.Warnings[0], "timed out") {
		t.Errorf("warnings = %v, want a timeout warning", p.Warnings)
	}
}
