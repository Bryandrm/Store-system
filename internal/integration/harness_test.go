// Package integration exercises features against the features they touch.
//
// Everything here enters through the real HTTP router, exactly like the client.
// A test that calls sales.Create() directly is not testing what runs in
// production: the point of this layer is the wiring, and one operation touches
// four modules at once.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bryandrm/store-system/internal/api"
	"github.com/bryandrm/store-system/internal/auth"
	"github.com/bryandrm/store-system/internal/testdb"
)

const deviceID = "integration-device"

// env is a running server plus a logged-in session.
type env struct {
	t      *testing.T
	tdb    *testdb.DB
	server *httptest.Server
	token  string

	userID     uuid.UUID
	customerID uuid.UUID
	productA   uuid.UUID
	productB   uuid.UUID
}

func newEnv(t *testing.T) *env {
	t.Helper()

	e := &env{
		t:          t,
		tdb:        testdb.New(t),
		userID:     uuid.Must(uuid.NewV7()),
		customerID: uuid.Must(uuid.NewV7()),
		productA:   uuid.Must(uuid.NewV7()),
		productB:   uuid.Must(uuid.NewV7()),
	}

	const password = "clave-de-integracion"
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("could not hash the password: %v", err)
	}

	e.exec(`INSERT INTO users (id, username, display_name, password_hash, role)
	        VALUES ($1, 'tester', 'Tester', $2, 'owner')`, e.userID, hash)
	e.exec(`INSERT INTO customers (id, name) VALUES ($1, 'Cliente')`, e.customerID)

	for i, id := range []uuid.UUID{e.productA, e.productB} {
		e.exec(`INSERT INTO products (id, name, sort_order) VALUES ($1, $2, $3)`,
			id, fmt.Sprintf("producto %d", i), i)
		e.exec(`INSERT INTO product_prices
		        (id, product_id, price_cents, effective_from, created_by_user_id)
		        VALUES ($1, $2, 50, now(), $3)`, uuid.Must(uuid.NewV7()), id, e.userID)
		// Opening stock, so derived levels do not start negative.
		e.exec(`INSERT INTO stock_movements
		        (id, product_id, delta_qty_milli, reason, ref_kind, occurred_at, created_by_user_id)
		        VALUES ($1, $2, 100000, 'initial', 'manual', now(), $3)`,
			uuid.Must(uuid.NewV7()), id, e.userID)
	}

	handler, err := api.New(api.Deps{
		Pool:           e.tdb.App,
		AllowedOrigins: []string{"http://localhost"},
		// The suite logs in far more often than the shipped limits allow. Those
		// defaults are covered by internal/auth/ratelimit_test.go.
		LoginLimits: auth.Limits{PerIPPerMinute: 10_000, PerUsernamePerHour: 10_000},
	})
	if err != nil {
		t.Fatalf("could not build the router: %v", err)
	}

	e.server = httptest.NewServer(handler)
	t.Cleanup(e.server.Close)

	e.token = e.login("tester", password)
	return e
}

func (e *env) exec(sql string, args ...any) {
	e.t.Helper()
	if _, err := e.tdb.App.Exec(context.Background(), sql, args...); err != nil {
		e.t.Fatalf("setup failed: %v", err)
	}
}

// scalar reads a single value, for assertions about derived state.
func (e *env) scalar(sql string, args ...any) int64 {
	e.t.Helper()
	var v int64
	if err := e.tdb.App.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		e.t.Fatalf("query failed: %v", err)
	}
	return v
}

func (e *env) login(username, password string) string {
	e.t.Helper()

	var body struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	e.post("/api/v1/auth/login", map[string]any{
		"username": username, "password": password, "device_label": "test",
	}, "", &body)

	if body.Data.Token == "" {
		e.t.Fatal("login returned no token")
	}
	return body.Data.Token
}

// post sends a request and decodes the envelope into out. It returns the status
// so a test can assert on rejection as well as success.
func (e *env) post(path string, payload any, token string, out any) int {
	e.t.Helper()

	encoded, err := json.Marshal(payload)
	if err != nil {
		e.t.Fatalf("could not encode the request: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, e.server.URL+path, bytes.NewReader(encoded))
	if err != nil {
		e.t.Fatalf("could not build the request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := e.server.Client().Do(req)
	if err != nil {
		e.t.Fatalf("request failed: %v", err)
	}
	defer resp.Body.Close()

	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			e.t.Fatalf("could not decode the response: %v", err)
		}
	}
	return resp.StatusCode
}

// syncResponse mirrors what the client receives.
type syncResponse struct {
	OK   bool `json:"ok"`
	Data struct {
		Results []struct {
			OpID      string `json:"op_id"`
			Status    string `json:"status"`
			ErrorCode string `json:"error_code"`
		} `json:"results"`
		Changes []struct {
			Entity   string `json:"entity"`
			EntityID string `json:"entity_id"`
		} `json:"changes"`
		Cursor  string `json:"cursor"`
		HasMore bool   `json:"has_more"`
	} `json:"data"`
}

// sync posts a batch of operations exactly as a device would.
func (e *env) sync(cursor string, operations ...map[string]any) syncResponse {
	e.t.Helper()

	var out syncResponse
	e.post("/api/v1/sync", map[string]any{
		"device_id":  deviceID,
		"cursor":     cursor,
		"operations": operations,
	}, e.token, &out)
	return out
}

// saleOp builds a create_sale operation.
func saleOp(saleID uuid.UUID, method string, customerID *uuid.UUID, lines ...map[string]any) map[string]any {
	var total, paid int64
	for _, l := range lines {
		total += l["line_total_cents"].(int64)
	}
	if method == "cash" {
		paid = total
	}

	payload := map[string]any{
		"sale_id":        saleID,
		"total_cents":    total,
		"paid_cents":     paid,
		"payment_method": method,
		"occurred_at":    time.Now().UTC().Format(time.RFC3339),
		"device_id":      deviceID,
		"lines":          lines,
	}
	if customerID != nil {
		payload["customer_id"] = *customerID
	}

	return map[string]any{
		"op_id":   uuid.Must(uuid.NewV7()),
		"type":    "create_sale",
		"payload": payload,
	}
}

func line(productID uuid.UUID, qtyMilli, unitPriceCents int64) map[string]any {
	return map[string]any{
		"product_id":       productID,
		"qty_milli":        qtyMilli,
		"unit_price_cents": unitPriceCents,
		"line_total_cents": unitPriceCents * qtyMilli / 1000,
	}
}
