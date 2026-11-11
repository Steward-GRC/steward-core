-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Rolling back needs every version of a policy to fit the old rules again, so
-- the unnumbered review states are folded back first: a version in review goes
-- back to a draft when it holds the slot, and rejected versions are archived
-- with a number of their own.
UPDATE policy_versions pv
   SET status = 'archived',
       version_no = n.next_no
  FROM (
    SELECT id,
           (SELECT COALESCE(MAX(version_no), 0) FROM policy_versions o WHERE o.policy_id = r.policy_id)
             + row_number() OVER (PARTITION BY policy_id ORDER BY created_at, id) AS next_no
      FROM policy_versions r
     WHERE status = 'rejected'
  ) n
 WHERE pv.id = n.id;
UPDATE policy_versions SET status = 'draft' WHERE status = 'in_review';

DROP INDEX policy_versions_numbered_idx;
ALTER TABLE policy_versions ADD CONSTRAINT policy_versions_policy_id_version_no_key UNIQUE (policy_id, version_no);
DROP INDEX policy_versions_one_draft_idx;
CREATE UNIQUE INDEX policy_versions_one_draft_idx ON policy_versions (policy_id) WHERE status = 'draft';
