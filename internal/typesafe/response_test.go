package typesafe

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestParseResponseAcceptsAWellFormedReply(t *testing.T) {
	specs := specsFor(t, sampleRequest(t))
	resp, err := parseResponse([]byte(sampleResponse), specs)
	if err != nil {
		t.Fatalf("sample response should parse, got %v", err)
	}
	if resp.Model != Model {
		t.Errorf("model = %q", resp.Model)
	}
	if len(resp.Answers) != 3 {
		t.Fatalf("got %d answers, want 3", len(resp.Answers))
	}

	var noul struct {
		Type string  `json:"type"`
		Noul float64 `json:"noul"`
	}
	if err := json.Unmarshal(resp.Answers["is_urgent"], &noul); err != nil {
		t.Fatalf("noul answer: %v", err)
	}
	if noul.Type != TypeNoul || noul.Noul != 0.92 {
		t.Errorf("noul answer = %+v", noul)
	}

	var usage struct {
		In  int64 `json:"input_tokens"`
		Out int64 `json:"output_tokens"`
	}
	if err := json.Unmarshal(resp.Usage, &usage); err != nil {
		t.Fatalf("usage: %v", err)
	}
	if usage.In != 312 || usage.Out != 48 {
		t.Errorf("usage = %+v", usage)
	}
}

// The pin has to hold in both directions. A reply naming another model is
// refused, not reported: accepting it would hand the service a free text field
// that travels into a tool result, and would defeat the version pin that exists
// so tuned thresholds keep meaning the same thing.
func TestParseResponseRequiresThePinnedModel(t *testing.T) {
	req := Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}}
	specs := specsFor(t, req)
	answer := `{"type":"noul","noul":0.5}`
	usage := `"usage":{"input_tokens":1,"output_tokens":1}`

	bad := []string{
		`{"answers":{"q":` + answer + `},` + usage + `}`,
		`{"model":"","answers":{"q":` + answer + `},` + usage + `}`,
		`{"model":null,"answers":{"q":` + answer + `},` + usage + `}`,
		`{"model":123,"answers":{"q":` + answer + `},` + usage + `}`,
		// An alias, a newer version, and a near miss are all refused.
		`{"model":"jev-latest","answers":{"q":` + answer + `},` + usage + `}`,
		`{"model":"jev-1.14.0","answers":{"q":` + answer + `},` + usage + `}`,
		`{"model":"jev-1.13.0 ","answers":{"q":` + answer + `},` + usage + `}`,
		`{"model":"` + leakMarker + `","answers":{"q":` + answer + `},` + usage + `}`,
	}
	for _, body := range bad {
		_, err := parseResponse([]byte(body), specs)
		if !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("body %s should be rejected, got %v", body, err)
		}
		assertNoLeak(t, err, leakMarker, "jev-1.14.0", "jev-latest")
	}

	good := `{"model":"` + Model + `","answers":{"q":` + answer + `},` + usage + `}`
	if _, err := parseResponse([]byte(good), specs); err != nil {
		t.Fatalf("the pinned model should parse, got %v", err)
	}
}

// Anything the service sends that this package does not recognise is dropped,
// rather than relayed into a tool result a model will read.
func TestParseResponseStripsUnrecognisedOutput(t *testing.T) {
	specs := specsFor(t, sampleRequest(t))
	body := `{
	  "model": "jev-1.13.0",
	  "unexpected_top_level": {"ignore": "` + leakMarker + `"},
	  "answers": {
	    "is_urgent": {"type": "noul", "noul": 0.5, "reasoning": "` + leakMarker + `", "confidence": 0.9},
	    "department": {"type":"choice","choice":"sales","probabilities":{"billing":0,"technical":0,"sales":1},"confidence":1,"debug":"` + leakMarker + `"},
	    "frustration": {"type":"score","score":0,"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},"probabilities":{"0":1,"1":0,"2":0},"confidence":1}
	  },
	  "usage": {"input_tokens": 1, "output_tokens": 2, "cost_usd": 0.01}
	}`

	resp, err := parseResponse([]byte(body), specs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	encoded, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), leakMarker) {
		t.Fatalf("unrecognised output survived: %s", encoded)
	}
	if strings.Contains(string(encoded), "cost_usd") {
		t.Errorf("unrecognised usage member survived: %s", encoded)
	}
	// A noul answer has no confidence of its own; the value is the
	// probability, so an added one is dropped rather than relayed.
	if strings.Contains(string(resp.Answers["is_urgent"]), "confidence") {
		t.Errorf("noul answer kept a confidence: %s", resp.Answers["is_urgent"])
	}
}

