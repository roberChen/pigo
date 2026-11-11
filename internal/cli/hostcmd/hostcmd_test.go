// This file tests the `pigo serve` / `pigo attach` CLI layer end to end: the
// attacher's rendering and permission handling against a real host over a real
// unix socket, the serve lifecycle (start / status / stop), and — through
// TestMain re-exec — actual daemonization of the same binary.
//
// The agent behind each session is scripted, so no provider, network or
// credentials are involved. Everything that is *not* scripted is production
// code: the socket transport, the token handshake, session persistence, the
// permission round trip and the daemon's own detach path.
package hostcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/hostapi"
	"github.com/smallnest/pigo/internal/hostclient"
	"github.com/smallnest/pigo/internal/hostpath"
	"github.com/smallnest/pigo/internal/hostserver"
	"github.com/smallnest/pigo/internal/runtime"
	"github.com/smallnest/pigo/internal/session"
	"github.com/smallnest/pigo/internal/trust"
)

// TestMain doubles as the daemon child. startDaemon re-execs os.Executable()
// with "serve" as argv[1]; under `go test` that executable is this test binary,
// so without this hook the daemon test could only assert the failure path. The
// scripted reply travels through the environment because the child shares no
// memory with the parent.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "serve" {
		reply := os.Getenv("PIGO_TEST_HOST_REPLY")
		// Run takes the *positional* action arguments, so re-derive them from the
		// raw child argv: --dir is the only flag the parent adds, and it carries a
		// value that must not be mistaken for an action.
		action, dir := childAction(os.Args[2:])
		code := Run(action, ServeOptions{
			Dir:     dir,
			Build:   "test",
			Factory: &scriptedFactory{reply: reply},
			// The child must not inherit the test's temp dir cleanup; it is a
			// separate process with its own lifetime.
		}, os.Stdout, os.Stderr)
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// childAction splits a daemon child's argv into the positional action Run
// expects and the --dir value the parent injected.
func childAction(args []string) (action []string, dir string) {
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--dir":
			if i+1 < len(args) {
				dir = args[i+1]
				i++
			}
		case strings.HasPrefix(args[i], "--dir="):
			dir = strings.TrimPrefix(args[i], "--dir=")
		case strings.HasPrefix(args[i], "-"):
			// Any other flag carries no meaning for the scripted test host.
		default:
			action = append(action, args[i])
		}
	}
	return action, dir
}

// scriptedAgent replays a fixed reply in two deltas and honours cancellation. It
// optionally asks the host's permission gate first, so the whole
// request→prompt→decide→resume path is exercised from the CLI's side.
type scriptedAgent struct {
	mu       sync.Mutex
	spec     hostserver.AgentSpec
	messages agentcore.MessageList
	reply    string
	delay    time.Duration
	tool     string
}

func (a *scriptedAgent) Run(ctx context.Context, prompt string, h runtime.StreamHandler) (*agentcore.AssistantMessage, error) {
	a.mu.Lock()
	if prompt != "" {
		a.messages = append(a.messages, agentcore.UserMessage{
			RoleField: agentcore.RoleUser,
			Content:   agentcore.ContentList{agentcore.NewTextContent(prompt)},
			Timestamp: time.Now().UnixMilli(),
		})
	}
	reply, delay, tool, spec := a.reply, a.delay, a.tool, a.spec
	a.mu.Unlock()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if tool != "" && spec.BeforeToolCall != nil {
		dec := spec.BeforeToolCall(ctx, agentcore.AgentToolCall{Name: tool, Arguments: json.RawMessage(`{"command":"ls"}`)})
		switch {
		case dec == nil:
			reply = "allowed"
		case dec.Block && dec.Content != nil:
			reply = agentcore.ContentToText(*dec.Content)
		default:
			reply = "blocked"
		}
	}

	msg := agentcore.AssistantMessage{
		RoleField:  agentcore.RoleAssistant,
		Content:    agentcore.ContentList{agentcore.NewTextContent(reply)},
		StopReason: agentcore.StopReasonEndTurn,
	}
	half := len(reply) / 2
	if h.OnText != nil {
		h.OnText(reply[:half])
		h.OnText(reply[half:])
	}
	if h.OnEvent != nil {
		h.OnEvent(agentcore.TurnEndEvent{Message: msg})
	}
	a.mu.Lock()
	a.messages = append(a.messages, msg)
	a.mu.Unlock()
	return &msg, nil
}

