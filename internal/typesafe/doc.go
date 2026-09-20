// Package typesafe is a bounded client for the official TypeSafe evaluation
// endpoint, POST https://api.typesafe.ai/v1/systemone.
//
// The package does three things and nothing else: it validates an evaluation
// request, it reads a dedicated credential file, and it performs exactly one
// HTTP exchange (plus at most two retries) to obtain a validated response. It
// has no configuration surface. The endpoint is a compile-time constant, the
// model is pinned to [Model], and there is no code path that reads the
// environment, writes to stdout or stderr, or touches anything outside the
// credential file it is handed.
//
// # Shapes
//
// A [Request] carries a State and a map of [Question] values, mirroring the
// API reference at https://docs.typesafe.ai/api.md. State and question
// instructions are a JSON string, object, or array at the top level. Inside an
// object or array, ordinary JSON values are allowed: the documented state
// example at https://docs.typesafe.ai/concepts/state is a record carrying
// numbers alongside its text, and records are the recommended shape.
//
// # The deliberate subset
//
// Three things the API supports are intentionally out of scope for this
// version. Each is a narrowing, so anything this package accepts the API
// accepts, but not the reverse:
//
//   - Rubric descriptions are plain strings. The API reference gives criteria
//     as strings, and the primitive pages document a wider form where a score
//     level or a choice option may be an object or an array carrying examples.
//     That form is worth having and is not supported yet.
//   - A score question takes 2 to 10 levels, per the score primitive. There is
//     no way to ask for more.
//   - An omitted rubric is an absent field. A criteria value of null is an
//     error for every question type rather than a synonym for "omitted" on
//     noul, so the same input never means two things at different layers.
//
// # Text handling
//
// String contents in State and Instructions are not filtered. Real material
// includes log excerpts, diffs and terminal output, where a control character
// is data; it travels as a JSON escape and is never executed here. Making it
// safe to display belongs to whatever renders it, and doing it twice would
// corrupt the evidence the model is meant to read. Structural strings are
// treated differently: object member names, choice options, and rubric
// descriptions are labels that this package hands back to its caller, so they
// are held to a stricter rule.
//
// # Errors
//
// Every error returned by this package is one of the exported sentinels in
// errors.go, optionally wrapped with a fixed explanation. Error strings are
// built only from string literals in this package. Caller input, the
// credential, filesystem paths, and the API response body never reach an error
// string, because these errors are expected to end up in an MCP tool result
// that a model reads and a user sees. Use [errors.Is] against the sentinels to
// classify a failure; do not parse the text.
//
// Cancellation is the exception to sentinel-only errors: a request cancelled
// through its [context.Context] returns that context's own error, so callers
// can detect it with errors.Is(err, context.Canceled).
//
// # What a validated response guarantees
//
// A [Response] is rebuilt from checked values rather than forwarded. Its Model
// equals [Model] or the call failed. Its Answers cover exactly the question ids
// that were sent. Each answer's numbers are real JSON numbers in range, each
// distribution covers exactly the keys the question defined and sums to 1, a
// choice names the peak of its own distribution, a score agrees with the
// weighted mean of its distribution, and a score legend carries the level
// descriptions that were sent rather than any the service supplied. Members the
// package does not recognise are dropped.
//
// The numeric checks allow for the service rounding its decimals to two places,
// which is the precision its own documented examples carry. The tolerances are
// derived from that assumption in response.go and are stated there so they can
// be re-derived if the precision changes.
//
// # Limits
//
// [MaxRequestBytes], [MaxResponseBytes] and [MaxQuestions] bound memory and
// message size. They are not token limits and imply nothing about the model's
// context budget, which is counted in tokens. A request inside every limit here
// can still be refused by the service for exceeding that budget, which arrives
// as [ErrRequestRejected].
//
// # Provenance
//
// Derived from github.com/itsmostafa/typesafe-mcp at revision
// 0c9f35d9b1859189fc7e7d01947061f311ca6dde (MIT). See the NOTICE file at the
// repository root for what was taken, what was rewritten, and why.
package typesafe