// The legend is the one free-text field in an answer. Its keys are checked
// against the question, and then its text is thrown away and rebuilt from the
// criteria that were sent, so a description can never be something the service
// invented.
func TestParseResponseRebuildsTheLegendFromTheRequest(t *testing.T) {
	req := Request{
		State: raw(`"s"`),
		Questions: map[string]Question{"q": {
			Type:         TypeScore,
			Instructions: raw(`"rate"`),
			Criteria:     raw(`["Calm","Frustrated","Very angry"]`),
		}},
	}
	specs := specsFor(t, req)

	tampered := `{"type":"score","score":1,` +
		`"legend":{"0":"` + leakMarker + `","1":"also ` + leakMarker + `","2":"still ` + leakMarker + `"},` +
		`"probabilities":{"0":0,"1":1,"2":0},"confidence":1}`

	resp, err := parseResponse([]byte(wrapAnswer("q", tampered)), specs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	answer := string(resp.Answers["q"])
	if strings.Contains(answer, leakMarker) {
		t.Fatalf("service-supplied legend text survived: %s", answer)
	}
	for _, want := range []string{"Calm", "Frustrated", "Very angry"} {
		if !strings.Contains(answer, want) {
			t.Errorf("legend is missing the requested description %q: %s", want, answer)
		}
	}
}

// A choice that is not the peak of its own distribution is internally
// inconsistent, and the two fields would disagree for anyone reading both.
func TestParseResponseRequiresChoiceToBeTheArgmax(t *testing.T) {
	specs := specsFor(t, choiceRequest(t))

	bad := `{"type":"choice","choice":"a","probabilities":{"a":0.2,"b":0.8},"confidence":0.6}`
	if _, err := parseResponse([]byte(wrapAnswer("q", bad)), specs); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}

	good := []string{
		`{"type":"choice","choice":"b","probabilities":{"a":0.2,"b":0.8},"confidence":0.6}`,
		// A tie is not an inconsistency; either side may be named.
		`{"type":"choice","choice":"a","probabilities":{"a":0.5,"b":0.5},"confidence":0.5}`,
		// A negligible floating-point difference is tolerated.
		`{"type":"choice","choice":"a","probabilities":{"a":0.4999999999,"b":0.5000000001},"confidence":0.5}`,
	}
	for _, answer := range good {
		if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); err != nil {
			t.Errorf("answer %s should parse, got %v", answer, err)
		}
	}
}

// The docs define the score as each level number multiplied by its probability,
// added up. A reply whose score and distribution tell different stories cannot
// be reconciled by any downstream reader.
func TestParseResponseRequiresScoreToMatchItsDistribution(t *testing.T) {
	specs := specsFor(t, scoreRequest(t))
	legend := `"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"}`

	// 0*0.05 + 1*0.30 + 2*0.65 = 1.6, the worked example from the API docs.
	good := `{"type":"score","score":1.6,` + legend + `,"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}`
	if _, err := parseResponse([]byte(wrapAnswer("q", good)), specs); err != nil {
		t.Fatalf("the documented example should parse, got %v", err)
	}

	bad := map[string]string{
		"score too high": `{"type":"score","score":2,` + legend + `,"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}`,
		"score too low":  `{"type":"score","score":0,` + legend + `,"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}`,
		"inverted":       `{"type":"score","score":0.4,` + legend + `,"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}`,
	}
	for name, answer := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}

	// A materially different mean must not be excused by guessed rounding.
	inconsistent := `{"type":"score","score":1.6,` + legend + `,"probabilities":{"0":0.05,"1":0.29,"2":0.66},"confidence":0.78}`
	if _, err := parseResponse([]byte(wrapAnswer("q", inconsistent)), specs); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("inconsistent distribution must fail, got %v", err)
	}
}

