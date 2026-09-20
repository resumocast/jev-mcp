package typesafe

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"unicode/utf8"

	"jev-mcp/internal/jsonfields"
)

// Response is a validated evaluation result.
//
// Answers and Usage are json.RawMessage, but they are not the bytes the service
// sent. Every field was parsed, checked against the question that produced it,
// checked for internal consistency, and re-encoded from the checked values.
// Anything the service returned that this package does not recognise was
// dropped on the way through, and the one free-text field in an answer, a score
// legend, is rebuilt from the request rather than copied from the reply. That
// is the point: the result is forwarded into an MCP tool result that a model
// reads, so it must not be a channel for arbitrary bytes from the network.
type Response struct {
	// Model is the model that answered. It always equals [Model]: a reply
	// naming anything else is rejected rather than reported. The field is kept
	// so the value travels with the result, not because it can vary.
	Model string `json:"model"`

	// Answers holds one answer per question, under the same ids that were
	// sent. Every requested id is present and no others are.
	Answers map[string]json.RawMessage `json:"answers"`

	// Usage is {"input_tokens": n, "output_tokens": n}, both non-negative.
	Usage json.RawMessage `json:"usage"`
}

// Fixed tolerances accommodate small serialization differences without
// weakening validation as the option count grows. Documentation examples do
// not establish that the service rounds its actual responses to two decimals.
const argmaxTolerance = 1e-9

func sumTolerance(_ int) float64 { return 1e-3 }

func scoreTolerance(_ int) float64 { return 1e-3 }

