// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// setupCategoryAndTemplate creates a single category and a published template
// version against the provided stores. It's used by every PolicyStore test
// since a policy needs both a home category (for the number sequence) and a
// published template version (to pin policy_version.template_version_id).
func setupCategoryAndTemplate(t *testing.T, gs *store.CategoryStore, ts *store.TemplateStore) (domain.Category, domain.TemplateVersion) {
	t.Helper()
	ctx := context.Background()

	// Unique slug per call: category slugs are now globally unique, so helpers
	// invoked more than once against the same test DB (e.g. appendix
	// makeVersion) must not reuse a fixed slug. The base stays "it" so the
	// derived category code still starts IT.
	g, err := domain.NewCategory("IT", "it-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	g, err = gs.Create(ctx, g)
	if err != nil {
		t.Fatalf("gs.Create: %v", err)
	}

	tpl, err := domain.NewTemplate("Standard", uuid.Nil)
	if err != nil {
		t.Fatalf("NewTemplate: %v", err)
	}
	tpl, err = ts.CreateTemplate(ctx, tpl)
	if err != nil {
		t.Fatalf("CreateTemplate: %v", err)
	}

	sections := []domain.Section{{
		Key:    "purpose",
		Title:  "Purpose",
		Order:  1,
		Blocks: []domain.Block{{Type: domain.BlockTypeEditable}},
	}}
	tv, err := domain.NewTemplateVersion(tpl.ID.String(), sections)
	if err != nil {
		t.Fatalf("NewTemplateVersion: %v", err)
	}
	tv, err = ts.CreateTemplateVersion(ctx, tv)
	if err != nil {
		t.Fatalf("CreateTemplateVersion: %v", err)
	}
	tv, err = ts.PublishTemplateVersion(ctx, tv.ID)
	if err != nil {
		t.Fatalf("PublishTemplateVersion: %v", err)
	}
	return g, tv
}

func TestPolicyStoreCreateAssignsNumber(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	p, err := domain.NewPolicy("IT Security Policy", g.ID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	created, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if created.Number == "" {
		t.Fatal("expected non-empty number")
	}
	// Number fronts the category's authoritative code (assigned by CategoryStore
	// from the slug), followed by the per-category sequence.
	code := store.PolicyNumberCode(g.Slug)
	want1 := "POL-" + code + "-000001"
	if created.Number != want1 {
		t.Fatalf("expected %q, got %q", want1, created.Number)
	}

	// Second policy in same category increments the per-category sequence.
	p2, _ := domain.NewPolicy("IT Access Policy", g.ID, domain.SensitivityStandard, uuid.New())
	created2, err := ps.CreatePolicy(ctx, p2)
	if err != nil {
		t.Fatalf("CreatePolicy 2: %v", err)
	}
	want2 := "POL-" + code + "-000002"
	if created2.Number != want2 {
		t.Fatalf("expected %q, got %q", want2, created2.Number)
	}
}

// TestPolicyStoreCreateProcedureNumbersIndependently checks that a procedure
// renders PRC-<code>-<seq> from its own counter, and that creating procedures
// leaves the policy sequence in the same category alone.
func TestPolicyStoreCreateProcedureNumbersIndependently(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)
	code := store.PolicyNumberCode(g.Slug)

	create := func(title string, dt domain.DocumentType) domain.Policy {
		t.Helper()
		p, err := domain.NewPolicy(title, g.ID, domain.SensitivityStandard, uuid.New())
		if err != nil {
			t.Fatalf("NewPolicy: %v", err)
		}
		p.DocumentType = dt
		created, err := ps.CreatePolicy(ctx, p)
		if err != nil {
			t.Fatalf("CreatePolicy(%s): %v", dt, err)
		}
		return created
	}

	// Policy #1 then procedure #1 in the SAME category: each starts at 000001.
	pol1 := create("IT Security Policy", domain.DocumentTypePolicy)
	if want := "POL-" + code + "-000001"; pol1.Number != want {
		t.Fatalf("policy 1 number: got %q want %q", pol1.Number, want)
	}
	prc1 := create("IT Onboarding Procedure", domain.DocumentTypeProcedure)
	if want := "PRC-" + code + "-000001"; prc1.Number != want {
		t.Fatalf("procedure 1 number: got %q want %q (per-type counter must start at 1)", prc1.Number, want)
	}

	// A second procedure advances only the procedure counter.
	prc2 := create("IT Offboarding Procedure", domain.DocumentTypeProcedure)
	if want := "PRC-" + code + "-000002"; prc2.Number != want {
		t.Fatalf("procedure 2 number: got %q want %q", prc2.Number, want)
	}

	// A second policy advances only the policy counter — unaffected by the two
	// procedures created in between.
	pol2 := create("IT Access Policy", domain.DocumentTypePolicy)
	if want := "POL-" + code + "-000002"; pol2.Number != want {
		t.Fatalf("policy 2 number: got %q want %q (procedure creates must not perturb policy sequence)", pol2.Number, want)
	}
}

// TestPolicyStoreCreatePersistsDocumentType proves the document_type is
// persisted on create and surfaced on read (GetPolicy), and that an unset
// document type on NewPolicy defaults to policy (back-compat).
func TestPolicyStoreCreatePersistsDocumentType(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	// A procedure round-trips as a procedure.
	proc, _ := domain.NewPolicy("Backup Procedure", g.ID, domain.SensitivityStandard, uuid.New())
	proc.DocumentType = domain.DocumentTypeProcedure
	proc, err := ps.CreatePolicy(ctx, proc)
	if err != nil {
		t.Fatalf("CreatePolicy procedure: %v", err)
	}
	got, err := ps.GetPolicy(ctx, proc.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.DocumentType != domain.DocumentTypeProcedure {
		t.Fatalf("GetPolicy document_type: got %q want %q", got.DocumentType, domain.DocumentTypeProcedure)
	}

	// A default (NewPolicy) create round-trips as a policy — the unchanged path.
	pol, _ := domain.NewPolicy("Data Policy", g.ID, domain.SensitivityStandard, uuid.New())
	pol, err = ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy policy: %v", err)
	}
	gotPol, err := ps.GetPolicy(ctx, pol.ID)
	if err != nil {
		t.Fatalf("GetPolicy policy: %v", err)
	}
	if gotPol.DocumentType != domain.DocumentTypePolicy {
		t.Fatalf("default document_type: got %q want %q", gotPol.DocumentType, domain.DocumentTypePolicy)
	}
}

// TestPolicyStoreListPoliciesExcludesProcedures checks that a policy-typed
// ListPolicies returns policies only (the handler maps an unset filter to
// it), while the procedure is still readable with GetPolicy.
func TestPolicyStoreListPoliciesExcludesProcedures(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	pol, _ := domain.NewPolicy("Listed Policy", g.ID, domain.SensitivityStandard, uuid.New())
	pol, err := ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy policy: %v", err)
	}
	proc, _ := domain.NewPolicy("Hidden Procedure", g.ID, domain.SensitivityStandard, uuid.New())
	proc.DocumentType = domain.DocumentTypeProcedure
	proc, err = ps.CreatePolicy(ctx, proc)
	if err != nil {
		t.Fatalf("CreatePolicy procedure: %v", err)
	}

	for _, includeDesc := range []bool{false, true} {
		list, err := ps.ListPolicies(ctx, g.ID, includeDesc, domain.DocumentTypePolicy)
		if err != nil {
			t.Fatalf("ListPolicies(includeDescendants=%v): %v", includeDesc, err)
		}
		var ids []uuid.UUID
		for _, p := range list {
			ids = append(ids, p.ID)
			if p.DocumentType != domain.DocumentTypePolicy {
				t.Fatalf("ListPolicies returned a non-policy: %q (%s)", p.Number, p.DocumentType)
			}
		}
		if !containsID(ids, pol.ID) {
			t.Fatalf("ListPolicies(includeDescendants=%v) missing the policy", includeDesc)
		}
		if containsID(ids, proc.ID) {
			t.Fatalf("ListPolicies(includeDescendants=%v) must NOT include the procedure", includeDesc)
		}
	}
}

