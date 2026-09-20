package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"jev-mcp/internal/typesafe"
)

const (
	// testTimeout bounds a wait for something that should already be happening.
	testTimeout = 3 * time.Second

	// quietTime is how long a test waits to conclude that nothing is coming.
	quietTime = 200 * time.Millisecond
)

// sampleArguments is a minimal well-formed tool call.
const sampleArguments = `{"state":"Payouts have failed for three days.","questions":{"urgent":{"type":"noul","instructions":"Does the message convey urgency?"}}}`

// sampleResponseJSON is a response in the shape the API reference documents.
const sampleResponseJSON = `{"model":"jev-1.13.0","answers":{"urgent":{"type":"noul","noul":0.92}},"usage":{"input_tokens":312,"output_tokens":48}}`

// decodeResponse builds a typesafe.Response from documented JSON rather than
// from field names, so this package's tests do not encode assumptions about the
// API package's internal shape.
func decodeResponse(t *testing.T, body string) typesafe.Response {
	t.Helper()
	var res typesafe.Response
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("test setup: a documented API response no longer decodes into typesafe.Response: %v", err)
	}
	return res
}

// fakeEvaluator stands in for the API client. It records what it was asked and
// defers the answer to fn, so a test can hold an evaluation open, fail it, or
// let it finish.
type fakeEvaluator struct {
	fn func(ctx context.Context, req typesafe.Request) (typesafe.Response, error)

	started chan struct{}

	mu       sync.Mutex
	requests []typesafe.Request
}

func newFake(fn func(ctx context.Context, req typesafe.Request) (typesafe.Response, error)) *fakeEvaluator {
	return &fakeEvaluator{fn: fn, started: make(chan struct{}, 8)}
}

func (f *fakeEvaluator) Evaluate(ctx context.Context, req typesafe.Request) (typesafe.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	f.mu.Unlock()
	select {
	case f.started <- struct{}{}:
	default:
	}
	if f.fn == nil {
		return typesafe.Response{}, nil
	}
	return f.fn(ctx, req)
}

func (f *fakeEvaluator) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *fakeEvaluator) request(i int) typesafe.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[i]
}

func (f *fakeEvaluator) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-f.started:
	case <-time.After(testTimeout):
		t.Fatal("the evaluation never started")
	}
}

// harness runs a server over a pair of pipes and gives the test line-level
// access to both directions.
type harness struct {
	t        *testing.T
	in       *io.PipeWriter
	lines    chan string
	serveErr chan error
	cancel   context.CancelFunc
	waited   bool
	nextID   int
}

func newHarness(t *testing.T, eval Evaluator, opts Options) *harness {
	t.Helper()

	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	ctx, cancel := context.WithCancel(context.Background())

	server := NewServer(eval, opts)
	serveErr := make(chan error, 1)
	go func() {
		err := server.Serve(ctx, inR, outW)
		outW.Close()
		serveErr <- err
	}()

	lines := make(chan string, 64)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(outR)
		scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes+1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	h := &harness{t: t, in: inW, lines: lines, serveErr: serveErr, cancel: cancel, nextID: 1}
	t.Cleanup(func() {
		cancel()
		inW.Close()
		if h.waited {
			return
		}
		select {
		case <-serveErr:
		case <-time.After(testTimeout):
			t.Error("Serve did not return after its context was cancelled")
		}
	})
	return h
}

// send writes one raw line to the server.
func (h *harness) send(line string) {
	h.t.Helper()
	if _, err := io.WriteString(h.in, line+"\n"); err != nil {
		h.t.Fatalf("writing %q to the server: %v", line, err)
	}
}

// recv returns the next line the server wrote.
func (h *harness) recv() string {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if !ok {
			h.t.Fatal("the server closed its output stream while a message was expected")
		}
		return line
	case <-time.After(testTimeout):
		h.t.Fatal("timed out waiting for a message from the server")
		return ""
	}
}

// wireMessage is a response as it appears on the wire.
type wireMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error"`
	raw     string
}

// recvMessage reads one response and checks the framing invariants every
// message has to satisfy: valid JSON, one message per line, jsonrpc 2.0, an id
// member, and exactly one of result and error.
func (h *harness) recvMessage() wireMessage {
	h.t.Helper()
	line := h.recv()

	var msg wireMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		h.t.Fatalf("the server wrote a line that is not JSON: %q", line)
	}
	msg.raw = line
	if msg.JSONRPC != jsonrpcVersion {
		h.t.Errorf("message jsonrpc = %q, want %q: %s", msg.JSONRPC, jsonrpcVersion, line)
	}
	if len(msg.ID) == 0 {
		h.t.Errorf("message has no id member: %s", line)
	}
	if (len(msg.Result) == 0) == (msg.Error == nil) {
		h.t.Errorf("message must carry exactly one of result and error: %s", line)
	}
	if msg.Error != nil {
		assertSafeMessage(h.t, msg.Error.Message)
	}
	return msg
}

// expectQuiet fails if the server says anything within quietTime.
func (h *harness) expectQuiet() {
	h.t.Helper()
	select {
	case line, ok := <-h.lines:
		if ok {
			h.t.Fatalf("the server sent a message when none was expected: %s", line)
		}
	case <-time.After(quietTime):
	}
}

func (h *harness) waitServe() error {
	h.t.Helper()
	h.waited = true
	select {
	case err := <-h.serveErr:
		return err
	case <-time.After(testTimeout):
		h.t.Fatal("Serve did not return")
		return nil
	}
}

func (h *harness) id() int {
	h.nextID++
	return h.nextID
}

