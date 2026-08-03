-- seed migration  (Go: 0002_seed; Spring: V2__seed.sql, or a CommandLineRunner)
INSERT INTO customers (name, surname, email, password) VALUES ('rasim','yetim','demo@aurora.test', '1')
  ON CONFLICT (email) DO NOTHING;

-- categories FIRST: products.category_id is NOT NULL (§4.1). All images share the one placeholder URL —
-- a plain migration file has no variables, so the literal repeats.
INSERT INTO categories (name, slug, image_url) VALUES
  ('Drinkware',   'drinkware',   'https://upload.wikimedia.org/wikipedia/commons/thumb/3/37/Schenker_VIA14_Laptop_asv2021-01.jpg/500px-Schenker_VIA14_Laptop_asv2021-01.jpg'),
  ('Apparel',     'apparel',     'https://upload.wikimedia.org/wikipedia/commons/thumb/3/37/Schenker_VIA14_Laptop_asv2021-01.jpg/500px-Schenker_VIA14_Laptop_asv2021-01.jpg'),
  ('Accessories', 'accessories', 'https://upload.wikimedia.org/wikipedia/commons/thumb/3/37/Schenker_VIA14_Laptop_asv2021-01.jpg/500px-Schenker_VIA14_Laptop_asv2021-01.jpg')
  ON CONFLICT (slug) DO NOTHING;

-- products resolve their category BY SLUG (never a hardcoded id); a re-run (e.g. a CommandLineRunner on
-- every boot) keeps exactly 3 products (DoD #3)
INSERT INTO products (name, unit_price, stock, category_id)
SELECT v.name, v.unit_price, v.stock, c.id
FROM (VALUES
  ('Aurora Mug',          1499,  50, 'drinkware'),
  ('Aurora Tee',          2999,  12, 'apparel'),
  ('Aurora Sticker Pack',  499, 200, 'accessories')
) AS v(name, unit_price, stock, slug)
JOIN categories c ON c.slug = v.slug
ON CONFLICT (name) DO NOTHING;

-- one placeholder image per product at position 0 (the §5.1 images list)
INSERT INTO product_images (product_id, url, position)
SELECT p.id, 'https://upload.wikimedia.org/wikipedia/commons/thumb/3/37/Schenker_VIA14_Laptop_asv2021-01.jpg/500px-Schenker_VIA14_Laptop_asv2021-01.jpg', 0
FROM products p
ON CONFLICT (product_id, position) DO NOTHING;