// TestPolicyStoreListPoliciesProcedureFilter checks that a procedure-typed
// ListPolicies returns procedures only, from a category holding one of each.
func TestPolicyStoreListPoliciesProcedureFilter(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	pol, _ := domain.NewPolicy("A Policy", g.ID, domain.SensitivityStandard, uuid.New())
	pol, err := ps.CreatePolicy(ctx, pol)
	if err != nil {
		t.Fatalf("CreatePolicy policy: %v", err)
	}
	proc, _ := domain.NewPolicy("A Procedure", g.ID, domain.SensitivityStandard, uuid.New())
	proc.DocumentType = domain.DocumentTypeProcedure
	proc, err = ps.CreatePolicy(ctx, proc)
	if err != nil {
		t.Fatalf("CreatePolicy procedure: %v", err)
	}

	for _, includeDesc := range []bool{false, true} {
		// PROCEDURE filter → procedures only.
		procs, err := ps.ListPolicies(ctx, g.ID, includeDesc, domain.DocumentTypeProcedure)
		if err != nil {
			t.Fatalf("ListPolicies(PROCEDURE, includeDescendants=%v): %v", includeDesc, err)
		}
		var procIDs []uuid.UUID
		for _, p := range procs {
			procIDs = append(procIDs, p.ID)
			if p.DocumentType != domain.DocumentTypeProcedure {
				t.Fatalf("PROCEDURE filter returned a non-procedure: %q (%s)", p.Number, p.DocumentType)
			}
		}
		if !containsID(procIDs, proc.ID) {
			t.Fatalf("PROCEDURE filter(includeDescendants=%v) missing the procedure", includeDesc)
		}
		if containsID(procIDs, pol.ID) {
			t.Fatalf("PROCEDURE filter(includeDescendants=%v) must NOT include the policy", includeDesc)
		}

		// POLICY filter → policies only (regression alongside the procedure path).
		pols, err := ps.ListPolicies(ctx, g.ID, includeDesc, domain.DocumentTypePolicy)
		if err != nil {
			t.Fatalf("ListPolicies(POLICY, includeDescendants=%v): %v", includeDesc, err)
		}
		var polIDs []uuid.UUID
		for _, p := range pols {
			polIDs = append(polIDs, p.ID)
		}
		if !containsID(polIDs, pol.ID) {
			t.Fatalf("POLICY filter(includeDescendants=%v) missing the policy", includeDesc)
		}
		if containsID(polIDs, proc.ID) {
			t.Fatalf("POLICY filter(includeDescendants=%v) must NOT include the procedure", includeDesc)
		}
	}
}

// TestPolicyStoreListPoliciesByOwner checks the owned set is returned, other
// owners are excluded, and the retired filter is honoured.
func TestPolicyStoreListPoliciesByOwner(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	owner := uuid.New()
	other := uuid.New()

	mine1, _ := domain.NewPolicy("Mine One", g.ID, domain.SensitivityStandard, owner)
	mine1, err := ps.CreatePolicy(ctx, mine1)
	if err != nil {
		t.Fatalf("CreatePolicy mine1: %v", err)
	}
	mine2, _ := domain.NewPolicy("Mine Two", g.ID, domain.SensitivityStandard, owner)
	mine2, err = ps.CreatePolicy(ctx, mine2)
	if err != nil {
		t.Fatalf("CreatePolicy mine2: %v", err)
	}
	theirs, _ := domain.NewPolicy("Theirs", g.ID, domain.SensitivityStandard, other)
	if _, err := ps.CreatePolicy(ctx, theirs); err != nil {
		t.Fatalf("CreatePolicy theirs: %v", err)
	}
	if _, err := ps.RetirePolicy(ctx, mine2.ID); err != nil {
		t.Fatalf("RetirePolicy mine2: %v", err)
	}

	// Default: active-only, owned-only.
	active, err := ps.ListPoliciesByOwner(ctx, owner, false)
	if err != nil {
		t.Fatalf("ListPoliciesByOwner active: %v", err)
	}
	var activeIDs []uuid.UUID
	for _, p := range active {
		activeIDs = append(activeIDs, p.ID)
	}
	if len(activeIDs) != 1 || !containsID(activeIDs, mine1.ID) {
		t.Fatalf("active owned set: got %v want [%v]", activeIDs, mine1.ID)
	}

	// include_retired: both owned policies.
	all, err := ps.ListPoliciesByOwner(ctx, owner, true)
	if err != nil {
		t.Fatalf("ListPoliciesByOwner include_retired: %v", err)
	}
	var allIDs []uuid.UUID
	for _, p := range all {
		allIDs = append(allIDs, p.ID)
	}
	if len(allIDs) != 2 || !containsID(allIDs, mine1.ID) || !containsID(allIDs, mine2.ID) {
		t.Fatalf("include_retired owned set: got %v", allIDs)
	}
}