// initialize performs the initialize exchange and returns the result.
func (h *harness) initialize(protocolVersion string) initializeResult {
	h.t.Helper()
	id := h.id()
	h.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`, id, protocolVersion))

	msg := h.recvMessage()
	if msg.Error != nil {
		h.t.Fatalf("initialize failed: %s", msg.raw)
	}
	var result initializeResult
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		h.t.Fatalf("initialize result does not decode: %s", msg.raw)
	}
	return result
}

// handshake completes initialization so tools may be used.
func (h *harness) handshake() {
	h.t.Helper()
	h.initialize(defaultProtocolVersion)
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
}

// callTool sends a tools/call for evaluate with the given raw arguments.
func (h *harness) callTool(id int, arguments string) {
	h.t.Helper()
	h.callNamedTool(id, toolName, arguments)
}

func (h *harness) callNamedTool(id int, name, arguments string) {
	h.t.Helper()
	h.send(fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, id, name, arguments))
}

// toolResult decodes a successful tools/call response.
func (h *harness) toolResult(msg wireMessage) callToolResult {
	h.t.Helper()
	if msg.Error != nil {
		h.t.Fatalf("expected a tool result, got a JSON-RPC error: %s", msg.raw)
	}
	var result callToolResult
	if err := json.Unmarshal(msg.Result, &result); err != nil {
		h.t.Fatalf("tool result does not decode: %s", msg.raw)
	}
	if len(result.Content) == 0 {
		h.t.Fatalf("tool result carries no content: %s", msg.raw)
	}
	return result
}

// succeedingEvaluator answers every call with the sample response. The response
// is decoded on the test's own goroutine, because a decoding failure has to be
// reported from there.
func succeedingEvaluator(t *testing.T) *fakeEvaluator {
	t.Helper()
	res := decodeResponse(t, sampleResponseJSON)
	return newFake(func(context.Context, typesafe.Request) (typesafe.Response, error) {
		return res, nil
	})
}

// safeLog collects diagnostics from the serving goroutine.
type safeLog struct {
	mu   sync.Mutex
	text strings.Builder
}

func (l *safeLog) logf(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.text.WriteString(fmt.Sprintf(format, args...))
	l.text.WriteByte('\n')
}

func (l *safeLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.text.String()
}

// --- handshake ---------------------------------------------------------

func TestInitializeAdvertisesToolsAndInstructions(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{Version: "1.2.3"})

	result := h.initialize(defaultProtocolVersion)

	if result.ProtocolVersion != defaultProtocolVersion {
		t.Errorf("protocolVersion = %q, want %q", result.ProtocolVersion, defaultProtocolVersion)
	}
	if result.ServerInfo.Name != serverName || result.ServerInfo.Version != "1.2.3" {
		t.Errorf("serverInfo = %+v, want the server name and the build version", result.ServerInfo)
	}
	if result.Capabilities.Tools == nil {
		t.Error("the server did not advertise the tools capability")
	}
	if !strings.Contains(result.Instructions, "Jev") {
		t.Errorf("instructions = %q, want the evaluation guidance", result.Instructions)
	}
}

func TestInitializeNegotiatesTheProtocolVersion(t *testing.T) {
	tests := []struct {
		requested string
		want      string
	}{
		{requested: "2024-11-05", want: "2024-11-05"},
		{requested: "2025-03-26", want: "2025-03-26"},
		{requested: "2025-06-18", want: "2025-06-18"},
		{requested: "2099-01-01", want: defaultProtocolVersion},
		{requested: "nonsense", want: defaultProtocolVersion},
	}

	for _, tt := range tests {
		t.Run(tt.requested, func(t *testing.T) {
			h := newHarness(t, succeedingEvaluator(t), Options{})
			if got := h.initialize(tt.requested).ProtocolVersion; got != tt.want {
				t.Errorf("protocolVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestInitializeRejectsMalformedParams(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{name: "no params", params: ``},
		{name: "params array", params: `,"params":[]`},
		{name: "params string", params: `,"params":"2024-11-05"`},
		{name: "params null", params: `,"params":null`},
		{name: "protocolVersion missing", params: `,"params":{"capabilities":{}}`},
		{name: "protocolVersion number", params: `,"params":{"protocolVersion":20241105}`},
		{name: "protocolVersion empty", params: `,"params":{"protocolVersion":""}`},
		// The backslash is spliced in so the line carries the JSON escape
		// sequence rather than a raw control byte, which would be a parse error
		// instead of the validation failure this case is about.
		{name: "protocolVersion with an escaped control character", params: `,"params":{"protocolVersion":"2024` + `\` + `u000b11-05"}`},
		{name: "capabilities missing", params: `,"params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"0"}}`},
		{name: "capabilities null", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":null,"clientInfo":{"name":"test","version":"0"}}`},
		{name: "capabilities string", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":"tools","clientInfo":{"name":"test","version":"0"}}`},
		{name: "clientInfo missing", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{}}`},
		{name: "clientInfo null", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":null}`},
		{name: "clientInfo array", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":[]}`},
		{name: "clientInfo name missing", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"version":"0"}}`},
		{name: "clientInfo name empty", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"","version":"0"}}`},
		{name: "clientInfo name not a string", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":7,"version":"0"}}`},
		{name: "clientInfo name too long", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"` + strings.Repeat("n", maxClientInfoBytes+1) + `","version":"0"}}`},
		{name: "clientInfo version missing", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test"}}`},
		{name: "clientInfo version not a string", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":0}}`},
		{name: "clientInfo name with an escaped control character", params: `,"params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"te` + `\` + `u0007st","version":"0"}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, succeedingEvaluator(t), Options{})
			h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize"` + tt.params + `}`)

			msg := h.recvMessage()
			if msg.Error == nil || msg.Error.Code != codeInvalidParams {
				t.Fatalf("got %s, want an invalid params error", msg.raw)
			}
			// The handshake did not happen, so tools stay closed.
			h.send(`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
			if next := h.recvMessage(); next.Error == nil || next.Error.Code != codeNotInitialized {
				t.Fatalf("got %s, want a not-initialized error after a failed initialize", next.raw)
			}
		})
	}
}

func TestInitializeDoesNotEchoClientMetadata(t *testing.T) {
	const sentinel = "ZEBRA-CLIENT-SENTINEL"
	h := newHarness(t, succeedingEvaluator(t), Options{})

	h.send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{"roots":{}},"clientInfo":{"name":"` + sentinel + `","version":"` + sentinel + `"}}}`)

	msg := h.recvMessage()
	if msg.Error != nil {
		t.Fatalf("initialize failed: %s", msg.raw)
	}
	// What a client says about itself is validated and then dropped: nothing it
	// chose may travel back out of this server.
	if strings.Contains(msg.raw, sentinel) {
		t.Errorf("the initialize result repeats what the client said about itself: %s", msg.raw)
	}
}

func TestSecondInitializeIsRejected(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})
	h.handshake()

	h.send(`{"jsonrpc":"2.0","id":99,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)

	msg := h.recvMessage()
	if msg.Error == nil || msg.Error.Message != msgAlreadyInitialized {
		t.Fatalf("got %s, want a rejection of the second initialize", msg.raw)
	}
}

func TestToolsAreClosedUntilTheHandshakeCompletes(t *testing.T) {
	eval := succeedingEvaluator(t)
	h := newHarness(t, eval, Options{})

	// Before initialize.
	h.send(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	if msg := h.recvMessage(); msg.Error == nil || msg.Error.Code != codeNotInitialized || msg.Error.Message != msgNotInitialized {
		t.Fatalf("tools/list before initialize: got %s, want a not-initialized error", msg.raw)
	}
	h.callTool(2, sampleArguments)
	if msg := h.recvMessage(); msg.Error == nil || msg.Error.Message != msgNotInitialized {
		t.Fatalf("tools/call before initialize: got %s, want a not-initialized error", msg.raw)
	}

	// After the initialize response but before notifications/initialized.
	h.initialize(defaultProtocolVersion)
	h.send(`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`)
	if msg := h.recvMessage(); msg.Error == nil || msg.Error.Message != msgNotReady {
		t.Fatalf("tools/list before the initialized notification: got %s, want a not-ready error", msg.raw)
	}
	h.callTool(4, sampleArguments)
	if msg := h.recvMessage(); msg.Error == nil || msg.Error.Message != msgNotReady {
		t.Fatalf("tools/call before the initialized notification: got %s, want a not-ready error", msg.raw)
	}

	if eval.calls() != 0 {
		t.Errorf("the evaluator ran %d times before the handshake completed, want 0", eval.calls())
	}

	// And now it works.
	h.send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	h.send(`{"jsonrpc":"2.0","id":5,"method":"tools/list"}`)
	if msg := h.recvMessage(); msg.Error != nil {
		t.Fatalf("tools/list after the handshake: got %s, want the tool list", msg.raw)
	}
}

func TestPingIsAnsweredBeforeInitialize(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})

	h.send(`{"jsonrpc":"2.0","id":"ping-1","method":"ping"}`)

	msg := h.recvMessage()
	if msg.Error != nil {
		t.Fatalf("ping failed: %s", msg.raw)
	}
	if string(msg.ID) != `"ping-1"` {
		t.Errorf("ping id = %s, want the string id echoed back", msg.ID)
	}
	if string(msg.Result) != "{}" {
		t.Errorf("ping result = %s, want an empty object", msg.Result)
	}
}

// --- malformed input ---------------------------------------------------

func TestMalformedMessagesAreRejectedSafely(t *testing.T) {
	tests := []struct {
		name string
		line string
		code int
	}{
		{name: "not JSON", line: `{"jsonrpc":`, code: codeParseError},
		{name: "bare word", line: `hello`, code: codeParseError},
		{name: "truncated literal", line: `nul`, code: codeParseError},
		{name: "unterminated string", line: `"ping`, code: codeParseError},
		{name: "malformed number", line: `01`, code: codeParseError},
		{name: "trailing comma", line: `{"jsonrpc":"2.0","id":1,"method":"ping",}`, code: codeParseError},
		{name: "JSON array batch", line: `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, code: codeInvalidRequest},
		{name: "JSON string", line: `"ping"`, code: codeInvalidRequest},
		{name: "JSON number", line: `42`, code: codeInvalidRequest},
		{name: "JSON null", line: `null`, code: codeInvalidRequest},
		{name: "method and result", line: `{"jsonrpc":"2.0","id":1,"method":"ping","result":{}}`, code: codeInvalidRequest},
		{name: "method and error", line: `{"jsonrpc":"2.0","id":1,"method":"ping","error":{"code":-1,"message":"x"}}`, code: codeInvalidRequest},
		{name: "wrong jsonrpc version", line: `{"jsonrpc":"1.0","id":1,"method":"ping"}`, code: codeInvalidRequest},
		{name: "missing jsonrpc", line: `{"id":1,"method":"ping"}`, code: codeInvalidRequest},
		{name: "method not a string", line: `{"jsonrpc":"2.0","id":1,"method":7}`, code: codeInvalidRequest},
		{name: "no method", line: `{"jsonrpc":"2.0","id":1}`, code: codeInvalidRequest},
		{name: "unknown method", line: `{"jsonrpc":"2.0","id":1,"method":"resources/list"}`, code: codeMethodNotFound},
		{name: "overlong method", line: `{"jsonrpc":"2.0","id":1,"method":"` + strings.Repeat("m", maxMethodBytes+1) + `"}`, code: codeMethodNotFound},
		{name: "null id", line: `{"jsonrpc":"2.0","id":null,"method":"ping"}`, code: codeInvalidRequest},
		{name: "boolean id", line: `{"jsonrpc":"2.0","id":true,"method":"ping"}`, code: codeInvalidRequest},
		{name: "object id", line: `{"jsonrpc":"2.0","id":{"n":1},"method":"ping"}`, code: codeInvalidRequest},
		{name: "array id", line: `{"jsonrpc":"2.0","id":[1],"method":"ping"}`, code: codeInvalidRequest},
		{name: "fractional id", line: `{"jsonrpc":"2.0","id":1.5,"method":"ping"}`, code: codeInvalidRequest},
		{name: "negative zero id", line: `{"jsonrpc":"2.0","id":-0,"method":"ping"}`, code: codeInvalidRequest},
		{name: "oversize string id", line: `{"jsonrpc":"2.0","id":"` + strings.Repeat("i", maxIDBytes) + `","method":"ping"}`, code: codeInvalidRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, succeedingEvaluator(t), Options{})
			h.send(tt.line)

			msg := h.recvMessage()
			if msg.Error == nil || msg.Error.Code != tt.code {
				t.Fatalf("got %s, want error code %d", msg.raw, tt.code)
			}

			// A rejected message does not end the session.
			h.send(`{"jsonrpc":"2.0","id":"after","method":"ping"}`)
			if next := h.recvMessage(); next.Error != nil {
				t.Fatalf("the server stopped serving after a bad message: %s", next.raw)
			}
		})
	}
}

func TestAMessageThatIsBothACallAndAnAnswerIsRejected(t *testing.T) {
	eval := succeedingEvaluator(t)
	h := newHarness(t, eval, Options{})
	h.handshake()

	// Shaped like a tools/call and like an answer at the same time. Guessing
	// which one was meant is how a request gets executed twice.
	h.send(`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"evaluate","arguments":` + sampleArguments + `},"result":{"content":[]}}`)

	msg := h.recvMessage()
	if msg.Error == nil || msg.Error.Message != msgAmbiguousMessage {
		t.Fatalf("got %s, want the ambiguous message rejection", msg.raw)
	}
	if eval.calls() != 0 {
		t.Errorf("the evaluator ran for an ambiguous message")
	}
}

func TestUnusableIDsAreAnsweredWithANullID(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})

	h.send(`{"jsonrpc":"2.0","id":{"nested":true},"method":"ping"}`)

	msg := h.recvMessage()
	if string(msg.ID) != "null" {
		t.Errorf("id = %s, want null when the request id cannot be used", msg.ID)
	}
	if msg.Error == nil || msg.Error.Message != msgBadID {
		t.Fatalf("got %s, want the invalid id message", msg.raw)
	}
}

func TestOversizedFrameIsRejectedAndTheSessionSurvives(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})

	h.send(`{"jsonrpc":"2.0","id":1,"method":"ping","pad":"` + strings.Repeat("x", maxFrameBytes) + `"}`)

	msg := h.recvMessage()
	if msg.Error == nil || msg.Error.Message != msgFrameTooLarge {
		t.Fatalf("got %s, want the frame limit error", msg.raw)
	}
	if string(msg.ID) != "null" {
		t.Errorf("id = %s, want null: the id of an unparsed message is unknown", msg.ID)
	}

	h.send(`{"jsonrpc":"2.0","id":2,"method":"ping"}`)
	if next := h.recvMessage(); next.Error != nil {
		t.Fatalf("the server stopped serving after an oversized message: %s", next.raw)
	}
}

func TestNotificationsAreNeverAnswered(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})
	h.handshake()

	for _, line := range []string{
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","method":"notifications/unknown","params":{"a":1}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"never-sent"}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled"}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":null}}`,
		`{"jsonrpc":"2.0","method":"ping"}`,
		`{"jsonrpc":"1.0","method":"ping"}`,
		`{"jsonrpc":"2.0","method":""}`,
		`{"jsonrpc":"2.0","id":7,"result":{"unexpected":true}}`,
		`{"jsonrpc":"2.0","id":8,"error":{"code":-1,"message":"unexpected"}}`,
		// Ambiguous and unanswerable: a method and an answer, but no id.
		`{"jsonrpc":"2.0","method":"ping","result":{}}`,
	} {
		h.send(line)
	}
	h.expectQuiet()

	// The session is still usable, which is how we know the messages were
	// consumed rather than the server having stalled.
	h.send(`{"jsonrpc":"2.0","id":"after","method":"ping"}`)
	if msg := h.recvMessage(); msg.Error != nil {
		t.Fatalf("got %s, want a ping response", msg.raw)
	}
}

