-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

CREATE TYPE document_type AS ENUM ('policy', 'procedure');
CREATE TYPE policy_version_status AS ENUM ('draft', 'published', 'superseded', 'archived');
CREATE TYPE sensitivity AS ENUM ('standard', 'sensitive');
CREATE TYPE template_version_status AS ENUM ('draft', 'published', 'archived');

-- The category tree. A NULL audience, exclusion or ack_everyone value
-- inherits from the nearest ancestor; an empty array is an explicit "none".
CREATE TABLE categories (
  id                    UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  parent_id             UUID        REFERENCES categories(id) ON DELETE RESTRICT,
  name                  TEXT        NOT NULL,
  slug                  TEXT        NOT NULL,
  default_template_id   UUID,
  default_workflow_id   UUID,
  created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
  owners                UUID[]      NOT NULL DEFAULT '{}',
  audience_group_ids    TEXT[],
  ack_triggers          TEXT        NOT NULL DEFAULT 'none',
  review_cadence        TEXT        NOT NULL DEFAULT 'none',
  review_date           DATE,
  ack_everyone          BOOLEAN,
  exclusion_group_ids   TEXT[],
  default_template_none BOOLEAN     NOT NULL DEFAULT false,
  -- The number segment of the documents homed here.
  code                  TEXT,
  CONSTRAINT categories_code_key UNIQUE (code),
  CONSTRAINT categories_parent_id_slug_key UNIQUE (parent_id, slug),
  CONSTRAINT categories_ack_triggers_chk CHECK (ack_triggers IN ('none', 'on-publish', 'on-change')),
  CONSTRAINT categories_review_cadence_chk CHECK (review_cadence IN ('none', 'annual', 'biennial', 'on-date'))
);
CREATE INDEX categories_parent_id_idx ON categories (parent_id);
-- Slugs are unique among siblings, roots included.
CREATE UNIQUE INDEX categories_parent_slug_key
  ON categories (COALESCE(parent_id, '00000000-0000-0000-0000-000000000000'::uuid), slug);

-- The last number used per category and document type.
CREATE TABLE category_policy_seq (
  category_id   UUID          NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  last_seq      INTEGER       NOT NULL DEFAULT 0,
  document_type document_type NOT NULL DEFAULT 'policy',
  PRIMARY KEY (category_id, document_type)
);

-- Ordered access rules; the first rule with an opinion wins.
CREATE TABLE category_rules (
  id            UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
  category_id   UUID    NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  ordinal       INTEGER NOT NULL,
  subject_kind  TEXT    NOT NULL,
  subject_ref   TEXT    NOT NULL DEFAULT '',
  grant_read    TEXT    NOT NULL DEFAULT '',
  grant_ack     TEXT    NOT NULL DEFAULT '',
  grant_approve TEXT    NOT NULL DEFAULT '',
  grant_author  TEXT    NOT NULL DEFAULT '',
  UNIQUE (category_id, ordinal),
  CHECK (subject_kind IN ('everyone', 'group', 'user')),
  CHECK (grant_read IN ('', 'allow', 'deny')),
  CHECK (grant_ack IN ('', 'allow', 'deny')),
  CHECK (grant_approve IN ('', 'allow', 'deny')),
  CHECK (grant_author IN ('', 'allow', 'deny'))
);
CREATE INDEX category_rules_category_idx ON category_rules (category_id, ordinal);

CREATE TABLE contact_blocks (
  id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name       TEXT        NOT NULL,
  role       TEXT        NOT NULL DEFAULT '',
  department TEXT        NOT NULL DEFAULT '',
  email      TEXT        NOT NULL DEFAULT '',
  phone      TEXT        NOT NULL DEFAULT '',
  hours      TEXT        NOT NULL DEFAULT '',
  notes      TEXT        NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  label      TEXT        NOT NULL DEFAULT '',
  archived   BOOLEAN     NOT NULL DEFAULT false
);
CREATE INDEX contact_blocks_active_label_idx ON contact_blocks (archived, label);

