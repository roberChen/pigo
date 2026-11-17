// This file implements the workspace fixtures the tool plans read. Every
// workspace gets the same text files and image with the same markers, so a tool
// result can be checked against the plan without knowing which directory the
// session ran in (and a shared workspace verifies exactly like a private one).
package main

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"

	"github.com/roberChen/pigo/internal/memory"
)

// Fixture markers. catalog.go names them; keep the two in step.
const (
	notesMarker   = "NOTES-MARKER-LINE"
	logMarker     = "LOG-MARKER-LINE"
	dataMarker    = "DATA-MARKER"
	fixtureIgnore = "# fixtures\n"
)

// writeWorkspaceFixtures creates the client workspaces up front. They are
// prepared before the host starts so the first turn's read can never race a
// fixture write, and they are identical in both scenarios: the stress scenario
// simply never reads them.
//
// pool mirrors -workspace-pool: pool>0 pre-creates that many shared directories,
// 0 pre-creates maxClients private ones (the runner never exceeds that cap).
func writeWorkspaceFixtures(workDir string, pool, maxClients int) error {
	count := pool
	if count <= 0 {
		// Clients are added lazily up to -max-clients; create that many now so a
		// session never starts in a directory without fixtures.
		count = maxClients
	}
	for i := 1; i <= count; i++ {
		dir := filepath.Join(workDir, fmt.Sprintf("client-%03d", i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		files := map[string]string{
			"notes.txt": "fixture notes\n" + notesMarker + "\nend of notes\n",
			// read is planned with offset 2, limit 2, so the marker must sit on
			// line 2 and survive a two-line window.
			"log.txt":    "log header\n" + logMarker + "\nthird line\n",
			"data.txt":   "DATA-MARKER payload for grep\n",
			".gitignore": fixtureIgnore,
		}
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				return err
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "pixel.png"), fixturePNG, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// prepareMemoryHome enables the real persistent-memory store and seeds a file
// whose marker must be returned by memory_search. It runs before the host starts.
func prepareMemoryHome(home string) error {
	configDir := filepath.Join(home, "xdg", "pigo")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.toml"), []byte("[memory]\nenabled = true\n"), 0o600); err != nil {
		return err
	}
	memoryDir := filepath.Join(home, "memory", "global")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(memoryDir, "notes.md"), []byte("MEMORY-MARKER from the longstay memory fixture\n"), 0o644); err != nil {
		return err
	}
	root := filepath.Join(home, "memory")
	store, err := memory.Open(filepath.Join(root, "index.db"), root, "")
	if err != nil {
		return fmt.Errorf("prepare memory store: %w", err)
	}
	defer store.Close()
	results, err := store.Search("MEMORY-MARKER", memory.SearchOptions{Limit: 1, ReconcileFirst: true})
	if err != nil {
		return fmt.Errorf("validate memory fixture: %w", err)
	}
	if len(results) == 0 || filepath.Base(results[0].Path) != "notes.md" {
		return fmt.Errorf("validate memory fixture: marker not indexed")
	}
	return nil
}

// fixturePNG is a valid 1x1 PNG used by view_image. It is decoded once at init,
// so view_image exercises real image loading rather than a text placeholder.
var fixturePNG = func() []byte {
	data, err := base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")
	if err != nil {
		panic(err)
	}
	return data
}()