func TestNumericToleranceDoesNotGrowWithOptionCount(t *testing.T) {
	if scoreTolerance(10) > 0.001 || sumTolerance(255) > 0.001 {
		t.Fatal("numeric tolerance must remain bounded")
	}
	probabilities := make(map[string]json.RawMessage, 255)
	for i := 0; i < 255; i++ {
		probabilities[fmt.Sprint(i)] = raw(`0`)
	}
	if _, err := distribution(probabilities, 255, func(string) bool { return true }); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("an all-zero distribution must fail even at 255 options: %v", err)
	}
}

// encoding/json decodes the JSON string "0.92" into a json.Number, because
// json.Number is a string kind. A validator built on json.Number would accept a
// quoted number as a real one, so every numeric field is checked as raw bytes.
func TestParseResponseRejectsQuotedNumbers(t *testing.T) {
	noulSpecs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})
	choiceSpecs := specsFor(t, choiceRequest(t))
	scoreSpecs := specsFor(t, scoreRequest(t))
	legend := `"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"}`

	cases := []struct {
		name   string
		specs  map[string]questionSpec
		answer string
	}{
		{"noul", noulSpecs, `{"type":"noul","noul":"0.92"}`},
		{"choice probability", choiceSpecs, `{"type":"choice","choice":"b","probabilities":{"a":"0.2","b":0.8},"confidence":0.6}`},
		{"choice confidence", choiceSpecs, `{"type":"choice","choice":"b","probabilities":{"a":0.2,"b":0.8},"confidence":"0.6"}`},
		{"score", scoreSpecs, `{"type":"score","score":"1.6",` + legend + `,"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}`},
		{"score probability", scoreSpecs, `{"type":"score","score":1.6,` + legend + `,"probabilities":{"0":"0.05","1":0.3,"2":0.65},"confidence":0.78}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseResponse([]byte(wrapAnswer("q", tc.answer)), tc.specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}

	// Usage too, in both directions.
	body := `{"model":"` + Model + `","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":"312","output_tokens":48}}`
	if _, err := parseResponse([]byte(body), noulSpecs); !errors.Is(err, ErrInvalidResponse) {
		t.Fatalf("want ErrInvalidResponse, got %v", err)
	}
}

// Out of range means rejected. Clamping would invent a plausible number to
// stand in for one the service should not have sent, and the caller would have
// no way to tell the difference.
func TestParseResponseRejectsOutOfRangeProbabilitiesWithoutClamping(t *testing.T) {
	noulSpecs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})
	choiceSpecs := specsFor(t, choiceRequest(t))

	cases := []struct {
		name   string
		specs  map[string]questionSpec
		answer string
	}{
		{"noul just below zero", noulSpecs, `{"type":"noul","noul":-0.000001}`},
		{"noul just above one", noulSpecs, `{"type":"noul","noul":1.000001}`},
		{"noul far out", noulSpecs, `{"type":"noul","noul":-3}`},
		{"negative probability", choiceSpecs, `{"type":"choice","choice":"b","probabilities":{"a":-0.2,"b":1.2},"confidence":0.6}`},
		{"confidence above one", choiceSpecs, `{"type":"choice","choice":"b","probabilities":{"a":0.2,"b":0.8},"confidence":1.01}`},
		{"infinite noul", noulSpecs, `{"type":"noul","noul":1e400}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseResponse([]byte(wrapAnswer("q", tc.answer)), tc.specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}

	// The exact endpoints are values, not errors.
	for _, answer := range []string{`{"type":"noul","noul":0}`, `{"type":"noul","noul":1}`} {
		if _, err := parseResponse([]byte(wrapAnswer("q", answer)), noulSpecs); err != nil {
			t.Errorf("answer %s should parse, got %v", answer, err)
		}
	}
}

