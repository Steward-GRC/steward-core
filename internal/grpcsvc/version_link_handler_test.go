// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// Handler tests for VersionLinkService (issue #70). The store has its own
// tests; these check the wire shape and the mapping of its errors to codes.

type fakeVersionLinkBackend struct {
	links domain.VersionLinks
	err   error
	// last call, by kind
	refIDs, contactIDs, defIDs []uuid.UUID
	related                    []domain.RelatedLinkInput
	calledWith                 uuid.UUID
}

func (f *fakeVersionLinkBackend) GetVersionLinks(_ context.Context, id uuid.UUID) (domain.VersionLinks, error) {
	f.calledWith = id
	return f.links, f.err
}

func (f *fakeVersionLinkBackend) SetReferences(_ context.Context, id uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error) {
	f.calledWith, f.refIDs = id, ids
	return f.links, f.err
}

func (f *fakeVersionLinkBackend) SetContactBlocks(_ context.Context, id uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error) {
	f.calledWith, f.contactIDs = id, ids
	return f.links, f.err
}

func (f *fakeVersionLinkBackend) SetDefinitions(_ context.Context, id uuid.UUID, ids []uuid.UUID) (domain.VersionLinks, error) {
	f.calledWith, f.defIDs = id, ids
	return f.links, f.err
}

func (f *fakeVersionLinkBackend) SetRelated(_ context.Context, id uuid.UUID, l []domain.RelatedLinkInput) (domain.VersionLinks, error) {
	f.calledWith, f.related = id, l
	return f.links, f.err
}

func TestListRelationshipTypes_EachTypeWithItsInverse(t *testing.T) {
	h := grpcsvc.NewVersionLinkHandler(&fakeVersionLinkBackend{}, nil)
	resp, err := h.ListRelationshipTypes(context.Background(), &corev1.ListRelationshipTypesRequest{})
	if err != nil {
		t.Fatalf("ListRelationshipTypes: %v", err)
	}
	types := resp.GetTypes()
	if len(types) != 10 {
		t.Fatalf("types = %d, want 10 (five pairs)", len(types))
	}
	byID := map[string]*corev1.RelationshipType{}
	for _, ty := range types {
		byID[ty.GetId()] = ty
	}
	for _, ty := range types {
		inv, ok := byID[ty.GetInverseId()]
		if !ok {
			t.Fatalf("%s: inverse %q is not in the list", ty.GetId(), ty.GetInverseId())
		}
		if inv.GetInverseId() != ty.GetId() {
			t.Fatalf("%s: inverse of the inverse is %q", ty.GetId(), inv.GetInverseId())
		}
		if ty.GetInverseLabel() != inv.GetLabel() || ty.GetLabel() == "" {
			t.Fatalf("%s: labels do not line up: %+v / %+v", ty.GetId(), ty, inv)
		}
	}
	if byID["RELATED_TO"].GetLabel() != "is related to" || byID["RELATED_TO"].GetInverseId() != "RELATED_BY" {
		t.Fatalf("RELATED_TO = %+v", byID["RELATED_TO"])
	}
}

