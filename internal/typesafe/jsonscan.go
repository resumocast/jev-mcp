package typesafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// structuralError is a violation of the JSON shape rules that scanJSON
// enforces. It carries only a fixed reason string and no sentinel of its own,
// because the same scan guards both the request being sent and the response
// coming back, and those failures are different kinds of problem. Callers
// convert it with [asRequestError] or [asResponseError].
type structuralError struct{ why string }

func (e structuralError) Error() string { return e.why }

func structural(why string) error { return structuralError{why: why} }

// asRequestError attributes a structural failure to the caller's request.
func asRequestError(err error) error {
	var se structuralError
	if errors.As(err, &se) {
		return invalidRequest(se.why)
	}
	return err
}

// asResponseError attributes a structural failure to the service's reply.
func asResponseError(err error) error {
	var se structuralError
	if errors.As(err, &se) {
		return invalidResponse(se.why)
	}
	return err
}

// topKind is the kind of the top-level value in a scanned document.
type topKind int

const (
	topInvalid topKind = iota
	topString
	topObject
	topArray
	topScalar // number, boolean or null
)

// scanLimits bounds one JSON value.
type scanLimits struct {
	maxBytes int
	maxDepth int
}

// scanJSON walks a JSON value iteratively, enforcing structural limits, and
// reports the kind of its top-level value.
//
// Depth is bounded explicitly rather than by relying on the goroutine stack, so
// a deeply nested value is an error rather than a crash. Duplicate object
// member names are rejected outright: two members with one name mean the
// caller, this package and the service can each pick a different value, and the
// raw bytes are available here to catch it before any of that happens.
//
// String *contents* are not restricted beyond length and UTF-8 validity. State
// and instructions carry real material such as log excerpts and diffs, where a
// control character is data; it travels as a JSON escape, is never executed,
// and sanitising it for display is the renderer's job, not this package's.
// Member *names* are held to a stricter rule, because they are structure.
func scanJSON(raw []byte, lim scanLimits) (topKind, error) {
	if len(raw) == 0 {
		return topInvalid, structural("value is missing")
	}
	if len(raw) > lim.maxBytes {
		return topInvalid, structural("value is longer than this field allows")
	}
	if !utf8.Valid(raw) {
		return topInvalid, structural("value is not valid UTF-8")
	}
	// json.Valid rejects malformed input and trailing data up front, so the
	// token walk below only has to enforce limits.
	if !json.Valid(raw) {
		return topInvalid, structural("value is not well-formed JSON")
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()

	type frame struct {
		object bool
		// wantKey is true when the next token in an object frame is a member
		// name rather than a value.
		wantKey bool
		count   int
		seen    map[string]struct{}
	}
	var stack []*frame
	kind := topInvalid
	done := false

	// closeValue records that a complete value has just been consumed, and
	// attributes it to the container it belonged to.
	closeValue := func() error {
		if n := len(stack); n > 0 {
			f := stack[n-1]
			if f.object {
				f.wantKey = true
				return nil
			}
			f.count++
			if f.count > maxArrayElements {
				return structural("an array has too many elements")
			}
			return nil
		}
		done = true
		return nil
	}

	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			// Never wrapped: encoding/json error text quotes the input.
			return topInvalid, structural("value is not well-formed JSON")
		}
		if done {
			return topInvalid, structural("value has trailing content")
		}

		// Object member names are handled before value dispatch, so a name is
		// never mistaken for a string value or vice versa.
		if n := len(stack); n > 0 && stack[n-1].object && stack[n-1].wantKey {
			f := stack[n-1]
			if d, ok := tok.(json.Delim); ok && d == '}' {
				stack = stack[:n-1]
				if err := closeValue(); err != nil {
					return topInvalid, err
				}
				continue
			}
			name, ok := tok.(string)
			if !ok {
				return topInvalid, structural("value is not well-formed JSON")
			}
			if name == "" || len(name) > maxMemberNameBytes {
				return topInvalid, structural("an object member name is empty or too long")
			}
			if !safeLabel(name) {
				return topInvalid, structural("an object member name contains control characters")
			}
			if _, dup := f.seen[name]; dup {
				return topInvalid, structural("an object has a duplicate member name")
			}
			f.seen[name] = struct{}{}
			f.count++
			if f.count > maxObjectMembers {
				return topInvalid, structural("an object has too many members")
			}
			f.wantKey = false
			continue
		}

		switch v := tok.(type) {
		case json.Delim:
			switch v {
			case '{', '[':
				if len(stack) >= lim.maxDepth {
					return topInvalid, structural("value is nested too deeply")
				}
				if kind == topInvalid {
					if v == '{' {
						kind = topObject
					} else {
						kind = topArray
					}
				}
				f := &frame{object: v == '{'}
				if f.object {
					f.wantKey = true
					f.seen = make(map[string]struct{})
				}
				stack = append(stack, f)
			default:
				// Objects always close in the member-name branch above, since
				// a frame expecting a name is exactly a frame that can close.
				// This is the array case, plus a guard json.Valid already
				// makes unreachable.
				if len(stack) == 0 {
					return topInvalid, structural("value is not well-formed JSON")
				}
				stack = stack[:len(stack)-1]
				if err := closeValue(); err != nil {
					return topInvalid, err
				}
			}
		case string:
			if len(v) > maxStringBytes {
				return topInvalid, structural("a string in this value is too long")
			}
			if kind == topInvalid {
				kind = topString
			}
			if err := closeValue(); err != nil {
				return topInvalid, err
			}
		default:
			// json.Number, bool, or nil. Structured state is records, and
			// records hold numbers, flags and absent fields: the official
			// state example at https://docs.typesafe.ai/concepts/state carries
			// {"amount_usd": 49}. Only the top level is restricted.
			if kind == topInvalid {
				kind = topScalar
			}
			if err := closeValue(); err != nil {
				return topInvalid, err
			}
		}
	}

	if len(stack) != 0 || kind == topInvalid {
		return topInvalid, structural("value is not well-formed JSON")
	}
	return kind, nil
}

// scanText checks a text-bearing field: state, or a question's instructions.
//
// The top level must be a string, an object, or an array, as the API documents
// for both fields. A bare number, boolean or null at the top level is refused:
// the field is the material the model reads, and 42 on its own is not material.
// Inside an object or array those scalars are ordinary record values and are
// allowed. A top-level string must have some non-space content, matching what
// the MCP layer enforces on the same field.
func scanText(raw []byte, maxBytes int) error {
	kind, err := scanJSON(raw, scanLimits{maxBytes: maxBytes, maxDepth: maxJSONDepth})
	if err != nil {
		return asRequestError(err)
	}
	switch kind {
	case topString:
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return invalidRequest("value is not well-formed JSON")
		}
		if strings.TrimSpace(s) == "" {
			return invalidRequest("value must not be empty")
		}
		return nil
	case topObject, topArray:
		return nil
	default:
		return invalidRequest("value must be a string, an object, or an array")
	}
}

// safeText reports whether s is free of control characters that have no place
// in a label this package hands back to a caller. Tab, newline and carriage
// return are kept: a rubric description can be several lines.
func safeText(s string) bool {
	for _, r := range s {
		switch r {
		case '\t', '\n', '\r':
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// safeLabel is safeText for identifier-like strings, where even a newline is a
// formatting hazard rather than content: object member names, choice options,
// and the model name.
func safeLabel(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// safeToken reports whether s is a non-empty run of printable, non-space ASCII:
// the shape of an API key, and the only shape this package will put into an
// Authorization header.
func safeToken(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