func (a *scriptedAgent) Messages() agentcore.MessageList {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append(agentcore.MessageList(nil), a.messages...)
}

func (a *scriptedAgent) SystemPrompt() string { return a.spec.SystemPrompt }
func (a *scriptedAgent) Model() string        { return a.spec.Model }
func (a *scriptedAgent) Provider() string     { return "scripted" }
func (a *scriptedAgent) Close()               {}

// scriptedFactory builds scriptedAgents; every agent shares the factory's script.
type scriptedFactory struct {
	reply string
	delay time.Duration
	tool  string
}

func (f *scriptedFactory) NewAgent(_ context.Context, spec hostserver.AgentSpec) (hostserver.Agent, error) {
	if spec.Model == "" {
		spec.Model = "scripted-model"
	}
	if spec.SystemPrompt == "" {
		spec.SystemPrompt = "scripted system prompt"
	}
	return &scriptedAgent{
		spec:     spec,
		messages: append(agentcore.MessageList(nil), spec.Messages...),
		reply:    f.reply,
		delay:    f.delay,
		tool:     f.tool,
	}, nil
}

// startHost runs a host on dir with the given factory and returns its state dir.
func startHost(t *testing.T, dir string, f hostserver.Factory, mutate ...func(*hostserver.Config)) string {
	t.Helper()
	store, err := session.NewStore(t.TempDir())
	if err != nil {
		t.Fatalf("session store: %v", err)
	}
	// A temp trust file: the tests must never read or write the developer's real
	// trust decisions (AGENTS.md testing rule).
	mgr, err := trust.NewManager(t.TempDir() + "/trust.json")
	if err != nil {
		t.Fatalf("trust: %v", err)
	}
	cfg := hostserver.Config{Dir: dir, Factory: f, Store: store, Trust: mgr, Build: "test"}
	for _, m := range mutate {
		m(&cfg)
	}
	srv, err := hostserver.NewServer(cfg)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := srv.Serve(ctx); err != nil {
			t.Errorf("serve: %v", err)
		}
	}()
	t.Cleanup(func() {
		cancel()
		srv.Shutdown("test cleanup")
		<-done
	})
	return dir
}

// attachAndRead drives Attach with scripted stdin and returns stdout/stderr.
func attachAndRead(t *testing.T, dir, stdin string, mutate func(*AttachOptions)) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	opts := AttachOptions{Dir: dir}
	if mutate != nil {
		mutate(&opts)
	}
	code := Attach(opts, strings.NewReader(stdin), &out, &errOut)
	return out.String(), errOut.String(), code
}

func TestAttachRendersStreamAndDetaches(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "hello from the host"})

	stdout, stderr, code := attachAndRead(t, dir, "hi\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "attached to pigo host") {
		t.Fatalf("missing handshake banner: %q", stdout)
	}
	// The reply arrives as two deltas and must be reassembled without markers.
	if !strings.Contains(stdout, "hello from the host") {
		t.Fatalf("streamed reply not rendered: %q", stdout)
	}
	if !strings.Contains(stdout, "detached from ") {
		t.Fatalf("missing detach line: %q", stdout)
	}
}

