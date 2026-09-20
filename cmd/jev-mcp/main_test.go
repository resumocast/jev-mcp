package main

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseArgsDefaultsToTheDedicatedCredentialPath(t *testing.T) {
	cfg, err := parseArgs(nil, "/Users/example")

	if err != nil {
		t.Fatalf("parseArgs() error = %v", err)
	}
	want := filepath.Join("/Users/example", ".config", "typesafe", "jev-pi.key")
	if cfg.keyFile != want {
		t.Errorf("keyFile = %q, want %q", cfg.keyFile, want)
	}
	if cfg.enableSelection {
		t.Error("selection is enabled by default")
	}
}

func TestParseArgsAcceptsAnAbsoluteKeyFile(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "separate value", args: []string{"--key-file", "/tmp/test/jev.key"}, want: "/tmp/test/jev.key"},
		{name: "joined value", args: []string{"--key-file=/tmp/test/jev.key"}, want: "/tmp/test/jev.key"},
		{name: "cleaned", args: []string{"--key-file", "/tmp/test/../test/jev.key"}, want: "/tmp/test/jev.key"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseArgs(tt.args, "/Users/example")
			if err != nil {
				t.Fatalf("parseArgs(%q) error = %v", tt.args, err)
			}
			if cfg.keyFile != tt.want {
				t.Errorf("keyFile = %q, want %q", cfg.keyFile, tt.want)
			}
		})
	}
}

func TestParseArgsAcceptsSelectionOptIn(t *testing.T) {
	for _, args := range [][]string{
		{"--enable-selection"},
		{"--enable-selection", "--key-file", "/tmp/test/jev.key"},
		{"--key-file=/tmp/test/jev.key", "--enable-selection"},
	} {
		cfg, err := parseArgs(args, "/Users/example")
		if err != nil {
			t.Fatalf("parseArgs(%q) error = %v", args, err)
		}
		if !cfg.enableSelection {
			t.Errorf("parseArgs(%q) did not enable selection", args)
		}
	}
}

func TestParseArgsRejects(t *testing.T) {
	tests := []struct {
		name string
		args []string
		home string
	}{
		{name: "relative key file", args: []string{"--key-file", "jev.key"}, home: "/Users/example"},
		{name: "relative joined key file", args: []string{"--key-file=./jev.key"}, home: "/Users/example"},
		{name: "empty key file", args: []string{"--key-file="}, home: "/Users/example"},
		{name: "missing value", args: []string{"--key-file"}, home: "/Users/example"},
		{name: "repeated flag", args: []string{"--key-file", "/a/one.key", "--key-file", "/a/two.key"}, home: "/Users/example"},
		{name: "repeated selection flag", args: []string{"--enable-selection", "--enable-selection"}, home: "/Users/example"},
		{name: "selection flag with value", args: []string{"--enable-selection=true"}, home: "/Users/example"},
		{name: "unknown flag", args: []string{"--endpoint", "https://example.test"}, home: "/Users/example"},
		{name: "api key flag", args: []string{"--api-key", "sk-live-SENTINEL"}, home: "/Users/example"},
		{name: "subcommand", args: []string{"mcp"}, home: "/Users/example"},
		{name: "help", args: []string{"--help"}, home: "/Users/example"},
		{name: "no home and no flag", args: nil, home: ""},
		{name: "relative home", args: nil, home: "relative/home"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseArgs(tt.args, tt.home)
			if err == nil {
				t.Fatalf("parseArgs(%q) = %+v, want an error", tt.args, cfg)
			}
			if cfg.keyFile != "" {
				t.Errorf("parseArgs(%q) returned a key file (%q) alongside an error", tt.args, cfg.keyFile)
			}
			// Arguments are visible in the process table; an error that repeats
			// one can copy something that should not be there into a log.
			for _, arg := range tt.args {
				if strings.Contains(err.Error(), arg) && arg != keyFileFlag && arg != enableSelectionFlag {
					t.Errorf("error %q repeats the argument %q", err, arg)
				}
			}
		})
	}
}

func TestRunWritesNothingToStdout(t *testing.T) {
	// Stdout is the protocol stream. A failure before the server starts must
	// still leave it untouched, or the first thing a client parses is a
	// diagnostic.
	tests := []struct {
		name string
		args []string
	}{
		{name: "unreadable credential", args: []string{"--key-file", filepath.Join(t.TempDir(), "absent.key")}},
		{name: "unrecognized argument", args: []string{"--endpoint", "https://example.test"}},
		{name: "relative key file", args: []string{"--key-file", "relative.key"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer

			err := run(tt.args, strings.NewReader(""), &stdout, &stderr)

			if err == nil {
				t.Fatal("run() = nil, want an error")
			}
			if stdout.Len() != 0 {
				t.Errorf("run() wrote %q to stdout, want nothing", stdout.String())
			}
			if stderr.Len() == 0 && !strings.Contains(err.Error(), "credential") {
				t.Error("run() reported nothing on stderr")
			}
		})
	}
}

func TestWatchdogEndsTheProcessWhenShutdownOverruns(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	restored := make(chan struct{})
	exited := make(chan int, 1)
	reported := make(chan string, 1)

	go func() {
		var stderr bytes.Buffer
		watchdog(ctx, func() { close(restored) }, 10*time.Millisecond, &stderr, func(code int) { exited <- code })
		reported <- stderr.String()
	}()
	cancel()

	select {
	case <-restored:
	case <-time.After(time.Second):
		t.Fatal("the watchdog did not restore the default signal handling")
	}
	select {
	case code := <-exited:
		if code != exitShutdownOverran {
			t.Errorf("exit code = %d, want %d", code, exitShutdownOverran)
		}
	case <-time.After(time.Second):
		t.Fatal("the watchdog did not end the process after the shutdown budget")
	}
	if message := <-reported; !strings.Contains(message, "shutdown") {
		t.Errorf("stderr = %q, want a note about the overrun", message)
	}
}

func TestWatchdogDoesNothingWhileTheServerIsRunning(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exited := make(chan int, 1)

	go watchdog(ctx, func() {}, time.Millisecond, io.Discard, func(code int) { exited <- code })

	select {
	case code := <-exited:
		t.Fatalf("the watchdog ended the process with %d while the context was still live", code)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestParseArgsAcceptsNoOtherCommands(t *testing.T) {
	// The roadmap excludes setup, self-update, the remote installer, and a
	// command line evaluation. This is the check that none of them appear.
	for _, command := range []string{"setup", "update", "install", "evaluate", "version", "serve"} {
		if _, err := parseArgs([]string{command}, "/Users/example"); err == nil {
			t.Errorf("parseArgs(%q) was accepted, but this binary has no subcommands", command)
		}
	}
}
