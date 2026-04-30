package agent

import (
	"bytes"
	"encoding/json"
)

// canonicalJSON re-marshals JSON in a deterministic form so two configs that
// differ only in whitespace, key ordering, or trailing newlines compare equal.
//
// `json.Marshal` already sorts map keys alphabetically; UseNumber preserves
// integer/float values byte-for-byte through the round trip rather than
// reformatting them.
func canonicalJSON(in []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// configEqualCanonical reports whether two JSON byte slices are semantically
// identical. If either side fails to parse, it returns false (the caller should
// treat that as "must apply" rather than "skip").
func configEqualCanonical(a, b []byte) bool {
	ca, err := canonicalJSON(a)
	if err != nil {
		return false
	}
	cb, err := canonicalJSON(b)
	if err != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}
