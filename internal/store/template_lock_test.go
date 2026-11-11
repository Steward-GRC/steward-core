// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func newLockCategory(t *testing.T, gs *store.CategoryStore, parent uuid.UUID) domain.Category {
	t.Helper()
	g, err := domain.NewCategory("Finance", "fin-"+uuid.New().String()[:8], parent)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	g, err = gs.Create(context.Background(), g)
	if err != nil {
		t.Fatalf("Create category: %v", err)
	}
	return g
}

// Mock tests: "locks a category that names a default template", "can be lifted again".
func TestCategoryStoreSetTemplateLock(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	if _, err := gs.SetDefaults(ctx, g.ID, tv.TemplateID, uuid.Nil, false); err != nil {
		t.Fatalf("SetDefaults: %v", err)
	}

	locked, err := gs.SetTemplateLock(ctx, g.ID, true)
	if err != nil {
		t.Fatalf("SetTemplateLock(true): %v", err)
	}
	if !locked.DefaultTemplateLocked || locked.DefaultTemplateID != tv.TemplateID {
		t.Fatalf("lock not stored: %+v", locked)
	}
	got, err := gs.Get(ctx, g.ID)
	if err != nil || !got.DefaultTemplateLocked {
		t.Fatalf("Get after lock: locked=%v err=%v", got.DefaultTemplateLocked, err)
	}
	chain, err := gs.AncestorChain(ctx, g.ID)
	if err != nil || len(chain) != 1 || !chain[0].DefaultTemplateLocked {
		t.Fatalf("AncestorChain must carry the lock: %+v err=%v", chain, err)
	}
	kids, err := gs.ListChildren(ctx, uuid.Nil)
	if err != nil {
		t.Fatalf("ListChildren: %v", err)
	}
	found := false
	for _, k := range kids {
		if k.ID == g.ID {
			found = k.DefaultTemplateLocked
		}
	}
	if !found {
		t.Fatal("ListChildren must carry the lock")
	}

	lifted, err := gs.SetTemplateLock(ctx, g.ID, false)
	if err != nil {
		t.Fatalf("SetTemplateLock(false): %v", err)
	}
	if lifted.DefaultTemplateLocked {
		t.Fatal("lock should be lifted")
	}
}

// Mock test: "needs a default template to lock".
func TestCategoryStoreSetTemplateLockNeedsDefault(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	g := newLockCategory(t, gs, uuid.Nil)

	_, err := gs.SetTemplateLock(context.Background(), g.ID, true)
	if !errors.Is(err, store.ErrTemplateLockNeedsDefault) {
		t.Fatalf("want ErrTemplateLockNeedsDefault, got %v", err)
	}
	_, err = gs.SetTemplateLock(context.Background(), uuid.New(), true)
	if !errors.Is(err, store.ErrCategoryNotFound) {
		t.Fatalf("unknown category: want ErrCategoryNotFound, got %v", err)
	}
}

// The mock's setCategoryDefaults drops the lock when no template is left to hold to.
func TestCategoryStoreSetDefaultsDropsLockWithoutTemplate(t *testing.T) {
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ctx := context.Background()
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	if _, err := gs.SetDefaults(ctx, g.ID, tv.TemplateID, uuid.Nil, false); err != nil {
		t.Fatal(err)
	}
	if _, err := gs.SetTemplateLock(ctx, g.ID, true); err != nil {
		t.Fatal(err)
	}

	same, err := gs.SetDefaults(ctx, g.ID, tv.TemplateID, uuid.Nil, false)
	if err != nil || !same.DefaultTemplateLocked {
		t.Fatalf("re-saving the same template keeps the lock: locked=%v err=%v", same.DefaultTemplateLocked, err)
	}
	none, err := gs.SetDefaults(ctx, g.ID, uuid.Nil, uuid.Nil, true)
	if err != nil {
		t.Fatal(err)
	}
	if none.DefaultTemplateLocked {
		t.Fatal("freeform default must drop the lock")
	}
}

func textDoc(texts ...string) string {
	children := ""
	for i, tx := range texts {
		if i > 0 {
			children += ","
		}
		children += `{"type":"paragraph","children":[{"type":"text","text":"` + tx + `"}]}`
	}
	return `{"root":{"type":"root","children":[` + children + `]}}`
}

