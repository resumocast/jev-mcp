package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestHalfCloseFlushesFinalSynchronousResponse(t *testing.T) {
	var output bytes.Buffer
	server := NewServer(nil, Options{})
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"ping\"}\n")
	if err := server.Serve(context.Background(), input, &output); err != nil {
		t.Fatal(err)
	}
	var response struct {
		ID int `json:"id"`
	}
	if err := json.Unmarshal(output.Bytes(), &response); err != nil {
		t.Fatalf("final response lost: %v", err)
	}
	if response.ID != 7 {
		t.Fatalf("wrong response id: %d", response.ID)
	}
}

func TestDispatchRejectsNonCanonicalEnvelopeFields(t *testing.T) {
	for _, input := range []string{
		`{"jsonrpc":"2.0","id":1,"ID":2,"method":"ping"}`,
		`{"JSONRPC":"2.0","METHOD":"ping","ID":1}`,
	} {
		var output bytes.Buffer
		server := NewServer(nil, Options{})
		if err := server.Serve(context.Background(), strings.NewReader(input+"\n"), &output); err != nil {
			t.Fatal(err)
		}
		var message struct {
			ID    json.RawMessage `json:"id"`
			Error *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(output.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		if message.Error == nil || message.Error.Code != codeInvalidRequest || string(message.ID) != "null" {
			t.Fatal("non-canonical envelope was not rejected with a null id")
		}
	}
}

func TestParseIDRejectsInvalidUTF8(t *testing.T) {
	if _, ok := parseID(json.RawMessage{'"', 0xff, '"'}); ok {
		t.Fatal("invalid UTF-8 id accepted")
	}
}
