package jsonfields

import "testing"

func TestCanonicalFieldsRejectAliasesWithoutChangingDataKeys(t *testing.T) {
	for _, raw := range []string{
		`{"id":1,"ID":2}`, `{"State":"a"}`, `{"ſtate":"a","state":"b"}`,
		`{"k":1,"K":2}`, `null`, `[]`,
	} {
		if _, err := Object([]byte(raw), "id", "state", "k"); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if _, err := Object([]byte(`{"state":{"A":1,"a":2},"custom":7}`), "state"); err != nil {
		t.Fatal("arbitrary nested data and unknown metadata must remain untouched", err)
	}
}
