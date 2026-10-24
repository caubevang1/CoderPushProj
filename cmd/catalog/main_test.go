package main

import (
	"bytes"
	"strings"
	"testing"
)

// Command-line mistakes are reported before touching the database.
func TestUsageErrors(t *testing.T) {
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"frobnicate"}, `unknown command "frobnicate"`},
		{[]string{"update"}, "Usage:"},
		{[]string{"update", "restart"}, `unknown update command "restart"`},
		{[]string{"products", "-status", "sold"}, `invalid -status "sold"`},
		{[]string{"changes", "-kind", "deleted"}, `invalid -kind "deleted"`},
		{[]string{"problems", "resolve"}, "usage: catalog problems resolve ID"},
		{[]string{"problems", "resolve", "x"}, `invalid problem ID "x"`},
		{[]string{"update", "status", "extra"}, `unexpected argument "extra"`},
	}
	for _, tc := range tests {
		var out, errOut bytes.Buffer
		code := run(tc.args, &out, &errOut)
		if code != 1 {
			t.Errorf("%v: exit code %d, want 1", tc.args, code)
		}
		if !strings.Contains(errOut.String(), tc.want) {
			t.Errorf("%v: stderr = %q, want it to contain %q", tc.args, errOut.String(), tc.want)
		}
	}
}

func TestHelp(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run(nil, &out, &errOut); code != 0 || !strings.Contains(out.String(), "update start") {
		t.Fatalf("help: code %d, output %q", code, out.String())
	}
}
