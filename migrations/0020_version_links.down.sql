-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The per-policy tables are still in place (they are retired separately), so
-- dropping the version tables loses only the per-version split.
DROP TABLE policy_version_relations;
DROP TABLE policy_version_definitions;
DROP TABLE policy_version_contact_blocks;
DROP TABLE policy_version_references;
