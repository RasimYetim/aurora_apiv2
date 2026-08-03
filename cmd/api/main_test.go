package main

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasim/aurora/internal/httpapi"
	"github.com/rasim/aurora/internal/models"
	"github.com/rasim/aurora/internal/store"
)

func getPool(t *testing.T) *pgxpool.Pool {
	dsn := cmp.Or(os.Getenv("DATABASE_URL"), "postgres://postgres:dev@localhost:5432/aurora?sslmode=disable")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	return pool
}

func TestStore_Checkout(t *testing.T) {
	pool := getPool(t)
	defer pool.Close()
	s := store.NewStore(pool)
	ctx := context.Background()

	// Setup: clear and insert a product and a customer
	_, err := pool.Exec(ctx, `TRUNCATE products, customers, orders CASCADE`)
	if err != nil {
		t.Fatalf("failed to truncate tables: %v", err)
	}

	var customerID int64
	err = pool.QueryRow(ctx, `INSERT INTO customers (name, surname, email) VALUES ('Test', 'User', 'test@example.com') RETURNING id`).Scan(&customerID)
	if err != nil {
		t.Fatalf("failed to insert customer: %v", err)
	}

	var productID int64
	var unitPrice int64 = 1500
	var initialStock int = 10
	err = pool.QueryRow(ctx, `INSERT INTO products (name, unit_price, stock) VALUES ('Test Product', $1, $2) RETURNING id`, unitPrice, initialStock).Scan(&productID)
	if err != nil {
		t.Fatalf("failed to insert product: %v", err)
	}

	t.Run("Ordering more than stock returns ErrOutOfStock and leaves stock unchanged", func(t *testing.T) {
		lines := []models.Line{{ProductID: productID, Qty: 20}} // more than stock
		_, err := s.Checkout(ctx, customerID, lines, "idem-1")
		if !errors.Is(err, models.ErrOutOfStock) {
			t.Fatalf("expected models.ErrOutOfStock, got %v", err)
		}

		var currentStock int
		pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&currentStock)
		if currentStock != initialStock {
			t.Errorf("expected stock to be unchanged %d, got %d", initialStock, currentStock)
		}
	})

	t.Run("A cart with the same productId on two lines succeeds as ONE aggregated line", func(t *testing.T) {
		lines := []models.Line{
			{ProductID: productID, Qty: 2},
			{ProductID: productID, Qty: 3},
		}
		orderID, err := s.Checkout(ctx, customerID, lines, "idem-2")
		if err != nil {
			t.Fatalf("checkout failed: %v", err)
		}

		var currentStock int
		pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&currentStock)
		expectedStock := initialStock - 5
		if currentStock != expectedStock {
			t.Errorf("expected stock to be %d, got %d", expectedStock, currentStock)
		}

		var total int64
		pool.QueryRow(ctx, `SELECT total FROM orders WHERE id = $1`, orderID).Scan(&total)
		expectedTotal := int64(5) * unitPrice
		if total != expectedTotal {
			t.Errorf("expected total %d, got %d", expectedTotal, total)
		}

		// check order_items to ensure only one aggregated line was inserted
		var itemCount int
		pool.QueryRow(ctx, `SELECT count(*) FROM order_items WHERE order_id = $1`, orderID).Scan(&itemCount)
		if itemCount != 1 {
			t.Errorf("expected 1 aggregated order item, got %d", itemCount)
		}
	})

	t.Run("A success reduces stock by exactly the quantity and total equals sum", func(t *testing.T) {
		lines := []models.Line{
			{ProductID: productID, Qty: 2},
		}
		orderID, err := s.Checkout(ctx, customerID, lines, "idem-3")
		if err != nil {
			t.Fatalf("checkout failed: %v", err)
		}

		var currentStock int
		pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&currentStock)
		expectedStock := initialStock - 7 // previously reduced by 5, now by 2
		if currentStock != expectedStock {
			t.Errorf("expected stock to be %d, got %d", expectedStock, currentStock)
		}

		var total int64
		pool.QueryRow(ctx, `SELECT total FROM orders WHERE id = $1`, orderID).Scan(&total)
		expectedTotal := int64(2) * unitPrice
		if total != expectedTotal {
			t.Errorf("expected total %d, got %d", expectedTotal, total)
		}
	})
}

