package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/rasim/aurora/internal/models"
)

type Store struct{ Pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{Pool: pool}
}

func (s *Store) CheckUserLogin(ctx context.Context, email, password string) (*models.Customer, error) {
	var c models.Customer
	query := `SELECT id, name, surname, email FROM customers WHERE email = $1 AND password = $2`
	err := s.Pool.QueryRow(ctx, query, email, password).Scan(&c.ID, &c.Name, &c.Surname, &c.Email)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) RegisterUser(ctx context.Context, name, surname, email, password string) (int64, error) {
	query := `INSERT INTO customers (name, surname, email, password) VALUES ($1, $2, $3, $4) RETURNING id`
	var id int64
	err := s.Pool.QueryRow(ctx, query, name, surname, email, password).Scan(&id)
	if err != nil {
		return 0, err
	}
	return id, nil
}

func (s *Store) Checkout(ctx context.Context, customerID int64, cartToken string, idemKey string) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var cartID int64
	err = tx.QueryRow(ctx, "UPDATE carts SET status = 'converted', customer_id = $1, updated_at = now() WHERE token = $2 AND status = 'active' RETURNING id", customerID, cartToken).Scan(&cartID)
	if err != nil {
		return 0, fmt.Errorf("claim cart: %w", err)
	}

	rows, err := tx.Query(ctx, "SELECT product_id, SUM(qty) FROM cart_items WHERE cart_id = $1 GROUP BY product_id", cartID)
	if err != nil {
		return 0, fmt.Errorf("cart items: %w", err)
	}
	type cartItem struct {
		productID int64
		qty       int
	}
	var items []cartItem
	for rows.Next() {
		var item cartItem
		if err := rows.Scan(&item.productID, &item.qty); err != nil {
			rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	rows.Close()

	if len(items) == 0 {
		return 0, fmt.Errorf("empty cart")
	}

	var total int64
	type capturedItem struct {
		productID int64
		qty       int
		unitPrice int64
	}
	var captured []capturedItem

	for _, item := range items {
		pid := item.productID
		qty := item.qty

		var basePrice int64
		err := tx.QueryRow(ctx, "UPDATE products SET stock=stock-$1 WHERE id=$2 AND stock>=$1 RETURNING unit_price", qty, pid).Scan(&basePrice)
		if err != nil {
			if err.Error() == "no rows in result set" {
				return 0, models.ErrOutOfStock
			}
			return 0, fmt.Errorf("update stock for product %d: %w", pid, err)
		}

		effectivePrice := basePrice

		var dealID int64
		var salePrice int64
		var perCustomerCap int

		err = tx.QueryRow(ctx, "UPDATE deals SET sold = sold + $1 WHERE product_id = $2 AND now() BETWEEN starts_at AND ends_at AND sold + $1 <= allocation_cap RETURNING id, sale_price_cents, per_customer_cap", qty, pid).Scan(&dealID, &salePrice, &perCustomerCap)

		if err != nil {
			if err.Error() == "no rows in result set" {
				var dealExists bool
				errEx := tx.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM deals WHERE product_id = $1 AND now() BETWEEN starts_at AND ends_at)", pid).Scan(&dealExists)
				if errEx != nil {
					return 0, fmt.Errorf("check deal exists: %w", errEx)
				}
				if dealExists {
					return 0, models.ErrDealSoldOut
				}
			} else {
				return 0, fmt.Errorf("update deals: %w", err)
			}
		} else {
			var reservedQty int
			errUpsert := tx.QueryRow(ctx, "INSERT INTO deal_purchases (deal_id, customer_id, qty) SELECT CAST($1 AS bigint), CAST($2 AS bigint), CAST($3 AS integer) WHERE CAST($3 AS integer) <= CAST($4 AS integer) ON CONFLICT (deal_id, customer_id) DO UPDATE SET qty = deal_purchases.qty + EXCLUDED.qty WHERE deal_purchases.qty + EXCLUDED.qty <= CAST($4 AS integer) RETURNING qty", dealID, customerID, qty, perCustomerCap).Scan(&reservedQty)

			if errUpsert != nil {
				if errUpsert.Error() == "no rows in result set" {
					return 0, models.ErrPurchaseLimit
				}
				return 0, fmt.Errorf("upsert deal_purchases: %w", errUpsert)
			}
			effectivePrice = salePrice
		}

		total += effectivePrice * int64(qty)
		captured = append(captured, capturedItem{
			productID: pid,
			qty:       qty,
			unitPrice: effectivePrice,
		})
	}

	var dbIdemKey any
	if idemKey != "" {
		dbIdemKey = idemKey
	}

	var orderID int64
	err = tx.QueryRow(ctx, "INSERT INTO orders (customer_id, total, idempotency_key) VALUES ($1, $2, $3) RETURNING id", customerID, total, dbIdemKey).Scan(&orderID)
	if err != nil {
		return 0, fmt.Errorf("insert order: %w", err)
	}

	for _, c := range captured {
		_, err := tx.Exec(ctx, "INSERT INTO order_items (order_id, product_id, quantity, unit_price) VALUES ($1, $2, $3, $4)", orderID, c.productID, c.qty, c.unitPrice)
		if err != nil {
			return 0, fmt.Errorf("insert order item %d: %w", c.productID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, fmt.Errorf("commit tx: %w", err)
	}

	return orderID, nil
}

func (s *Store) getOrCreateCartID(ctx context.Context, tx pgx.Tx, customerID int64) (int64, error) {
	var cartID int64
	err := tx.QueryRow(ctx, `
		SELECT id FROM carts 
		WHERE customer_id = $1 AND status = 'active'
	`, customerID).Scan(&cartID)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				INSERT INTO carts (customer_id, status) 
				VALUES ($1, 'active') 
				RETURNING id
			`, customerID).Scan(&cartID)
			if err != nil {
				return 0, err
			}
		} else {
			return 0, err
		}
	}
	return cartID, nil
}

func (s *Store) AddToCart(ctx context.Context, customerID int64, productID int64, quantity int) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	cartID, err := s.getOrCreateCartID(ctx, tx, customerID)
	if err != nil {
		return err
	}

	var remainingStock int
	err = tx.QueryRow(ctx, `
        UPDATE products
        SET stock = stock - $1
        WHERE id = $2 AND stock >= $1
        RETURNING stock`, quantity, productID).Scan(&remainingStock)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("yetersiz stok veya ürün bulunamadı")
		}
		return err
	}

	_, err = tx.Exec(ctx, `
        INSERT INTO cart_items (cart_id, product_id, qty)
        VALUES ($1, $2, $3)
        ON CONFLICT (cart_id, product_id)
        DO UPDATE SET
            qty = cart_items.qty + $3
    `, cartID, productID, quantity)

	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `UPDATE carts SET updated_at = NOW() WHERE id = $1`, cartID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) RemoveFromCart(ctx context.Context, customerID int64, productID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cartID int64
	err = tx.QueryRow(ctx, `SELECT id FROM carts WHERE customer_id = $1 AND status = 'active'`, customerID).Scan(&cartID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ürün sepette bulunamadı")
		}
		return err
	}

	var removedQty int
	err = tx.QueryRow(ctx, `
		DELETE FROM cart_items 
		WHERE cart_id = $1 AND product_id = $2 
		RETURNING qty`, cartID, productID).Scan(&removedQty)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ürün sepette bulunamadı")
		}
		return err
	}

	_, err = tx.Exec(ctx, `
		UPDATE products 
		SET stock = stock + $1 
		WHERE id = $2`, removedQty, productID)

	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `UPDATE carts SET updated_at = NOW() WHERE id = $1`, cartID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) UpdateCartItemQuantity(ctx context.Context, customerID int64, productID int64, newQuantity int) error {
	if newQuantity <= 0 {
		return s.RemoveFromCart(ctx, customerID, productID)
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cartID int64
	err = tx.QueryRow(ctx, `SELECT id FROM carts WHERE customer_id = $1 AND status = 'active'`, customerID).Scan(&cartID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ürün sepette bulunamadı")
		}
		return err
	}

	var currentQty int
	err = tx.QueryRow(ctx, `
		SELECT qty FROM cart_items 
		WHERE cart_id = $1 AND product_id = $2 FOR UPDATE`, cartID, productID).Scan(&currentQty)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ürün sepette bulunamadı")
		}
		return err
	}

	diff := newQuantity - currentQty
	if diff == 0 {
		return nil
	}

	if diff > 0 {
		var remainingStock int
		err = tx.QueryRow(ctx, `
			UPDATE products 
			SET stock = stock - $1 
			WHERE id = $2 AND stock >= $1 
			RETURNING stock`, diff, productID).Scan(&remainingStock)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("yetersiz stok")
			}
			return err
		}
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE products 
			SET stock = stock + $1 
			WHERE id = $2`, -diff, productID)
		if err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx, `
		UPDATE cart_items 
		SET qty = $1 
		WHERE cart_id = $2 AND product_id = $3`, newQuantity, cartID, productID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `UPDATE carts SET updated_at = NOW() WHERE id = $1`, cartID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) ClearCart(ctx context.Context, customerID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var cartID int64
	err = tx.QueryRow(ctx, `SELECT id FROM carts WHERE customer_id = $1 AND status = 'active'`, customerID).Scan(&cartID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}

	rows, err := tx.Query(ctx, `
		DELETE FROM cart_items 
		WHERE cart_id = $1 
		RETURNING product_id, qty`, cartID)
	if err != nil {
		return err
	}

	type returnItem struct {
		pid int64
		qty int
	}
	var returns []returnItem
	for rows.Next() {
		var r returnItem
		if err := rows.Scan(&r.pid, &r.qty); err != nil {
			return err
		}
		returns = append(returns, r)
	}
	rows.Close()

	for _, r := range returns {
		_, err = tx.Exec(ctx, `
			UPDATE products 
			SET stock = stock + $1 
			WHERE id = $2`, r.qty, r.pid)
		if err != nil {
			return err
		}
	}

	_, err = tx.Exec(ctx, `UPDATE carts SET updated_at = NOW() WHERE id = $1`, cartID)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (s *Store) ReleaseExpiredCarts(ctx context.Context) error {
	query := `
    WITH abandoned_carts AS (
        UPDATE carts SET status = 'abandoned'
        WHERE status = 'active' AND updated_at < NOW() - INTERVAL '15 minutes'
        RETURNING id
    ),
    expired_items AS (
        DELETE FROM cart_items
        WHERE cart_id IN (SELECT id FROM abandoned_carts)
        RETURNING product_id, qty as quantity
    ),
    aggregated_returns AS (
        SELECT product_id, SUM(quantity) as total_qty
        FROM expired_items
        GROUP BY product_id
    )
    UPDATE products p
    SET stock = p.stock + ar.total_qty
    FROM aggregated_returns ar
    WHERE p.id = ar.product_id;
    `
	_, err := s.Pool.Exec(ctx, query)
	return err
}

func (s *Store) GetCart(ctx context.Context, customerID int64) (*models.Cart, error) {
	var cartToken string
	err := s.Pool.QueryRow(ctx, "SELECT token FROM carts WHERE customer_id = $1 AND status = 'active'", customerID).Scan(&cartToken)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	query := `
		SELECT c.product_id, p.name, c.qty, COALESCE((SELECT MIN(sale_price_cents) FROM deals WHERE product_id = p.id AND now() BETWEEN starts_at AND ends_at AND sold < allocation_cap), p.unit_price), p.unit_price,
		       COALESCE((SELECT url FROM product_images WHERE product_id = p.id ORDER BY position LIMIT 1), '') as image_url
		FROM cart_items c
		JOIN products p ON c.product_id = p.id
		JOIN carts ct ON ct.id = c.cart_id
		WHERE ct.customer_id = $1 AND ct.status = 'active'
		ORDER BY c.product_id
	`
	rows, err := s.Pool.Query(ctx, query, customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	cart := &models.Cart{
		Token: cartToken,
		Items: []models.CartItem{},
		Total: 0,
	}

	for rows.Next() {
		var item models.CartItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPrice, &item.OriginalPrice, &item.ImageURL); err != nil {
			return nil, err
		}
		cart.Items = append(cart.Items, item)
		cart.Total += item.UnitPrice * int64(item.Quantity)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return cart, nil
}

func (s *Store) OrderIDByKey(ctx context.Context, key string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx, `SELECT id FROM orders WHERE idempotency_key = $1`, key).Scan(&id)
	return id, err
}

func (s *Store) Product(ctx context.Context, id int64) (models.Product, error) {
	var p models.Product
	query := `
		SELECT 
			p.id, p.name, COALESCE((SELECT MIN(sale_price_cents) FROM deals WHERE product_id = p.id AND now() BETWEEN starts_at AND ends_at AND sold < allocation_cap), p.unit_price), p.unit_price, p.stock, p.category_id,
			COALESCE(array_agg(pi.url ORDER BY pi.position) FILTER (WHERE pi.url IS NOT NULL), '{}') as images
		FROM products p
		LEFT JOIN product_images pi ON p.id = pi.product_id
		WHERE p.id = $1
		GROUP BY p.id`
	err := s.Pool.QueryRow(ctx, query, id).Scan(&p.ID, &p.Name, &p.UnitPrice, &p.OriginalPrice, &p.Stock, &p.CategoryID, &p.Images)
	return p, err
}

func (s *Store) Products(ctx context.Context) ([]models.Product, error) {
	query := `
		SELECT 
			p.id, p.name, COALESCE((SELECT MIN(sale_price_cents) FROM deals WHERE product_id = p.id AND now() BETWEEN starts_at AND ends_at AND sold < allocation_cap), p.unit_price), p.unit_price, p.stock, p.category_id,
			COALESCE(array_agg(pi.url ORDER BY pi.position) FILTER (WHERE pi.url IS NOT NULL), '{}') as images
		FROM products p
		LEFT JOIN product_images pi ON p.id = pi.product_id
		GROUP BY p.id
		ORDER BY p.id`
	rows, err := s.Pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Product{}
	for rows.Next() {
		var p models.Product
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitPrice, &p.OriginalPrice, &p.Stock, &p.CategoryID, &p.Images); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ProductsByCategory(ctx context.Context, categoryID int64) ([]models.Product, error) {
	query := `
		SELECT 
			p.id, p.name, COALESCE((SELECT MIN(sale_price_cents) FROM deals WHERE product_id = p.id AND now() BETWEEN starts_at AND ends_at AND sold < allocation_cap), p.unit_price), p.unit_price, p.stock, p.category_id,
			COALESCE(array_agg(pi.url ORDER BY pi.position) FILTER (WHERE pi.url IS NOT NULL), '{}') as images
		FROM products p
		LEFT JOIN product_images pi ON p.id = pi.product_id
		WHERE p.category_id = $1
		GROUP BY p.id
		ORDER BY p.id`
	rows, err := s.Pool.Query(ctx, query, categoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Product{}
	for rows.Next() {
		var p models.Product
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitPrice, &p.OriginalPrice, &p.Stock, &p.CategoryID, &p.Images); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) SearchProducts(ctx context.Context, searchQuery string) ([]models.Product, error) {
	query := `
       SELECT 
          p.id, p.name, p.unit_price, p.stock, p.category_id,
          COALESCE(array_agg(pi.url ORDER BY pi.position) FILTER (WHERE pi.url IS NOT NULL), '{}') as images
       FROM products p
       LEFT JOIN product_images pi ON p.id = pi.product_id
       WHERE similarity(p.name, $1) > 0.15 OR p.name ILIKE '%' || $1 || '%' OR word_similarity($1, p.name) > 0.3
       GROUP BY p.id, p.name, p.unit_price, p.stock, p.category_id
       ORDER BY GREATEST(similarity(p.name, $1), word_similarity($1, p.name)) DESC, p.id ASC`

	rows, err := s.Pool.Query(ctx, query, searchQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []models.Product{}
	for rows.Next() {
		var p models.Product
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitPrice, &p.Stock, &p.CategoryID, &p.Images); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) Categories(ctx context.Context) ([]models.Category, error) {
	query := `SELECT id, name, slug, COALESCE(image_url, '') FROM categories ORDER BY id`
	rows, err := s.Pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []models.Category
	for rows.Next() {
		var c models.Category
		if err := rows.Scan(&c.ID, &c.Name, &c.Slug, &c.ImageURL); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) Order(ctx context.Context, id int64) (*models.Order, error) {
	var o models.Order
	var t time.Time
	err := s.Pool.QueryRow(ctx, `SELECT id, customer_id, total, status, created_at FROM orders WHERE id = $1`, id).Scan(&o.ID, &o.CustomerID, &o.Total, &o.Status, &t)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgx.ErrNoRows
		}
		return nil, fmt.Errorf("query order: %w", err)
	}
	o.CreatedAt = t.UTC().Format(time.RFC3339)

	rows, err := s.Pool.Query(ctx, `
		SELECT oi.product_id, oi.quantity, oi.unit_price,
		       COALESCE((SELECT url FROM product_images WHERE product_id = oi.product_id ORDER BY position LIMIT 1), '') as image_url
		FROM order_items oi
		WHERE oi.order_id = $1 ORDER BY oi.product_id`, id)
	if err != nil {
		return nil, fmt.Errorf("query order items: %w", err)
	}
	defer rows.Close()

	o.Items = []models.OrderItem{}
	for rows.Next() {
		var item models.OrderItem
		if err := rows.Scan(&item.ProductID, &item.Qty, &item.UnitPrice, &item.ImageURL); err != nil {
			return nil, fmt.Errorf("scan order item: %w", err)
		}
		o.Items = append(o.Items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows err: %w", err)
	}

	return &o, nil
}

func (s *Store) OrdersByCustomer(ctx context.Context, customerID int64) ([]models.Order, error) {
	rows, err := s.Pool.Query(ctx, `SELECT id, customer_id, total, status, created_at FROM orders WHERE customer_id = $1 ORDER BY created_at DESC`, customerID)
	if err != nil {
		return nil, fmt.Errorf("query orders: %w", err)
	}
	defer rows.Close()

	orders := []models.Order{}
	for rows.Next() {
		var o models.Order
		var t time.Time
		if err := rows.Scan(&o.ID, &o.CustomerID, &o.Total, &o.Status, &t); err != nil {
			return nil, fmt.Errorf("scan order: %w", err)
		}
		o.CreatedAt = t.UTC().Format(time.RFC3339)
		o.Items = []models.OrderItem{}
		orders = append(orders, o)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows err: %w", err)
	}

	return orders, nil
}

func (s *Store) CancelOrder(ctx context.Context, orderID int64, customerID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	var dbCustomerID int64
	err = tx.QueryRow(ctx, "SELECT status, customer_id FROM orders WHERE id = $1", orderID).Scan(&status, &dbCustomerID)
	if err != nil {
		return errors.New("not_found")
	}

	if dbCustomerID != customerID {
		return errors.New("unauthorized")
	}

	if status == "cancelled" {
		return nil
	}

	if status != "pending" {
		return errors.New("not_cancellable")
	}

	_, err = tx.Exec(ctx, "UPDATE orders SET status = 'cancelled' WHERE id = $1", orderID)
	if err != nil {
		return err
	}

	rows, err := tx.Query(ctx, "SELECT product_id, quantity FROM order_items WHERE order_id = $1", orderID)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var pID int64
		var qty int
		if err := rows.Scan(&pID, &qty); err == nil {
			tx.Exec(ctx, "UPDATE products SET stock = stock + $1 WHERE id = $2", qty, pID)
		}
	}

	return tx.Commit(ctx)
}

func (s *Store) GetRecommendations(ctx context.Context, productID int64, limit int) ([]models.Recommendation, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	query := `
        SELECT p.id, p.name, COUNT(oi2.order_id) as frequency
        FROM order_items oi1
        JOIN order_items oi2 ON oi1.order_id = oi2.order_id
        JOIN products p ON p.id = oi2.product_id
        WHERE oi1.product_id = $1 AND oi2.product_id != $1
        GROUP BY p.id, p.name
        ORDER BY frequency DESC
        LIMIT $2
    `

	rows, err := tx.Query(ctx, query, productID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var recommendations []models.Recommendation
	for rows.Next() {
		var r models.Recommendation
		if err := rows.Scan(&r.ProductID, &r.Name, &r.Frequency); err != nil {
			return nil, err
		}
		recommendations = append(recommendations, r)
	}

	return recommendations, nil
}

func (s *Store) GetStoreAnalytics(ctx context.Context) (models.AnalyticsPayload, error) {
	var payload models.AnalyticsPayload
	payload.Period = "Son 30 Gün"

	payload.VIPCustomers = []models.VIPCustomer{}
	payload.TopProducts = []models.TopProduct{}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return payload, err
	}
	defer tx.Rollback(ctx)

	metricsQuery := `
		SELECT 
			COALESCE(SUM(total), 0)::FLOAT AS total_revenue,
			COUNT(id) AS total_orders,
			COALESCE(AVG(total), 0)::FLOAT AS average_order_value
		FROM orders
		WHERE status != 'cancelled' AND created_at >= NOW() - INTERVAL '30 days'
	`
	err = tx.QueryRow(ctx, metricsQuery).Scan(
		&payload.Metrics.TotalRevenue,
		&payload.Metrics.TotalOrders,
		&payload.Metrics.AverageOrderValue,
	)
	if err != nil {
		return payload, fmt.Errorf("mağaza metrikleri hesaplanırken hata oluştu: %v", err)
	}

	rfmQuery := `
		WITH rfm_base AS (
			SELECT 
				c.id AS customer_id,
				c.name || ' ' || c.surname AS full_name,
				EXTRACT(DAY FROM NOW() - MAX(o.created_at))::INT AS days_since_last_order,
				COUNT(o.id) AS order_count,
				SUM(o.total)::FLOAT AS total_spent
			FROM orders o
			JOIN customers c ON o.customer_id = c.id
			WHERE o.status != 'cancelled'
			GROUP BY c.id, c.name, c.surname
		)
		SELECT 
			customer_id,
			full_name,
			days_since_last_order,
			order_count,
			total_spent,
			CASE 
				WHEN days_since_last_order <= 30 AND order_count >= 5 AND total_spent > 1000 THEN 'Şampiyonlar'
				WHEN days_since_last_order <= 60 AND order_count >= 3 THEN 'Sadık Müşteriler'
				WHEN days_since_last_order > 60 AND order_count >= 4 THEN 'Riskli '
				WHEN days_since_last_order <= 15 AND order_count = 1 THEN 'Yeni Müşteriler'
				ELSE 'Standart Müşteri'
			END AS segment
		FROM rfm_base
		ORDER BY total_spent DESC
		LIMIT 10;
	`
	rows, err := tx.Query(ctx, rfmQuery)
	if err != nil {
		return payload, fmt.Errorf("rfm analizi çekilirken hata oluştu: %v", err)
	}
	defer rows.Close()

	for rows.Next() {
		var c models.VIPCustomer
		if err := rows.Scan(&c.CustomerID, &c.FullName, &c.DaysSinceLastOrder, &c.OrderCount, &c.TotalSpent, &c.Segment); err != nil {
			return payload, fmt.Errorf("müşteri verisi okunurken hata: %v", err)
		}
		payload.VIPCustomers = append(payload.VIPCustomers, c)
	}

	topProductsQuery := `
		SELECT 
			p.id AS product_id,
			p.name,
			SUM(oi.quantity) AS total_sold,
			SUM(oi.quantity * oi.unit_price)::FLOAT AS revenue
		FROM order_items oi
		JOIN products p ON p.id = oi.product_id
		JOIN orders o ON o.id = oi.order_id
		WHERE o.status != 'cancelled' AND o.created_at >= NOW() - INTERVAL '30 days'
		GROUP BY p.id, p.name
		ORDER BY revenue DESC
		LIMIT 5;
	`
	pRows, err := tx.Query(ctx, topProductsQuery)
	if err != nil {
		return payload, fmt.Errorf("en çok satanlar çekilirken hata oluştu: %v", err)
	}
	defer pRows.Close()

	for pRows.Next() {
		var tp models.TopProduct
		if err := pRows.Scan(&tp.ProductID, &tp.Name, &tp.TotalSold, &tp.Revenue); err != nil {
			return payload, fmt.Errorf("ürün verisi okunurken hata: %v", err)
		}
		payload.TopProducts = append(payload.TopProducts, tp)
	}

	return payload, nil
}

func (s *Store) GetStorefront(ctx context.Context) (models.StorefrontResponse, error) {
	var response models.StorefrontResponse

	categories, err := s.Categories(ctx)
	if err == nil && categories != nil {
		response.Categories = categories
	} else {
		response.Categories = []models.Category{}
	}

	products, err := s.Products(ctx)
	if err == nil && products != nil {
		if len(products) > 4 {
			response.FeaturedProducts = products[:4]
		} else {
			response.FeaturedProducts = products
		}
	} else {
		response.FeaturedProducts = []models.Product{}
		products = []models.Product{} // Hata varsa boş kalsın
	}

	response.Banners = []models.Banner{
		{ID: 1, ImageURL: "https://images.unsplash.com/photo-1607082348824-0a96f2a4b9da?q=80&w=2070"}, // Örnek kampanya
		{ID: 2, ImageURL: "https://images.unsplash.com/photo-1607082350899-7e105aa886ae?q=80&w=2070"},
	}

	response.FlashSales = []models.FlashSale{}
	if len(products) > 0 {
		p := products[0]
		var exists bool
		s.Pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM deals WHERE product_id = $1 AND ends_at > now())", p.ID).Scan(&exists)
		if !exists {
			salePrice := p.OriginalPrice * 60 / 100 // 40% discount
			s.Pool.Exec(ctx, "INSERT INTO deals (product_id, sale_price_cents, starts_at, ends_at, allocation_cap, per_customer_cap, sold) VALUES ($1, $2, now(), now() + interval '1 day', 15, 5, 0)", p.ID, salePrice)
			p.UnitPrice = salePrice
		}

		var discount int
		if p.OriginalPrice > 0 {
			discount = int(100 - (p.UnitPrice * 100 / p.OriginalPrice))
		}

		response.FlashSales = append(response.FlashSales, models.FlashSale{
			Product:            p,
			DiscountPercentage: discount,
			RemainingStock:     15,
		})
	}

	return response, nil
}
