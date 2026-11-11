-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The library links of a document belong to its version. The library items
-- (definitions, references, contact blocks) stay unversioned; a link row ties
-- one of them, or another policy, to a policy version, so it is approved with
-- that version, stays as approved once published and is copied into a new
-- draft. References and contact blocks keep the author's order (ordinal);
-- definitions have none and are listed alphabetically.

CREATE TABLE policy_version_references (
  policy_version_id UUID    NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  reference_id      UUID    NOT NULL REFERENCES "references"(id) ON DELETE RESTRICT,
  ordinal           INTEGER NOT NULL,
  PRIMARY KEY (policy_version_id, reference_id),
  UNIQUE (policy_version_id, ordinal)
);
CREATE INDEX policy_version_references_reference_idx ON policy_version_references (reference_id);

CREATE TABLE policy_version_contact_blocks (
  policy_version_id UUID    NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  contact_block_id  UUID    NOT NULL REFERENCES contact_blocks(id) ON DELETE CASCADE,
  ordinal           INTEGER NOT NULL,
  PRIMARY KEY (policy_version_id, contact_block_id),
  UNIQUE (policy_version_id, ordinal)
);
CREATE INDEX policy_version_contact_blocks_block_idx ON policy_version_contact_blocks (contact_block_id);

CREATE TABLE policy_version_definitions (
  policy_version_id UUID NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  definition_id     UUID NOT NULL REFERENCES definitions(id) ON DELETE RESTRICT,
  PRIMARY KEY (policy_version_id, definition_id)
);
CREATE INDEX policy_version_definitions_definition_idx ON policy_version_definitions (definition_id);

-- A related item is another policy and how this version relates to it. The
-- relation_type ids are the fixed list kept in the application; the other
-- policy reads the link back under the inverse type.
CREATE TABLE policy_version_relations (
  id                UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
  policy_version_id UUID        NOT NULL REFERENCES policy_versions(id) ON DELETE CASCADE,
  related_policy_id UUID        NOT NULL REFERENCES policies(id) ON DELETE CASCADE,
  relation_type     TEXT        NOT NULL,
  ordinal           INTEGER     NOT NULL,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
  UNIQUE (policy_version_id, related_policy_id, relation_type),
  UNIQUE (policy_version_id, ordinal)
);
CREATE INDEX policy_version_relations_related_idx ON policy_version_relations (related_policy_id);

-- Move the per-policy links onto the versions that carry them now: each
-- policy's published version and its working copy (the version holding the
-- draft slot). A plain related link takes the RELATED_TO type.
INSERT INTO policy_version_references (policy_version_id, reference_id, ordinal)
SELECT v.id, l.reference_id, l.order_index
  FROM policy_references l
  JOIN policies p ON p.id = l.policy_id
  JOIN policy_versions v ON v.policy_id = p.id
   AND (v.id = p.current_published_version_id OR v.status IN ('draft', 'in_review'));

INSERT INTO policy_version_contact_blocks (policy_version_id, contact_block_id, ordinal)
SELECT v.id, l.contact_block_id, l.ordinal
  FROM policy_contact_blocks l
  JOIN policies p ON p.id = l.policy_id
  JOIN policy_versions v ON v.policy_id = p.id
   AND (v.id = p.current_published_version_id OR v.status IN ('draft', 'in_review'));

INSERT INTO policy_version_definitions (policy_version_id, definition_id)
SELECT v.id, l.definition_id
  FROM policy_definition_attachments l
  JOIN policies p ON p.id = l.policy_id
  JOIN policy_versions v ON v.policy_id = p.id
   AND (v.id = p.current_published_version_id OR v.status IN ('draft', 'in_review'));

INSERT INTO policy_version_relations (policy_version_id, related_policy_id, relation_type, ordinal)
SELECT v.id, l.related_policy_id, 'RELATED_TO', l.ordinal
  FROM policy_relations l
  JOIN policies p ON p.id = l.policy_id
  JOIN policy_versions v ON v.policy_id = p.id
   AND (v.id = p.current_published_version_id OR v.status IN ('draft', 'in_review'));