func TestCheckoutHandler(t *testing.T) {
	pool := getPool(t)
	defer pool.Close()
	s := store.NewStore(pool)
	router := httpapi.NewRouter(s)
	ctx := context.Background()

	_, _ = pool.Exec(ctx, `TRUNCATE products, customers, orders CASCADE`)

	var customerID int64
	pool.QueryRow(ctx, `INSERT INTO customers (name, surname, email) VALUES ('Test', 'User', 'test2@example.com') RETURNING id`).Scan(&customerID)

	var productID int64
	pool.QueryRow(ctx, `INSERT INTO products (name, unit_price, stock) VALUES ('Handler Product', 1000, 10) RETURNING id`).Scan(&productID)

	doReq := func(body string, idem string) *http.Response {
		req := httptest.NewRequest("POST", "/checkout", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if idem != "" {
			req.Header.Set("Idempotency-Key", idem)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Result()
	}

	t.Run("Empty lines returns 422", func(t *testing.T) {
		body := fmt.Sprintf(`{"customerId": %d, "lines": []}`, customerID)
		res := doReq(body, "")
		if res.StatusCode != 422 {
			t.Errorf("expected 422, got %d", res.StatusCode)
		}
	})

	t.Run("Unknown customerId returns 404", func(t *testing.T) {
		body := fmt.Sprintf(`{"customerId": 999999, "lines": [{"productId": %d, "quantity": 1}]}`, productID)
		res := doReq(body, "")
		if res.StatusCode != 404 {
			t.Errorf("expected 404, got %d", res.StatusCode)
		}
		// verify stock unchanged
		var stock int
		pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&stock)
		if stock != 10 {
			t.Errorf("expected stock 10, got %d", stock)
		}
	})

	t.Run("Idempotency key replay returns same orderId", func(t *testing.T) {
		body := fmt.Sprintf(`{"customerId": %d, "lines": [{"productId": %d, "quantity": 2}]}`, customerID, productID)
		res1 := doReq(body, "my-idem-key")
		if res1.StatusCode != 200 {
			t.Fatalf("first request failed: %d", res1.StatusCode)
		}
		var resp1 map[string]int64
		json.NewDecoder(res1.Body).Decode(&resp1)
		orderID1 := resp1["orderId"]

		res2 := doReq(body, "my-idem-key")
		if res2.StatusCode != 200 {
			t.Fatalf("second request failed: %d", res2.StatusCode)
		}
		var resp2 map[string]int64
		json.NewDecoder(res2.Body).Decode(&resp2)
		orderID2 := resp2["orderId"]

		if orderID1 != orderID2 {
			t.Errorf("expected identical orderId, got %d and %d", orderID1, orderID2)
		}

		var count int
		pool.QueryRow(ctx, `SELECT count(*) FROM orders WHERE idempotency_key = 'my-idem-key'`).Scan(&count)
		if count != 1 {
			t.Errorf("expected 1 order with this key, got %d", count)
		}
	})

	t.Run("Over-stock returns 409 and leaves stock unchanged", func(t *testing.T) {
		body := fmt.Sprintf(`{"customerId": %d, "lines": [{"productId": %d, "quantity": 50}]}`, customerID, productID)
		res := doReq(body, "another-key")
		if res.StatusCode != 409 {
			t.Errorf("expected 409, got %d", res.StatusCode)
		}
		var stock int
		pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&stock)
		if stock != 8 { // Started with 10, used 2 in the idempotency test above
			t.Errorf("expected stock 8, got %d", stock)
		}
	})
}

