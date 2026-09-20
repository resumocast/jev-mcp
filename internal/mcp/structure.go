package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"unicode/utf8"
)

// validStructure rejects duplicate members before decoding into maps or structs.
// Sixteen levels leave room for the RPC wrapper plus eight-level tool values.
func validStructure(data []byte) bool {
	if len(data) > maxFrameBytes || !utf8.Valid(data) || !json.Valid(data) {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 16 {
			return false
		}
		token, err := dec.Token()
		if err != nil {
			return false
		}
		delim, container := token.(json.Delim)
		if !container {
			return true
		}
		if delim != '{' && delim != '[' {
			return false
		}
		seen := make(map[string]struct{})
		for dec.More() {
			if delim == '{' {
				key, err := dec.Token()
				if err != nil {
					return false
				}
				name, ok := key.(string)
				if !ok {
					return false
				}
				if _, duplicate := seen[name]; duplicate {
					return false
				}
				seen[name] = struct{}{}
			}
			if !walk(depth + 1) {
				return false
			}
		}
		end, err := dec.Token()
		return err == nil && (delim == '{' && end == json.Delim('}') || delim == '[' && end == json.Delim(']'))
	}
	if !walk(0) {
		return false
	}
	_, err := dec.Token()
	return err == io.EOF
}