func newPolicyForTemplateTests(t *testing.T) (*store.PolicyStore, domain.Policy, domain.TemplateVersion) {
	t.Helper()
	pool := newTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	g, tv := setupCategoryAndTemplate(t, gs, ts)
	p, _ := domain.NewPolicy("Starter", g.ID, domain.SensitivityStandard, uuid.New())
	p, err := ps.CreatePolicy(context.Background(), p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	return ps, p, tv
}

func saveDraft(t *testing.T, ps *store.PolicyStore, p domain.Policy, tv uuid.UUID, content string) {
	t.Helper()
	_, _, err := ps.UpsertDraft(context.Background(), domain.PolicyVersion{
		ID: uuid.New(), PolicyID: p.ID, TemplateVersionID: tv, ContentJSON: content, CreatedBy: p.OwnerUserID,
	})
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
}

func currentDraft(t *testing.T, ps *store.PolicyStore, p domain.Policy) domain.PolicyVersion {
	t.Helper()
	cur, err := ps.GetPolicy(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	v, err := ps.GetPolicyVersion(context.Background(), cur.CurrentDraftVersionID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	return v
}

// Mock test (freeform-and-empty.test.ts): "blanks the untouched starting draft, drops its
// template and marks the policy freeform".
func TestPolicyStoreSetTemplateNoneBlanksUntouchedDraft(t *testing.T) {
	ps, p, tv := newPolicyForTemplateTests(t)
	// The starting draft is the template's section titles and nothing the author wrote.
	saveDraft(t, ps, p, tv.ID, textDoc("Purpose"))

	got, err := ps.SetTemplate(context.Background(), p.ID, uuid.Nil, true)
	if err != nil {
		t.Fatalf("SetTemplate: %v", err)
	}
	if !got.TemplateNone || got.TemplateID != uuid.Nil {
		t.Fatalf("policy should be freeform: %+v", got)
	}
	v := currentDraft(t, ps, p)
	if v.TemplateVersionID != uuid.Nil {
		t.Fatalf("draft must drop its template pin, got %v", v.TemplateVersionID)
	}
	if v.ContentJSON != "{}" {
		t.Fatalf("draft must be blank, got %s", v.ContentJSON)
	}
}

func TestPolicyStoreSetTemplateNoneBlanksSeededDraft(t *testing.T) {
	ps, p, _ := newPolicyForTemplateTests(t)
	if _, err := ps.SetTemplate(context.Background(), p.ID, uuid.Nil, true); err != nil {
		t.Fatalf("SetTemplate: %v", err)
	}
	if v := currentDraft(t, ps, p); v.ContentJSON != "{}" || v.TemplateVersionID != uuid.Nil {
		t.Fatalf("seeded draft should stay blank: %+v", v)
	}
}

// Mock test: "leaves a draft the author has already written alone".
func TestPolicyStoreSetTemplateNoneLeavesWrittenDraft(t *testing.T) {
	ps, p, tv := newPolicyForTemplateTests(t)
	saveDraft(t, ps, p, tv.ID, textDoc("Purpose", "My own words."))

	if _, err := ps.SetTemplate(context.Background(), p.ID, uuid.Nil, true); err != nil {
		t.Fatalf("SetTemplate: %v", err)
	}
	v := currentDraft(t, ps, p)
	if v.TemplateVersionID != tv.ID {
		t.Fatalf("written draft keeps its pin, got %v", v.TemplateVersionID)
	}
	if !strings.Contains(v.ContentJSON, "My own words.") {
		t.Fatalf("written draft must be untouched, got %s", v.ContentJSON)
	}
}

func TestPolicyStoreSetTemplateNamedLeavesDraftAlone(t *testing.T) {
	ps, p, tv := newPolicyForTemplateTests(t)
	saveDraft(t, ps, p, tv.ID, textDoc("Purpose"))

	if _, err := ps.SetTemplate(context.Background(), p.ID, tv.TemplateID, false); err != nil {
		t.Fatalf("SetTemplate: %v", err)
	}
	if v := currentDraft(t, ps, p); v.TemplateVersionID != tv.ID {
		t.Fatalf("only freeform blanks the draft, pin is now %v", v.TemplateVersionID)
	}
}
