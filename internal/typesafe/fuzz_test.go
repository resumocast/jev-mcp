package typesafe

import (
	"encoding/json"
	"testing"
)

func FuzzValidateRequest(f *testing.F) {
	f.Add([]byte(`{"state":{"count":3},"questions":{"q":{"type":"noul","instructions":"Is it urgent?"}}}`))
	f.Add([]byte(`{"state":"s","questions":{"q":{"type":"score","instructions":"Rate","criteria":["low","high"]}}}`))
	f.Add([]byte(`{"state":null}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxRequestBytes {
			return
		}
		var req Request
		if json.Unmarshal(data, &req) != nil {
			return
		}
		if ValidateRequest(req) == nil {
			body, err := encodeRequest(req)
			if err != nil || len(body) > MaxRequestBytes || !json.Valid(body) {
				t.Fatal("validated request did not produce bounded valid JSON")
			}
		}
	})
}

func FuzzParseResponse(f *testing.F) {
	f.Add([]byte(sampleResponse))
	f.Add([]byte(`{"model":"jev-1.13.0","answers":{},"usage":null}`))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxResponseBytes {
			return
		}
		specs := specsFor(t, sampleRequest(t))
		response, err := parseResponse(data, specs)
		if err == nil {
			if response.Model != Model || len(response.Answers) != len(specs) {
				t.Fatal("accepted response violated model or answer count")
			}
			if _, err := json.Marshal(response); err != nil {
				t.Fatal(err)
			}
		}
	})
}
