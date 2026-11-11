-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- What each approver last reviewed. A version in review is edited in place, so
-- the snapshot is a COPY of the content, not a reference to it. One row per
-- approver per version, taken on the approver's first read and moved when they
-- act. The rows go with the version.
CREATE TABLE review_snapshots (
  policy_version_id UUID        NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  user_id           UUID        NOT NULL,
  content           JSONB       NOT NULL,
  taken_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (policy_version_id, user_id)
);
