package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/roberChen/pigo/internal/hostapi"
	"github.com/roberChen/pigo/internal/hostclient"
	"github.com/roberChen/pigo/internal/hostpath"
)

// runner owns one harness run: the real host process, the mock provider, all
// client slots, health monitoring and the artifact set.
type runner struct {
	cfg       Config
	outputDir string
	homeDir   string
	workDir   string
	binary    string
	mock      *mockLLM
	log       *eventLog
	protocol  *protocolLog
	metrics   metrics

	hostCmd     *exec.Cmd
	hostExited  chan error
	hostExitErr error
	hostLog     *os.File
	monitor     *hostclient.Client
	// statesMu guards states. The monitor goroutine appends client slots while the
	// main goroutine snapshots them for verification, so an unguarded slice would
	// be a data race (and appending past the cap would silently exceed
	// -max-clients).
	statesMu  sync.Mutex
	states    []*clientState
	clients   sync.WaitGroup
	clientCtx context.Context

	connectedClients atomic.Int64
}

// main parses configuration, runs until the deadline or an interrupt, and exits
// non-zero on reliability failures.
func main() {
	cfg := parseConfig()
	if err := cfg.validate(); err != nil {
		fmt.Fprintln(os.Stderr, "longstay:", err)
		os.Exit(2)
	}
	if cfg.OutputDir == "" {
		cfg.OutputDir = filepath.Join("e2e-results", "longstay-"+time.Now().UTC().Format("20060102-150405"))
	}
	r, err := newRunner(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "longstay:", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	code := r.run(ctx)
	os.Exit(code)
}

// newRunner prepares the artifact directory and starts the mock LLM server.
func newRunner(cfg Config) (*runner, error) {
	output, err := filepath.Abs(cfg.OutputDir)
	if err != nil {
		return nil, err
	}
	r := &runner{
		cfg:        cfg,
		outputDir:  output,
		homeDir:    filepath.Join(output, "home"),
		workDir:    filepath.Join(output, "work"),
		hostExited: make(chan error, 1),
	}
	for _, dir := range []string{output, r.homeDir, r.workDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	if currentProfile.enabled("memory_search") {
		if err := prepareMemoryHome(r.homeDir); err != nil {
			return nil, err
		}
	}
	if err := writeWorkspaceFixtures(r.workDir, cfg.WorkspacePool, cfg.MaxClients); err != nil {
		return nil, err
	}
	log, err := openEventLog(filepath.Join(output, "events.jsonl"))
	if err != nil {
		return nil, err
	}
	r.log = log
	protocol, err := openProtocolLog(filepath.Join(output, "protocol-errors.log"))
	if err != nil {
		_ = log.Close()
		return nil, err
	}
	r.protocol = protocol
	if err := r.writeConfig(); err != nil {
		_ = protocol.Close()
		_ = log.Close()
		return nil, err
	}
	mock, err := startMockLLM(cfg.LLMLatency, cfg.LLMJitter, cfg.LLMErrorRate, validateOptions{
		Model:  "mock-chat",
		APIKey: "longstay",
		Tools:  cfg.toolsEnabled(),
	}, log)
	if err != nil {
		_ = protocol.Close()
		_ = log.Close()
		return nil, err
	}
	r.mock = mock
	currentProfile.FixtureURL = mock.FixtureURL()
	return r, nil
}

// writeConfig records the exact scenario so a failure can be reproduced.
func (r *runner) writeConfig() error {
	data, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.outputDir, "config.json"), data, 0o600)
}

// run executes the full lifecycle and returns the process exit code.
func (r *runner) run(parent context.Context) int {
	defer r.cleanup()
	if err := r.startHost(parent); err != nil {
		r.log.write("host_start_error", map[string]any{"error": err.Error()})
		fmt.Fprintln(os.Stderr, "longstay:", err)
		return r.finish(false)
	}
	r.metrics.startGoroutines = runtime.NumGoroutine()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	r.metrics.startHeapBytes = mem.HeapAlloc

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	r.clientCtx = ctx
	timer := time.AfterFunc(r.cfg.Duration, cancel)
	defer timer.Stop()

	r.log.write("run_started", map[string]any{"seed": r.cfg.Seed, "duration": r.cfg.Duration.String()})
	for i := 0; i < r.cfg.InitialClients; i++ {
		r.addClient()
	}
	go r.printSummaries(ctx)
	// Exactly one monitor: it both spawns clients up to the cap and probes host
	// health. Starting it twice (as this used to) doubled the join rate past
	// -max-clients and double-counted every health failure.
	healthDone := make(chan struct{})
	go func() {
		r.monitorHealth(ctx)
		close(healthDone)
	}()

	select {
	case <-ctx.Done():
	case err := <-r.hostExited:
		r.metrics.hostErrors.Add(1)
		r.hostExitErr = err
		r.log.write("host_exited_early", map[string]any{"error": fmt.Sprint(err)})
		cancel()
	}
	r.clients.Wait()
	cancel()
	<-healthDone
	// Verification runs after the clients have stopped, over a snapshot: the
	// count in the summary must match the clients actually checked.
	states := r.clientStates()
	for _, state := range states {
		state.verifyReconnect()
		state.exit("verification complete")
	}
	r.metrics.endGoroutines = runtime.NumGoroutine()
	runtime.ReadMemStats(&mem)
	r.metrics.endHeapBytes = mem.HeapAlloc
	return r.finish(r.healthy())
}

