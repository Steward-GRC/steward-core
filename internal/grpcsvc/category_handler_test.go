// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type fakeCategoryStore struct {
	categories map[uuid.UUID]domain.Category
}

func newFakeCategoryStore() *fakeCategoryStore {
	return &fakeCategoryStore{categories: map[uuid.UUID]domain.Category{}}
}

func (f *fakeCategoryStore) Create(_ context.Context, g domain.Category) (domain.Category, error) {
	if g.ID == uuid.Nil {
		g.ID = uuid.New()
	}
	f.categories[g.ID] = g
	return g, nil
}

func (f *fakeCategoryStore) Get(_ context.Context, id uuid.UUID) (domain.Category, error) {
	g, ok := f.categories[id]
	if !ok {
		return domain.Category{}, fmt.Errorf("not found")
	}
	return g, nil
}

func (f *fakeCategoryStore) ListChildren(_ context.Context, parentID uuid.UUID) ([]domain.Category, error) {
	var out []domain.Category
	for _, g := range f.categories {
		if g.ParentID == parentID {
			out = append(out, g)
		}
	}
	return out, nil
}

func (f *fakeCategoryStore) AncestorChain(_ context.Context, categoryID uuid.UUID) ([]domain.Category, error) {
	var chain []domain.Category
	id := categoryID
	for id != uuid.Nil {
		g, ok := f.categories[id]
		if !ok {
			break
		}
		chain = append(chain, g)
		id = g.ParentID
	}
	return chain, nil
}

func (f *fakeCategoryStore) SetDefaults(_ context.Context, id uuid.UUID, tplID, wfID uuid.UUID, tplNone bool) (domain.Category, error) {
	g := f.categories[id]
	g.DefaultTemplateID = tplID
	g.DefaultTemplateNone = tplNone
	g.DefaultWorkflowID = wfID
	f.categories[id] = g
	return g, nil
}

func (f *fakeCategoryStore) SetGovernance(_ context.Context, id uuid.UUID, owners []uuid.UUID, audienceGroupIDs *[]string, ack domain.AckTrigger, cadence domain.ReviewCadence, reviewDate *time.Time, exclusionGroupIDs *[]string, ackEveryone *bool) (domain.Category, error) {
	g, ok := f.categories[id]
	if !ok {
		return domain.Category{}, fmt.Errorf("not found")
	}
	g.Owners = owners
	g.AudienceGroupIDs = audienceGroupIDs
	g.AckTriggers = ack
	g.ReviewCadence = cadence
	g.ReviewDate = reviewDate
	g.ExclusionGroupIDs = exclusionGroupIDs
	g.AckEveryone = ackEveryone
	f.categories[id] = g
	return g, nil
}

func (f *fakeCategoryStore) Rename(_ context.Context, id uuid.UUID, name, slug string) (domain.Category, error) {
	g, ok := f.categories[id]
	if !ok {
		return domain.Category{}, store.ErrCategoryNotFound
	}
	g.Name = name
	g.Slug = slug
	f.categories[id] = g
	return g, nil
}

func (f *fakeCategoryStore) Delete(_ context.Context, id uuid.UUID) error {
	if _, ok := f.categories[id]; !ok {
		return store.ErrCategoryNotFound
	}
	// The fake holds no documents; the store test covers that guard.
	queue := []uuid.UUID{id}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, c := range f.categories {
			if c.ParentID == cur {
				queue = append(queue, c.ID)
			}
		}
		delete(f.categories, cur)
	}
	return nil
}

