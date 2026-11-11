// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// Tests translated from the mock's version-links.test.ts: the library links
// belong to the policy version (issue #70).

type linkFixture struct {
	*statusFixture
	vl   *store.VersionLinkStore
	refs *store.ReferenceStore
	cons *store.ContactStore
	defs *store.DefinitionLibraryStore
}

func newLinkFixture(t *testing.T) *linkFixture {
	t.Helper()
	f := &linkFixture{statusFixture: newStatusFixture(t)}
	f.vl = store.NewVersionLinkStore(f.db)
	f.refs = store.NewReferenceStore(f.db)
	f.cons = store.NewContactStore(f.db)
	f.defs = store.NewDefinitionLibraryStore(f.db)
	return f
}

func (f *linkFixture) ref(label string) uuid.UUID {
	f.t.Helper()
	r, err := f.refs.CreateReference(f.ctx, domain.Reference{Label: label, Kind: domain.ReferenceKindStandard})
	if err != nil {
		f.t.Fatalf("CreateReference %s: %v", label, err)
	}
	return r.ID
}

func (f *linkFixture) contact(label string) uuid.UUID {
	f.t.Helper()
	b, err := f.cons.CreateBlock(f.ctx, domain.ContactBlock{Label: label, Name: label})
	if err != nil {
		f.t.Fatalf("CreateBlock %s: %v", label, err)
	}
	return b.ID
}

func (f *linkFixture) def(term string) uuid.UUID {
	f.t.Helper()
	d, err := f.defs.CreateDefinitionEntry(f.ctx, domain.DefinitionEntry{CategoryID: f.p.HomeCategoryID, Term: term, Definition: "A meaning."})
	if err != nil {
		f.t.Fatalf("CreateDefinitionEntry %s: %v", term, err)
	}
	return d.ID
}

// otherPolicy creates a second policy in the same category.
func (f *linkFixture) otherPolicy(title string) domain.Policy {
	f.t.Helper()
	p, _ := domain.NewPolicy(title, f.p.HomeCategoryID, domain.SensitivityStandard, uuid.New())
	p, err := f.ps.CreatePolicy(f.ctx, p)
	if err != nil {
		f.t.Fatalf("CreatePolicy %s: %v", title, err)
	}
	return p
}

func (f *linkFixture) links(versionID uuid.UUID) domain.VersionLinks {
	f.t.Helper()
	l, err := f.vl.GetVersionLinks(f.ctx, versionID)
	if err != nil {
		f.t.Fatalf("GetVersionLinks %s: %v", versionID, err)
	}
	return l
}

func (f *linkFixture) setRefs(versionID uuid.UUID, ids ...uuid.UUID) domain.VersionLinks {
	f.t.Helper()
	l, err := f.vl.SetReferences(f.ctx, versionID, ids)
	if err != nil {
		f.t.Fatalf("SetReferences: %v", err)
	}
	return l
}

func refIDs(l domain.VersionLinks) []uuid.UUID {
	out := []uuid.UUID{}
	for _, r := range l.References {
		out = append(out, r.ID)
	}
	return out
}

func contactIDs(l domain.VersionLinks) []uuid.UUID {
	out := []uuid.UUID{}
	for _, c := range l.Contacts {
		out = append(out, c.ID)
	}
	return out
}

func sameIDs(t *testing.T, label string, got []uuid.UUID, want ...uuid.UUID) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(append([]uuid.UUID{}, want...)) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

// seeded builds a published version that links two references, two contact
// blocks, one definition and one related policy, as the mock's seed does.
type seeded struct {
	pub        domain.PolicyVersion
	r1, r2, r3 uuid.UUID
	c1, c2     uuid.UUID
	d1         uuid.UUID
	target     domain.Policy
}

func (f *linkFixture) seed() seeded {
	f.t.Helper()
	s := seeded{r1: f.ref("ISO 27001"), r2: f.ref("NIST"), r3: f.ref("SOC 2"), c1: f.contact("Help desk"), c2: f.contact("Security"), d1: f.def("Business expense")}
	s.target = f.otherPolicy("Gifts and Hospitality")
	d, _ := f.save(reviewedText)
	f.setRefs(d.ID, s.r1, s.r3)
	if _, err := f.vl.SetContactBlocks(f.ctx, d.ID, []uuid.UUID{s.c2, s.c1}); err != nil {
		f.t.Fatalf("SetContactBlocks: %v", err)
	}
	if _, err := f.vl.SetDefinitions(f.ctx, d.ID, []uuid.UUID{s.d1}); err != nil {
		f.t.Fatalf("SetDefinitions: %v", err)
	}
	if _, err := f.vl.SetRelated(f.ctx, d.ID, []domain.RelatedLinkInput{{PolicyID: s.target.ID, Type: "RELATED_TO"}}); err != nil {
		f.t.Fatalf("SetRelated: %v", err)
	}
	s.pub = f.setStatus(d.ID, domain.PolicyVersionStatusPublished)
	return s
}