// startHost builds pigo if needed and starts the real daemon over its native
// transport (a named pipe on Windows, a unix socket elsewhere).
func (r *runner) startHost(ctx context.Context) error {
	binary, err := r.resolveBinary(ctx)
	if err != nil {
		return err
	}
	r.binary = binary
	logFile, err := os.Create(filepath.Join(r.outputDir, "server.log"))
	if err != nil {
		return err
	}
	r.hostLog = logFile
	args := []string{
		"serve", "--dir", r.homeDir,
		"--provider", "openai", "-m", "mock-chat",
		"--base-url", r.mock.URL(), "-k", "longstay",
		"--no-skills",
	}
	if !r.cfg.toolsEnabled() {
		args = append(args, "--no-tools")
	}
	cmd := exec.Command(binary, args...)
	cmd.Dir = r.workDir
	cmd.Env = append(os.Environ(), "PIGO_HOME="+r.homeDir, "PIGO_DEBUG=1")
	if currentProfile.enabled("memory_search") {
		cmd.Env = append(cmd.Env, "XDG_CONFIG_HOME="+filepath.Join(r.homeDir, "xdg"))
	}
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return fmt.Errorf("start pigo host: %w", err)
	}
	r.hostCmd = cmd
	go func() { r.hostExited <- cmd.Wait() }()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if hostpath.Probe(hostpath.SocketPath(r.homeDir), 100*time.Millisecond) {
			monitor, err := hostclient.Connect(hostclient.Options{Dir: r.homeDir, Client: "longstay-monitor"})
			if err == nil {
				r.monitor = monitor
				return nil
			}
		}
		select {
		case err := <-r.hostExited:
			r.hostExitErr = err
			return fmt.Errorf("pigo host exited during startup: %v (see server.log)", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	return errors.New("pigo host did not become ready within 20s")
}

// resolveBinary uses -pigo when provided, otherwise builds the current source.
func (r *runner) resolveBinary(ctx context.Context) (string, error) {
	if r.cfg.PigoBinary != "" {
		path, err := filepath.Abs(r.cfg.PigoBinary)
		if err != nil {
			return "", err
		}
		if _, err := os.Stat(path); err != nil {
			return "", fmt.Errorf("pigo binary: %w", err)
		}
		return path, nil
	}
	target := filepath.Join(r.outputDir, "pigo")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	cmd := exec.CommandContext(ctx, "go", "build", "-o", target, "./cmd/pigo")
	cmd.Dir = "."
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build pigo: %w: %s", err, out)
	}
	return target, nil
}

// addClient starts a new independent UI lifecycle, never exceeding -max-clients.
func (r *runner) addClient() {
	r.statesMu.Lock()
	defer r.statesMu.Unlock()
	if len(r.states) >= r.cfg.MaxClients {
		return
	}
	state := newClientState(r, len(r.states)+1)
	r.states = append(r.states, state)
	r.clients.Add(1)
	go state.run(r.clientCtx)
}

// clientStates returns a snapshot of every client slot started so far.
func (r *runner) clientStates() []*clientState {
	r.statesMu.Lock()
	defer r.statesMu.Unlock()
	return append([]*clientState(nil), r.states...)
}

// monitorHealth checks the host and prints periodic progress independently of
// any client's callback or prompt. A single transport failure is not a host
// fault: the socket may be mid-reconnect, so only consecutive failures count
// (and one recovery resets the streak).
func (r *runner) monitorHealth(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	streak := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// New UIs may join while existing sessions are running.
			r.statesMu.Lock()
			slots := len(r.states)
			r.statesMu.Unlock()
			if slots < r.cfg.MaxClients {
				r.addClient()
			}
			if r.monitor == nil {
				continue
			}
			if _, err := r.monitor.ServerStatus(); err != nil {
				streak++
				r.log.write("health_error", map[string]any{"error": err.Error(), "streak": streak})
				if streak >= healthFailureThreshold {
					r.metrics.hostErrors.Add(1)
					streak = 0
				}
				continue
			}
			streak = 0
		}
	}
}

