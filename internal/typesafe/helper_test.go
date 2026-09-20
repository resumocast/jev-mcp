package typesafe

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeKey is a sentinel, not a credential. No test in this package reads a real
// key, and no fixture in this repository contains one. It is shaped like a
// token only so that it passes safeToken.
const fakeKey = "jev-mcp-test-not-a-real-key"

// leakMarker is embedded in request fixtures that check error sanitation. If it
// ever shows up in an error string, something is quoting caller input.
const leakMarker = "CANARY-do-not-echo-this"

// newTestClient points a client at an httptest server.
//
// This helper lives in a _test.go file on purpose: it is the only way to change
// the endpoint, and it does not exist in a production build. Nothing else is
// replaced: the client keeps the transport NewClient built, so these tests
// exercise the real redirect policy, the real proxy setting and the real caps.
func newTestClient(t *testing.T, srv *httptest.Server) *Client {
	t.Helper()
	c := NewClient(fakeKey)
	c.endpoint = srv.URL
	c.backoff = time.Millisecond
	return c
}

// raw is a terser json.RawMessage literal.
func raw(s string) json.RawMessage { return json.RawMessage(s) }

// jsonString quotes s as a JSON string.
func jsonString(t *testing.T, s string) json.RawMessage {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		t.Fatalf("marshal string fixture: %v", err)
	}
	return bytes.TrimSpace(buf.Bytes())
}

// sampleRequest is one valid request covering all three question types.
func sampleRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		State: jsonString(t, "Help! My payouts have been failing for 3 days."),
		Questions: map[string]Question{
			"is_urgent": {
				Type:         TypeNoul,
				Instructions: jsonString(t, "Does this convey urgency?"),
				Criteria:     raw(`{"true":"Explicitly time-sensitive","false":"No urgency expressed"}`),
			},
			"department": {
				Type:         TypeChoice,
				Instructions: jsonString(t, "Which team should handle this?"),
				Criteria:     raw(`{"billing":"Payments","technical":"Bugs","sales":null}`),
			},
			"frustration": {
				Type:         TypeScore,
				Instructions: jsonString(t, "How frustrated is the customer?"),
				Criteria:     raw(`["Calm","Frustrated","Very angry"]`),
			},
		},
	}
}

// sampleResponse is a well-formed reply to sampleRequest.
const sampleResponse = `{
  "model": "jev-1.13.0",
  "answers": {
    "is_urgent": {"type": "noul", "noul": 0.92},
    "department": {
      "type": "choice",
      "choice": "technical",
      "probabilities": {"billing": 0.08, "technical": 0.85, "sales": 0.07},
      "confidence": 0.82
    },
    "frustration": {
      "type": "score",
      "score": 1.6,
      "legend": {"0": "Calm", "1": "Frustrated", "2": "Very angry"},
      "probabilities": {"0": 0.05, "1": 0.3, "2": 0.65},
      "confidence": 0.78
    }
  },
  "usage": {"input_tokens": 312, "output_tokens": 48}
}`

// specsFor runs validation to obtain the question specs a response is checked
// against, failing the test if the fixture request is itself invalid.
func specsFor(t *testing.T, req Request) map[string]questionSpec {
	t.Helper()
	specs, err := validateRequest(req)
	if err != nil {
		t.Fatalf("fixture request should be valid, got %v", err)
	}
	return specs
}

// assertNoLeak fails if an error string quotes anything it should not.
func assertNoLeak(t *testing.T, err error, forbidden ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	msg := err.Error()
	for _, f := range forbidden {
		if f != "" && strings.Contains(msg, f) {
			t.Errorf("error message leaked %q: %s", f, msg)
		}
	}
}

// repeat builds a string of n bytes.
func repeat(n int) string { return strings.Repeat("a", n) }
