// This file implements the line REPL's side of the human-in-the-loop seam: when
// the question tool asks the operator something, the question is printed and the
// answer is read from the same buffered input the prompt and the tool-call
// confirmation use. Reads are serialized with the confirmation prompt's mutex so
// two prompts can never interleave on the shared reader.
package repl

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/smallnest/pigo/internal/agentcore"
	"github.com/smallnest/pigo/internal/cli/ui"
)

// askUserREPL prints the model's question and reads one line as the answer. A
// numeric answer selects that option; anything else is taken verbatim. EOF is an
// error so the tool reports it back to the model instead of waiting forever.
//
// A run canceled while this is blocked (Ctrl+C) cannot interrupt the read itself
// — the same limitation the tool-confirmation prompt has — so the answer is
// simply discarded once it arrives.
func askUserREPL(out io.Writer, deps replDeps, req agentcore.AskRequest) (string, error) {
	if deps.confirmMu != nil {
		deps.confirmMu.Lock()
		defer deps.confirmMu.Unlock()
	}
	color := ui.Enabled()
	fmt.Fprintf(out, "\n%s %s\n", ui.Colorize(color, ui.Cyan, "?"), req.Question)
	for i, opt := range req.Options {
		fmt.Fprintf(out, "  %s %s\n", ui.Colorize(color, ui.Bold, fmt.Sprintf("%d)", i+1)), opt)
	}
	if len(req.Options) > 0 {
		fmt.Fprint(out, "answer (number or text): ")
	} else {
		fmt.Fprint(out, "answer: ")
	}
	if deps.in == nil {
		return "", fmt.Errorf("no input available")
	}
	line, err := deps.in.ReadString('\n')
	if err != nil && strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("input closed")
	}
	answer := strings.TrimSpace(line)
	if n, convErr := strconv.Atoi(answer); convErr == nil && n >= 1 && n <= len(req.Options) {
		return req.Options[n-1], nil
	}
	return answer, nil
}

// withAsker installs the REPL asker on the turn's context, so the question tool
// can reach the operator. It is a tiny wrapper that keeps streamRun readable.
func withAsker(ctx context.Context, out io.Writer, deps replDeps) context.Context {
	return agentcore.WithAskUser(ctx, func(_ context.Context, req agentcore.AskRequest) (string, error) {
		return askUserREPL(out, deps, req)
	})
}