func TestOrderHandler(t *testing.T) {
	pool := getPool(t)
	defer pool.Close()
	s := store.NewStore(pool)
	router := httpapi.NewRouter(s)
	ctx := context.Background()

	_, _ = pool.Exec(ctx, `TRUNCATE products, customers, orders CASCADE`)

	var customerID int64
	pool.QueryRow(ctx, `INSERT INTO customers (name, surname, email) VALUES ('Test', 'User', 'test3@example.com') RETURNING id`).Scan(&customerID)

	var productID int64
	pool.QueryRow(ctx, `INSERT INTO products (name, unit_price, stock) VALUES ('Order Product', 1000, 10) RETURNING id`).Scan(&productID)

	// Create an order
	reqBody := fmt.Sprintf(`{"customerId": %d, "lines": [{"productId": %d, "quantity": 1}]}`, customerID, productID)
	req := httptest.NewRequest("POST", "/checkout", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if rec.Result().StatusCode != 200 {
		t.Fatalf("checkout failed: %d", rec.Result().StatusCode)
	}
	var resp map[string]int64
	json.NewDecoder(rec.Result().Body).Decode(&resp)
	orderID := resp["orderId"]

	t.Run("GET /orders/{id} returns the order", func(t *testing.T) {
		req := httptest.NewRequest("GET", fmt.Sprintf("/orders/%d", orderID), nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		res := rec.Result()
		if res.StatusCode != 200 {
			t.Fatalf("expected 200, got %d", res.StatusCode)
		}

		var order models.Order
		if err := json.NewDecoder(res.Body).Decode(&order); err != nil {
			t.Fatalf("failed to decode: %v", err)
		}

		if order.ID != orderID {
			t.Errorf("expected id %d, got %d", orderID, order.ID)
		}
		if order.CustomerID != customerID {
			t.Errorf("expected customerId %d, got %d", customerID, order.CustomerID)
		}
		if order.Total != 1000 {
			t.Errorf("expected total 1000, got %d", order.Total)
		}
		if order.Status != "pending" {
			t.Errorf("expected status pending, got %s", order.Status)
		}
		if len(order.Items) != 1 {
			t.Errorf("expected 1 item, got %d", len(order.Items))
		} else {
			if order.Items[0].ProductID != productID || order.Items[0].Qty != 1 || order.Items[0].UnitPrice != 1000 {
				t.Errorf("unexpected item: %+v", order.Items[0])
			}
		}

		if !strings.HasSuffix(order.CreatedAt, "Z") {
			t.Errorf("expected trailing Z in createdAt, got %s", order.CreatedAt)
		}
		if strings.Contains(order.CreatedAt, ".") {
			t.Errorf("expected no sub-second digits in createdAt, got %s", order.CreatedAt)
		}
	})

	t.Run("GET /orders/999999 returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/orders/999999", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Result().StatusCode != 404 {
			t.Errorf("expected 404, got %d", rec.Result().StatusCode)
		}
	})

	t.Run("GET /orders/abc returns 404", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/orders/abc", nil)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Result().StatusCode != 404 {
			t.Errorf("expected 404, got %d", rec.Result().StatusCode)
		}
	})
}

func TestStore_Checkout_Concurrency(t *testing.T) {
	pool := getPool(t)
	defer pool.Close()
	s := store.NewStore(pool)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `TRUNCATE products, customers, orders CASCADE`)
	if err != nil {
		t.Fatalf("failed to truncate tables: %v", err)
	}

	var productID int64
	err = pool.QueryRow(ctx, `INSERT INTO products (name, unit_price, stock) VALUES ('Concurrency Product', 1000, 1) RETURNING id`).Scan(&productID)
	if err != nil {
		t.Fatalf("failed to insert product: %v", err)
	}

	var customerIDs []int64
	for i := 0; i < 20; i++ {
		var id int64
		err = pool.QueryRow(ctx, `INSERT INTO customers (name, surname, email) VALUES ('Test', 'User', $1) RETURNING id`, fmt.Sprintf("buyer%d@example.com", i)).Scan(&id)
		if err != nil {
			t.Fatalf("failed to insert customer: %v", err)
		}
		customerIDs = append(customerIDs, id)
	}

	var wg sync.WaitGroup
	var successCount int32
	var failCount int32

	// Run 20 concurrent buyers
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(cID int64) {
			defer wg.Done()
			lines := []models.Line{{ProductID: productID, Qty: 1}}
			_, err := s.Checkout(ctx, cID, lines, "")
			if err == nil {
				atomic.AddInt32(&successCount, 1)
			} else if errors.Is(err, models.ErrOutOfStock) {
				atomic.AddInt32(&failCount, 1)
			} else {
				t.Errorf("unexpected error: %v", err)
			}
		}(customerIDs[i])
	}

	wg.Wait()

	if successCount != 1 {
		t.Errorf("expected exactly 1 success, got %d", successCount)
	}
	if failCount != 19 {
		t.Errorf("expected exactly 19 failures, got %d", failCount)
	}

	var finalStock int
	pool.QueryRow(ctx, `SELECT stock FROM products WHERE id = $1`, productID).Scan(&finalStock)
	if finalStock != 0 {
		t.Errorf("expected final stock to be 0, got %d", finalStock)
	}
}

