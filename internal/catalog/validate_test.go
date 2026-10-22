package catalog

import (
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := Item{SKU: "TEA-001", Name: "Green tea", Price: 45000, Currency: "VND", Version: 1}

	with := func(f func(*Item)) Item {
		it := valid
		f(&it)
		return it
	}

	tests := []struct {
		name    string
		item    Item
		wantErr string // empty means valid
	}{
		{"valid item", valid, ""},
		{"free product", with(func(it *Item) { it.Price = 0 }), ""},
		{"missing sku", with(func(it *Item) { it.SKU = "" }), "missing sku"},
		{"blank sku", with(func(it *Item) { it.SKU = "   " }), "missing sku"},
		{"sku too long", with(func(it *Item) { it.SKU = strings.Repeat("A", 65) }), "sku longer"},
		{"sku at max length", with(func(it *Item) { it.SKU = strings.Repeat("A", 64) }), ""},
		{"zero version", with(func(it *Item) { it.Version = 0 }), "version must be positive"},
		{"negative version", with(func(it *Item) { it.Version = -2 }), "version must be positive"},
		{"missing name", with(func(it *Item) { it.Name = "" }), "missing name"},
		{"blank name", with(func(it *Item) { it.Name = " \t" }), "missing name"},
		{"name too long", with(func(it *Item) { it.Name = strings.Repeat("ă", 256) }), "name longer"},
		{"name at max length (multibyte)", with(func(it *Item) { it.Name = strings.Repeat("ă", 255) }), ""},
		{"negative price", with(func(it *Item) { it.Price = -1 }), "price must not be negative"},
		{"missing currency", with(func(it *Item) { it.Currency = "" }), "currency"},
		{"lowercase currency", with(func(it *Item) { it.Currency = "vnd" }), "currency"},
		{"currency too long", with(func(it *Item) { it.Currency = "VNDX" }), "currency"},
		{"withdraw with sku and version only", Item{SKU: "TEA-001", Version: 2, Withdrawn: true}, ""},
		{"withdraw without sku", Item{Version: 2, Withdrawn: true}, "missing sku"},
		{"withdraw with zero version", Item{SKU: "TEA-001", Withdrawn: true}, "version must be positive"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.item)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("want valid, got error %q", err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("want error containing %q, got nil", tc.wantErr)
			case tc.wantErr != "" && !strings.Contains(err.Error(), tc.wantErr):
				t.Fatalf("want error containing %q, got %q", tc.wantErr, err)
			}
		})
	}
}