// --- the evaluate tool -------------------------------------------------

func TestToolsListAdvertisesEvaluateOnly(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})
	h.handshake()

	h.send(`{"jsonrpc":"2.0","id":10,"method":"tools/list"}`)

	msg := h.recvMessage()
	var list toolsListResult
	if err := json.Unmarshal(msg.Result, &list); err != nil {
		t.Fatalf("tools/list result does not decode: %s", msg.raw)
	}
	if len(list.Tools) != 1 || list.Tools[0].Name != toolName {
		t.Fatalf("tools = %+v, want only the evaluate tool", list.Tools)
	}
	if !json.Valid(list.Tools[0].InputSchema) {
		t.Errorf("the advertised input schema is not valid JSON: %s", list.Tools[0].InputSchema)
	}
}

func TestEvaluateReturnsTextAndStructuredContent(t *testing.T) {
	eval := succeedingEvaluator(t)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(11, sampleArguments)

	result := h.toolResult(h.recvMessage())
	if result.IsError {
		t.Fatalf("result is marked as an error: %+v", result)
	}
	if result.Content[0].Text != string(result.StructuredContent) {
		t.Errorf("text and structuredContent differ:\n text = %s\n structured = %s", result.Content[0].Text, result.StructuredContent)
	}
	var structured map[string]any
	if err := json.Unmarshal(result.StructuredContent, &structured); err != nil {
		t.Fatalf("structuredContent is not an object: %s", result.StructuredContent)
	}
	for _, key := range []string{"model", "answers", "usage"} {
		if _, ok := structured[key]; !ok {
			t.Errorf("structuredContent has no %q member: %s", key, result.StructuredContent)
		}
	}
	if structured["model"] != typesafe.Model {
		t.Errorf("model = %v, want the pinned model %q", structured["model"], typesafe.Model)
	}

	if eval.calls() != 1 {
		t.Fatalf("the evaluator ran %d times, want once", eval.calls())
	}
	req := eval.request(0)
	if string(req.State) != `"Payouts have failed for three days."` {
		t.Errorf("state = %s, want the compacted state", req.State)
	}
	if q, ok := req.Questions["urgent"]; !ok || q.Type != "noul" {
		t.Errorf("questions = %+v, want the noul question under its id", req.Questions)
	}
}

