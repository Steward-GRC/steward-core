// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"strings"
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

type docTypesEnv struct {
	cats      *fakeCategoryStore
	pols      *fakePolicyStore
	policies  *grpcsvc.PolicyHandler
	category  *grpcsvc.CategoryHandler
	audit     *capturePublisher
	expenses  domain.Category // policies only
	conduct   domain.Category // procedures only
	workplace domain.Category // container
	finance   domain.Category // both
}

func newDocTypesEnv(t *testing.T) docTypesEnv {
	t.Helper()
	cap := &capturePublisher{}
	em := audit.New(cap)
	e := docTypesEnv{cats: newFakeCategoryStore(), pols: newFakePolicyStore(), audit: cap}
	seed := func(name string, types domain.CategoryDocumentTypes) domain.Category {
		g, err := e.cats.Create(context.Background(), domain.Category{Name: name, Slug: strings.ToLower(name), DocumentTypes: types})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	e.expenses = seed("Expenses", domain.CategoryDocumentTypesPolicies)
	e.conduct = seed("Conduct", domain.CategoryDocumentTypesProcedures)
	e.workplace = seed("Workplace", domain.CategoryDocumentTypesNone)
	e.finance = seed("Finance", domain.CategoryDocumentTypesBoth)
	e.policies = grpcsvc.NewPolicyHandler(e.pols, e.cats, newFakeTemplateStore(), domain.NoopValidator{}, em)
	e.category = grpcsvc.NewCategoryHandler(e.cats, em)
	return e
}

func (e docTypesEnv) create(cat domain.Category, dt corev1.DocumentType) (*corev1.CreatePolicyResponse, error) {
	return e.policies.CreatePolicy(context.Background(), &corev1.CreatePolicyRequest{
		HomeCategoryId: cat.ID.String(), Title: "A document", OwnerUserId: uuid.NewString(), DocumentType: dt,
	})
}

func errorInfo(t *testing.T, err error) (codes.Code, string, map[string]string, string) {
	t.Helper()
	st := status.Convert(err)
	info, ok := apperrgrpc.FromStatus(st)
	if !ok {
		t.Fatalf("status %v carries no ErrorInfo (a generic error)", st)
	}
	return st.Code(), info.Symbol, info.Metadata, st.Message()
}

// Mock: "has one category of each kind ... reads through the gateway's category fields".
func TestCategoryProtoCarriesDocumentTypes(t *testing.T) {
	e := newDocTypesEnv(t)
	cases := map[uuid.UUID]corev1.CategoryDocumentTypes{
		e.expenses.ID:  corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_POLICIES,
		e.conduct.ID:   corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_PROCEDURES,
		e.workplace.ID: corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_NONE,
		e.finance.ID:   corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_BOTH,
	}
	for id, want := range cases {
		resp, err := e.category.GetCategory(context.Background(), &corev1.GetCategoryRequest{Id: id.String()})
		if err != nil {
			t.Fatal(err)
		}
		if got := resp.GetCategory().GetDocumentTypes(); got != want {
			t.Errorf("document_types = %v, want %v", got, want)
		}
	}
	// A category stored before the setting existed reads as both.
	old, _ := e.cats.Create(context.Background(), domain.Category{Name: "Old", Slug: "old"})
	resp, err := e.category.GetCategory(context.Background(), &corev1.GetCategoryRequest{Id: old.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.GetCategory().GetDocumentTypes(); got != corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_BOTH {
		t.Errorf("unset reads %v, want BOTH", got)
	}
}

// Mock: "refuses a procedure in a policies-only category and a policy in a procedures-only one".
func TestCreatePolicyRefusedByCategoryKind(t *testing.T) {
	e := newDocTypesEnv(t)

	_, err := e.create(e.expenses, corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE)
	code, symbol, meta, msg := errorInfo(t, err)
	if code != codes.FailedPrecondition || symbol != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("procedure in policies-only: %v %s", code, symbol)
	}
	if meta["category"] != "Expenses" || meta["type"] != "procedure" {
		t.Fatalf("the error must name the category and the type: %v", meta)
	}
	if !strings.Contains(msg, "Expenses") || !strings.Contains(msg, "procedures") {
		t.Fatalf("message %q must name the category and the type", msg)
	}

	_, err = e.create(e.conduct, corev1.DocumentType_DOCUMENT_TYPE_POLICY)
	if _, symbol, _, _ = errorInfo(t, err); symbol != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("policy in procedures-only: %s", symbol)
	}
	// An unset type creates a policy, so a procedures-only category refuses it too.
	_, err = e.create(e.conduct, corev1.DocumentType_DOCUMENT_TYPE_UNSPECIFIED)
	if _, symbol, _, _ = errorInfo(t, err); symbol != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("unset type in procedures-only: %s", symbol)
	}
	if len(e.pols.policies) != 0 {
		t.Fatalf("a refused create stored %d policies", len(e.pols.policies))
	}
}

