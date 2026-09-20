package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadFrameReturnsMessagesAndSkipsBlankLines(t *testing.T) {
	input := "{\"a\":1}\n\n   \n{\"b\":2}\r\n{\"c\":3}\n"
	r := newFrameReader(strings.NewReader(input))

	for _, want := range []string{`{"a":1}`, `{"b":2}`, `{"c":3}`} {
		got, err := r.readFrame()
		if err != nil {
			t.Fatalf("readFrame() error = %v, want a frame %q", err, want)
		}
		if string(got) != want {
			t.Errorf("readFrame() = %q, want %q", got, want)
		}
	}
	if _, err := r.readFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("readFrame() at end of input error = %v, want io.EOF", err)
	}
}

func TestReadFrameDropsIncompleteFinalLine(t *testing.T) {
	// A fragment with no terminator is not a message: the peer died mid-write.
	r := newFrameReader(strings.NewReader("{\"a\":1}\n{\"incomplet"))

	if _, err := r.readFrame(); err != nil {
		t.Fatalf("readFrame() error = %v, want the first frame", err)
	}
	if got, err := r.readFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("readFrame() = (%q, %v), want (nil, io.EOF)", got, err)
	}
}

func TestReadFrameAcceptsExactlyTheLimit(t *testing.T) {
	// 10 bytes of framing: {"pad":" and ".
	line := `{"pad":"` + strings.Repeat("x", maxFrameBytes-10) + `"}`
	if len(line) != maxFrameBytes {
		t.Fatalf("test setup: line is %d bytes, want %d", len(line), maxFrameBytes)
	}
	r := newFrameReader(strings.NewReader(line + "\n"))

	got, err := r.readFrame()
	if err != nil {
		t.Fatalf("readFrame() error = %v, want a frame of exactly the limit", err)
	}
	if len(got) != maxFrameBytes {
		t.Errorf("readFrame() returned %d bytes, want %d", len(got), maxFrameBytes)
	}
}

func TestReadFrameRejectsOversizeAndKeepsReading(t *testing.T) {
	oversize := strings.Repeat("x", maxFrameBytes+1)
	r := newFrameReader(strings.NewReader(oversize + "\n" + `{"next":true}` + "\n"))

	if _, err := r.readFrame(); !errors.Is(err, errFrameTooLarge) {
		t.Fatalf("readFrame() error = %v, want errFrameTooLarge", err)
	}
	// The reader resynchronised on the newline that ended the oversized
	// message, so the message after it is still readable.
	got, err := r.readFrame()
	if err != nil {
		t.Fatalf("readFrame() after an oversized message error = %v", err)
	}
	if string(got) != `{"next":true}` {
		t.Errorf("readFrame() = %q, want the message after the oversized one", got)
	}
}

// endlessReader never returns an error and never produces a newline.
type endlessReader struct{}

func (endlessReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

func TestReadFrameGivesUpOnUnterminatedFlood(t *testing.T) {
	r := newFrameReader(endlessReader{})

	if _, err := r.readFrame(); !errors.Is(err, errUnsynchronized) {
		t.Fatalf("readFrame() error = %v, want errUnsynchronized", err)
	}
}

// newTestWriter returns a writer over out with a generous budget and no abort
// channel, and stops its goroutine when the test ends.
func newTestWriter(t *testing.T, out io.Writer) *frameWriter {
	t.Helper()
	w := newFrameWriter(out, nil, testTimeout)
	t.Cleanup(w.close)
	return w
}

func TestFrameWriterWritesOneLinePerMessage(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)
	ctx := context.Background()

	if err := w.write(ctx, &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`), Result: struct{}{}}); err != nil {
		t.Fatalf("write() error = %v", err)
	}
	if err := w.write(ctx, &response{JSONRPC: jsonrpcVersion, ID: nil, Error: &rpcError{Code: codeParseError, Message: msgParseError}}); err != nil {
		t.Fatalf("write() error = %v", err)
	}

	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("write() produced %d lines, want 2: %q", len(lines), buf.String())
	}
	if !strings.Contains(lines[0], `"id":1`) || !strings.Contains(lines[0], `"result":{}`) {
		t.Errorf("first line = %q, want an id and an empty result", lines[0])
	}
	if !strings.Contains(lines[1], `"id":null`) {
		t.Errorf("second line = %q, want a null id", lines[1])
	}
}

func TestFrameWriterRefusesOversizeMessage(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)

	huge := strings.Repeat("x", maxFrameBytes)
	err := w.write(context.Background(), &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`), Result: huge})

	if !errors.Is(err, errOversizeOutgoing) {
		t.Fatalf("write() error = %v, want errOversizeOutgoing", err)
	}
	if buf.Len() != 0 {
		t.Errorf("write() wrote %d bytes for a refused message, want 0", buf.Len())
	}
}