// A duplicate member would let the service state two values for one field and
// leave encoding/json to pick the second.
func TestParseResponseRejectsDuplicateMembers(t *testing.T) {
	specs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})
	bodies := []string{
		`{"model":"` + Model + `","model":"other","answers":{"q":{"type":"noul","noul":0.5}},"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"model":"` + Model + `","answers":{"q":{"type":"noul","noul":0.5},"q":{"type":"noul","noul":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`,
		`{"model":"` + Model + `","answers":{"q":{"type":"noul","noul":0.5,"noul":0.9}},"usage":{"input_tokens":1,"output_tokens":1}}`,
	}
	for _, body := range bodies {
		if _, err := parseResponse([]byte(body), specs); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("body %s should be rejected, got %v", body, err)
		}
	}
}

func TestParseResponseRequiresExactlyTheQuestionsAsked(t *testing.T) {
	specs := specsFor(t, sampleRequest(t))
	choice := `{"type":"choice","choice":"sales","probabilities":{"billing":0,"technical":0,"sales":1},"confidence":1}`
	score := `{"type":"score","score":0,"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"},"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`
	usage := `"usage":{"input_tokens":1,"output_tokens":1}`

	cases := map[string]string{
		"missing an answer": `{"model":"` + Model + `","answers":{"is_urgent":{"type":"noul","noul":0.5}},` + usage + `}`,
		"extra answer": `{"model":"` + Model + `","answers":{"is_urgent":{"type":"noul","noul":0.5},` +
			`"department":` + choice + `,"frustration":` + score + `,"uninvited":{"type":"noul","noul":0.5}},` + usage + `}`,
		"renamed answer": `{"model":"` + Model + `","answers":{"is_urgent_typo":{"type":"noul","noul":0.5},` +
			`"department":` + choice + `,"frustration":` + score + `},` + usage + `}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResponse([]byte(body), specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}
}

func TestParseResponseNoulAnswers(t *testing.T) {
	specs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})

	good := []string{`{"type":"noul","noul":0}`, `{"type":"noul","noul":1}`, `{"type":"noul","noul":0.5}`}
	bad := []string{
		`{"type":"noul"}`,
		`{"type":"noul","noul":null}`,
		`{"type":"noul","noul":true}`,
		`{"type":"choice","noul":0.5}`,
		`{"noul":0.5}`,
		`"just a string"`,
	}
	for _, answer := range good {
		if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); err != nil {
			t.Errorf("answer %s should parse, got %v", answer, err)
		}
	}
	for _, answer := range bad {
		if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); !errors.Is(err, ErrInvalidResponse) {
			t.Errorf("answer %s should be rejected, got %v", answer, err)
		}
	}
}

func TestParseResponseChoiceAnswers(t *testing.T) {
	specs := specsFor(t, choiceRequest(t))

	good := []string{
		`{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.4},"confidence":0.7}`,
		`{"type":"choice","choice":"b","probabilities":{"a":0.0,"b":1.0},"confidence":1}`,
	}
	bad := map[string]string{
		"option not offered":  `{"type":"choice","choice":"c","probabilities":{"a":0.6,"b":0.4},"confidence":0.7}`,
		"missing choice":      `{"type":"choice","probabilities":{"a":0.6,"b":0.4},"confidence":0.7}`,
		"extra probability":   `{"type":"choice","choice":"a","probabilities":{"a":0.5,"b":0.4,"c":0.1},"confidence":0.7}`,
		"missing probability": `{"type":"choice","choice":"a","probabilities":{"a":1.0},"confidence":0.7}`,
		"renamed probability": `{"type":"choice","choice":"a","probabilities":{"a":0.6,"z":0.4},"confidence":0.7}`,
		"does not sum to 1":   `{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.1},"confidence":0.7}`,
		"no probabilities":    `{"type":"choice","choice":"a","confidence":0.7}`,
		"no confidence":       `{"type":"choice","choice":"a","probabilities":{"a":0.6,"b":0.4}}`,
		"wrong type":          `{"type":"score","choice":"a","probabilities":{"a":0.6,"b":0.4},"confidence":0.7}`,
	}
	for _, answer := range good {
		if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); err != nil {
			t.Errorf("answer %s should parse, got %v", answer, err)
		}
	}
	for name, answer := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}
}

func TestParseResponseScoreAnswers(t *testing.T) {
	specs := specsFor(t, scoreRequest(t))
	legend := `"legend":{"0":"Calm","1":"Frustrated","2":"Very angry"}`

	good := []string{
		`{"type":"score","score":1.6,` + legend + `,"probabilities":{"0":0.05,"1":0.3,"2":0.65},"confidence":0.78}`,
		`{"type":"score","score":0,` + legend + `,"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		`{"type":"score","score":2,` + legend + `,"probabilities":{"0":0,"1":0,"2":1},"confidence":1}`,
	}

	// Three levels are 0..2, so a score of 3 is off the end. Getting that
	// wrong is the classic N-versus-N-1 misread of a 0-indexed score.
	bad := map[string]string{
		"score past the last level": `{"type":"score","score":3,` + legend + `,"probabilities":{"0":0,"1":0,"2":1},"confidence":1}`,
		"negative score":            `{"type":"score","score":-1,` + legend + `,"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		"missing score":             `{"type":"score",` + legend + `,"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		"legend too short":          `{"type":"score","score":0,"legend":{"0":"Calm","1":"Frustrated"},"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		"legend keyed from 1":       `{"type":"score","score":0,"legend":{"1":"a","2":"b","3":"c"},"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		"legend key not a number":   `{"type":"score","score":0,"legend":{"0":"a","1":"b","x":"c"},"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		"legend value not a string": `{"type":"score","score":0,"legend":{"0":"a","1":2,"2":"c"},"probabilities":{"0":1,"1":0,"2":0},"confidence":1}`,
		"probabilities too short":   `{"type":"score","score":0,` + legend + `,"probabilities":{"0":0.5,"1":0.5},"confidence":1}`,
		"probabilities off by one":  `{"type":"score","score":0,` + legend + `,"probabilities":{"1":0.05,"2":0.3,"3":0.65},"confidence":1}`,
		"does not sum to 1":         `{"type":"score","score":0,` + legend + `,"probabilities":{"0":0.1,"1":0.1,"2":0.1},"confidence":1}`,
		"missing confidence":        `{"type":"score","score":0,` + legend + `,"probabilities":{"0":1,"1":0,"2":0}}`,
	}
	for _, answer := range good {
		if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); err != nil {
			t.Errorf("answer %s should parse, got %v", answer, err)
		}
	}
	for name, answer := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResponse([]byte(wrapAnswer("q", answer)), specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}
}

