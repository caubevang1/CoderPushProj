// Package catalog holds the pure business rules for supplier catalog updates.
// It has no knowledge of the database or HTTP.
package catalog

// Item is one record received from the supplier.
type Item struct {
	SKU       string `json:"sku"`
	Name      string `json:"name"`
	Price     int64  `json:"price"`
	Currency  string `json:"currency"`
	Version   int64  `json:"version"`
	Withdrawn bool   `json:"withdrawn"`
}

// Status is the sale status of a product in the catalog.
type Status string

const (
	StatusActive    Status = "active"
	StatusWithdrawn Status = "withdrawn"
)

// Product is a product stored in the catalog.
// A withdrawn product is kept as a tombstone so its version keeps
// blocking stale records.
type Product struct {
	SKU      string `json:"sku"`
	Name     string `json:"name"`
	Price    int64  `json:"price"`
	Currency string `json:"currency"`
	Status   Status `json:"status"`
	Version  int64  `json:"version"`
}

// SameContent reports whether p and q differ only in version.
func (p Product) SameContent(q Product) bool {
	return p.SKU == q.SKU &&
		p.Name == q.Name &&
		p.Price == q.Price &&
		p.Currency == q.Currency &&
		p.Status == q.Status
}

// Action is what to do with an incoming item given the current product.
type Action int

const (
	ActionCreate          Action = iota // unseen SKU, for sale
	ActionCreateTombstone               // unseen SKU, withdrawn
	ActionUpdate                        // newer version of an active product
	ActionWithdraw                      // newer version withdraws an active product
	ActionReactivate                    // newer version puts a withdrawn product back on sale
	ActionBumpTombstone                 // newer withdraw of a withdrawn product: only the version moves
	ActionSkipStale                     // older version than stored
	ActionSkipDuplicate                 // same version as stored
)

var actionNames = [...]string{
	ActionCreate:          "create",
	ActionCreateTombstone: "create_tombstone",
	ActionUpdate:          "update",
	ActionWithdraw:        "withdraw",
	ActionReactivate:      "reactivate",
	ActionBumpTombstone:   "bump_tombstone",
	ActionSkipStale:       "skip_stale",
	ActionSkipDuplicate:   "skip_duplicate",
}

func (a Action) String() string {
	if a < 0 || int(a) >= len(actionNames) {
		return "unknown"
	}
	return actionNames[a]
}

// Skipped reports whether a leaves the stored product untouched.
func (a Action) Skipped() bool {
	return a == ActionSkipStale || a == ActionSkipDuplicate
}
