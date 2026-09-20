// Command jev-mcp serves the TypeSafe Jev evaluate tool to a local MCP client
// over stdio.
//
// The command has one job. Running it starts the Model Context Protocol server
// on stdin and stdout; there is no setup command, no self-update, no remote
// installer, and no way to run an evaluation from a shell. The credential is
// read from a dedicated file and from nowhere else: not from the environment,
// not from a flag, not from a tool argument. One accepted flag names that file,
// so that a test or an explicit installation can point at a different absolute
// path. Evidence selection is available only when the
// process is explicitly started with --enable-selection.
//
// Stdout carries protocol messages only. Usage, errors and diagnostics go to
// stderr, and they never contain a prompt, an evaluation result, or the
// credential.
//
// SIGINT and SIGTERM stop the server: the evaluation in flight is cancelled and
// the process leaves within a bounded time whether or not the client is still
// reading its output.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"jev-mcp/internal/mcp"
	"jev-mcp/internal/typesafe"
)

// version is overwritten by release builds with -ldflags "-X main.version=...".
var version = "dev"

const (
	keyFileFlag         = "--key-file"
	enableSelectionFlag = "--enable-selection"

	// defaultKeyDir and defaultKeyName make up the dedicated credential path
	// under the user's home directory: ~/.config/typesafe/jev-pi.key.
	defaultKeyDir  = ".config/typesafe"
	defaultKeyName = "jev-pi.key"
)

// Exit codes.
const (
	exitFailure         = 1
	exitShutdownOverran = 2
)

// shutdownDeadline bounds the time between the first signal and the process
// actually leaving. The server's own shutdown is bounded well below it; this is
// the backstop for the case where it is not, so that a stuck write or a stuck
// evaluation cannot turn a stop request into a process that never stops.
const shutdownDeadline = 5 * time.Second

const usage = `jev-mcp serves the TypeSafe Jev evaluate tool over stdio (Model Context Protocol).

Usage:
  jev-mcp [--key-file <absolute path>] [--enable-selection]

Running it with no arguments starts the evaluate-only server. The credential is
read from ~/.config/typesafe/jev-pi.key unless --key-file names another absolute
path. --enable-selection additionally exposes select_evidence. No API key is
read from the environment or accepted on the command line.`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "jev-mcp:", err)
		os.Exit(exitFailure)
	}
}

// run wires the command up from streams it is given, so a test can prove that
// nothing but protocol traffic reaches stdout.
func run(args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	// An unset or unreadable home directory is not fatal on its own: it only
	// matters when no explicit path was given.
	home, _ := os.UserHomeDir()

	cfg, err := parseArgs(args, home)
	if err != nil {
		fmt.Fprintln(stderr, usage)
		return err
	}

	key, err := typesafe.ReadKey(cfg.keyFile)
	if err != nil {
		// The API package reports what is wrong with the credential file
		// without naming the file or quoting its contents.
		return fmt.Errorf("credential: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go watchdog(ctx, stop, shutdownDeadline, stderr, os.Exit)

	server := mcp.NewServer(typesafe.NewClient(key), mcp.Options{
		Version:         version,
		Logf:            logger(stderr),
		EnableSelection: cfg.enableSelection,
	})
	return server.Serve(ctx, stdin, stdout)
}

// watchdog makes a stop request final. Once the context is cancelled, which
// happens on the first SIGINT or SIGTERM, it restores the default signal
// disposition so a second signal is no longer swallowed by the handler, and it
// ends the process itself if the orderly shutdown overruns its budget.
//
// It returns without doing anything if the process leaves first, because the
// process leaving is what stops this goroutine.
func watchdog(ctx context.Context, restoreSignals func(), after time.Duration, stderr io.Writer, exit func(int)) {
	<-ctx.Done()
	restoreSignals()

	timer := time.NewTimer(after)
	defer timer.Stop()
	<-timer.C

	fmt.Fprintln(stderr, "jev-mcp: shutdown did not finish within its budget; exiting now")
	exit(exitShutdownOverran)
}

// logger returns the server's diagnostic sink. It writes to stderr because
// stdout carries the protocol.
func logger(stderr io.Writer) func(format string, args ...any) {
	return func(format string, args ...any) {
		fmt.Fprintln(stderr, "jev-mcp: "+fmt.Sprintf(format, args...))
	}
}

type config struct {
	keyFile         string
	enableSelection bool
}

// parseArgs accepts the key-file forms and the valueless --enable-selection
// opt-in, and rejects everything else, including help and version flags: this binary is started by
// an adapter, not typed at a prompt, and an unrecognised argument is more
// likely a mistake in a client configuration than a request for help.
//
// The offending argument is never echoed. Arguments are visible in the process
// table, so a message that repeats one risks copying something that should not
// have been there into a log.
func parseArgs(args []string, home string) (config, error) {
	var cfg config
	for i := 0; i < len(args); i++ {
		var value string
		switch arg := args[i]; {
		case arg == enableSelectionFlag:
			if cfg.enableSelection {
				return config{}, fmt.Errorf("%s may be given only once", enableSelectionFlag)
			}
			cfg.enableSelection = true
			continue
		case arg == keyFileFlag:
			if i+1 >= len(args) {
				return config{}, fmt.Errorf("%s requires an absolute path", keyFileFlag)
			}
			i++
			value = args[i]
		case strings.HasPrefix(arg, keyFileFlag+"="):
			value = strings.TrimPrefix(arg, keyFileFlag+"=")
		default:
			return config{}, fmt.Errorf("unrecognized argument: accepted flags are %s <absolute path> and %s", keyFileFlag, enableSelectionFlag)
		}
		if cfg.keyFile != "" {
			return config{}, fmt.Errorf("%s may be given only once", keyFileFlag)
		}
		if !filepath.IsAbs(value) {
			return config{}, fmt.Errorf("%s requires an absolute path", keyFileFlag)
		}
		cfg.keyFile = filepath.Clean(value)
	}

	if cfg.keyFile == "" {
		if !filepath.IsAbs(home) {
			return config{}, errors.New("the home directory is unknown, so the default credential path cannot be resolved; pass " + keyFileFlag + " <absolute path>")
		}
		cfg.keyFile = filepath.Join(home, defaultKeyDir, defaultKeyName)
	}
	return cfg, nil
}
