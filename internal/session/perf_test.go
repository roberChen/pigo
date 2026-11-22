// This file tests the session persistence measurements: every save and append
// must publish a stage (with the bytes the file rewrite moved) so a growing
// session's per-turn cost is visible in /perf.
package session

import (
	"testing"

	"github.com/roberChen/pigo/internal/perf"
)

// TestSaveRecordsWriteStage saves a session and asserts session.write carries the
// size of the file it rewrote.
func TestSaveRecordsWriteStage(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)

	s := newStore(t)
	if err := s.Save(SessionHeader{ID: "sess-write"}, sampleMessages()); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var stage *perf.Stage
	for _, st := range perf.Snapshot().Stages {
		if st.Name == "session.write" {
			got := st
			stage = &got
		}
	}
	if stage == nil {
		t.Fatalf("session.write not recorded")
	}
	if stage.Count != 1 {
		t.Errorf("count = %d, want 1", stage.Count)
	}
	if stage.Bytes <= 0 {
		t.Errorf("bytes = %d, want the written file size", stage.Bytes)
	}
	if got := perf.Gauge("session.bytes"); got != stage.Bytes {
		t.Errorf("session.bytes gauge = %d, want %d", got, stage.Bytes)
	}
}

// TestAppendRecordsAppendStage covers the per-turn read-modify-write path, which
// is the one whose cost grows with the stored conversation.
func TestAppendRecordsAppendStage(t *testing.T) {
	perf.Reset()
	t.Cleanup(perf.Reset)

	s := newStore(t)
	h := SessionHeader{ID: "sess-append"}
	if err := s.Save(h, sampleMessages()); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Append(h.ID, h.UpdatedAt, sampleMessages()); err != nil {
		t.Fatalf("Append: %v", err)
	}

	seen := map[string]int64{}
	for _, st := range perf.Snapshot().Stages {
		seen[st.Name] = st.Count
	}
	if seen["session.append"] != 1 {
		t.Errorf("session.append count = %d, want 1", seen["session.append"])
	}
	// Save + Append each do one atomic write.
	if seen["session.write"] != 2 {
		t.Errorf("session.write count = %d, want 2", seen["session.write"])
	}
}
