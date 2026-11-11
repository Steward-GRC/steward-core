-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Appendices that belong to the policy, not to one of its versions. Each has a
-- version table of its own: at most one DRAFT beside a PUBLISHED history, and
-- readers see the latest published row. Retiring keeps an appendix and its
-- history and hides it. The version-bound policy_appendices table from the
-- baseline stays as it is, so AppendixService keeps working unchanged.

CREATE TABLE policy_owned_appendices (
  id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_id   UUID        NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  -- Zero-based among the live appendices of the policy. A retired appendix
  -- keeps its old value, which no live one may reuse, so the uniqueness below
  -- covers live rows only.
  order_index INTEGER     NOT NULL CHECK (order_index >= 0),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by  UUID        NOT NULL,
  retired_at  TIMESTAMPTZ,
  retired_by  UUID,
  CONSTRAINT policy_owned_appendices_retired_chk CHECK ((retired_at IS NULL) = (retired_by IS NULL))
);
CREATE INDEX policy_owned_appendices_policy_idx ON policy_owned_appendices (policy_id);
CREATE UNIQUE INDEX policy_owned_appendices_live_order_idx
  ON policy_owned_appendices (policy_id, order_index) WHERE retired_at IS NULL;

CREATE TABLE policy_appendix_versions (
  id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  appendix_id  UUID        NOT NULL REFERENCES policy_owned_appendices(id) ON DELETE CASCADE,
  version_no   INTEGER     NOT NULL CHECK (version_no >= 1),
  state        TEXT        NOT NULL CHECK (state IN ('DRAFT', 'PUBLISHED')),
  title        TEXT        NOT NULL,
  content_json JSONB       NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  created_by   UUID        NOT NULL,
  updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by   UUID        NOT NULL,
  published_at TIMESTAMPTZ,
  published_by UUID,
  -- Everyone who saved or published this row, each once, in the order they first did.
  editors      UUID[]      NOT NULL DEFAULT '{}',
  CONSTRAINT policy_appendix_versions_appendix_version_key UNIQUE (appendix_id, version_no),
  CONSTRAINT policy_appendix_versions_published_chk CHECK ((state = 'PUBLISHED') = (published_at IS NOT NULL))
);
-- At most one draft per appendix.
CREATE UNIQUE INDEX policy_appendix_versions_one_draft_idx
  ON policy_appendix_versions (appendix_id) WHERE state = 'DRAFT';

-- Carry the existing appendices over. Per policy, the newest version that has
-- appendices is the source. Each of its appendices becomes v1 of an appendix of
-- the policy (keeping its id), PUBLISHED when that version was published and a
-- DRAFT when it never was. The author of the source version stands in for the
-- author and publisher, which the old rows did not record.
WITH src AS (
  SELECT DISTINCT ON (pv.policy_id) pv.id, pv.policy_id, pv.created_at, pv.published_at, pv.created_by
    FROM policy_versions pv
   WHERE EXISTS (SELECT 1 FROM policy_appendices a WHERE a.policy_version_id = pv.id)
   ORDER BY pv.policy_id, pv.created_at DESC, pv.id DESC
), ordered AS (
  SELECT a.id, s.policy_id, s.created_at, s.published_at, s.created_by, a.title, a.content,
         row_number() OVER (PARTITION BY s.policy_id ORDER BY a.order_index, a.created_at, a.id) - 1 AS idx
    FROM src s
    JOIN policy_appendices a ON a.policy_version_id = s.id
), made AS (
  INSERT INTO policy_owned_appendices (id, policy_id, order_index, created_at, created_by)
  SELECT id, policy_id, idx, created_at, created_by FROM ordered
  RETURNING id
)
INSERT INTO policy_appendix_versions
  (appendix_id, version_no, state, title, content_json, created_at, created_by, updated_at, updated_by,
   published_at, published_by, editors)
SELECT o.id, 1,
       CASE WHEN o.published_at IS NULL THEN 'DRAFT' ELSE 'PUBLISHED' END,
       o.title, o.content, o.created_at, o.created_by,
       COALESCE(o.published_at, o.created_at), o.created_by,
       o.published_at,
       CASE WHEN o.published_at IS NULL THEN NULL ELSE o.created_by END,
       ARRAY[o.created_by]
  FROM ordered o;
