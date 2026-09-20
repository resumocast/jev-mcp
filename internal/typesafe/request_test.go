package typesafe

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateRequestAcceptsAllQuestionTypes(t *testing.T) {
	if err := ValidateRequest(sampleRequest(t)); err != nil {
		t.Fatalf("sample request should validate, got %v", err)
	}
}

func TestValidateRequestAcceptsStructuredState(t *testing.T) {
	for name, state := range map[string]string{
		"string":       `"a plain string"`,
		"object":       `{"ticket":{"messages":["first","second"]}}`,
		"array":        `["one","two"]`,
		"empty object": `{}`,
		"empty array":  `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest(t)
			req.State = raw(state)
			if err := ValidateRequest(req); err != nil {
				t.Fatalf("state %s should validate, got %v", state, err)
			}
		})
	}
}

// Records hold numbers, flags and absent fields. The documented state example
// at https://docs.typesafe.ai/concepts/state carries {"amount_usd": 49}, so
// rejecting nested scalars would reject the shape the API recommends.
func TestValidateRequestAcceptsScalarsInsideRecords(t *testing.T) {
	documentedExample := `{
	  "ticket": {
	    "subject": "Duplicate charge",
	    "messages": [
	      {"from": "customer", "text": "I was charged twice for order A-104."},
	      {"from": "support", "text": "We are checking the charges."}
	    ]
	  },
	  "order": {
	    "id": "A-104",
	    "charges": [
	      {"amount_usd": 49, "status": "captured"},
	      {"amount_usd": 49, "status": "captured"}
	    ]
	  },
	  "refund_policy": "Duplicate charges are eligible for a refund."
	}`

	cases := map[string]string{
		"documented example": documentedExample,
		"nested number":      `{"count":3}`,
		"nested boolean":     `{"open":false}`,
		"nested null":        `{"assignee":null}`,
		"number in array":    `["a",1]`,
		"negative and float": `{"delta":-1.5,"ratio":0.25}`,
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest(t)
			req.State = raw(state)
			if err := ValidateRequest(req); err != nil {
				t.Fatalf("state should validate, got %v", err)
			}
		})
	}
}

// The top level is still restricted: the field is the material the model
// reads, and 42 on its own is not material.
func TestValidateRequestRejectsNonTextAtTopLevel(t *testing.T) {
	for name, state := range map[string]string{
		"top-level number":  `42`,
		"top-level boolean": `true`,
		"top-level null":    `null`,
		"missing":           ``,
		"malformed":         `{"unclosed":`,
		"trailing content":  `{"a":"b"} {"c":"d"}`,
	} {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest(t)
			req.State = raw(state)
			err := ValidateRequest(req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("state %s: want ErrInvalidRequest, got %v", state, err)
			}
		})
	}
}

// A blank top-level string is rejected here and by the MCP layer, so the same
// input cannot be accepted at one boundary and refused at the next.
func TestValidateRequestRejectsBlankTopLevelString(t *testing.T) {
	for _, state := range []string{`""`, `"   "`, `"\n\t"`} {
		t.Run(state, func(t *testing.T) {
			req := sampleRequest(t)
			req.State = raw(state)
			if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
		t.Run("instructions "+state, func(t *testing.T) {
			req := sampleRequest(t)
			q := noulQuestion(t)
			q.Instructions = raw(state)
			req.Questions = map[string]Question{"q": q}
			if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

// State carries real material: log excerpts, diffs, terminal output. A control
// character in there is data, it travels as a JSON escape, and stripping it
// would corrupt the evidence the model is meant to read.
func TestValidateRequestAllowsControlCharactersInsideText(t *testing.T) {
	cases := map[string]string{
		"escaped NUL in a log":   `{"log":"before` + esc(0x00) + `after"}`,
		"terminal escape":        `{"log":"` + esc(0x1b) + `[31mred` + esc(0x1b) + `[0m"}`,
		"bell":                   `"a log line with a bell ` + esc(0x07) + ` in it"`,
		"tabs and newlines":      `"line one\nline two\tcolumn\r\n"`,
		"vertical tab in a diff": `{"diff":"@@ -1 +1 @@` + esc(0x0b) + `"}`,
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest(t)
			req.State = raw(state)
			if err := ValidateRequest(req); err != nil {
				t.Fatalf("control characters in text should be allowed, got %v", err)
			}
		})
	}
}

// Member names are structure, not content, and are held to the stricter rule.
func TestValidateRequestRejectsUnsafeStructure(t *testing.T) {
	cases := map[string]string{
		"invalid UTF-8":      "\"\xff\xfe\"",
		"duplicate member":   `{"a":"one","a":"two"}`,
		"control in name":    `{"a` + esc(0x01) + `b":"value"}`,
		"newline in name":    `{"a\nb":"value"}`,
		"too deep":           deepJSON(maxJSONDepth + 1),
		"too many members":   wideObject(maxObjectMembers + 1),
		"too many elements":  wideArray(maxArrayElements + 1),
		"state over its cap": fmt.Sprintf("%q", repeat(maxStateBytes+1)),
	}
	for name, state := range cases {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest(t)
			req.State = raw(state)
			if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

// encoding/json resolves a duplicate member last-wins, so a rubric that reads
// as three options to its author would be sent as two. The raw bytes are still
// available at this layer, so the ambiguity is refused.
func TestValidateRequestRejectsDuplicateCriteriaMembers(t *testing.T) {
	cases := map[string]Question{
		"choice": {
			Type:         TypeChoice,
			Instructions: raw(`"which"`),
			Criteria:     raw(`{"a":"first","b":"second","a":"shadowed"}`),
		},
		"noul": {
			Type:         TypeNoul,
			Instructions: raw(`"is it"`),
			Criteria:     raw(`{"true":"yes","true":"also yes"}`),
		},
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": q}}
			if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}

	// A duplicate inside the state is caught by the same scan.
	t.Run("state", func(t *testing.T) {
		req := Request{
			State:     raw(`{"order":{"id":"A-104","id":"A-105"}}`),
			Questions: map[string]Question{"q": noulQuestion(t)},
		}
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("want ErrInvalidRequest, got %v", err)
		}
	})

	// Control: the same rubrics without duplicates still pass, so the check is
	// rejecting the ambiguity and not the shape.
	t.Run("control", func(t *testing.T) {
		req := Request{
			State: raw(`"s"`),
			Questions: map[string]Question{
				"c": {Type: TypeChoice, Instructions: raw(`"which"`), Criteria: raw(`{"a":"first","b":"second"}`)},
				"n": {Type: TypeNoul, Instructions: raw(`"is it"`), Criteria: raw(`{"true":"yes","false":"no"}`)},
			},
		}
		if err := ValidateRequest(req); err != nil {
			t.Fatalf("control case should validate, got %v", err)
		}
	})
}

func TestValidateRequestQuestionCount(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		req := sampleRequest(t)
		req.Questions = nil
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("want ErrInvalidRequest, got %v", err)
		}
	})
	t.Run("at the limit", func(t *testing.T) {
		req := sampleRequest(t)
		req.Questions = noulQuestions(t, MaxQuestions)
		if err := ValidateRequest(req); err != nil {
			t.Fatalf("%d questions should validate, got %v", MaxQuestions, err)
		}
	})
	t.Run("over the limit", func(t *testing.T) {
		req := sampleRequest(t)
		req.Questions = noulQuestions(t, MaxQuestions+1)
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Fatalf("want ErrInvalidRequest, got %v", err)
		}
	})
}

func TestValidateRequestQuestionIDs(t *testing.T) {
	ok := []string{"is_urgent", "dept-2", "a.b.c", "A1", repeat(maxQuestionIDBytes)}
	bad := []string{"", "has space", "slash/", "quote\"", "non-ascii-é", "new\nline", repeat(maxQuestionIDBytes + 1)}

	for _, id := range ok {
		req := sampleRequest(t)
		req.Questions = map[string]Question{id: noulQuestion(t)}
		if err := ValidateRequest(req); err != nil {
			t.Errorf("id %q should be accepted, got %v", id, err)
		}
	}
	for _, id := range bad {
		req := sampleRequest(t)
		req.Questions = map[string]Question{id: noulQuestion(t)}
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("id %q should be rejected, got %v", id, err)
		}
	}
}

func TestValidateRequestQuestionType(t *testing.T) {
	for _, typ := range []string{"", "Noul", "boolean", "rank", "noul "} {
		req := sampleRequest(t)
		q := noulQuestion(t)
		q.Type = typ
		req.Questions = map[string]Question{"q": q}
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("type %q should be rejected, got %v", typ, err)
		}
	}
}

func TestValidateRequestInstructions(t *testing.T) {
	bad := map[string]json.RawMessage{
		"missing":         nil,
		"null":            raw(`null`),
		"number":          raw(`7`),
		"boolean":         raw(`true`),
		"over the cap":    raw(fmt.Sprintf("%q", repeat(maxInstructionsBytes))),
		"not well-formed": raw(`{"a":`),
	}
	for name, instr := range bad {
		t.Run(name, func(t *testing.T) {
			req := sampleRequest(t)
			q := noulQuestion(t)
			q.Instructions = instr
			req.Questions = map[string]Question{"q": q}
			if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}

	t.Run("object instructions are allowed", func(t *testing.T) {
		req := sampleRequest(t)
		q := noulQuestion(t)
		q.Instructions = raw(`{"question":"Is it urgent?","note":"ignore tone"}`)
		req.Questions = map[string]Question{"q": q}
		if err := ValidateRequest(req); err != nil {
			t.Fatalf("object instructions should validate, got %v", err)
		}
	})
}

// An omitted rubric is an absent field. Sending null used to be silently
// tolerated on noul and refused on the other two; now it means the same thing
// everywhere, so the MCP layer above cannot accidentally rely on the
// difference.
func TestValidateRequestRejectsNullCriteriaConsistently(t *testing.T) {
	for _, typ := range []string{TypeNoul, TypeChoice, TypeScore} {
		t.Run(typ, func(t *testing.T) {
			q := Question{Type: typ, Instructions: raw(`"ask"`), Criteria: raw(`null`)}
			req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": q}}
			if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

func TestValidateNoulCriteria(t *testing.T) {
	good := []json.RawMessage{
		nil,
		raw(`{"true":"yes means this"}`),
		raw(`{"false":"no means this"}`),
		raw(`{"true":"a","false":"b"}`),
	}
	bad := []json.RawMessage{
		raw(`null`),
		raw(`{}`),
		raw(`{"yes":"a"}`),
		raw(`{"true":null}`),
		raw(`{"true":1}`),
		raw(`{"true":""}`),
		raw(`["a","b"]`),
		raw(`"a"`),
	}
	for _, c := range good {
		req := sampleRequest(t)
		q := noulQuestion(t)
		q.Criteria = c
		req.Questions = map[string]Question{"q": q}
		if err := ValidateRequest(req); err != nil {
			t.Errorf("noul criteria %s should be accepted, got %v", c, err)
		}
	}
	for _, c := range bad {
		req := sampleRequest(t)
		q := noulQuestion(t)
		q.Criteria = c
		req.Questions = map[string]Question{"q": q}
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("noul criteria %s should be rejected, got %v", c, err)
		}
	}
}

func TestValidateChoiceCriteria(t *testing.T) {
	good := []json.RawMessage{
		raw(`{"a":"first","b":"second"}`),
		raw(`{"a":null,"b":null}`),
		raw(`{"only":"one option is legal, if useless"}`),
	}
	bad := []json.RawMessage{
		nil,
		raw(`null`),
		raw(`{}`),
		raw(`["a","b"]`),
		raw(`{"a":1}`),
		raw(`{"a":{"nested":"object"}}`),
		raw(fmt.Sprintf(`{%q:"too long"}`, repeat(maxOptionBytes+1))),
		manyOptions(maxChoiceOptions + 1),
	}
	for _, c := range good {
		q := Question{Type: TypeChoice, Instructions: raw(`"pick"`), Criteria: c}
		req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": q}}
		if err := ValidateRequest(req); err != nil {
			t.Errorf("choice criteria %s should be accepted, got %v", c, err)
		}
	}
	for _, c := range bad {
		q := Question{Type: TypeChoice, Instructions: raw(`"pick"`), Criteria: c}
		req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": q}}
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("choice criteria %s should be rejected, got %v", c, err)
		}
	}
}

// Score levels are 2 to 10 per https://docs.typesafe.ai/primitives/score.
func TestValidateScoreCriteria(t *testing.T) {
	good := []json.RawMessage{
		raw(`["low","high"]`),
		raw(`["Calm","Frustrated","Very angry"]`),
		manyLevels(maxScoreLevels),
	}
	bad := []json.RawMessage{
		nil,
		raw(`null`),
		raw(`[]`),
		raw(`["only one"]`),
		raw(`{"0":"a","1":"b"}`),
		raw(`["a",2]`),
		raw(`["a",null]`),
		raw(`["a",""]`),
		// The structured level form documented in the primitive pages is a
		// deliberate v1 omission, not an oversight.
		raw(`[{"what":"low"},{"what":"high"}]`),
		manyLevels(maxScoreLevels + 1),
	}
	for _, c := range good {
		q := Question{Type: TypeScore, Instructions: raw(`"rate"`), Criteria: c}
		req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": q}}
		if err := ValidateRequest(req); err != nil {
			t.Errorf("score criteria %s should be accepted, got %v", c, err)
		}
	}
	for _, c := range bad {
		q := Question{Type: TypeScore, Instructions: raw(`"rate"`), Criteria: c}
		req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": q}}
		if err := ValidateRequest(req); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("score criteria %s should be rejected, got %v", c, err)
		}
	}

	if maxScoreLevels != 10 {
		t.Errorf("maxScoreLevels = %d, want 10 per the score primitive docs", maxScoreLevels)
	}
}

// Each field can be under its own cap while the request as a whole is not, so
// the total is checked against the bytes that would actually be sent.
func TestValidateRequestTotalSize(t *testing.T) {
	req := Request{
		State:     jsonString(t, repeat(maxStateBytes-16)),
		Questions: map[string]Question{},
	}
	for i := 0; i < 6; i++ {
		req.Questions[fmt.Sprintf("q%d", i)] = Question{
			Type:         TypeNoul,
			Instructions: jsonString(t, repeat(maxInstructionsBytes-16)),
		}
	}
	err := ValidateRequest(req)
	if !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("want ErrRequestTooLarge, got %v", err)
	}
}

// json.Marshal rewrites every <, > and & as a six-byte Unicode escape. On state
// made of HTML, XML or shell redirection that inflates the body several times
// over and could push a legitimate request past the limit for no reason.
func TestEncodeRequestDoesNotEscapeHTML(t *testing.T) {
	state := `<div class="x">a & b</div><span>c > d</span>`
	req := Request{
		State:     jsonString(t, state),
		Questions: map[string]Question{"q": noulQuestion(t)},
	}
	body, err := encodeRequest(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if strings.Contains(string(body), string(rune(92))+"u003c") || strings.Contains(string(body), string(rune(92))+"u0026") {
		t.Fatalf("body was HTML-escaped: %s", body)
	}
	if !strings.Contains(string(body), "<div") {
		t.Fatalf("body lost the original markup: %s", body)
	}
	// A state made entirely of markup stays close to its own size. With
	// escaping on, these 30000 bytes would encode to about 180000 and be
	// rejected as too large.
	dense := strings.Repeat("<&>", 10000)
	big := Request{
		State:     jsonString(t, dense),
		Questions: map[string]Question{"q": noulQuestion(t)},
	}
	denseBody, err := encodeRequest(big)
	if err != nil {
		t.Fatalf("a body that only escaping would inflate was rejected: %v", err)
	}
	if len(denseBody) > len(dense)+1024 {
		t.Fatalf("encoded body is %d bytes for %d bytes of state", len(denseBody), len(dense))
	}
	if err := ValidateRequest(big); err != nil {
		t.Fatalf("markup-heavy state should validate, got %v", err)
	}
}

func TestEncodeRequestHasNoTrailingNewline(t *testing.T) {
	body, err := encodeRequest(sampleRequest(t))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if len(body) == 0 || body[len(body)-1] != '}' {
		t.Fatalf("body does not end at the closing brace: %q", tail(body))
	}
}

// Errors are read by a model and shown to a user, so they must describe the
// rule without quoting the value that broke it.
func TestValidateRequestErrorsDoNotQuoteInput(t *testing.T) {
	req := Request{
		State: jsonString(t, "state containing "+leakMarker),
		Questions: map[string]Question{
			"q": {
				Type:         "bogus",
				Instructions: jsonString(t, "instructions containing "+leakMarker),
				Criteria:     raw(`{"` + leakMarker + `":"x"}`),
			},
		},
	}
	assertNoLeak(t, ValidateRequest(req), leakMarker)

	req2 := Request{
		State:     raw(`{"secret":"` + leakMarker + `","a":"b","a":"c"}`),
		Questions: map[string]Question{"q": noulQuestion(t)},
	}
	assertNoLeak(t, ValidateRequest(req2), leakMarker)
}

// The model is pinned and is not a caller-settable field, so it can only ever
// appear once, with one value.
func TestEncodeRequestPinsTheModel(t *testing.T) {
	body, err := encodeRequest(sampleRequest(t))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var decoded struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Model != Model {
		t.Fatalf("model = %q, want %q", decoded.Model, Model)
	}
	if n := strings.Count(string(body), `"model"`); n != 1 {
		t.Fatalf(`body contains %d "model" members, want 1`, n)
	}
	if Model != "jev-1.13.0" {
		t.Fatalf("pinned model drifted: %q", Model)
	}
}

// Helpers.

func noulQuestion(t *testing.T) Question {
	t.Helper()
	return Question{Type: TypeNoul, Instructions: jsonString(t, "Is it urgent?")}
}

func noulQuestions(t *testing.T, n int) map[string]Question {
	t.Helper()
	qs := make(map[string]Question, n)
	for i := 0; i < n; i++ {
		qs[fmt.Sprintf("q%d", i)] = noulQuestion(t)
	}
	return qs
}

func deepJSON(depth int) string {
	return strings.Repeat(`{"a":`, depth) + `"x"` + strings.Repeat(`}`, depth)
}

func wideObject(n int) string {
	var b strings.Builder
	b.WriteByte('{')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%d":"v"`, i)
	}
	b.WriteByte('}')
	return b.String()
}

func wideArray(n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"v"`)
	}
	b.WriteByte(']')
	return b.String()
}

func manyOptions(n int) json.RawMessage { return raw(wideObject(n)) }

func manyLevels(n int) json.RawMessage {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"level %d"`, i)
	}
	b.WriteByte(']')
	return raw(b.String())
}

func tail(b []byte) string {
	if len(b) > 32 {
		return string(b[len(b)-32:])
	}
	return string(b)
}

// esc renders a JSON \u escape for a control code point. Fixtures build
// their escapes through this helper instead of writing them inline, so a
// literal control byte can never end up in this file pretending to be an
// escape sequence. A raw control character inside a JSON string is invalid
// JSON, so the mix-up would fail the test for the wrong reason.
func esc(code byte) string { return fmt.Sprintf(`\u%04x`, code) }
