package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"jev-mcp/internal/typesafe"
)

func TestNormalizeArgumentsAcceptsEveryQuestionType(t *testing.T) {
	args := json.RawMessage(`{
		"state": { "message": "Payouts have been failing for three days." },
		"questions": {
			"urgent": { "type": "noul", "instructions": "Does the message convey urgency?" },
			"explained": {
				"type": "noul",
				"instructions": "Does the message say what was tried?",
				"criteria": { "true": "It names an attempt", "false": "It names no attempt" }
			},
			"team": {
				"type": "choice",
				"instructions": "Which team should handle this?",
				"criteria": { "billing": "Payments and refunds", "technical": "Bugs and outages" }
			},
			"frustration": {
				"type": "score",
				"instructions": "How frustrated is the customer?",
				"criteria": ["Calm", "Frustrated", "Very angry"]
			}
		}
	}`)

	req, err := normalizeArguments(args)
	if err != nil {
		t.Fatalf("normalizeArguments() error = %v, want a request", err)
	}
	if got, want := string(req.State), `{"message":"Payouts have been failing for three days."}`; got != want {
		t.Errorf("state = %s, want it compacted to %s", got, want)
	}
	if len(req.Questions) != 4 {
		t.Fatalf("questions = %d, want 4", len(req.Questions))
	}
	if got := req.Questions["urgent"]; got.Type != "noul" || len(got.Criteria) != 0 {
		t.Errorf("urgent = %+v, want a noul question with no criteria", got)
	}
	if got := string(req.Questions["explained"].Criteria); !strings.HasPrefix(got, `{"`) {
		t.Errorf("explained criteria = %s, want the object to be kept", got)
	}
	if got := string(req.Questions["frustration"].Criteria); got != `["Calm","Frustrated","Very angry"]` {
		t.Errorf("frustration criteria = %s, want the ordered levels", got)
	}
	if got := string(req.Questions["team"].Instructions); got != `"Which team should handle this?"` {
		t.Errorf("team instructions = %s, want the JSON string", got)
	}
}

func TestNormalizeArgumentsTreatsExplicitNullCriteriaAsAbsent(t *testing.T) {
	args := json.RawMessage(`{"state":"text","questions":{"q":{"type":"noul","instructions":"Is it text?","criteria":null}}}`)

	req, err := normalizeArguments(args)
	if err != nil {
		t.Fatalf("normalizeArguments() error = %v", err)
	}
	if got := req.Questions["q"].Criteria; len(got) != 0 {
		t.Errorf("criteria = %s, want it dropped", got)
	}
}

