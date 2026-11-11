-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- A definition's title is its key: unique across every category (archived
-- entries included), ignoring case and surrounding spaces.
CREATE UNIQUE INDEX definitions_term_unique_idx ON definitions (lower(btrim(term)));
