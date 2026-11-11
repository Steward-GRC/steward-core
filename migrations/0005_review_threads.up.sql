-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Inline review threads on a policy version (core#33). A thread is with ONE
-- approver (approver_user_id); the starter (created_by) is the only user who
-- may delete it. Core stores ids only. Threads live and die with the version
-- row: a rejected version keeps them, and they are not copied to a later draft.
CREATE TABLE review_threads (
  id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_version_id  UUID NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  approver_user_id   UUID NOT NULL,
  created_by         UUID NOT NULL,
  quote              TEXT NOT NULL CHECK (btrim(quote) <> ''),
  occurrence         INTEGER NOT NULL CHECK (occurrence >= 1),
  resolved           BOOLEAN NOT NULL DEFAULT false,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX review_threads_version_idx ON review_threads (policy_version_id, created_at);
CREATE INDEX review_threads_approver_idx ON review_threads (policy_version_id, approver_user_id);

CREATE TABLE review_messages (
  id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  thread_id       UUID NOT NULL REFERENCES review_threads(id) ON DELETE CASCADE,
  author_user_id  UUID NOT NULL,
  body            TEXT NOT NULL CHECK (btrim(body) <> ''),
  created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX review_messages_thread_idx ON review_messages (thread_id, created_at);
