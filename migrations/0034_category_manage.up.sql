-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- A fifth cell on each access rule: Manage, which moves and reorders documents in
-- the categories a person has access to (steward-core#100). It is stored and
-- returned like the other cells; existing rules keep a blank one.
ALTER TABLE category_rules
  ADD COLUMN IF NOT EXISTS grant_manage TEXT NOT NULL DEFAULT ''
  CHECK (grant_manage IN ('', 'allow', 'deny'));

-- One-off data fix: a category with no rules gets the rule a new category starts
-- with, Everyone: Read. A category that has any rule is left alone, so the
-- statement is safe to run again.
INSERT INTO category_rules (category_id, ordinal, subject_kind, subject_ref, grant_read)
SELECT c.id, 1, 'everyone', '', 'allow'
FROM categories c
WHERE NOT EXISTS (SELECT 1 FROM category_rules r WHERE r.category_id = c.id);
