package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"
	"unicode/utf8"
)

// JSON-RPC 2.0 framing for the MCP stdio transport: one JSON message per line,
// terminated by LF, with no embedded newlines. Every bound in this file exists
// so that a peer cannot make the server allocate memory it did not choose to
// allocate.

const (
	// jsonrpcVersion is the only accepted value of the "jsonrpc" member.
	jsonrpcVersion = "2.0"

	// maxFrameBytes caps one message in either direction, excluding the
	// terminating newline.
	maxFrameBytes = 128 << 10

	// maxDiscardBytes bounds how much of an oversized message the reader will
	// skip while looking for the newline that ends it. A peer that never sends
	// that newline is not a peer this server can resynchronise with.
	maxDiscardBytes = 8 << 20

	// maxIDBytes bounds the raw text of a request id. The id is echoed back
	// verbatim, so it is part of the response budget.
	maxIDBytes = 512

	// maxMethodBytes bounds the method name of an incoming message.
	maxMethodBytes = 128

	// readBufferBytes is the reader's working buffer. Messages larger than this
	// are assembled across several ReadSlice calls, up to maxFrameBytes.
	readBufferBytes = 64 << 10
)

// JSON-RPC 2.0 error codes, plus the "not initialized" code MCP servers use
// before the handshake completes.
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
	codeNotInitialized = -32002
)

var (
	// errFrameTooLarge reports an incoming message above maxFrameBytes whose
	// terminating newline was found, so the stream is still usable.
	errFrameTooLarge = errors.New("mcp: incoming message exceeds the size limit")

	// errUnsynchronized reports that the reader gave up looking for the end of
	// an oversized message. The connection cannot continue.
	errUnsynchronized = errors.New("mcp: cannot resynchronize with the input stream")

	// errOversizeOutgoing reports a message this server declined to send
	// because it would exceed maxFrameBytes.
	errOversizeOutgoing = errors.New("mcp: outgoing message exceeds the size limit")

	// errWriteStalled reports a write that did not complete within its budget
	// because the consumer stopped reading. The output stream is not usable
	// afterwards: the abandoned bytes may be partly written.
	errWriteStalled = errors.New("mcp: the output stream stopped accepting messages")

	// errWriteAborted reports a write given up because the input stream had
	// already ended, which means nothing is left to read the answer.
	errWriteAborted = errors.New("mcp: the output stream was abandoned after the input ended")
)

// incoming is the union of the message shapes a peer may send us. The server
// only ever acts on requests and notifications; Result and Error exist so that
// a stray response can be recognised and ignored rather than answered.
type incoming struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// response is a JSON-RPC response. Exactly one of Result and Error is set; ID
// is null only when the request it answers had no usable id.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError carries a code and a message built from this package's string
// literals. There is deliberately no Data member: everything this server knows
// about a failure that a peer may safely see fits in Message.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// requestID is a validated JSON-RPC id: a string or an integer, never null,
// never a fractional number, never a structure.
//
// raw is echoed back byte for byte, because a peer matching responses to
// requests compares the id it sent. key is a canonical form used to detect a
// second request that reuses an id still in flight; the "s:"/"n:" prefixes keep
// the string id "1" distinct from the number id 1.
type requestID struct {
	raw json.RawMessage
	key string
}

// parseID validates the id member of a request. A missing id means the message
// is a notification and never reaches here.
func parseID(raw json.RawMessage) (requestID, bool) {
	text := string(bytes.TrimSpace(raw))
	if text == "" || text == "null" || len(text) > maxIDBytes || !utf8.ValidString(text) {
		return requestID{}, false
	}
	if text[0] == '"' {
		var s string
		if err := json.Unmarshal([]byte(text), &s); err != nil {
			return requestID{}, false
		}
		return requestID{raw: append(json.RawMessage(nil), text...), key: "s:" + s}, true
	}
	if !integerText(text) {
		return requestID{}, false
	}
	return requestID{raw: append(json.RawMessage(nil), text...), key: "n:" + text}, true
}