// TestPolicyStoreReassignUserPolicies checks ownership moves to another user
// and user-subject category rules are rewritten in the same transaction,
// without deleting a policy.
func TestPolicyStoreReassignUserPolicies(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	from := uuid.New()
	to := uuid.New()

	p1, _ := domain.NewPolicy("From One", g.ID, domain.SensitivityStandard, from)
	p1, err := ps.CreatePolicy(ctx, p1)
	if err != nil {
		t.Fatalf("CreatePolicy p1: %v", err)
	}
	p2, _ := domain.NewPolicy("From Two", g.ID, domain.SensitivityStandard, from)
	p2, err = ps.CreatePolicy(ctx, p2)
	if err != nil {
		t.Fatalf("CreatePolicy p2: %v", err)
	}
	// A RACI author grant for the source user on the category.
	if _, err := gs.SetCategoryRuleset(ctx, g.ID, []domain.CategoryRule{
		{Ordinal: 1, SubjectKind: "user", SubjectRef: from.String(), Author: "allow"},
	}); err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}

	ids, ownerCount, authorGrants, err := ps.ReassignUserPolicies(ctx, from, to)
	if err != nil {
		t.Fatalf("ReassignUserPolicies: %v", err)
	}
	if ownerCount != 2 || len(ids) != 2 {
		t.Fatalf("owner count/ids: count=%d ids=%v", ownerCount, ids)
	}
	if authorGrants != 1 {
		t.Fatalf("author grants: got %d want 1", authorGrants)
	}

	// Both policies now owned by the target; none deleted.
	got1, err := ps.GetPolicy(ctx, p1.ID)
	if err != nil {
		t.Fatalf("GetPolicy p1: %v", err)
	}
	got2, err := ps.GetPolicy(ctx, p2.ID)
	if err != nil {
		t.Fatalf("GetPolicy p2: %v", err)
	}
	if got1.OwnerUserID != to || got2.OwnerUserID != to {
		t.Fatalf("owner not moved: p1=%v p2=%v want %v", got1.OwnerUserID, got2.OwnerUserID, to)
	}

	// The RACI author grant now points at the target user.
	rules, err := gs.GetCategoryRuleset(ctx, g.ID)
	if err != nil {
		t.Fatalf("GetCategoryRuleset: %v", err)
	}
	if len(rules) != 1 || rules[0].SubjectRef != to.String() {
		t.Fatalf("ruleset not rewritten: %+v", rules)
	}

	// Re-running against the (now source-free) user is a clean no-op.
	ids2, ownerCount2, authorGrants2, err := ps.ReassignUserPolicies(ctx, from, to)
	if err != nil {
		t.Fatalf("ReassignUserPolicies (idempotent): %v", err)
	}
	if ownerCount2 != 0 || len(ids2) != 0 || authorGrants2 != 0 {
		t.Fatalf("second run not a no-op: count=%d ids=%v grants=%d", ownerCount2, ids2, authorGrants2)
	}
}

func containsID(ids []uuid.UUID, want uuid.UUID) bool {
	return slices.Contains(ids, want)
}

