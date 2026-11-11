// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// docTypesFixture builds the stores and one category of each kind, the way the mock seeds them:
// expenses holds policies, conduct holds procedures, workplace is a container.
type docTypesFixture struct {
	gs *store.CategoryStore
	ps *store.PolicyStore
}

func newDocTypesFixture(t *testing.T) docTypesFixture {
	t.Helper()
	pool := newTestDB(t)
	return docTypesFixture{gs: store.NewCategoryStore(pool), ps: store.NewPolicyStore(pool)}
}

func (f docTypesFixture) category(t *testing.T, name string, types domain.CategoryDocumentTypes) domain.Category {
	t.Helper()
	ctx := context.Background()
	c, err := domain.NewCategory(name, name+"-"+uuid.NewString()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	c, err = f.gs.Create(ctx, c)
	if err != nil {
		t.Fatalf("Create %s: %v", name, err)
	}
	if types != domain.CategoryDocumentTypesBoth {
		if c, err = f.gs.SetDocumentTypes(ctx, c.ID, types); err != nil {
			t.Fatalf("SetDocumentTypes %s: %v", name, err)
		}
	}
	return c
}

func (f docTypesFixture) policy(t *testing.T, cat uuid.UUID, dt domain.DocumentType) domain.Policy {
	t.Helper()
	p, err := domain.NewPolicy("A document", cat, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.DocumentType = dt
	p, err = f.ps.CreatePolicy(context.Background(), p)
	if err != nil {
		t.Fatalf("CreatePolicy(%s): %v", dt, err)
	}
	return p
}

// Mock: "has one category of each kind and the rest holding both" / existing categories read as both.
func TestCategoryDocumentTypesDefaultAndRoundTrip(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()

	c := f.category(t, "Finance", domain.CategoryDocumentTypesBoth)
	got, err := f.gs.Get(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocumentTypes != domain.CategoryDocumentTypesBoth {
		t.Fatalf("a new category reads %q, want both", got.DocumentTypes)
	}

	for _, want := range []domain.CategoryDocumentTypes{
		domain.CategoryDocumentTypesPolicies, domain.CategoryDocumentTypesProcedures,
		domain.CategoryDocumentTypesNone, domain.CategoryDocumentTypesBoth,
	} {
		if _, err := f.gs.SetDocumentTypes(ctx, c.ID, want); err != nil {
			t.Fatalf("SetDocumentTypes(%s): %v", want, err)
		}
		got, err := f.gs.Get(ctx, c.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.DocumentTypes != want {
			t.Fatalf("Get after set: %q, want %q", got.DocumentTypes, want)
		}
		kids, err := f.gs.ListChildren(ctx, uuid.Nil)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, k := range kids {
			if k.ID == c.ID {
				found = true
				if k.DocumentTypes != want {
					t.Fatalf("ListChildren: %q, want %q", k.DocumentTypes, want)
				}
			}
		}
		if !found {
			t.Fatal("category missing from ListChildren")
		}
	}
}

// A child does not inherit its parent's setting: the mock stores the field per category and creates
// every category as BOTH.
func TestCategoryDocumentTypesNotInherited(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()
	parent := f.category(t, "Workplace", domain.CategoryDocumentTypesNone)
	child, err := domain.NewCategory("Conduct", "conduct-"+uuid.NewString()[:8], parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	child, err = f.gs.Create(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.gs.Get(ctx, child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DocumentTypes != domain.CategoryDocumentTypesBoth {
		t.Fatalf("child of a container reads %q, want both", got.DocumentTypes)
	}
	// And the child takes a policy although the parent is a container.
	f.policy(t, child.ID, domain.DocumentTypePolicy)
}

// Mock: "refuses a procedure in a policies-only category and a policy in a procedures-only one",
// "refuses any document in a container", "accepts a document of the kind the category holds".
func TestCreatePolicyRefusedByCategoryKind(t *testing.T) {
	f := newDocTypesFixture(t)
	expenses := f.category(t, "Expenses", domain.CategoryDocumentTypesPolicies)
	conduct := f.category(t, "Conduct", domain.CategoryDocumentTypesProcedures)
	workplace := f.category(t, "Workplace", domain.CategoryDocumentTypesNone)

	cases := []struct {
		name string
		cat  uuid.UUID
		dt   domain.DocumentType
		ok   bool
	}{
		{"procedure in policies-only", expenses.ID, domain.DocumentTypeProcedure, false},
		{"policy in procedures-only", conduct.ID, domain.DocumentTypePolicy, false},
		{"policy in container", workplace.ID, domain.DocumentTypePolicy, false},
		{"procedure in container", workplace.ID, domain.DocumentTypeProcedure, false},
		{"policy in policies-only", expenses.ID, domain.DocumentTypePolicy, true},
		{"procedure in procedures-only", conduct.ID, domain.DocumentTypeProcedure, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := domain.NewPolicy("A document", c.cat, domain.SensitivityStandard, uuid.New())
			if err != nil {
				t.Fatal(err)
			}
			p.DocumentType = c.dt
			_, err = f.ps.CreatePolicy(context.Background(), p)
			if c.ok && err != nil {
				t.Fatalf("CreatePolicy: %v", err)
			}
			if !c.ok && !errors.Is(err, store.ErrCategoryRefusesType) {
				t.Fatalf("CreatePolicy err = %v, want ErrCategoryRefusesType", err)
			}
		})
	}
}

// A refused create leaves nothing behind: no policy row and no burnt sequence number.
func TestRefusedCreateLeavesNoPolicy(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()
	expenses := f.category(t, "Expenses", domain.CategoryDocumentTypesPolicies)
	p, _ := domain.NewPolicy("A procedure", expenses.ID, domain.SensitivityStandard, uuid.New())
	p.DocumentType = domain.DocumentTypeProcedure
	if _, err := f.ps.CreatePolicy(ctx, p); !errors.Is(err, store.ErrCategoryRefusesType) {
		t.Fatalf("want ErrCategoryRefusesType, got %v", err)
	}
	counts, err := f.gs.CountDocuments(ctx, expenses.ID)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Policies != 0 || counts.Procedures != 0 {
		t.Fatalf("counts after a refused create: %+v", counts)
	}
	first := f.policy(t, expenses.ID, domain.DocumentTypePolicy)
	if first.Sequence != 1 {
		t.Fatalf("first accepted policy has sequence %d, want 1", first.Sequence)
	}
}

// Mock: "refuses to move a document into a category that does not hold its type".
func TestSetHomeCategoryRefusedByCategoryKind(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()
	both := f.category(t, "Finance", domain.CategoryDocumentTypesBoth)
	conduct := f.category(t, "Conduct", domain.CategoryDocumentTypesProcedures)
	workplace := f.category(t, "Workplace", domain.CategoryDocumentTypesNone)
	policy := f.policy(t, both.ID, domain.DocumentTypePolicy)
	procedure := f.policy(t, both.ID, domain.DocumentTypeProcedure)

	if _, err := f.ps.SetHomeCategory(ctx, policy.ID, conduct.ID); !errors.Is(err, store.ErrCategoryRefusesType) {
		t.Fatalf("policy into procedures-only: %v", err)
	}
	if _, err := f.ps.SetHomeCategory(ctx, procedure.ID, workplace.ID); !errors.Is(err, store.ErrCategoryRefusesType) {
		t.Fatalf("procedure into container: %v", err)
	}
	got, err := f.ps.GetPolicy(ctx, policy.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.HomeCategoryID != both.ID {
		t.Fatal("a refused move must leave the policy where it was")
	}
	// The accepted kind still moves.
	moved, err := f.ps.SetHomeCategory(ctx, procedure.ID, conduct.ID)
	if err != nil {
		t.Fatalf("procedure into procedures-only: %v", err)
	}
	if moved.HomeCategoryID != conduct.ID {
		t.Fatal("procedure did not move")
	}
}

// Mock: "counts the policies and procedures a category holds". Retired documents are not counted.
func TestCountDocuments(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()
	c := f.category(t, "Finance", domain.CategoryDocumentTypesBoth)
	f.policy(t, c.ID, domain.DocumentTypePolicy)
	f.policy(t, c.ID, domain.DocumentTypePolicy)
	f.policy(t, c.ID, domain.DocumentTypeProcedure)
	retired := f.policy(t, c.ID, domain.DocumentTypePolicy)
	if _, err := f.ps.RetirePolicy(ctx, retired.ID); err != nil {
		t.Fatalf("RetirePolicy: %v", err)
	}

	got, err := f.gs.CountDocuments(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Policies != 2 || got.Procedures != 1 {
		t.Fatalf("counts = %+v, want 2 policies and 1 procedure", got)
	}
	if _, err := f.gs.CountDocuments(ctx, uuid.New()); !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("unknown category: %v", err)
	}
}

// Mock: "refuses to narrow while documents would be stranded, with the count" and leaves the
// setting as it was.
func TestSetDocumentTypesRefusesStrandingNarrowing(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()
	c := f.category(t, "Finance", domain.CategoryDocumentTypesBoth)
	f.policy(t, c.ID, domain.DocumentTypePolicy)
	f.policy(t, c.ID, domain.DocumentTypePolicy)
	f.policy(t, c.ID, domain.DocumentTypeProcedure)

	cases := []struct {
		to         domain.CategoryDocumentTypes
		policies   int
		procedures int
	}{
		{domain.CategoryDocumentTypesProcedures, 2, 0},
		{domain.CategoryDocumentTypesPolicies, 0, 1},
		{domain.CategoryDocumentTypesNone, 2, 1},
	}
	for _, tc := range cases {
		t.Run(string(tc.to), func(t *testing.T) {
			_, err := f.gs.SetDocumentTypes(ctx, c.ID, tc.to)
			if !errors.Is(err, store.ErrCategoryHoldsDocuments) {
				t.Fatalf("err = %v, want ErrCategoryHoldsDocuments", err)
			}
			var stranded *store.StrandedDocumentsError
			if !errors.As(err, &stranded) {
				t.Fatalf("err %v carries no StrandedDocumentsError", err)
			}
			if stranded.Policies != tc.policies || stranded.Procedures != tc.procedures {
				t.Fatalf("stranded = %d policies, %d procedures; want %d, %d",
					stranded.Policies, stranded.Procedures, tc.policies, tc.procedures)
			}
			got, err := f.gs.Get(ctx, c.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.DocumentTypes != domain.CategoryDocumentTypesBoth {
				t.Fatalf("a refused narrowing changed the setting to %q", got.DocumentTypes)
			}
		})
	}
}

// Mock: "narrows an empty category and widens any category". Draft documents count as held.
func TestSetDocumentTypesNarrowEmptyAndWidenAlways(t *testing.T) {
	f := newDocTypesFixture(t)
	ctx := context.Background()

	empty := f.category(t, "Purchasing", domain.CategoryDocumentTypesBoth)
	got, err := f.gs.SetDocumentTypes(ctx, empty.ID, domain.CategoryDocumentTypesNone)
	if err != nil {
		t.Fatalf("narrow an empty category: %v", err)
	}
	if got.DocumentTypes != domain.CategoryDocumentTypesNone {
		t.Fatalf("returned %q, want none", got.DocumentTypes)
	}

	held := f.category(t, "Expenses", domain.CategoryDocumentTypesPolicies)
	f.policy(t, held.ID, domain.DocumentTypePolicy) // a fresh policy holds only a draft
	if _, err := f.gs.SetDocumentTypes(ctx, held.ID, domain.CategoryDocumentTypesPolicies); err != nil {
		t.Fatalf("setting the same value must pass: %v", err)
	}
	if _, err := f.gs.SetDocumentTypes(ctx, held.ID, domain.CategoryDocumentTypesBoth); err != nil {
		t.Fatalf("widen: %v", err)
	}

	// A retired document no longer blocks a narrowing.
	archive := f.category(t, "Archive", domain.CategoryDocumentTypesBoth)
	p := f.policy(t, archive.ID, domain.DocumentTypePolicy)
	if _, err := f.ps.RetirePolicy(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.gs.SetDocumentTypes(ctx, archive.ID, domain.CategoryDocumentTypesProcedures); err != nil {
		t.Fatalf("narrow past a retired document: %v", err)
	}
	if _, err := f.gs.SetDocumentTypes(ctx, uuid.New(), domain.CategoryDocumentTypesBoth); !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("unknown category: %v", err)
	}
	if _, err := f.gs.SetDocumentTypes(ctx, held.ID, domain.CategoryDocumentTypes("everything")); err == nil {
		t.Fatal("an unknown value must be refused")
	}
}
