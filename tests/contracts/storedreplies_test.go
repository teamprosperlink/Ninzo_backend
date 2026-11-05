package contracts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// A reply to a request with an Idempotency-Key is stored in idempotency_keys, so it must
// never hold a token, password or code.
//
// One exception: handover_otp is part of the customer Order, which stored replies return. It
// is not listed here. The data map's rule is that the stored copy must leave it out.
var secretsInStoredReplies = map[string]bool{
	"access_token": true, "refresh_token": true, "password": true,
	"new_password": true, "current_password": true, "otp": true,
}

func findSecrets(root, node any, seen map[string]bool, out map[string]bool) {
	switch n := node.(type) {
	case []any:
		for _, v := range n {
			findSecrets(root, v, seen, out)
		}
	case map[string]any:
		if ref, ok := n["$ref"].(string); ok {
			if !seen[ref] {
				seen[ref] = true
				var cur = root
				for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
					m, _ := cur.(map[string]any)
					cur = m[part]
				}
				findSecrets(root, cur, seen, out)
			}
			return
		}
		if props, ok := n["properties"].(map[string]any); ok {
			for name := range props {
				if secretsInStoredReplies[name] {
					out[name] = true
				}
			}
		}
		for _, v := range n {
			findSecrets(root, v, seen, out)
		}
	}
}

func TestStoredRepliesHoldNoSecrets(t *testing.T) {
	checked := 0
	for _, file := range []string{"customer.openapi.yaml", "rider.openapi.yaml"} {
		raw, err := os.ReadFile(filepath.Join(contractsDir, "dist", file))
		if err != nil {
			t.Fatal(err)
		}
		var root map[string]any
		if err := yaml.Unmarshal(raw, &root); err != nil {
			t.Fatal(err)
		}
		paths, _ := root["paths"].(map[string]any)
		for path, item := range paths {
			methods, _ := item.(map[string]any)
			for method, opNode := range methods {
				op, _ := opNode.(map[string]any)
				params := map[string]bool{}
				findParamNames(root, op["parameters"], params)
				if !params["Idempotency-Key"] {
					continue
				}
				checked++
				responses, _ := op["responses"].(map[string]any)
				for code, resp := range responses {
					if !strings.HasPrefix(code, "2") {
						continue
					}
					found := map[string]bool{}
					findSecrets(root, resp, map[string]bool{}, found)
					for name := range found {
						t.Errorf("%s %s stores a reply holding %s", method, path, name)
					}
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("no operation with an Idempotency-Key found, so this test would prove nothing")
	}
}

func findParamNames(root, node any, out map[string]bool) {
	list, _ := node.([]any)
	for _, item := range list {
		m, _ := item.(map[string]any)
		if ref, ok := m["$ref"].(string); ok {
			var cur = root
			for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
				c, _ := cur.(map[string]any)
				cur = c[part]
			}
			m, _ = cur.(map[string]any)
		}
		if name, ok := m["name"].(string); ok {
			out[name] = true
		}
	}
}