// TestPolicyStoreCreateSeedsInitialDraft checks that CreatePolicy seeds one
// freeform version_no=0 draft with content '{}' in the same transaction, and
// that GetPolicy returns its id as CurrentDraftVersionID.
func TestPolicyStoreCreateSeedsInitialDraft(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	owner := uuid.New()
	p, _ := domain.NewPolicy("Seeded Policy", g.ID, domain.SensitivityStandard, owner)
	created, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// CreatePolicy returns the seeded draft's id on the policy.
	if created.CurrentDraftVersionID == uuid.Nil {
		t.Fatal("expected CurrentDraftVersionID to be set on create")
	}

	// Exactly one draft row exists, before any SaveDraft/UpsertDraft call.
	var (
		count      int
		draftID    uuid.UUID
		versionNo  int
		statusStr  string
		tvIsNull   bool
		contentStr string
		createdBy  uuid.UUID
	)
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policy_versions WHERE policy_id=$1 AND status='draft'`,
		created.ID).Scan(&count); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 seeded draft, got %d", count)
	}
	if err := pool.Pool().QueryRow(ctx,
		`SELECT id, version_no, status, template_version_id IS NULL, content::text, created_by
		   FROM policy_versions WHERE policy_id=$1 AND status='draft'`,
		created.ID).Scan(&draftID, &versionNo, &statusStr, &tvIsNull, &contentStr, &createdBy); err != nil {
		t.Fatalf("read seeded draft: %v", err)
	}
	if draftID != created.CurrentDraftVersionID {
		t.Fatalf("draft id mismatch: row %s vs returned %s", draftID, created.CurrentDraftVersionID)
	}
	if versionNo != 0 {
		t.Fatalf("expected seeded draft version_no=0, got %d", versionNo)
	}
	if statusStr != "draft" {
		t.Fatalf("expected status='draft', got %q", statusStr)
	}
	if !tvIsNull {
		t.Fatal("expected seeded draft to be freeform (template_version_id NULL)")
	}
	if contentStr != "{}" {
		t.Fatalf("expected content '{}', got %q", contentStr)
	}
	if createdBy != owner {
		t.Fatalf("expected created_by=%s (owner), got %s", owner, createdBy)
	}

	// GetPolicy surfaces the same draft id.
	got, err := ps.GetPolicy(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.CurrentDraftVersionID != created.CurrentDraftVersionID {
		t.Fatalf("GetPolicy draft id: got %s want %s", got.CurrentDraftVersionID, created.CurrentDraftVersionID)
	}
}

// TestPolicyStoreSetSensitivity verifies the in-place classification flip:
// (a) standard->sensitive persists and is returned, (b) sensitive->standard
// persists (both directions), and (d) the call creates NO new policy_version
// row (the seeded draft count is unchanged).
func TestPolicyStoreSetSensitivity(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Classification Policy", g.ID, domain.SensitivityStandard, uuid.New())
	created, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// Baseline policy_version row count (CreatePolicy seeds exactly one draft).
	versionCount := func() int {
		t.Helper()
		var n int
		if err := pool.Pool().QueryRow(ctx,
			`SELECT COUNT(*) FROM policy_versions WHERE policy_id=$1`, created.ID).Scan(&n); err != nil {
			t.Fatalf("count versions: %v", err)
		}
		return n
	}
	before := versionCount()

	// (a) standard -> sensitive persists and is returned.
	got, err := ps.SetSensitivity(ctx, created.ID, domain.SensitivitySensitive)
	if err != nil {
		t.Fatalf("SetSensitivity standard->sensitive: %v", err)
	}
	if got.Sensitivity != domain.SensitivitySensitive {
		t.Fatalf("returned sensitivity: got %q want sensitive", got.Sensitivity)
	}
	reread, err := ps.GetPolicy(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetPolicy after flip: %v", err)
	}
	if reread.Sensitivity != domain.SensitivitySensitive {
		t.Fatalf("persisted sensitivity: got %q want sensitive", reread.Sensitivity)
	}

	// (d) no new policy_version row was created by the flip.
	if after := versionCount(); after != before {
		t.Fatalf("SetSensitivity created a version row: before=%d after=%d", before, after)
	}

	// (b) sensitive -> standard persists (reverse direction).
	got, err = ps.SetSensitivity(ctx, created.ID, domain.SensitivityStandard)
	if err != nil {
		t.Fatalf("SetSensitivity sensitive->standard: %v", err)
	}
	if got.Sensitivity != domain.SensitivityStandard {
		t.Fatalf("returned reverse sensitivity: got %q want standard", got.Sensitivity)
	}
	reread, err = ps.GetPolicy(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetPolicy after reverse flip: %v", err)
	}
	if reread.Sensitivity != domain.SensitivityStandard {
		t.Fatalf("persisted reverse sensitivity: got %q want standard", reread.Sensitivity)
	}
	if after := versionCount(); after != before {
		t.Fatalf("reverse SetSensitivity created a version row: before=%d after=%d", before, after)
	}
}

func TestPolicyStoreCreateAndDraft(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	draft, err := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	saved, _, err := ps.UpsertDraft(ctx, draft)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	if saved.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("expected draft status, got %q", saved.Status)
	}
}

func TestPolicyStoreOnlyOneDraft(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Policy A", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	draft1, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, draft1); err != nil {
		t.Fatalf("UpsertDraft 1: %v", err)
	}

	// Second UpsertDraft updates in-place rather than inserting another draft row.
	draft2, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"Updated"}}`)
	if _, _, err := ps.UpsertDraft(ctx, draft2); err != nil {
		t.Fatalf("UpsertDraft 2: %v", err)
	}

	var count int
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policy_versions WHERE policy_id=$1 AND status='draft'`,
		p.ID).Scan(&count); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 draft, got %d", count)
	}
}

// TestPolicyStoreRawSecondDraftRejected verifies the underlying partial-unique
// index is wired correctly: a raw INSERT of a second draft for the same policy
// surfaces as a domain error (ErrDraftAlreadyExists), not a raw pgx 23505.
func TestPolicyStoreRawSecondDraftRejected(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Policy A", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	draft, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, draft); err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}

	// Bypass UpsertDraft to attempt a raw second-draft INSERT. The store
	// should translate the partial-unique-index violation to a domain error.
	secondID := uuid.New()
	err = ps.InsertDraftRaw(ctx, secondID, p.ID, tv.ID, `{"sections":{}}`, uuid.New())
	if err == nil {
		t.Fatal("expected error from second draft insert, got nil")
	}
	if !errors.Is(err, store.ErrDraftAlreadyExists) {
		t.Fatalf("expected ErrDraftAlreadyExists, got %v", err)
	}
}

func TestPolicyStorePublishAssignsNumberAndIncrementsSequence(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	code := store.PolicyNumberCode(g.Slug)

	// Create + publish policy #1 -> number = POL-<code>-000001.
	p1, _ := domain.NewPolicy("Policy 1", g.ID, domain.SensitivityStandard, uuid.New())
	p1, err := ps.CreatePolicy(ctx, p1)
	if err != nil {
		t.Fatalf("CreatePolicy 1: %v", err)
	}
	if want := "POL-" + code + "-000001"; p1.Number != want {
		t.Fatalf("expected %q, got %q", want, p1.Number)
	}
	d1, _ := domain.NewPolicyVersionDraft(p1.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, d1); err != nil {
		t.Fatalf("UpsertDraft 1: %v", err)
	}
	pub1, err := ps.PublishDraft(ctx, p1.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft 1: %v", err)
	}
	if pub1.VersionNo != 1 {
		t.Fatalf("expected version_no=1, got %d", pub1.VersionNo)
	}

	// Create + publish policy #2 in the same category -> number = POL-<code>-000002.
	p2, _ := domain.NewPolicy("Policy 2", g.ID, domain.SensitivityStandard, uuid.New())
	p2, err = ps.CreatePolicy(ctx, p2)
	if err != nil {
		t.Fatalf("CreatePolicy 2: %v", err)
	}
	if want := "POL-" + code + "-000002"; p2.Number != want {
		t.Fatalf("expected %q, got %q", want, p2.Number)
	}
}

func TestPolicyStorePublishDraftFreezesVersion(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Policy B", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	draft, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, draft); err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}

	published, err := ps.PublishDraft(ctx, p.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if published.Status != domain.PolicyVersionStatusPublished {
		t.Fatalf("expected published, got %q", published.Status)
	}
	if published.VersionNo != 1 {
		t.Fatalf("expected version_no=1, got %d", published.VersionNo)
	}

	// After publishing there should be no remaining draft row.
	var count int
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policy_versions WHERE policy_id=$1 AND status='draft'`,
		p.ID).Scan(&count); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected 0 drafts after publish, got %d", count)
	}
}

