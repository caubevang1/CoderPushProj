// Package supplier talks to the supplier's batch API, and provides a fake
// supplier for demos and tests.
package supplier

import "CoderPush/internal/catalog"

// Batch is one page of the supplier feed: GET /batches?cursor=<cursor>.
// An empty cursor asks for the first batch.
type Batch struct {
	BatchID    string         `json:"batch_id"`
	NextCursor string         `json:"next_cursor"`
	HasMore    bool           `json:"has_more"`
	Items      []catalog.Item `json:"items"`
}