// MoveCategory repeats the store's cycle and depth checks in memory, so the
// handler's error mapping is testable without Postgres.
func (f *fakeCategoryStore) MoveCategory(_ context.Context, categoryID uuid.UUID, newParentID *uuid.UUID) (domain.Category, int, error) {
	if _, ok := f.categories[categoryID]; !ok {
		return domain.Category{}, 0, store.ErrCategoryNotFound
	}
	subtree := map[uuid.UUID]int{}
	queue := []struct {
		id  uuid.UUID
		rel int
	}{{categoryID, 0}}
	maxRel := 0
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		subtree[cur.id] = cur.rel
		if cur.rel > maxRel {
			maxRel = cur.rel
		}
		for _, c := range f.categories {
			if c.ParentID == cur.id {
				queue = append(queue, struct {
					id  uuid.UUID
					rel int
				}{c.ID, cur.rel + 1})
			}
		}
	}
	height := maxRel + 1

	newParentDepth := 0
	if newParentID != nil {
		if _, inSubtree := subtree[*newParentID]; inSubtree {
			return domain.Category{}, 0, store.ErrCategoryMoveCycle
		}
		if _, ok := f.categories[*newParentID]; !ok {
			return domain.Category{}, 0, store.ErrCategoryNotFound
		}
		depth := 0
		id := *newParentID
		for id != uuid.Nil {
			g, ok := f.categories[id]
			if !ok {
				break
			}
			depth++
			id = g.ParentID
		}
		newParentDepth = depth
	}
	if newParentDepth+height > 3 {
		return domain.Category{}, 0, store.ErrCategoryMoveTooDeep
	}

	g := f.categories[categoryID]
	if newParentID != nil {
		g.ParentID = *newParentID
	} else {
		g.ParentID = uuid.Nil
	}
	f.categories[categoryID] = g
	return g, len(subtree), nil
}

func (f *fakeCategoryStore) Subtree(_ context.Context, rootID uuid.UUID) ([]domain.Category, error) {
	var out []domain.Category
	visited := map[uuid.UUID]bool{}
	queue := []uuid.UUID{rootID}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if visited[cur] {
			continue
		}
		visited[cur] = true
		g, ok := f.categories[cur]
		if !ok {
			continue
		}
		out = append(out, g)
		for _, candidate := range f.categories {
			if candidate.ParentID == cur {
				queue = append(queue, candidate.ID)
			}
		}
	}
	return out, nil
}

func (f *fakeCategoryStore) GetCategoryRuleset(_ context.Context, _ uuid.UUID) ([]domain.CategoryRule, error) {
	return []domain.CategoryRule{}, nil
}

func (f *fakeCategoryStore) SetCategoryRuleset(_ context.Context, _ uuid.UUID, rules []domain.CategoryRule) ([]domain.CategoryRule, error) {
	return rules, nil
}

func (f *fakeCategoryStore) PurgeUserCategoryRules(_ context.Context, _ uuid.UUID, _ bool) ([]store.PurgedCategoryRule, error) {
	return nil, nil
}

func TestCategoryHandlerCreateAndGet(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	ctx := context.Background()

	resp, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "HR", Slug: "hr"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	if resp.Category.Id == "" {
		t.Fatal("expected non-empty ID")
	}

	got, err := h.GetCategory(ctx, &corev1.GetCategoryRequest{Id: resp.Category.Id})
	if err != nil {
		t.Fatalf("GetCategory: %v", err)
	}
	if got.Category.Name != "HR" {
		t.Fatalf("expected HR, got %q", got.Category.Name)
	}
}

func TestCategoryHandlerCreateInvalidParent(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	_, err := h.CreateCategory(context.Background(), &corev1.CreateCategoryRequest{
		Name: "HR", Slug: "hr", ParentId: "not-a-uuid",
	})
	if err == nil {
		t.Fatal("expected error for invalid parent_id")
	}
}

func TestCategoryHandlerGetInvalidID(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	_, err := h.GetCategory(context.Background(), &corev1.GetCategoryRequest{Id: "not-a-uuid"})
	if err == nil {
		t.Fatal("expected error for invalid id")
	}
}

