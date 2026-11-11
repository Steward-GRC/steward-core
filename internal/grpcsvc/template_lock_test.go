// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"testing"

	"github.com/Bugs5382/go-apperr/apperrgrpc"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// SetTemplateLock mirrors the store: a lock needs a default template to hold to.
func (f *fakeCategoryStore) SetTemplateLock(_ context.Context, id uuid.UUID, locked bool) (domain.Category, error) {
	g, ok := f.categories[id]
	if !ok {
		return domain.Category{}, store.ErrCategoryNotFound
	}
	if locked && g.DefaultTemplateID == uuid.Nil {
		return domain.Category{}, store.ErrTemplateLockNeedsDefault
	}
	g.DefaultTemplateLocked = locked
	f.categories[id] = g
	return g, nil
}

// GetTemplate names every template the same, enough for the refusal message.
func (f *fakeTemplateStore) GetTemplate(_ context.Context, id uuid.UUID) (domain.Template, error) {
	return domain.Template{ID: id, Name: "Standard Policy"}, nil
}

type lockFixture struct {
	cats     *fakeCategoryStore
	policies *fakePolicyStore
	catH     *grpcsvc.CategoryHandler
	polH     *grpcsvc.PolicyHandler
	finance  domain.Category
	expenses domain.Category
	travel   domain.Category
	tpl1     uuid.UUID
	tpl2     uuid.UUID
}

// newLockFixture mirrors the mock: Finance names template 1, Expenses inherits it, Travel names none.
func newLockFixture(t *testing.T) *lockFixture {
	t.Helper()
	f := &lockFixture{
		cats: newFakeCategoryStore(), policies: newFakePolicyStore(),
		tpl1: uuid.New(), tpl2: uuid.New(),
	}
	ctx := context.Background()
	var err error
	if f.finance, err = f.cats.Create(ctx, domain.Category{Name: "Finance", Slug: "finance", DefaultTemplateID: f.tpl1}); err != nil {
		t.Fatal(err)
	}
	if f.expenses, err = f.cats.Create(ctx, domain.Category{Name: "Expenses", Slug: "expenses", ParentID: f.finance.ID}); err != nil {
		t.Fatal(err)
	}
	if f.travel, err = f.cats.Create(ctx, domain.Category{Name: "Travel", Slug: "travel"}); err != nil {
		t.Fatal(err)
	}
	em := audit.New(&capturePublisher{})
	f.catH = grpcsvc.NewCategoryHandler(f.cats, em)
	f.polH = grpcsvc.NewPolicyHandler(f.policies, f.cats, newFakeTemplateStore(), domain.NoopValidator{}, em)
	return f
}

func (f *lockFixture) setLock(t *testing.T, id uuid.UUID, locked bool) (*corev1.SetCategoryTemplateLockResponse, error) {
	t.Helper()
	return f.catH.SetCategoryTemplateLock(context.Background(), &corev1.SetCategoryTemplateLockRequest{
		Id: id.String(), Locked: locked, ActorUserId: uuid.New().String(),
	})
}

func (f *lockFixture) lock(t *testing.T) {
	t.Helper()
	if _, err := f.setLock(t, f.finance.ID, true); err != nil {
		t.Fatalf("lock Finance: %v", err)
	}
}

func (f *lockFixture) create(category uuid.UUID, template string) (*corev1.CreatePolicyResponse, error) {
	return f.polH.CreatePolicy(context.Background(), &corev1.CreatePolicyRequest{
		HomeCategoryId: category.String(), TemplateId: template, Title: "A document",
		Sensitivity: corev1.Sensitivity_SENSITIVITY_STANDARD, OwnerUserId: uuid.New().String(),
	})
}

func requireCoded(t *testing.T, err error, code codes.Code, symbol string, num int) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %s refusal, got nil", symbol)
	}
	st := status.Convert(err)
	if st.Code() != code {
		t.Fatalf("code = %v, want %v (%v)", st.Code(), code, err)
	}
	info, ok := apperrgrpc.FromStatus(st)
	if !ok || info.Symbol != symbol || info.Code != num {
		t.Fatalf("want coded %s/%d, got ok=%v info=%+v", symbol, num, ok, info)
	}
}

// Mock test: "locks a category that names a default template".
func TestSetCategoryTemplateLockLocks(t *testing.T) {
	f := newLockFixture(t)
	resp, err := f.setLock(t, f.finance.ID, true)
	if err != nil {
		t.Fatalf("SetCategoryTemplateLock: %v", err)
	}
	c := resp.GetCategory()
	if !c.GetDefaultTemplateLocked() || c.GetDefaultTemplateId() != f.tpl1.String() || c.GetId() != f.finance.ID.String() {
		t.Fatalf("unexpected category: %+v", c)
	}
}

