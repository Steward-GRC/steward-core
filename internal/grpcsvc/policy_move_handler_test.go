// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Bugs5382/go-apperr/apperrgrpc"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// The bulk-move methods of the policy fake. Numbers are "<kind>-<first 8 of the category id>-<seq>"
// from a per-category, per-type counter, as the real store does; the real numbering is covered by
// the store tests.
func (f *fakePolicyStore) counter(cat uuid.UUID, dt domain.DocumentType) *int {
	if f.moveSeq == nil {
		f.moveSeq = map[string]*int{}
	}
	k := cat.String() + "/" + string(dt)
	if f.moveSeq[k] == nil {
		n := 0
		f.moveSeq[k] = &n
	}
	return f.moveSeq[k]
}

func fakeNumber(cat uuid.UUID, dt domain.DocumentType, seq int) string {
	return fmt.Sprintf("%s-%s-%06d", dt, cat.String()[:8], seq)
}

func (f *fakePolicyStore) PreviewMoveNumbers(_ context.Context, target uuid.UUID, types []domain.DocumentType) ([]string, error) {
	taken := map[domain.DocumentType]int{}
	out := make([]string, 0, len(types))
	for _, dt := range types {
		taken[dt]++
		out = append(out, fakeNumber(target, dt, *f.counter(target, dt)+taken[dt]))
	}
	return out, nil
}

func (f *fakePolicyStore) MovePolicies(_ context.Context, target uuid.UUID, ids []uuid.UUID) ([]domain.Policy, error) {
	// All or nothing: check everything, then apply.
	for _, id := range ids {
		p, ok := f.policies[id]
		if !ok {
			return nil, pgx.ErrNoRows
		}
		if p.RetiredAt != nil {
			return nil, fmt.Errorf("move %s: %w", id, store.ErrPolicyRetired)
		}
	}
	out := make([]domain.Policy, 0, len(ids))
	for _, id := range ids {
		p := f.policies[id]
		if p.HomeCategoryID != target {
			c := f.counter(target, p.DocumentType)
			*c++
			p.HomeCategoryID = target
			p.Number = fakeNumber(target, p.DocumentType, *c)
			f.policies[id] = p
		}
		out = append(out, p)
	}
	return out, nil
}

// moveCategories answers an unknown id the way the real store does.
type moveCategories struct{ *fakeCategoryStore }

func (m moveCategories) Get(ctx context.Context, id uuid.UUID) (domain.Category, error) {
	c, ok := m.categories[id]
	if !ok {
		return domain.Category{}, pgx.ErrNoRows
	}
	return c, nil
}

// movePolicies answers an unknown id the way the real store does.
type movePolicies struct{ *fakePolicyStore }

func (m movePolicies) GetPolicy(_ context.Context, id uuid.UUID) (domain.Policy, error) {
	p, ok := m.policies[id]
	if !ok {
		return domain.Policy{}, pgx.ErrNoRows
	}
	return p, nil
}

type moveFixture struct {
	h        *grpcsvc.PolicyHandler
	ps       *fakePolicyStore
	gs       *fakeCategoryStore
	cap      *capturePublisher
	src, dst uuid.UUID
	docs     []uuid.UUID // two policies and a procedure in src
}

func newMoveFixture(t *testing.T) moveFixture {
	t.Helper()
	cap := &capturePublisher{}
	ps := newFakePolicyStore()
	gs := newFakeCategoryStore()
	f := moveFixture{ps: ps, cap: cap, src: uuid.New(), dst: uuid.New()}
	gs.categories[f.src] = domain.Category{ID: f.src, Name: "Expenses", Slug: "expenses"}
	gs.categories[f.dst] = domain.Category{ID: f.dst, Name: "Finance", Slug: "finance"}
	for i, dt := range []domain.DocumentType{domain.DocumentTypePolicy, domain.DocumentTypePolicy, domain.DocumentTypeProcedure} {
		id := uuid.New()
		n := *ps.counter(f.src, dt) + 1
		*ps.counter(f.src, dt) = n
		ps.policies[id] = domain.Policy{ID: id, HomeCategoryID: f.src, DocumentType: dt, Title: fmt.Sprintf("Doc %d", i), Number: fakeNumber(f.src, dt, n)}
		f.docs = append(f.docs, id)
	}
	f.gs = gs
	f.h = grpcsvc.NewPolicyHandler(movePolicies{ps}, moveCategories{gs}, newFakeTemplateStore(), domain.NoopValidator{}, audit.New(cap))
	return f
}

