package typesafe

import "errors"

// The complete set of failures this package reports. Classify with errors.Is;
// the text is documentation, not an interface.
//
// These messages are read by a model and shown to a user, so they name what
// went wrong without quoting anything: no prompt text, no credential, no
// filesystem path, no API response body.
var (
	// ErrInvalidRequest means the request did not satisfy the shape and bounds
	// this package enforces. The wrapped explanation names the rule that was
	// broken, never the offending value.
	ErrInvalidRequest = errors.New("typesafe: invalid evaluation request")

	// ErrRequestTooLarge means the marshaled request exceeded MaxRequestBytes.
	ErrRequestTooLarge = errors.New("typesafe: evaluation request is too large")

	// ErrCredential means the client has no usable API key.
	ErrCredential = errors.New("typesafe: no usable API credential")

	// ErrKeyFile means the credential file could not be read, or failed its
	// ownership, permission, file-type or content checks.
	ErrKeyFile = errors.New("typesafe: credential file is not usable")

	// ErrUnauthorized means the service rejected the credential.
	ErrUnauthorized = errors.New("typesafe: the evaluation service rejected the credential")

	// ErrRequestRejected means the service refused the request body itself
	// (HTTP 422). The body that says which field was at fault is not relayed,
	// so the message names the three things that are actually worth checking.
	ErrRequestRejected = errors.New("typesafe: the evaluation service rejected the request: check the state, the question rubrics, and whether the request exceeds the model's token budget (which is separate from this client's byte limits)")

	// ErrRateLimited means the service returned 429 and no further attempt was
	// allowed or affordable within the request deadline.
	ErrRateLimited = errors.New("typesafe: the evaluation service is rate limiting this client")

	// ErrOverloaded means the service returned 529 under the same conditions.
	ErrOverloaded = errors.New("typesafe: the evaluation service is temporarily overloaded")

	// ErrAPI means the service returned some other unsuccessful status. The
	// numeric status is included; the response body never is.
	ErrAPI = errors.New("typesafe: the evaluation service returned an error")

	// ErrResponseTooLarge means the response body exceeded MaxResponseBytes.
	ErrResponseTooLarge = errors.New("typesafe: evaluation response is too large")

	// ErrInvalidResponse means the response was not a well-formed, complete,
	// internally consistent answer to the request that was sent.
	ErrInvalidResponse = errors.New("typesafe: evaluation response is not valid")

	// ErrNetwork means the exchange failed below HTTP: DNS, connection, TLS, or
	// a truncated body. The underlying error is deliberately discarded rather
	// than wrapped, because its text is assembled from addresses and system
	// messages this package does not control.
	ErrNetwork = errors.New("typesafe: could not reach the evaluation service")

	// ErrTimeout means the call exhausted RequestTimeout. A cancellation that
	// came from the caller's own context returns that context's error instead.
	ErrTimeout = errors.New("typesafe: evaluation timed out")
)

// detailError is a sentinel plus a fixed explanation. Keeping the two apart
// rather than formatting them together lets wrapField add the field name to the
// explanation without the sentinel's own text landing in the middle of the
// sentence.
type detailError struct {
	sentinel error
	why      string
}

func (e detailError) Error() string { return e.sentinel.Error() + ": " + e.why }
func (e detailError) Unwrap() error { return e.sentinel }

// invalidRequest wraps ErrInvalidRequest with a fixed explanation.
//
// Every call site passes a string literal. Nothing derived from caller input
// is ever formatted into an error in this package, so an error can be logged or
// handed back to a model without reviewing what it might have captured.
func invalidRequest(why string) error {
	return detailError{sentinel: ErrInvalidRequest, why: why}
}

// invalidResponse wraps ErrInvalidResponse with a fixed explanation. Note in
// particular that errors from encoding/json are never wrapped: their text
// quotes the offending input.
func invalidResponse(why string) error {
	return detailError{sentinel: ErrInvalidResponse, why: why}
}

// badKeyFile wraps ErrKeyFile with a fixed explanation. The path is never
// included: the credential file's location is the one piece of filesystem
// layout worth not printing into a session transcript.
func badKeyFile(why string) error {
	return detailError{sentinel: ErrKeyFile, why: why}
}

// wrapField names the field an error came from. Every field name passed in is a
// string literal in this package, so the result still contains nothing derived
// from the request.
func wrapField(field string, err error) error {
	var de detailError
	if errors.As(err, &de) {
		return detailError{sentinel: de.sentinel, why: field + ": " + de.why}
	}
	return err
}