func TestSetGovernanceOnChange(t *testing.T) {
	fgs := newFakeCategoryStore()
	cap := &capturePublisher{}
	em := audit.New(cap)
	h := grpcsvc.NewCategoryHandler(fgs, em)
	ctx := context.Background()

	cr, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Finance", Slug: "finance"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	gid := cr.Category.Id
	owner1 := uuid.New().String()

	resp, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                       gid,
		Owners:                   []string{owner1},
		AudienceGroupIds:         []string{"AD-Finance", "AD-Admins"},
		AudienceGroupIdsProvided: true,
		AckTriggers:              corev1.AckTrigger_ACK_TRIGGER_ON_CHANGE,
		ReviewCadence:            corev1.ReviewCadence_REVIEW_CADENCE_ANNUAL,
		ReviewDate:               "",
	})
	if err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}
	g := resp.Category
	if g.Id != gid {
		t.Fatalf("id: got %q want %q", g.Id, gid)
	}
	if len(g.Owners) != 1 || g.Owners[0] != owner1 {
		t.Fatalf("owners: got %v want [%s]", g.Owners, owner1)
	}
	if len(g.AudienceGroupIds) != 2 {
		t.Fatalf("audience_group_ids: got %v", g.AudienceGroupIds)
	}
	if g.AckTriggers != corev1.AckTrigger_ACK_TRIGGER_ON_CHANGE {
		t.Fatalf("ack_triggers: got %v want ON_CHANGE", g.AckTriggers)
	}
	if g.ReviewCadence != corev1.ReviewCadence_REVIEW_CADENCE_ANNUAL {
		t.Fatalf("review_cadence: got %v want ANNUAL", g.ReviewCadence)
	}
	if g.ReviewDate != "" {
		t.Fatalf("review_date: expected empty, got %q", g.ReviewDate)
	}
	if len(cap.calls) < 2 { // first for CreateCategory, second for SetGovernance
		t.Fatalf("expected at least 2 audit events, got %d", len(cap.calls))
	}
	last := cap.calls[len(cap.calls)-1]
	if last.event.Action != "category.governance_updated" {
		t.Fatalf("audit action: got %q want category.governance_updated", last.event.Action)
	}
}

func TestCategoryHandlerActorFlowsToAudit(t *testing.T) {
	fgs := newFakeCategoryStore()
	cap := &capturePublisher{}
	h := grpcsvc.NewCategoryHandler(fgs, audit.New(cap))
	ctx := context.Background()
	actor := uuid.New().String()

	cr, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{
		Name: "Ops", Slug: "ops", ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	gid := cr.Category.Id

	if _, err := h.SetCategoryDefaults(ctx, &corev1.SetCategoryDefaultsRequest{
		Id: gid, ActorUserId: actor,
	}); err != nil {
		t.Fatalf("SetCategoryDefaults: %v", err)
	}
	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id: gid, ActorUserId: actor,
	}); err != nil {
		t.Fatalf("SetGovernance: %v", err)
	}

	want := map[string]bool{
		"category.created":            false,
		"category.defaults_set":       false,
		"category.governance_updated": false,
	}
	for _, c := range cap.calls {
		if _, ok := want[c.event.Action]; !ok {
			continue
		}
		want[c.event.Action] = true
		if c.event.ActorUserID != actor {
			t.Fatalf("action %q: actor_user_id got %q want %q", c.event.Action, c.event.ActorUserID, actor)
		}
	}
	for action, seen := range want {
		if !seen {
			t.Fatalf("expected an audit event for %q", action)
		}
	}
}

func TestSetGovernanceEnumMapping(t *testing.T) {
	fgs := newFakeCategoryStore()
	h := grpcsvc.NewCategoryHandler(fgs, nil)
	ctx := context.Background()

	cr, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "HR", Slug: "hr"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	gid := cr.Category.Id

	// An unset trigger reads back as NONE.
	resp, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:          gid,
		AckTriggers: corev1.AckTrigger_ACK_TRIGGER_UNSPECIFIED,
	})
	if err != nil {
		t.Fatalf("SetGovernance UNSPECIFIED: %v", err)
	}
	if resp.Category.AckTriggers != corev1.AckTrigger_ACK_TRIGGER_NONE {
		t.Fatalf("UNSPECIFIED should map to NONE, got %v", resp.Category.AckTriggers)
	}

	resp, err = h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:          gid,
		AckTriggers: corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH,
	})
	if err != nil {
		t.Fatalf("SetGovernance ON_PUBLISH: %v", err)
	}
	if resp.Category.AckTriggers != corev1.AckTrigger_ACK_TRIGGER_ON_PUBLISH {
		t.Fatalf("ON_PUBLISH round-trip: got %v", resp.Category.AckTriggers)
	}
}