// Mock: "refuses any document in a container and says to pick a child category".
func TestCreatePolicyRefusedInContainer(t *testing.T) {
	e := newDocTypesEnv(t)
	for _, dt := range []corev1.DocumentType{corev1.DocumentType_DOCUMENT_TYPE_POLICY, corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE} {
		_, err := e.create(e.workplace, dt)
		code, symbol, _, msg := errorInfo(t, err)
		if code != codes.FailedPrecondition || symbol != "CATEGORY_REFUSES_TYPE" {
			t.Fatalf("%v in container: %v %s", dt, code, symbol)
		}
		if !strings.Contains(msg, "child category") {
			t.Fatalf("message %q must say to pick a child category", msg)
		}
	}
}

// Mock: "accepts a document of the kind the category holds".
func TestCreatePolicyAcceptedByCategoryKind(t *testing.T) {
	e := newDocTypesEnv(t)
	if _, err := e.create(e.expenses, corev1.DocumentType_DOCUMENT_TYPE_POLICY); err != nil {
		t.Fatalf("policy in policies-only: %v", err)
	}
	if _, err := e.create(e.conduct, corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE); err != nil {
		t.Fatalf("procedure in procedures-only: %v", err)
	}
	if _, err := e.create(e.finance, corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE); err != nil {
		t.Fatalf("procedure in both: %v", err)
	}
}

// The store re-checks inside its transaction; a refusal it reports after the handler's check (the
// category changed in between) is the same coded error.
func TestCreatePolicyMapsStoreRefusal(t *testing.T) {
	e := newDocTypesEnv(t)
	e.pols.createErr = store.ErrCategoryRefusesType
	_, err := e.create(e.finance, corev1.DocumentType_DOCUMENT_TYPE_POLICY)
	code, symbol, meta, _ := errorInfo(t, err)
	if code != codes.FailedPrecondition || symbol != "CATEGORY_REFUSES_TYPE" || meta["category"] != "Finance" {
		t.Fatalf("got %v %s %v", code, symbol, meta)
	}
}

// Mock: "refuses to move a document into a category that does not hold its type".
func TestMovePolicyRefusedByCategoryKind(t *testing.T) {
	e := newDocTypesEnv(t)
	pid := uuid.New()
	e.pols.policies[pid] = domain.Policy{ID: pid, HomeCategoryID: e.finance.ID, DocumentType: domain.DocumentTypePolicy}

	move := func(dst domain.Category) error {
		_, err := e.policies.MovePolicy(context.Background(), &corev1.MovePolicyRequest{
			PolicyId: pid.String(), HomeCategoryId: dst.ID.String(), ActorUserId: uuid.NewString(),
		})
		return err
	}
	for _, dst := range []domain.Category{e.conduct, e.workplace} {
		code, symbol, meta, _ := errorInfo(t, move(dst))
		if code != codes.FailedPrecondition || symbol != "CATEGORY_REFUSES_TYPE" || meta["category"] != dst.Name {
			t.Fatalf("move into %s: %v %s %v", dst.Name, code, symbol, meta)
		}
	}
	if e.pols.policies[pid].HomeCategoryID != e.finance.ID {
		t.Fatal("a refused move must leave the policy where it was")
	}
	if len(e.audit.calls) != 0 {
		t.Fatalf("a refused move must not audit a move: %+v", e.audit.calls)
	}
	if err := move(e.expenses); err != nil {
		t.Fatalf("policy into policies-only: %v", err)
	}

	e.pols.moveErr = store.ErrCategoryRefusesType
	if _, symbol, _, _ := errorInfo(t, move(e.finance)); symbol != "CATEGORY_REFUSES_TYPE" {
		t.Fatalf("store refusal on move: %s", symbol)
	}
}

