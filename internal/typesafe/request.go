package typesafe

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
)

// Request is one evaluation: a State to judge, and the questions to ask about
// it. The model is not a field. It is pinned to [Model] and added when the
// request is marshaled, so no caller can select a different one.
type Request struct {
	// State is the content to evaluate: a JSON string, object, or array. It
	// must be the evidence itself, not a conclusion about it. A verdict
	// asserted in State biases the answer toward it, and the confidence that
	// comes back is then agreement with the caller rather than independent
	// corroboration.
	State json.RawMessage `json:"state"`

	// Questions maps a caller-chosen id to a question. Answers come back under
	// the same ids. Ids are not sent to the model and carry no meaning for it,
	// so each question's Instructions must stand alone.
	Questions map[string]Question `json:"questions"`
}

// Question is one typed judgment about the request's state.
type Question struct {
	// Type is "noul", "choice", or "score".
	Type string `json:"type"`

	// Instructions is the judgment to make: a JSON string, object, or array.
	// It should name the condition to test, not the conclusion expected.
	Instructions json.RawMessage `json:"instructions"`

	// Criteria is the rubric, and its shape follows Type:
	//
	//	noul    optional object with "true" and/or "false" descriptions
	//	choice  required object mapping each option to a description or null
	//	score   required ordered array of 2 to 10 level descriptions
	//
	// Every description is a plain string in this version. See the package
	// documentation for why the structured form is out of scope for now.
	//
	// An omitted rubric is an absent field, not a JSON null. Sending null is
	// an error for all three types rather than a silent synonym for "omitted"
	// on one of them.
	Criteria json.RawMessage `json:"criteria,omitempty"`
}

// Question types.
const (
	TypeNoul   = "noul"
	TypeChoice = "choice"
	TypeScore  = "score"
)

// questionSpec is what validation learned about a question, kept so the
// response can be checked against the question that produced it.
//
// It holds the level descriptions rather than just their count, because the
// legend that comes back is rebuilt from them. Nothing a response says about
// what a level means is taken at face value.
type questionSpec struct {
	kind    string
	options map[string]struct{} // choice only
	levels  []string            // score only, in order
}

// ValidateRequest reports whether req is acceptable: well-formed, within every
// bound in limits.go, and shaped the way the API documents each question type.
//
// It is a pure check with no I/O. [Client.Evaluate] runs it too, so calling it
// first is only useful to reject bad input earlier, for instance to answer an
// MCP tool call without opening a connection.
func ValidateRequest(req Request) error {
	_, err := validateRequest(req)
	return err
}

func validateRequest(req Request) (map[string]questionSpec, error) {
	if err := scanText(req.State, maxStateBytes); err != nil {
		return nil, wrapField("state", err)
	}
	if len(req.Questions) == 0 {
		return nil, invalidRequest("at least one question is required")
	}
	if len(req.Questions) > MaxQuestions {
		return nil, invalidRequest("too many questions in one request")
	}

	// Sorted, not range-over-map: which question is reported first should not
	// depend on Go's map iteration order, or the same bad request would
	// produce a different message each time it is retried.
	specs := make(map[string]questionSpec, len(req.Questions))
	for _, id := range slices.Sorted(maps.Keys(req.Questions)) {
		if err := validateQuestionID(id); err != nil {
			return nil, err
		}
		spec, err := validateQuestion(req.Questions[id])
		if err != nil {
			return nil, err
		}
		specs[id] = spec
	}

	// The authoritative size is the size of what would go on the wire.
	if _, err := encodeRequest(req); err != nil {
		return nil, err
	}
	return specs, nil
}

// validateQuestionID keeps ids to an identifier alphabet. The id is the key the
// answer comes back under and the key an MCP result is rendered with; keeping
// it boring means it can be handled as data everywhere without quoting rules.
func validateQuestionID(id string) error {
	if id == "" {
		return invalidRequest("question id must not be empty")
	}
	if len(id) > maxQuestionIDBytes {
		return invalidRequest("question id is too long")
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_', c == '-', c == '.':
		default:
			return invalidRequest("question id may use only letters, digits, underscore, hyphen and dot")
		}
	}
	return nil
}