func TestSetGovernanceReviewDate(t *testing.T) {
	fgs := newFakeCategoryStore()
	h := grpcsvc.NewCategoryHandler(fgs, nil)
	ctx := context.Background()

	cr, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Legal", Slug: "legal"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}

	resp, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:            cr.Category.Id,
		ReviewCadence: corev1.ReviewCadence_REVIEW_CADENCE_ON_DATE,
		ReviewDate:    "2027-01-15",
	})
	if err != nil {
		t.Fatalf("SetGovernance with review_date: %v", err)
	}
	if resp.Category.ReviewDate != "2027-01-15" {
		t.Fatalf("review_date: got %q want 2027-01-15", resp.Category.ReviewDate)
	}
}

func TestSetGovernanceExclusionGroups(t *testing.T) {
	store := newFakeCategoryStore()
	h := grpcsvc.NewCategoryHandler(store, nil)
	ctx := context.Background()

	created, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Eng", Slug: "eng"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	gid := created.Category.Id

	resp, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                        gid,
		AudienceGroupIds:          []string{"audience-category"},
		AudienceGroupIdsProvided:  true,
		ExclusionGroupIds:         []string{"excluded-category"},
		ExclusionGroupIdsProvided: true,
		AckTriggers:               corev1.AckTrigger_ACK_TRIGGER_NONE,
		ReviewCadence:             corev1.ReviewCadence_REVIEW_CADENCE_NONE,
		ActorUserId:               uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("SetGovernance with exclusion: %v", err)
	}
	if !resp.Category.ExclusionGroupIdsSet {
		t.Fatal("expected ExclusionGroupIdsSet=true")
	}
	if len(resp.Category.ExclusionGroupIds) != 1 || resp.Category.ExclusionGroupIds[0] != "excluded-category" {
		t.Fatalf("exclusion_group_ids: got %v, want [excluded-category]", resp.Category.ExclusionGroupIds)
	}
}

func TestGetEffectiveGovernance_InheritedAudience(t *testing.T) {
	store := newFakeCategoryStore()
	h := grpcsvc.NewCategoryHandler(store, nil)
	ctx := context.Background()

	rootResp, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Root", Slug: "root"})
	if err != nil {
		t.Fatalf("CreateCategory root: %v", err)
	}
	rootID := rootResp.Category.Id

	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                       rootID,
		AudienceGroupIds:         []string{"all-staff"},
		AudienceGroupIdsProvided: true,
		ActorUserId:              uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetGovernance root: %v", err)
	}

	childResp, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{
		Name: "Child", Slug: "child", ParentId: rootID,
	})
	if err != nil {
		t.Fatalf("CreateCategory child: %v", err)
	}
	childID := childResp.Category.Id

	// The child inherits the root's audience.
	eff, err := h.GetEffectiveGovernance(ctx, &corev1.GetEffectiveGovernanceRequest{CategoryId: childID})
	if err != nil {
		t.Fatalf("GetEffectiveGovernance: %v", err)
	}
	if !eff.AudienceSet {
		t.Fatal("expected AudienceSet=true (root has audience)")
	}
	if len(eff.AckAudienceGroups) != 1 || eff.AckAudienceGroups[0] != "all-staff" {
		t.Fatalf("effective audience: got %v, want [all-staff]", eff.AckAudienceGroups)
	}
	if eff.ExclusionSet {
		t.Fatal("expected ExclusionSet=false (no exclusion set in chain)")
	}
}

func TestGetEffectiveGovernance_LeafOverridesRoot(t *testing.T) {
	store := newFakeCategoryStore()
	h := grpcsvc.NewCategoryHandler(store, nil)
	ctx := context.Background()

	rootResp, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Root", Slug: "root"})
	if err != nil {
		t.Fatalf("CreateCategory root: %v", err)
	}
	rootID := rootResp.Category.Id
	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                       rootID,
		AudienceGroupIds:         []string{"root-audience"},
		AudienceGroupIdsProvided: true,
		ActorUserId:              uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetGovernance root: %v", err)
	}

	childResp, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{
		Name: "Child", Slug: "child", ParentId: rootID,
	})
	if err != nil {
		t.Fatalf("CreateCategory child: %v", err)
	}
	childID := childResp.Category.Id
	if _, err := h.SetGovernance(ctx, &corev1.SetGovernanceRequest{
		Id:                       childID,
		AudienceGroupIds:         []string{"leaf-audience"},
		AudienceGroupIdsProvided: true,
		ActorUserId:              uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetGovernance child: %v", err)
	}

	eff, err := h.GetEffectiveGovernance(ctx, &corev1.GetEffectiveGovernanceRequest{CategoryId: childID})
	if err != nil {
		t.Fatalf("GetEffectiveGovernance: %v", err)
	}
	if len(eff.AckAudienceGroups) != 1 || eff.AckAudienceGroups[0] != "leaf-audience" {
		t.Fatalf("leaf should win; got %v", eff.AckAudienceGroups)
	}
}

