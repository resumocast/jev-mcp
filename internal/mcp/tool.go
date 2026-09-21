package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"jev-mcp/internal/jsonfields"
	"jev-mcp/internal/typesafe"
)

// The evaluate tool: its advertised description, its input schema, and the
// validation that turns a tool call into a [typesafe.Request].
//
// Everything here runs before the network call, so its error strings are shown
// to the model that made the call. They are built only from literals in this
// file. Nothing from the arguments is quoted back, not even a question id: an
// id is caller-controlled text, and a message that repeats one is a way for
// content to travel out of this server and into a transcript.

const toolName = "evaluate"

// toolDescription is what a model reads when it decides whether to call the
// tool. It has to be accurate about what comes back, and explicit about the
// work this model does not do, because a wrong call costs a paid request and
// returns a confident answer to the wrong kind of question.
const toolDescription = "Use TypeSafe Jev for bounded semantic uncertainty about evidence you already have, " +
	"such as classification, routing, triage, extraction, or rubric scoring. It returns typed " +
	"probability judgments, not generated text or facts. Do not call it merely to confirm your " +
	"conclusion. Supply raw evidence, uncertainty, and counterevidence, preserving source claims " +
	"but not adding your verdict; never include credentials or secrets. Each " +
	"call reaches a paid external API. Do not substitute it for text or code generation, " +
	"arithmetic, counting, date calculations, deterministic tests, calculations, or builds, " +
	"authorization, or security or safety assurance. Confidence is distribution concentration, " +
	"not correctness. Verify consequential decisions by other means."

// serverInstructions is served in the initialize result. It complements the
// schema with guidance for deciding when and how to evaluate. One line is one
// complete guideline for clients that split instructions on newlines.
const serverInstructions = `Use evaluate only when bounded semantic uncertainty affects a decision; it returns typed probability judgments, not generated text or facts. Do not call it merely to confirm your conclusion.
Ask one narrow, self-contained condition per question because ids are not sent to Jev. Batch independent questions about the same state; they cannot see each other's answers.
Supply raw evidence, uncertainty, and counterevidence, or a faithful condensation with named fields. Preserve source claims but do not add your verdict. Never include credentials or secrets. Each call reaches a paid external TypeSafe API.
For choice, include "none of these" when appropriate. Use concrete score levels.
A noul near 0.5 is undecided, not medium intensity. Confidence is distribution concentration, not correctness, safety, or authorization.
Score levels are zero-based: N levels run from 0 to N-1. Interpret scores with their legend and probabilities.
Do not use evaluate for text or code generation, arithmetic, counting, date calculations, or instead of deterministic tests, calculations, or builds. It cannot authorize actions or establish correctness, completeness, security, or safety. Report its probability with supporting evidence, verify consequential decisions by other means, and disclose when one relies on the judgment.
Reference: https://docs.typesafe.ai/api.md`

// evaluateInputSchema is the tool's JSON Schema. It is written by hand because
// this module has no dependencies; schemaMaxQuestions is checked against
// typesafe.MaxQuestions by a test so the two cannot drift apart.
//
// The schema stays inside the subset that MCP clients reliably forward to
// model providers: object types, enums, required, and additionalProperties. The
// Pi adapter declares its own provider-side schema and validates arguments
// independently, so this one is documentation and a first filter, never the
// only check.
const evaluateInputSchema = `{
  "type": "object",
  "properties": {
    "state": {
      "type": ["string", "object", "array"],
      "description": "Raw evidence, uncertainty, and counterevidence as text, object, or array. Preserve source claims; omit your own verdict, credentials, and secrets."
    },
    "questions": {
      "type": "object",
      "description": "Map of question id to question. Answers come back under the same ids. An id may use letters, digits, underscore, hyphen and dot, up to 64 characters. Ids are not sent to the model.",
      "minProperties": 1,
      "maxProperties": 16,
      "additionalProperties": {
        "type": "object",
        "properties": {
          "type": {
            "type": "string",
            "enum": ["noul", "choice", "score"],
            "description": "noul: yes-probability. choice: selected criteria option with distribution and confidence. score: probability-weighted position with distribution and confidence."
          },
          "instructions": {
            "type": ["string", "object", "array"],
            "description": "Self-contained condition to judge; ids are not sent to Jev."
          },
          "criteria": {
            "description": "noul: optional object with \"true\"/\"false\" string descriptions. choice: required map of 1-255 options to a string description or null. score: required array of 2-10 string descriptions, low to high; indices are 0 to N-1."
          }
        },
        "required": ["type", "instructions"],
        "additionalProperties": false
      }
    }
  },
  "required": ["state", "questions"],
  "additionalProperties": false
}`

