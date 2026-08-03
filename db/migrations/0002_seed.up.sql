-- =====================================
-- Customers
-- =====================================

INSERT INTO customers (email, name, surname, password)
VALUES
('demo@aurora.test', 'Demo', 'User', '1')
ON CONFLICT (email) DO NOTHING;


-- =====================================
-- Categories
-- =====================================

INSERT INTO categories (name, slug, image_url)
VALUES
('Fruits & Vegetables', 'fruits-vegetables', ''),
('Beverages', 'beverages', ''),
('Dairy', 'dairy', ''),
('Snacks', 'snacks', ''),
('Household', 'household', '')
ON CONFLICT (slug) DO NOTHING;


-- =====================================
-- Products
-- =====================================

INSERT INTO products (name, unit_price, stock, category_id)
SELECT
    v.name,
    v.unit_price,
    v.stock,
    c.id
FROM (
VALUES

('Bananas 1kg',          8990, 42, 'fruits-vegetables'),
('Tomatoes 1kg',         6990, 35, 'fruits-vegetables'),

('Coca-Cola 1L',         4490, 60, 'beverages'),
('Orange Juice 1L',      5490, 28, 'beverages'),

('Whole Milk 1L',        3490, 44, 'dairy'),
('Cheddar Cheese 400g', 12990, 20, 'dairy'),

('Potato Chips',         2990, 55, 'snacks'),
('Chocolate Cookies',    3990, 38, 'snacks'),

('Dishwashing Liquid',   6990, 26, 'household'),
('Paper Towels',        12990, 22, 'household')

) AS v(name, unit_price, stock, slug)
JOIN categories c
ON c.slug = v.slug
ON CONFLICT (name) DO NOTHING;


-- =====================================
-- Product Images
-- =====================================

INSERT INTO product_images (product_id, url, position)
SELECT
    p.id,
    '',
    0
FROM products p
ON CONFLICT (product_id, position) DO NOTHING;
