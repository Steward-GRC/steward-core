-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Semantic versions (MAJOR.MINOR.FIX). version_no stays the ordinal of a numbered version, so
-- ordering, the numbered-version index and everything that reads version_no are unchanged; the
-- three parts sit beside it and are written only when a version is published (0.0.0 until then).
--
-- A version that was numbered before this migration read as N.0.0 in the web (it had no better
-- number), so it is backfilled as version_no.0.0 and the history does not change under readers.
-- Core's migrations are forward-only, so this is a new migration, not an edit of 0001.
--
-- Policy versions also keep the change type: what core suggested at submit, the type the
-- version will publish as, a lowering the author asked for that is waiting for the approvers to
-- agree, the reason given, who set it, and who agreed.
ALTER TABLE policy_versions
  ADD COLUMN major INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN minor INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN fix   INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN suggested_change_type TEXT,
  ADD COLUMN change_type           TEXT,
  ADD COLUMN requested_change_type TEXT,
  ADD COLUMN change_type_reason    TEXT NOT NULL DEFAULT '',
  ADD COLUMN change_type_set_by    UUID,
  ADD COLUMN change_type_agreed_by UUID[] NOT NULL DEFAULT '{}',
  ADD CONSTRAINT policy_versions_change_types_valid CHECK (
    (suggested_change_type IS NULL OR suggested_change_type IN ('major', 'minor', 'fix'))
    AND (change_type IS NULL OR change_type IN ('major', 'minor', 'fix'))
    AND (requested_change_type IS NULL OR requested_change_type IN ('major', 'minor', 'fix'))
  );

UPDATE policy_versions
   SET major = version_no
 WHERE status NOT IN ('draft', 'in_review', 'rejected') AND version_no > 0;

CREATE UNIQUE INDEX policy_versions_semver_idx ON policy_versions (policy_id, major, minor, fix)
  WHERE status NOT IN ('draft', 'in_review', 'rejected');

-- Template versions are numbered the same way, at publish, from the change type classified
-- against the version before. published_at dates the version history.
ALTER TABLE template_versions
  ADD COLUMN major INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN minor INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN fix   INTEGER NOT NULL DEFAULT 0,
  ADD COLUMN change_type TEXT CHECK (change_type IS NULL OR change_type IN ('major', 'minor', 'fix')),
  ADD COLUMN published_at TIMESTAMPTZ;

UPDATE template_versions
   SET major = version_no, published_at = created_at
 WHERE status IN ('published', 'archived');

CREATE UNIQUE INDEX template_versions_semver_idx ON template_versions (template_id, major, minor, fix)
  WHERE status IN ('published', 'archived');