func TestPolicyStorePublishSupersedesPriorVersion(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// First publish.
	d1, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, d1); err != nil {
		t.Fatalf("UpsertDraft 1: %v", err)
	}
	pub1, err := ps.PublishDraft(ctx, p.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft 1: %v", err)
	}

	// Verify policies.current_published_version_id points at pub1.
	gotP, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy after publish 1: %v", err)
	}
	if gotP.CurrentPublishedVersionID != pub1.ID {
		t.Fatalf("expected current_published_version_id=%v, got %v",
			pub1.ID, gotP.CurrentPublishedVersionID)
	}

	// Second draft + publish.
	d2, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"v2"}}`)
	if _, _, err := ps.UpsertDraft(ctx, d2); err != nil {
		t.Fatalf("UpsertDraft 2: %v", err)
	}
	pub2, err := ps.PublishDraft(ctx, p.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft 2: %v", err)
	}
	if pub2.VersionNo != 2 {
		t.Fatalf("expected version_no=2, got %d", pub2.VersionNo)
	}

	// Prior version is now 'superseded'.
	prior, err := ps.GetPolicyVersion(ctx, pub1.ID)
	if err != nil {
		t.Fatalf("GetPolicyVersion pub1: %v", err)
	}
	if prior.Status != domain.PolicyVersionStatusSuperseded {
		t.Fatalf("expected superseded, got %q", prior.Status)
	}

	// policies.current_published_version_id now points at pub2.
	gotP, err = ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy after publish 2: %v", err)
	}
	if gotP.CurrentPublishedVersionID != pub2.ID {
		t.Fatalf("expected current_published_version_id=%v, got %v",
			pub2.ID, gotP.CurrentPublishedVersionID)
	}
}

func TestPolicyStoreIsTemplateUpdateAvailable(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Policy C", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	draft, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, draft); err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	published, err := ps.PublishDraft(ctx, p.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}

	// Not flagged yet -- pinned version IS the latest published.
	flagged, err := ps.IsTemplateUpdateAvailable(ctx, published.ID, tv.TemplateID)
	if err != nil {
		t.Fatalf("IsTemplateUpdateAvailable: %v", err)
	}
	if flagged {
		t.Fatal("expected not flagged when pinned = latest")
	}

	// Publish a new template version -> flag flips true.
	tpl2Sections := []domain.Section{{
		Key:    "purpose",
		Title:  "Purpose",
		Order:  1,
		Blocks: []domain.Block{{Type: domain.BlockTypeEditable}},
	}}
	tv2, _ := domain.NewTemplateVersion(tv.TemplateID.String(), tpl2Sections)
	tv2, err = ts.CreateTemplateVersion(ctx, tv2)
	if err != nil {
		t.Fatalf("CreateTemplateVersion v2: %v", err)
	}
	if _, err := ts.PublishTemplateVersion(ctx, tv2.ID); err != nil {
		t.Fatalf("PublishTemplateVersion v2: %v", err)
	}

	flagged, err = ps.IsTemplateUpdateAvailable(ctx, published.ID, tv.TemplateID)
	if err != nil {
		t.Fatalf("IsTemplateUpdateAvailable after new tv: %v", err)
	}
	if !flagged {
		t.Fatal("expected flagged after new template version published")
	}
}

// TestPolicyStoreUpdateDraftContent verifies that UpdateDraftContent overwrites
// the JSONB content of an existing draft policy version and surfaces an error
// when the draft does not exist or is not in 'draft' status.
func TestPolicyStoreUpdateDraftContent(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Update Draft Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	draft, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	saved, _, err := ps.UpsertDraft(ctx, draft)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}

	newContent := `{"root":{"type":"root","children":[]}}`
	if err := ps.UpdateDraftContent(ctx, saved.ID, newContent); err != nil {
		t.Fatalf("UpdateDraftContent: %v", err)
	}

	got, err := ps.GetPolicyVersion(ctx, saved.ID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	// JSONB normalises whitespace, so the round-tripped string is not
	// byte-equal to the input. Re-parse both sides and compare structurally.
	var wantParsed, gotParsed any
	if err := json.Unmarshal([]byte(newContent), &wantParsed); err != nil {
		t.Fatalf("parse want: %v", err)
	}
	if err := json.Unmarshal([]byte(got.ContentJSON), &gotParsed); err != nil {
		t.Fatalf("parse got: %v", err)
	}
	if !reflect.DeepEqual(wantParsed, gotParsed) {
		t.Fatalf("content not updated; got %q want %q", got.ContentJSON, newContent)
	}

	// A missing row should surface as an error rather than silently succeeding.
	if err := ps.UpdateDraftContent(ctx, uuid.New(), `{}`); err == nil {
		t.Fatal("expected error updating nonexistent draft, got nil")
	}

	// Publish the draft and confirm updates against the published row are rejected.
	if _, err := ps.PublishDraft(ctx, p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if err := ps.UpdateDraftContent(ctx, saved.ID, `{"after":"publish"}`); err == nil {
		t.Fatal("expected error updating non-draft row, got nil")
	}
}

// TestPolicyStoreDiscardDraft verifies that DiscardDraft removes ONLY the draft
// row: the policy survives, published versions survive, the draft slot frees up
// for a new draft, and discarding with no draft present is an error. This is the
// data-loss guard for the "discard draft deleted my policy" report.
func TestPolicyStoreDiscardDraft(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Discard Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// Publish v1 so we can prove DiscardDraft never touches a published version.
	d1, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, d1); err != nil {
		t.Fatalf("UpsertDraft 1: %v", err)
	}
	pub1, err := ps.PublishDraft(ctx, p.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}

	// Discarding when there is no draft is an error (and must not delete anything).
	if err := ps.DiscardDraft(ctx, p.ID); err == nil {
		t.Fatal("expected error discarding with no draft, got nil")
	}

	// Create a new draft, then discard it.
	d2, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"wip"}}`)
	if _, _, err := ps.UpsertDraft(ctx, d2); err != nil {
		t.Fatalf("UpsertDraft 2: %v", err)
	}
	if err := ps.DiscardDraft(ctx, p.ID); err != nil {
		t.Fatalf("DiscardDraft: %v", err)
	}

	// The policy itself MUST still exist.
	gotP, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy after discard (policy must survive): %v", err)
	}
	if gotP.ID != p.ID {
		t.Fatalf("policy id changed: got %s want %s", gotP.ID, p.ID)
	}
	if gotP.CurrentPublishedVersionID != pub1.ID {
		t.Fatalf("published pointer changed after discard: got %s want %s", gotP.CurrentPublishedVersionID, pub1.ID)
	}

	// The published version MUST survive untouched.
	stillPub, err := ps.GetPolicyVersion(ctx, pub1.ID)
	if err != nil {
		t.Fatalf("GetPolicyVersion published after discard: %v", err)
	}
	if stillPub.Status != domain.PolicyVersionStatusPublished {
		t.Fatalf("published version status changed: got %q", stillPub.Status)
	}

	// No draft row should remain.
	var draftCount int
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policy_versions WHERE policy_id=$1 AND status='draft'`,
		p.ID).Scan(&draftCount); err != nil {
		t.Fatalf("count drafts: %v", err)
	}
	if draftCount != 0 {
		t.Fatalf("expected 0 drafts after discard, got %d", draftCount)
	}

	// The freed slot allows a fresh draft.
	d3, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"again"}}`)
	if _, _, err := ps.UpsertDraft(ctx, d3); err != nil {
		t.Fatalf("UpsertDraft after discard: %v", err)
	}
}