func TestParseResponseUsage(t *testing.T) {
	specs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})
	head := `{"model":"` + Model + `","answers":{"q":{"type":"noul","noul":0.5}}`

	bad := map[string]string{
		"missing":       head + `}`,
		"null":          head + `,"usage":null}`,
		"missing input": head + `,"usage":{"output_tokens":1}}`,
		"negative":      head + `,"usage":{"input_tokens":-1,"output_tokens":1}}`,
		"fractional":    head + `,"usage":{"input_tokens":1.5,"output_tokens":1}}`,
		"exponent":      head + `,"usage":{"input_tokens":1e3,"output_tokens":1}}`,
		"absurd":        head + `,"usage":{"input_tokens":99999999999999999,"output_tokens":1}}`,
		"string":        head + `,"usage":{"input_tokens":"312","output_tokens":1}}`,
	}
	for name, body := range bad {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResponse([]byte(body), specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}

	t.Run("zero is allowed", func(t *testing.T) {
		if _, err := parseResponse([]byte(head+`,"usage":{"input_tokens":0,"output_tokens":0}}`), specs); err != nil {
			t.Fatalf("zero usage should parse, got %v", err)
		}
	})
}

func TestParseResponseMalformedBodies(t *testing.T) {
	specs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})

	cases := map[string]string{
		"empty":            ``,
		"not JSON":         `<html>error</html>`,
		"truncated":        `{"model":"jev-1.13.0","answers":`,
		"array":            `[]`,
		"bare string":      `"ok"`,
		"no answers":       `{"model":"jev-1.13.0","usage":{"input_tokens":1,"output_tokens":1}}`,
		"answers is array": `{"model":"jev-1.13.0","answers":[],"usage":{"input_tokens":1,"output_tokens":1}}`,
		"invalid UTF-8":    "{\"model\":\"jev-1.13.0\",\"answers\":{\"q\":{\"type\":\"noul\",\"noul\":0.5}},\"usage\":{\"input_tokens\":1,\"output_tokens\":1},\"x\":\"\xff\"}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseResponse([]byte(body), specs); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("want ErrInvalidResponse, got %v", err)
			}
		})
	}
}