func makeCategory(t *testing.T, h *grpcsvc.CategoryHandler, name, slug, parentID string) string {
	t.Helper()
	resp, err := h.CreateCategory(context.Background(), &corev1.CreateCategoryRequest{
		Name: name, Slug: slug, ParentId: parentID,
	})
	if err != nil {
		t.Fatalf("CreateCategory %s: %v", name, err)
	}
	return resp.Category.Id
}

func TestMoveCategoryReParents(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	ctx := context.Background()

	rootA := makeCategory(t, h, "A", "a", "")
	rootB := makeCategory(t, h, "B", "b", "")

	resp, err := h.MoveCategory(ctx, &corev1.MoveCategoryRequest{CategoryId: rootB, NewParentId: rootA})
	if err != nil {
		t.Fatalf("MoveCategory: %v", err)
	}
	if resp.Category.ParentId != rootA {
		t.Fatalf("parent: got %q want %q", resp.Category.ParentId, rootA)
	}
	if resp.AffectedCount != 1 {
		t.Fatalf("affected_count: got %d want 1", resp.AffectedCount)
	}
}

func TestMoveCategoryToRoot(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	ctx := context.Background()

	root := makeCategory(t, h, "Root", "root", "")
	child := makeCategory(t, h, "Child", "child", root)

	resp, err := h.MoveCategory(ctx, &corev1.MoveCategoryRequest{CategoryId: child, NewParentId: ""})
	if err != nil {
		t.Fatalf("MoveCategory to root: %v", err)
	}
	if resp.Category.ParentId != "" {
		t.Fatalf("expected empty parent (root), got %q", resp.Category.ParentId)
	}
}

func TestMoveCategoryDepthBoundary(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	ctx := context.Background()

	// top -> mid -> leaf: the subtree under top is two levels high.
	top := makeCategory(t, h, "Top", "top", "")
	mid := makeCategory(t, h, "Mid", "mid", "")
	_ = makeCategory(t, h, "Leaf", "leaf", mid) // mid's subtree height = 2

	if _, err := h.MoveCategory(ctx, &corev1.MoveCategoryRequest{CategoryId: mid, NewParentId: top}); err != nil {
		t.Fatalf("depth-3 move should succeed: %v", err)
	}

	// Under another root, top's three levels would reach depth four.
	other := makeCategory(t, h, "Other", "other", "")
	_, err := h.MoveCategory(ctx, &corev1.MoveCategoryRequest{CategoryId: top, NewParentId: other})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("depth-4 move should fail FailedPrecondition, got %v (code %s)", err, status.Code(err))
	}
	got, err := h.GetCategory(ctx, &corev1.GetCategoryRequest{Id: top})
	if err != nil {
		t.Fatalf("GetCategory top: %v", err)
	}
	if got.Category.ParentId != "" {
		t.Fatalf("failed move must not change parent; got %q", got.Category.ParentId)
	}
}

// fakeCategoryStoreWithRules adds stored rulesets to fakeCategoryStore.
type fakeCategoryStoreWithRules struct {
	*fakeCategoryStore
	rulesets map[uuid.UUID][]domain.CategoryRule
}

func newFakeCategoryStoreWithRules() *fakeCategoryStoreWithRules {
	return &fakeCategoryStoreWithRules{
		fakeCategoryStore: newFakeCategoryStore(),
		rulesets:          map[uuid.UUID][]domain.CategoryRule{},
	}
}