// healthFailureThreshold is how many consecutive failed probes make a host
// failure. Two probes is ~4s with the default interval, which is long enough
// that a transient reconnect never fails a multi-hour run.
const healthFailureThreshold = 3

// printSummaries emits a compact operational view without flooding the log with
// every text delta.
func (r *runner) printSummaries(ctx context.Context) {
	ticker := time.NewTicker(r.cfg.SummaryInterval)
	defer ticker.Stop()
	start := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m := &r.metrics
			fmt.Printf("elapsed=%s clients=%d prompts=%d completed=%d reconnects=%d mock=%d errors=%d/%d/%d gaps=%d\n",
				time.Since(start).Round(time.Second), r.connectedClients.Load(), m.prompts.Load(), m.completed.Load(),
				m.reconnects.Load(), r.mock.requests.Load(), m.protocolErrors.Load(), m.transportErrors.Load(),
				m.hostErrors.Load(), m.missingEvents.Load())
		}
	}
}

// injectedFailure reports whether the mock answered one of this turn's requests
// with a deliberate 500. It is the only evidence that separates an expected
// provider fault from a real defect in the host.
func (r *runner) injectedFailure(token string) bool { return r.mock.injectedFailure(token) }

// toolsEnabled reports whether this run advertises tools. The host flags, the
// mock's validation and every client's plan derive from this one answer, so the
// three can never disagree about a turn's expected shape.
func (r *runner) toolsEnabled() bool { return r.cfg.toolsEnabled() }

// recordProtocol marks a reliability violation with structured evidence. It
// writes to the JSONL log *and* to the bounded sidecar, and prints the finding
// so the CI console carries the reason without a trip to the artifact.
func (r *runner) recordProtocol(kind string, err error) {
	r.metrics.protocolErrors.Add(1)
	r.log.write("protocol_error", map[string]any{"kind": kind, "error": err.Error()})
	r.protocol.record(kind, err)
	fmt.Printf("protocol_error kind=%s error=%s\n", kind, truncate(err.Error(), 300))
}

// healthy evaluates both correctness and whether the churn actually happened.
func (r *runner) healthy() bool {
	m := &r.metrics
	if m.protocolErrors.Load() != 0 ||
		m.transportErrors.Load() != 0 ||
		m.hostErrors.Load() != 0 ||
		m.missingEvents.Load() != 0 ||
		m.replayTruncated.Load() != 0 ||
		m.turnsUnverified.Load() != 0 ||
		m.prompts.Load() == 0 ||
		m.completed.Load() == 0 ||
		m.reconnects.Load() == 0 ||
		m.peakClients.Load() <= 1 ||
		r.mock.requests.Load() == 0 {
		return false
	}
	if !r.cfg.RequireTaskCoverage {
		return true
	}
	if r.mock.taskChildRequests.Load() == 0 || m.subAgentProgress.Load() == 0 || m.subAgentDone.Load() == 0 {
		return false
	}
	for _, key := range requiredTaskChildKeys(currentProfile) {
		if r.mock.taskChildKeyCount(key, false) == 0 {
			return false
		}
	}
	if currentProfile.enabled("task_error") && r.mock.taskChildKeyCount("error", true) == 0 {
		return false
	}
	if currentProfile.enabled("task_async") || currentProfile.enabled("task_output") || currentProfile.enabled("task_output_list") {
		if m.taskOutputList.Load() == 0 || m.taskOutputSingle.Load() == 0 {
			return false
		}
	}
	if (currentProfile.enabled("task_batch") || currentProfile.enabled("task_output_batch")) && m.taskOutputBatch.Load() == 0 {
		return false
	}
	if currentProfile.enabled("task_wait_all") && m.taskOutputWaitAll.Load() == 0 {
		return false
	}
	if taskSessionCoverageExpected(currentProfile) &&
		(r.mock.taskChildCompleted.Load() == 0 || r.mock.taskChildReadChecks.Load() == 0 || m.taskSessionsVerified.Load() == 0) {
		return false
	}
	if currentProfile.enabled("task_auto") && !currentProfile.enabled("task_wait_all") &&
		(r.mock.taskFollowUps.Load() == 0 || m.taskFollowUpsVerified.Load() == 0) {
		return false
	}
	return true
}

