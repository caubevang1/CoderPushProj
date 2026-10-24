package report

import (
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
		if got := Money(tc.amount, tc.currency); got != tc.want {
			t.Errorf("Money(%d, %q) = %q, want %q", tc.amount, tc.currency, got, tc.want)
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
		if got := Details(tc.c); got != tc.want {
			t.Errorf("%s: Details = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestVersions(t *testing.T) {
	two := int64(2)
	if got := Versions(store.Change{NewVersion: 1}); got != "v1" {
		t.Errorf("new product: %q", got)
	}
	if got := Versions(store.Change{OldVersion: &two, NewVersion: 3}); got != "v2 → v3" {
		t.Errorf("update: %q", got)
	}
}