// newDraftFromPublished saves a new draft and copies the links forward the way
// the SaveDraft handler does.
func (f *linkFixture) newDraft() domain.PolicyVersion {
	f.t.Helper()
	d, res := f.save(reviewedText)
	if !res.Inserted {
		f.t.Fatal("expected a new draft row")
	}
	src, err := f.ps.AppendixSource(f.ctx, f.p.ID)
	if err != nil {
		f.t.Fatalf("AppendixSource: %v", err)
	}
	if src != uuid.Nil {
		if err := f.vl.CopyForward(f.ctx, src, d.ID); err != nil {
			f.t.Fatalf("CopyForward: %v", err)
		}
	}
	return d
}

// --- the links belong to the policy version ---

func TestVersionLinks_ReadsPublishedLinks(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	l := f.links(s.pub.ID)
	sameIDs(t, "references", refIDs(l), s.r1, s.r3)
	sameIDs(t, "contacts", contactIDs(l), s.c2, s.c1)
	if len(l.Definitions) != 1 || l.Definitions[0].ID != s.d1 {
		t.Fatalf("definitions = %+v", l.Definitions)
	}
	if len(l.Related) != 1 || l.Related[0].PolicyID != s.target.ID || l.Related[0].Type != "RELATED_TO" || l.Related[0].Direction != domain.RelatedOutgoing {
		t.Fatalf("related = %+v", l.Related)
	}
	if l.Editable {
		t.Fatal("a published version must not be editable")
	}
	if l.PolicyVersionID != s.pub.ID {
		t.Fatalf("policy version id = %s", l.PolicyVersionID)
	}
}

func TestVersionLinks_NotFound(t *testing.T) {
	f := newLinkFixture(t)
	if _, err := f.vl.GetVersionLinks(f.ctx, uuid.New()); !errors.Is(err, store.ErrVersionNotFound) {
		t.Fatalf("get: err = %v, want ErrVersionNotFound", err)
	}
	if _, err := f.vl.SetReferences(f.ctx, uuid.New(), nil); !errors.Is(err, store.ErrVersionNotFound) {
		t.Fatalf("set: err = %v, want ErrVersionNotFound", err)
	}
}

