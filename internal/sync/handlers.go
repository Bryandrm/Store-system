package sync

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/httpx"
)

// Handler serves the two endpoints every device lives on.
type Handler struct {
	pool *pgxpool.Pool
}

func NewHandler(pool *pgxpool.Pool) *Handler {
	return &Handler{pool: pool}
}

type syncRequest struct {
	DeviceID   string      `json:"device_id"`
	Cursor     string      `json:"cursor"`
	Operations []Operation `json:"operations"`
}

type syncResponse struct {
	Results    []Result  `json:"results"`
	Changes    []Change  `json:"changes"`
	Cursor     string    `json:"cursor"`
	HasMore    bool      `json:"has_more"`
	ServerTime time.Time `json:"server_time"`
}

// HandleSync is the single endpoint every operational mutation goes through.
//
// It applies the operations the device sent, then returns everything that
// changed since the device's cursor. Both directions in one round trip, because
// a phone on mobile data pays for each one.
func (h *Handler) HandleSync(w http.ResponseWriter, r *http.Request) {
	identity := auth.MustIdentity(r.Context())

	var req syncRequest
	if err := httpx.DecodeJSON(w, r, &req); err != nil {
		httpx.Fail(w, r, err)
		return
	}

	if strings.TrimSpace(req.DeviceID) == "" {
		httpx.Fail(w, r, httpx.ErrValidation.WithDetails(
			httpx.FieldDetail{Field: "device_id", Code: "REQUIRED"}))
		return
	}
	if len(req.Operations) > MaxOperationsPerBatch {
		httpx.Fail(w, r, httpx.ErrValidation.
			WithMessage("Demasiadas operaciones en un solo envio").
			WithDetails(httpx.FieldDetail{Field: "operations", Code: "TOO_MANY"}))
		return
	}

	cursor, err := parseCursor(req.Cursor)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	// Operations apply sequentially, in the order the device sent them, each in
	// its own transaction. Sequential order is what lets an operation reference
	// something an earlier one in the same batch created.
	results := make([]Result, 0, len(req.Operations))
	for _, op := range req.Operations {
		result, transient := ApplyOne(r.Context(), h.pool, op, identity, req.DeviceID)
		results = append(results, result)

		// Stop the batch on a transient failure and let the client retry the
		// rest. Batch atomicity is deliberately NOT offered: one bad operation
		// must never block the other ninety-nine.
		if transient {
			break
		}
	}

	changes, nextCursor, hasMore, err := ReadFeed(r.Context(), h.pool, cursor, MaxFeedPage)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, http.StatusOK, "", syncResponse{
		Results: results,
		Changes: changes,
		Cursor:  strconv.FormatUint(nextCursor, 10),
		HasMore: hasMore,
		// server_time lets the client measure its own clock drift and warn the
		// user. Sync itself is clock-immune, but occurred_at is not, and every
		// report groups by it.
		ServerTime: time.Now().UTC(),
	})
}

// HandleBootstrap returns a complete replica for a device starting fresh.
func (h *Handler) HandleBootstrap(w http.ResponseWriter, r *http.Request) {
	_ = auth.MustIdentity(r.Context())

	result, err := Bootstrap(r.Context(), h.pool)
	if err != nil {
		httpx.Fail(w, r, err)
		return
	}

	httpx.OK(w, r, http.StatusOK, "", result)
}

// parseCursor accepts an empty string as "start from the beginning", so a fresh
// client does not have to send a magic zero.
func parseCursor(raw string) (uint64, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, httpx.ErrValidation.
			WithMessage("El cursor de sincronizacion no es valido").
			WithDetails(httpx.FieldDetail{Field: "cursor", Code: "INVALID"}).
			WithCause(err)
	}
	return cursor, nil
}
