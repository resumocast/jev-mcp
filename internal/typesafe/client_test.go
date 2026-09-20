package typesafe

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// jsonHandler replies with a JSON body and counts the attempts it saw.
func jsonHandler(calls *atomic.Int32, status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		io.WriteString(w, body)
	}
}

// The endpoint is a constant, not configuration. This is the check that a
// future refactor has not quietly introduced a way to point the client
// somewhere else.
func TestNewClientUsesTheOfficialEndpointOnly(t *testing.T) {
	c := NewClient(fakeKey)
	if c.endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("endpoint = %q", c.endpoint)
	}
	if c.http.Timeout != RequestTimeout {
		t.Errorf("timeout = %v, want %v", c.http.Timeout, RequestTimeout)
	}
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T", c.http.Transport)
	}
	// An inherited HTTPS_PROXY must not be able to route the bearer token
	// through a host nobody chose.
	if transport.Proxy != nil {
		t.Error("transport honours ambient proxy settings")
	}
}

func TestNewClientRejectsUnusableKeys(t *testing.T) {
	for name, key := range map[string]string{
		"empty":            "",
		"newline injected": "abc\r\nX-Evil: 1",
		"with space":       "abc def",
		"non-ASCII":        "naïve-token",
		"too long":         repeat(maxKeyBytes + 1),
	} {
		t.Run(name, func(t *testing.T) {
			c := NewClient(key)
			if c.keyOK {
				t.Fatal("key should have been rejected")
			}
			_, err := c.Evaluate(context.Background(), sampleRequest(t))
			if !errors.Is(err, ErrCredential) {
				t.Fatalf("want ErrCredential, got %v", err)
			}
		})
	}
}

func TestEvaluateSendsTheExpectedRequest(t *testing.T) {
	var seen struct {
		method string
		auth   string
		ctype  string
		accept string
		body   []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.method = r.Method
		seen.auth = r.Header.Get("Authorization")
		seen.ctype = r.Header.Get("Content-Type")
		seen.accept = r.Header.Get("Accept")
		seen.body, _ = io.ReadAll(io.LimitReader(r.Body, 1<<20))
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	resp, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t))
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(resp.Answers) != 3 {
		t.Errorf("got %d answers", len(resp.Answers))
	}
	if seen.method != http.MethodPost {
		t.Errorf("method = %s", seen.method)
	}
	if seen.auth != "Bearer "+fakeKey {
		t.Errorf("authorization header = %q", seen.auth)
	}
	if seen.ctype != "application/json" || seen.accept != "application/json" {
		t.Errorf("content-type = %q, accept = %q", seen.ctype, seen.accept)
	}

	var body struct {
		Model     string          `json:"model"`
		State     json.RawMessage `json:"state"`
		Questions map[string]struct {
			Type string `json:"type"`
		} `json:"questions"`
	}
	if err := json.Unmarshal(seen.body, &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if body.Model != Model {
		t.Errorf("model = %q, want %q", body.Model, Model)
	}
	if len(body.Questions) != 3 {
		t.Errorf("sent %d questions", len(body.Questions))
	}
}

func TestEvaluateValidatesBeforeSending(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(jsonHandler(&calls, 200, sampleResponse))
	defer srv.Close()

	bad := Request{State: raw(`42`), Questions: map[string]Question{"q": noulQuestion(t)}}
	if _, err := newTestClient(t, srv).Evaluate(context.Background(), bad); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("an invalid request reached the network %d times", n)
	}
}

