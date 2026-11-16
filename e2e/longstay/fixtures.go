// This file implements the workspace fixtures the tool plans read. Every
// workspace gets the same three files with the same markers, so a tool result
// can be checked against the plan without knowing which directory the session
// ran in (and a shared workspace verifies exactly like a private one).
package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// Fixture markers. plan.go's plannedTools names them; keep the two in step.
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
	}
	return nil
}
