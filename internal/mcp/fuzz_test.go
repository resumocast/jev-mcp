package mcp

import (
	"encoding/json"
	"testing"

	"jev-mcp/internal/typesafe"
)

func FuzzNormalizeArguments(f *testing.F) {
	f.Add([]byte(`{"state":"ticket","questions":{"q":{"type":"noul","instructions":"Is it urgent?"}}}`))
	f.Add([]byte(`{"state":{},"questions":{}}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > maxFrameBytes {
			return
		}
		req, err := normalizeArguments(json.RawMessage(data))
		if err == nil && typesafe.ValidateRequest(req) != nil {
			t.Fatal("normalized arguments must satisfy API request validation")
		}
	})
}