func TestEvaluateRetriesOnlyRateLimitAndOverload(t *testing.T) {
	cases := []struct {
		name   string
		status int
		want   error
		calls  int32
	}{
		{"rate limited", http.StatusTooManyRequests, ErrRateLimited, MaxRetries + 1},
		{"overloaded", statusOverloaded, ErrOverloaded, MaxRetries + 1},
		{"unauthorized", http.StatusUnauthorized, ErrUnauthorized, 1},
		{"forbidden", http.StatusForbidden, ErrUnauthorized, 1},
		{"unprocessable", http.StatusUnprocessableEntity, ErrRequestRejected, 1},
		{"server error", http.StatusInternalServerError, ErrAPI, 1},
		{"bad gateway", http.StatusBadGateway, ErrAPI, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(jsonHandler(&calls, tc.status, `{"error":"`+leakMarker+`"}`))
			defer srv.Close()

			_, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t))
			if !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
			if got := calls.Load(); got != tc.calls {
				t.Errorf("made %d attempts, want %d", got, tc.calls)
			}
			// The API's error body is never relayed: it ends up in an MCP
			// tool result that a model reads.
			assertNoLeak(t, err, leakMarker)
		})
	}
}

func TestEvaluateSucceedsAfterARetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t)); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("made %d attempts, want 2", got)
	}
}

// A POST that failed at the network layer is never replayed: a request that
// arrived, was billed, and lost its reply looks exactly like one that never
// arrived, and retrying turns a lost answer into a second charge.
func TestEvaluateDoesNotRetryNetworkFailures(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("response writer is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		conn.Close()
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t))
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("want ErrNetwork, got %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("made %d attempts, want exactly 1", got)
	}
}

func TestEvaluateRejectsUnreachableService(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()

	c := NewClient(fakeKey)
	c.endpoint = url
	c.backoff = time.Millisecond

	_, err := c.Evaluate(context.Background(), sampleRequest(t))
	if !errors.Is(err, ErrNetwork) {
		t.Fatalf("want ErrNetwork, got %v", err)
	}
	assertNoLeak(t, err, url)
}

// A redirect would replay the Authorization header at an address the service
// chose. It is refused, and the target is never contacted.
func TestEvaluateRefusesRedirects(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, sampleResponse)
	}))
	defer target.Close()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t))
	if !errors.Is(err, ErrAPI) {
		t.Fatalf("want ErrAPI, got %v", err)
	}
	if n := targetHits.Load(); n != 0 {
		t.Fatalf("redirect target was contacted %d times", n)
	}
	assertNoLeak(t, err, target.URL)
}

func TestEvaluateRejectsOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"padding":"`)
		io.WriteString(w, strings.Repeat("x", MaxResponseBytes+64))
		io.WriteString(w, `"}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t)); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

func TestEvaluateRejectsNonJSONSuccess(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		io.WriteString(w, "<html>"+leakMarker+"</html>")
	}))
	defer srv.Close()

	_, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t))
	if !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}
	assertNoLeak(t, err, leakMarker)
}

// Cancellation has to take effect while the HTTP request is in flight, not
// only between attempts: the MCP server cancels an evaluation when its client
// goes away, and a request that ignores that keeps a slot occupied.
func TestEvaluateCancelsAnInFlightRequest(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(release)

	// Built on the test goroutine: the helpers call t.Fatalf, which is only
	// legal there.
	c := newTestClient(t, srv)
	req := sampleRequest(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Evaluate(ctx, req)
		done <- err
	}()

	<-started
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Evaluate ignored cancellation")
	}
}

// Cancellation during a backoff wait must be just as prompt, or a 429 storm
// would pin the request for the full retry schedule.
func TestEvaluateCancelsDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(jsonHandler(&calls, http.StatusTooManyRequests, `{"error":"slow down"}`))
	defer srv.Close()

	c := newTestClient(t, srv)
	c.backoff = 10 * time.Second
	req := sampleRequest(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := c.Evaluate(ctx, req)
		done <- err
	}()

	// Wait for the first attempt to be answered, then cancel mid-backoff.
	deadline := time.After(5 * time.Second)
	for calls.Load() == 0 {
		select {
		case <-deadline:
			t.Fatal("no attempt was made")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want context.Canceled, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backoff was not cancellable")
	}
}

func TestEvaluateRejectsAnswersForOtherQuestions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"jev-1.13.0","answers":{"something_else":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()

	if _, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t)); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, time.October, 21, 7, 28, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"", 0},
		{"0", 0},
		{"2", 2 * time.Second},
		{" 3 ", 3 * time.Second},
		{"3600", time.Hour},
		{"-1", 0},
		{"soon", 0},
		// The HTTP-date form, relative to the clock passed in.
		{"Wed, 21 Oct 2026 07:28:30 GMT", 30 * time.Second},
		{"Wed, 21 Oct 2026 07:27:00 GMT", 0}, // already past
		// Both forms are bounded at the same ceiling.
		{"999999", maxRetryAfterSeconds * time.Second},
		{"Fri, 21 Oct 2039 07:28:00 GMT", maxRetryAfterSeconds * time.Second},
	}
	for _, tc := range cases {
		if got := parseRetryAfter(tc.in, now); got != tc.want {
			t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// A seconds value larger than an int64 can hold in nanoseconds must not wrap.
// Multiplying first would turn the longest possible request to back off into
// the shortest, and an overflowed negative duration into an immediate retry.
func TestParseRetryAfterDoesNotOverflow(t *testing.T) {
	now := time.Now()
	for _, in := range []string{
		"99999999999999999999",
		"9223372036854775807",
		"18446744073709551616",
	} {
		got := parseRetryAfter(in, now)
		if got != maxRetryAfterSeconds*time.Second {
			t.Errorf("parseRetryAfter(%q) = %v, want the ceiling %v", in, got, maxRetryAfterSeconds*time.Second)
		}
		if got <= 0 {
			t.Errorf("parseRetryAfter(%q) wrapped to %v", in, got)
		}
	}
	// A negative overflow is not a wait at all.
	if got := parseRetryAfter("-99999999999999999999", now); got != 0 {
		t.Errorf("negative overflow = %v, want 0", got)
	}
}

// Waiting less than the service asked for is the one thing a retry must not
// do. When the requested delay does not fit in what is left of the deadline,
// the call gives up rather than retrying early.
func TestEvaluateStopsWhenRetryAfterExceedsTheBudget(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "600")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":"slow down"}`)
	}))
	defer srv.Close()

	start := time.Now()
	_, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t))
	elapsed := time.Since(start)

	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("made %d attempts, want 1: a 10 minute wait does not fit in the deadline", n)
	}
	if elapsed > 5*time.Second {
		t.Errorf("gave up after %v; it should not have waited at all", elapsed)
	}
}

// A Retry-After that does fit is honoured in full rather than truncated to the
// client's own backoff.
func TestEvaluateHonoursAShortRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"error":"slow down"}`)
			return
		}
		io.WriteString(w, sampleResponse)
	}))
	defer srv.Close()

	start := time.Now()
	if _, err := newTestClient(t, srv).Evaluate(context.Background(), sampleRequest(t)); err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	elapsed := time.Since(start)

	if got := calls.Load(); got != 2 {
		t.Fatalf("made %d attempts, want 2", got)
	}
	// The test client's own backoff is a millisecond, so anything near a
	// second can only have come from the header.
	if elapsed < 900*time.Millisecond {
		t.Errorf("retried after %v, sooner than the second the service asked for", elapsed)
	}
}

func TestIsJSONContentType(t *testing.T) {
	good := []string{"application/json", "application/json; charset=utf-8", "APPLICATION/JSON", "application/problem+json"}
	bad := []string{"", "text/html", "text/plain; charset=utf-8", "application/jsonp", "application/octet-stream"}
	for _, v := range good {
		if !isJSONContentType(v) {
			t.Errorf("%q should be accepted", v)
		}
	}
	for _, v := range bad {
		if isJSONContentType(v) {
			t.Errorf("%q should be rejected", v)
		}
	}
}

// Client satisfies the interface the rest of the server depends on, so the MCP
// layer can be tested against a fake with no network and no credential.
func TestClientSatisfiesEvaluator(t *testing.T) {
	var e Evaluator = NewClient(fakeKey)
	if e == nil {
		t.Fatal("nil evaluator")
	}
}
