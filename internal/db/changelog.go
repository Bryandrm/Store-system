package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// Change operations. There is no 'delete': nothing transactional is ever
// deleted, and catalog rows are hidden with is_active rather than removed.
const (
	OpInsert = "insert"
	OpUpdate = "update"
)

// Entity names as they travel to the client. They match the IndexedDB object
// store names on the other side, so renaming one here silently breaks the
// client's read model.
const (
	EntitySale           = "sale"
	EntitySaleLine       = "sale_line"
	EntitySaleVoid       = "sale_void"
	EntityProduct        = "product"
	EntityProductPrice   = "product_price"
	EntityCustomer       = "customer"
	EntityStockMovement  = "stock_movement"
	EntityCustomerLedger = "customer_ledger"
	EntityPayment        = "payment"
	EntityRestock        = "restock"
	EntityRestockLine    = "restock_line"
)

// RecordChange appends a row to the change_log so the sync feed can ship it.
//
// It MUST be called inside the same transaction as the domain row it describes.
// If the two were written separately, a crash in between would leave a row that
// no device ever learns about, and no amount of retrying would fix it because
// the write itself succeeded.
func RecordChange(ctx context.Context, q Querier, entity string, entityID uuid.UUID, op string, payload any) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("db: could not encode change payload for %s: %w", entity, err)
	}

	_, err = q.Exec(ctx,
		`INSERT INTO change_log (entity, entity_id, op, payload)
		 VALUES ($1, $2, $3, $4)`,
		entity, entityID, op, encoded)
	if err != nil {
		return fmt.Errorf("db: could not record change for %s: %w", entity, err)
	}
	return nil
}
