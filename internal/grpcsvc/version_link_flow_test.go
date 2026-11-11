// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
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

// End to end against real Postgres: the handlers over the real stores, to show
// that a new draft really starts with the links of the version it follows and
// that the store's refusals reach the wire as the right codes (issue #70).

type linkFlowEnv struct {
	ctx  context.Context
	ps   *store.PolicyStore
	refs *store.ReferenceStore
	ph   *grpcsvc.PolicyHandler
	vh   *grpcsvc.VersionLinkHandler
	gid  uuid.UUID
}

func setupLinkFlow(t *testing.T) linkFlowEnv {
	t.Helper()
	ctx := context.Background()
	pool := renameTestDB(t)
	gs := store.NewCategoryStore(pool)
	ps := store.NewPolicyStore(pool)
	vl := store.NewVersionLinkStore(pool)
	g, err := domain.NewCategory("IT", "it-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	if g, err = gs.Create(ctx, g); err != nil {
		t.Fatalf("Create category: %v", err)
	}
	em := audit.New(&capturePublisher{})
	return linkFlowEnv{
		ctx: ctx, ps: ps, refs: store.NewReferenceStore(pool), gid: g.ID,
		ph: grpcsvc.NewPolicyHandler(ps, gs, store.NewTemplateStore(pool), domain.NoopValidator{}, em).
			WithAppendixCopier(store.NewAppendixStore(pool)).WithLinkCopier(vl),
		vh: grpcsvc.NewVersionLinkHandler(vl, em),
	}
}