func TestNormalizeArgumentsRejects(t *testing.T) {
	tests := []struct {
		name string
		args string
	}{
		{name: "not an object", args: `["state"]`},
		{name: "empty arguments", args: `{}`},
		{name: "state missing", args: `{"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`},
		{name: "state null", args: `{"state":null,"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`},
		{name: "state number", args: `{"state":42,"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`},
		{name: "state boolean", args: `{"state":true,"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`},
		{name: "state empty string", args: `{"state":"   ","questions":{"q":{"type":"noul","instructions":"Is it?"}}}`},
		{name: "questions missing", args: `{"state":"text"}`},
		{name: "questions empty", args: `{"state":"text","questions":{}}`},
		{name: "questions not an object", args: `{"state":"text","questions":[]}`},
		{name: "unknown argument", args: `{"state":"text","model":"jev-1.13.0","questions":{"q":{"type":"noul","instructions":"Is it?"}}}`},
		{name: "unknown question field", args: `{"state":"text","questions":{"q":{"type":"noul","instructions":"Is it?","weight":2}}}`},
		{name: "question not an object", args: `{"state":"text","questions":{"q":"noul"}}`},
		{name: "unknown question type", args: `{"state":"text","questions":{"q":{"type":"rank","instructions":"Is it?"}}}`},
		{name: "type missing", args: `{"state":"text","questions":{"q":{"instructions":"Is it?"}}}`},
		{name: "instructions missing", args: `{"state":"text","questions":{"q":{"type":"noul"}}}`},
		{name: "instructions number", args: `{"state":"text","questions":{"q":{"type":"noul","instructions":7}}}`},
		{name: "instructions empty", args: `{"state":"text","questions":{"q":{"type":"noul","instructions":""}}}`},
		{name: "noul criteria array", args: `{"state":"text","questions":{"q":{"type":"noul","instructions":"Is it?","criteria":["a","b"]}}}`},
		{name: "choice criteria missing", args: `{"state":"text","questions":{"q":{"type":"choice","instructions":"Which?"}}}`},
		{name: "choice criteria empty", args: `{"state":"text","questions":{"q":{"type":"choice","instructions":"Which?","criteria":{}}}}`},
		{name: "choice criteria array", args: `{"state":"text","questions":{"q":{"type":"choice","instructions":"Which?","criteria":["a","b"]}}}`},
		{name: "score criteria missing", args: `{"state":"text","questions":{"q":{"type":"score","instructions":"How much?"}}}`},
		{name: "score criteria object", args: `{"state":"text","questions":{"q":{"type":"score","instructions":"How much?","criteria":{"0":"low"}}}}`},
		{name: "score criteria one level", args: `{"state":"text","questions":{"q":{"type":"score","instructions":"How much?","criteria":["only"]}}}`},
		{name: "trailing content", args: `{"state":"text","questions":{"q":{"type":"noul","instructions":"Is it?"}}} {"state":"again"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := normalizeArguments(json.RawMessage(tt.args)); err == nil {
				t.Fatalf("normalizeArguments(%s) = nil error, want a rejection", tt.args)
			} else {
				assertSafeMessage(t, err.Error())
			}
		})
	}
}

func TestNormalizeArgumentsRejectsTooManyQuestions(t *testing.T) {
	questions := make(map[string]any, typesafe.MaxQuestions+1)
	for i := 0; i <= typesafe.MaxQuestions; i++ {
		questions[fmt.Sprintf("q%d", i)] = map[string]any{"type": "noul", "instructions": "Is it?"}
	}
	args, err := json.Marshal(map[string]any{"state": "text", "questions": questions})
	if err != nil {
		t.Fatalf("test setup: %v", err)
	}

	if _, err := normalizeArguments(args); err == nil {
		t.Fatalf("normalizeArguments() accepted %d questions, want at most %d", len(questions), typesafe.MaxQuestions)
	}
}

func TestNormalizeArgumentsRejectsArgumentsAboveTheRequestBudget(t *testing.T) {
	args := json.RawMessage(`{"state":"` + strings.Repeat("x", typesafe.MaxRequestBytes) + `","questions":{"q":{"type":"noul","instructions":"Is it?"}}}`)

	_, err := normalizeArguments(args)
	if err == nil {
		t.Fatal("normalizeArguments() accepted arguments above the request budget")
	}
	if err.Error() != msgArgumentsTooLarge {
		t.Errorf("error = %q, want %q", err, msgArgumentsTooLarge)
	}
}

func TestNormalizeArgumentsAcceptsTheDocumentedQuestionIDAlphabet(t *testing.T) {
	// The alphabet is the one internal/typesafe enforces, so an id this server
	// accepts is never rejected one layer down for a reason it could have
	// explained itself.
	for _, id := range []string{"a", "Q9", "is_urgent", "is-urgent", "ticket.priority", strings.Repeat("z", maxQuestionIDBytes)} {
		t.Run(id, func(t *testing.T) {
			if !validQuestionID(id) {
				t.Errorf("validQuestionID(%q) = false, want true", id)
			}
		})
	}
}

func TestNormalizeArgumentsRejectsQuestionIDsOutsideTheAlphabetWithoutEchoingThem(t *testing.T) {
	// The ids carry a token that does not appear in any message this package
	// sends, so "the error repeats the id" cannot pass by coincidence.
	ids := []string{
		"bad\nzebra", "bad\x00zebra", "\x1b]0;zebra\x07", " zebra", "zebra ",
		"zebra!", "zebra/path", "zebra:1", "zébra", "", strings.Repeat("z", maxQuestionIDBytes+1),
	}

	for _, id := range ids {
		t.Run(fmt.Sprintf("%q", id), func(t *testing.T) {
			args, err := json.Marshal(map[string]any{
				"state":     "text",
				"questions": map[string]any{id: map[string]any{"type": "noul", "instructions": "Is it?"}},
			})
			if err != nil {
				t.Fatalf("test setup: %v", err)
			}
			_, err = normalizeArguments(args)
			if err == nil {
				t.Fatalf("normalizeArguments() accepted the question id %q", id)
			}
			if id != "" && strings.Contains(err.Error(), id) {
				t.Errorf("error %q repeats the rejected id %q", err, id)
			}
			assertSafeMessage(t, err.Error())
		})
	}
}

func TestNormalizeArgumentsNeverQuotesTheArguments(t *testing.T) {
	// One sentinel in every position a caller controls. None of them may come
	// back in an error, because an error ends up in a transcript.
	const sentinel = "ZEBRA-SENTINEL"
	args := []string{
		`{"state":"` + sentinel + `","questions":{"q":{"type":"rank","instructions":"Is it?"}}}`,
		`{"state":"text","questions":{"` + sentinel + `!":{"type":"noul","instructions":"Is it?"}}}`,
		`{"state":"text","questions":{"q":{"type":"noul","instructions":"` + sentinel + `","criteria":["a"]}}}`,
		`{"state":"text","questions":{"q":{"type":"choice","instructions":"Which?","criteria":{"` + sentinel + `":1}}}}`,
		`{"state":"text","questions":{"q":{"type":"noul","instructions":"Is it?","` + sentinel + `":true}}}`,
		`{"state":"text","` + sentinel + `":1,"questions":{"q":{"type":"noul","instructions":"Is it?"}}}`,
	}

	for _, arg := range args {
		_, err := normalizeArguments(json.RawMessage(arg))
		if err == nil {
			t.Errorf("normalizeArguments(%s) was accepted, want a rejection", arg)
			continue
		}
		if strings.Contains(err.Error(), sentinel) {
			t.Errorf("error %q repeats caller-controlled text from %s", err, arg)
		}
		assertSafeMessage(t, err.Error())
	}
}

// assertSafeMessage checks the invariant every message this package sends must
// hold: printable, single-line, and bounded, so nothing can smuggle terminal
// control sequences or extra framing into a result a user reads.
func assertSafeMessage(t *testing.T, message string) {
	t.Helper()
	if message == "" {
		t.Error("message is empty")
	}
	if len(message) > 2048 {
		t.Errorf("message is %d bytes, want a bounded message", len(message))
	}
	for _, r := range message {
		if r < 0x20 || r == 0x7f {
			t.Errorf("message %q contains the control character %q", message, r)
			return
		}
	}
}

func TestEvaluateInputSchemaMatchesTheEnforcedLimits(t *testing.T) {
	var schema map[string]any
	if err := json.Unmarshal([]byte(evaluateInputSchema), &schema); err != nil {
		t.Fatalf("the advertised input schema is not valid JSON: %v", err)
	}
	if schema["type"] != "object" {
		t.Errorf("schema type = %v, want object", schema["type"])
	}

	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("schema has no properties object")
	}
	questions, ok := properties["questions"].(map[string]any)
	if !ok {
		t.Fatal("schema has no questions property")
	}
	maxProps, ok := questions["maxProperties"].(float64)
	if !ok {
		t.Fatal("the questions property declares no maxProperties")
	}
	if int(maxProps) != typesafe.MaxQuestions {
		t.Errorf("schema maxProperties = %d, want typesafe.MaxQuestions (%d)", int(maxProps), typesafe.MaxQuestions)
	}
	if schemaMaxQuestions != typesafe.MaxQuestions {
		t.Errorf("schemaMaxQuestions = %d, want typesafe.MaxQuestions (%d)", schemaMaxQuestions, typesafe.MaxQuestions)
	}

	// The schema is where a model learns which ids it may choose, so it has to
	// describe the alphabet validQuestionID actually enforces.
	description, _ := questions["description"].(string)
	for _, word := range []string{"letters", "digits", "underscore", "hyphen", "dot", "64"} {
		if !strings.Contains(description, word) {
			t.Errorf("the questions description does not document the id alphabet: %q is missing", word)
		}
	}

	question, ok := questions["additionalProperties"].(map[string]any)
	if !ok {
		t.Fatal("the questions property declares no question schema")
	}
	questionProps, ok := question["properties"].(map[string]any)
	if !ok {
		t.Fatal("the question schema has no properties")
	}
	typeProp, ok := questionProps["type"].(map[string]any)
	if !ok {
		t.Fatal("the question schema has no type property")
	}
	enum, ok := typeProp["enum"].([]any)
	if !ok || len(enum) != 3 {
		t.Fatalf("question type enum = %v, want the three question types", typeProp["enum"])
	}
	for i, want := range []string{"noul", "choice", "score"} {
		if enum[i] != want {
			t.Errorf("enum[%d] = %v, want %q", i, enum[i], want)
		}
	}
	if question["additionalProperties"] != false {
		t.Error("the question schema allows unknown fields, but the server rejects them")
	}
}

func TestEvaluateToolAdvertisesItsBoundaries(t *testing.T) {
	tool := evaluateTool()

	if tool.Name != toolName {
		t.Errorf("tool name = %q, want %q", tool.Name, toolName)
	}
	if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
		t.Error("the tool does not advertise the read-only hint")
	}
	if !json.Valid(tool.InputSchema) {
		t.Error("the tool advertises an invalid input schema")
	}

	const baselineDescriptionBytes = 1052
	maxDescriptionBytes := baselineDescriptionBytes * 85 / 100
	if got := len([]byte(tool.Description)); got > maxDescriptionBytes {
		t.Errorf("tool description = %d bytes, want at most %d", got, maxDescriptionBytes)
	}
	if strings.Contains(strings.ToLower(tool.Description), "criteria") {
		t.Error("tool description repeats format details that belong in the input schema")
	}

	// Format details belong in the schema, while selection, interpretation, and
	// safety guidance remain available across the complete model-facing surface.
	visible := strings.ToLower(tool.Description + "\n" + string(tool.InputSchema) + "\n" + serverInstructions)
	for _, phrase := range []string{
		"bounded semantic uncertainty",
		"do not call it merely to confirm your conclusion",
		"noul: yes-probability",
		"selected criteria option with distribution and confidence",
		"probability-weighted position with distribution and confidence",
		"1-255 options to a string description or null",
		"2-10 string descriptions",
		"indices are 0 to n-1",
		"batch independent questions",
		"raw evidence, uncertainty, and counterevidence",
		"preserve source claims",
		"your verdict",
		"credentials or secrets",
		"paid external",
		"text or code generation",
		"arithmetic, counting, date calculations",
		"deterministic tests, calculations, or builds",
		"cannot authorize actions",
		"security or safety assurance",
		"confidence is distribution concentration, not correctness",
		"probability with supporting evidence",
		"verify consequential decisions",
		"disclose when one relies on the judgment",
	} {
		if !strings.Contains(visible, phrase) {
			t.Errorf("the model-facing MCP interface does not mention %q", phrase)
		}
	}
	// The claim this description must never make again.
	if strings.Contains(tool.Description, "Each answer comes back with a probability distribution") {
		t.Error("the tool description claims every answer carries a distribution, but a noul answer is one probability")
	}
	if strings.Contains(toolDescription, "\n") {
		t.Error("the tool description contains a newline")
	}
}

func TestDocumentationDoesNotOversellConfidence(t *testing.T) {
	// Confidence is the concentration of a distribution. Saying anything else
	// in the text a model reads is how a judgment becomes an authorization.
	for name, text := range map[string]string{"tool description": toolDescription, "server instructions": serverInstructions} {
		lower := strings.ToLower(text)
		for _, claim := range []string{
			"confidence means the answer is correct",
			"high confidence proves",
			"safe to proceed",
		} {
			if strings.Contains(lower, claim) {
				t.Errorf("the %s claims %q", name, claim)
			}
		}
		if !strings.Contains(lower, "security") {
			t.Errorf("the %s does not say that a judgment is not a security control", name)
		}
	}
}

func TestServerInstructionsAreOneGuidelinePerLine(t *testing.T) {
	lines := strings.Split(serverInstructions, "\n")
	if len(lines) < 5 {
		t.Fatalf("instructions have %d lines, want the full guidance", len(lines))
	}
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			t.Errorf("instruction line %d is blank; a client that splits on newlines would ship an empty guideline", i)
		}
	}
}

func TestToolSuccessCarriesTheSameJSONTwiceAndCopiesIt(t *testing.T) {
	body := []byte(`{"model":"jev-1.13.0","answers":{},"usage":{}}`)
	result := toolSuccess(body)

	if len(result.Content) != 1 || result.Content[0].Type != "text" {
		t.Fatalf("content = %+v, want one text block", result.Content)
	}
	if result.Content[0].Text != string(body) {
		t.Errorf("text = %q, want the response JSON", result.Content[0].Text)
	}
	if string(result.StructuredContent) != string(body) {
		t.Errorf("structuredContent = %s, want the response JSON", result.StructuredContent)
	}
	if result.IsError {
		t.Error("a successful result must not set isError")
	}

	copy(body, []byte(`{"model":"zzzzzzzzz"`))
	if strings.Contains(string(result.StructuredContent), "zzzzzzzzz") {
		t.Error("structuredContent aliases the caller's buffer")
	}
}

func TestToolFailureIsMarkedAndTextOnly(t *testing.T) {
	result := toolFailure(msgBusy)

	if !result.IsError {
		t.Error("a failure result must set isError")
	}
	if len(result.StructuredContent) != 0 {
		t.Errorf("structuredContent = %s, want nothing on a failure", result.StructuredContent)
	}
	if len(result.Content) != 1 || result.Content[0].Text != msgBusy {
		t.Errorf("content = %+v, want the failure message", result.Content)
	}
}

func TestClipBoundsAndFlattensBorrowedText(t *testing.T) {
	clipped := clip("line one\nline two\x07", maxBorrowedErrorBytes)
	assertSafeMessage(t, clipped)

	long := clip(strings.Repeat("y", 1000), maxBorrowedErrorBytes)
	if len(long) > maxBorrowedErrorBytes+3 {
		t.Errorf("clip() returned %d bytes, want at most %d", len(long), maxBorrowedErrorBytes+3)
	}
}

func TestJSONKindNamesValuesWithoutQuotingThem(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{raw: `{"secret":"x"}`, want: "an object"},
		{raw: `["secret"]`, want: "an array"},
		{raw: `"secret"`, want: "a string"},
		{raw: `-12.5`, want: "a number"},
		{raw: `false`, want: "a boolean"},
		{raw: `null`, want: "null"},
		{raw: ``, want: "missing"},
	}

	for _, tt := range tests {
		if got := jsonKind(json.RawMessage(tt.raw)); got != tt.want {
			t.Errorf("jsonKind(%q) = %q, want %q", tt.raw, got, tt.want)
		}
	}
}