func (f *fakeCategoryStoreWithRules) GetCategoryRuleset(_ context.Context, categoryID uuid.UUID) ([]domain.CategoryRule, error) {
	if r, ok := f.rulesets[categoryID]; ok {
		return r, nil
	}
	return []domain.CategoryRule{}, nil
}

func (f *fakeCategoryStoreWithRules) SetCategoryRuleset(_ context.Context, categoryID uuid.UUID, rules []domain.CategoryRule) ([]domain.CategoryRule, error) {
	out := make([]domain.CategoryRule, len(rules))
	for i, r := range rules {
		r.Ordinal = i + 1
		out[i] = r
	}
	f.rulesets[categoryID] = out
	return out, nil
}

// PurgeUserCategoryRules mirrors the store: user rules naming userID go, the
// surviving ordinals close up, and the removed rows come back sorted so the
// audit order is stable.
func (f *fakeCategoryStoreWithRules) PurgeUserCategoryRules(_ context.Context, userID uuid.UUID, dryRun bool) ([]store.PurgedCategoryRule, error) {
	cats := make([]uuid.UUID, 0, len(f.rulesets))
	for id := range f.rulesets {
		cats = append(cats, id)
	}
	sort.Slice(cats, func(i, j int) bool { return cats[i].String() < cats[j].String() })

	out := []store.PurgedCategoryRule{}
	for _, catID := range cats {
		keep := make([]domain.CategoryRule, 0, len(f.rulesets[catID]))
		for _, r := range f.rulesets[catID] {
			if r.SubjectKind == "user" && r.SubjectRef == userID.String() {
				out = append(out, store.PurgedCategoryRule{CategoryID: catID, Rule: r})
				continue
			}
			keep = append(keep, r)
		}
		if dryRun || len(keep) == len(f.rulesets[catID]) {
			continue
		}
		for i := range keep {
			keep[i].Ordinal = i + 1
		}
		f.rulesets[catID] = keep
	}
	return out, nil
}

func TestCategoryHandler_SetThenGetCategoryRuleset(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	h := grpcsvc.NewCategoryHandler(fs, nil)
	ctx := context.Background()

	cr, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Legal", Slug: "legal"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	catID := cr.Category.Id

	setResp, err := h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			{
				SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_EVERYONE,
				SubjectRef:  "",
				Read:        corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				Ack:         corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				Approve:     corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				Author:      corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			},
			{
				SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_GROUP,
				SubjectRef:  "finance-leads",
				Read:        corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				Ack:         corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				Approve:     corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				Author:      corev1.GrantEffect_GRANT_EFFECT_DENY,
			},
			{
				SubjectKind: corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER,
				SubjectRef:  "alice@example.com",
				Read:        corev1.GrantEffect_GRANT_EFFECT_ALLOW,
				Ack:         corev1.GrantEffect_GRANT_EFFECT_DENY,
				Approve:     corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
				Author:      corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED,
			},
		},
	})
	if err != nil {
		t.Fatalf("SetCategoryRuleset: %v", err)
	}
	if len(setResp.Rules) != 3 {
		t.Fatalf("Set: expected 3 rules back, got %d", len(setResp.Rules))
	}
	r0 := setResp.Rules[0]
	if r0.SubjectKind != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_EVERYONE {
		t.Fatalf("r0.SubjectKind: got %v want EVERYONE", r0.SubjectKind)
	}
	if r0.Read != corev1.GrantEffect_GRANT_EFFECT_ALLOW {
		t.Fatalf("r0.Read: got %v want ALLOW", r0.Read)
	}
	if r0.Approve != corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED {
		t.Fatalf("r0.Approve: got %v want UNSPECIFIED", r0.Approve)
	}
	r1 := setResp.Rules[1]
	if r1.SubjectKind != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_GROUP {
		t.Fatalf("r1.SubjectKind: got %v want CATEGORY", r1.SubjectKind)
	}
	if r1.SubjectRef != "finance-leads" {
		t.Fatalf("r1.SubjectRef: got %q want finance-leads", r1.SubjectRef)
	}
	if r1.Author != corev1.GrantEffect_GRANT_EFFECT_DENY {
		t.Fatalf("r1.Author: got %v want DENY", r1.Author)
	}
	r2 := setResp.Rules[2]
	if r2.SubjectKind != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER {
		t.Fatalf("r2.SubjectKind: got %v want USER", r2.SubjectKind)
	}
	if r2.SubjectRef != "alice@example.com" {
		t.Fatalf("r2.SubjectRef: got %q want alice@example.com", r2.SubjectRef)
	}
	if r2.Ack != corev1.GrantEffect_GRANT_EFFECT_DENY {
		t.Fatalf("r2.Ack: got %v want DENY", r2.Ack)
	}

	getResp, err := h.GetCategoryRuleset(ctx, &corev1.GetCategoryRulesetRequest{CategoryId: catID})
	if err != nil {
		t.Fatalf("GetCategoryRuleset: %v", err)
	}
	if len(getResp.Rules) != 3 {
		t.Fatalf("Get: expected 3 rules, got %d", len(getResp.Rules))
	}
	if getResp.Rules[0].SubjectKind != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_EVERYONE {
		t.Fatalf("Get r0.SubjectKind: got %v want EVERYONE", getResp.Rules[0].SubjectKind)
	}
	if getResp.Rules[1].SubjectKind != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_GROUP {
		t.Fatalf("Get r1.SubjectKind: got %v want CATEGORY", getResp.Rules[1].SubjectKind)
	}
	if getResp.Rules[1].Author != corev1.GrantEffect_GRANT_EFFECT_DENY {
		t.Fatalf("Get r1.Author: got %v want DENY", getResp.Rules[1].Author)
	}
	if getResp.Rules[2].SubjectKind != corev1.RuleSubjectKind_RULE_SUBJECT_KIND_USER {
		t.Fatalf("Get r2.SubjectKind: got %v want USER", getResp.Rules[2].SubjectKind)
	}
	if getResp.Rules[0].Approve != corev1.GrantEffect_GRANT_EFFECT_UNSPECIFIED {
		t.Fatalf("Get r0.Approve: got %v want UNSPECIFIED", getResp.Rules[0].Approve)
	}
}

