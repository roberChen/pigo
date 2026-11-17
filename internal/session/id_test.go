package session

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNewIDUniqueAtSameTimestamp(t *testing.T) {
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	const count = 256
	ids := make(chan string, count)
	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ids <- NewID(now)
		}()
	}
	wg.Wait()
	close(ids)

	seen := make(map[string]bool, count)
	for id := range ids {
		if !strings.HasPrefix(id, "20260102-030405-123456-") {
			t.Fatalf("NewID lost its timestamp stem: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate session id %q", id)
		}
		seen[id] = true
	}
	if len(seen) != count {
		t.Fatalf("generated %d unique ids, want %d", len(seen), count)
	}
}
