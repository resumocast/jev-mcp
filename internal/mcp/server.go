// Package mcp implements the Jev Model Context Protocol server: a bounded,
// standard-library-only JSON-RPC 2.0 server that speaks the MCP stdio
// transport and exposes evaluate plus an optional evidence-selection tool.
//
// The transport is one JSON message per line, terminated by LF. The writer
// carries protocol traffic and nothing else: diagnostics go to the optional
// logger in [Options], which the command wires to stderr. No prompt, no
// evaluation result, and no credential is ever logged.
//
// # Bounds
//
// One message is at most 128 KiB in either direction. One evaluation runs at a
// time, in one goroutine, and a second call is refused rather than queued, so
// neither a chatty client nor a slow API can grow this process without limit.
// Every request is answered from the same loop that reads the input, so the
// protocol state is owned by one goroutine and needs no locking. Writes are
// bounded too: they run on the writer's own goroutine and are given up on when
// the context ends, when the input stream ends, or when their budget expires,
// so a consumer that stops reading cannot stop this server from shutting down.
//
// # Errors
//
// A tool failure comes back as a result with isError set and a message built
// from literals in this package. An error from the API package is classified
// through the sentinels that package exports, so the caller learns what kind
// of failure it was without the raw error, the response body, or the
// credential ever reaching the client, the transcript, or the log.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"syscall"
	"time"

	"jev-mcp/internal/jsonfields"
	"jev-mcp/internal/typesafe"
)

// MCP methods this server recognises.
const (
	methodInitialize  = "initialize"
	methodInitialized = "notifications/initialized"
	methodCancelled   = "notifications/cancelled"
	methodPing        = "ping"
	methodToolsList   = "tools/list"
	methodToolsCall   = "tools/call"
)

// defaultProtocolVersion is what this server advertises when the client asks
// for a version it does not know. The client decides whether to continue.
const defaultProtocolVersion = "2024-11-05"

// supportedProtocolVersions are the revisions whose wire shape this server
// implements. They differ in features this server does not use, so the
// difference is limited to the version string it echoes back.
var supportedProtocolVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18"}

// serverName identifies this implementation in the initialize result.
const serverName = "jev-mcp"

// Bounds on a single evaluation and on the handshake metadata.
const (
	// defaultEvaluateTimeout is an upper bound on one tool call, above the
	// total deadline the API package applies to the HTTP exchange. It exists so
	// that a client which never cancels still gets its answer, and so the one
	// evaluation slot is always released.
	defaultEvaluateTimeout = 40 * time.Second

	// defaultShutdownGrace bounds how long shutdown waits for a cancelled
	// evaluation to return before giving up on it.
	defaultShutdownGrace = 2 * time.Second

	// defaultWriteTimeout bounds one message write to a consumer that is still
	// connected but has stopped reading. Cancellation and end of input end a
	// write sooner; this is the backstop for a client that does neither.
	defaultWriteTimeout = 10 * time.Second

	// maxDuplicatedResultBytes is the largest evaluation JSON this server will
	// carry twice in one result, as text and as structuredContent. Escaping the
	// text copy can double it, so three times this size plus the envelope has to
	// fit inside maxFrameBytes. Every ordinary evaluation is far below it.
	maxDuplicatedResultBytes = 40 << 10

	// maxResultBytes bounds the evaluation JSON this server will return at all.
	// Between the two limits the answers are sent once, in structuredContent,
	// with a note in place of the text copy: a response that large is legitimate
	// (16 score questions carry their level descriptions back in each legend),
	// and returning it once is better than refusing it.
	maxResultBytes = 126 << 10

	// maxClientInfoBytes bounds the name and version a client reports about
	// itself. The values are validated and then dropped: nothing a client says
	// about itself appears in a response or in a diagnostic.
	maxClientInfoBytes = 128
)

