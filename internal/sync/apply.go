package sync

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/db"
	"github.com/bryandrm/store-system/internal/httpx"
	"github.com/bryandrm/store-system/internal/sales"
)

// Operation types the client may send. Every operational mutation arrives
// through /sync; administrative ones (users, sessions) go through plain REST,
// because creating a user offline is meaningless.
const (
	OpCreateSale = "create_sale"
)

// Result statuses returned per operation.
const (
	StatusApplied   = "applied"
	StatusDuplicate = "duplicate"
	StatusRejected  = "rejected"
	StatusRetry     = "retry"
)

// Operation is one unit of work from a device.
type Operation struct {
	OpID    uuid.UUID       `json:"op_id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Result is what the client gets back for each operation it sent.
type Result struct {
	OpID      uuid.UUID `json:"op_id"`
	Status    string    `json:"status"`
	ErrorCode string    `json:"error_code,omitempty"`
	Message   string    `json:"message,omitempty"`
}

// MaxOperationsPerBatch caps one request.
const MaxOperationsPerBatch = 100

// ApplyOne applies a single operation, in its own transaction.
//
// The domain rows, the change_log rows and the sync_operations idempotency row
// all commit TOGETHER. That is what makes a lost response harmless: the client
// retries, gets "duplicate", and nothing is written twice.
//
// The boolean reports whether the failure was transient, in which case the
// caller should stop the batch and let the client retry the rest.
func ApplyOne(ctx context.Context, pool *pgxpool.Pool, op Operation, identity auth.Identity, deviceID string) (Result, bool) {
	hash := sha256.Sum256(op.Payload)

	var (
		result    Result
		transient bool
	)
	result.OpID = op.OpID

	err := db.WithTx(ctx, pool, func(tx pgx.Tx) error {
		// Claim the op_id first. If the insert finds an existing row, this
		// operation already ran and every dependent write must be skipped.
		var claimed uuid.UUID
		err := tx.QueryRow(ctx,
			`INSERT INTO sync_operations (op_id, device_id, user_id, op_type, status, request_hash)
			 VALUES ($1, $2, $3, $4, 'applied', $5)
			 ON CONFLICT (op_id) DO NOTHING
			 RETURNING op_id`,
			op.OpID, deviceID, identity.UserID, op.Type, hash[:],
		).Scan(&claimed)

		if errors.Is(err, pgx.ErrNoRows) {
			return errAlreadyApplied
		}
		if err != nil {
			return fmt.Errorf("sync: could not claim operation: %w", err)
		}

		return dispatch(ctx, tx, op, identity)
	})

	switch {
	case err == nil:
		result.Status = StatusApplied

	case errors.Is(err, errAlreadyApplied):
		// First write wins, with no exception. A replay carrying a different
		// payload is a client bug, so it is logged rather than applied.
		prev := previousResult(ctx, pool, op.OpID, hash[:])
		result.Status = StatusDuplicate
		result.ErrorCode = prev

	case isTransient(err):
		result.Status = StatusRetry
		result.ErrorCode = "TRANSIENT"
		transient = true

	default:
		// A business rejection is permanent: recording it means retrying the
		// same batch re-rejects it instead of doing work again.
		appErr := httpx.AsAppError(err)
		result.Status = StatusRejected
		result.ErrorCode = appErr.Code
		result.Message = appErr.Message
		recordRejection(ctx, pool, op, identity, deviceID, hash[:], appErr)
	}

	return result, transient
}

// errAlreadyApplied is an internal signal, never returned to a client.
var errAlreadyApplied = errors.New("sync: operation already applied")

// dispatch is the ONLY switch over operation type in the codebase.
//
// Each case delegates to the domain function that already exists. Business
// logic has one implementation and two entry points — never a "sync" version
// and an "online" version that quietly drift apart.
func dispatch(ctx context.Context, tx pgx.Tx, op Operation, identity auth.Identity) error {
	switch op.Type {
	case OpCreateSale:
		var in sales.CreateInput
		if err := json.Unmarshal(op.Payload, &in); err != nil {
			return httpx.ErrValidation.
				WithMessage("El contenido de la operacion no es valido").
				WithCause(err)
		}
		// The device declares who made the sale; the token says who uploaded
		// it. Storing both keeps authorship auditable when someone logs in as
		// the other person.
		if in.CreatedByUserID == uuid.Nil {
			in.CreatedByUserID = identity.UserID
		}
		return sales.Create(ctx, tx, in, identity.UserID)

	default:
		return httpx.ErrValidation.
			WithMessage("Tipo de operacion desconocido").
			WithDetails(httpx.FieldDetail{Field: "type", Code: "UNKNOWN_OPERATION"})
	}
}

// previousResult reads back what happened the first time this op_id ran, and
// warns when the same id arrives with a different payload.
func previousResult(ctx context.Context, pool *pgxpool.Pool, opID uuid.UUID, hash []byte) string {
	var (
		status     string
		errorCode  *string
		storedHash []byte
	)
	err := pool.QueryRow(ctx,
		`SELECT status, error_code, request_hash FROM sync_operations WHERE op_id = $1`,
		opID).Scan(&status, &errorCode, &storedHash)
	if err != nil {
		return ""
	}

	if string(storedHash) != string(hash) {
		slog.WarnContext(ctx, "op_id reused with a different payload",
			"op_id", opID, "stored_status", status)
	}

	if errorCode != nil {
		return *errorCode
	}
	return ""
}

// recordRejection stores a permanent rejection in its own transaction, since
// the one that carried the operation was rolled back.
func recordRejection(ctx context.Context, pool *pgxpool.Pool, op Operation,
	identity auth.Identity, deviceID string, hash []byte, appErr *httpx.AppError) {

	_, err := pool.Exec(ctx,
		`INSERT INTO sync_operations (op_id, device_id, user_id, op_type, status,
		                              error_code, error_message, request_hash)
		 VALUES ($1,$2,$3,$4,'rejected',$5,$6,$7)
		 ON CONFLICT (op_id) DO NOTHING`,
		op.OpID, deviceID, identity.UserID, op.Type,
		appErr.Code, appErr.Message, hash)
	if err != nil {
		slog.ErrorContext(ctx, "could not record rejected operation",
			"op_id", op.OpID, "error", err)
	}
}

// isTransient tells infrastructure failures apart from business rejections.
//
// The distinction drives client behaviour: a transient failure must be retried
// forever, because a device that was offline for five days still holds real
// sales. A permanent one goes to the error tray and stops burning battery.
func isTransient(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "40001", // serialization_failure
			"40P01", // deadlock_detected
			"53300", // too_many_connections
			"55P03", // lock_not_available
			"57014": // query_canceled (statement_timeout)
			return true
		}
		// Any other Postgres error is a real constraint violation: retrying
		// would just fail again.
		return false
	}

	// A domain error is by definition permanent.
	var appErr *httpx.AppError
	if errors.As(err, &appErr) {
		return appErr.StatusCode >= 500
	}

	// Anything unrecognized (connection dropped, context deadline) is treated
	// as transient: losing a real sale is far worse than a wasted retry.
	return true
}