// finish writes the final summary and shuts everything down.
func (r *runner) finish(ok bool) int {
	if r.monitor != nil {
		if err := r.monitor.Shutdown(); err != nil && !isCode(err, hostapi.CodeUnavailable) {
			r.metrics.hostErrors.Add(1)
			ok = false
		}
		_ = r.monitor.Close()
	}
	if r.hostCmd != nil && r.hostExitErr == nil {
		select {
		case <-r.hostExited:
		case <-time.After(10 * time.Second):
			_ = r.hostCmd.Process.Kill()
			ok = false
		}
	}
	if r.hostLog != nil {
		_ = r.hostLog.Close()
	}
	if r.mock != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = r.mock.Close(ctx)
		cancel()
	}
	ok = ok && r.healthy()
	summary := r.summary(ok)
	data, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(r.outputDir, "summary.json"), data, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, "longstay: write summary:", err)
		ok = false
	}
	fmt.Printf("%s elapsed=%s prompts=%d reconnects=%d mock=%d errors=%d/%d/%d gaps=%d artifacts=%s\n",
		map[bool]string{true: "PASS", false: "FAIL"}[ok], r.cfg.Duration,
		r.metrics.prompts.Load(), r.metrics.reconnects.Load(), r.mock.requests.Load(),
		r.metrics.protocolErrors.Load(), r.metrics.transportErrors.Load(), r.metrics.hostErrors.Load(),
		r.metrics.missingEvents.Load(), r.outputDir)
	if !ok {
		return 1
	}
	return 0
}

// summary returns the machine-readable final report.
func (r *runner) summary(ok bool) map[string]any {
	m := &r.metrics
	return map[string]any{
		"pass":                  ok,
		"seed":                  r.cfg.Seed,
		"duration":              r.cfg.Duration.String(),
		"clientsStarted":        m.clientsStarted.Load(),
		"peakClients":           m.peakClients.Load(),
		"reconnects":            m.reconnects.Load(),
		"adoptions":             m.adoptions.Load(),
		"prompts":               m.prompts.Load(),
		"completed":             m.completed.Load(),
		"aborted":               m.aborted.Load(),
		"providerErrors":        m.providerErrors.Load(),
		"protocolErrors":        m.protocolErrors.Load(),
		"transportErrors":       m.transportErrors.Load(),
		"hostErrors":            m.hostErrors.Load(),
		"duplicates":            m.duplicates.Load(),
		"verifications":         m.verifications.Load(),
		"verificationSkips":     m.verificationSkips.Load(),
		"missingEvents":         m.missingEvents.Load(),
		"queueOverflows":        m.queueOverflows.Load(),
		"replayTruncated":       m.replayTruncated.Load(),
		"toolVerifications":     m.toolVerifications.Load(),
		"degradedResults":       m.degradedResults.Load(),
		"subAgentProgress":      m.subAgentProgress.Load(),
		"subAgentDone":          m.subAgentDone.Load(),
		"taskSessionsVerified":  m.taskSessionsVerified.Load(),
		"taskFollowUpsVerified": m.taskFollowUpsVerified.Load(),
		"taskOutputList":        m.taskOutputList.Load(),
		"taskOutputSingle":      m.taskOutputSingle.Load(),
		"taskOutputBatch":       m.taskOutputBatch.Load(),
		"taskOutputWaitAll":     m.taskOutputWaitAll.Load(),
		"turnsVerified":         m.turnsVerified.Load(),
		"turnsUnverified":       m.turnsUnverified.Load(),
		"turnsDegraded":         m.turnsDegraded.Load(),
		"turnsCompacted":        m.turnsCompacted.Load(),
		"transcriptsVerified":   m.transcriptsVerified.Load(),
		"scenario":              r.cfg.Scenario,
		"workspacePool":         r.cfg.WorkspacePool,
		"mockRequests":          r.mock.requests.Load(),
		"mockFailures":          r.mock.failures.Load(),
		"mockPeakActive":        r.mock.peak.Load(),
		"taskChildRequests":     r.mock.taskChildRequests.Load(),
		"taskChildCompleted":    r.mock.taskChildCompleted.Load(),
		"taskChildReadChecks":   r.mock.taskChildReadChecks.Load(),
		"taskChildFailures":     r.mock.taskChildFailures.Load(),
		"taskFollowUps":         r.mock.taskFollowUps.Load(),
		"taskChildren":          r.mock.taskChildKeySnapshot(false),
		"taskFailedChildren":    r.mock.taskChildKeySnapshot(true),
		"startGoroutines":       m.startGoroutines,
		"endGoroutines":         m.endGoroutines,
		"startHeapBytes":        m.startHeapBytes,
		"endHeapBytes":          m.endHeapBytes,
		"artifacts":             r.outputDir,
	}
}

// cleanup closes the event log even when startup fails halfway through.
func (r *runner) cleanup() {
	if r.log != nil {
		_ = r.protocol.Close()
		_ = r.log.Close()
	}
}