// Messages sent to the client. Every one of them is a literal: nothing derived
// from an API response body, a credential, or a filesystem path appears here.
const (
	msgFrameTooLarge       = "the message exceeds the 128 KiB limit on a single JSON-RPC message"
	msgNoBatch             = "batch requests are not supported"
	msgNotAnObject         = "a JSON-RPC message must be a JSON object"
	msgParseError          = "the message is not valid JSON"
	msgInvalidRequest      = "the message is not a valid JSON-RPC request"
	msgAmbiguousMessage    = "a message may carry a method or a result or an error, not both"
	msgWrongVersion        = `the jsonrpc member must be "2.0"`
	msgBadID               = "the id of a request must be a string or an integer"
	msgDuplicateID         = "a request with this id is already in flight"
	msgMethodNotFound      = "unknown method"
	msgAlreadyInitialized  = "the server is already initialized"
	msgNotInitialized      = "the server is not initialized; send initialize first"
	msgNotReady            = "initialization is not complete; send the notifications/initialized notification first"
	msgBadInitializeParams = "initialize requires an object with a protocolVersion string, a capabilities object, and a clientInfo object naming the client and its version"
	msgBadCallParams       = "tools/call requires an object with a name and an arguments object"
	msgUnknownTool         = "unknown tool"
	msgBusy                = "this server runs one evaluation at a time and another one is already running; retry after it finishes"
	msgResultTooLarge      = "the evaluation result was too large to return"
	msgNoEvaluator         = "this server has no evaluator configured"
)

// Failure messages for an evaluation that reached the API package. Each one
// corresponds to one exported sentinel, so the caller learns what kind of
// failure happened and what could be done about it, while the error's own text
// (which may quote a remote body or a status line) is discarded.
const (
	msgCredentialRejected = "the evaluation service rejected the credential; check the dedicated key file"
	msgCredentialUnusable = "the credential file could not be used; check that it exists and is readable only by its owner"
	msgRateLimited        = "the evaluation service is rate limiting this client; retry later"
	msgOverloaded         = "the evaluation service is temporarily overloaded; retry shortly"
	msgServiceRejected    = "the evaluation service rejected the request; check the state and the question rubrics"
	msgServiceError       = "the evaluation service returned an error"
	msgResponseInvalid    = "the evaluation service returned an answer this server could not validate"
	msgResponseTooLarge   = "the evaluation service returned an answer larger than this server accepts"
	msgNetwork            = "the evaluation service could not be reached"
	msgRequestInvalid     = "the evaluation request was rejected before it was sent"
	msgEvaluationTimeout  = "the evaluation timed out"
	msgEvaluationFailed   = "the evaluation did not complete"
)

// Evaluator is the part of the API client this server depends on. It is
// declared here, next to its use, so the protocol can be exercised against a
// fake and so nothing in this package can reach the network on its own. It
// mirrors [typesafe.Evaluator]; the assertion below fails to compile if the
// two ever diverge.
type Evaluator interface {
	Evaluate(ctx context.Context, req typesafe.Request) (typesafe.Response, error)
}

var _ Evaluator = (*typesafe.Client)(nil)

// Options configures a server. The zero value is usable: the timeouts fall back
// to their defaults and no diagnostics are written.
type Options struct {
	// Version is reported in the initialize result.
	Version string

	// Logf receives short diagnostics. It must not write to the protocol
	// stream. It is called only from the serving goroutine.
	Logf func(format string, args ...any)

	// EvaluateTimeout bounds one tool call. Zero means defaultEvaluateTimeout.
	EvaluateTimeout time.Duration

	// ShutdownGrace bounds the wait for a cancelled evaluation during shutdown.
	// Zero means defaultShutdownGrace.
	ShutdownGrace time.Duration

	// WriteTimeout bounds one message write. Zero means defaultWriteTimeout.
	WriteTimeout time.Duration

	// EnableSelection exposes the opt-in select_evidence tool. It is off by
	// default, leaving the original evaluate-only interface unchanged.
	EnableSelection bool
}

// handshake tracks how far the client has got through initialization.
type handshake int

const (
	// stateNew is before the initialize request.
	stateNew handshake = iota
	// stateInitializing is after the initialize response and before the
	// notifications/initialized notification.
	stateInitializing
	// stateReady is when tools may be listed and called.
	stateReady
)

// Server serves one client over one pair of streams. A Server is used once:
// [Server.Serve] returns when the input ends or the context is cancelled.
type Server struct {
	eval            Evaluator
	version         string
	logf            func(format string, args ...any)
	evaluateTimeout time.Duration
	shutdownGrace   time.Duration
	writeTimeout    time.Duration
	enableSelection bool

	out *frameWriter

	// Protocol state. Only the goroutine running Serve touches these fields.
	state           handshake
	protocolVersion string

	// The evaluation in flight, if any. activeDone is nil when idle, which also
	// makes its case in the serve loop's select block forever.
	activeDone      chan evalOutcome
	activeID        requestID
	activeCancel    context.CancelFunc
	activeCancelled bool
}