func (e linkFlowEnv) publishedPolicyWithReference(t *testing.T) (domain.Policy, string) {
	t.Helper()
	p, _ := domain.NewPolicy("Linked "+uuid.New().String()[:6], e.gid, domain.SensitivityStandard, uuid.New())
	p, err := e.ps.CreatePolicy(e.ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	ref, err := e.refs.CreateReference(e.ctx, domain.Reference{Label: "ISO " + uuid.New().String()[:6], Kind: domain.ReferenceKindStandard})
	if err != nil {
		t.Fatalf("CreateReference: %v", err)
	}
	if _, err := e.vh.SetVersionReferences(e.ctx, &corev1.SetVersionReferencesRequest{
		PolicyVersionId: p.CurrentDraftVersionID.String(), ReferenceIds: []string{ref.ID.String()},
	}); err != nil {
		t.Fatalf("SetVersionReferences on the seeded draft: %v", err)
	}
	if _, err := e.ps.PublishDraft(e.ctx, p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	p, err = e.ps.GetPolicy(e.ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	return p, ref.ID.String()
}

func (e linkFlowEnv) refIDs(t *testing.T, versionID string) []string {
	t.Helper()
	resp, err := e.vh.GetVersionLinks(e.ctx, &corev1.GetVersionLinksRequest{PolicyVersionId: versionID})
	if err != nil {
		t.Fatalf("GetVersionLinks: %v", err)
	}
	out := []string{}
	for _, r := range resp.GetLinks().GetReferences() {
		out = append(out, r.GetId())
	}
	return out
}

func TestSaveDraft_NewDraftStartsWithThePublishedLinks(t *testing.T) {
	e := setupLinkFlow(t)
	p, refID := e.publishedPolicyWithReference(t)

	resp, err := e.ph.SaveDraft(e.ctx, &corev1.SaveDraftRequest{
		PolicyId: p.ID.String(), ContentJson: `{"sections":{}}`, ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	draftID := resp.GetVersion().GetId()
	if got := e.refIDs(t, draftID); len(got) != 1 || got[0] != refID {
		t.Fatalf("new draft references = %v, want [%s]", got, refID)
	}

	// Copied, not shared: emptying the draft leaves the published version alone.
	if _, err := e.vh.SetVersionReferences(e.ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: draftID}); err != nil {
		t.Fatalf("SetVersionReferences(draft): %v", err)
	}
	if got := e.refIDs(t, p.CurrentPublishedVersionID.String()); len(got) != 1 {
		t.Fatalf("published references = %v, want the one it was approved with", got)
	}
	if got := e.refIDs(t, draftID); len(got) != 0 {
		t.Fatalf("draft references = %v, want none", got)
	}
}

func TestRenamePolicy_StagedDraftStartsWithThePublishedLinks(t *testing.T) {
	e := setupLinkFlow(t)
	p, refID := e.publishedPolicyWithReference(t)
	resp, err := e.ph.RenamePolicy(e.ctx, &corev1.RenamePolicyRequest{
		PolicyId: p.ID.String(), NewTitle: "Renamed " + uuid.New().String()[:6], ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("RenamePolicy: %v", err)
	}
	if got := e.refIDs(t, resp.GetDraftVersionId()); len(got) != 1 || got[0] != refID {
		t.Fatalf("staged draft references = %v, want [%s]", got, refID)
	}
}

func TestVersionLinks_RefusalsReachTheWireAsCodes(t *testing.T) {
	e := setupLinkFlow(t)
	p, refID := e.publishedPolicyWithReference(t)
	ctx := e.ctx
	published := p.CurrentPublishedVersionID.String()

	_, err := e.vh.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: published, ReferenceIds: []string{refID}})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("published version: code = %v, want FailedPrecondition", status.Code(err))
	}
	// Linking never makes a draft.
	if got, _ := e.ps.GetPolicy(ctx, p.ID); got.CurrentDraftVersionID != uuid.Nil {
		t.Fatalf("a draft appeared: %s", got.CurrentDraftVersionID)
	}

	resp, err := e.ph.SaveDraft(ctx, &corev1.SaveDraftRequest{PolicyId: p.ID.String(), ContentJson: `{"sections":{}}`, ActorUserId: uuid.New().String()})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	draft := resp.GetVersion().GetId()
	other, _ := domain.NewPolicy("Other "+uuid.New().String()[:6], e.gid, domain.SensitivityStandard, uuid.New())
	other, err = e.ps.CreatePolicy(ctx, other)
	if err != nil {
		t.Fatalf("CreatePolicy(other): %v", err)
	}
	cases := []struct {
		name string
		call func() error
		want codes.Code
	}{
		{"repeated reference", func() error {
			_, err := e.vh.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: draft, ReferenceIds: []string{refID, refID}})
			return err
		}, codes.InvalidArgument},
		{"reference not in the library", func() error {
			_, err := e.vh.SetVersionReferences(ctx, &corev1.SetVersionReferencesRequest{PolicyVersionId: draft, ReferenceIds: []string{uuid.New().String()}})
			return err
		}, codes.NotFound},
		{"unknown version", func() error {
			_, err := e.vh.GetVersionLinks(ctx, &corev1.GetVersionLinksRequest{PolicyVersionId: uuid.New().String()})
			return err
		}, codes.NotFound},
		{"unknown relationship type", func() error {
			_, err := e.vh.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: draft,
				Links: []*corev1.RelatedLinkInput{{PolicyId: other.ID.String(), RelationType: "LIKES"}}})
			return err
		}, codes.InvalidArgument},
		{"link to itself", func() error {
			_, err := e.vh.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: draft,
				Links: []*corev1.RelatedLinkInput{{PolicyId: p.ID.String(), RelationType: "RELATED_TO"}}})
			return err
		}, codes.InvalidArgument},
		{"policy that does not exist", func() error {
			_, err := e.vh.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: draft,
				Links: []*corev1.RelatedLinkInput{{PolicyId: uuid.New().String(), RelationType: "RELATED_TO"}}})
			return err
		}, codes.NotFound},
	}
	for _, tc := range cases {
		if got := status.Code(tc.call()); got != tc.want {
			t.Errorf("%s: code = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The same link twice is refused; two types to the same policy are fine.
	_, err = e.vh.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: draft, Links: []*corev1.RelatedLinkInput{
		{PolicyId: other.ID.String(), RelationType: "RELATED_TO"}, {PolicyId: other.ID.String(), RelationType: "RELATED_TO"}}})
	if status.Code(err) != codes.InvalidArgument {
		t.Errorf("repeated link: code = %v, want InvalidArgument", status.Code(err))
	}
	ok, err := e.vh.SetVersionRelated(ctx, &corev1.SetVersionRelatedRequest{PolicyVersionId: draft, Links: []*corev1.RelatedLinkInput{
		{PolicyId: other.ID.String(), RelationType: "RELATED_TO"}, {PolicyId: other.ID.String(), RelationType: "DEPENDS_ON"}}})
	if err != nil || len(ok.GetLinks().GetRelated()) != 2 || !ok.GetLinks().GetEditable() {
		t.Fatalf("two types to one policy: %v, %+v", err, ok)
	}
}
