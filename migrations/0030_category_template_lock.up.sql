-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- A category can lock its default template (core#38): a policy created under it, or under a
-- category that inherits its template, must use that template. Only meaningful while
-- default_template_id is set; the store clears it when the template is removed.
ALTER TABLE categories
  ADD COLUMN default_template_locked BOOLEAN NOT NULL DEFAULT false;