// NewServer returns a server that evaluates through ev, which must not be nil.
func NewServer(ev Evaluator, opts Options) *Server {
	s := &Server{
		eval:            ev,
		version:         opts.Version,
		logf:            opts.Logf,
		evaluateTimeout: opts.EvaluateTimeout,
		shutdownGrace:   opts.ShutdownGrace,
		writeTimeout:    opts.WriteTimeout,
		enableSelection: opts.EnableSelection,
	}
	if s.version == "" {
		s.version = "dev"
	}
	if s.evaluateTimeout <= 0 {
		s.evaluateTimeout = defaultEvaluateTimeout
	}
	if s.shutdownGrace <= 0 {
		s.shutdownGrace = defaultShutdownGrace
	}
	if s.writeTimeout <= 0 {
		s.writeTimeout = defaultWriteTimeout
	}
	return s
}

// evalOutcome is what the evaluation goroutine reports back to the serve loop.
// The loop, not the goroutine, writes the response, so message ordering and
// protocol state stay under the control of a single goroutine.
type evalOutcome struct {
	id        requestID
	res       typesafe.Response
	selection *selectionPlan
	err       error
}

// readItem is one message from the input stream, or the error that ended or
// interrupted it.
type readItem struct {
	frame []byte
	err   error
}

// Serve reads messages from in and writes responses to out until the input
// ends, the input becomes unusable, or ctx is cancelled. It returns nil for an
// ordinary end of input, including a closed pipe and a cancelled context.
//
// Cancelling ctx (the command does this on SIGINT and SIGTERM) cancels an
// evaluation in flight and returns within the shutdown grace period, even if
// the consumer of out has stopped reading. The goroutine reading the input may
// still be blocked in a read at that point, and so may the goroutine writing
// the output: there is no portable way to interrupt either, so Serve does not
// wait for them. The process exits and the descriptors go with it.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Closed by the reader when the input stream ends. A write in progress is
	// given up on as soon as it closes: once the client is gone there is
	// nothing left to read the answer, and waiting out the write budget would
	// delay cancelling the evaluation that is still running.
	inputEnded := make(chan struct{})

	s.out = newFrameWriter(out, inputEnded, s.writeTimeout)
	defer s.out.close()

	frames := make(chan readItem)
	stop := make(chan struct{})
	defer close(stop)
	go readLoop(newFrameReader(in), frames, stop, inputEnded)

	for {
		select {
		case <-ctx.Done():
			s.log("shutting down: the context was cancelled")
			s.shutdown()
			return nil

		case outcome := <-s.activeDone:
			if err := s.finishEvaluate(ctx, outcome); err != nil {
				return s.terminate(err)
			}

		case item := <-frames:
			switch {
			case item.err == nil:
				if err := s.dispatch(ctx, item.frame); err != nil {
					return s.terminate(err)
				}
			case errors.Is(item.err, errFrameTooLarge):
				if err := s.replyError(ctx, nil, codeInvalidRequest, msgFrameTooLarge); err != nil {
					return s.terminate(err)
				}
			case errors.Is(item.err, errUnsynchronized):
				s.log("shutting down: the input stream could not be resynchronized")
				return s.terminate(item.err)
			default:
				// End of input, including a closed pipe: the client is gone.
				s.log("shutting down: the input stream ended")
				return s.terminate(item.err)
			}
		}
	}
}

// terminate stops any evaluation in flight and decides whether cause is worth
// reporting to the caller. A stream that ended, a pipe the peer closed, a
// cancelled context, and a write abandoned because the input had already ended
// are all ordinary ends of service rather than failures.
func (s *Server) terminate(cause error) error {
	s.shutdown()
	switch {
	case cause == nil,
		errors.Is(cause, io.EOF),
		errors.Is(cause, context.Canceled),
		errors.Is(cause, context.DeadlineExceeded),
		errors.Is(cause, errWriteAborted),
		isClosedPipe(cause):
		return nil
	}
	return cause
}

