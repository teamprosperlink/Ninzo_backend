package contracts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const dataMapPath = "../../docs/engineering/endpoint_data_map.md"

// mapSection returns the text of one numbered section of the data map.
func mapSection(t *testing.T, doc, heading string) string {
	t.Helper()
	_, rest, found := strings.Cut(doc, "\n"+heading+"\n")
	if !found {
		t.Fatalf("data map has no section %q", heading)
	}
	if next := strings.Index(rest, "\n## "); next >= 0 {
		return rest[:next]
	}
	return rest
}

// mapRows returns each table row of a section, split into cells.
func mapRows(section string) [][]string {
	var rows [][]string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "| `") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		rows = append(rows, cells)
	}
	return rows
}

// TestDataMapCoversEveryEndpoint stops an endpoint being added with no plan for its data.
func TestDataMapCoversEveryEndpoint(t *testing.T) {
	raw, err := os.ReadFile(dataMapPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	sections := map[string]string{"customer": "## 3. Customer API", "rider": "## 4. Rider API", "partner": "## 4.1 Partner API", "console": "## 4.2 Console API"}
	for app, heading := range sections {
		rows := map[string]bool{}
		for _, cells := range mapRows(mapSection(t, text, heading)) {
			id := strings.Trim(cells[0], "`")
			if rows[id] {
				t.Errorf("%s: %s is listed twice", app, id)
			}
			rows[id] = true
		}
		l := &loader{docs: map[string]*doc{}}
		d, err := l.load(filepath.Join(contractsDir, app, "openapi.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		ops := map[string]bool{}
		for _, op := range operations(d) {
			ops[op.id] = true
			if !rows[op.id] {
				t.Errorf("%s: %s is not in the data map", app, op.id)
			}
		}
		for id := range rows {
			if !ops[id] {
				t.Errorf("%s: %s is in the data map, but not in the contract", app, id)
			}
		}
		if len(rows) == 0 {
			t.Errorf("%s: no rows, so this test proves nothing", app)
		}
	}
}

var backticked = regexp.MustCompile("`([a-z_]+)`")

// TestDataMapOnlyNamesKnownTables stops a row relying on a table nobody plans to build.
func TestDataMapOnlyNamesKnownTables(t *testing.T) {
	raw, err := os.ReadFile(dataMapPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	known := map[string]bool{}
	for _, cells := range mapRows(mapSection(t, text, "## 1. Tables by module")) {
		name := strings.Trim(cells[1], "`")
		if known[name] {
			t.Errorf("table %s is listed twice", name)
		}
		known[name] = true
	}
	if len(known) == 0 {
		t.Fatal("no tables found, so this test proves nothing")
	}
	used := map[string]bool{}
	for _, heading := range []string{"## 3. Customer API", "## 4. Rider API", "## 4.1 Partner API", "## 4.2 Console API", "## 5. Background work"} {
		for _, cells := range mapRows(mapSection(t, text, heading)) {
			if len(cells) < 3 {
				t.Errorf("short row: %v", cells)
				continue
			}
			for _, cell := range cells[1:3] {
				for _, m := range backticked.FindAllStringSubmatch(cell, -1) {
					used[m[1]] = true
					if !known[m[1]] {
						t.Errorf("%s names unknown table %s", cells[0], m[1])
					}
				}
			}
		}
	}
	// Only the console writes prices, so no customer or rider endpoint uses this one yet.
	// rider_credentials stays in the schema for the phone-lost options.
	notYetUsed := map[string]bool{"price_history": true, "rider_credentials": true}
	for name := range known {
		if !used[name] && !notYetUsed[name] {
			t.Errorf("table %s is listed, but no customer or rider endpoint uses it", name)
		}
	}
}
