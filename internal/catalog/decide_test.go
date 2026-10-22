package catalog

import "testing"

func active(version int64) *Product {
	return &Product{SKU: "TEA-001", Name: "Green tea", Price: 45000, Currency: "VND", Status: StatusActive, Version: version}
}

func withdrawn(version int64) *Product {
	p := active(version)
	p.Status = StatusWithdrawn
	return p
}

func forSale(version int64) Item {
	return Item{SKU: "TEA-001", Name: "Green tea", Price: 42000, Currency: "VND", Version: version}
}

func withdraw(version int64) Item {
	return Item{SKU: "TEA-001", Version: version, Withdrawn: true}
}

// Rows 2-10 of the decision table in PLAN.md section 7.3.
// Row 1 (invalid item) is covered by TestValidate.
func TestDecide(t *testing.T) {
	tests := []struct {
		row  int
		name string
		cur  *Product
		item Item
		want Action
	}{
		{2, "unseen sku, for sale", nil, forSale(1), ActionCreate},
		{3, "unseen sku, withdrawn", nil, withdraw(1), ActionCreateTombstone},
		{4, "older version", active(5), forSale(3), ActionSkipStale},
		{5, "same version", active(5), forSale(5), ActionSkipDuplicate},
		{6, "newer version, for sale", active(5), forSale(6), ActionUpdate},
		{7, "newer version withdraws", active(5), withdraw(6), ActionWithdraw},
		{8, "older record does not revive withdrawn", withdrawn(5), forSale(4), ActionSkipStale},
		{9, "newer record reactivates withdrawn", withdrawn(5), forSale(6), ActionReactivate},
		{10, "newer withdraw of withdrawn", withdrawn(5), withdraw(7), ActionBumpTombstone},

		// Extra edges around rows 4, 5 and 10.
		{4, "older withdraw of active", active(5), withdraw(3), ActionSkipStale},
		{5, "same version withdraw of withdrawn", withdrawn(5), withdraw(5), ActionSkipDuplicate},
		{5, "same version for sale of withdrawn", withdrawn(5), forSale(5), ActionSkipDuplicate},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Decide(tc.cur, tc.item); got != tc.want {
				t.Fatalf("row %d: Decide = %v, want %v", tc.row, got, tc.want)
			}
		})
	}
}

func TestApply(t *testing.T) {
	tests := []struct {
		name string
		cur  *Product
		item Item
		want Product
	}{
		{
			name: "create takes the item",
			cur:  nil,
			item: forSale(1),
			want: Product{SKU: "TEA-001", Name: "Green tea", Price: 42000, Currency: "VND", Status: StatusActive, Version: 1},
		},
		{
			name: "tombstone has only sku, status and version",
			cur:  nil,
			item: withdraw(2),
			want: Product{SKU: "TEA-001", Status: StatusWithdrawn, Version: 2},
		},
		{
			name: "update takes the item",
			cur:  active(5),
			item: forSale(6),
			want: Product{SKU: "TEA-001", Name: "Green tea", Price: 42000, Currency: "VND", Status: StatusActive, Version: 6},
		},
		{
			name: "withdraw keeps last known details",
			cur:  active(5),
			item: withdraw(6),
			want: Product{SKU: "TEA-001", Name: "Green tea", Price: 45000, Currency: "VND", Status: StatusWithdrawn, Version: 6},
		},
		{
			name: "reactivate takes the item",
			cur:  withdrawn(5),
			item: forSale(6),
			want: Product{SKU: "TEA-001", Name: "Green tea", Price: 42000, Currency: "VND", Status: StatusActive, Version: 6},
		},
		{
			name: "bump tombstone only moves the version",
			cur:  withdrawn(5),
			item: withdraw(7),
			want: Product{SKU: "TEA-001", Name: "Green tea", Price: 45000, Currency: "VND", Status: StatusWithdrawn, Version: 7},
		},
		{
			name: "stale leaves product untouched",
			cur:  active(5),
			item: forSale(3),
			want: *active(5),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Apply(tc.cur, tc.item, Decide(tc.cur, tc.item))
			if got != tc.want {
				t.Fatalf("Apply =\n  %+v\nwant\n  %+v", got, tc.want)
			}
		})
	}
}

// The scenario behind row 10: the supplier's real order is v5 withdraw,
// v6 back on sale, v7 withdraw, but v6 arrives last. It must not revive
// the product.
func TestLateReactivationAfterNewerWithdraw(t *testing.T) {
	var cur *Product
	for _, it := range []Item{forSale(1), withdraw(5), withdraw(7), forSale(6)} {
		next := Apply(cur, it, Decide(cur, it))
		cur = &next
	}
	if cur.Status != StatusWithdrawn || cur.Version != 7 {
		t.Fatalf("got status %s version %d, want withdrawn version 7", cur.Status, cur.Version)
	}
}

func TestSameContent(t *testing.T) {
	a := *active(5)
	b := *active(6)
	if !a.SameContent(b) {
		t.Fatal("products differing only in version should have same content")
	}
	b.Price++
	if a.SameContent(b) {
		t.Fatal("products with different prices should not have same content")
	}
}