// shutdown cancels an evaluation in flight and waits a bounded time for its
// goroutine to return. The result is discarded: a cancelled request gets no
// response, and by this point there is nobody left to read one.
func (s *Server) shutdown() {
	if s.activeDone == nil {
		return
	}
	s.activeCancel()
	timer := time.NewTimer(s.shutdownGrace)
	defer timer.Stop()
	select {
	case <-s.activeDone:
	case <-timer.C:
		s.log("an evaluation did not stop within the shutdown grace period")
	}
	s.activeDone = nil
	s.activeCancel = nil
	s.activeCancelled = false
	s.activeID = requestID{}
}

// isClosedPipe reports whether err is the peer closing the other end.
func isClosedPipe(err error) bool {
	return errors.Is(err, io.ErrClosedPipe) || errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EPIPE)
}

// readLoop turns the input stream into messages. An oversized message is
// reported and reading continues, because readFrame has already skipped to the
// end of it; any other error ends the loop.
func readLoop(r *frameReader, out chan<- readItem, stop <-chan struct{}, inputEnded chan<- struct{}) {
	for {
		frame, err := r.readFrame()
		terminal := err != nil && !errors.Is(err, errFrameTooLarge)
		if terminal {
			// Announce the end of the input before handing the item over. A
			// serve loop blocked writing to a consumer that has gone away has
			// to be able to give up on that write before it can accept this
			// item, and closing here is what lets it.
			close(inputEnded)
		}
		select {
		case out <- readItem{frame: frame, err: err}:
		case <-stop:
			return
		}
		if terminal {
			return
		}
	}
}

func (s *Server) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// reply answers a request. A result too large to frame is replaced by a
// bounded error, so a client is never left waiting for a message this server
// decided not to send.
func (s *Server) reply(ctx context.Context, id requestID, result any) error {
	err := s.out.write(ctx, &response{JSONRPC: jsonrpcVersion, ID: id.raw, Result: result})
	if errors.Is(err, errOversizeOutgoing) {
		s.log("a response was too large to send and was replaced by an error")
		return s.replyError(ctx, id.raw, codeInternalError, msgResultTooLarge)
	}
	return err
}

// replyError answers with a JSON-RPC error. A nil id produces the null id that
// JSON-RPC requires when the request's own id could not be determined.
func (s *Server) replyError(ctx context.Context, id json.RawMessage, code int, message string) error {
	return s.out.write(ctx, &response{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: message}})
}

// dispatch handles one incoming message. It returns an error only when the
// connection cannot continue, which in practice means the output stream failed.
func (s *Server) dispatch(ctx context.Context, frame []byte) error {
	trimmed := bytes.TrimSpace(frame)
	if len(trimmed) == 0 {
		return nil
	}
	// Syntax first, shape second: a malformed scalar such as an unquoted word
	// or a truncated number is a parse error, while a well-formed scalar is a
	// valid JSON document that is not a valid JSON-RPC message.
	if !json.Valid(trimmed) {
		return s.replyError(ctx, nil, codeParseError, msgParseError)
	}
	switch trimmed[0] {
	case '[':
		// A batch cannot be answered piecewise, and this server does not
		// implement one. Saying so is better than a parse error.
		return s.replyError(ctx, nil, codeInvalidRequest, msgNoBatch)
	case '{':
	default:
		return s.replyError(ctx, nil, codeInvalidRequest, msgNotAnObject)
	}

	if !validStructure(trimmed) {
		return s.replyError(ctx, nil, codeInvalidRequest, msgInvalidRequest)
	}
	var msg incoming
	if err := jsonfields.Unmarshal(trimmed, &msg, "jsonrpc", "id", "method", "params", "result", "error"); err != nil {
		// The document is valid JSON, so this is a member of the wrong type.
		return s.replyError(ctx, nil, codeInvalidRequest, msgInvalidRequest)
	}

	notification := len(bytes.TrimSpace(msg.ID)) == 0
	answered := len(msg.Result) > 0 || len(msg.Error) > 0

	switch {
	case answered && msg.Method != "":
		// Both a call and an answer. Guessing which one was meant is how a
		// request gets executed twice, or an answer gets executed as a request.
		if notification {
			return nil
		}
		return s.replyError(ctx, nil, codeInvalidRequest, msgAmbiguousMessage)
	case answered:
		// An answer to a request this server never sent. Answering an answer
		// is not allowed, so drop it.
		return nil
	case msg.Method == "":
		if notification {
			return nil
		}
		return s.replyError(ctx, nil, codeInvalidRequest, msgInvalidRequest)
	}

	if msg.JSONRPC != jsonrpcVersion {
		if notification {
			return nil
		}
		return s.replyError(ctx, nil, codeInvalidRequest, msgWrongVersion)
	}
	if len(msg.Method) > maxMethodBytes {
		if notification {
			return nil
		}
		return s.replyError(ctx, nil, codeMethodNotFound, msgMethodNotFound)
	}

	if notification {
		s.handleNotification(msg.Method, msg.Params)
		return nil
	}

	id, ok := parseID(msg.ID)
	if !ok {
		return s.replyError(ctx, nil, codeInvalidRequest, msgBadID)
	}
	// The only request that outlives its dispatch is an evaluation, so the set
	// of ids in flight has at most one member.
	if s.activeDone != nil && s.activeID.key == id.key {
		return s.replyError(ctx, id.raw, codeInvalidRequest, msgDuplicateID)
	}
	return s.handleRequest(ctx, id, msg.Method, msg.Params)
}

