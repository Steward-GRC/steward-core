-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The version-bound policy_appendices table is never touched by this
-- migration, so rolling back only drops the policy-owned pair.
DROP TABLE IF EXISTS policy_appendix_versions;
DROP TABLE IF EXISTS policy_owned_appendices;