func (e docTypesEnv) set(cat domain.Category, to corev1.CategoryDocumentTypes) (*corev1.SetCategoryDocumentTypesResponse, error) {
	return e.category.SetCategoryDocumentTypes(context.Background(), &corev1.SetCategoryDocumentTypesRequest{
		Id: cat.ID.String(), DocumentTypes: to, ActorUserId: uuid.NewString(),
	})
}

// Mock: "counts the policies and procedures a category holds".
func TestCountCategoryDocuments(t *testing.T) {
	e := newDocTypesEnv(t)
	e.cats.docs[e.expenses.ID] = store.DocumentCounts{Policies: 4, Procedures: 1}
	resp, err := e.category.CountCategoryDocuments(context.Background(), &corev1.CountCategoryDocumentsRequest{CategoryId: e.expenses.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if resp.GetPolicies() != 4 || resp.GetProcedures() != 1 {
		t.Fatalf("counts = %d/%d, want 4/1", resp.GetPolicies(), resp.GetProcedures())
	}
	if _, err := e.category.CountCategoryDocuments(context.Background(), &corev1.CountCategoryDocumentsRequest{CategoryId: uuid.NewString()}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown category: %v", err)
	}
	if _, err := e.category.CountCategoryDocuments(context.Background(), &corev1.CountCategoryDocumentsRequest{CategoryId: "nope"}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
}

// Mock: "refuses to narrow while documents would be stranded, with the count".
func TestSetCategoryDocumentTypesRefusesStranding(t *testing.T) {
	e := newDocTypesEnv(t)
	e.cats.docs[e.expenses.ID] = store.DocumentCounts{Policies: 3}

	_, err := e.set(e.expenses, corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_PROCEDURES)
	code, symbol, meta, msg := errorInfo(t, err)
	if code != codes.FailedPrecondition || symbol != "CATEGORY_HOLDS_DOCUMENTS" {
		t.Fatalf("got %v %s", code, symbol)
	}
	if meta["count"] != "3" || meta["policies"] != "3" || meta["procedures"] != "0" || meta["category"] != "Expenses" {
		t.Fatalf("the error must carry the count and which types: %v", meta)
	}
	if !strings.Contains(msg, "3 documents") {
		t.Fatalf("message %q must carry the count", msg)
	}
	if got := e.cats.categories[e.expenses.ID].DocumentTypes; got != domain.CategoryDocumentTypesPolicies {
		t.Fatalf("a refused narrowing changed the setting to %q", got)
	}
	if len(e.audit.calls) != 0 {
		t.Fatalf("a refused narrowing must not audit a change: %+v", e.audit.calls)
	}
}

// Mock: "narrows an empty category and widens any category".
func TestSetCategoryDocumentTypesNarrowEmptyAndWiden(t *testing.T) {
	e := newDocTypesEnv(t)
	resp, err := e.set(e.finance, corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_NONE)
	if err != nil {
		t.Fatalf("narrow an empty category: %v", err)
	}
	if got := resp.GetCategory().GetDocumentTypes(); got != corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_NONE {
		t.Fatalf("returned %v", got)
	}
	if len(e.audit.calls) != 1 || e.audit.calls[0].event.Action != "category.document_types_set" {
		t.Fatalf("audit: %+v", e.audit.calls)
	}

	// Widening is allowed even when the category holds documents.
	e.cats.docs[e.expenses.ID] = store.DocumentCounts{Policies: 3}
	resp, err = e.set(e.expenses, corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_BOTH)
	if err != nil {
		t.Fatalf("widen: %v", err)
	}
	if got := resp.GetCategory().GetDocumentTypes(); got != corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_BOTH {
		t.Fatalf("returned %v", got)
	}
}

func TestSetCategoryDocumentTypesValidates(t *testing.T) {
	e := newDocTypesEnv(t)
	if _, err := e.set(e.finance, corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_UNSPECIFIED); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unspecified: %v", err)
	}
	if _, err := e.category.SetCategoryDocumentTypes(context.Background(), &corev1.SetCategoryDocumentTypesRequest{
		Id: "nope", DocumentTypes: corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_BOTH,
	}); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("bad id: %v", err)
	}
	if _, err := e.category.SetCategoryDocumentTypes(context.Background(), &corev1.SetCategoryDocumentTypesRequest{
		Id: uuid.NewString(), DocumentTypes: corev1.CategoryDocumentTypes_CATEGORY_DOCUMENT_TYPES_BOTH,
	}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown category: %v", err)
	}
}