func (s *Server) handleRequest(ctx context.Context, id requestID, method string, params json.RawMessage) error {
	switch method {
	case methodInitialize:
		if s.state != stateNew {
			return s.replyError(ctx, id.raw, codeInvalidRequest, msgAlreadyInitialized)
		}
		result, err := s.buildInitializeResult(params)
		if err != nil {
			return s.replyError(ctx, id.raw, codeInvalidParams, msgBadInitializeParams)
		}
		// The state advances only once the response is on the wire, so a failed
		// write cannot leave the server believing a handshake happened.
		if err := s.reply(ctx, id, result); err != nil {
			return err
		}
		s.state = stateInitializing
		s.protocolVersion = result.ProtocolVersion
		return nil

	case methodPing:
		// Ping is answered in any state: it is how a client checks that the
		// process is alive, including before the handshake.
		return s.reply(ctx, id, struct{}{})

	case methodToolsList:
		if ok, err := s.ready(ctx, id); !ok {
			return err
		}
		tools := []toolDescriptor{evaluateTool()}
		if s.enableSelection {
			tools = append(tools, selectionTool())
		}
		return s.reply(ctx, id, toolsListResult{Tools: tools})

	case methodToolsCall:
		if ok, err := s.ready(ctx, id); !ok {
			return err
		}
		return s.handleToolCall(ctx, id, params)

	default:
		return s.replyError(ctx, id.raw, codeMethodNotFound, msgMethodNotFound)
	}
}

// ready reports whether tools may be listed and called. When they may not, it
// has already answered with the reason the handshake is incomplete, and the
// error it returns is a failure to write that answer.
func (s *Server) ready(ctx context.Context, id requestID) (bool, error) {
	switch s.state {
	case stateNew:
		return false, s.replyError(ctx, id.raw, codeNotInitialized, msgNotInitialized)
	case stateInitializing:
		return false, s.replyError(ctx, id.raw, codeNotInitialized, msgNotReady)
	}
	return true, nil
}

func (s *Server) handleNotification(method string, params json.RawMessage) {
	switch method {
	case methodInitialized:
		if s.state == stateInitializing {
			s.state = stateReady
		}
		// Out of order, this notification is meaningless rather than fatal, and
		// a notification is never answered.

	case methodCancelled:
		s.handleCancelled(params)

	default:
		// Unknown notifications are ignored, as the protocol requires.
	}
}

// handleCancelled cancels the evaluation in flight if the notification names
// it. The reason field is deliberately not read: it is remote text with no use
// here, and logging it would put client-supplied strings in the diagnostics.
func (s *Server) handleCancelled(params json.RawMessage) {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if err := jsonfields.Unmarshal(params, &p, "requestId"); err != nil {
		return
	}
	id, ok := parseID(p.RequestID)
	if !ok || s.activeDone == nil || s.activeID.key != id.key {
		// Nothing to cancel: the request already finished, was never in flight,
		// or is the initialize request, which the protocol says may not be
		// cancelled and which never outlives its dispatch here anyway.
		return
	}
	s.activeCancelled = true
	s.activeCancel()
	s.log("an evaluation was cancelled by the client")
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeParams struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ClientInfo      json.RawMessage `json:"clientInfo"`
}

type toolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

