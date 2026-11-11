// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	objectstore "github.com/Bugs5382/go-objectstore"
	"github.com/Bugs5382/go-objectstore/memstore"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

func putOpts() objectstore.PutOptions { return objectstore.PutOptions{ContentType: "application/pdf"} }

type fakeAssetAdmin struct {
	files   map[uuid.UUID]domain.LinkedAsset
	deleted []uuid.UUID
	err     error
}

func (f *fakeAssetAdmin) ListWithLinks(_ context.Context, unlinkedOnly bool) ([]domain.LinkedAsset, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []domain.LinkedAsset
	for _, la := range f.files {
		if unlinkedOnly && len(la.Links) > 0 {
			continue
		}
		out = append(out, la)
	}
	return out, nil
}

func (f *fakeAssetAdmin) DeleteWithLinks(_ context.Context, id uuid.UUID) (domain.LinkedAsset, error) {
	if f.err != nil {
		return domain.LinkedAsset{}, f.err
	}
	la, ok := f.files[id]
	if !ok {
		return domain.LinkedAsset{}, store.ErrAssetNotFound
	}
	delete(f.files, id)
	f.deleted = append(f.deleted, id)
	return la, nil
}

type adminFixture struct {
	h       *AssetHandler
	admin   *fakeAssetAdmin
	objs    *memstore.Store
	audit   *assetAuditSink
	linked  domain.LinkedAsset
	orphan  domain.LinkedAsset
	policy  uuid.UUID
	context context.Context
}

func newAdminFixture(t *testing.T) *adminFixture {
	t.Helper()
	ctx := context.Background()
	objs := memstore.New()
	mk := func(name string) domain.Asset {
		id := uuid.New()
		a := domain.Asset{ID: id, StorageKey: domain.StorageKeyFor(id), ContentType: "application/pdf",
			SizeBytes: 5, Filename: name, CreatedByUserID: "uploader-1"}
		if _, err := objs.Put(ctx, a.StorageKey, strings.NewReader("12345"), putOpts()); err != nil {
			t.Fatal(err)
		}
		return a
	}
	pol := uuid.New()
	f := &adminFixture{policy: pol, context: ctx, objs: objs, audit: &assetAuditSink{}}
	f.linked = domain.LinkedAsset{Asset: mk("secret.pdf"), Links: []domain.AssetDocLink{{PolicyID: pol, Title: "Access", Location: domain.LinkLocationPolicy}}}
	f.orphan = domain.LinkedAsset{Asset: mk("orphan.pdf")}
	f.admin = &fakeAssetAdmin{files: map[uuid.UUID]domain.LinkedAsset{f.linked.Asset.ID: f.linked, f.orphan.Asset.ID: f.orphan}}
	backend := newFakeAssetBackend()
	backend.byID[f.linked.Asset.ID] = f.linked.Asset
	backend.byID[f.orphan.Asset.ID] = f.orphan.Asset
	f.h = NewAssetHandler(backend, objs, f.audit).WithLinks(f.admin)
	return f
}

func TestListAssets_ReturnsLinkStateAndUploader(t *testing.T) {
	f := newAdminFixture(t)
	resp, err := f.h.ListAssets(f.context, &corev1.ListAssetsRequest{})
	if err != nil {
		t.Fatalf("ListAssets: %v", err)
	}
	if len(resp.GetAssets()) != 2 {
		t.Fatalf("got %d, want 2", len(resp.GetAssets()))
	}
	for _, s := range resp.GetAssets() {
		a := s.GetAsset()
		if a.GetCreatedByUserId() != "uploader-1" || a.GetFilename() == "" || a.GetKind() != "pdf" || a.GetSizeBytes() != 5 {
			t.Errorf("asset fields = %+v", a)
		}
		switch a.GetId() {
		case f.linked.Asset.ID.String():
			if !s.GetLinked() || len(s.GetDocuments()) != 1 || s.GetDocuments()[0].GetPolicyId() != f.policy.String() ||
				s.GetDocuments()[0].GetTitle() != "Access" || s.GetDocuments()[0].GetLocation() != "policy" {
				t.Errorf("linked entry = %+v", s)
			}
		case f.orphan.Asset.ID.String():
			if s.GetLinked() || len(s.GetDocuments()) != 0 {
				t.Errorf("orphan reported as linked: %+v", s)
			}
		}
	}
}

