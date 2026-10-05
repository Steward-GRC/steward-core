// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"bytes"
	"context"
	"testing"

	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Bugs5382/go-objectstore/memstore"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

var pngFixture = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR-imagedata")

type fakeAssetBackend struct {
	byID map[uuid.UUID]domain.Asset
}

func newFakeAssetBackend() *fakeAssetBackend {
	return &fakeAssetBackend{byID: map[uuid.UUID]domain.Asset{}}
}

func (f *fakeAssetBackend) Create(_ context.Context, a domain.Asset) (domain.Asset, error) {
	f.byID[a.ID] = a
	return a, nil
}

func (f *fakeAssetBackend) Get(_ context.Context, id uuid.UUID) (domain.Asset, error) {
	a, ok := f.byID[id]
	if !ok {
		return domain.Asset{}, store.ErrAssetNotFound
	}
	return a, nil
}

func newFakeObjectStore() *memstore.Store { return memstore.New() }

func TestUploadAsset_RoundTrip(t *testing.T) {
	backend := newFakeAssetBackend()
	objs := newFakeObjectStore()
	h := NewAssetHandler(backend, objs, nil)

	resp, err := h.UploadAsset(context.Background(), &corev1.UploadAssetRequest{
		Data:            pngFixture,
		ContentType:     "application/octet-stream", // wrong on purpose — sniff wins
		Filename:        "/tmp/logo.png",
		CreatedByUserId: "user-1",
	})
	if err != nil {
		t.Fatalf("UploadAsset: %v", err)
	}
	a := resp.GetAsset()
	if a.GetContentType() != "image/png" {
		t.Errorf("stored content type = %q, want image/png (sniffed, not declared)", a.GetContentType())
	}
	if a.GetFilename() != "logo.png" {
		t.Errorf("filename = %q, want sanitized logo.png", a.GetFilename())
	}
	if a.GetSizeBytes() != int64(len(pngFixture)) {
		t.Errorf("size = %d, want %d", a.GetSizeBytes(), len(pngFixture))
	}
	if a.GetUrl() != domain.AssetURLPrefix+a.GetId() {
		t.Errorf("url = %q, want %q", a.GetUrl(), domain.AssetURLPrefix+a.GetId())
	}

	got, err := h.GetAsset(context.Background(), &corev1.GetAssetRequest{Id: a.GetId()})
	if err != nil {
		t.Fatalf("GetAsset: %v", err)
	}
	if !bytes.Equal(got.GetData(), pngFixture) {
		t.Errorf("GetAsset bytes mismatch")
	}
	if got.GetContentType() != "image/png" {
		t.Errorf("GetAsset content type = %q, want image/png", got.GetContentType())
	}
}

func TestUploadAsset_Validation(t *testing.T) {
	h := NewAssetHandler(newFakeAssetBackend(), newFakeObjectStore(), nil)

	cases := map[string]struct {
		req  *corev1.UploadAssetRequest
		code codes.Code
	}{
		"missing actor": {&corev1.UploadAssetRequest{Data: pngFixture}, codes.InvalidArgument},
		"empty data":    {&corev1.UploadAssetRequest{CreatedByUserId: "u"}, codes.InvalidArgument},
		"not an image":  {&corev1.UploadAssetRequest{Data: []byte("plain text not image"), CreatedByUserId: "u"}, codes.InvalidArgument},
		"too large":     {&corev1.UploadAssetRequest{Data: append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, domain.MaxAssetBytes+1)...), CreatedByUserId: "u"}, codes.InvalidArgument},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := h.UploadAsset(context.Background(), tc.req)
			if status.Code(err) != tc.code {
				t.Fatalf("code = %v, want %v (err=%v)", status.Code(err), tc.code, err)
			}
		})
	}
}

func TestGetAsset_NotFound(t *testing.T) {
	h := NewAssetHandler(newFakeAssetBackend(), newFakeObjectStore(), nil)
	_, err := h.GetAsset(context.Background(), &corev1.GetAssetRequest{Id: uuid.NewString()})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("code = %v, want NotFound", status.Code(err))
	}
}

func TestGetAsset_InvalidID(t *testing.T) {
	h := NewAssetHandler(newFakeAssetBackend(), newFakeObjectStore(), nil)
	_, err := h.GetAsset(context.Background(), &corev1.GetAssetRequest{Id: "not-a-uuid"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", status.Code(err))
	}
}

func TestAssetService_Unavailable_WhenNoObjectStore(t *testing.T) {
	h := NewAssetHandler(newFakeAssetBackend(), nil, nil)
	if _, err := h.UploadAsset(context.Background(), &corev1.UploadAssetRequest{Data: pngFixture, CreatedByUserId: "u"}); status.Code(err) != codes.Unavailable {
		t.Errorf("UploadAsset code = %v, want Unavailable", status.Code(err))
	}
	if _, err := h.GetAsset(context.Background(), &corev1.GetAssetRequest{Id: uuid.NewString()}); status.Code(err) != codes.Unavailable {
		t.Errorf("GetAsset code = %v, want Unavailable", status.Code(err))
	}
}