CREATE TABLE definitions (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  category_id        UUID        NOT NULL REFERENCES categories(id) ON DELETE RESTRICT,
  term               TEXT        NOT NULL,
  definition         TEXT        NOT NULL DEFAULT '',
  archived           BOOLEAN     NOT NULL DEFAULT false,
  created_by_user_id TEXT        NOT NULL DEFAULT '',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX definitions_category_active_term_idx ON definitions (category_id, archived, term);

CREATE TABLE editor_assets (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  storage_key        TEXT        NOT NULL UNIQUE,
  content_type       TEXT        NOT NULL,
  size_bytes         BIGINT      NOT NULL,
  filename           TEXT        NOT NULL DEFAULT '',
  created_by_user_id TEXT        NOT NULL,
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX editor_assets_created_by_idx ON editor_assets (created_by_user_id, created_at);

-- The outbound email-service configuration, one row. The API key is stored
-- encrypted; every value is an adopter setting with an empty default.
CREATE TABLE email_service_config (
  id                 BOOLEAN     PRIMARY KEY DEFAULT true CHECK (id),
  domain             TEXT        NOT NULL DEFAULT '',
  region             TEXT        NOT NULL DEFAULT '',
  from_address       TEXT        NOT NULL DEFAULT '',
  enabled            BOOLEAN     NOT NULL DEFAULT false,
  updated_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_by         TEXT        NOT NULL DEFAULT '',
  api_key_ciphertext BYTEA       NOT NULL DEFAULT ''::bytea,
  provider           TEXT        NOT NULL DEFAULT ''
);
INSERT INTO email_service_config (id) VALUES (true);

-- The global settings, one row.
CREATE TABLE global_settings (
  id                   BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
  announcement_enabled BOOLEAN NOT NULL DEFAULT false,
  announcement_level   TEXT    NOT NULL DEFAULT 'info',
  announcement_message TEXT    NOT NULL DEFAULT '',
  maintenance_enabled  BOOLEAN NOT NULL DEFAULT false,
  maintenance_message  TEXT    NOT NULL DEFAULT ''
);
INSERT INTO global_settings (id) VALUES (true);

CREATE TABLE templates (
  id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  name              TEXT        NOT NULL,
  owner_category_id UUID        REFERENCES categories(id) ON DELETE RESTRICT,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  -- TPL-NNN, from template_code_seq.
  code              TEXT        NOT NULL,
  retired_at        TIMESTAMPTZ,
  CONSTRAINT templates_code_unique UNIQUE (code)
);
CREATE SEQUENCE template_code_seq;

ALTER TABLE categories
  ADD CONSTRAINT categories_default_template_id_fk
  FOREIGN KEY (default_template_id) REFERENCES templates(id) ON DELETE SET NULL;

CREATE TABLE template_versions (
  id          UUID                    PRIMARY KEY DEFAULT gen_random_uuid(),
  template_id UUID                    NOT NULL REFERENCES templates(id) ON DELETE CASCADE,
  version_no  INTEGER                 NOT NULL,
  status      template_version_status NOT NULL DEFAULT 'draft',
  sections    JSONB                   NOT NULL DEFAULT '[]',
  created_at  TIMESTAMPTZ             NOT NULL DEFAULT now(),
  UNIQUE (template_id, version_no)
);
CREATE INDEX template_versions_template_id_idx ON template_versions (template_id);

-- Policies and procedures. The display number is derived from the home
-- category's code, the document type and sequence.
CREATE TABLE policies (
  id                           UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
  home_category_id             UUID          NOT NULL REFERENCES categories(id) ON DELETE RESTRICT,
  title                        TEXT          NOT NULL,
  sensitivity                  sensitivity   NOT NULL DEFAULT 'standard',
  effective_date               DATE,
  review_date                  DATE,
  owner_user_id                UUID          NOT NULL,
  current_published_version_id UUID,
  template_id                  UUID          REFERENCES templates(id) ON DELETE RESTRICT,
  created_at                   TIMESTAMPTZ   NOT NULL DEFAULT now(),
  updated_at                   TIMESTAMPTZ   NOT NULL DEFAULT now(),
  ack_triggers                 TEXT,
  ack_audience_override        UUID[],
  template_none                BOOLEAN       NOT NULL DEFAULT false,
  sequence                     INTEGER       NOT NULL,
  retired_at                   TIMESTAMPTZ,
  document_type                document_type NOT NULL DEFAULT 'policy',
  CONSTRAINT policies_home_category_id_document_type_sequence_key UNIQUE (home_category_id, document_type, sequence),
  CONSTRAINT policies_ack_triggers_chk CHECK (ack_triggers IS NULL OR ack_triggers IN ('none', 'on-publish', 'on-change'))
);
CREATE INDEX policies_owner_user_id_idx ON policies (owner_user_id);

CREATE TABLE policy_versions (
  id                    UUID                  PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_id             UUID                  NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  version_no            INTEGER               NOT NULL,
  status                policy_version_status NOT NULL DEFAULT 'draft',
  -- NULL for a freeform document.
  template_version_id   UUID                  REFERENCES template_versions(id) ON DELETE RESTRICT,
  content               JSONB                 NOT NULL DEFAULT '{}',
  created_at            TIMESTAMPTZ           NOT NULL DEFAULT now(),
  published_at          TIMESTAMPTZ,
  created_by            UUID                  NOT NULL,
  supersedes_version_id UUID                  REFERENCES policy_versions(id) ON DELETE SET NULL,
  -- A staged rename, applied when this draft is published.
  proposed_title        TEXT,
  UNIQUE (policy_id, version_no)
);
CREATE UNIQUE INDEX policy_versions_one_draft_idx ON policy_versions (policy_id) WHERE status = 'draft';
CREATE INDEX policy_versions_policy_id_idx ON policy_versions (policy_id);

ALTER TABLE policies
  ADD CONSTRAINT policies_current_published_version_id_fk
  FOREIGN KEY (current_published_version_id) REFERENCES policy_versions(id) ON DELETE SET NULL;

-- Appendices belong to one version and are copied forward into a new draft.
CREATE TABLE policy_appendices (
  id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_version_id UUID        NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  title             TEXT        NOT NULL,
  content           JSONB       NOT NULL DEFAULT '{}',
  order_index       INTEGER     NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (policy_version_id, order_index)
);
CREATE INDEX policy_appendices_version_idx ON policy_appendices (policy_version_id);

CREATE TABLE policy_contact_blocks (
  id               UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_id        UUID    NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  contact_block_id UUID    NOT NULL REFERENCES contact_blocks(id) ON DELETE CASCADE,
  ordinal          INTEGER NOT NULL,
  UNIQUE (policy_id, contact_block_id),
  UNIQUE (policy_id, ordinal)
);
CREATE INDEX policy_contact_blocks_block_idx ON policy_contact_blocks (contact_block_id);
CREATE INDEX policy_contact_blocks_policy_idx ON policy_contact_blocks (policy_id, ordinal);

CREATE TABLE policy_definition_attachments (
  policy_id     UUID    NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  definition_id UUID    NOT NULL REFERENCES definitions(id) ON DELETE RESTRICT,
  order_index   INTEGER NOT NULL,
  PRIMARY KEY (policy_id, definition_id),
  UNIQUE (policy_id, order_index)
);
CREATE INDEX policy_definition_attachments_definition_idx ON policy_definition_attachments (definition_id);
CREATE INDEX policy_definition_attachments_policy_idx ON policy_definition_attachments (policy_id, order_index);

CREATE TABLE "references" (
  id                 UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  label              TEXT        NOT NULL,
  kind               TEXT        NOT NULL,
  clause             TEXT        NOT NULL DEFAULT '',
  body               TEXT        NOT NULL DEFAULT '',
  url                TEXT        NOT NULL DEFAULT '',
  archived           BOOLEAN     NOT NULL DEFAULT false,
  created_by_user_id TEXT        NOT NULL DEFAULT '',
  created_at         TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX references_active_label_idx ON "references" (archived, label);

CREATE TABLE policy_references (
  policy_id    UUID    NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  reference_id UUID    NOT NULL REFERENCES "references"(id) ON DELETE RESTRICT,
  order_index  INTEGER NOT NULL,
  PRIMARY KEY (policy_id, reference_id),
  UNIQUE (policy_id, order_index)
);
CREATE INDEX policy_references_policy_idx ON policy_references (policy_id, order_index);
CREATE INDEX policy_references_reference_idx ON policy_references (reference_id);

-- Related links: one-way and not versioned.
CREATE TABLE policy_relations (
  id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_id         UUID        NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  related_policy_id UUID        NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  ordinal           INTEGER     NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (policy_id, related_policy_id),
  UNIQUE (policy_id, ordinal),
  CHECK (policy_id <> related_policy_id)
);
CREATE INDEX policy_relations_policy_idx ON policy_relations (policy_id, ordinal);
CREATE INDEX policy_relations_related_idx ON policy_relations (related_policy_id);
