package main

import (
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// metrics contains only lock-free counters. It is safe to read for periodic
// summaries while client goroutines and the mock server are active.
type metrics struct {
	clientsStarted    atomic.Int64
	reconnects        atomic.Int64
	prompts           atomic.Int64
	completed         atomic.Int64
	aborted           atomic.Int64
	providerErrors    atomic.Int64
	protocolErrors    atomic.Int64
	transportErrors   atomic.Int64
	hostErrors        atomic.Int64
	duplicates        atomic.Int64
	missingEvents     atomic.Int64
	queueOverflows    atomic.Int64
	replayTruncated   atomic.Int64
	adoptions         atomic.Int64
	verifications     atomic.Int64
	verificationSkips atomic.Int64
	peakClients       atomic.Int64
	startGoroutines   int
	endGoroutines     int
	startHeapBytes    uint64
	endHeapBytes      uint64
}

// eventLog serializes state transitions as JSONL. It intentionally excludes
// streamed text, so a multi-hour run does not duplicate the whole transcript.
type eventLog struct {
	mu   sync.Mutex
	file *os.File
	enc  *json.Encoder
}

// openEventLog creates a JSONL artifact.
func openEventLog(path string) (*eventLog, error) {
	file, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	return &eventLog{file: file, enc: json.NewEncoder(file)}, nil
}

// write appends one event. Errors are ignored only because the final summary
// checks file sync; a diagnostic is printed so an unavailable disk is visible.
func (l *eventLog) write(kind string, fields map[string]any) {
	if l == nil {
		return
	}
	entry := make(map[string]any, len(fields)+2)
	entry["time"] = time.Now().UTC().Format(time.RFC3339Nano)
	entry["kind"] = kind
	for key, value := range fields {
		entry[key] = value
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.enc.Encode(entry); err != nil {
		// Keep the runner alive long enough to produce its final failure report.
		println("longstay: event log:", err.Error())
	}
}

// Close flushes and closes the artifact.
func (l *eventLog) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.file.Sync(); err != nil {
		_ = l.file.Close()
		return err
	}
	return l.file.Close()
}

// countPeak updates a maximum without a separate mutex.
func countPeak(counter *atomic.Int64, value int64) {
	for {
		old := counter.Load()
		if value <= old || counter.CompareAndSwap(old, value) {
			return
		}
	}
}