func TestFrameWriterEscapesNewlinesInsideStrings(t *testing.T) {
	var buf bytes.Buffer
	w := newTestWriter(t, &buf)

	if err := w.write(context.Background(), &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`), Result: "a\nb"}); err != nil {
		t.Fatalf("write() error = %v", err)
	}
	if n := strings.Count(buf.String(), "\n"); n != 1 {
		t.Errorf("write() produced %d newlines, want only the terminator: %q", n, buf.String())
	}
}

// blockingWriter takes the first allow messages and then accepts nothing until
// release is closed. It stands in for a consumer that has stopped reading its
// end of the pipe, optionally after a working handshake.
type blockingWriter struct {
	release chan struct{}
	blocked chan struct{}

	mu    sync.Mutex
	allow int
	taken []string
}

func newBlockingWriter(allow int) *blockingWriter {
	return &blockingWriter{
		release: make(chan struct{}),
		blocked: make(chan struct{}, 8),
		allow:   allow,
	}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	if w.allow > 0 {
		w.allow--
		w.taken = append(w.taken, string(p))
		w.mu.Unlock()
		return len(p), nil
	}
	w.mu.Unlock()

	select {
	case w.blocked <- struct{}{}:
	default:
	}
	<-w.release
	return len(p), nil
}

// waitBlocked waits until a write has actually blocked.
func (w *blockingWriter) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-w.blocked:
	case <-time.After(testTimeout):
		t.Fatal("no write ever blocked")
	}
}

func (w *blockingWriter) accepted() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.taken)
}

func TestFrameWriterGivesUpWhenTheContextEnds(t *testing.T) {
	out := newBlockingWriter(0)
	defer close(out.release)
	w := newFrameWriter(out, nil, testTimeout)
	defer w.close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- w.write(ctx, &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`), Result: struct{}{}})
	}()
	out.waitBlocked(t)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("write() error = %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("write() did not return after its context was cancelled")
	}

	// The stream is unusable afterwards: the abandoned bytes may be half
	// written, so a later message could resume inside one.
	if err := w.write(context.Background(), &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`2`), Result: struct{}{}}); !errors.Is(err, errWriteStalled) {
		t.Errorf("a later write returned %v, want errWriteStalled", err)
	}
}

func TestFrameWriterGivesUpWhenTheInputEnds(t *testing.T) {
	out := newBlockingWriter(0)
	defer close(out.release)
	abort := make(chan struct{})
	w := newFrameWriter(out, abort, testTimeout)
	defer w.close()

	done := make(chan error, 1)
	go func() {
		done <- w.write(context.Background(), &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`), Result: struct{}{}})
	}()
	out.waitBlocked(t)
	close(abort)

	select {
	case err := <-done:
		if !errors.Is(err, errWriteAborted) {
			t.Fatalf("write() error = %v, want errWriteAborted", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("write() did not return after the input ended")
	}
}

func TestFrameWriterGivesUpAfterItsBudget(t *testing.T) {
	out := newBlockingWriter(0)
	defer close(out.release)
	w := newFrameWriter(out, nil, 50*time.Millisecond)
	defer w.close()

	start := time.Now()
	err := w.write(context.Background(), &response{JSONRPC: jsonrpcVersion, ID: json.RawMessage(`1`), Result: struct{}{}})

	if !errors.Is(err, errWriteStalled) {
		t.Fatalf("write() error = %v, want errWriteStalled", err)
	}
	if elapsed := time.Since(start); elapsed > testTimeout {
		t.Errorf("write() took %v, want it bounded by its budget", elapsed)
	}
}

func TestParseID(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		ok   bool
		key  string
	}{
		{name: "integer", raw: `7`, ok: true, key: "n:7"},
		{name: "negative integer", raw: `-7`, ok: true, key: "n:-7"},
		{name: "zero", raw: `0`, ok: true, key: "n:0"},
		{name: "string", raw: `"abc"`, ok: true, key: "s:abc"},
		{name: "empty string is a usable id", raw: `""`, ok: true, key: "s:"},
		{name: "string and number do not collide", raw: `"7"`, ok: true, key: "s:7"},
		{name: "surrounding space", raw: "  7  ", ok: true, key: "n:7"},
		{name: "null", raw: `null`, ok: false},
		{name: "absent", raw: ``, ok: false},
		{name: "boolean", raw: `true`, ok: false},
		{name: "object", raw: `{"a":1}`, ok: false},
		{name: "array", raw: `[1]`, ok: false},
		{name: "fractional", raw: `1.5`, ok: false},
		// Negative zero is refused rather than folded into zero, so one id is
		// one text and the in-flight check cannot be fooled by two spellings.
		{name: "negative zero", raw: `-0`, ok: false},
		{name: "exponent", raw: `1e3`, ok: false},
		{name: "leading zero", raw: `01`, ok: false},
		{name: "oversize string", raw: `"` + strings.Repeat("x", maxIDBytes) + `"`, ok: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, ok := parseID(json.RawMessage(tt.raw))
			if ok != tt.ok {
				t.Fatalf("parseID(%q) ok = %v, want %v", tt.raw, ok, tt.ok)
			}
			if !tt.ok {
				return
			}
			if id.key != tt.key {
				t.Errorf("parseID(%q) key = %q, want %q", tt.raw, id.key, tt.key)
			}
			if !json.Valid(id.raw) {
				t.Errorf("parseID(%q) raw = %q, want valid JSON to echo back", tt.raw, id.raw)
			}
		})
	}
}

func TestParseIDCopiesTheRawBytes(t *testing.T) {
	// readFrame reuses its buffer, so an id that aliased it would change under
	// the next message.
	raw := []byte(`"abc"`)
	id, ok := parseID(json.RawMessage(raw))
	if !ok {
		t.Fatalf("parseID(%q) failed", raw)
	}
	copy(raw, `"zzz"`)
	if string(id.raw) != `"abc"` {
		t.Errorf("id.raw = %q after the source buffer changed, want %q", id.raw, `"abc"`)
	}
}
