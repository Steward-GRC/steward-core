-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The admin setting for which kinds of file the editor's File card accepts,
-- one row. Every kind is on until an admin turns one off. Kinds that are never
-- accepted (web pages, scripts, executables, macro-enabled Office files) are
-- not stored here: they are refused in code whatever this says.
CREATE TABLE asset_settings (
  id            BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),
  allowed_kinds TEXT[]      NOT NULL DEFAULT ARRAY['pdf','image','video','word','text','markdown','office'],
  updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by    TEXT        NOT NULL DEFAULT ''
);
INSERT INTO asset_settings (id) VALUES (true);
