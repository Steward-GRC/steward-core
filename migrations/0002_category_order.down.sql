-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

DROP INDEX IF EXISTS categories_parent_order_idx;
ALTER TABLE categories DROP COLUMN IF EXISTS order_index;