func TestEvaluateRejectsBadArgumentsWithoutCallingTheAPI(t *testing.T) {
	eval := succeedingEvaluator(t)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(12, `{"state":42,"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`)

	result := h.toolResult(h.recvMessage())
	if !result.IsError {
		t.Error("an invalid call was not reported as a tool error")
	}
	assertSafeMessage(t, result.Content[0].Text)
	if eval.calls() != 0 {
		t.Errorf("the evaluator ran %d times for invalid arguments, want 0", eval.calls())
	}
}

func TestCallToolRejectsMalformedParams(t *testing.T) {
	tests := []struct {
		name   string
		params string
	}{
		{name: "no params", params: ``},
		{name: "params array", params: `,"params":[]`},
		{name: "name missing", params: `,"params":{"arguments":{}}`},
		{name: "unknown tool", params: `,"params":{"name":"shell","arguments":{}}`},
		{name: "arguments array", params: `,"params":{"name":"evaluate","arguments":[]}`},
		{name: "arguments string", params: `,"params":{"name":"evaluate","arguments":"state"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			eval := succeedingEvaluator(t)
			h := newHarness(t, eval, Options{})
			h.handshake()

			h.send(`{"jsonrpc":"2.0","id":13,"method":"tools/call"` + tt.params + `}`)

			msg := h.recvMessage()
			if msg.Error == nil || msg.Error.Code != codeInvalidParams {
				t.Fatalf("got %s, want an invalid params error", msg.raw)
			}
			if eval.calls() != 0 {
				t.Errorf("the evaluator ran for a malformed call")
			}
		})
	}
}

func TestEvaluationFailureNeverLeaksTheUnderlyingError(t *testing.T) {
	const secret = "sk-live-SENTINEL-do-not-leak"
	eval := newFake(func(context.Context, typesafe.Request) (typesafe.Response, error) {
		return typesafe.Response{}, fmt.Errorf("POST https://api.typesafe.ai/v1/systemone: 401 with key %s and body <html>denied</html>", secret)
	})
	var logged safeLog
	h := newHarness(t, eval, Options{Logf: logged.logf})
	h.handshake()

	h.callTool(14, sampleArguments)

	msg := h.recvMessage()
	result := h.toolResult(msg)
	if !result.IsError {
		t.Error("a failed evaluation was not reported as a tool error")
	}
	if result.Content[0].Text != msgEvaluationFailed {
		t.Errorf("text = %q, want the generic failure message", result.Content[0].Text)
	}
	for _, forbidden := range []string{secret, "401", "<html>", "api.typesafe.ai"} {
		if strings.Contains(msg.raw, forbidden) {
			t.Errorf("the response repeats %q: %s", forbidden, msg.raw)
		}
		if strings.Contains(logged.String(), forbidden) {
			t.Errorf("the diagnostics repeat %q: %s", forbidden, logged.String())
		}
	}
}

func TestEvaluationFailuresAreClassifiedFromTheAPISentinels(t *testing.T) {
	// Every sentinel the API package exports maps to one fixed message of this
	// package's own. The caller learns what kind of failure it was and what to
	// do about it; the error's own text, which may carry a status line, an
	// address, or a response body, is dropped.
	const secret = "sk-live-SENTINEL <html>denied</html> 10.1.2.3:443"

	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "unauthorized", err: typesafe.ErrUnauthorized, want: msgCredentialRejected},
		{name: "key file", err: typesafe.ErrKeyFile, want: msgCredentialUnusable},
		{name: "no credential", err: typesafe.ErrCredential, want: msgCredentialUnusable},
		{name: "rate limited", err: typesafe.ErrRateLimited, want: msgRateLimited},
		{name: "overloaded", err: typesafe.ErrOverloaded, want: msgOverloaded},
		{name: "request rejected", err: typesafe.ErrRequestRejected, want: msgServiceRejected},
		{name: "response too large", err: typesafe.ErrResponseTooLarge, want: msgResponseTooLarge},
		{name: "invalid response", err: typesafe.ErrInvalidResponse, want: msgResponseInvalid},
		{name: "network", err: typesafe.ErrNetwork, want: msgNetwork},
		{name: "timeout", err: typesafe.ErrTimeout, want: msgEvaluationTimeout},
		{name: "invalid request", err: typesafe.ErrInvalidRequest, want: msgRequestInvalid},
		{name: "request too large", err: typesafe.ErrRequestTooLarge, want: msgRequestInvalid},
		{name: "other service error", err: typesafe.ErrAPI, want: msgServiceError},
		{name: "unrecognised", err: errors.New("something this package has never seen"), want: msgEvaluationFailed},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := fmt.Errorf("%w: %s", tt.err, secret)
			eval := newFake(func(context.Context, typesafe.Request) (typesafe.Response, error) {
				return typesafe.Response{}, failure
			})
			var logged safeLog
			h := newHarness(t, eval, Options{Logf: logged.logf})
			h.handshake()

			h.callTool(19, sampleArguments)

			msg := h.recvMessage()
			result := h.toolResult(msg)
			if !result.IsError {
				t.Errorf("a failed evaluation was not reported as a tool error")
			}
			if result.Content[0].Text != tt.want {
				t.Errorf("text = %q, want %q", result.Content[0].Text, tt.want)
			}
			for _, forbidden := range []string{"sk-live-SENTINEL", "<html>", "10.1.2.3"} {
				if strings.Contains(msg.raw, forbidden) {
					t.Errorf("the response repeats %q: %s", forbidden, msg.raw)
				}
				if strings.Contains(logged.String(), forbidden) {
					t.Errorf("the diagnostics repeat %q: %s", forbidden, logged.String())
				}
			}
		})
	}
}

func TestEvaluationTimeoutIsReported(t *testing.T) {
	eval := newFake(func(ctx context.Context, _ typesafe.Request) (typesafe.Response, error) {
		<-ctx.Done()
		return typesafe.Response{}, ctx.Err()
	})
	h := newHarness(t, eval, Options{EvaluateTimeout: 50 * time.Millisecond})
	h.handshake()

	h.callTool(15, sampleArguments)

	result := h.toolResult(h.recvMessage())
	if !result.IsError || result.Content[0].Text != msgEvaluationTimeout {
		t.Fatalf("result = %+v, want the timeout message", result)
	}

	// The slot is released, so the next call is accepted.
	h.callTool(16, sampleArguments)
	if next := h.toolResult(h.recvMessage()); next.Content[0].Text == msgBusy {
		t.Error("the evaluation slot was not released after a timeout")
	}
}

// responseWithAnswers builds a documented response carrying n noul answers, as
// a way to reach a chosen encoded size.
func responseWithAnswers(n int) string {
	var body strings.Builder
	body.WriteString(`{"model":"jev-1.13.0","answers":{`)
	for i := 0; i < n; i++ {
		if i > 0 {
			body.WriteByte(',')
		}
		fmt.Fprintf(&body, `"q%05d":{"type":"noul","noul":0.5}`, i)
	}
	body.WriteString(`},"usage":{"input_tokens":1,"output_tokens":1}}`)
	return body.String()
}

func TestLargeResultIsReturnedOnceInStructuredContent(t *testing.T) {
	body := responseWithAnswers(2000)
	if len(body) <= maxDuplicatedResultBytes || len(body) > maxResultBytes {
		t.Fatalf("test setup: the response is %d bytes, want it between %d and %d", len(body), maxDuplicatedResultBytes, maxResultBytes)
	}
	res := decodeResponse(t, body)
	eval := newFake(func(context.Context, typesafe.Request) (typesafe.Response, error) {
		return res, nil
	})
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(17, sampleArguments)

	msg := h.recvMessage()
	result := h.toolResult(msg)
	if result.IsError {
		t.Fatalf("a large but returnable result was refused: %+v", result.Content)
	}
	if result.Content[0].Text != msgResultInStructuredContent {
		t.Errorf("text = %q, want the note that points at structuredContent", result.Content[0].Text)
	}
	if len(result.StructuredContent) < maxDuplicatedResultBytes {
		t.Errorf("structuredContent is %d bytes, want the whole evaluation", len(result.StructuredContent))
	}
	if len(msg.raw) > maxFrameBytes {
		t.Errorf("the message is %d bytes, above the frame limit", len(msg.raw))
	}
}

func TestResultAboveTheSizeLimitIsRefusedNotTruncated(t *testing.T) {
	body := responseWithAnswers(5000)
	if len(body) <= maxResultBytes {
		t.Fatalf("test setup: the oversized response is only %d bytes", len(body))
	}
	res := decodeResponse(t, body)
	eval := newFake(func(context.Context, typesafe.Request) (typesafe.Response, error) {
		return res, nil
	})
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(18, sampleArguments)

	msg := h.recvMessage()
	result := h.toolResult(msg)
	if !result.IsError || result.Content[0].Text != msgResultTooLarge {
		t.Fatalf("result = %+v, want the size refusal", result)
	}
	if len(result.StructuredContent) != 0 {
		t.Error("a refused result still carried structuredContent")
	}
	if len(msg.raw) > maxFrameBytes {
		t.Errorf("the refusal itself is %d bytes, above the frame limit", len(msg.raw))
	}
}

// --- concurrency, cancellation, shutdown -------------------------------

// blockingEvaluator returns an evaluator that waits until release is closed or
// its context ends, reporting the context error it saw on ctxErr.
func blockingEvaluator(t *testing.T, release <-chan struct{}) (*fakeEvaluator, <-chan error) {
	t.Helper()
	res := decodeResponse(t, sampleResponseJSON)
	ctxErr := make(chan error, 4)
	eval := newFake(func(ctx context.Context, _ typesafe.Request) (typesafe.Response, error) {
		select {
		case <-release:
			return res, nil
		case <-ctx.Done():
			ctxErr <- ctx.Err()
			return typesafe.Response{}, ctx.Err()
		}
	})
	return eval, ctxErr
}

func TestSecondEvaluationIsRefusedWhileOneIsRunning(t *testing.T) {
	release := make(chan struct{})
	eval, _ := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(20, sampleArguments)
	eval.waitStarted(t)

	h.callTool(21, sampleArguments)
	busy := h.toolResult(h.recvMessage())
	if !busy.IsError || busy.Content[0].Text != msgBusy {
		t.Fatalf("second call result = %+v, want the busy refusal", busy)
	}
	if eval.calls() != 1 {
		t.Errorf("the evaluator ran %d times, want once: the second call must not reach it", eval.calls())
	}

	// The first call still finishes normally.
	close(release)
	first := h.toolResult(h.recvMessage())
	if first.IsError {
		t.Errorf("the running evaluation failed after the refusal: %+v", first)
	}

	// And the slot is free again.
	h.callTool(22, sampleArguments)
	if again := h.toolResult(h.recvMessage()); again.IsError {
		t.Errorf("a later call was refused: %+v", again)
	}
}

func TestPingAndListAreAnsweredWhileAnEvaluationRuns(t *testing.T) {
	release := make(chan struct{})
	eval, _ := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(30, sampleArguments)
	eval.waitStarted(t)

	h.send(`{"jsonrpc":"2.0","id":31,"method":"ping"}`)
	if msg := h.recvMessage(); msg.Error != nil || string(msg.ID) != "31" {
		t.Fatalf("ping during an evaluation: got %s", msg.raw)
	}
	h.send(`{"jsonrpc":"2.0","id":32,"method":"tools/list"}`)
	if msg := h.recvMessage(); msg.Error != nil || string(msg.ID) != "32" {
		t.Fatalf("tools/list during an evaluation: got %s", msg.raw)
	}

	close(release)
	if msg := h.recvMessage(); string(msg.ID) != "30" {
		t.Fatalf("the evaluation response carries id %s, want 30", msg.ID)
	}
}

func TestRequestReusingAnInFlightIDIsRejected(t *testing.T) {
	release := make(chan struct{})
	eval, _ := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(40, sampleArguments)
	eval.waitStarted(t)

	h.send(`{"jsonrpc":"2.0","id":40,"method":"ping"}`)

	msg := h.recvMessage()
	if msg.Error == nil || msg.Error.Message != msgDuplicateID {
		t.Fatalf("got %s, want the duplicate id rejection", msg.raw)
	}

	// The string "40" is a different id from the number 40, so it is not in
	// flight and is answered normally.
	h.send(`{"jsonrpc":"2.0","id":"40","method":"ping"}`)
	if next := h.recvMessage(); next.Error != nil || string(next.ID) != `"40"` {
		t.Fatalf("got %s, want the string id to be treated as a different id", next.raw)
	}

	close(release)
	if next := h.recvMessage(); next.Error != nil {
		t.Fatalf("the original request did not complete: %s", next.raw)
	}
}

func TestAnIDIsUsableAgainOnceItsRequestHasFinished(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})
	h.handshake()

	h.callTool(41, sampleArguments)
	if first := h.recvMessage(); first.Error != nil {
		t.Fatalf("the first call failed: %s", first.raw)
	}

	// Nothing is in flight now, so the same id is not a duplicate.
	h.callTool(41, sampleArguments)

	second := h.recvMessage()
	if second.Error != nil {
		t.Fatalf("got %s, want the id to be reusable once its request finished", second.raw)
	}
	if result := h.toolResult(second); result.IsError {
		t.Errorf("the second call failed: %+v", result.Content)
	}
}

func TestCancellationStopsTheEvaluationAndSendsNoResponse(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	eval, ctxErr := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(50, sampleArguments)
	eval.waitStarted(t)

	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":50,"reason":"user pressed escape"}}`)

	select {
	case err := <-ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the evaluation context ended with %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the cancellation did not reach the evaluation while it was pending")
	}

	// The protocol says a cancelled request gets no response.
	h.expectQuiet()

	// The session and the evaluation slot are both usable again. The second
	// call reaching the evaluator is what proves the slot was released: this
	// evaluator blocks, so there is no result to wait for.
	h.send(`{"jsonrpc":"2.0","id":51,"method":"ping"}`)
	if msg := h.recvMessage(); msg.Error != nil {
		t.Fatalf("got %s, want a ping response after a cancellation", msg.raw)
	}
	h.callTool(52, sampleArguments)
	eval.waitStarted(t)
	if eval.calls() != 2 {
		t.Errorf("the evaluator ran %d times, want the call after the cancellation to have been accepted", eval.calls())
	}
}

func TestCancellingAnotherIDLeavesTheEvaluationRunning(t *testing.T) {
	release := make(chan struct{})
	eval, _ := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{})
	h.handshake()

	h.callTool(60, sampleArguments)
	eval.waitStarted(t)

	// A different id, a string that looks like the number, and the initialize
	// id: none of them name the evaluation in flight.
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":61}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":"60"}}`)
	h.send(`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":2}}`)
	h.expectQuiet()

	close(release)
	msg := h.recvMessage()
	if string(msg.ID) != "60" {
		t.Fatalf("got %s, want the evaluation to have survived", msg.raw)
	}
	if result := h.toolResult(msg); result.IsError {
		t.Errorf("the surviving evaluation failed: %+v", result)
	}
}

