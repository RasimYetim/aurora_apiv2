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

func (s *Store) Checkout(ctx context.Context, customerID int64, lines []models.Line, idemKey string) (int64, error) {
	aggLines := make(map[int64]int)
	for _, l := range lines {
		aggLines[l.ProductID] += l.Qty
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback(ctx)

	var total int64
	type capturedItem struct {
		productID int64
		qty       int
		unitPrice int64
	}
	var captured []capturedItem

	for pid, qty := range aggLines {
		var unitPrice int64
		err := tx.QueryRow(ctx, `UPDATE products SET stock=stock-$1 WHERE id=$2 AND stock>=$1 RETURNING unit_price`, qty, pid).Scan(&unitPrice)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return 0, models.ErrOutOfStock
			}
			return 0, fmt.Errorf("update stock for product %d: %w", pid, err)
		}
		total += unitPrice * int64(qty)
		captured = append(captured, capturedItem{
			productID: pid,
			qty:       qty,
			unitPrice: unitPrice,
		})
	}

	var dbIdemKey any
	if idemKey != "" {
		dbIdemKey = idemKey
	}

	var orderID int64
	err = tx.QueryRow(ctx, `INSERT INTO orders (customer_id, total, idempotency_key) VALUES ($1, $2, $3) RETURNING id`, customerID, total, dbIdemKey).Scan(&orderID)
	if err != nil {
		return 0, fmt.Errorf("insert order: %w", err)
	}

	for _, c := range captured {
		_, err := tx.Exec(ctx, `INSERT INTO order_items (order_id, product_id, quantity, unit_price) VALUES ($1, $2, $3, $4)`, orderID, c.productID, c.qty, c.unitPrice)
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
	query := `
		SELECT c.product_id, p.name, c.qty, p.unit_price,
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
		Items: []models.CartItem{},
		Total: 0,
	}

	for rows.Next() {
		var item models.CartItem
		if err := rows.Scan(&item.ProductID, &item.ProductName, &item.Quantity, &item.UnitPrice, &item.ImageURL); err != nil {
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
			p.id, p.name, p.unit_price, p.stock, p.category_id,
			COALESCE(array_agg(pi.url ORDER BY pi.position) FILTER (WHERE pi.url IS NOT NULL), '{}') as images
		FROM products p
		LEFT JOIN product_images pi ON p.id = pi.product_id
		WHERE p.id = $1
		GROUP BY p.id`
	err := s.Pool.QueryRow(ctx, query, id).Scan(&p.ID, &p.Name, &p.UnitPrice, &p.Stock, &p.CategoryID, &p.Images)
	return p, err
}

func (s *Store) Products(ctx context.Context) ([]models.Product, error) {
	query := `
		SELECT 
			p.id, p.name, p.unit_price, p.stock, p.category_id,
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
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitPrice, &p.Stock, &p.CategoryID, &p.Images); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ProductsByCategory(ctx context.Context, categoryID int64) ([]models.Product, error) {
	query := `
		SELECT 
			p.id, p.name, p.unit_price, p.stock, p.category_id,
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
		if err := rows.Scan(&p.ID, &p.Name, &p.UnitPrice, &p.Stock, &p.CategoryID, &p.Images); err != nil {
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
		WHERE p.name ILIKE $1
		GROUP BY p.id
		ORDER BY p.id`
	searchTerm := "%" + searchQuery + "%"
	rows, err := s.Pool.Query(ctx, query, searchTerm)
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

func (s *Store) CancelOrder(ctx context.Context, orderID int64) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// 1. Siparişin durumunu kontrol et
	var status string
	err = tx.QueryRow(ctx, "SELECT status FROM orders WHERE id = $1", orderID).Scan(&status)
	if err != nil {
		return errors.New("not_found")
	}

	// 2. Replay-Safe: Zaten iptal edilmişse hata verme, başarıyla çık (stokları tekrar artırma)
	if status == "cancelled" {
		return nil
	}

	// 3. Sadece 'pending' siparişler iptal edilebilir
	if status != "pending" {
		return errors.New("not_cancellable")
	}

	// 4. Durumu güncelle
	_, err = tx.Exec(ctx, "UPDATE orders SET status = 'cancelled' WHERE id = $1", orderID)
	if err != nil {
		return err
	}

	// 5. Stokları geri yatır
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
