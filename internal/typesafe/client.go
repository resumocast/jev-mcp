package typesafe

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Client evaluates requests against the official TypeSafe endpoint.
//
// A Client has no exported configuration. The endpoint, the model, the
// timeout, the retry policy and the size caps are all fixed, so there is no
// call that can point it at another service or relax a bound. It is safe for
// concurrent use.
type Client struct {
	key     string
	keyOK   bool
	http    *http.Client
	backoff time.Duration

	// endpoint is unexported and set once by NewClient. Tests replace it
	// through a helper that exists only in the test build; no production code
	// path writes it.
	endpoint string
}

// NewClient returns a client that authenticates with key, which should come
// from [ReadKey].
//
// A key that is not a printable ASCII token is not stored, and every Evaluate
// call on the resulting client fails with [ErrCredential]. Rejecting it here
// rather than at request time keeps a malformed value out of an Authorization
// header, where a stray newline would be header injection.
func NewClient(key string) *Client {
	c := &Client{
		endpoint: endpoint,
		backoff:  defaultBackoff,
		http:     newHTTPClient(),
	}
	if safeToken(key) && len(key) <= maxKeyBytes {
		c.key = key
		c.keyOK = true
	}
	return c
}

// newHTTPClient builds the only transport this package uses.
//
// Proxy is nil rather than http.ProxyFromEnvironment: an HTTPS_PROXY inherited
// from whatever launched the MCP server would silently route evaluations, and
// the bearer token with them, through a host nobody chose.
//
// CheckRedirect returns ErrUseLastResponse so a redirect is handed back as a
// 3xx to be rejected, rather than followed. Following one would replay the
// Authorization header at an address the service picked; returning an error
// from CheckRedirect instead would put that address into the error string.
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: RequestTimeout,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   dialTimeout,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:      true,
			TLSClientConfig:        &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:    tlsHandshakeTimeout,
			ResponseHeaderTimeout:  responseHeaderTimeout,
			ExpectContinueTimeout:  time.Second,
			MaxIdleConns:           4,
			MaxIdleConnsPerHost:    2,
			IdleConnTimeout:        30 * time.Second,
			MaxResponseHeaderBytes: maxResponseHeaders,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Evaluate validates req, sends it, and returns the validated answers.
//
// The whole call is bounded by [RequestTimeout], covering the connection, every
// attempt, every backoff wait and the response read. Cancelling ctx aborts an
// in-flight request; that case returns ctx's own error so a caller can tell
// cancellation from failure with errors.Is.
//
// Only 429 and 529 are retried, at most [MaxRetries] times. A POST that failed
// at the network layer is never retried: a request that reached the service,
// was billed, and lost its response on the way back is indistinguishable from
// one that never arrived, and retrying turns a lost answer into a second
// charge.
func (c *Client) Evaluate(ctx context.Context, req Request) (Response, error) {
	var zero Response
	if c == nil || !c.keyOK {
		return zero, ErrCredential
	}

	specs, err := validateRequest(req)
	if err != nil {
		return zero, err
	}
	body, err := encodeRequest(req)
	if err != nil {
		return zero, err
	}

	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	delay := c.backoff
	for attempt := 0; ; attempt++ {
		res, err := c.post(ctx, parent, body)
		if err != nil {
			return zero, err
		}

		switch {
		case res.status >= 200 && res.status < 300:
			if !isJSONContentType(res.contentType) {
				return zero, invalidResponse("response was not JSON")
			}
			return parseResponse(res.body, specs)

		case res.status == http.StatusTooManyRequests, res.status == statusOverloaded:
			if attempt >= MaxRetries {
				return zero, exhausted(res.status)
			}

		case res.status == http.StatusUnauthorized, res.status == http.StatusForbidden:
			return zero, ErrUnauthorized

		case res.status == http.StatusUnprocessableEntity:
			// The body names the offending field, but it is service-controlled
			// text and is not relayed. The sentinel's message lists what is
			// worth checking instead.
			return zero, ErrRequestRejected

		case res.status >= 300 && res.status < 400:
			// Not followed: see newHTTPClient.
			return zero, fmt.Errorf("%w: the service tried to redirect the request", ErrAPI)

		default:
			// The status code is the only thing from the exchange that reaches
			// the caller. The body is not included, and neither is the
			// server-supplied status text.
			return zero, fmt.Errorf("%w (status %d)", ErrAPI, res.status)
		}

		// Our own backoff is capped; a Retry-After the service asked for is
		// not. Waiting less than the service asked for is the one thing a
		// retry must not do, so when the requested delay does not fit in what
		// is left of the deadline, the call gives up instead of retrying
		// early. RequestTimeout is therefore the effective ceiling, and it is
		// enforced by stopping rather than by truncating the wait.
		wait := max(delay, res.retryAfter)
		if deadline, ok := ctx.Deadline(); ok && wait >= time.Until(deadline) {
			return zero, exhausted(res.status)
		}
		if err := sleep(ctx, parent, wait); err != nil {
			return zero, err
		}
		delay = min(delay*2, maxBackoff)
	}
}

