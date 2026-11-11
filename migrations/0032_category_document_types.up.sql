-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Which document types a category holds: both (the default, so every existing category keeps
-- taking everything), policies only, procedures only, or none (a container for child categories).
-- The setting belongs to the category; children do not inherit it.
ALTER TABLE categories
  ADD COLUMN document_types TEXT NOT NULL DEFAULT 'both'
  CONSTRAINT categories_document_types_check
    CHECK (document_types IN ('both', 'policies', 'procedures', 'none'));