func TestEndOfInputEndsTheSession(t *testing.T) {
	h := newHarness(t, succeedingEvaluator(t), Options{})
	h.handshake()

	h.in.Close()

	if err := h.waitServe(); err != nil {
		t.Errorf("Serve() = %v, want nil at end of input", err)
	}
}

func TestEndOfInputCancelsAnEvaluationInFlight(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	eval, ctxErr := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{ShutdownGrace: time.Second})
	h.handshake()

	h.callTool(70, sampleArguments)
	eval.waitStarted(t)

	h.in.Close()

	if err := h.waitServe(); err != nil {
		t.Errorf("Serve() = %v, want nil at end of input", err)
	}
	select {
	case err := <-ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the evaluation context ended with %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the evaluation was not cancelled when the input ended")
	}
}

func TestContextCancellationShutsDownWithinTheGracePeriod(t *testing.T) {
	// An evaluation that ignores its context entirely: shutdown must not wait
	// for it beyond the grace period.
	stuck := make(chan struct{})
	defer close(stuck)
	eval := newFake(func(context.Context, typesafe.Request) (typesafe.Response, error) {
		<-stuck
		return typesafe.Response{}, nil
	})
	h := newHarness(t, eval, Options{ShutdownGrace: 100 * time.Millisecond})
	h.handshake()

	h.callTool(80, sampleArguments)
	eval.waitStarted(t)

	start := time.Now()
	h.cancel()

	if err := h.waitServe(); err != nil {
		t.Errorf("Serve() = %v, want nil after its context was cancelled", err)
	}
	if elapsed := time.Since(start); elapsed > testTimeout {
		t.Errorf("shutdown took %v, want it bounded by the grace period", elapsed)
	}
}

