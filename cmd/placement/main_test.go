package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestUsage(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		args []string
		env  map[string]string
		want string
	}{
		{nil, nil, "usage"},
		{[]string{"-zone", "z"}, nil, "OSRM URL is needed"},
		{[]string{"-zone", "z", "-osrm", "http://127.0.0.1:1"}, nil, "DATABASE_URL"},
		{[]string{"-zone", "z"}, map[string]string{"ROUTING_ENGINE_URL": "http://127.0.0.1:1"}, "DATABASE_URL"},
		// Not this machine, and no -remote: refused before anything is written.
		{[]string{"-zone", "z", "-osrm", "http://127.0.0.1:1"}, map[string]string{"DATABASE_URL": "postgres://u:p@db.example.com:5432/x"}, "local"},
	} {
		if err := run(context.Background(), c.args, env(c.env), &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}