// answerOut is the sanitized shape of one answer. Fields are pointers or
// omitempty so that each answer type emits exactly its own fields and nothing
// else, regardless of what arrived.
type answerOut struct {
	Type          string             `json:"type"`
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Score         *float64           `json:"score,omitempty"`
	Legend        map[string]string  `json:"legend,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    *float64           `json:"confidence,omitempty"`
}

// answerIn is the permissive shape used for decoding. Unknown members are
// ignored by encoding/json, which is how unrecognised output gets stripped
// instead of forwarded.
//
// Every numeric member is decoded as json.RawMessage, not json.Number. This is
// not a style choice. json.Number is a string kind, so encoding/json will
// happily decode the JSON string "0.92" into one, and a validator built on it
// would accept {"noul": "0.92"} as a number. Holding the raw bytes lets
// jsonFloat insist on an actual JSON number literal.
type answerIn struct {
	Type          *string                    `json:"type"`
	Noul          json.RawMessage            `json:"noul"`
	Choice        *string                    `json:"choice"`
	Score         json.RawMessage            `json:"score"`
	Legend        map[string]json.RawMessage `json:"legend"`
	Probabilities map[string]json.RawMessage `json:"probabilities"`
	Confidence    json.RawMessage            `json:"confidence"`
}

type usageIn struct {
	Input  json.RawMessage `json:"input_tokens"`
	Output json.RawMessage `json:"output_tokens"`
}

type bodyIn struct {
	Model   *string                    `json:"model"`
	Answers map[string]json.RawMessage `json:"answers"`
	Usage   *usageIn                   `json:"usage"`
}

// parseResponse validates a response body against the questions that were
// asked and returns the sanitized result.
//
// specs is what makes the check meaningful: without the question that produced
// an answer there is no way to tell whether a choice picked a real option,
// whether a score landed on a level that exists, or what a legend entry is
// supposed to say.
func parseResponse(payload []byte, specs map[string]questionSpec) (Response, error) {
	var zero Response
	if len(payload) == 0 {
		return zero, invalidResponse("response is empty")
	}
	if len(payload) > MaxResponseBytes {
		return zero, ErrResponseTooLarge
	}
	if !utf8.Valid(payload) {
		return zero, invalidResponse("response is not valid UTF-8")
	}
	// The same structural scan the request gets, for the same reason: a
	// duplicate member name would let the service state two different values
	// for one field and leave encoding/json to pick.
	kind, err := scanJSON(payload, scanLimits{maxBytes: MaxResponseBytes, maxDepth: maxJSONDepth})
	if err != nil {
		return zero, asResponseError(err)
	}
	if kind != topObject {
		return zero, invalidResponse("response is not an object")
	}

	fields, err := jsonfields.Object(payload, "model", "answers", "usage")
	if err != nil {
		return zero, invalidResponse("response has non-canonical fields")
	}
	if _, err := jsonfields.Object(fields["usage"], "input_tokens", "output_tokens"); err != nil {
		return zero, invalidResponse("response usage is not the expected object")
	}
	var body bodyIn
	if err := json.Unmarshal(payload, &body); err != nil {
		return zero, invalidResponse("response is not the expected object")
	}

	// The model pin fails closed. Accepting any plausible-looking string here
	// would hand the service a free text field that travels all the way into a
	// tool result, and would quietly defeat the pin that exists so answers
	// cannot change underneath tuned thresholds.
	if body.Model == nil || *body.Model != Model {
		return zero, invalidResponse("response was not produced by the pinned model")
	}

	if body.Answers == nil {
		return zero, invalidResponse("response is missing answers")
	}
	if len(body.Answers) != len(specs) {
		return zero, invalidResponse("response does not answer exactly the questions that were asked")
	}

	answers := make(map[string]json.RawMessage, len(specs))
	for id, spec := range specs {
		raw, ok := body.Answers[id]
		if !ok {
			return zero, invalidResponse("response does not answer exactly the questions that were asked")
		}
		out, err := parseAnswer(raw, spec)
		if err != nil {
			return zero, err
		}
		encoded, err := json.Marshal(out)
		if err != nil {
			return zero, invalidResponse("an answer could not be re-encoded")
		}
		answers[id] = encoded
	}

	usage, err := parseUsage(body.Usage)
	if err != nil {
		return zero, err
	}

	return Response{Model: Model, Answers: answers, Usage: usage}, nil
}

func parseAnswer(raw json.RawMessage, spec questionSpec) (answerOut, error) {
	var zero answerOut
	var in answerIn
	if err := jsonfields.Unmarshal(raw, &in, "type", "noul", "choice", "score", "legend", "probabilities", "confidence"); err != nil {
		// json's own message quotes the offending value; never surface it.
		return zero, invalidResponse("an answer is not the expected object")
	}
	if in.Type == nil || *in.Type != spec.kind {
		return zero, invalidResponse("an answer does not match the type of its question")
	}

	out := answerOut{Type: spec.kind}
	switch spec.kind {
	case TypeNoul:
		// No confidence on a noul answer: the value is itself the probability,
		// so anything the service adds here is dropped rather than relayed.
		v, ok := probability(in.Noul)
		if !ok {
			return zero, invalidResponse("a noul answer is missing a probability between 0 and 1")
		}
		out.Noul = &v

	case TypeChoice:
		if in.Choice == nil {
			return zero, invalidResponse("a choice answer is missing the chosen option")
		}
		if _, ok := spec.options[*in.Choice]; !ok {
			return zero, invalidResponse("a choice answer picked an option that was not offered")
		}
		out.Choice = *in.Choice

		probs, err := distribution(in.Probabilities, len(spec.options), func(key string) bool {
			_, ok := spec.options[key]
			return ok
		})
		if err != nil {
			return zero, err
		}
		// The API defines choice as "the highest-probability option". A reply
		// whose named choice is not the peak of its own distribution is
		// internally inconsistent, and the two fields would disagree for any
		// caller that reads both.
		best := 0.0
		for _, p := range probs {
			best = math.Max(best, p)
		}
		if probs[*in.Choice] < best-argmaxTolerance {
			return zero, invalidResponse("a choice answer did not pick the highest-probability option")
		}
		out.Probabilities = probs

		c, ok := probability(in.Confidence)
		if !ok {
			return zero, invalidResponse("a choice answer is missing a confidence between 0 and 1")
		}
		out.Confidence = &c

	case TypeScore:
		levels := len(spec.levels)
		// Levels are 0-indexed, so n levels use the keys "0" through "n-1".
		isLevel := func(key string) bool {
			n, err := strconv.Atoi(key)
			return err == nil && n >= 0 && n < levels && strconv.Itoa(n) == key
		}

		score, ok := jsonFloat(in.Score)
		if !ok {
			return zero, invalidResponse("a score answer is missing a numeric score")
		}
		if score < 0 || score > float64(levels-1) {
			return zero, invalidResponse("a score answer is outside the range of its levels")
		}

		// The legend is validated for shape and then discarded. Its text is
		// rebuilt from the criteria that were sent, so a level description can
		// never be something the service made up: this is the one part of an
		// answer that would otherwise be free-form remote text.
		if len(in.Legend) != levels {
			return zero, invalidResponse("a score answer's legend does not match its levels")
		}
		for key, text := range in.Legend {
			if !isLevel(key) {
				return zero, invalidResponse("a score answer's legend does not match its levels")
			}
			var s string
			if err := json.Unmarshal(text, &s); err != nil {
				return zero, invalidResponse("a score answer's legend entry is not a string")
			}
		}
		legend := make(map[string]string, levels)
		for i, description := range spec.levels {
			legend[strconv.Itoa(i)] = description
		}
		out.Legend = legend

		probs, err := distribution(in.Probabilities, levels, isLevel)
		if err != nil {
			return zero, err
		}
		// The docs define the score as each level number multiplied by its
		// probability, added up. Checking it costs nothing and catches a reply
		// whose score and distribution tell different stories, which no
		// downstream reader could reconcile.
		weighted := 0.0
		for key, p := range probs {
			n, err := strconv.Atoi(key)
			if err != nil {
				return zero, invalidResponse("a probability distribution does not cover exactly the expected keys")
			}
			weighted += float64(n) * p
		}
		if math.Abs(score-weighted) > scoreTolerance(levels) {
			return zero, invalidResponse("a score answer disagrees with its own probability distribution")
		}
		out.Score = &score
		out.Probabilities = probs

		c, ok := probability(in.Confidence)
		if !ok {
			return zero, invalidResponse("a score answer is missing a confidence between 0 and 1")
		}
		out.Confidence = &c
	}
	return out, nil
}

// distribution validates a probability map: exactly the expected keys, every
// value a real probability, and the whole thing summing to 1.
func distribution(in map[string]json.RawMessage, want int, valid func(string) bool) (map[string]float64, error) {
	if len(in) != want {
		return nil, invalidResponse("a probability distribution does not cover exactly the expected keys")
	}
	out := make(map[string]float64, want)
	sum := 0.0
	for key, raw := range in {
		if !valid(key) {
			return nil, invalidResponse("a probability distribution does not cover exactly the expected keys")
		}
		v, ok := probability(raw)
		if !ok {
			return nil, invalidResponse("a probability is not a number between 0 and 1")
		}
		out[key] = v
		sum += v
	}
	if math.Abs(sum-1) > sumTolerance(want) {
		return nil, invalidResponse("a probability distribution does not sum to 1")
	}
	return out, nil
}

// jsonFloat decodes a JSON number literal.
//
// The first-byte test is what makes this strict. In JSON only a number can
// begin with a digit or a minus sign, so anything else, including the string
// "0.92" that json.Number would have accepted, is refused before it is parsed.
// A literal with a huge exponent parses to an infinity, which is the one way a
// well-formed JSON document can smuggle in a value that is not a real number.
func jsonFloat(raw json.RawMessage) (float64, bool) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return 0, false
	}
	if b[0] != '-' && (b[0] < '0' || b[0] > '9') {
		return 0, false
	}
	if !json.Valid(b) {
		return 0, false
	}
	v, err := strconv.ParseFloat(string(b), 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}

// probability is jsonFloat restricted to [0, 1], with no clamping.
//
// A value outside the range is rejected rather than pulled back to the nearest
// end. Clamping would invent a plausible number to stand in for one the service
// should not have sent, and a caller reading the result would have no way to
// tell the difference.
func probability(raw json.RawMessage) (float64, bool) {
	v, ok := jsonFloat(raw)
	if !ok {
		return 0, false
	}
	if v < 0 || v > 1 {
		return 0, false
	}
	return v, true
}

type usageOut struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

func parseUsage(in *usageIn) (json.RawMessage, error) {
	if in == nil {
		return nil, invalidResponse("response is missing usage")
	}
	input, ok := tokenCount(in.Input)
	if !ok {
		return nil, invalidResponse("response reports an unusable input token count")
	}
	output, ok := tokenCount(in.Output)
	if !ok {
		return nil, invalidResponse("response reports an unusable output token count")
	}
	encoded, err := json.Marshal(usageOut{InputTokens: input, OutputTokens: output})
	if err != nil {
		return nil, invalidResponse("usage could not be re-encoded")
	}
	return encoded, nil
}

// tokenCount requires a non-negative JSON integer literal. ParseInt rejects a
// fractional or exponent form outright, so no rounding decision has to be made
// here, and the same first-byte test keeps a quoted count from passing.
func tokenCount(raw json.RawMessage) (int64, bool) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 {
		return 0, false
	}
	if b[0] != '-' && (b[0] < '0' || b[0] > '9') {
		return 0, false
	}
	v, err := strconv.ParseInt(string(b), 10, 64)
	if err != nil || v < 0 || v > maxTokenCount {
		return 0, false
	}
	return v, true
}
