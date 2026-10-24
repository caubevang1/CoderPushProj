package main

import (
	"bytes"
	"strings"
	"testing"

	"CoderPush/internal/catalog"
	"CoderPush/internal/store"
)

func TestMoney(t *testing.T) {
	tests := []struct {
		amount   int64
		currency string
		want     string
	}{
		{0, "VND", "0 VND"},
		{999, "VND", "999 VND"},
		{45000, "VND", "45,000 VND"},
		{1234567, "VND", "1,234,567 VND"},
		{-1000, "VND", "-1,000 VND"},
		{100, "", "100"},
	}
	for _, tc := range tests {
		if got := money(tc.amount, tc.currency); got != tc.want {
			t.Errorf("money(%d, %q) = %q, want %q", tc.amount, tc.currency, got, tc.want)
		}
	}
}

func TestDetails(t *testing.T) {
	old := &catalog.Product{SKU: "A", Name: "Tea", Price: 45000, Currency: "VND", Status: catalog.StatusActive, Version: 1}
	renamed := *old
	renamed.Name, renamed.Price, renamed.Version = "Green tea", 42000, 2
	tomb := &catalog.Product{SKU: "A", Status: catalog.StatusWithdrawn, Version: 2}

	tests := []struct {
		name string
		c    store.Change
		want string
	}{
		{"created", store.Change{Kind: store.ChangeCreated, New: old}, "Tea, 45,000 VND"},
		{"updated", store.Change{Kind: store.ChangeUpdated, Old: old, New: &renamed},
			`name "Tea" → "Green tea"; price 45,000 VND → 42,000 VND`},
		{"withdrawn", store.Change{Kind: store.ChangeWithdrawn, Old: old, New: tomb}, "no longer for sale"},
		{"tombstone", store.Change{Kind: store.ChangeWithdrawn, New: tomb}, "withdrawn before it was ever listed"},
	}
	for _, tc := range tests {
		if got := details(tc.c); got != tc.want {
			t.Errorf("%s: details = %q, want %q", tc.name, got, tc.want)
		}
	}
}

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