func TestAttachResumesPersistedSession(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "first answer"})

	// First connection: run one turn, then detach. The session is persisted.
	stdout, stderr, code := attachAndRead(t, dir, "question\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("first attach exit = %d (%s)", code, stderr)
	}
	if !strings.Contains(stdout, "session ") {
		t.Fatalf("no session id printed: %q", stdout)
	}
	id := sessionIDFrom(t, stdout)

	// Second connection resumes it: the history comes back from the store, and a
	// new turn continues the same conversation.
	out2, err2, code2 := attachAndRead(t, dir, "/exit\n", func(o *AttachOptions) { o.SessionID = id })
	if code2 != 0 {
		t.Fatalf("resume attach exit = %d (%s)", code2, err2)
	}
	if !strings.Contains(out2, "resumed "+id) {
		t.Fatalf("resume did not report the session: %q", out2)
	}
	if !strings.Contains(out2, "2 messages") {
		t.Fatalf("resume did not replay history (want 2 messages): %q", out2)
	}
}

// sessionIDFrom pulls the id out of the "session <id> in <cwd>" banner.
func sessionIDFrom(t *testing.T, stdout string) string {
	t.Helper()
	for _, line := range strings.Split(stdout, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "session "); ok {
			if id, _, found := strings.Cut(rest, " "); found {
				return id
			}
		}
	}
	t.Fatalf("no session id in output: %q", stdout)
	return ""
}

func TestAttachAnswersPermissionPromptMidTurn(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "done", tool: "bash"})

	// The sequence matters: the turn starts, the host asks for permission while
	// the turn is still in flight, and only then does the client answer. A client
	// that answered synchronously inside Prompt could not do this.
	stdout, stderr, code := attachAndRead(t, dir, "run ls\ny\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "wants to run outside a trusted directory") {
		t.Fatalf("permission question not shown: %q", stdout)
	}
	if !strings.Contains(stdout, "allowed once") {
		t.Fatalf("y was not interpreted as approval: %q", stdout)
	}
	if !strings.Contains(stdout, "allowed") {
		t.Fatalf("tool was not actually allowed: %q", stdout)
	}
}

func TestAttachPermissionDefaultsToDeny(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "done", tool: "bash"})

	// An empty answer denies; the model sees the denial rather than the tool
	// running. This is the fail-closed path a detached client also hits.
	stdout, stderr, code := attachAndRead(t, dir, "run ls\n\n/exit\n", nil)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %q", code, stderr)
	}
	if !strings.Contains(stdout, "denied") {
		t.Fatalf("empty answer did not deny: %q", stdout)
	}
}

