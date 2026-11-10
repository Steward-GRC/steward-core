-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The position of a category among its siblings, counted from zero under each
-- parent (roots included). Existing rows keep a stable order: by name, then id.
-- Gaps are allowed (a delete leaves one); only the relative order counts.
ALTER TABLE categories ADD COLUMN order_index INTEGER;

UPDATE categories c
SET order_index = r.pos
FROM (
  SELECT id, (row_number() OVER (PARTITION BY parent_id ORDER BY name, id) - 1) AS pos
  FROM categories
) r
WHERE c.id = r.id;

ALTER TABLE categories
  ALTER COLUMN order_index SET NOT NULL,
  ALTER COLUMN order_index SET DEFAULT 0;

CREATE INDEX categories_parent_order_idx ON categories (parent_id, order_index);