func TestVersionLinks_DraftChangeStaysOutOfPublishedUntilPublished(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	draft := f.newDraft()
	f.setRefs(draft.ID, s.r2)
	sameIDs(t, "draft references", refIDs(f.links(draft.ID)), s.r2)
	sameIDs(t, "published references", refIDs(f.links(s.pub.ID)), s.r1, s.r3)

	if _, err := f.ps.PublishDraft(f.ctx, f.p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	sameIDs(t, "published draft references", refIDs(f.links(draft.ID)), s.r2)
	// the version it replaced is left as it was approved
	sameIDs(t, "superseded references", refIDs(f.links(s.pub.ID)), s.r1, s.r3)
}

// --- only while a version is editable ---

func TestSetVersionLinks_RefusesPublishedAndSuperseded(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	if _, err := f.vl.SetReferences(f.ctx, s.pub.ID, nil); !errors.Is(err, store.ErrVersionNotEditable) {
		t.Fatalf("published: err = %v, want ErrVersionNotEditable", err)
	}
	f.newDraft()
	if _, err := f.ps.PublishDraft(f.ctx, f.p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	// s.pub is now superseded
	for name, set := range map[string]func() error{
		"references": func() error { _, err := f.vl.SetReferences(f.ctx, s.pub.ID, nil); return err },
		"contacts":   func() error { _, err := f.vl.SetContactBlocks(f.ctx, s.pub.ID, nil); return err },
		"defs":       func() error { _, err := f.vl.SetDefinitions(f.ctx, s.pub.ID, nil); return err },
		"related":    func() error { _, err := f.vl.SetRelated(f.ctx, s.pub.ID, nil); return err },
	} {
		if err := set(); !errors.Is(err, store.ErrVersionNotEditable) {
			t.Errorf("superseded %s: err = %v, want ErrVersionNotEditable", name, err)
		}
	}
	sameIDs(t, "superseded references", refIDs(f.links(s.pub.ID)), s.r1, s.r3)
}

// Core has no withdrawn status: withdrawing a submission puts the version back
// to draft, so draft and in review are the editable statuses.
func TestSetVersionLinks_AcceptsDraftAndInReview(t *testing.T) {
	f := newLinkFixture(t)
	r := f.ref("Policy of record")
	d, _ := f.save(reviewedText)
	steps := []struct {
		name string
		to   domain.PolicyVersionStatus
	}{
		{"draft", ""},
		{"in review", domain.PolicyVersionStatusInReview},
		{"withdrawn back to draft", domain.PolicyVersionStatusDraft},
	}
	for _, step := range steps {
		if step.to != "" {
			f.setStatus(d.ID, step.to)
		}
		l, err := f.vl.SetReferences(f.ctx, d.ID, []uuid.UUID{r})
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		if !l.Editable {
			t.Fatalf("%s: editable = false", step.name)
		}
	}
}

func TestSetVersionLinks_RefusesRejected(t *testing.T) {
	f := newLinkFixture(t)
	rej := f.reject(reviewedText)
	if _, err := f.vl.SetReferences(f.ctx, rej.ID, nil); !errors.Is(err, store.ErrVersionNotEditable) {
		t.Fatalf("err = %v, want ErrVersionNotEditable", err)
	}
	if l := f.links(rej.ID); l.Editable {
		t.Fatal("a rejected version is final")
	}
}

func TestSetVersionLinks_DoesNotCreateDraft(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	before, err := f.ps.ListPublishedVersions(f.ctx, f.p.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.vl.SetReferences(f.ctx, s.pub.ID, nil)
	if !errors.Is(err, store.ErrVersionNotEditable) {
		t.Fatalf("err = %v", err)
	}
	var count int
	if err := f.db.Querier().QueryRow(f.ctx, `SELECT count(*) FROM policy_versions WHERE policy_id=$1`, f.p.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != len(before) {
		t.Fatalf("versions = %d, want %d: linking must not make a draft", count, len(before))
	}
}

// --- carrying the links over to a new draft ---

func TestCopyLinksForward_CopiesPublished(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	draft := f.newDraft()
	l := f.links(draft.ID)
	sameIDs(t, "references", refIDs(l), s.r1, s.r3)
	sameIDs(t, "contacts", contactIDs(l), s.c2, s.c1)
	if len(l.Definitions) != 1 || len(l.Related) != 1 || l.Related[0].Type != "RELATED_TO" {
		t.Fatalf("links = %+v", l)
	}
	if !l.Editable {
		t.Fatal("the new draft must be editable")
	}
}

func TestCopyLinksForward_FromRejectedSource(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	draft := f.newDraft()
	f.setRefs(draft.ID, s.r2)
	f.setStatus(draft.ID, domain.PolicyVersionStatusInReview)
	f.setStatus(draft.ID, domain.PolicyVersionStatusRejected)
	next := f.newDraft()
	if next.ID == draft.ID {
		t.Fatal("next save must be a new draft")
	}
	sameIDs(t, "references", refIDs(f.links(next.ID)), s.r2)
}

func TestCopyLinksForward_CopiesNotShares(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	draft := f.newDraft()
	if _, err := f.vl.SetContactBlocks(f.ctx, draft.ID, nil); err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "published contacts", contactIDs(f.links(s.pub.ID)), s.c2, s.c1)
	sameIDs(t, "draft contacts", contactIDs(f.links(draft.ID)))
}

func TestCopyLinksForward_RefusesANonEditableTarget(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	if err := f.vl.CopyForward(f.ctx, s.pub.ID, s.pub.ID); !errors.Is(err, store.ErrVersionNotEditable) {
		t.Fatalf("err = %v, want ErrVersionNotEditable", err)
	}
}

// --- order, duplicates and unknown items ---

func TestSetVersionLinks_KeepsAuthorOrder(t *testing.T) {
	f := newLinkFixture(t)
	a, b, c := f.ref("a"), f.ref("b"), f.ref("c")
	d, _ := f.save(reviewedText)
	sameIDs(t, "references", refIDs(f.setRefs(d.ID, c, a, b)), c, a, b)
	sameIDs(t, "stored references", refIDs(f.links(d.ID)), c, a, b)
	x, y, z := f.contact("x"), f.contact("y"), f.contact("z")
	l, err := f.vl.SetContactBlocks(f.ctx, d.ID, []uuid.UUID{z, x, y})
	if err != nil {
		t.Fatal(err)
	}
	sameIDs(t, "contacts", contactIDs(l), z, x, y)
}

func TestSetVersionLinks_RefusesDuplicateIDs(t *testing.T) {
	f := newLinkFixture(t)
	r := f.ref("a")
	d, _ := f.save(reviewedText)
	if _, err := f.vl.SetReferences(f.ctx, d.ID, []uuid.UUID{r, r}); !errors.Is(err, domain.ErrRepeatedLink) {
		t.Fatalf("err = %v, want ErrRepeatedLink", err)
	}
}

func TestSetVersionLinks_RefusesUnknownItem(t *testing.T) {
	f := newLinkFixture(t)
	r := f.ref("a")
	d, _ := f.save(reviewedText)
	f.setRefs(d.ID, r)
	cases := map[string]error{}
	_, cases["reference"] = f.vl.SetReferences(f.ctx, d.ID, []uuid.UUID{uuid.New()})
	_, cases["contact"] = f.vl.SetContactBlocks(f.ctx, d.ID, []uuid.UUID{uuid.New()})
	_, cases["definition"] = f.vl.SetDefinitions(f.ctx, d.ID, []uuid.UUID{uuid.New()})
	for kind, err := range cases {
		if !errors.Is(err, store.ErrLinkItemNotFound) {
			t.Errorf("%s: err = %v, want ErrLinkItemNotFound", kind, err)
		}
	}
	// a refused call changes nothing
	sameIDs(t, "references", refIDs(f.links(d.ID)), r)
}

// --- definitions ---

func TestSetVersionDefinitions_ListedAlphabetically(t *testing.T) {
	f := newLinkFixture(t)
	rto, rpo, biz := f.def("RTO"), f.def("RPO"), f.def("Business expense")
	d, _ := f.save(reviewedText)
	l, err := f.vl.SetDefinitions(f.ctx, d.ID, []uuid.UUID{biz, rpo, rto})
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, e := range l.Definitions {
		got = append(got, e.Term)
	}
	if fmt.Sprint(got) != "[Business expense RPO RTO]" {
		t.Fatalf("terms = %v", got)
	}
}

func TestDefinitionTitle_UniqueIgnoringCaseAndSpace(t *testing.T) {
	f := newLinkFixture(t)
	f.def("RTO")
	for _, term := range []string{"RTO", "rto", "  Rto "} {
		_, err := f.defs.CreateDefinitionEntry(f.ctx, domain.DefinitionEntry{CategoryID: f.p.HomeCategoryID, Term: term, Definition: "x"})
		if !errors.Is(err, store.ErrDefinitionTermTaken) {
			t.Errorf("%q: err = %v, want ErrDefinitionTermTaken", term, err)
		}
	}
	if _, err := f.defs.CreateDefinitionEntry(f.ctx, domain.DefinitionEntry{CategoryID: f.p.HomeCategoryID, Term: "MTTR", Definition: "x"}); err != nil {
		t.Fatalf("a fresh title: %v", err)
	}
}

func TestDefinitionTitle_UniqueAcrossCategories(t *testing.T) {
	f := newLinkFixture(t)
	f.def("Business expense")
	other, err := store.NewCategoryStore(f.db).Create(f.ctx, mustCategory(t, "Records"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.defs.CreateDefinitionEntry(f.ctx, domain.DefinitionEntry{CategoryID: other.ID, Term: "Business expense", Definition: "x"})
	if !errors.Is(err, store.ErrDefinitionTermTaken) {
		t.Fatalf("err = %v, want ErrDefinitionTermTaken", err)
	}
}

func TestDefinitionTitle_ArchivedStillHoldsTheTitle(t *testing.T) {
	f := newLinkFixture(t)
	id := f.def("RTO")
	if _, err := f.defs.SetDefinitionEntryArchived(f.ctx, id, true); err != nil {
		t.Fatal(err)
	}
	_, err := f.defs.CreateDefinitionEntry(f.ctx, domain.DefinitionEntry{CategoryID: f.p.HomeCategoryID, Term: "rto", Definition: "x"})
	if !errors.Is(err, store.ErrDefinitionTermTaken) {
		t.Fatalf("err = %v, want ErrDefinitionTermTaken", err)
	}
}

func TestDefinitionTitle_RenameRules(t *testing.T) {
	f := newLinkFixture(t)
	f.def("RTO")
	rpo := f.def("RPO")
	_, err := f.defs.UpdateDefinitionEntry(f.ctx, rpo, domain.DefinitionEntry{Term: "RTO", Definition: "x"})
	if !errors.Is(err, store.ErrDefinitionTermTaken) {
		t.Fatalf("rename onto another title: err = %v, want ErrDefinitionTermTaken", err)
	}
	got, err := f.defs.UpdateDefinitionEntry(f.ctx, rpo, domain.DefinitionEntry{Term: " RPO ", Definition: "Reworded."})
	if err != nil {
		t.Fatalf("keeping its own title: %v", err)
	}
	if got.Term != "RPO" || got.Definition != "Reworded." {
		t.Fatalf("updated = %+v", got)
	}
}

func TestDefinitionList_Alphabetical(t *testing.T) {
	f := newLinkFixture(t)
	for _, term := range []string{"add", "bob", "a", "Zed", "Charlie"} {
		f.def(term)
	}
	list, err := f.defs.ListDefinitionEntries(f.ctx, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, d := range list {
		got = append(got, d.Term)
	}
	if fmt.Sprint(got) != "[a add bob Charlie Zed]" {
		t.Fatalf("terms = %v, want case-insensitive alphabetical", got)
	}
}

// --- related items and their relationship types ---

func TestVersionLinks_IncomingFromPublishedVersion(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	// The target has no published version; its own draft shows the incoming link.
	d, _ := f.targetDraft(s.target)
	l := f.links(d.ID)
	if len(l.Related) != 1 {
		t.Fatalf("related = %+v", l.Related)
	}
	r := l.Related[0]
	if r.PolicyID != f.p.ID || r.Type != "RELATED_BY" || r.Label != "is related by" || r.Direction != domain.RelatedIncoming {
		t.Fatalf("incoming link = %+v", r)
	}
	if r.Number == "" || r.Title != f.p.Title {
		t.Fatalf("incoming number/title = %q / %q", r.Number, r.Title)
	}
}

func (f *linkFixture) targetDraft(p domain.Policy) (domain.PolicyVersion, store.UpsertDraftResult) {
	f.t.Helper()
	d, _ := domain.NewPolicyVersionDraft(p.ID, f.tv.ID, uuid.New(), reviewedText)
	saved, res, err := f.ps.UpsertDraft(f.ctx, d)
	if err != nil {
		f.t.Fatalf("UpsertDraft target: %v", err)
	}
	return saved, res
}

func TestSetVersionRelated_StoresType(t *testing.T) {
	f := newLinkFixture(t)
	t1, t2 := f.otherPolicy("One"), f.otherPolicy("Two")
	d, _ := f.save(reviewedText)
	l, err := f.vl.SetRelated(f.ctx, d.ID, []domain.RelatedLinkInput{{PolicyID: t1.ID, Type: "SUPERSEDES"}, {PolicyID: t2.ID, Type: "RELATED_TO"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Related) != 2 {
		t.Fatalf("related = %+v", l.Related)
	}
	want := []struct {
		id         uuid.UUID
		typ, label string
	}{{t1.ID, "SUPERSEDES", "supersedes"}, {t2.ID, "RELATED_TO", "is related to"}}
	for i, w := range want {
		r := l.Related[i]
		if r.PolicyID != w.id || r.Type != w.typ || r.Label != w.label || r.Direction != domain.RelatedOutgoing {
			t.Errorf("related[%d] = %+v, want %+v", i, r, w)
		}
	}
}

func TestVersionLinks_IncomingAppearsOnPublish(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	tv, _ := f.targetDraft(s.target)
	draft := f.newDraft()
	if _, err := f.vl.SetRelated(f.ctx, draft.ID, []domain.RelatedLinkInput{{PolicyID: s.target.ID, Type: "SUPERSEDES"}}); err != nil {
		t.Fatal(err)
	}
	before := f.links(tv.ID).Related
	if len(before) != 1 || before[0].Type != "RELATED_BY" {
		t.Fatalf("before publish = %+v, want the published version's inverse only", before)
	}
	if _, err := f.ps.PublishDraft(f.ctx, f.p.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	after := f.links(tv.ID).Related
	if len(after) != 1 || after[0].Type != "SUPERSEDED_BY" || after[0].Label != "is superseded by" {
		t.Fatalf("after publish = %+v", after)
	}
}

func TestSetVersionRelated_Refusals(t *testing.T) {
	f := newLinkFixture(t)
	other := f.otherPolicy("Other")
	d, _ := f.save(reviewedText)
	_, err := f.vl.SetRelated(f.ctx, d.ID, []domain.RelatedLinkInput{{PolicyID: other.ID, Type: "LOVES"}})
	if !errors.Is(err, domain.ErrUnknownRelationshipType) {
		t.Errorf("unknown type: %v", err)
	}
	_, err = f.vl.SetRelated(f.ctx, d.ID, []domain.RelatedLinkInput{{PolicyID: f.p.ID, Type: "RELATED_TO"}})
	if !errors.Is(err, domain.ErrRelatedToItself) {
		t.Errorf("itself: %v", err)
	}
	_, err = f.vl.SetRelated(f.ctx, d.ID, []domain.RelatedLinkInput{{PolicyID: other.ID, Type: "RELATED_TO"}, {PolicyID: other.ID, Type: "RELATED_TO"}})
	if !errors.Is(err, domain.ErrRepeatedLink) {
		t.Errorf("repeated: %v", err)
	}
}

func TestSetVersionRelated_RefusesUnknownPolicy(t *testing.T) {
	f := newLinkFixture(t)
	d, _ := f.save(reviewedText)
	_, err := f.vl.SetRelated(f.ctx, d.ID, []domain.RelatedLinkInput{{PolicyID: uuid.New(), Type: "RELATED_TO"}})
	if !errors.Is(err, store.ErrRelatedPolicyNotFound) {
		t.Fatalf("err = %v, want ErrRelatedPolicyNotFound", err)
	}
}

// --- the library stays unversioned ---

func TestVersionLinks_LibraryEditShowsOnEveryVersion(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	draft := f.newDraft()
	if _, err := f.refs.UpdateReference(f.ctx, s.r1, domain.Reference{Label: "Renamed", Kind: domain.ReferenceKindStandard}); err != nil {
		t.Fatal(err)
	}
	for name, id := range map[string]uuid.UUID{"published": s.pub.ID, "draft": draft.ID} {
		found := ""
		for _, r := range f.links(id).References {
			if r.ID == s.r1 {
				found = r.Label
			}
		}
		if found != "Renamed" {
			t.Errorf("%s: label = %q, want Renamed", name, found)
		}
	}
}

func TestUsedByCount_PolicyCountedOnce(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	f.newDraft() // published and draft both link r1, d1, c1
	l := f.links(s.pub.ID)
	if l.References[0].ID != s.r1 || l.References[0].UsedByCount != 1 {
		t.Fatalf("link used-by = %+v, want 1", l.References[0])
	}
	list, err := f.refs.ListReferences(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range list {
		if r.ID == s.r1 && r.UsedByCount != 1 {
			t.Errorf("library reference used-by = %d, want 1", r.UsedByCount)
		}
		if r.ID == s.r2 && r.UsedByCount != 0 {
			t.Errorf("unlinked reference used-by = %d, want 0", r.UsedByCount)
		}
	}
	blocks, _ := f.cons.ListBlocks(f.ctx, false)
	for _, b := range blocks {
		if b.ID == s.c1 && b.UsedByCount != 1 {
			t.Errorf("contact used-by = %d, want 1", b.UsedByCount)
		}
	}
	defs, _ := f.defs.ListDefinitionEntries(f.ctx, nil, false)
	for _, d := range defs {
		if d.ID == s.d1 && d.UsedByCount != 1 {
			t.Errorf("definition used-by = %d, want 1", d.UsedByCount)
		}
	}
}

// A library item that any version links cannot be deleted (archive it
// instead), so an approved version never loses a link.
func TestDelete_RefusedWhileAVersionLinksTheItem(t *testing.T) {
	f := newLinkFixture(t)
	s := f.seed()
	if err := f.refs.DeleteReference(f.ctx, s.r1); !errors.Is(err, store.ErrReferenceInUse) {
		t.Fatalf("reference: err = %v, want ErrReferenceInUse", err)
	}
	if err := f.defs.DeleteDefinitionEntry(f.ctx, s.d1); !errors.Is(err, store.ErrDefinitionEntryInUse) {
		t.Fatalf("definition: err = %v, want ErrDefinitionEntryInUse", err)
	}
	if err := f.refs.DeleteReference(f.ctx, s.r2); err != nil {
		t.Fatalf("unlinked reference: %v", err)
	}
}

func mustCategory(t *testing.T, name string) domain.Category {
	t.Helper()
	c, err := domain.NewCategory(name, "records-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	return c
}