func validateQuestion(q Question) (questionSpec, error) {
	spec := questionSpec{kind: q.Type}

	switch q.Type {
	case TypeNoul, TypeChoice, TypeScore:
	default:
		return spec, invalidRequest(`question type must be "noul", "choice" or "score"`)
	}

	if err := scanText(q.Instructions, maxInstructionsBytes); err != nil {
		return spec, wrapField("question instructions", err)
	}

	criteria, err := criteriaValue(q.Criteria)
	if err != nil {
		return spec, err
	}

	switch q.Type {
	case TypeNoul:
		if err := validateNoulCriteria(criteria); err != nil {
			return spec, err
		}
	case TypeChoice:
		options, err := validateChoiceCriteria(criteria)
		if err != nil {
			return spec, err
		}
		spec.options = options
	case TypeScore:
		levels, err := validateScoreCriteria(criteria)
		if err != nil {
			return spec, err
		}
		spec.levels = levels
	}
	return spec, nil
}

// criteriaValue normalises the rubric field to "absent" or "present and
// structurally sound", and runs the structural scan that the typed decoding
// below cannot do for itself.
//
// The scan matters because encoding/json resolves a duplicate member
// last-wins. Decoding {"a": "x", "a": "y"} into a map yields one option and
// discards the other silently, so a rubric that reads as three options to the
// caller could be sent as two. The raw bytes are still here, so the ambiguity
// is rejected instead.
func criteriaValue(raw json.RawMessage) (json.RawMessage, error) {
	trimmed := json.RawMessage(bytes.TrimSpace(raw))
	if len(trimmed) == 0 {
		return nil, nil
	}
	if string(trimmed) == "null" {
		return nil, invalidRequest("criteria must be omitted rather than sent as null")
	}
	if _, err := scanJSON(trimmed, scanLimits{maxBytes: maxCriteriaBytes, maxDepth: maxJSONDepth}); err != nil {
		return nil, wrapField("criteria", asRequestError(err))
	}
	return trimmed, nil
}

// validateNoulCriteria accepts an omitted rubric, or an object carrying a
// description of what a yes means, what a no means, or both. Any other member
// name is rejected rather than ignored: a caller that wrote "yes"/"no" has a
// rubric the API will silently not apply, and silently is the problem.
func validateNoulCriteria(raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	members, err := decodeObject(raw, "noul criteria")
	if err != nil {
		return err
	}
	if len(members) == 0 {
		return invalidRequest(`noul criteria must describe "true", "false", or both, or be omitted`)
	}
	for _, name := range slices.Sorted(maps.Keys(members)) {
		if name != "true" && name != "false" {
			return invalidRequest(`noul criteria may contain only "true" and "false"`)
		}
		if err := validateDescription(members[name], "noul criteria"); err != nil {
			return err
		}
	}
	return nil
}

// validateChoiceCriteria requires the option map the API requires, and returns
// the option set so the answer can be checked against it.
func validateChoiceCriteria(raw json.RawMessage) (map[string]struct{}, error) {
	if raw == nil {
		return nil, invalidRequest("choice criteria are required: map each option to a description or null")
	}
	members, err := decodeObject(raw, "choice criteria")
	if err != nil {
		return nil, err
	}
	if len(members) < minChoiceOptions {
		return nil, invalidRequest("choice criteria must contain at least one option")
	}
	if len(members) > maxChoiceOptions {
		return nil, invalidRequest("choice criteria contain too many options")
	}
	options := make(map[string]struct{}, len(members))
	for _, name := range slices.Sorted(maps.Keys(members)) {
		if name == "" || len(name) > maxOptionBytes {
			return nil, invalidRequest("a choice option name is empty or too long")
		}
		// An option name comes back as the chosen answer and is rendered as a
		// label, so it is held to the stricter rule.
		if !safeLabel(name) {
			return nil, invalidRequest("a choice option name contains control characters")
		}
		// null is the documented way to say "this option needs no rubric".
		if string(bytes.TrimSpace(members[name])) != "null" {
			if err := validateDescription(members[name], "choice criteria"); err != nil {
				return nil, err
			}
		}
		options[name] = struct{}{}
	}
	return options, nil
}

