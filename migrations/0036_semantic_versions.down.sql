-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

DROP INDEX template_versions_semver_idx;
ALTER TABLE template_versions
  DROP COLUMN published_at,
  DROP COLUMN change_type,
  DROP COLUMN fix,
  DROP COLUMN minor,
  DROP COLUMN major;

DROP INDEX policy_versions_semver_idx;
ALTER TABLE policy_versions
  DROP CONSTRAINT policy_versions_change_types_valid,
  DROP COLUMN change_type_agreed_by,
  DROP COLUMN change_type_set_by,
  DROP COLUMN change_type_reason,
  DROP COLUMN requested_change_type,
  DROP COLUMN change_type,
  DROP COLUMN suggested_change_type,
  DROP COLUMN fix,
  DROP COLUMN minor,
  DROP COLUMN major;