func TestShutdownSendsNoResponseForTheCancelledEvaluation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	eval, _ := blockingEvaluator(t, release)
	h := newHarness(t, eval, Options{ShutdownGrace: time.Second})
	h.handshake()

	h.callTool(90, sampleArguments)
	eval.waitStarted(t)

	h.cancel()

	if err := h.waitServe(); err != nil {
		t.Errorf("Serve() = %v, want nil", err)
	}
	// The output stream is closed when Serve returns; anything still in it
	// would be a response to the request that shutdown cancelled.
	for line := range h.lines {
		t.Errorf("the server answered a cancelled request during shutdown: %s", line)
	}
}

// --- a consumer that stops reading -------------------------------------

// stuckOutput drives a server whose consumer accepts the handshake response and
// then stops reading, with one evaluation running and one response stuck in the
// write. It returns the server's input, the writer, the evaluator's context
// error channel, and the channel Serve's result arrives on.
func stuckOutput(t *testing.T, ctx context.Context, release <-chan struct{}) (*io.PipeWriter, *blockingWriter, <-chan error, <-chan error) {
	t.Helper()

	eval, ctxErr := blockingEvaluator(t, release)
	out := newBlockingWriter(1) // the initialize response, and nothing after it
	inR, inW := io.Pipe()

	// A write budget far beyond the test's patience: if this test passes, it is
	// because cancellation or the end of the input released the write, not
	// because the budget expired.
	server := NewServer(eval, Options{ShutdownGrace: time.Second, WriteTimeout: time.Minute})
	served := make(chan error, 1)
	go func() { served <- server.Serve(ctx, inR, out) }()

	send := func(line string) {
		t.Helper()
		if _, err := io.WriteString(inW, line+"\n"); err != nil {
			t.Fatalf("writing to the server: %v", err)
		}
	}
	send(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`)
	send(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	send(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"evaluate","arguments":` + sampleArguments + `}}`)
	eval.waitStarted(t)

	// This response is the one that gets stuck.
	send(`{"jsonrpc":"2.0","id":3,"method":"ping"}`)
	out.waitBlocked(t)

	if got := out.accepted(); len(got) != 1 {
		t.Fatalf("the consumer accepted %d messages before blocking, want the initialize response only", len(got))
	}
	return inW, out, ctxErr, served
}