// integerText reports whether s is a JSON number with no fraction or exponent.
// JSON-RPC says ids should not carry fractional parts, and an id that only
// round-trips through a float is an id this server cannot echo faithfully.
//
// Negative zero is refused rather than folded into zero. Canonicalising it
// would make two different texts one id, and two ids in flight that this
// server cannot tell apart is the situation the in-flight check exists to
// prevent; refusing it keeps one text for one id.
func integerText(s string) bool {
	if len(s) == 0 || len(s) > 20 || s == "-0" {
		return false
	}
	i := 0
	if s[0] == '-' {
		i++
	}
	if i >= len(s) {
		return false
	}
	if s[i] == '0' {
		// Only "0" and "-0"; JSON forbids leading zeros anyway, and rejecting
		// them here keeps key canonical.
		return i == len(s)-1
	}
	for ; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// frameReader reads LF-delimited messages with a hard size limit.
type frameReader struct {
	br *bufio.Reader
}

func newFrameReader(r io.Reader) *frameReader {
	return &frameReader{br: bufio.NewReaderSize(r, readBufferBytes)}
}

// readFrame returns the next non-empty message without its terminator.
//
// A message longer than maxFrameBytes is discarded up to its newline and
// reported as errFrameTooLarge: the caller can answer with an error and keep
// serving. Bytes left in the buffer when the stream ends are not a message and
// are dropped with the io.EOF.
func (r *frameReader) readFrame() ([]byte, error) {
	for {
		var (
			line      []byte
			oversize  bool
			discarded int
		)
		for {
			chunk, err := r.br.ReadSlice('\n')
			if err != nil && !errors.Is(err, bufio.ErrBufferFull) {
				return nil, err
			}
			// The delimiter is transport framing, not part of the payload cap.
			if err == nil {
				chunk = chunk[:len(chunk)-1]
			}
			switch {
			case oversize:
				discarded += len(chunk)
				if discarded > maxDiscardBytes {
					return nil, errUnsynchronized
				}
			case len(line)+len(chunk) > maxFrameBytes:
				oversize = true
				discarded = len(line) + len(chunk)
				line = nil
			default:
				// ReadSlice returns a view into the reader's buffer; append
				// copies it before the next read invalidates it.
				line = append(line, chunk...)
			}
			if err == nil {
				break
			}
		}
		if oversize {
			return nil, errFrameTooLarge
		}
		line = bytes.TrimRight(line, "\r\n")
		if len(bytes.TrimSpace(line)) == 0 {
			// Blank lines are not messages. Some clients use them as keepalive.
			continue
		}
		return line, nil
	}
}

// frameWriter owns the output stream.
//
// The write itself runs on one goroutine of the writer's own, created once and
// never per message. A consumer that stops reading therefore blocks that
// goroutine rather than the serve loop, which keeps its ability to notice a
// cancelled context, an input stream that ended, and an evaluation that
// finished. Each message is written with a single Write call, so no
// interleaving can split a line, and at most one write is ever in flight.
//
// Giving up on a write is final. The bytes handed to a blocked Write may be
// partly on the stream, so anything sent afterwards could resume in the middle
// of an abandoned message; the writer refuses every later message instead.
type frameWriter struct {
	mu      sync.Mutex
	stalled bool

	reqs    chan []byte
	result  chan error
	quit    chan struct{}
	abort   <-chan struct{}
	timeout time.Duration
}

// newFrameWriter starts the writing goroutine. A write ends early when ctx is
// cancelled, shortly after abort is closed, or after timeout. EOF permits a
// 100ms flush grace: a client may half-close stdin while still reading stdout.
// A nil abort channel simply never fires.
func newFrameWriter(out io.Writer, abort <-chan struct{}, timeout time.Duration) *frameWriter {
	if timeout <= 0 {
		// A zero budget would expire before the first message, so treat it as
		// "unset" rather than as "never write anything".
		timeout = defaultWriteTimeout
	}
	w := &frameWriter{
		reqs:    make(chan []byte),
		result:  make(chan error, 1),
		quit:    make(chan struct{}),
		abort:   abort,
		timeout: timeout,
	}
	go w.run(out)
	return w
}

func (w *frameWriter) run(out io.Writer) {
	for {
		select {
		case b := <-w.reqs:
			_, err := out.Write(b)
			// The channel holds one value and a request is only ever sent when
			// no result is outstanding, so this cannot block, including when
			// the caller has already given up on the write.
			w.result <- err
		case <-w.quit:
			return
		}
	}
}

// close releases the writing goroutine. A goroutine blocked in a write that
// will never complete stays blocked: there is no portable way to interrupt a
// write to a pipe, and the process is on its way out.
func (w *frameWriter) close() {
	close(w.quit)
}

// write marshals v and appends the newline terminator. json.Marshal escapes
// newlines inside strings, so the encoded message can never contain the
// delimiter. A message that would exceed maxFrameBytes is not written at all;
// the caller decides what smaller message to send instead.
func (w *frameWriter) write(ctx context.Context, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxFrameBytes {
		return errOversizeOutgoing
	}
	b = append(b, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stalled {
		return errWriteStalled
	}

	deadline := time.Now().Add(w.timeout)
	timer := time.NewTimer(w.timeout)
	defer timer.Stop()
	abort := w.abort
	requests := w.reqs
	var result <-chan error
	inputEnded := false

	// One budget covers handoff and writing. EOF shortens that budget without
	// randomly dropping an immediately writable final response. Context
	// cancellation remains immediate, and a blocked consumer cannot extend it.
	for {
		select {
		case requests <- b:
			requests = nil
			result = w.result
		case err := <-result:
			return err
		case <-ctx.Done():
			w.stalled = true
			return ctx.Err()
		case <-abort:
			abort = nil
			inputEnded = true
			grace := min(100*time.Millisecond, time.Until(deadline))
			timer.Reset(grace)
		case <-timer.C:
			w.stalled = true
			if inputEnded {
				return errWriteAborted
			}
			return errWriteStalled
		}
	}
}