func TestCategoryHandler_SetCategoryRuleset_RejectsUnspecifiedSubject(t *testing.T) {
	fs := newFakeCategoryStoreWithRules()
	h := grpcsvc.NewCategoryHandler(fs, nil)
	ctx := context.Background()

	cr, err := h.CreateCategory(ctx, &corev1.CreateCategoryRequest{Name: "Legal", Slug: "legal"})
	if err != nil {
		t.Fatalf("CreateCategory: %v", err)
	}
	catID := cr.Category.Id

	_, err = h.SetCategoryRuleset(ctx, &corev1.SetCategoryRulesetRequest{
		CategoryId: catID,
		Rules: []*corev1.CategoryRule{
			{
				SubjectRef: "",
				Read:       corev1.GrantEffect_GRANT_EFFECT_ALLOW,
			},
		},
	})
	if err == nil {
		t.Fatal("expected error for UNSPECIFIED subject_kind, got nil")
	}
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", got)
	}
}

func TestMoveCategoryCycleRejected(t *testing.T) {
	h := grpcsvc.NewCategoryHandler(newFakeCategoryStore(), nil)
	ctx := context.Background()

	root := makeCategory(t, h, "Root", "root", "")
	child := makeCategory(t, h, "Child", "child", root)

	_, err := h.MoveCategory(ctx, &corev1.MoveCategoryRequest{CategoryId: root, NewParentId: child})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("cycle move should fail InvalidArgument, got %v (code %s)", err, status.Code(err))
	}
	_, err = h.MoveCategory(ctx, &corev1.MoveCategoryRequest{CategoryId: root, NewParentId: root})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("self move should fail InvalidArgument, got %v (code %s)", err, status.Code(err))
	}
	got, err := h.GetCategory(ctx, &corev1.GetCategoryRequest{Id: child})
	if err != nil {
		t.Fatalf("GetCategory child: %v", err)
	}
	if got.Category.ParentId != root {
		t.Fatalf("failed cycle move must not change hierarchy; child parent = %q", got.Category.ParentId)
	}
}
