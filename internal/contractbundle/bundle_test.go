package contractbundle

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const contractsDir = "../../contracts"

func TestCommittedBundlesAreUpToDate(t *testing.T) {
	bundles, err := All(contractsDir)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range bundles {
		got, err := os.ReadFile(filepath.Join(contractsDir, "dist", name))
		if err != nil {
			t.Fatalf("%s: %v. Run task contracts:bundle", name, err)
		}
		if !bytes.Equal(bytes.ReplaceAll(got, []byte("\r\n"), []byte("\n")), want) {
			t.Errorf("%s is out of date. Run task contracts:bundle", name)
		}
	}
}

func TestBundlesHaveNoExternalReferences(t *testing.T) {
	bundles, err := All(contractsDir)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range bundles {
		var doc map[string]any
		if err := yaml.Unmarshal(data, &doc); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		checkRefs(t, name, doc, doc)
	}
}

func checkRefs(t *testing.T, name string, root map[string]any, node any) {
	t.Helper()
	switch v := node.(type) {
	case []any:
		for _, item := range v {
			checkRefs(t, name, root, item)
		}
	case map[string]any:
		if ref, ok := v["$ref"].(string); ok {
			if !strings.HasPrefix(ref, "#/") {
				t.Errorf("%s: external reference %s", name, ref)
				return
			}
			var cur any = root
			for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
				m, _ := cur.(map[string]any)
				next, ok := m[part]
				if !ok {
					t.Errorf("%s: broken reference %s", name, ref)
					return
				}
				cur = next
			}
			return
		}
		for _, child := range v {
			checkRefs(t, name, root, child)
		}
	}
}

func TestClashingNamesAreRefused(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("shared.yaml", "components:\n  schemas:\n    Money:\n      type: string\n")
	write("main.yaml", "components:\n  schemas:\n    Money:\n      type: integer\n    Price:\n      $ref: 'shared.yaml#/components/schemas/Money'\n")
	if _, err := Bundle(filepath.Join(dir, "main.yaml")); err == nil {
		t.Error("expected an error when two files define the same name")
	}
}
