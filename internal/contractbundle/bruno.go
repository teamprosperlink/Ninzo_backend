package contractbundle

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

var pathParam = regexp.MustCompile(`\{([^}]+)\}`)

// Bruno returns a Bruno collection for a bundled OpenAPI file, as relative paths and contents.
func Bruno(app string, bundled []byte) (map[string]string, error) {
	var doc map[string]any
	if err := yaml.Unmarshal(bundled, &doc); err != nil {
		return nil, err
	}
	paths, _ := doc["paths"].(map[string]any)
	type entry struct {
		id, method, path string
		node             map[string]any
	}
	var entries []entry
	for path, item := range paths {
		methods, _ := item.(map[string]any)
		for method, raw := range methods {
			node, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if id, ok := node["operationId"].(string); ok {
				entries = append(entries, entry{id, method, path, node})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })

	files := map[string]string{}
	collection, err := json.MarshalIndent(map[string]any{"version": "1", "name": "Ninzo " + app + " mock", "type": "collection"}, "", "  ")
	if err != nil {
		return nil, err
	}
	files[app+"/bruno.json"] = string(collection) + "\n"
	files[app+"/environments/mock.bru"] = "vars {\n  baseUrl: http://localhost:8080/" + app + "/v1\n  token: mock-access-token\n  scenario: \n}\n"
	for i, e := range entries {
		var b strings.Builder
		fmt.Fprintf(&b, "meta {\n  name: %s\n  type: http\n  seq: %d\n}\n\n", e.id, i+1)
		url := "{{baseUrl}}" + pathParam.ReplaceAllString(e.path, "mock_id")
		hasBody := e.node["requestBody"] != nil
		bodyMode := "none"
		if hasBody {
			bodyMode = "json"
		}
		fmt.Fprintf(&b, "%s {\n  url: %s\n  body: %s\n  auth: none\n}\n\n", e.method, url, bodyMode)
		b.WriteString("headers {\n  Authorization: Bearer {{token}}\n  X-Mock-Scenario: {{scenario}}\n")
		if e.method != "get" {
			fmt.Fprintf(&b, "  Idempotency-Key: bruno-%s-0001\n", e.id)
		}
		b.WriteString("}\n")
		if hasBody {
			sample, err := json.MarshalIndent(sampleBody(doc, e.node), "  ", "  ")
			if err != nil {
				return nil, err
			}
			fmt.Fprintf(&b, "\nbody:json {\n  %s\n}\n", sample)
		}
		files[app+"/"+e.id+".bru"] = b.String()
	}
	return files, nil
}

// sampleBody builds a valid request body from the operation's schema.
func sampleBody(doc, op map[string]any) any {
	body, _ := op["requestBody"].(map[string]any)
	content, _ := body["content"].(map[string]any)
	media, _ := content["application/json"].(map[string]any)
	schema, _ := media["schema"].(map[string]any)
	return sampleValue(doc, schema, 0)
}

func sampleValue(doc, schema map[string]any, depth int) any {
	if depth > 8 || schema == nil {
		return nil
	}
	if ref, ok := schema["$ref"].(string); ok {
		var cur any = doc
		for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
			m, _ := cur.(map[string]any)
			cur = m[part]
		}
		target, _ := cur.(map[string]any)
		return sampleValue(doc, target, depth+1)
	}
	if v, ok := schema["const"]; ok {
		return v
	}
	if v, ok := schema["example"]; ok {
		return v
	}
	if values, ok := schema["enum"].([]any); ok && len(values) > 0 {
		return values[0]
	}
	// A oneOf beside properties only says which fields are required, as in "one of these two fields".
	_, hasProps := schema["properties"]
	if choices, ok := schema["oneOf"].([]any); ok && len(choices) > 0 && !hasProps {
		first, _ := choices[0].(map[string]any)
		return sampleValue(doc, first, depth+1)
	}
	kind, _ := schema["type"].(string)
	switch kind {
	case "object":
		out := map[string]any{}
		props, _ := schema["properties"].(map[string]any)
		required, _ := schema["required"].([]any)
		choices, ok := schema["oneOf"].([]any)
		if !ok {
			choices, ok = schema["anyOf"].([]any)
		}
		if ok && len(choices) > 0 {
			first, _ := choices[0].(map[string]any)
			extra, _ := first["required"].([]any)
			required = append(append([]any{}, required...), extra...)
		}
		for _, r := range required {
			name, _ := r.(string)
			prop, _ := props[name].(map[string]any)
			out[name] = sampleValue(doc, prop, depth+1)
		}
		return out
	case "array":
		items, _ := schema["items"].(map[string]any)
		return []any{sampleValue(doc, items, depth+1)}
	case "integer":
		return 1
	case "number":
		return 1.5
	case "boolean":
		return true
	case "string":
		if schema["format"] == "date-time" {
			return "2026-11-04T10:00:00Z"
		}
		if pattern, _ := schema["pattern"].(string); strings.Contains(pattern, "0-9]{4,8}") {
			return "123456"
		}
		return "mock_value"
	}
	return nil
}
