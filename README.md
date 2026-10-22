# Supplier Catalog Updates

Take-home assessment: keep a product catalog in sync with a supplier that
sends its data in batches, possibly repeated or out of order, without ever
losing the existing catalog when something goes wrong.

Written in Go 1.27 with MySQL 8 (Docker).

> Work in progress. Setup instructions, CLI usage and the design overview
> will be added as the remaining parts are implemented.

## Assumptions

| # | Assumption / decision | Why |
|---|---|---|
| A1 | A product is identified by the supplier's `sku`. | A stable key, so importing the same data twice never creates duplicates. |
| A2 | Every record carries a `version`: a positive integer, issued by the supplier, increasing per SKU. | The only reliable way to tell old from new when records arrive out of order. Arrival time is not used. |
| A3 | A withdrawal is sent explicitly as `"withdrawn": true`. | Withdrawals are never inferred from a product being absent, because a partial update would then withdraw products by mistake. |
| A4 | A withdrawn product is kept as a tombstone (`status = withdrawn`). | Its version must be remembered so stale records cannot bring it back. |
| A5 | A withdrawal for a SKU never seen before is still stored as a tombstone. | The withdrawal may arrive before the record that created the product. |
| A6 | A record with a higher version than the tombstone puts the product back on sale (reactivation). | The brief only forbids *old* information from restoring a withdrawn product. |
| A7 | An invalid record is skipped and logged as a problem; it does not block the rest of the batch. | One bad record should not stop the whole update. |
| A8 | The supplier paginates with a cursor, and asking again with the same cursor returns the same batch. | Required to resume an interrupted update. |
| A9 | An update is finished only when the supplier answers `has_more = false`. | The single condition under which an update may be reported as complete. |
| A10 | Only one update can be open at a time. | Prevents two processes from writing over each other. |
| A11 | Prices are stored as integers in the currency's smallest unit (VND: đồng). | Avoids floating-point rounding errors. |
| A12 | All timestamps are stored in UTC. | Avoids time zone mismatches. |
