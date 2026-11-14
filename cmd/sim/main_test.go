package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

func TestBadArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"-zone", "mysuru", "-osrm", "http://x"}, "unknown zone"},
		{[]string{"-orders", "0", "-osrm", "http://x"}, "orders must be"},
		{[]string{"-orders", "20000", "-osrm", "http://x"}, "orders must be"},
		{[]string{}, "-osrm is required"},
		{[]string{"stray"}, "usage"},
		{[]string{"-osrm", "ftp://x"}, "http or https"},
	} {
		if err := run(context.Background(), c.args, nil, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v: %v, want %q", c.args, err, c.want)
		}
	}
}

func TestRelocateAndPlaceAreSeparate(t *testing.T) {
	err := run(context.Background(), []string{"-osrm", "http://127.0.0.1:1", "-fleet", "3", "-relocate", "-place"}, nil, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "separate") {
		t.Fatal(err)
	}
}
