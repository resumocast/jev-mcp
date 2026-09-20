package typesafe

import "time"

// Model is the pinned evaluation model. The roadmap pins a version rather than
// an alias so that an upstream release cannot silently change answers, and the
// confidence thresholds callers tune stay attached to one set of weights.
//
// The pin is enforced in both directions: it is the only value ever sent, and a
// response that reports a different model is rejected. See [Response].
const Model = "jev-1.13.0"

// endpoint is the only address this package will talk to. It is a constant, not
// a field: there is deliberately no production code path that can point the
// client somewhere else. Tests substitute an httptest address through an
// unexported helper that only exists in the test build.
const endpoint = "https://api.typesafe.ai/v1/systemone"

// Bounds a caller can rely on.
//
// These are resource limits, in bytes and counts. They are not token limits and
// say nothing about the model's context budget, which is measured in tokens and
// documented separately at https://docs.typesafe.ai/models.md. A request well
// inside MaxRequestBytes can still be refused by the service for exceeding its
// token budget; that arrives as an HTTP 422 and surfaces as
// [ErrRequestRejected].
const (
	// MaxRequestBytes bounds the marshaled request body.
	MaxRequestBytes = 65536

	// MaxResponseBytes bounds the response body that will be read. Anything
	// larger is an error, not a truncation.
	MaxResponseBytes = 131072

	// MaxQuestions bounds how many questions one request may carry.
	MaxQuestions = 16
)

// Per-field bounds. These are unexported: they are implementation detail of
// what "a reasonable evaluation" looks like, and tightening one should not be a
// breaking change for callers.
const (
	maxStateBytes        = 32768
	maxInstructionsBytes = 8192
	maxCriteriaBytes     = 8192
	maxQuestionIDBytes   = 64
	maxOptionBytes       = 128
	maxMemberNameBytes   = 256
	maxDescriptionBytes  = 1024

	// maxJSONDepth bounds nesting in state and instructions. Eight levels is
	// far past anything a useful judgment needs and keeps the scanner's work
	// linear in the input.
	maxJSONDepth     = 8
	maxObjectMembers = 256
	maxArrayElements = 512

	// maxStringBytes matches maxStateBytes so that a single string can fill
	// the state field. The enclosing field's own byte cap is always the
	// tighter of the two for instructions and criteria.
	maxStringBytes = maxStateBytes

	minChoiceOptions = 1

	// maxChoiceOptions is the API's documented ceiling on the answer space of
	// a choice question.
	maxChoiceOptions = 255

	// Score levels are 2 to 10, per https://docs.typesafe.ai/primitives/score:
	// "Needs at least two levels and takes up to 10."
	minScoreLevels = 2
	maxScoreLevels = 10

	// maxTokenCount bounds a usage counter. Real counts are in the thousands;
	// this only exists so a malformed reply cannot report an absurd number.
	maxTokenCount = 1 << 40
)

// HTTP behaviour.
const (
	// RequestTimeout is the deadline for a whole Evaluate call: connection,
	// request, every retry, every backoff wait, and the response read. It is
	// also the effective ceiling on any Retry-After the service asks for.
	RequestTimeout = 30 * time.Second

	// MaxRetries is the number of retries after the first attempt, so at most
	// three requests reach the network. Only 429 and 529 are retried; a POST
	// that failed at the network layer is never retried, because there is no
	// way to tell a request that never arrived from one that arrived, was
	// billed, and lost its reply.
	MaxRetries = 2

	// statusOverloaded is TypeSafe's 529 Overloaded. Go's net/http has no
	// constant for it.
	statusOverloaded = 529

	defaultBackoff = 500 * time.Millisecond
	maxBackoff     = 4 * time.Second

	// maxRetryAfterSeconds bounds a parsed Retry-After before it is multiplied
	// by time.Second. Without the guard, a header of "99999999999999999999"
	// would overflow an int64 nanosecond count and wrap to a negative or tiny
	// duration. The value itself is generous; the remaining request budget is
	// what actually decides whether a wait is honoured.
	maxRetryAfterSeconds = 86400

	dialTimeout           = 10 * time.Second
	tlsHandshakeTimeout   = 10 * time.Second
	responseHeaderTimeout = 25 * time.Second
	maxResponseHeaders    = 16 << 10

	userAgent = "jev-mcp"
)

// Credential file bounds.
const (
	maxKeyBytes = 256
	minKeyBytes = 8
)
