-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Drops the column only. The Everyone: Read rules the up migration added stay:
-- they cannot be told apart from rules an admin has since written.
ALTER TABLE category_rules DROP COLUMN IF EXISTS grant_manage;
