-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

DROP TRIGGER policy_search_appendix_version_update ON policy_appendix_versions;
DROP TRIGGER policy_search_appendix_version_insert ON policy_appendix_versions;
DROP TRIGGER policy_search_appendix ON policy_owned_appendices;
DROP TRIGGER policy_search_version ON policy_versions;
DROP TRIGGER policy_search_policy ON policies;
DROP FUNCTION policy_search_on_appendix_version();
DROP FUNCTION policy_search_on_appendix();
DROP FUNCTION policy_search_on_version();
DROP FUNCTION policy_search_on_policy();
DROP FUNCTION policy_search_refresh(UUID);
DROP FUNCTION policy_search_text(JSONB);
DROP TABLE policy_search;