type serverCapabilities struct {
	Tools *toolsCapability `json:"tools,omitempty"`
}

type implementation struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    serverCapabilities `json:"capabilities"`
	ServerInfo      implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// buildInitializeResult validates the handshake metadata and builds the
// response. The protocol requires a protocol version, a capabilities object
// and a client identity, and all three are checked rather than assumed: a
// client that cannot manage the handshake it started is one this server should
// not begin an evaluation for.
//
// What the client says about itself is validated and then dropped. It is not
// echoed, not logged, and not used, so no string a client chose can travel back
// out of this server.
//
// An unknown protocol version is answered with the version this server speaks
// rather than refused: the protocol puts that decision on the client.
func (s *Server) buildInitializeResult(params json.RawMessage) (initializeResult, error) {
	if kindOf(params) != kindObject {
		return initializeResult{}, errors.New(msgBadInitializeParams)
	}
	var p initializeParams
	if err := jsonfields.Unmarshal(params, &p, "protocolVersion", "capabilities", "clientInfo"); err != nil {
		return initializeResult{}, errors.New(msgBadInitializeParams)
	}
	if !boundedASCII(p.ProtocolVersion, 1, 64) {
		return initializeResult{}, errors.New(msgBadInitializeParams)
	}
	if kindOf(p.Capabilities) != kindObject {
		return initializeResult{}, errors.New(msgBadInitializeParams)
	}
	if err := validateClientInfo(p.ClientInfo); err != nil {
		return initializeResult{}, err
	}
	return initializeResult{
		ProtocolVersion: negotiateProtocol(p.ProtocolVersion),
		Capabilities:    serverCapabilities{Tools: &toolsCapability{}},
		ServerInfo:      implementation{Name: serverName, Version: s.version},
		Instructions:    serverInstructions,
	}, nil
}

// validateClientInfo requires the object the protocol requires: a name and a
// version, both bounded printable strings, neither null nor absent.
func validateClientInfo(raw json.RawMessage) error {
	if kindOf(raw) != kindObject {
		return errors.New(msgBadInitializeParams)
	}
	var info clientInfo
	if err := jsonfields.Unmarshal(raw, &info, "name", "version"); err != nil {
		return errors.New(msgBadInitializeParams)
	}
	if !boundedASCII(info.Name, 1, maxClientInfoBytes) {
		return errors.New(msgBadInitializeParams)
	}
	if !boundedASCII(info.Version, 1, maxClientInfoBytes) {
		return errors.New(msgBadInitializeParams)
	}
	return nil
}

