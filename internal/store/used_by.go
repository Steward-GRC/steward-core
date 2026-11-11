// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "fmt"

// usedBySQL returns a scalar subquery that counts the policies using the
// library item whose row is aliased alias. A policy counts once however many of
// its versions link the item, and only its live versions count: the published
// version and the one holding the draft slot. The older per-policy links are
// counted too until they are retired, so the number stays right while both are
// written.
func usedBySQL(alias, legacyTable, legacyCol, versionTable, versionCol string) string {
	return fmt.Sprintf(`(SELECT COUNT(*) FROM (
		   SELECT policy_id FROM %[2]s WHERE %[3]s = %[1]s.id
		   UNION
		   SELECT v.policy_id FROM %[4]s l
		     JOIN policy_versions v ON v.id = l.policy_version_id
		     JOIN policies p ON p.id = v.policy_id
		    WHERE l.%[5]s = %[1]s.id AND `+livePolicyVersion+`
		 ) users)`, alias, legacyTable, legacyCol, versionTable, versionCol)
}

func usedByReferences(alias string) string {
	return usedBySQL(alias, "policy_references", "reference_id", "policy_version_references", "reference_id")
}

func usedByContacts(alias string) string {
	return usedBySQL(alias, "policy_contact_blocks", "contact_block_id", "policy_version_contact_blocks", "contact_block_id")
}

func usedByDefinitions(alias string) string {
	return usedBySQL(alias, "policy_definition_attachments", "definition_id", "policy_version_definitions", "definition_id")
}
