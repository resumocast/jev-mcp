// Package jsonfields guards struct-shaped JSON against encoding/json's implicit
// case folding. Arbitrary caller-owned object keys remain case-sensitive data.
package jsonfields

import (
	"encoding/json"
	"errors"
	"strings"
)

// Object requires an object and rejects non-canonical spellings of known fields.
// Unknown fields are left to the caller's policy. Duplicate-member detection and
// byte/depth bounds belong to the surrounding structural scanner.
func Object(raw []byte, names ...string) (map[string]json.RawMessage, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, errors.New("expected a JSON object")
	}
	for key := range fields {
		for _, name := range names {
			if key != name && strings.EqualFold(key, name) {
				return nil, errors.New("non-canonical JSON field name")
			}
		}
	}
	return fields, nil
}

// Unmarshal retains encoding/json's value decoding but not its field aliases.
func Unmarshal(raw []byte, out any, names ...string) error {
	if _, err := Object(raw, names...); err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}
