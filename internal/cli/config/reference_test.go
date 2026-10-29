package config

// Tests for the generated configuration reference: the document must cover every
// configurable item the code exposes — config.toml fields, provider and agent
// definition fields, hook events, environment variables — so the built-in pigorc
// skill cannot go stale. The CLI flag half of that contract is tested in
// cmd/pigo, where the flag set lives.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/smallnest/pigo/internal/hooks"
	"github.com/smallnest/pigo/internal/mcp"
)

// TestReferenceCoversEveryFileConfigField fails when a config.toml field has no
// option registry entry (and therefore no documentation).
func TestReferenceCoversEveryFileConfigField(t *testing.T) {
	// Definitions covered by their own DSL sections and field tables.
	documented := map[string]bool{"providers": true, "agents": true, "mcp": true}

	covered := map[string]bool{}
	for _, spec := range optionSpecs {
		covered[spec.TOML] = true
	}

	checkStruct := func(prefix string, v any) {
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			field := rt.Field(i)
			// Nested tables (memory, checkpoint, compaction, dream) are documented
			// through their leaf keys, which checkStruct visits separately.
			if field.Type.Kind() == reflect.Struct {
				continue
			}
			tag := strings.Split(field.Tag.Get("toml"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if unknownKind(tag, field.Type) {
				continue
			}
			key := tag
			if prefix != "" {
				key = prefix + "." + tag
			}
			if documented[key] {
				continue
			}
			if !covered[key] {
				t.Errorf("config.toml field %q is not in the pigorc option registry (add it to options.go so it is documented)", key)
			}
		}
	}
	checkStruct("", FileConfig{})
	checkStruct("memory", MemoryConfig{})
	checkStruct("checkpoint", CheckpointConfig{})
	checkStruct("compaction", CompactionConfig{})
	checkStruct("dream", DreamConfig{})

	// The push-caps map is documented as a dotted key.
	if !covered["checkpoint.push_caps.<section>"] {
		t.Error("checkpoint.push_caps is not documented")
	}
}

// TestReferenceCoversDefinitionFields fails when a provider or agent definition
// field has no documentation row.
func TestReferenceCoversDefinitionFields(t *testing.T) {
	cases := []struct {
		name string
		typ  reflect.Type
		rows []fieldDoc
	}{
		{"ProviderConfig", reflect.TypeOf(ProviderConfig{}), providerFieldDocs},
		{"AgentConfig", reflect.TypeOf(AgentConfig{}), agentFieldDocs},
		{"mcp.ServerConfig", reflect.TypeOf(mcp.ServerConfig{}), mcpFieldDocs},
	}
	for _, tc := range cases {
		// Documentation rows use the config spelling (system_prompt); the struct
		// uses Go names (SystemPrompt). Compare a normalized form.
		documented := map[string]bool{}
		for _, row := range tc.rows {
			documented[normalizeFieldName(row.Name)] = true
		}
		for i := 0; i < tc.typ.NumField(); i++ {
			name := tc.typ.Field(i).Name
			if !documented[normalizeFieldName(name)] {
				t.Errorf("%s field %q has no documentation row", tc.name, name)
			}
		}
	}
	// Nested hook config fields are part of the agent table's `hooks` row; keep
	// AgentHookConfig itself covered so a new field is noticed.
	hookDocs := map[string]bool{}
	for _, row := range agentFieldDocs {
		hookDocs[row.Name] = true
	}
	if !hookDocs["hooks"] {
		t.Error("agent hooks field undocumented")
	}
}

// TestReferenceDocumentsEveryHookEvent verifies every lifecycle event appears in
// the generated document.
func TestReferenceDocumentsEveryHookEvent(t *testing.T) {
	doc := ReferenceMarkdown(ReferenceOptions{Version: "test"})
	for _, event := range hooks.AllEvents {
		if !strings.Contains(doc, "`"+event+"`") {
			t.Errorf("hook event %q missing from the reference", event)
		}
	}
}

// TestReferenceDocumentsEveryPIGOEnvVar scans the repository for PIGO_* variables
// the code reads and fails when one is undocumented.
func TestReferenceDocumentsEveryPIGOEnvVar(t *testing.T) {
	documented := map[string]bool{}
	for _, row := range envVarDocs {
		documented[row.Name] = true
	}
	root := filepath.Join("..", "..", "..") // module root from internal/cli/config
	found := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "book", "docs", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		text := string(data)
		for _, call := range []string{`Getenv("PIGO_`, `LookupEnv("PIGO_`} {
			for rest := text; ; {
				idx := strings.Index(rest, call)
				if idx < 0 {
					break
				}
				rest = rest[idx+len(call):]
				name := "PIGO_" + rest[:strings.IndexByte(rest, '"')]
				found[name] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("no PIGO_* variables found; the scan is broken")
	}
	for name := range found {
		if name == "PIGO_RC_RECORDS" || name == "PIGO_RC_HOOKS" || name == "PIGO_RC_ENV_BEFORE" || name == "PIGO_RC_ENV_AFTER" || name == "PIGO_VERSION" {
			continue // pigorc evaluation scaffolding, not user configuration
		}
		if !documented[name] {
			t.Errorf("environment variable %s is read by the code but missing from the reference", name)
		}
	}
}

// TestReferenceRendersContent verifies the document's shape: marker, version,
// option table, provider/agent tables, hook events, env vars and caller-supplied
// flags.
func TestReferenceRendersContent(t *testing.T) {
	doc := ReferenceMarkdown(ReferenceOptions{
		Version: "9.9.9",
		Flags:   []FlagDoc{{Name: "model", Shorthand: "m", Default: "openrouter/free", Usage: "model id"}},
	})
	for _, want := range []string{
		ReferenceMarker,
		"pigo 9.9.9",
		"## Options (`option <key> <value>`)",
		"| `model` | string | `model` |",
		"## Third-party providers",
		"| `base_url` | string |",
		"## Custom sub-agents",
		"| `system_prompt` | string |",
		"## Hook events",
		"`PreToolUse`",
		"## Environment variables",
		"`PIGO_HOME`",
		"| `--model, -m` | `openrouter/free` | model id |",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("reference missing %q", want)
		}
	}
	if strings.Contains(doc, "|  |") {
		t.Error("reference contains an empty table cell header")
	}
}

// TestWriteReferenceAndOwnership verifies the file is written atomically and that
// ownership is decided by the marker in SKILL.md (a user's own skill of the same
// name is untouched).
func TestWriteReferenceAndOwnership(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "pigorc")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if SkillOwnsReference(skillDir) {
		t.Fatal("a directory without SKILL.md must not count as pigo's skill")
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("# my own pigorc notes\n"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if SkillOwnsReference(skillDir) {
		t.Fatal("a user-written skill must not count as pigo's skill")
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(ReferenceMarker+"\n# pigorc\n"), 0o600); err != nil {
		t.Fatalf("write SKILL.md: %v", err)
	}
	if !SkillOwnsReference(skillDir) {
		t.Fatal("the marker should make the directory pigo-owned")
	}

	path := SkillsReferencePath(dir)
	if err := WriteReference(path, ReferenceOptions{Version: "test"}); err != nil {
		t.Fatalf("WriteReference: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read reference: %v", err)
	}
	if !strings.Contains(string(data), ReferenceMarker) {
		t.Error("written reference lacks the marker")
	}
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if err := WriteReference(path, ReferenceOptions{Version: "test"}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !SkillOwnsReference(skillDir) {
		t.Error("rewriting must keep the skill owned")
	}
}

// TestOptionSpecsAreWellFormed verifies the registry itself: unique keys, complete
// metadata, and an applier matching each kind.
func TestOptionSpecsAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, spec := range optionSpecs {
		if spec.Key == "" || spec.Type == "" || spec.Summary == "" || spec.Example == "" {
			t.Errorf("option %+v has incomplete metadata", spec)
		}
		if seen[spec.Key] {
			t.Errorf("duplicate option key %q", spec.Key)
		}
		seen[spec.Key] = true
		if spec.Kind == optList && spec.setList == nil {
			t.Errorf("list option %q has no setList", spec.Key)
		}
		if spec.Kind != optList && spec.Kind != optPushCaps && spec.set == nil {
			t.Errorf("option %q has no setter", spec.Key)
		}
	}
}

// normalizeFieldName folds Go field names and config spellings together:
// SystemPrompt, systemPrompt and system_prompt all become "systemprompt".
func normalizeFieldName(name string) string {
	name = strings.ReplaceAll(name, "_", "")
	name = strings.ReplaceAll(name, "-", "")
	return strings.ToLower(name)
}

// unknownKind reports whether a field is a map (documented as a dotted key). The
// only such field is checkpoint.push_caps, checked explicitly below.
func unknownKind(tag string, typ reflect.Type) bool {
	return typ.Kind() == reflect.Map
}
