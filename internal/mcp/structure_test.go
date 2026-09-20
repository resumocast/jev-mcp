package mcp

import (
	"strings"
	"testing"
)

func TestStructureRejectsDuplicateMembersAndDeepInput(t *testing.T) {
	for _, input := range []string{
		`{"id":1,"id":2}`,
		`{"state":"s","questions":{"q":{"type":"noul","instructions":"a"},"q":{"type":"noul","instructions":"b"}}}`,
		`{"outer":{"type":"noul","type":"score"}}`,
		strings.Repeat("[", 18) + "0" + strings.Repeat("]", 18),
		`{"a":1} {"a":2}`,
	} {
		if validStructure([]byte(input)) {
			t.Fatalf("accepted ambiguous or excessive structure: %s", input)
		}
	}
	if !validStructure([]byte(`{"a":{"id":1},"b":{"id":2}}`)) {
		t.Fatal("distinct objects may reuse member names")
	}
}

func TestNormalizeArgumentsRejectsCaseAliases(t *testing.T) {
	for _, input := range []string{
		`{"State":"s","questions":{"q":{"type":"noul","instructions":"a"}}}`,
		`{"ſtate":"s","state":"t","questions":{"q":{"type":"noul","instructions":"a"}}}`,
		`{"state":"s","questions":{"q":{"type":"noul","Type":"score","instructions":"a"}}}`,
	} {
		if _, err := normalizeArguments([]byte(input)); err == nil {
			t.Fatal("accepted a non-canonical field")
		}
	}
}

func TestNormalizeArgumentsRejectsDuplicateQuestionIDs(t *testing.T) {
	input := `{"state":"s","questions":{"q":{"type":"noul","instructions":"a"},"q":{"type":"noul","instructions":"b"}}}`
	if _, err := normalizeArguments([]byte(input)); err == nil {
		t.Fatal("duplicate question ids must not be collapsed")
	}
}