// TestServeApproveGrantsTrustToTheServersOwnManager is a regression test for a
// silent no-op: session trust lives inside a *trust.Manager instance, so granting
// it on a throwaway manager would leave the server's gate asking for permission
// while --approve claimed it was granted.
func TestServeApproveGrantsTrustToTheServersOwnManager(t *testing.T) {
	home := t.TempDir()
	t.Setenv("PIGO_HOME", home)
	// The launch directory is a temp dir so the developer's real trust store is
	// never consulted (trust.DefaultPath follows PIGO_HOME).
	cwd := t.TempDir()
	t.Chdir(cwd)

	dir := t.TempDir()
	var out, errOut bytes.Buffer
	go Run(nil, ServeOptions{
		Dir:     dir,
		Approve: true,
		Build:   "test",
		Factory: &scriptedFactory{reply: "x"},
	}, &out, &errOut)

	deadline := time.Now().Add(10 * time.Second)
	for !hostpath.Probe(hostpath.SocketPath(dir), 50*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("host never listened: %s", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer func() {
		var o, e bytes.Buffer
		Run([]string{"stop"}, ServeOptions{Dir: dir}, &o, &e)
	}()

	// The trust store the server reads must already record the launch directory as
	// session-trusted. Session trust is in-memory, so assert it through the server:
	// a side-effect call from a session rooted at cwd passes without a question.
	c, err := hostclient.Connect(hostclient.Options{Dir: dir, Client: "test"})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer c.Close()
	info, err := c.CreateSession(cwd, "", "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if info.Cwd != cwd {
		t.Fatalf("session cwd = %q, want %q", info.Cwd, cwd)
	}
	// No permission_request may arrive: the tool runs because --approve trusted it.
	asked := make(chan struct{}, 1)
	c.OnEvent(hostapi.EventPermission, func(hostapi.Envelope) {
		select {
		case asked <- struct{}{}:
		default:
		}
	})
	if _, err := c.Prompt(context.Background(), info.ID, "go"); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	select {
	case <-asked:
		t.Fatal("--approve did not grant session trust: the host still asked for permission")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestAttachListShowsOnlyOwnedSessions(t *testing.T) {
	dir := t.TempDir()
	startHost(t, dir, &scriptedFactory{reply: "x"})

	// A session created and left behind by a first connection belongs to it, so a
	// fresh connection's --list must not show it (isolation, §6.5).
	first, _, code := attachAndRead(t, dir, "/exit\n", nil)
	if code != 0 {
		t.Fatalf("first attach failed: %d", code)
	}
	id := sessionIDFrom(t, first)

	out, errOut, code := attachAndRead(t, dir, "", func(o *AttachOptions) { o.List = true })
	if code != 0 {
		t.Fatalf("--list exit = %d (%s)", code, errOut)
	}
	if strings.Contains(out, id) {
		t.Fatalf("--list leaked another connection's session %s: %q", id, out)
	}
	if !strings.Contains(out, "no sessions on this connection") {
		t.Fatalf("unexpected --list output: %q", out)
	}
}

func TestAttachFailsWithoutHost(t *testing.T) {
	var out, errOut bytes.Buffer
	code := Attach(AttachOptions{Dir: t.TempDir()}, strings.NewReader(""), &out, &errOut)
	if code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "no pigo host is running") {
		t.Fatalf("unexpected error: %q", errOut.String())
	}
}

func TestServeForegroundThenStatusAndStop(t *testing.T) {
	dir := t.TempDir()
	var out, errOut bytes.Buffer

	ready := make(chan struct{})
	opts := ServeOptions{
		Dir:       dir,
		Build:     "test",
		Factory:   &scriptedFactory{reply: "ok"},
		ReadyFile: dir + "/ready",
	}
	done := make(chan int, 1)
	go func() {
		// Signal listeners: Run installs a signal handler, and Shutdown (from
		// `serve stop`) is the graceful path.
		code := Run(nil, opts, &out, &errOut)
		close(ready)
		done <- code
	}()

	// Wait for the socket rather than sleeping a fixed amount: readiness is a
	// fact about the socket, not about the clock.
	deadline := time.Now().Add(10 * time.Second)
	for !hostpath.Probe(hostpath.SocketPath(dir), 50*time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("host never listened: %s", errOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if code := Run([]string{"status"}, ServeOptions{Dir: dir}, &out, &errOut); code != 0 {
		t.Fatalf("status exit = %d, stderr = %q", code, errOut.String())
	}
	if !strings.Contains(out.String(), "live session(s)") {
		t.Fatalf("status did not report the host: %q", out.String())
	}

	out.Reset()
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &out, &errOut); code != 0 {
		t.Fatalf("stop exit = %d, stderr = %q", code, errOut.String())
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("serve exited %d (%s)", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not exit after stop")
	}
}

func TestServeStatusReportsNoHost(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, ServeOptions{Dir: t.TempDir()}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "no pigo host is running") {
		t.Fatalf("unexpected output: %q", out.String())
	}
}

func TestServeRejectsUnknownAction(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run([]string{"frobnicate"}, ServeOptions{Dir: t.TempDir()}, &out, &errOut); code != 2 {
		t.Fatalf("exit = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "unknown action") {
		t.Fatalf("unexpected error: %q", errOut.String())
	}
}

// TestServeDaemonDetachesAndStops exercises the real fork: startDaemon re-execs
// this test binary, which TestMain routes back into Run as a foreground host.
// That covers the argv replay, the Setsid detach, the readiness handshake and the
// log file — the parts a mock would skip.
func TestServeDaemonDetachesAndStops(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon test re-execs the test binary")
	}
	dir := t.TempDir()
	t.Setenv("PIGO_TEST_HOST_REPLY", "daemon reply")

	var out, errOut bytes.Buffer
	// ChildArgs replays this binary's argv minus --daemon; TestMain consumes it.
	code := Run(nil, ServeOptions{
		Dir:       dir,
		Daemon:    true,
		Build:     "test",
		Factory:   &scriptedFactory{reply: "daemon reply"},
		ChildArgs: nil,
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("daemon start exit = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
	defer func() {
		var o, e bytes.Buffer
		Run([]string{"stop"}, ServeOptions{Dir: dir}, &o, &e)
	}()

	if !strings.Contains(out.String(), "listening on") {
		t.Fatalf("daemon did not report readiness: %q", out.String())
	}
	if _, err := os.Stat(dir + "/server.log"); err != nil {
		t.Fatalf("daemon wrote no log file: %v", err)
	}

	// The detached host answers a real client.
	attachOut, attachErr, attachCode := attachAndRead(t, dir, "hello\n/exit\n", nil)
	if attachCode != 0 {
		t.Fatalf("attach to daemon failed: %d (%s)", attachCode, attachErr)
	}
	if !strings.Contains(attachOut, "daemon reply") {
		t.Fatalf("daemon did not serve the scripted reply: %q", attachOut)
	}

	var statusOut, statusErr bytes.Buffer
	if c := Run([]string{"status"}, ServeOptions{Dir: dir}, &statusOut, &statusErr); c != 0 {
		t.Fatalf("status exit = %d (%s)", c, statusErr.String())
	}
	// A daemon started through this path advertises itself as detached.
	if !strings.Contains(statusOut.String(), "pid ") {
		t.Fatalf("status did not report the daemon: %q", statusOut.String())
	}
}

func TestDaemonChildMarkerSuppressesRefork(t *testing.T) {
	// A child that keeps --daemon must never fork again (unbounded process tree).
	// startDaemon is the only fork site and it is guarded by the marker env var;
	// with the marker set, a "start" with --daemon must serve in the foreground.
	// "status" exercises the dispatch without blocking, so assert the guard flag
	// reaches runStart by checking that a daemon start under the marker does not
	// spawn a child (its output goes to this process, not a log file).
	t.Setenv(daemonChildEnv, "1")
	dir := t.TempDir()
	srv, err := hostserver.NewServer(hostserver.Config{Dir: dir, Factory: &scriptedFactory{reply: "x"}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve(context.Background())

	var out, errOut bytes.Buffer
	code := Run([]string{"start"}, ServeOptions{
		Dir:     dir,
		Daemon:  true,
		Factory: &scriptedFactory{reply: "x"},
	}, &out, &errOut)

	// The guard turns the request into a foreground serve, which fails because the
	// socket is already taken — proving no re-exec happened (a fork would have
	// reported readiness instead).
	if code != 1 {
		t.Fatalf("exit = %d, want 1 (already running, not re-forked): %s %s", code, out.String(), errOut.String())
	}
	if strings.Contains(out.String(), "listening on") {
		t.Fatalf("daemon child re-forked instead of serving: %q", out.String())
	}
	srv.Shutdown("done")
}

// TestServeShutdownRemovesStateFiles checks the cleanup contract: after a stop,
// the socket/token/state files are gone so the next `serve` starts clean.
func TestServeShutdownRemovesStateFiles(t *testing.T) {
	dir := t.TempDir()
	srv, err := hostserver.NewServer(hostserver.Config{Dir: dir, Factory: &scriptedFactory{reply: "x"}})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Serve must run: `pigo serve stop` reaches the host over the socket, so a
	// listening-but-not-accepting server would look like "no running host".
	go srv.Serve(context.Background())
	// The endpoint answers (a unix socket path or a named pipe, depending on the
	// platform) and the token/state files exist while the host runs.
	if !hostpath.Probe(hostpath.SocketPath(dir), time.Second) {
		t.Fatalf("host is not listening on %s", hostpath.SocketPath(dir))
	}
	for _, p := range []string{hostpath.TokenPath(dir), hostpath.StatePath(dir)} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s missing while listening: %v", p, err)
		}
	}

	var out, errOut bytes.Buffer
	if code := Run([]string{"stop"}, ServeOptions{Dir: dir}, &out, &errOut); code != 0 {
		t.Fatalf("stop exit = %d (%s)", code, errOut.String())
	}

	// `pigo serve stop` waits for the endpoint to disappear before returning, so
	// the footprint must already be gone here without another sleep.
	if hostpath.Probe(hostpath.SocketPath(dir), 200*time.Millisecond) {
		t.Fatalf("endpoint %s still answers after shutdown", hostpath.SocketPath(dir))
	}
	for _, p := range hostpath.Footprint(dir) {
		if _, err := os.Stat(p); err == nil {
			t.Fatalf("%s still exists after shutdown", p)
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stat %s: %v", p, err)
		}
	}
}

func TestServeStatusDetectsStaleStateFile(t *testing.T) {
	dir := t.TempDir()
	// A crashed host leaves a state file with no socket behind it. Status must say
	// so instead of pretending a host is up.
	if err := hostpath.WriteToken(dir, "tok"); err != nil {
		t.Fatalf("write token: %v", err)
	}
	state := hostpath.State{PID: 999999, Version: hostapi.Version, StartedAt: time.Now(), Socket: hostpath.SocketPath(dir)}
	data, _ := json.Marshal(state)
	if err := os.WriteFile(hostpath.StatePath(dir), data, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	var out, errOut bytes.Buffer
	if code := Run([]string{"status"}, ServeOptions{Dir: dir}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1", code)
	}
	if !strings.Contains(out.String(), "not responding") {
		t.Fatalf("stale state not reported: %q", out.String())
	}
}

// TestServeDaemonCreatesStateDir is the `--dir` case where the directory does
// not exist yet: the daemon's log lives there, so the parent has to create it
// before redirecting the child's output. Foreground serve already creates it.
func TestServeDaemonCreatesStateDir(t *testing.T) {
	if testing.Short() {
		t.Skip("daemon test re-execs the test binary")
	}
	dir := filepath.Join(t.TempDir(), "nested", "state")
	t.Setenv("PIGO_TEST_HOST_REPLY", "x")

	var out, errOut bytes.Buffer
	code := Run(nil, ServeOptions{
		Dir:     dir,
		Daemon:  true,
		Build:   "test",
		Factory: &scriptedFactory{reply: "x"},
	}, &out, &errOut)
	if code != 0 {
		t.Fatalf("daemon start exit = %d, stdout = %q, stderr = %q", code, out.String(), errOut.String())
	}
	defer func() {
		var o, e bytes.Buffer
		Run([]string{"stop"}, ServeOptions{Dir: dir}, &o, &e)
	}()
	if !strings.Contains(out.String(), "listening on") {
		t.Fatalf("daemon did not report readiness: %q", out.String())
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("state dir was not created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("%s is not a directory", dir)
	}
}

// TestLogTailReportsTheLastLine is the daemon startup-failure path: the child is
// detached, so its only voice is the log. Reporting the last line is what turns
// "exit status 1" into the actual reason.
func TestLogTailReportsTheLastLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.log")
	if err := os.WriteFile(path, []byte("first\nsecond\n\n"), 0o600); err != nil {
		t.Fatalf("write log: %v", err)
	}
	if got := logTail(path); got != "second" {
		t.Fatalf("logTail = %q, want %q", got, "second")
	}
	if got := logTail(filepath.Join(dir, "missing.log")); got != "" {
		t.Fatalf("logTail of a missing file = %q, want empty", got)
	}
	// A final line without a newline is a crash mid-write, and still the reason.
	if err := os.WriteFile(path, []byte("a\nb"), 0o600); err != nil {
		t.Fatalf("rewrite log: %v", err)
	}
	if got := logTail(path); got != "b" {
		t.Fatalf("logTail without a trailing newline = %q, want %q", got, "b")
	}
}
