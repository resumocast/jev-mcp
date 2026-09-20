package typesafe

import (
	"errors"
	"strings"
	"testing"
)

func TestResponseRejectsCaseFoldedFieldAliases(t *testing.T) {
	for _, field := range []string{"model", "type", "input_tokens"} {
		t.Run(field, func(t *testing.T) {
			needle := `"` + field + `":`
			if !strings.Contains(sampleResponse, needle) {
				t.Fatal("fixture does not contain expected field")
			}
			body := strings.Replace(sampleResponse, needle, `"`+strings.ToUpper(field)+`":`, 1)
			if _, err := parseResponse([]byte(body), specsFor(t, sampleRequest(t))); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("non-canonical field was not rejected: %v", err)
			}
		})
	}
}