func TestGetVersionLinks_ShapesTheReadModel(t *testing.T) {
	vid, pid := uuid.New(), uuid.New()
	refID, conID, defID := uuid.New(), uuid.New(), uuid.New()
	be := &fakeVersionLinkBackend{links: domain.VersionLinks{
		PolicyVersionID: vid,
		Editable:        true,
		Definitions:     []domain.DefinitionEntry{{ID: defID, CategoryID: uuid.New(), Term: "Asset", Definition: "A thing.", UsedByCount: 2}},
		Related: []domain.RelatedLink{
			{PolicyID: pid, Number: "POL-1", Title: "Other", Type: "SUPERSEDES", Label: "supersedes", Direction: domain.RelatedOutgoing},
			{PolicyID: uuid.New(), Number: "POL-2", Title: "Third", Type: "REQUIRED_BY", Label: "is required by", Direction: domain.RelatedIncoming},
		},
		References: []domain.Reference{{ID: refID, Label: "ISO", Kind: domain.ReferenceKindStandard}},
		Contacts:   []domain.ContactBlock{{ID: conID, Label: "Help desk", Name: "Help"}},
	}}
	h := grpcsvc.NewVersionLinkHandler(be, nil)
	resp, err := h.GetVersionLinks(context.Background(), &corev1.GetVersionLinksRequest{PolicyVersionId: vid.String()})
	if err != nil {
		t.Fatalf("GetVersionLinks: %v", err)
	}
	got := resp.GetLinks()
	if be.calledWith != vid || got.GetPolicyVersionId() != vid.String() || !got.GetEditable() {
		t.Fatalf("links = %+v (store asked for %s)", got, be.calledWith)
	}
	if len(got.GetDefinitions()) != 1 || got.GetDefinitions()[0].GetId() != defID.String() || got.GetDefinitions()[0].GetUsedByCount() != 2 {
		t.Fatalf("definitions = %+v", got.GetDefinitions())
	}
	if len(got.GetReferences()) != 1 || got.GetReferences()[0].GetId() != refID.String() {
		t.Fatalf("references = %+v", got.GetReferences())
	}
	if len(got.GetContacts()) != 1 || got.GetContacts()[0].GetId() != conID.String() {
		t.Fatalf("contacts = %+v", got.GetContacts())
	}
	rel := got.GetRelated()
	if len(rel) != 2 {
		t.Fatalf("related = %+v", rel)
	}
	if rel[0].GetPolicyId() != pid.String() || rel[0].GetNumber() != "POL-1" || rel[0].GetTitle() != "Other" ||
		rel[0].GetRelationType() != "SUPERSEDES" || rel[0].GetLabel() != "supersedes" ||
		rel[0].GetDirection() != corev1.RelatedDirection_RELATED_DIRECTION_OUTGOING {
		t.Fatalf("outgoing = %+v", rel[0])
	}
	if rel[1].GetDirection() != corev1.RelatedDirection_RELATED_DIRECTION_INCOMING || rel[1].GetRelationType() != "REQUIRED_BY" {
		t.Fatalf("incoming = %+v", rel[1])
	}
}

func TestVersionLinkSetters_PassTheIDsInOrderAndAudit(t *testing.T) {
	vid := uuid.New()
	a, b := uuid.New(), uuid.New()
	other := uuid.New()
	actor := uuid.New().String()
	cap := &capturePublisher{}
	be := &fakeVersionLinkBackend{links: domain.VersionLinks{PolicyVersionID: vid, Editable: true}}
	h := grpcsvc.NewVersionLinkHandler(be, audit.New(cap))
	ctx := context.Background()

	if _, err := h.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{
		PolicyVersionId: vid.String(), ReferenceIds: []string{b.String(), a.String()}, ActorUserId: actor,
	}); err != nil {
		t.Fatalf("SetVersionReferences: %v", err)
	}
	if be.calledWith != vid || len(be.refIDs) != 2 || be.refIDs[0] != b || be.refIDs[1] != a {
		t.Fatalf("references reached the store as %v for %s, want [b a] for %s", be.refIDs, be.calledWith, vid)
	}
	if _, err := h.SetVersionContactBlocks(ctx, &corev1.SetVersionContactBlocksRequest{
		PolicyVersionId: vid.String(), ContactBlockIds: []string{a.String()}, ActorUserId: actor,
	}); err != nil || len(be.contactIDs) != 1 || be.contactIDs[0] != a {
		t.Fatalf("SetVersionContactBlocks: %v, ids %v", err, be.contactIDs)
	}
	if _, err := h.SetVersionDefinitions(ctx, &corev1.SetVersionDefinitionsRequest{
		PolicyVersionId: vid.String(), DefinitionIds: []string{a.String(), b.String()}, ActorUserId: actor,
	}); err != nil || len(be.defIDs) != 2 {
		t.Fatalf("SetVersionDefinitions: %v, ids %v", err, be.defIDs)
	}
	resp, err := h.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{
		PolicyVersionId: vid.String(), ActorUserId: actor,
		Links: []*corev1.RelatedLinkInput{{PolicyId: other.String(), RelationType: "DEPENDS_ON"}},
	})
	if err != nil {
		t.Fatalf("SetVersionRelated: %v", err)
	}
	if len(be.related) != 1 || be.related[0].PolicyID != other || be.related[0].Type != "DEPENDS_ON" {
		t.Fatalf("related reached the store as %+v", be.related)
	}
	if resp.GetLinks().GetPolicyVersionId() != vid.String() {
		t.Fatalf("response = %+v", resp)
	}
	want := []string{"policy.version_references_changed", "policy.version_contacts_changed", "policy.version_definitions_changed", "policy.version_related_changed"}
	if len(cap.calls) != len(want) {
		t.Fatalf("audit events = %d, want %d", len(cap.calls), len(want))
	}
	for i, w := range want {
		ev := cap.calls[i].event
		if ev.Action != w || ev.Subject != "policy_version:"+vid.String() || ev.ActorUserID != actor {
			t.Fatalf("audit %d = %+v, want %s", i, ev, w)
		}
	}
}

