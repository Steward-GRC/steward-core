-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

DROP TABLE IF EXISTS policy_relations, policy_references, "references", policy_definition_attachments,
  policy_contact_blocks, policy_appendices CASCADE;
ALTER TABLE IF EXISTS policies DROP CONSTRAINT IF EXISTS policies_current_published_version_id_fk;
DROP TABLE IF EXISTS policy_versions, policies, template_versions CASCADE;
ALTER TABLE IF EXISTS categories DROP CONSTRAINT IF EXISTS categories_default_template_id_fk;
DROP TABLE IF EXISTS templates, global_settings, email_service_config, editor_assets, definitions,
  contact_blocks, category_rules, category_policy_seq, categories CASCADE;
DROP SEQUENCE IF EXISTS template_code_seq;
DROP TYPE IF EXISTS template_version_status, sensitivity, policy_version_status, document_type;
