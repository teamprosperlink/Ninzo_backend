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
			b.WriteString("\nbody:json {\n  {}\n}\n")
		}
		files[app+"/"+e.id+".bru"] = b.String()
	}
	return files, nil
}