// An empty list is a valid full replace: it clears the links of that kind.
func TestVersionLinkSetters_EmptyListClears(t *testing.T) {
	vid := uuid.New()
	be := &fakeVersionLinkBackend{links: domain.VersionLinks{PolicyVersionID: vid}}
	h := grpcsvc.NewVersionLinkHandler(be, nil)
	if _, err := h.SetVersionReferences(context.Background(), &corev1.SetVersionReferencesRequest{PolicyVersionId: vid.String()}); err != nil {
		t.Fatalf("SetVersionReferences(empty): %v", err)
	}
	if be.calledWith != vid || len(be.refIDs) != 0 {
		t.Fatalf("store not called with an empty list: %v", be.refIDs)
	}
}

func TestVersionLinkHandler_MapsStoreErrorsToCodes(t *testing.T) {
	vid := uuid.New()
	cases := []struct {
		name string
		err  error
		want codes.Code
	}{
		{"version not found", store.ErrVersionNotFound, codes.NotFound},
		{"item not in the library", store.ErrLinkItemNotFound, codes.NotFound},
		{"related policy not found", store.ErrRelatedPolicyNotFound, codes.NotFound},
		{"version not editable", store.ErrVersionNotEditable, codes.FailedPrecondition},
		{"repeated id", domain.ErrRepeatedLink, codes.InvalidArgument},
		{"unknown relationship type", domain.ErrUnknownRelationshipType, codes.InvalidArgument},
		{"relates to itself", domain.ErrRelatedToItself, codes.InvalidArgument},
		{"wrapped store error", errors.Join(errors.New("context"), store.ErrVersionNotEditable), codes.FailedPrecondition},
		{"anything else", errors.New("connection reset"), codes.Internal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			be := &fakeVersionLinkBackend{err: tc.err}
			h := grpcsvc.NewVersionLinkHandler(be, nil)
			ctx := context.Background()
			id := vid.String()
			calls := map[string]func() error{
				"get": func() error {
					_, err := h.GetVersionLinks(ctx, &corev1.GetVersionLinksRequest{PolicyVersionId: id})
					return err
				},
				"references": func() error {
					_, err := h.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: id})
					return err
				},
				"contacts": func() error {
					_, err := h.SetVersionContactBlocks(ctx, &corev1.SetVersionContactBlocksRequest{PolicyVersionId: id})
					return err
				},
				"definitions": func() error {
					_, err := h.SetVersionDefinitions(ctx, &corev1.SetVersionDefinitionsRequest{PolicyVersionId: id})
					return err
				},
				"related": func() error {
					_, err := h.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: id})
					return err
				},
			}
			for name, call := range calls {
				if got := status.Code(call()); got != tc.want {
					t.Errorf("%s: code = %v, want %v", name, got, tc.want)
				}
			}
		})
	}
}

