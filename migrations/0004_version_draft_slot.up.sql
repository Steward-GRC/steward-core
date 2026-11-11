-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The one working copy per policy is a draft or a draft submitted for review.
-- A rejected version is final: it does not hold the slot, so the author's next
-- save inserts a new draft beside it.
DROP INDEX policy_versions_one_draft_idx;
CREATE UNIQUE INDEX policy_versions_one_draft_idx ON policy_versions (policy_id)
  WHERE status IN ('draft', 'in_review');

-- Drafts, versions in review and rejected versions are not numbered (version_no
-- stays 0), so the number is unique only among the numbered statuses.
ALTER TABLE policy_versions DROP CONSTRAINT policy_versions_policy_id_version_no_key;
CREATE UNIQUE INDEX policy_versions_numbered_idx ON policy_versions (policy_id, version_no)
  WHERE status NOT IN ('draft', 'in_review', 'rejected');