// schemaMaxQuestions mirrors the maxProperties in evaluateInputSchema.
const schemaMaxQuestions = 16

// maxQuestionIDBytes matches the bound the API package enforces. The two
// alphabets are kept identical so that a question this server accepts is never
// rejected one layer down for a reason this server could have explained.
const maxQuestionIDBytes = 64

// Tool result and descriptor shapes.

type textContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type callToolResult struct {
	Content           []textContent   `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

type toolAnnotations struct {
	Title         string `json:"title,omitempty"`
	ReadOnlyHint  bool   `json:"readOnlyHint,omitempty"`
	OpenWorldHint bool   `json:"openWorldHint,omitempty"`
}

type toolDescriptor struct {
	Name        string           `json:"name"`
	Description string           `json:"description"`
	InputSchema json.RawMessage  `json:"inputSchema"`
	Annotations *toolAnnotations `json:"annotations,omitempty"`
}

type toolsListResult struct {
	Tools []toolDescriptor `json:"tools"`
}

func evaluateTool() toolDescriptor {
	return toolDescriptor{
		Name:        toolName,
		Description: toolDescription,
		InputSchema: json.RawMessage(evaluateInputSchema),
		Annotations: &toolAnnotations{
			Title:        "Evaluate with Jev",
			ReadOnlyHint: true,
			// The judgment comes from a remote model, so the answer is not a
			// function of the arguments alone.
			OpenWorldHint: true,
		},
	}
}

// toolSuccess returns the result for a completed evaluation. The same JSON is
// carried twice on purpose: as text for clients that only render content, and
// as structuredContent for clients that parse it.
func toolSuccess(responseJSON []byte) callToolResult {
	return callToolResult{
		Content:           []textContent{{Type: "text", Text: string(responseJSON)}},
		StructuredContent: append(json.RawMessage(nil), responseJSON...),
	}
}

// msgResultInStructuredContent replaces the text copy of an evaluation too
// large to carry twice. It says where the answers are rather than showing a
// truncated fragment of them, which a model would read as the whole answer.
const msgResultInStructuredContent = "the evaluation succeeded; its answers were too large to repeat as text and are in structuredContent"

// toolSuccessStructuredOnly returns a result that carries the evaluation once.
func toolSuccessStructuredOnly(responseJSON []byte) callToolResult {
	return callToolResult{
		Content:           []textContent{{Type: "text", Text: msgResultInStructuredContent}},
		StructuredContent: append(json.RawMessage(nil), responseJSON...),
	}
}

// toolFailure returns an error result. Only messages assembled in this package
// reach it; an error from the network or from the API is classified into one of
// the fixed strings in server.go first.
func toolFailure(message string) callToolResult {
	return callToolResult{
		Content: []textContent{{Type: "text", Text: message}},
		IsError: true,
	}
}

// providerFailureVersion is the first and only version of the compact failure
// shape. Its values are fixed in server.go, never copied from a remote error.
const providerFailureVersion = "v1"

// providerFailure is structuredContent for an evaluator failure that matched a
// typesafe sentinel. The text result remains the human-facing interface; this
// object lets clients make a bounded, safe decision without parsing that text.
type providerFailure struct {
	Version   string `json:"version"`
	Kind      string `json:"kind"`
	Retryable bool   `json:"retryable"`
}

// toolProviderFailure adds the versioned machine-readable envelope only to a
// classified evaluator failure. Other tool failures retain their text-only
// result so existing call and validation behavior does not grow a false kind.
func toolProviderFailure(message string, failure providerFailure) callToolResult {
	structured, err := json.Marshal(failure)
	if err != nil {
		// providerFailure has only fixed strings and a bool. Keep the established
		// text-only failure if that invariant is ever broken by a later change.
		return toolFailure(message)
	}
	result := toolFailure(message)
	result.StructuredContent = structured
	return result
}

// Fixed messages for arguments this server rejects before evaluating. None of
// them names a question, because naming one means quoting it.
const (
	msgBadArguments      = "the tool arguments must be a JSON object with exactly two members, state and questions"
	msgArgumentsTooLarge = "the tool arguments are larger than this server will evaluate"
	msgStateRequired     = "state is required"
	msgQuestionsRequired = "questions is required and must contain at least one question"
	msgQuestionIDInvalid = "every question id must be 1 to 64 characters using only letters, digits, underscore, hyphen and dot"
	msgQuestionFields    = "every question must be a JSON object with type, instructions and an optional criteria"
	msgQuestionType      = "every question type must be noul, choice or score"
	msgNoulCriteria      = `noul criteria must be an object with "true" and "false" descriptions, or be omitted`
	msgChoiceCriteria    = "choice criteria must be an object mapping each option to a description or null, naming at least one option"
	msgScoreCriteria     = "score criteria must be an array of at least two level descriptions, ordered from low to high"

	// fieldState and fieldInstructions label the two places a text value can
	// fail. They are literals so that a label can never become a quotation.
	fieldState        = "state"
	fieldInstructions = "question instructions"
)

// toolArguments is the wire shape of the evaluate arguments. Every value stays
// raw until it has been checked: decoding into any would lose the distinction
// between a JSON string and a JSON number written as a string, and would let an
// unbounded structure be materialised before it has been measured.
type toolArguments struct {
	State     json.RawMessage            `json:"state"`
	Questions map[string]json.RawMessage `json:"questions"`
}

type rawQuestion struct {
	Type         string          `json:"type"`
	Instructions json.RawMessage `json:"instructions"`
	Criteria     json.RawMessage `json:"criteria"`
}

// normalizeArguments validates a tool call and converts it into a request the
// API package will accept. It rejects here what can be explained here, and
// leaves the deeper bounds (nesting, per-field sizes, option counts) to
// typesafe.ValidateRequest, which owns them.
func normalizeArguments(raw json.RawMessage) (typesafe.Request, error) {
	if len(raw) > typesafe.MaxRequestBytes {
		return typesafe.Request{}, errors.New(msgArgumentsTooLarge)
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage("{}")
	}

	if !validStructure(raw) {
		return typesafe.Request{}, errors.New(msgBadArguments)
	}
	if _, err := jsonfields.Object(raw, "state", "questions"); err != nil {
		return typesafe.Request{}, errors.New(msgBadArguments)
	}
	var args toolArguments
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		// The decoder's own message quotes the offending field, which is
		// caller-controlled text of unbounded length. Say what the shape must
		// be instead.
		return typesafe.Request{}, errors.New(msgBadArguments)
	}
	if dec.More() {
		return typesafe.Request{}, errors.New(msgBadArguments)
	}

	state, err := textValue(args.State, fieldState)
	if err != nil {
		return typesafe.Request{}, err
	}
	if len(args.Questions) == 0 {
		return typesafe.Request{}, errors.New(msgQuestionsRequired)
	}
	if len(args.Questions) > typesafe.MaxQuestions {
		return typesafe.Request{}, fmt.Errorf("a request may carry at most %d questions", typesafe.MaxQuestions)
	}

	req := typesafe.Request{
		State:     state,
		Questions: make(map[string]typesafe.Question, len(args.Questions)),
	}
	// Sorted ids keep the reported error the same for the same arguments, which
	// matters when the caller is a model retrying a call it just made.
	for _, id := range slices.Sorted(maps.Keys(args.Questions)) {
		if !validQuestionID(id) {
			return typesafe.Request{}, errors.New(msgQuestionIDInvalid)
		}
		q, err := normalizeQuestion(args.Questions[id])
		if err != nil {
			return typesafe.Request{}, err
		}
		req.Questions[id] = q
	}

	if err := typesafe.ValidateRequest(req); err != nil {
		// The API package documents its errors as sanitized literals. Bounding
		// the text keeps that promise cheap to hold if that ever changes.
		return typesafe.Request{}, errors.New(clip(err.Error(), maxBorrowedErrorBytes))
	}
	return req, nil
}

func normalizeQuestion(raw json.RawMessage) (typesafe.Question, error) {
	if _, err := jsonfields.Object(raw, "type", "instructions", "criteria"); err != nil {
		return typesafe.Question{}, errors.New(msgQuestionFields)
	}
	var rq rawQuestion
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rq); err != nil {
		return typesafe.Question{}, errors.New(msgQuestionFields)
	}
	if dec.More() {
		return typesafe.Question{}, errors.New(msgQuestionFields)
	}

	instructions, err := textValue(rq.Instructions, fieldInstructions)
	if err != nil {
		return typesafe.Question{}, err
	}
	criteria, err := normalizeCriteria(rq.Type, rq.Criteria)
	if err != nil {
		return typesafe.Question{}, err
	}
	return typesafe.Question{Type: rq.Type, Instructions: instructions, Criteria: criteria}, nil
}

// normalizeCriteria enforces the criteria shape each question type requires.
// Unlike the upstream implementation this one rejects an unknown type outright:
// this server speaks to one pinned model whose three types it knows, and a
// typo that reached the API would come back as a validation error about a
// union branch the caller never wrote.
func normalizeCriteria(qType string, raw json.RawMessage) (json.RawMessage, error) {
	criteria := bytes.TrimSpace(raw)
	if bytes.Equal(criteria, []byte("null")) {
		criteria = nil
	}
	switch qType {
	case typesafe.TypeNoul:
		if len(criteria) == 0 {
			return nil, nil
		}
		if kindOf(criteria) != kindObject {
			return nil, fmt.Errorf("%s, but it is %s", msgNoulCriteria, jsonKind(criteria))
		}
	case typesafe.TypeChoice:
		if len(criteria) == 0 || kindOf(criteria) != kindObject {
			return nil, fmt.Errorf("%s, but it is %s", msgChoiceCriteria, jsonKind(criteria))
		}
		var options map[string]json.RawMessage
		if err := json.Unmarshal(criteria, &options); err != nil || len(options) == 0 {
			return nil, errors.New(msgChoiceCriteria)
		}
	case typesafe.TypeScore:
		if len(criteria) == 0 || kindOf(criteria) != kindArray {
			return nil, fmt.Errorf("%s, but it is %s", msgScoreCriteria, jsonKind(criteria))
		}
		var levels []json.RawMessage
		if err := json.Unmarshal(criteria, &levels); err != nil || len(levels) < 2 {
			return nil, errors.New(msgScoreCriteria)
		}
	default:
		return nil, errors.New(msgQuestionType)
	}
	return compact(criteria)
}

// textValue accepts the shapes the model reads as text: a non-empty string, an
// object, or an array. A number, boolean, or null is rejected here rather than
// at the API, so the caller learns which field was wrong. The field label is
// one of this package's own constants, never a value from the arguments.
func textValue(raw json.RawMessage, field string) (json.RawMessage, error) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 || bytes.Equal(value, []byte("null")) {
		if field == fieldState {
			return nil, errors.New(msgStateRequired)
		}
		return nil, fmt.Errorf("%s: this field is required", field)
	}
	switch kindOf(value) {
	case kindString:
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return nil, fmt.Errorf("%s: this field is not valid JSON text", field)
		}
		if strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("%s: this field must not be empty", field)
		}
	case kindObject, kindArray:
	default:
		return nil, fmt.Errorf("%s: this field must be a string, object, or array, but it is %s", field, jsonKind(value))
	}
	return compact(value)
}

// compact removes insignificant whitespace, so what is measured against the
// request budget is what will be sent.
func compact(raw json.RawMessage) (json.RawMessage, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, errors.New(msgBadArguments)
	}
	return json.RawMessage(buf.Bytes()), nil
}

// validQuestionID accepts the identifier alphabet the API package enforces:
// letters, digits, underscore, hyphen and dot, up to maxQuestionIDBytes. An id
// is the key an answer comes back under and the key a result is rendered with,
// so keeping it boring means it can be handled as data everywhere.
func validQuestionID(id string) bool {
	if len(id) == 0 || len(id) > maxQuestionIDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.':
		default:
			return false
		}
	}
	return true
}

type jsonKindCode int

const (
	kindUnknown jsonKindCode = iota
	kindNull
	kindObject
	kindArray
	kindString
	kindNumber
	kindBool
)

// kindOf names the JSON type of a raw value from its first byte, which the
// decoder has already proven well-formed.
func kindOf(raw json.RawMessage) jsonKindCode {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		return kindUnknown
	}
	switch value[0] {
	case '{':
		return kindObject
	case '[':
		return kindArray
	case '"':
		return kindString
	case 't', 'f':
		return kindBool
	case 'n':
		return kindNull
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		return kindNumber
	}
	return kindUnknown
}

// jsonKind describes a value the way the caller wrote it, never quoting it.
func jsonKind(raw json.RawMessage) string {
	switch kindOf(raw) {
	case kindNull:
		return "null"
	case kindObject:
		return "an object"
	case kindArray:
		return "an array"
	case kindString:
		return "a string"
	case kindNumber:
		return "a number"
	case kindBool:
		return "a boolean"
	}
	return "missing"
}

// maxBorrowedErrorBytes bounds text this package did not write itself.
const maxBorrowedErrorBytes = 200

// clip bounds a message and strips anything that is not printable, so a message
// cannot smuggle control characters or a newline into a result a user reads.
func clip(s string, limit int) string {
	var b strings.Builder
	for _, r := range s {
		if b.Len() >= limit {
			b.WriteString("...")
			break
		}
		if r < 0x20 || r == 0x7f {
			b.WriteByte(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
