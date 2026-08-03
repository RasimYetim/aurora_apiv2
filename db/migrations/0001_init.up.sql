CREATE TABLE categories (
  id        BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name      TEXT NOT NULL,
  slug      TEXT NOT NULL UNIQUE,                          -- UNIQUE so the seed's ON CONFLICT (slug) is idempotent
  image_url TEXT                                           -- the category's single image (§5.1); seeded with the placeholder
);

CREATE TABLE products (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  name        TEXT    NOT NULL UNIQUE,                     -- UNIQUE so the seed's ON CONFLICT (name) is idempotent
  unit_price  BIGINT  NOT NULL CHECK (unit_price >= 0),    -- cents
  stock       INTEGER NOT NULL CHECK (stock >= 0),
  category_id BIGINT  NOT NULL REFERENCES categories(id)   -- every product has a category → categories are seeded first (§4.3)
);

CREATE TABLE product_images (
  id         BIGINT  GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  product_id BIGINT  NOT NULL REFERENCES products(id) ON DELETE CASCADE,
  url        TEXT    NOT NULL,                             -- a reference the client fetches; the base never serves image bytes (§5.1 note)
  position   INTEGER NOT NULL DEFAULT 0,                   -- gallery order (§5.1 images sorted ascending)
  UNIQUE (product_id, position)                            -- also the seed's ON CONFLICT guard
);

CREATE TABLE customers (
  id    BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  email TEXT UNIQUE NOT NULL,
  name TEXT,
  surname TEXT,
  password TEXT NOT NULL
);

CREATE TABLE carts (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  token       UUID   NOT NULL DEFAULT gen_random_uuid() UNIQUE,  -- the X-Cart-Token identity (§5.2); gen_random_uuid() is core PostgreSQL (13+) — no extension
  customer_id BIGINT NULL REFERENCES customers(id),        -- NULL while anonymous; set when checkout claims the cart (§5.3)
  status      TEXT   NOT NULL DEFAULT 'active'
                CHECK (status IN ('active','converted','abandoned')),  -- 'abandoned' is reserved: no base producer, like orders' 'shipped'
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()           -- touched by every cart write; wire rule §5.2/§7
);

CREATE TABLE cart_items (
  cart_id    BIGINT  NOT NULL REFERENCES carts(id) ON DELETE CASCADE,
  product_id BIGINT  NOT NULL REFERENCES products(id),     -- FK: a cart can only ever hold real products (§5.2 404 mapping)
  qty        INTEGER NOT NULL CHECK (qty > 0),
  PRIMARY KEY (cart_id, product_id)                        -- one line per product; POST /cart/items merges via ON CONFLICT (§5.2)
);

CREATE TABLE orders (
  id          BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  customer_id BIGINT NOT NULL REFERENCES customers(id),   -- prerequisite FK row MUST exist
  total       BIGINT NOT NULL CHECK (total >= 0),         -- cents
  status      TEXT NOT NULL DEFAULT 'pending'
                CHECK (status IN ('pending','shipped','cancelled')),
  idempotency_key TEXT,                                   -- nullable; partial unique index (§4.2) allows many NULLs
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE order_items (
  order_id    BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
  product_id  BIGINT NOT NULL REFERENCES products(id),
  quantity    INTEGER NOT NULL CHECK (quantity > 0),
  unit_price  BIGINT  NOT NULL CHECK (unit_price >= 0),   -- price captured AT purchase time
  PRIMARY KEY (order_id, product_id)
);
-- orders.idempotency_key TEXT (declared on the orders table in 0001_init / V1__init):
CREATE UNIQUE INDEX orders_idem_key ON orders (idempotency_key)
  WHERE idempotency_key IS NOT NULL;   -- partial: many NULLs allowed, set keys unique