// validateScoreCriteria requires the ordered level array and returns the level
// descriptions in order. The count is 2 to 10, per the score documentation;
// answers are 0-indexed across those levels, so the count and the text are both
// needed to make a returned score and its legend meaningful.
func validateScoreCriteria(raw json.RawMessage) ([]string, error) {
	if raw == nil {
		return nil, invalidRequest("score criteria are required: an ordered array of level descriptions")
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, invalidRequest("score criteria must be an array of level descriptions, ordered low to high")
	}
	if len(entries) < minScoreLevels {
		return nil, invalidRequest("score criteria must contain at least two levels")
	}
	if len(entries) > maxScoreLevels {
		return nil, invalidRequest("score criteria may contain at most ten levels")
	}
	levels := make([]string, 0, len(entries))
	for _, entry := range entries {
		if err := validateDescription(entry, "score criteria"); err != nil {
			return nil, err
		}
		var s string
		if err := json.Unmarshal(entry, &s); err != nil {
			return nil, wrapField("score criteria", invalidRequest("each level must be a string"))
		}
		levels = append(levels, s)
	}
	return levels, nil
}

// validateDescription accepts one rubric description.
//
// Descriptions are strings only in this version, which is the shape the API
// reference gives for criteria. The wider form documented in the primitive
// pages, where a level may be an object or an array, is deliberately out of
// scope; see the package documentation.
func validateDescription(raw json.RawMessage, field string) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return wrapField(field, invalidRequest("each description must be a string"))
	}
	if s == "" {
		return wrapField(field, invalidRequest("a description is empty"))
	}
	if len(s) > maxDescriptionBytes {
		return wrapField(field, invalidRequest("a description is too long"))
	}
	// A rubric description is echoed back as a legend label, so unlike state
	// it is held to the label rule.
	if !safeText(s) {
		return wrapField(field, invalidRequest("a description contains control characters"))
	}
	return nil
}

// decodeObject unmarshals a JSON object, rejecting every other kind.
func decodeObject(raw json.RawMessage, field string) (map[string]json.RawMessage, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return nil, wrapField(field, invalidRequest("value must be an object"))
	}
	if members == nil {
		return nil, wrapField(field, invalidRequest("value must be an object"))
	}
	return members, nil
}

// wireRequest is the body that actually goes out. Model sits here rather than
// in Request so that it is set in exactly one place.
type wireRequest struct {
	State     json.RawMessage     `json:"state"`
	Model     string              `json:"model"`
	Questions map[string]Question `json:"questions"`
}

// encodeRequest marshals the request and enforces MaxRequestBytes on the real
// body rather than on an estimate.
//
// HTML escaping is off. json.Marshal would rewrite every <, > and & as a
// six-byte Unicode escape, which is invisible to the service but inflates state
// made of HTML, XML or shell redirection by up to six times and could push a
// request over the limit for no reason. The size check runs on the final bytes,
// after encoding, so whatever the encoder does is what gets measured.
func encodeRequest(req Request) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(wireRequest{
		State:     req.State,
		Model:     Model,
		Questions: req.Questions,
	}); err != nil {
		return nil, invalidRequest("request could not be encoded as JSON")
	}
	// Encode appends a newline that the request body does not need.
	body := bytes.TrimRight(buf.Bytes(), "\n")
	if len(body) > MaxRequestBytes {
		return nil, ErrRequestTooLarge
	}
	return body, nil
}