// setTargetTypes sets the accepted document types of the target category.
func (f moveFixture) setTargetTypes(t domain.CategoryDocumentTypes) {
	g := f.gs.categories[f.dst]
	g.DocumentTypes = t
	f.gs.categories[f.dst] = g
}

func (f moveFixture) idStrings() []string {
	out := make([]string, 0, len(f.docs))
	for _, id := range f.docs {
		out = append(out, id.String())
	}
	return out
}

func (f moveFixture) unchanged(t *testing.T) {
	t.Helper()
	for _, id := range f.docs {
		if f.ps.policies[id].HomeCategoryID != f.src {
			t.Fatalf("document %s left its category", id)
		}
	}
}

// Mock: "previewMovePolicies says what each document would get and changes nothing".
func TestPreviewMovePoliciesSaysWhatEachGetsAndChangesNothing(t *testing.T) {
	f := newMoveFixture(t)
	resp, err := f.h.PreviewMovePolicies(context.Background(), &corev1.PreviewMovePoliciesRequest{
		PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String(), ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("PreviewMovePolicies: %v", err)
	}
	items := resp.GetItems()
	if len(items) != len(f.docs) {
		t.Fatalf("got %d items, want %d", len(items), len(f.docs))
	}
	want := []string{fakeNumber(f.dst, "policy", 1), fakeNumber(f.dst, "policy", 2), fakeNumber(f.dst, "procedure", 1)}
	for i, it := range items {
		if it.GetNewNumber() != want[i] || it.GetRefusal() != "" || it.GetRefusalCode() != "" {
			t.Fatalf("item %d: %+v, want new number %q", i, it, want[i])
		}
		if it.GetPolicyId() != f.docs[i].String() || it.GetNumber() != f.ps.policies[f.docs[i]].Number {
			t.Fatalf("item %d does not describe its document: %+v", i, it)
		}
	}
	f.unchanged(t)
	if len(f.cap.calls) != 0 {
		t.Fatalf("a preview must not audit: %+v", f.cap.calls)
	}
}

// Mock: "gives a refusal, not a number, to a document the target does not hold".
func TestPreviewMovePoliciesRefusesATypeTheTargetDoesNotHold(t *testing.T) {
	f := newMoveFixture(t)
	f.h.WithMoveTypeGate(func(_ domain.Category, dt domain.DocumentType) bool { return dt != domain.DocumentTypePolicy })
	resp, err := f.h.PreviewMovePolicies(context.Background(), &corev1.PreviewMovePoliciesRequest{
		PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String(),
	})
	if err != nil {
		t.Fatalf("PreviewMovePolicies: %v", err)
	}
	first, last := resp.GetItems()[0], resp.GetItems()[2]
	if first.GetNewNumber() != "" || first.GetRefusalCode() != "CATEGORY_REFUSES_TYPE" || !strings.Contains(first.GetRefusal(), "CATEGORY_REFUSES_TYPE") {
		t.Fatalf("policy must be refused: %+v", first)
	}
	if last.GetRefusal() != "" || last.GetNewNumber() == "" {
		t.Fatalf("the procedure the target holds must get a number: %+v", last)
	}
}

func TestPreviewMovePoliciesRefusesARetiredDocumentAndKeepsOnesAlreadyThere(t *testing.T) {
	f := newMoveFixture(t)
	now := time.Now()
	p := f.ps.policies[f.docs[0]]
	p.RetiredAt = &now
	f.ps.policies[f.docs[0]] = p
	here := f.ps.policies[f.docs[1]]
	here.HomeCategoryID = f.dst
	f.ps.policies[f.docs[1]] = here

	resp, err := f.h.PreviewMovePolicies(context.Background(), &corev1.PreviewMovePoliciesRequest{
		PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String(),
	})
	if err != nil {
		t.Fatalf("PreviewMovePolicies: %v", err)
	}
	items := resp.GetItems()
	if items[0].GetRefusalCode() != "RETIRED" || items[0].GetNewNumber() != "" {
		t.Fatalf("retired: %+v", items[0])
	}
	if items[1].GetNewNumber() != items[1].GetNumber() || items[1].GetRefusal() != "" {
		t.Fatalf("a document already there keeps its number: %+v", items[1])
	}
	// The retired and the stay-put documents take no number from the sequence.
	if items[2].GetNewNumber() != fakeNumber(f.dst, "procedure", 1) {
		t.Fatalf("procedure number: %+v", items[2])
	}
}

func TestPreviewMovePoliciesNotFound(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	_, err := f.h.PreviewMovePolicies(ctx, &corev1.PreviewMovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: uuid.New().String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown target: want NotFound, got %v", err)
	}
	_, err = f.h.PreviewMovePolicies(ctx, &corev1.PreviewMovePoliciesRequest{PolicyIds: []string{uuid.New().String()}, HomeCategoryId: f.dst.String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown policy: want NotFound, got %v", err)
	}
	_, err = f.h.PreviewMovePolicies(ctx, &corev1.PreviewMovePoliciesRequest{PolicyIds: []string{"nope"}, HomeCategoryId: f.dst.String()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: want InvalidArgument, got %v", err)
	}
}

// Mock: "movePolicies moves them all, renumbering in the target's sequence, with an audit entry
// each and one for the batch".
func TestMovePoliciesMovesAllAuditsEachAndTheBatch(t *testing.T) {
	f := newMoveFixture(t)
	actor := uuid.New().String()
	resp, err := f.h.MovePolicies(context.Background(), &corev1.MovePoliciesRequest{
		PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String(), ActorUserId: actor,
	})
	if err != nil {
		t.Fatalf("MovePolicies: %v", err)
	}
	moved := resp.GetMoved()
	if len(moved) != len(f.docs) {
		t.Fatalf("moved %d, want %d", len(moved), len(f.docs))
	}
	seen := map[string]bool{}
	for i, m := range moved {
		seen[m.GetNewNumber()] = true
		if m.GetNewNumber() == "" || m.GetNewNumber() == m.GetNumber() || m.GetRefusal() != "" {
			t.Fatalf("moved[%d] = %+v", i, m)
		}
		if f.ps.policies[f.docs[i]].HomeCategoryID != f.dst || f.ps.policies[f.docs[i]].Number != m.GetNewNumber() {
			t.Fatalf("document %d not moved to the number it was told", i)
		}
	}
	if len(seen) != len(f.docs) {
		t.Fatalf("numbers are not distinct: %v", seen)
	}
	var each int
	for i, c := range f.cap.calls {
		if c.event.Action == "policy.moved" {
			each++
			if c.event.ActorUserID != actor || c.event.Attributes["from_group"] != f.src.String() || c.event.Attributes["to_group"] != f.dst.String() {
				t.Fatalf("policy.moved attributes: %+v", c.event)
			}
		}
		if c.event.Action == "policy.bulk_moved" && i != len(f.cap.calls)-1 {
			t.Fatalf("the batch entry must come last")
		}
	}
	last := f.cap.calls[len(f.cap.calls)-1].event
	if each != len(f.docs) || last.Action != "policy.bulk_moved" {
		t.Fatalf("audit: %d policy.moved, last %q", each, last.Action)
	}
	if last.ActorUserID != actor || last.Subject != "category:"+f.dst.String() || last.GroupID != f.dst.String() ||
		last.Attributes["count"] != "3" || last.Attributes["to_group"] != f.dst.String() || last.Attributes["from_groups"] != f.src.String() {
		t.Fatalf("policy.bulk_moved: %+v", last)
	}
}

// A document already in the target is not renumbered and has no policy.moved entry; the batch
// entry counts only what moved.
func TestMovePoliciesLeavesADocumentAlreadyThere(t *testing.T) {
	f := newMoveFixture(t)
	here := f.ps.policies[f.docs[0]]
	here.HomeCategoryID = f.dst
	f.ps.policies[f.docs[0]] = here
	before := here.Number

	resp, err := f.h.MovePolicies(context.Background(), &corev1.MovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String()})
	if err != nil {
		t.Fatalf("MovePolicies: %v", err)
	}
	if resp.GetMoved()[0].GetNewNumber() != before {
		t.Fatalf("renumbered a document already there: %+v", resp.GetMoved()[0])
	}
	var each int
	for _, c := range f.cap.calls {
		if c.event.Action == "policy.moved" {
			each++
		}
	}
	if each != 2 || f.cap.calls[len(f.cap.calls)-1].event.Attributes["count"] != "2" {
		t.Fatalf("audit: %d policy.moved, %+v", each, f.cap.calls)
	}
}

// Mock: "refuses the whole batch when one document would be refused, and moves nothing".
func TestMovePoliciesRefusesTheWholeBatchAndMovesNothing(t *testing.T) {
	f := newMoveFixture(t)
	f.h.WithMoveTypeGate(func(_ domain.Category, dt domain.DocumentType) bool { return dt != domain.DocumentTypeProcedure })
	_, err := f.h.MovePolicies(context.Background(), &corev1.MovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String()})
	st := status.Convert(err)
	if st.Code() != codes.FailedPrecondition || !strings.Contains(st.Message(), "CATEGORY_REFUSES_TYPE") {
		t.Fatalf("want FailedPrecondition naming CATEGORY_REFUSES_TYPE, got %v", err)
	}
	info, ok := apperrgrpc.FromStatus(st)
	if !ok || info.Symbol != "MOVE_REFUSED" || info.Metadata["reason"] != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("want a coded MOVE_REFUSED carrying the reason, got %+v ok=%v", info, ok)
	}
	f.unchanged(t)
	if len(f.cap.calls) != 0 {
		t.Fatalf("a refused batch must not audit: %+v", f.cap.calls)
	}
}

