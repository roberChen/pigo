package main

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceAndMemoryFixtures(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	if err := writeWorkspaceFixtures(work, 1, 4); err != nil {
		t.Fatalf("writeWorkspaceFixtures: %v", err)
	}
	dir := filepath.Join(work, "client-001")
	for _, name := range []string{"notes.txt", "log.txt", "data.txt", "pixel.png"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("fixture %s: %v", name, err)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "pixel.png"))
	if err != nil {
		t.Fatalf("read pixel fixture: %v", err)
	}
	if _, err := png.DecodeConfig(bytes.NewReader(data)); err != nil {
		t.Fatalf("pixel fixture is not a PNG: %v", err)
	}

	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("create memory home: %v", err)
	}
	if err := prepareMemoryHome(home); err != nil {
		t.Fatalf("prepareMemoryHome: %v", err)
	}
	for _, path := range []string{
		filepath.Join(home, "xdg", "pigo", "config.toml"),
		filepath.Join(home, "memory", "global", "notes.md"),
		filepath.Join(home, "memory", "index.db"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("memory fixture %s: %v", path, err)
		}
	}
}