func TestBlockedOutputDoesNotPreventCancellation(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())

	inW, out, ctxErr, served := stuckOutput(t, ctx, release)
	defer inW.Close()
	defer close(out.release)

	cancel()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve() = %v, want nil after its context was cancelled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Serve did not return while a write was blocked")
	}
	select {
	case err := <-ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the evaluation context ended with %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the evaluation was not cancelled while a write was blocked")
	}
}

func TestBlockedOutputDoesNotDelayTheEndOfInput(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	inW, out, ctxErr, served := stuckOutput(t, ctx, release)
	defer close(out.release)

	// The client goes away without cancelling anything.
	inW.Close()

	select {
	case err := <-served:
		if err != nil {
			t.Errorf("Serve() = %v, want nil at end of input", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("Serve did not return after the input ended while a write was blocked")
	}
	select {
	case err := <-ctxErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("the evaluation context ended with %v, want context.Canceled", err)
		}
	case <-time.After(testTimeout):
		t.Fatal("the evaluation was not cancelled when the input ended while a write was blocked")
	}
}

func TestServerWithoutAnEvaluatorRefusesCallsInsteadOfPanicking(t *testing.T) {
	h := newHarness(t, nil, Options{})
	h.handshake()

	h.callTool(100, sampleArguments)

	result := h.toolResult(h.recvMessage())
	if !result.IsError || result.Content[0].Text != msgNoEvaluator {
		t.Fatalf("result = %+v, want the missing evaluator refusal", result)
	}
}