func TestListAssets_UnlinkedOnlyFilter(t *testing.T) {
	f := newAdminFixture(t)
	resp, err := f.h.ListAssets(f.context, &corev1.ListAssetsRequest{UnlinkedOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetAssets()) != 1 || resp.GetAssets()[0].GetAsset().GetId() != f.orphan.Asset.ID.String() {
		t.Errorf("got %+v, want only the orphan", resp.GetAssets())
	}
}

func TestListAssets_NotConfiguredAndBackendFailure(t *testing.T) {
	h := NewAssetHandler(newFakeAssetBackend(), memstore.New(), nil)
	if _, err := h.ListAssets(context.Background(), &corev1.ListAssetsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("unconfigured code = %v, want FailedPrecondition", status.Code(err))
	}
	f := newAdminFixture(t)
	f.admin.err = errors.New("db down")
	if _, err := f.h.ListAssets(f.context, &corev1.ListAssetsRequest{}); status.Code(err) != codes.Internal {
		t.Errorf("backend failure code = %v, want Internal", status.Code(err))
	}
}

func TestDeleteAsset_UnlinkedRemovesBytesAndRecordAndAudits(t *testing.T) {
	f := newAdminFixture(t)
	o := f.orphan.Asset
	resp, err := f.h.DeleteAsset(f.context, &corev1.DeleteAssetRequest{Id: o.ID.String(), ActorUserId: "admin-1"})
	if err != nil {
		t.Fatalf("DeleteAsset: %v", err)
	}
	if resp.GetId() != o.ID.String() || resp.GetFilename() != "orphan.pdf" || resp.GetKind() != "pdf" || len(resp.GetRemovedFrom()) != 0 {
		t.Errorf("response = %+v", resp)
	}
	if _, err := f.objs.Stat(f.context, o.StorageKey); err == nil {
		t.Error("bytes still in the object store")
	}
	if len(f.admin.deleted) != 1 || f.admin.deleted[0] != o.ID {
		t.Errorf("record not deleted: %v", f.admin.deleted)
	}
}

func TestDeleteAsset_LinkedFileIsDeletedAndSaysWhereItCameOut(t *testing.T) {
	f := newAdminFixture(t)
	l := f.linked.Asset
	resp, err := f.h.DeleteAsset(f.context, &corev1.DeleteAssetRequest{Id: l.ID.String(), ActorUserId: "admin-1"})
	if err != nil {
		t.Fatalf("a linked file must be deletable: %v", err)
	}
	if len(resp.GetRemovedFrom()) != 1 || resp.GetRemovedFrom()[0].GetPolicyId() != f.policy.String() {
		t.Errorf("removed_from = %+v", resp.GetRemovedFrom())
	}
	if _, err := f.objs.Stat(f.context, l.StorageKey); err == nil {
		t.Error("bytes still in the object store")
	}
}

func TestDeleteAsset_AuditEntryHasWhoWhenWhatAndNoNetworkDetail(t *testing.T) {
	f := newAdminFixture(t)
	l := f.linked.Asset
	if _, err := f.h.DeleteAsset(f.context, &corev1.DeleteAssetRequest{Id: l.ID.String(), ActorUserId: "admin-1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.audit.evs) != 1 {
		t.Fatalf("got %d audit events, want 1", len(f.audit.evs))
	}
	ev := f.audit.evs[0]
	if ev.Action != "asset.deleted" || ev.ActorUserID != "admin-1" || ev.Subject != "asset:"+l.ID.String() {
		t.Errorf("event = %+v", ev)
	}
	if ev.Attributes["name"] != "secret.pdf" || ev.Attributes["kind"] != "pdf" || ev.Attributes["documents"] != f.policy.String() {
		t.Errorf("attributes = %+v", ev.Attributes)
	}
	// Nothing else about the caller: exactly these keys, no address or agent.
	if len(ev.Attributes) != 3 {
		t.Errorf("attributes = %+v, want only name, kind and documents", ev.Attributes)
	}
}

func TestDeleteAsset_Refusals(t *testing.T) {
	f := newAdminFixture(t)
	other := uuid.New().String() // an id the editor store never held, e.g. a reporting attachment
	cases := []struct {
		name string
		req  *corev1.DeleteAssetRequest
		want codes.Code
	}{
		{"bad id", &corev1.DeleteAssetRequest{Id: "nope", ActorUserId: "admin-1"}, codes.InvalidArgument},
		{"no actor", &corev1.DeleteAssetRequest{Id: f.orphan.Asset.ID.String()}, codes.InvalidArgument},
		{"unknown id", &corev1.DeleteAssetRequest{Id: other, ActorUserId: "admin-1"}, codes.NotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := f.h.DeleteAsset(f.context, tc.req); status.Code(err) != tc.want {
				t.Errorf("code = %v, want %v (%v)", status.Code(err), tc.want, err)
			}
		})
	}
	if len(f.admin.deleted) != 0 || len(f.audit.evs) != 0 {
		t.Errorf("a refused delete changed something: deleted=%v audit=%d", f.admin.deleted, len(f.audit.evs))
	}
	for _, la := range []domain.LinkedAsset{f.linked, f.orphan} {
		if _, err := f.objs.Stat(f.context, la.Asset.StorageKey); err != nil {
			t.Errorf("bytes of an untouched file are gone: %v", err)
		}
	}
}

func TestDeleteAsset_ObjectStoreOnlyHoldsEditorKeys(t *testing.T) {
	// A reporting attachment lives in another store under its own keys; the
	// delete only ever derives a key from an editor_assets row, so an id that
	// has no such row cannot reach any object.
	f := newAdminFixture(t)
	stray := "reporting-attachments/abc"
	if _, err := f.objs.Put(f.context, stray, strings.NewReader("x"), putOpts()); err != nil {
		t.Fatal(err)
	}
	_, _ = f.h.DeleteAsset(f.context, &corev1.DeleteAssetRequest{Id: uuid.New().String(), ActorUserId: "admin-1"})
	if _, err := f.objs.Stat(f.context, stray); err != nil {
		t.Errorf("an unrelated object was touched: %v", err)
	}
}

func TestDeleteAsset_NoAuditWhenTheRecordCannotBeRemoved(t *testing.T) {
	f := newAdminFixture(t)
	f.admin.err = errors.New("db down")
	if _, err := f.h.DeleteAsset(f.context, &corev1.DeleteAssetRequest{Id: f.orphan.Asset.ID.String(), ActorUserId: "admin-1"}); status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
	if len(f.audit.evs) != 0 {
		t.Error("audit entry written for a delete that failed")
	}
}