// exhausted names the reason a retryable status was given up on.
func exhausted(status int) error {
	if status == http.StatusTooManyRequests {
		return ErrRateLimited
	}
	return ErrOverloaded
}

// exchange is one completed HTTP round trip.
type exchange struct {
	status      int
	contentType string
	retryAfter  time.Duration
	body        []byte
}

func (c *Client) post(ctx, parent context.Context, body []byte) (*exchange, error) {
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, ErrNetwork
	}
	r.ContentLength = int64(len(body))
	r.Header.Set("Authorization", "Bearer "+c.key)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json")
	r.Header.Set("User-Agent", userAgent)

	resp, err := c.http.Do(r)
	if err != nil {
		// url.Error's text is assembled from the address, the proxy and the
		// system's message. None of it is needed to act on the failure, so the
		// error is classified and dropped rather than wrapped.
		return nil, networkErr(ctx, parent)
	}
	defer resp.Body.Close()

	// One byte past the cap, so an oversized body is an error instead of a
	// truncated prefix that might still parse.
	payload, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes+1))
	if err != nil {
		return nil, networkErr(ctx, parent)
	}
	if len(payload) > MaxResponseBytes {
		return nil, ErrResponseTooLarge
	}

	return &exchange{
		status:      resp.StatusCode,
		contentType: resp.Header.Get("Content-Type"),
		retryAfter:  parseRetryAfter(resp.Header.Get("Retry-After"), time.Now()),
		body:        payload,
	}, nil
}

// networkErr distinguishes the three ways an exchange can stop: the caller
// cancelled, the package's own deadline expired, or the network failed.
func networkErr(ctx, parent context.Context) error {
	if err := parent.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ErrTimeout
	}
	return ErrNetwork
}

// sleep waits for d, or stops early if the call is cancelled or out of time.
// The backoff is part of the total deadline, not extra time on top of it.
func sleep(ctx, parent context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return networkErr(ctx, parent)
	case <-t.C:
		return nil
	}
}

// parseRetryAfter reads both documented forms of Retry-After: delay-seconds,
// and an HTTP-date to wait until.
//
// The seconds value is bounded before it is multiplied by time.Second. A header
// of "99999999999999999999" would otherwise overflow an int64 nanosecond count
// and wrap into a negative or near-zero duration, turning the longest possible
// request to back off into the shortest. An out-of-range value is treated as
// the maximum rather than as unparseable, so it still leads to giving up rather
// than to an immediate retry.
//
// The date form is honoured, which does mean trusting the service's clock
// against ours. A skewed clock can only make the wait too long or too short by
// the skew; the caller's deadline still bounds the whole call either way.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}

	secs, err := strconv.ParseInt(v, 10, 64)
	if err == nil || errors.Is(err, strconv.ErrRange) {
		if secs <= 0 {
			return 0
		}
		if secs > maxRetryAfterSeconds {
			return maxRetryAfterSeconds * time.Second
		}
		return time.Duration(secs) * time.Second
	}

	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	d := t.Sub(now)
	if d <= 0 {
		return 0
	}
	if d > maxRetryAfterSeconds*time.Second {
		return maxRetryAfterSeconds * time.Second
	}
	return d
}

func isJSONContentType(v string) bool {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "application/json" || strings.HasSuffix(v, "+json")
}

// Evaluator is the behaviour the rest of the server depends on, so that it can
// be exercised against a fake without a network or a credential.
type Evaluator interface {
	Evaluate(ctx context.Context, req Request) (Response, error)
}

var _ Evaluator = (*Client)(nil)
