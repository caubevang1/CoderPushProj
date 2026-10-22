package catalog

// Decide picks the action for a valid item. cur is nil when the SKU has
// never been seen. Only a strictly greater version may change a product,
// so repeated and out-of-order records are harmless.
func Decide(cur *Product, it Item) Action {
	if cur == nil {
		if it.Withdrawn {
			return ActionCreateTombstone
		}
		return ActionCreate
	}

	switch {
	case it.Version < cur.Version:
		return ActionSkipStale
	case it.Version == cur.Version:
		return ActionSkipDuplicate
	}

	if it.Withdrawn {
		if cur.Status == StatusWithdrawn {
			return ActionBumpTombstone
		}
		return ActionWithdraw
	}
	if cur.Status == StatusWithdrawn {
		return ActionReactivate
	}
	return ActionUpdate
}

// Apply returns the product as it should be stored after action a.
// For the skip actions it returns *cur unchanged.
func Apply(cur *Product, it Item, a Action) Product {
	switch a {
	case ActionCreate, ActionUpdate, ActionReactivate:
		return Product{
			SKU:      it.SKU,
			Name:     it.Name,
			Price:    it.Price,
			Currency: it.Currency,
			Status:   StatusActive,
			Version:  it.Version,
		}
	case ActionCreateTombstone:
		return Product{SKU: it.SKU, Status: StatusWithdrawn, Version: it.Version}
	case ActionWithdraw, ActionBumpTombstone:
		// Keep the last known details so staff can still see what was withdrawn.
		p := *cur
		p.Status = StatusWithdrawn
		p.Version = it.Version
		return p
	default:
		return *cur
	}
}