func TestMovePoliciesRefusesARetiredDocumentRaceFromTheStore(t *testing.T) {
	f := newMoveFixture(t)
	now := time.Now()
	p := f.ps.policies[f.docs[2]]
	p.RetiredAt = &now
	f.ps.policies[f.docs[2]] = p
	_, err := f.h.MovePolicies(context.Background(), &corev1.MovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String()})
	if status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "RETIRED") {
		t.Fatalf("want FailedPrecondition RETIRED, got %v", err)
	}
	f.unchanged(t)
}

// Mock: "refuses an empty batch and an unknown target".
func TestMovePoliciesRefusesAnEmptyBatchAndAnUnknownTarget(t *testing.T) {
	f := newMoveFixture(t)
	ctx := context.Background()
	_, err := f.h.MovePolicies(ctx, &corev1.MovePoliciesRequest{HomeCategoryId: f.dst.String()})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("empty: want InvalidArgument, got %v", err)
	}
	_, err = f.h.MovePolicies(ctx, &corev1.MovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: uuid.New().String()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("unknown target: want NotFound, got %v", err)
	}
	_, err = f.h.MovePolicies(ctx, &corev1.MovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: "nowhere"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad target id: want InvalidArgument, got %v", err)
	}
	f.unchanged(t)
}

