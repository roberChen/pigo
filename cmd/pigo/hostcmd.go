// This file implements the `pigo serve` and `pigo attach` subcommand dispatch
// (docs/architecture.md §6): a standalone flag surface, separate from the
// agent-mode flags, that parses the subcommand, resolves the same configuration
// layers a normal run would, and hands the result to internal/cli/hostcmd.
//
// It lives beside main.go rather than inside it so main.go stays a dispatch
// table: "does this argv belong to a subcommand?" and nothing else.
package main

import (
	"fmt"
	"io"
	"os"

	flag "github.com/spf13/pflag"

	"github.com/smallnest/pigo/internal/cli/hostcmd"
	"github.com/smallnest/pigo/internal/cli/run"
	"github.com/smallnest/pigo/internal/diag"
)

// isHostSubcommand reports whether args selects the host verbs, i.e.
// `pigo serve ...` or `pigo attach ...`.
func isHostSubcommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	return args[1] == "serve" || args[1] == "attach"
}

// runHostSubcommand dispatches `serve`/`attach` and returns the process exit
// code. args is os.Args.
func runHostSubcommand(args []string, version string, out, errOut io.Writer) int {
	switch args[1] {
	case "serve":
		return runServe(args[2:], version, out, errOut)
	case "attach":
		return runAttach(args[2:], out, errOut)
	}
	return 2
}

// serveFlags is the `pigo serve` surface. It embeds cliOptions so the agent
// flags that affect assembly (model, provider, tools, prompt) mean exactly what
// they mean in a normal run, and adds the host-only ones.
type serveFlags struct {
	cliOptions
	daemon bool
	dir    string
}

func runServe(args []string, version string, out, errOut io.Writer) int {
	var sf serveFlags
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(errOut)
	// Reusing registerFlags keeps the host's flag list, defaults and help text
	// identical to a normal run, so "pigo serve -m X" means what "-m X" means.
	registerFlags(fs, &sf.cliOptions)
	fs.BoolVar(&sf.daemon, "daemon", false, "detach: start the host in the background and return once it is listening")
	fs.StringVar(&sf.dir, "dir", "", "host state directory (socket, token, log); default $PIGO_HOME")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}

	// --cwd must take effect before anything cwd-derived is resolved, exactly as
	// in main: the host's sessions inherit it as their default root.
	if sf.cwd != "" {
		if err := os.Chdir(sf.cwd); err != nil {
			fmt.Fprintf(errOut, "pigo serve: --cwd: %v\n", err)
			return 2
		}
	}
	cwd, _ := os.Getwd()
	resolved, messages := resolveConfig(sf.cliOptions, cwd)
	for _, msg := range messages {
		// A layer complaint is a diagnostic, not output: silent by default, and
		// never painted onto a detaching parent's terminal.
		diag.Diagf("pigo serve: %s", msg)
	}

	factory, err := hostcmd.NewFactory(hostcmd.FactoryOptions{
		Model:              resolved.model,
		BaseURL:            resolved.baseURL,
		Protocol:           resolved.protocol,
		Provider:           resolved.provider,
		APIKey:             resolved.apiKey,
		ThinkingLevel:      resolved.thinkingLevel,
		NoTools:            resolved.noTools,
		NoSkills:           resolved.noSkills,
		Memory:             resolved.memory.Memory.Enabled,
		Policy:             run.NewToolPolicy(resolved.allowedTools, resolved.disallowedTools),
		SystemPrompt:       resolved.systemPrompt,
		AppendSystemPrompt: resolved.appendSystemPrompt,
	})
	if err != nil {
		fmt.Fprintf(errOut, "pigo serve: %v\n", err)
		return 2
	}

	return hostcmd.Run(fs.Args(), hostcmd.ServeOptions{
		Dir:     sf.dir,
		Daemon:  sf.daemon,
		Approve: resolved.approve,
		Build:   version,
		Factory: factory,
		// The daemon child re-execs with the same argv minus --daemon, so it
		// resolves the same providers, flags and config layers as the parent.
		ChildArgs: args,
	}, out, errOut)
}

func runAttach(args []string, out, errOut io.Writer) int {
	var opts hostcmd.AttachOptions
	fs := flag.NewFlagSet("attach", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.StringVar(&opts.Dir, "dir", "", "host state directory; default $PIGO_HOME")
	fs.BoolVarP(&opts.List, "list", "l", false, "list this connection's sessions and exit")
	fs.StringVarP(&opts.Cwd, "cwd", "C", "", "root a new session in this directory (default: the host's directory)")
	fs.StringVarP(&opts.Model, "model", "m", "", "override the model for a new session")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	rest := fs.Args()
	switch len(rest) {
	case 0:
	case 1:
		opts.SessionID = rest[0]
	default:
		fmt.Fprintln(errOut, "usage: pigo attach [session-id] [--list] [--dir DIR]")
		return 2
	}
	if opts.List && opts.SessionID != "" {
		fmt.Fprintln(errOut, "pigo attach: --list does not take a session id")
		return 2
	}
	return hostcmd.Attach(opts, os.Stdin, out, errOut)
}