// Mock test: "needs a default template to lock".
func TestSetCategoryTemplateLockNeedsDefault(t *testing.T) {
	f := newLockFixture(t)
	_, err := f.setLock(t, f.travel.ID, true)
	requireCoded(t, err, codes.FailedPrecondition, "TEMPLATE_LOCK_NEEDS_DEFAULT", 4005)
}

// Mock test: "can be lifted again".
func TestSetCategoryTemplateLockLifts(t *testing.T) {
	f := newLockFixture(t)
	f.lock(t)
	resp, err := f.setLock(t, f.finance.ID, false)
	if err != nil {
		t.Fatalf("lift: %v", err)
	}
	if resp.GetCategory().GetDefaultTemplateLocked() {
		t.Fatal("lock should be lifted")
	}
}

func TestSetCategoryTemplateLockErrors(t *testing.T) {
	f := newLockFixture(t)
	if _, err := f.catH.SetCategoryTemplateLock(context.Background(), &corev1.SetCategoryTemplateLockRequest{Id: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := f.setLock(t, uuid.New(), true); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown category: %v", err)
	}
}

// Mock test: "uses the category's template when none is asked for".
func TestCreatePolicyUnderLockedCategoryFollowsTemplate(t *testing.T) {
	f := newLockFixture(t)
	f.lock(t)
	resp, err := f.create(f.finance.ID, "")
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	// It follows the category's template rather than naming its own, so it is not freeform.
	if resp.GetPolicy().GetTemplateNone() || resp.GetPolicy().GetTemplateId() != "" {
		t.Fatalf("policy should inherit the category template: %+v", resp.GetPolicy())
	}
}

// Mock test: "accepts the locked template when it is named".
func TestCreatePolicyUnderLockedCategoryAcceptsLockedTemplate(t *testing.T) {
	f := newLockFixture(t)
	f.lock(t)
	if _, err := f.create(f.finance.ID, f.tpl1.String()); err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
}

// Mock test: "refuses another template".
func TestCreatePolicyUnderLockedCategoryRefusesOtherTemplate(t *testing.T) {
	f := newLockFixture(t)
	f.lock(t)
	_, err := f.create(f.finance.ID, f.tpl2.String())
	requireCoded(t, err, codes.FailedPrecondition, "CATEGORY_TEMPLATE_LOCKED", 4004)
	if got := status.Convert(err).Message(); got != "This category requires the Standard Policy template." {
		t.Fatalf("message = %q", got)
	}
	if len(f.policies.policies) != 0 {
		t.Fatal("a refused create must not store a policy")
	}
}

// Mock test: "holds under a category that inherits the template".
func TestCreatePolicyUnderLockedCategoryHoldsForInheritingChild(t *testing.T) {
	f := newLockFixture(t)
	f.lock(t)
	_, err := f.create(f.expenses.ID, uuid.New().String())
	requireCoded(t, err, codes.FailedPrecondition, "CATEGORY_TEMPLATE_LOCKED", 4004)
	if _, err := f.create(f.expenses.ID, f.tpl1.String()); err != nil {
		t.Fatalf("the locked template is fine in the child: %v", err)
	}
}

// Mock test: "lets any template through once the lock is lifted".
func TestCreatePolicyLiftedLockLetsAnyTemplateThrough(t *testing.T) {
	f := newLockFixture(t)
	f.lock(t)
	if _, err := f.setLock(t, f.finance.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.create(f.finance.ID, f.tpl2.String()); err != nil {
		t.Fatalf("CreatePolicy after lift: %v", err)
	}
}

// Mock test: "refuses freeform and another template, and allows the locked one".
func TestSetPolicyTemplateUnderLock(t *testing.T) {
	f := newLockFixture(t)
	created, err := f.create(f.finance.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	pid := created.GetPolicy().GetId()
	f.lock(t)
	set := func(template string, none bool) error {
		_, err := f.polH.SetPolicyTemplate(context.Background(), &corev1.SetPolicyTemplateRequest{
			PolicyId: pid, TemplateId: template, None: none, ActorUserId: uuid.New().String(),
		})
		return err
	}
	requireCoded(t, set("", true), codes.FailedPrecondition, "CATEGORY_TEMPLATE_LOCKED", 4004)
	requireCoded(t, set(f.tpl2.String(), false), codes.FailedPrecondition, "CATEGORY_TEMPLATE_LOCKED", 4004)
	if err := set(f.tpl1.String(), false); err != nil {
		t.Fatalf("the locked template is allowed: %v", err)
	}
	if err := set("", false); err != nil {
		t.Fatalf("inheriting resolves to the locked template, so it is allowed: %v", err)
	}
	p := f.policies.policies[uuid.MustParse(pid)]
	if p.TemplateNone {
		t.Fatal("a refused freeform change must not be stored")
	}
	_ = p
}