// Mock: "is for a site admin only" is the gateway's check, as it is for MovePolicy; core admits
// the gateway on every PolicyService method.
func TestCallerPolicyAdmitsTheGatewayOnTheBulkMoveMethods(t *testing.T) {
	p := grpcsvc.CallerPolicy()
	for _, m := range []string{corev1.PolicyService_PreviewMovePolicies_FullMethodName, corev1.PolicyService_MovePolicies_FullMethodName} {
		if _, ok := p[m][grpcsvc.CallerGateway]; !ok {
			t.Fatalf("%s: the gateway is not admitted", m)
		}
		if len(p[m]) != 1 {
			t.Fatalf("%s: only the gateway may call it, got %v", m, p[m])
		}
	}
}

// The move asks the target category's own accepted document types, with no gate set by hand.
func TestMovePoliciesUseTheTargetCategoryAcceptedTypes(t *testing.T) {
	f := newMoveFixture(t)
	f.setTargetTypes(domain.CategoryDocumentTypesProcedures)

	resp, err := f.h.PreviewMovePolicies(context.Background(), &corev1.PreviewMovePoliciesRequest{
		PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String(),
	})
	if err != nil {
		t.Fatalf("PreviewMovePolicies: %v", err)
	}
	first, last := resp.GetItems()[0], resp.GetItems()[2]
	if first.GetNewNumber() != "" || first.GetRefusalCode() != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("a policy must be refused by a procedures-only category: %+v", first)
	}
	if last.GetRefusal() != "" || last.GetNewNumber() == "" {
		t.Fatalf("the procedure must get a number: %+v", last)
	}

	_, err = f.h.MovePolicies(context.Background(), &corev1.MovePoliciesRequest{PolicyIds: f.idStrings(), HomeCategoryId: f.dst.String()})
	info, ok := apperrgrpc.FromStatus(status.Convert(err))
	if !ok || info.Symbol != "MOVE_REFUSED" || info.Metadata["reason"] != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("want a coded MOVE_REFUSED carrying the reason, got %+v ok=%v err=%v", info, ok, err)
	}
	f.unchanged(t)
}

func TestMovePoliciesIntoAContainerCategoryAreRefused(t *testing.T) {
	f := newMoveFixture(t)
	f.setTargetTypes(domain.CategoryDocumentTypesNone)
	_, err := f.h.MovePolicies(context.Background(), &corev1.MovePoliciesRequest{PolicyIds: f.idStrings()[:1], HomeCategoryId: f.dst.String()})
	if status.Convert(err).Code() != codes.FailedPrecondition {
		t.Fatalf("want FailedPrecondition, got %v", err)
	}
	f.unchanged(t)
}
