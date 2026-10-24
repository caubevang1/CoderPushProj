// Package report formats catalog data for staff, the same way in the CLI
// and in the web demo.
package report

import (
	"fmt"
	"strconv"
	"strings"

	"CoderPush/internal/store"
)

// Versions shows the version move of a change, e.g. "v2 → v3".
func Versions(c store.Change) string {
	if c.OldVersion == nil {
		return "v" + strconv.FormatInt(c.NewVersion, 10)
	}
	return fmt.Sprintf("v%d → v%d", *c.OldVersion, c.NewVersion)
}

// Details describes a change in words staff can check quickly.
func Details(c store.Change) string {
	n, o := c.New, c.Old
	switch {
	case n == nil:
		return ""
	case c.Kind == store.ChangeWithdrawn && o == nil:
		return "withdrawn before it was ever listed"
	case c.Kind == store.ChangeWithdrawn:
		return "no longer for sale"
	case o == nil || c.Kind == store.ChangeReactivated:
		return fmt.Sprintf("%s, %s", n.Name, Money(n.Price, n.Currency))
	}

	var diffs []string
	if o.Name != n.Name {
		diffs = append(diffs, fmt.Sprintf("name %q → %q", o.Name, n.Name))
	}
	if o.Price != n.Price || o.Currency != n.Currency {
		diffs = append(diffs, fmt.Sprintf("price %s → %s", Money(o.Price, o.Currency), Money(n.Price, n.Currency)))
	}
	return strings.Join(diffs, "; ")
}

// Money formats an amount in the currency's smallest unit, e.g. 45,000 VND.
func Money(amount int64, currency string) string {
	s := strconv.FormatInt(amount, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return strings.TrimSpace(s + " " + currency)
}

// Plural returns "1 batch" or "3 batches".
func Plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
