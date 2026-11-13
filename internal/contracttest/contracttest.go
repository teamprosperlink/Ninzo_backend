// Package contracttest checks a reply against a schema of the bundled contracts. It is for tests
// only: a database test sends a real request, then asks here whether the answer is what the contract
// promises, down to types, formats and required fields, not only whether the fields exist.
package contracttest

import (
	"bytes"
	"encoding/json"
	"sync"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"gopkg.in/yaml.v3"

	"github.com/teamprosperlink/Ninzo_backend/contracts"
)

var (
	mu       sync.Mutex
	compiled = map[string]*jsonschema.Compiler{}
)

// compiler is the schema compiler for one bundled file, such as rider.openapi.yaml, built once.
func compiler(t testing.TB, file string) *jsonschema.Compiler {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if c, ok := compiled[file]; ok {
		return c
	}
	raw, err := contracts.Dist.ReadFile("dist/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	asJSON, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(asJSON))
	if err != nil {
		t.Fatal(err)
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	if err := c.AddResource("mem://"+file, inst); err != nil {
		t.Fatal(err)
	}
	compiled[file] = c
	return c
}

// Check fails the test if got, a decoded JSON value, does not match the named schema of the bundled
// file, for example Check(t, "rider.openapi.yaml", "Shift", shift).
func Check(t testing.TB, file, schema string, got any) {
	t.Helper()
	s, err := compiler(t, file).Compile("mem://" + file + "#/components/schemas/" + schema)
	if err != nil {
		t.Fatalf("contract %s has no usable schema %s: %v", file, schema, err)
	}
	// Round-trip so numbers and nulls are plain JSON, as a client reads them.
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Validate(inst); err != nil {
		t.Errorf("%s does not match the contract's %s: %v\n%s", file, schema, err, b)
	}
}