// boundedASCII reports whether s is printable ASCII within the given length
// bounds. Handshake metadata is measured before it is trusted, even when it is
// only going to be discarded.
func boundedASCII(s string, minBytes, maxBytes int) bool {
	if len(s) < minBytes || len(s) > maxBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

func negotiateProtocol(requested string) string {
	if slices.Contains(supportedProtocolVersions, requested) {
		return requested
	}
	return defaultProtocolVersion
}

type callToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// handleToolCall validates a call and starts the evaluation. A call this
// server will not run comes back as a result with isError set, which is what a
// model can act on; only a malformed call is a JSON-RPC error.
func (s *Server) handleToolCall(ctx context.Context, id requestID, params json.RawMessage) error {
	if kindOf(params) != kindObject {
		return s.replyError(ctx, id.raw, codeInvalidParams, msgBadCallParams)
	}
	var p callToolParams
	if err := jsonfields.Unmarshal(params, &p, "name", "arguments"); err != nil {
		return s.replyError(ctx, id.raw, codeInvalidParams, msgBadCallParams)
	}
	if p.Name != toolName && (p.Name != selectionToolName || !s.enableSelection) {
		return s.replyError(ctx, id.raw, codeInvalidParams, msgUnknownTool)
	}
	if len(p.Arguments) > 0 && kindOf(p.Arguments) != kindObject && kindOf(p.Arguments) != kindNull {
		return s.replyError(ctx, id.raw, codeInvalidParams, msgBadCallParams)
	}
	if s.eval == nil {
		return s.reply(ctx, id, toolFailure(msgNoEvaluator))
	}
	if s.activeDone != nil {
		// One evaluation at a time, refused rather than queued: a queue is an
		// unbounded goroutine count and an unbounded bill.
		return s.reply(ctx, id, toolFailure(msgBusy))
	}
	var (
		req       typesafe.Request
		selection *selectionPlan
		err       error
	)
	if p.Name == selectionToolName {
		req, selection, err = normalizeSelectionArguments(p.Arguments)
	} else {
		req, err = normalizeArguments(p.Arguments)
	}
	if err != nil {
		return s.reply(ctx, id, toolFailure(err.Error()))
	}
	s.startEvaluate(ctx, id, req, selection)
	return nil
}

// startEvaluate runs one evaluation in one goroutine. The goroutine touches no
// server state: it sends its outcome to the serve loop, which owns everything
// else.
func (s *Server) startEvaluate(ctx context.Context, id requestID, req typesafe.Request, selection *selectionPlan) {
	ctx, cancel := context.WithTimeout(ctx, s.evaluateTimeout)
	done := make(chan evalOutcome, 1)

	s.activeDone = done
	s.activeID = id
	s.activeCancel = cancel
	s.activeCancelled = false

	eval := s.eval
	go func() {
		res, err := eval.Evaluate(ctx, req)
		done <- evalOutcome{id: id, res: res, selection: selection, err: err}
	}()
}

// finishEvaluate releases the evaluation slot and answers the call.
func (s *Server) finishEvaluate(ctx context.Context, outcome evalOutcome) error {
	s.activeCancel()
	cancelled := s.activeCancelled
	s.activeDone = nil
	s.activeCancel = nil
	s.activeCancelled = false
	s.activeID = requestID{}

	if cancelled {
		// The protocol says a cancelled request gets no response.
		return nil
	}
	if outcome.err != nil {
		message := classify(outcome.err)
		s.log("an evaluation failed: %s", message)
		return s.reply(ctx, outcome.id, toolFailure(message))
	}
	if outcome.selection != nil {
		result, err := buildSelectionResult(outcome.selection, outcome.res)
		if err != nil {
			s.log("a selection result could not be validated")
			return s.reply(ctx, outcome.id, toolFailure(msgResponseInvalid))
		}
		return s.reply(ctx, outcome.id, result)
	}

	body, err := json.Marshal(outcome.res)
	if err != nil {
		s.log("an evaluation result could not be encoded")
		return s.reply(ctx, outcome.id, toolFailure(msgEvaluationFailed))
	}
	switch {
	case len(body) > maxResultBytes:
		s.log("an evaluation result exceeded the result size limit")
		return s.reply(ctx, outcome.id, toolFailure(msgResultTooLarge))
	case len(body) > maxDuplicatedResultBytes:
		s.log("an evaluation result was returned in structuredContent only")
		return s.reply(ctx, outcome.id, toolSuccessStructuredOnly(body))
	default:
		return s.reply(ctx, outcome.id, toolSuccess(body))
	}
}

// classify names the kind of failure an evaluation hit, using the sentinels the
// API package exports. The error's own text is never used: it is the one string
// in this path that could carry a response body, a status line, or an address,
// and a tool result that a model reads and a user sees is not the place to
// discover that an assumption about it was wrong.
func classify(err error) string {
	switch {
	case errors.Is(err, typesafe.ErrUnauthorized):
		return msgCredentialRejected
	case errors.Is(err, typesafe.ErrKeyFile), errors.Is(err, typesafe.ErrCredential):
		return msgCredentialUnusable
	case errors.Is(err, typesafe.ErrRateLimited):
		return msgRateLimited
	case errors.Is(err, typesafe.ErrOverloaded):
		return msgOverloaded
	case errors.Is(err, typesafe.ErrRequestRejected):
		return msgServiceRejected
	case errors.Is(err, typesafe.ErrResponseTooLarge):
		return msgResponseTooLarge
	case errors.Is(err, typesafe.ErrInvalidResponse):
		return msgResponseInvalid
	case errors.Is(err, typesafe.ErrNetwork):
		return msgNetwork
	case errors.Is(err, typesafe.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		return msgEvaluationTimeout
	case errors.Is(err, typesafe.ErrInvalidRequest), errors.Is(err, typesafe.ErrRequestTooLarge):
		return msgRequestInvalid
	case errors.Is(err, typesafe.ErrAPI):
		// Last: the statuses with their own sentinel are also API errors.
		return msgServiceError
	default:
		return msgEvaluationFailed
	}
}