func checkoutWithBudget(ctx context.Context, s *store.Store, customerID int64, budget int64, orderTotal int64, level pgx.TxIsoLevel) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: level})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var spent int64
	err = tx.QueryRow(ctx, `SELECT COALESCE(SUM(total), 0) FROM orders WHERE customer_id = $1`, customerID).Scan(&spent)
	if err != nil {
		return err
	}

	if spent+orderTotal > budget {
		return errors.New("budget exceeded")
	}

	time.Sleep(50 * time.Millisecond)

	_, err = tx.Exec(ctx, `INSERT INTO orders (customer_id, total) VALUES ($1, $2)`, customerID, orderTotal)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func checkoutWithBudgetRetry(ctx context.Context, s *store.Store, customerID int64, budget int64, orderTotal int64) error {
	maxRetries := 5
	for i := 0; i < maxRetries; i++ {
		err := checkoutWithBudget(ctx, s, customerID, budget, orderTotal, pgx.Serializable)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "40001" {
				// serialization_failure, retry
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return err
		}
		return nil
	}
	return errors.New("max retries reached")
}

func TestBudget_WriteSkew(t *testing.T) {
	pool := getPool(t)
	defer pool.Close()
	s := store.NewStore(pool)
	ctx := context.Background()

	setupCustomer := func(email string) int64 {
		_, _ = pool.Exec(ctx, `TRUNCATE products, customers, orders CASCADE`)
		var customerID int64
		pool.QueryRow(ctx, `INSERT INTO customers (name, surname, email) VALUES ('Test', 'User', $1) RETURNING id`, email).Scan(&customerID)
		return customerID
	}

	t.Run("READ COMMITTED suffers from write-skew", func(t *testing.T) {
		customerID := setupCustomer("rc@example.com")

		var wg sync.WaitGroup
		wg.Add(2)

		// Two concurrent orders of 60, total budget 100
		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				_ = checkoutWithBudget(ctx, s, customerID, 100, 60, pgx.ReadCommitted)
			}()
		}
		wg.Wait()

		var spent int64
		pool.QueryRow(ctx, `SELECT COALESCE(SUM(total), 0) FROM orders WHERE customer_id = $1`, customerID).Scan(&spent)
		if spent <= 100 {
			t.Errorf("expected write-skew to oversell budget (>100), but spent is %d", spent)
		} else {
			t.Logf("Write-skew reproduced! Spent %d over budget 100.", spent)
		}
	})

	t.Run("SERIALIZABLE prevents write-skew with retry", func(t *testing.T) {
		customerID := setupCustomer("serializable@example.com")

		var wg sync.WaitGroup
		wg.Add(2)

		var successCount int32
		var failCount int32

		for i := 0; i < 2; i++ {
			go func() {
				defer wg.Done()
				err := checkoutWithBudgetRetry(ctx, s, customerID, 100, 60)
				if err == nil {
					atomic.AddInt32(&successCount, 1)
				} else if err.Error() == "budget exceeded" {
					atomic.AddInt32(&failCount, 1)
				} else {
					t.Errorf("unexpected error: %v", err)
				}
			}()
		}
		wg.Wait()

		var spent int64
		pool.QueryRow(ctx, `SELECT COALESCE(SUM(total), 0) FROM orders WHERE customer_id = $1`, customerID).Scan(&spent)

		if spent > 100 {
			t.Errorf("SERIALIZABLE failed to protect budget, spent %d", spent)
		}
		if successCount != 1 || failCount != 1 {
			t.Errorf("expected 1 success and 1 fail, got success=%d, fail=%d", successCount, failCount)
		}
	})
}
