package main

// Tests for the generated configuration reference: every CLI flag this binary
// registers must appear in the document that ships with the built-in pigorc skill.

import (
	"strings"
	"testing"

	flag "github.com/spf13/pflag"

	"github.com/smallnest/pigo/internal/cli/config"
)

// TestReferenceDocumentsEveryCLIFlag renders the reference from the real flag set
// and checks every registered flag appears.
func TestReferenceDocumentsEveryCLIFlag(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var opts cliOptions
	registerFlags(fs, &opts)

	docs := flagDocs(fs)
	if len(docs) < 20 {
		t.Fatalf("flag docs = %d, want the full CLI surface", len(docs))
	}
	doc := config.ReferenceMarkdown(config.ReferenceOptions{Version: "test", Flags: docs})
	for _, d := range docs {
		if !strings.Contains(doc, "`--"+d.Name) {
			t.Errorf("flag --%s is registered but missing from the reference", d.Name)
		}
	}
	// Spot-check the rendering of a shorthand and a default.
	for _, want := range []string{"`--model, -m`", "`--thinking-level`", "`openrouter/free`"} {
		if !strings.Contains(doc, want) {
			t.Errorf("reference missing %q", want)
		}
	}
}

// TestFlagDocsOrderStable verifies docs are emitted in pflag's lexical VisitAll
// order, which keeps the generated file byte-stable across launches.
func TestFlagDocsOrderStable(t *testing.T) {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	var opts cliOptions
	registerFlags(fs, &opts)
	docs := flagDocs(fs)
	if docs[0].Name != "allowed-tools" {
		t.Errorf("first flag = %q, want allowed-tools (lexical order)", docs[0].Name)
	}
	for i := 1; i < len(docs); i++ {
		if docs[i-1].Name > docs[i].Name {
			t.Fatalf("flag docs not sorted: %q before %q", docs[i-1].Name, docs[i].Name)
		}
	}
}