// TestPolicyStoreDeletePolicyNeverPublishedCascades proves a never-published
// policy is hard-deleted along with its draft version(s) and appendices.
func TestPolicyStoreDeletePolicyNeverPublishedCascades(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	as := store.NewAppendixStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Delete Me", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// A draft version...
	draft, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"wip"}}`)
	savedDraft, _, err := ps.UpsertDraft(ctx, draft)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	// ...with an appendix hanging off it.
	ap, err := domain.NewAppendix(savedDraft.ID, "Intake Form", `{"a":0}`)
	if err != nil {
		t.Fatalf("NewAppendix: %v", err)
	}
	savedAp, err := as.Add(ctx, ap)
	if err != nil {
		t.Fatalf("Add appendix: %v", err)
	}

	if err := ps.DeletePolicy(ctx, p.ID); err != nil {
		t.Fatalf("DeletePolicy: %v", err)
	}

	// Policy gone.
	var policyCount int
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policies WHERE id=$1`, p.ID).Scan(&policyCount); err != nil {
		t.Fatalf("count policies: %v", err)
	}
	if policyCount != 0 {
		t.Fatalf("expected 0 policies after delete, got %d", policyCount)
	}

	// Versions gone (cascade).
	var versionCount int
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policy_versions WHERE policy_id=$1`, p.ID).Scan(&versionCount); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	if versionCount != 0 {
		t.Fatalf("expected 0 versions after delete, got %d", versionCount)
	}

	// Appendices gone (cascade).
	var appendixCount int
	if err := pool.Pool().QueryRow(ctx,
		`SELECT COUNT(*) FROM policy_appendices WHERE id=$1`, savedAp.ID).Scan(&appendixCount); err != nil {
		t.Fatalf("count appendices: %v", err)
	}
	if appendixCount != 0 {
		t.Fatalf("expected 0 appendices after delete, got %d", appendixCount)
	}
}

// TestPolicyStoreDeletePolicyWithPublishedRefuses proves a policy that has a
// published version cannot be hard-deleted.
func TestPolicyStoreDeletePolicyWithPublishedRefuses(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Published Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	d1, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	if _, _, err := ps.UpsertDraft(ctx, d1); err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	if _, err := ps.PublishDraft(ctx, p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}

	err = ps.DeletePolicy(ctx, p.ID)
	if !errors.Is(err, store.ErrHasPublishedVersion) {
		t.Fatalf("expected ErrHasPublishedVersion, got %v", err)
	}

	// Policy MUST still exist.
	if _, err := ps.GetPolicy(ctx, p.ID); err != nil {
		t.Fatalf("GetPolicy after refused delete: %v", err)
	}
}

// TestPolicyStoreDeletePolicyMissingReturnsNoRows proves deleting an unknown
// policy id returns pgx.ErrNoRows.
func TestPolicyStoreDeletePolicyMissingReturnsNoRows(t *testing.T) {
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	err := ps.DeletePolicy(ctx, uuid.New())
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows, got %v", err)
	}
}

// TestPolicyStoreRetirePolicyStampsHidesAndIsIdempotent proves RetirePolicy
// stamps retired_at, hides the policy from ListPolicies while keeping it
// retrievable via GetPolicy (with RetiredAt set), and is idempotent — a second
// retire does not error and preserves the original stamp.
func TestPolicyStoreRetirePolicyStampsHidesAndIsIdempotent(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Retire Me", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// Visible before retire.
	before, err := ps.ListPolicies(ctx, g.ID, false, domain.DocumentTypePolicy)
	if err != nil {
		t.Fatalf("ListPolicies before: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected 1 policy listed before retire, got %d", len(before))
	}

	retired, err := ps.RetirePolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("RetirePolicy: %v", err)
	}
	if retired.RetiredAt == nil {
		t.Fatal("expected RetiredAt set on returned policy")
	}
	if retired.Number == "" {
		t.Fatal("expected Number rendered on returned policy")
	}
	firstStamp := *retired.RetiredAt

	// Hidden from listings...
	after, err := ps.ListPolicies(ctx, g.ID, false, domain.DocumentTypePolicy)
	if err != nil {
		t.Fatalf("ListPolicies after: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("expected 0 policies listed after retire, got %d", len(after))
	}

	// ...but still retrievable via GetPolicy with RetiredAt populated.
	got, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy after retire: %v", err)
	}
	if got.RetiredAt == nil {
		t.Fatal("expected GetPolicy to return RetiredAt")
	}

	// Idempotent: retiring again does not error and keeps the original stamp.
	again, err := ps.RetirePolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("RetirePolicy (idempotent): %v", err)
	}
	if again.RetiredAt == nil || !again.RetiredAt.Equal(firstStamp) {
		t.Fatalf("expected stable retired_at on repeat retire: first=%v again=%v", firstStamp, again.RetiredAt)
	}
}

// TestPolicyStoreRetirePolicyMissingReturnsNoRows proves retiring an unknown
// policy id returns pgx.ErrNoRows (the store's NotFound convention).
func TestPolicyStoreRetirePolicyMissingReturnsNoRows(t *testing.T) {
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	_, err := ps.RetirePolicy(ctx, uuid.New())
	if !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("expected pgx.ErrNoRows, got %v", err)
	}
}

func TestPolicyStoreSetAckExplicitRoundTrips(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Ack Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// Set an explicit ack trigger + audience override.
	ack := domain.AckOnPublish
	audience := []uuid.UUID{uuid.New(), uuid.New()}
	updated, err := ps.SetAck(ctx, p.ID, &ack, audience)
	if err != nil {
		t.Fatalf("SetAck: %v", err)
	}
	if updated.AckTriggers == nil || *updated.AckTriggers != ack {
		t.Fatalf("expected ack_triggers=%q, got %v", ack, updated.AckTriggers)
	}
	if len(updated.AckAudienceOverride) != 2 {
		t.Fatalf("expected 2 audience members, got %d", len(updated.AckAudienceOverride))
	}
	if updated.AckAudienceOverride[0] != audience[0] || updated.AckAudienceOverride[1] != audience[1] {
		t.Fatalf("audience mismatch: got %v", updated.AckAudienceOverride)
	}

	// Round-trip via GetPolicy.
	fetched, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if fetched.AckTriggers == nil || *fetched.AckTriggers != ack {
		t.Fatalf("GetPolicy: expected ack_triggers=%q, got %v", ack, fetched.AckTriggers)
	}
	if len(fetched.AckAudienceOverride) != 2 {
		t.Fatalf("GetPolicy: expected 2 audience members, got %d", len(fetched.AckAudienceOverride))
	}
}

func TestPolicyStoreSetAckNilInheritRoundTrips(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, _ := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Inherit Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// Nil ack = inherit from category (stores NULL in DB).
	updated, err := ps.SetAck(ctx, p.ID, nil, nil)
	if err != nil {
		t.Fatalf("SetAck nil: %v", err)
	}
	if updated.AckTriggers != nil {
		t.Fatalf("expected nil AckTriggers (inherit), got %v", updated.AckTriggers)
	}
	if len(updated.AckAudienceOverride) != 0 {
		t.Fatalf("expected empty audience override, got %v", updated.AckAudienceOverride)
	}

	// Set a value then clear back to nil.
	ack := domain.AckOnChange
	if _, err := ps.SetAck(ctx, p.ID, &ack, []uuid.UUID{uuid.New()}); err != nil {
		t.Fatalf("SetAck set: %v", err)
	}
	cleared, err := ps.SetAck(ctx, p.ID, nil, nil)
	if err != nil {
		t.Fatalf("SetAck clear: %v", err)
	}
	if cleared.AckTriggers != nil {
		t.Fatalf("expected nil after clear, got %v", cleared.AckTriggers)
	}
	if len(cleared.AckAudienceOverride) != 0 {
		t.Fatalf("expected empty audience after clear, got %v", cleared.AckAudienceOverride)
	}
}

func TestPolicyStoreSetVersionStatusPublishAndWithdraw(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Lifecycle Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	saved, _, err := ps.UpsertDraft(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}

	// Drafts carry version_no=0; SetVersionStatus does not assign numbers, so
	// give the version a number first (publish path normally does this via
	// PublishDraft, but SetVersionStatus is workflow's status-only write).
	if _, err := pool.Pool().Exec(ctx, `UPDATE policy_versions SET version_no=1 WHERE id=$1`, saved.ID); err != nil {
		t.Fatalf("set version_no: %v", err)
	}

	// draft -> published stamps published_at + points the policy at it.
	pub, err := ps.SetVersionStatus(ctx, saved.ID, domain.PolicyVersionStatusPublished)
	if err != nil {
		t.Fatalf("SetVersionStatus(published): %v", err)
	}
	if pub.Status != domain.PolicyVersionStatusPublished {
		t.Fatalf("expected published, got %q", pub.Status)
	}
	if pub.PublishedAt.IsZero() {
		t.Fatal("expected published_at set")
	}
	pol, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol.CurrentPublishedVersionID != saved.ID {
		t.Fatalf("expected current_published_version_id=%s, got %s", saved.ID, pol.CurrentPublishedVersionID)
	}

	// published -> draft (withdraw) clears published_at + the policy pointer.
	wd, err := ps.SetVersionStatus(ctx, saved.ID, domain.PolicyVersionStatusDraft)
	if err != nil {
		t.Fatalf("SetVersionStatus(draft): %v", err)
	}
	if wd.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("expected draft, got %q", wd.Status)
	}
	if !wd.PublishedAt.IsZero() {
		t.Fatal("expected published_at cleared")
	}
	pol, err = ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy 2: %v", err)
	}
	if pol.CurrentPublishedVersionID != uuid.Nil {
		t.Fatalf("expected pointer cleared, got %s", pol.CurrentPublishedVersionID)
	}
}

// TestPolicyStoreSetVersionStatusPromotesDraftVersionNo checks that
// SetVersionStatus(draft → published) assigns a real version_no. Left in the
// version_no=0 slot, the next draft would collide on (policy_id, version_no).
func TestPolicyStoreSetVersionStatusPromotesDraftVersionNo(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Saga Publish Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	saved, _, err := ps.UpsertDraft(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	if saved.VersionNo != 0 {
		t.Fatalf("draft should carry version_no=0, got %d", saved.VersionNo)
	}

	// Saga-style publish via SetVersionStatus (NOT PublishDraft). The draft must
	// be promoted to a real version_no.
	pub, err := ps.SetVersionStatus(ctx, saved.ID, domain.PolicyVersionStatusPublished)
	if err != nil {
		t.Fatalf("SetVersionStatus(published): %v", err)
	}
	if pub.Status != domain.PolicyVersionStatusPublished {
		t.Fatalf("expected published, got %q", pub.Status)
	}
	if pub.VersionNo != 1 {
		t.Fatalf("expected version_no=1 after promotion, got %d", pub.VersionNo)
	}
	if pub.PublishedAt.IsZero() {
		t.Fatal("expected published_at set")
	}

	pol, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol.CurrentPublishedVersionID != saved.ID {
		t.Fatalf("expected current_published_version_id=%s, got %s", saved.ID, pol.CurrentPublishedVersionID)
	}

	// The published row must no longer occupy the version_no=0 draft slot, so a
	// fresh draft can be created without a 23505 on (policy_id, version_no).
	d2, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"v2 draft"}}`)
	newDraft, _, err := ps.UpsertDraft(ctx, d2)
	if err != nil {
		t.Fatalf("UpsertDraft after a workflow publish (duplicate-key regression): %v", err)
	}
	if newDraft.VersionNo != 0 {
		t.Fatalf("new draft should be version_no=0, got %d", newDraft.VersionNo)
	}
	if newDraft.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("expected draft status, got %q", newDraft.Status)
	}
}

