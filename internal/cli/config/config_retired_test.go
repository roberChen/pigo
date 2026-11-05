package config

// Tests for hints about config.toml keys pigo no longer reads. Image support
// used to live on [[providers]]; TOML decoding drops unknown keys silently, so a
// stale declaration would otherwise downgrade a custom endpoint's models to the
// text-only default without telling anyone.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadFileConfigWarnsOnProviderSupportsImages: a provider-level key yields one
// notice that names the replacement, and the rest of the config still loads.
func TestLoadFileConfigWarnsOnProviderSupportsImages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[providers]]
name = "retired-gw"
base_url = "https://gw.retired.test/v1"
supports_images = true

[[providers]]
name = "retired-gw2"
base_url = "https://gw2.retired.test/v1"
supports_images = false

[[models]]
id = "retired-gw-vl"
provider = "retired-gw"
supports_images = true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, notices, err := LoadFileConfigWithWarnings(path)
	if err != nil {
		t.Fatalf("LoadFileConfigWithWarnings: %v", err)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d, want 2 (a retired key must not drop the entry)", len(cfg.Providers))
	}
	// The [[models]] declaration is still honored: that is where the capability
	// now lives, and the notice tells the user so.
	if len(cfg.Models) != 1 {
		t.Fatalf("models = %d, want 1", len(cfg.Models))
	}
	if vision, ok := cfg.Models[0].Vision(); !ok || !vision {
		t.Errorf("model declaration = (%v, %v), want (true, true)", vision, ok)
	}
	// One notice for the whole load, however many entries still carry the key:
	// the decoder reports a key PATH, so two stale `providers.supports_images`
	// occurrences collapse into one — which is the right shape for a hint, since
	// the fix is the same for every entry.
	if len(notices) != 1 {
		t.Fatalf("notices = %d (%v), want 1", len(notices), notices)
	}
	for _, n := range notices {
		if !strings.Contains(n, "[[providers]] supports_images is no longer read") {
			t.Errorf("notice %q does not name the retired key", n)
		}
		if !strings.Contains(n, "[[models]]") || !strings.Contains(n, "--supports-images") {
			t.Errorf("notice %q does not point at either replacement form", n)
		}
	}
}

// TestLoadFileConfigQuietWithoutRetiredKeys: a config that never used the retired
// key (including one that declares image support the current way) produces no
// notice, so the hint cannot become background noise.
func TestLoadFileConfigQuietWithoutRetiredKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	body := `
[[providers]]
name = "current-gw"
base_url = "https://gw.current.test/v1"

[[models]]
id = "current-gw-vl"
provider = "current-gw"
supports_images = true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if _, notices, err := LoadFileConfigWithWarnings(path); err != nil {
		t.Fatalf("LoadFileConfigWithWarnings: %v", err)
	} else if len(notices) != 0 {
		t.Errorf("notices = %v, want none", notices)
	}
	if _, err := LoadFileConfig(path); err != nil {
		t.Fatalf("LoadFileConfig: %v", err)
	}
}
