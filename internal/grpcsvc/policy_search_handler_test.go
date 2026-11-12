// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// searchStore answers SearchPolicies from canned hits and records the call. A policy it does not
// hold is the real store's no-rows error.
type searchStore struct {
	*fakePolicyStore
	hits  []store.PolicySearchHit
	more  bool
	err   error
	calls int

	query   string
	docType domain.DocumentType
	limit   int
	offset  int
}

func (s *searchStore) SearchPolicies(_ context.Context, query string, docType domain.DocumentType, limit, offset int) ([]store.PolicySearchHit, bool, error) {
	s.calls++
	s.query, s.docType, s.limit, s.offset = query, docType, limit, offset
	return s.hits, s.more, s.err
}

func (s *searchStore) GetPolicy(ctx context.Context, id uuid.UUID) (domain.Policy, error) {
	if _, ok := s.policies[id]; !ok {
		return domain.Policy{}, fmt.Errorf("PolicyStore.GetPolicy: %w", pgx.ErrNoRows)
	}
	return s.fakePolicyStore.GetPolicy(ctx, id)
}

func newSearchHandler(t *testing.T) (*grpcsvc.PolicyHandler, *searchStore) {
	t.Helper()
	ss := &searchStore{fakePolicyStore: newFakePolicyStore()}
	return grpcsvc.NewPolicyHandler(ss, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil), ss
}

func (s *searchStore) add(title string) domain.Policy {
	p := domain.Policy{ID: uuid.New(), Title: title, Number: "POL-IT-000001", DocumentType: domain.DocumentTypePolicy, HomeCategoryID: uuid.New()}
	s.policies[p.ID] = p
	return p
}

func TestSearchPoliciesReturnsTheHitsWithTheirPolicyAndSnippet(t *testing.T) {
	h, ss := newSearchHandler(t)
	a, b := ss.add("Expense Claims Policy"), ss.add("Desk Booking Policy")
	ss.hits = []store.PolicySearchHit{
		{PolicyID: a.ID, Rank: 1.5, TitleMatch: true, TitleRank: 1.5},
		{PolicyID: b.ID, Rank: 0.4, Snippet: &store.PolicySearchSnippet{Text: "attach a photo", Matches: []store.SearchSpan{{Start: 9, Length: 5}}}},
	}
	ss.more = true

	resp, err := h.SearchPolicies(context.Background(), &corev1.SearchPoliciesRequest{Query: "photo", Limit: 2, Offset: 4})
	if err != nil {
		t.Fatalf("SearchPolicies: %v", err)
	}
	if ss.query != "photo" || ss.limit != 2 || ss.offset != 4 || ss.docType != "" {
		t.Fatalf("store called with %q %q %d %d", ss.query, ss.docType, ss.limit, ss.offset)
	}
	if !resp.GetHasMore() || len(resp.GetHits()) != 2 {
		t.Fatalf("has_more = %v hits = %d", resp.GetHasMore(), len(resp.GetHits()))
	}
	first, second := resp.GetHits()[0], resp.GetHits()[1]
	if first.GetPolicy().GetId() != a.ID.String() || first.GetPolicy().GetTitle() != "Expense Claims Policy" ||
		!first.GetTitleMatch() || first.GetTitleRank() != 1.5 || first.GetRank() != 1.5 || first.GetSnippet() != nil {
		t.Fatalf("first hit = %v", first)
	}
	sn := second.GetSnippet()
	if second.GetPolicy().GetId() != b.ID.String() || sn.GetText() != "attach a photo" ||
		len(sn.GetMatches()) != 1 || sn.GetMatches()[0].GetStart() != 9 || sn.GetMatches()[0].GetLength() != 5 {
		t.Fatalf("second hit = %v", second)
	}
}

func TestSearchPoliciesMapsTheDocumentType(t *testing.T) {
	cases := map[corev1.DocumentType]domain.DocumentType{
		corev1.DocumentType_DOCUMENT_TYPE_UNSPECIFIED: "",
		corev1.DocumentType_DOCUMENT_TYPE_POLICY:      domain.DocumentTypePolicy,
		corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE:   domain.DocumentTypeProcedure,
	}
	for in, want := range cases {
		h, ss := newSearchHandler(t)
		if _, err := h.SearchPolicies(context.Background(), &corev1.SearchPoliciesRequest{Query: "x", DocumentType: in}); err != nil {
			t.Fatalf("%v: %v", in, err)
		}
		if ss.docType != want {
			t.Fatalf("%v searched %q, want %q", in, ss.docType, want)
		}
	}
}

func TestSearchPoliciesPageSize(t *testing.T) {
	cases := []struct{ in, want int32 }{{0, 20}, {1, 1}, {100, 100}, {500, 100}}
	for _, c := range cases {
		h, ss := newSearchHandler(t)
		if _, err := h.SearchPolicies(context.Background(), &corev1.SearchPoliciesRequest{Query: "x", Limit: c.in}); err != nil {
			t.Fatalf("limit %d: %v", c.in, err)
		}
		if ss.limit != int(c.want) {
			t.Fatalf("limit %d searched with %d, want %d", c.in, ss.limit, c.want)
		}
	}
}

func TestSearchPoliciesRefusesABadRequest(t *testing.T) {
	cases := map[string]*corev1.SearchPoliciesRequest{
		"negative limit":  {Query: "x", Limit: -1},
		"negative offset": {Query: "x", Offset: -1},
		"long query":      {Query: strings.Repeat("a", 257)},
	}
	for name, req := range cases {
		h, ss := newSearchHandler(t)
		_, err := h.SearchPolicies(context.Background(), req)
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("%s: code = %v (%v), want InvalidArgument", name, status.Code(err), err)
		}
		if ss.calls != 0 {
			t.Fatalf("%s reached the store", name)
		}
	}
}

func TestSearchPoliciesOfABlankQueryFindsNothingWithoutAsking(t *testing.T) {
	h, ss := newSearchHandler(t)
	resp, err := h.SearchPolicies(context.Background(), &corev1.SearchPoliciesRequest{Query: "  \t "})
	if err != nil || len(resp.GetHits()) != 0 || resp.GetHasMore() {
		t.Fatalf("resp = %v err = %v", resp, err)
	}
	if ss.calls != 0 {
		t.Fatal("a blank query reached the store")
	}
}

func TestSearchPoliciesSkipsAPolicyThatVanishedMidSearch(t *testing.T) {
	h, ss := newSearchHandler(t)
	kept := ss.add("Kept")
	ss.hits = []store.PolicySearchHit{{PolicyID: uuid.New(), Rank: 2}, {PolicyID: kept.ID, Rank: 1}}
	resp, err := h.SearchPolicies(context.Background(), &corev1.SearchPoliciesRequest{Query: "x"})
	if err != nil {
		t.Fatalf("SearchPolicies: %v", err)
	}
	if len(resp.GetHits()) != 1 || resp.GetHits()[0].GetPolicy().GetId() != kept.ID.String() {
		t.Fatalf("hits = %v", resp.GetHits())
	}
}

func TestSearchPoliciesCodesAStoreFault(t *testing.T) {
	h, ss := newSearchHandler(t)
	ss.err = fmt.Errorf("connection reset")
	_, err := h.SearchPolicies(context.Background(), &corev1.SearchPoliciesRequest{Query: "x"})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v (%v), want Internal", status.Code(err), err)
	}
	if strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("the cause leaked to the caller: %v", err)
	}
}