// TestPolicyStoreSetVersionStatusMultiPublishSupersedes checks that repeated
// workflow publishes increment version_no and supersede the previous version,
// like PublishDraft.
func TestPolicyStoreSetVersionStatusMultiPublishSupersedes(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Multi Saga Publish", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}

	// First draft and publish -> version_no 1.
	d1, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	saved1, _, err := ps.UpsertDraft(ctx, d1)
	if err != nil {
		t.Fatalf("UpsertDraft 1: %v", err)
	}
	pub1, err := ps.SetVersionStatus(ctx, saved1.ID, domain.PolicyVersionStatusPublished)
	if err != nil {
		t.Fatalf("SetVersionStatus publish 1: %v", err)
	}
	if pub1.VersionNo != 1 {
		t.Fatalf("expected version_no=1, got %d", pub1.VersionNo)
	}

	// Second draft and publish -> version_no 2, prior superseded.
	d2, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{"purpose":"v2"}}`)
	saved2, _, err := ps.UpsertDraft(ctx, d2)
	if err != nil {
		t.Fatalf("UpsertDraft 2: %v", err)
	}
	pub2, err := ps.SetVersionStatus(ctx, saved2.ID, domain.PolicyVersionStatusPublished)
	if err != nil {
		t.Fatalf("SetVersionStatus publish 2: %v", err)
	}
	if pub2.VersionNo != 2 {
		t.Fatalf("expected version_no=2, got %d", pub2.VersionNo)
	}
	if pub2.SupersedesVersionID != pub1.ID {
		t.Fatalf("expected supersedes_version_id=%s, got %s", pub1.ID, pub2.SupersedesVersionID)
	}

	prior, err := ps.GetPolicyVersion(ctx, pub1.ID)
	if err != nil {
		t.Fatalf("GetPolicyVersion pub1: %v", err)
	}
	if prior.Status != domain.PolicyVersionStatusSuperseded {
		t.Fatalf("expected prior superseded, got %q", prior.Status)
	}

	pol, err := ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if pol.CurrentPublishedVersionID != pub2.ID {
		t.Fatalf("expected current_published_version_id=%s, got %s", pub2.ID, pol.CurrentPublishedVersionID)
	}
}

func TestPolicyStoreSetVersionStatusRejectsIllegalTransition(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)

	p, _ := domain.NewPolicy("Archived Policy", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, tv.ID, uuid.New(), `{"sections":{}}`)
	saved, _, err := ps.UpsertDraft(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	if _, err := pool.Pool().Exec(ctx, `UPDATE policy_versions SET version_no=1, status='archived' WHERE id=$1`, saved.ID); err != nil {
		t.Fatalf("force archived: %v", err)
	}
	// archived is terminal; archived -> published must error.
	_, err = ps.SetVersionStatus(ctx, saved.ID, domain.PolicyVersionStatusPublished)
	if !errors.Is(err, domain.ErrInvalidVersionStatusTransition) {
		t.Fatalf("expected ErrInvalidVersionStatusTransition, got %v", err)
	}
}

// SetProposedTitle only matches a draft: staging a title on a published
// version must return an error, never succeed silently.
func TestPolicyStoreSetProposedTitleNonDraftErrors(t *testing.T) {
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	_, pubID := makeVersion(t, ps, pool, "published")

	title := "Renamed"
	if err := ps.SetProposedTitle(ctx, pubID, &title); err == nil {
		t.Fatalf("expected an error staging proposed_title on a non-draft version, got nil")
	}
}

// SetProposedTitle on a draft stages the title and reads back.
func TestPolicyStoreSetProposedTitleDraftSucceeds(t *testing.T) {
	pool := newTestDB(t)
	ps := store.NewPolicyStore(pool)
	ctx := context.Background()

	_, draftID := makeVersion(t, ps, pool, "draft")

	title := "Renamed"
	if err := ps.SetProposedTitle(ctx, draftID, &title); err != nil {
		t.Fatalf("SetProposedTitle on draft: %v", err)
	}
	pv, err := ps.GetPolicyVersion(ctx, draftID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	if pv.ProposedTitle == nil || *pv.ProposedTitle != "Renamed" {
		t.Fatalf("proposed_title: got %v want %q", pv.ProposedTitle, "Renamed")
	}
}