func TestVersionLinkHandler_RejectsMalformedIDs(t *testing.T) {
	h := grpcsvc.NewVersionLinkHandler(&fakeVersionLinkBackend{}, nil)
	ctx := context.Background()
	good := uuid.New().String()
	calls := map[string]func() error{
		"get bad version": func() error {
			_, err := h.GetVersionLinks(ctx, &corev1.GetVersionLinksRequest{PolicyVersionId: "nope"})
			return err
		},
		"references bad version": func() error {
			_, err := h.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: ""})
			return err
		},
		"references bad item": func() error {
			_, err := h.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: good, ReferenceIds: []string{"x"}})
			return err
		},
		"contacts bad item": func() error {
			_, err := h.SetVersionContactBlocks(ctx, &corev1.SetVersionContactBlocksRequest{PolicyVersionId: good, ContactBlockIds: []string{"x"}})
			return err
		},
		"definitions bad item": func() error {
			_, err := h.SetVersionDefinitions(ctx, &corev1.SetVersionDefinitionsRequest{PolicyVersionId: good, DefinitionIds: []string{"x"}})
			return err
		},
		"related bad policy": func() error {
			_, err := h.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: good,
				Links: []*corev1.RelatedLinkInput{{PolicyId: "x", RelationType: "RELATED_TO"}}})
			return err
		},
	}
	for name, call := range calls {
		if got := status.Code(call()); got != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", name, got)
		}
	}
}

// A failed set is not audited.
func TestVersionLinkSetters_NoAuditOnFailure(t *testing.T) {
	cap := &capturePublisher{}
	h := grpcsvc.NewVersionLinkHandler(&fakeVersionLinkBackend{err: store.ErrVersionNotEditable}, audit.New(cap))
	_, err := h.SetVersionReferences(context.Background(), &corev1.SetVersionReferencesRequest{PolicyVersionId: uuid.New().String()})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("code = %v", status.Code(err))
	}
	if len(cap.calls) != 0 {
		t.Fatalf("audited a refused change: %+v", cap.calls)
	}
}

// SaveDraft copies the links forward the same way it copies appendices: from
// the version the store names as the source, once, when a draft is created.
func TestSaveDraft_CopiesLinksFromTheAppendixSource(t *testing.T) {
	cases := []struct {
		name     string
		source   uuid.UUID
		wantCall bool
	}{
		{"a rejected version", uuid.New(), true},
		{"no source", uuid.Nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakePS := newFakePolicyStore()
			links := &fakeAppendixCopier{}
			p, err := domain.NewPolicy("Linked Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
			if err != nil {
				t.Fatalf("NewPolicy: %v", err)
			}
			fakePS.policies[p.ID] = p
			fakePS.appendixSource = map[uuid.UUID]uuid.UUID{p.ID: tc.source}
			h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil).
				WithLinkCopier(links)
			req := &corev1.SaveDraftRequest{PolicyId: p.ID.String(), ContentJson: `{"sections":{}}`, ActorUserId: uuid.New().String()}
			resp, err := h.SaveDraft(context.Background(), req)
			if err != nil {
				t.Fatalf("SaveDraft: %v", err)
			}
			if !tc.wantCall {
				if len(links.calls) != 0 {
					t.Fatalf("link copy calls = %v, want none", links.calls)
				}
				return
			}
			if len(links.calls) != 1 || links.calls[0][0] != tc.source.String() || links.calls[0][1] != resp.GetVersion().GetId() {
				t.Fatalf("link copy calls = %v, want one from %s to %s", links.calls, tc.source, resp.GetVersion().GetId())
			}
			// The autosave that follows updates the draft: no second copy.
			if _, err := h.SaveDraft(context.Background(), req); err != nil {
				t.Fatalf("second SaveDraft: %v", err)
			}
			if len(links.calls) != 1 {
				t.Fatalf("autosave copied the links again: %v", links.calls)
			}
		})
	}
}

// A failed copy fails the save, as it does for appendices, so a draft is never
// left without the links it should have started with.
func TestSaveDraft_LinkCopyFailureFailsTheSave(t *testing.T) {
	fakePS := newFakePolicyStore()
	p, _ := domain.NewPolicy("Linked Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	fakePS.policies[p.ID] = p
	fakePS.appendixSource = map[uuid.UUID]uuid.UUID{p.ID: uuid.New()}
	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil).
		WithLinkCopier(failingCopier{})
	_, err := h.SaveDraft(context.Background(), &corev1.SaveDraftRequest{
		PolicyId: p.ID.String(), ContentJson: `{"sections":{}}`, ActorUserId: uuid.New().String(),
	})
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

type failingCopier struct{}

func (failingCopier) CopyForward(context.Context, uuid.UUID, uuid.UUID) error {
	return errors.New("copy failed")
}