func TestParseResponseRejectsOversizedBody(t *testing.T) {
	specs := specsFor(t, sampleRequest(t))
	body := make([]byte, MaxResponseBytes+1)
	if _, err := parseResponse(body, specs); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("want ErrResponseTooLarge, got %v", err)
	}
}

// Response errors are surfaced to a model, so they must not become a channel
// for whatever the service put in the body.
func TestParseResponseErrorsDoNotQuoteTheBody(t *testing.T) {
	specs := specsFor(t, Request{State: raw(`"s"`), Questions: map[string]Question{"q": noulQuestion(t)}})

	bodies := []string{
		`{"error":"` + leakMarker + `"}`,
		`{"model":"` + Model + `","answers":{"q":{"type":"noul","noul":"` + leakMarker + `"}},"usage":{"input_tokens":1,"output_tokens":1}}`,
		`<html><body>` + leakMarker + `</body></html>`,
		`{"model":"` + leakMarker + ` ` + repeat(200) + `","answers":{},"usage":{}}`,
	}
	for _, body := range bodies {
		_, err := parseResponse([]byte(body), specs)
		assertNoLeak(t, err, leakMarker)
	}
}

// Answers are re-encoded from validated values, so the same reply always
// produces the same bytes regardless of how the service ordered or spaced it.
func TestParseResponseOutputIsCanonical(t *testing.T) {
	specs := specsFor(t, sampleRequest(t))
	first, err := parseResponse([]byte(sampleResponse), specs)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, []byte(sampleResponse)); err != nil {
		t.Fatalf("compact: %v", err)
	}
	second, err := parseResponse(compact.Bytes(), specs)
	if err != nil {
		t.Fatalf("parse compact: %v", err)
	}
	for id := range first.Answers {
		if string(first.Answers[id]) != string(second.Answers[id]) {
			t.Errorf("answer %s differs: %s vs %s", id, first.Answers[id], second.Answers[id])
		}
	}
}

// Helpers.

func choiceRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		State: raw(`"s"`),
		Questions: map[string]Question{"q": {
			Type:         TypeChoice,
			Instructions: raw(`"which"`),
			Criteria:     raw(`{"a":"first","b":"second"}`),
		}},
	}
}

func scoreRequest(t *testing.T) Request {
	t.Helper()
	return Request{
		State: raw(`"s"`),
		Questions: map[string]Question{"q": {
			Type:         TypeScore,
			Instructions: raw(`"rate"`),
			Criteria:     raw(`["Calm","Frustrated","Very angry"]`),
		}},
	}
}

// wrapAnswer builds a complete response body around a single answer.
func wrapAnswer(id, answer string) string {
	return fmt.Sprintf(`{"model":%q,"answers":{%q:%s},"usage":{"input_tokens":1,"output_tokens":1}}`, Model, id, answer)
}
