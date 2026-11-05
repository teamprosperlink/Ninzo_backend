// Package sensitive knows which JSON fields must never be stored in audit records, the outbox or
// stored replies.
package sensitive

import "encoding/json"

var fields = map[string]bool{
	"access_token":     true,
	"refresh_token":    true,
	"id_token":         true,
	"token":            true,
	"token_hash":       true,
	"password":         true,
	"new_password":     true,
	"current_password": true,
	"password_hash":    true,
	"otp":              true,
	"code_hash":        true,
	"handover_otp":     true,
	"push_token":       true,
}

// keptAsNull are required by the contract on replies, so a stored copy keeps them, empty.
var keptAsNull = map[string]bool{"handover_otp": true}

// Find returns the secret field names holding a value anywhere in a JSON value. A null holds no
// secret, so a full Order with no code yet is fine. Invalid JSON holds none that can be named.
func Find(raw []byte) []string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	seen := map[string]bool{}
	walk(v, func(m map[string]any, k string) {
		if fields[k] && m[k] != nil {
			seen[k] = true
		}
	})
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	return out
}

// Strip removes secret fields anywhere in a JSON value, or empties those the contract requires.
func Strip(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	walk(v, func(m map[string]any, k string) {
		switch {
		case keptAsNull[k]:
			m[k] = nil
		case fields[k]:
			delete(m, k)
		}
	})
	return json.Marshal(v)
}

func walk(v any, visit func(map[string]any, string)) {
	switch n := v.(type) {
	case map[string]any:
		for k, child := range n {
			visit(n, k)
			walk(child, visit)
		}
	case []any:
		for _, child := range n {
			walk(child, visit)
		}
	}
}
