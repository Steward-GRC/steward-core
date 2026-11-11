// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	objectstore "github.com/Bugs5382/go-objectstore"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
)

func zipBytes(t *testing.T, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, n := range names {
		w, err := zw.Create(n)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte("x"))
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

var (
	pdfFixture  = []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj")
	oleFixture  = []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1\x00\x00")
	mp4Fixture  = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00")
	movFixture  = []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00")
	webmFixture = []byte("\x1aE\xdf\xa3\x01\x00")
)

type assetAuditSink struct{ evs []audit.Event }

func (a *assetAuditSink) Emit(_ context.Context, ev audit.Event) error {
	a.evs = append(a.evs, ev)
	return nil
}

type fakeAssetSettings struct {
	s   domain.AssetSettings
	err error
}

func newFakeAssetSettings() *fakeAssetSettings {
	return &fakeAssetSettings{s: domain.DefaultAssetSettings()}
}

func (f *fakeAssetSettings) Get(context.Context) (domain.AssetSettings, error) { return f.s, f.err }

func (f *fakeAssetSettings) Set(_ context.Context, kinds []domain.AssetKind, actor string) (domain.AssetSettings, error) {
	if f.err != nil {
		return domain.AssetSettings{}, f.err
	}
	f.s = domain.AssetSettings{AllowedKinds: kinds, UpdatedBy: actor}
	return f.s, nil
}

func newFileHandler(t *testing.T) (*AssetHandler, *fakeAssetSettings, *assetAuditSink) {
	t.Helper()
	settings := newFakeAssetSettings()
	sink := &assetAuditSink{}
	h := NewAssetHandler(newFakeAssetBackend(), newFakeObjectStore(), sink).WithSettings(settings)
	return h, settings, sink
}

func upload(h *AssetHandler, data []byte, name, declared string) (*corev1.UploadAssetResponse, error) {
	return h.UploadAsset(context.Background(), &corev1.UploadAssetRequest{
		Data: data, Filename: name, ContentType: declared, CreatedByUserId: "user-1",
	})
}

// Mock: "keeps <name> and serves it back as a download" (every row of the accepted table).
func TestUploadAsset_KeepsEachFileAndServesItBackAsAFile(t *testing.T) {
	cases := []struct {
		name, declared, wantType, wantFileKind string
		data                                   []byte
	}{
		{"report.pdf", "application/pdf", "application/pdf", "pdf", pdfFixture},
		{"letter.doc", "application/msword", "application/msword", "word", oleFixture},
		{"sheet.xls", "application/vnd.ms-excel", "application/vnd.ms-excel", "office", oleFixture},
		{"deck.ppt", "application/vnd.ms-powerpoint", "application/vnd.ms-powerpoint", "office", oleFixture},
		{"letter.docx", "application/octet-stream", "application/vnd.openxmlformats-officedocument.wordprocessingml.document", "word", zipBytes(t, "[Content_Types].xml", "word/document.xml")},
		{"sheet.xlsx", "application/octet-stream", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", "office", zipBytes(t, "[Content_Types].xml", "xl/workbook.xml")},
		{"deck.pptx", "application/octet-stream", "application/vnd.openxmlformats-officedocument.presentationml.presentation", "office", zipBytes(t, "[Content_Types].xml", "ppt/presentation.xml")},
		{"clip.mp4", "video/mp4", "video/mp4", "video", mp4Fixture},
		{"clip.mov", "video/quicktime", "video/quicktime", "video", movFixture},
		{"clip.webm", "video/webm", "video/webm", "video", webmFixture},
		{"notes.txt", "text/plain; charset=utf-8", "text/plain; charset=utf-8", "text", []byte("Visitors sign in.\n")},
		{"notes.md", "text/markdown; charset=utf-8", "text/markdown; charset=utf-8", "markdown", []byte("# Title\n\n- one\n")},
		{"rows.csv", "text/csv; charset=utf-8", "text/csv; charset=utf-8", "office", []byte("a,b\n1,2\n")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, _, sink := newFileHandler(t)
			resp, err := upload(h, tc.data, tc.name, tc.declared)
			if err != nil {
				t.Fatalf("UploadAsset: %v", err)
			}
			a := resp.GetAsset()
			if a.GetContentType() != tc.wantType || a.GetFilename() != tc.name || a.GetServeAs() != "file" || a.GetKind() != tc.wantFileKind {
				t.Errorf("asset = %+v, want type %q name %q serve_as file kind %s", a, tc.wantType, tc.name, tc.wantFileKind)
			}
			got, err := h.GetAsset(context.Background(), &corev1.GetAssetRequest{Id: a.GetId()})
			if err != nil {
				t.Fatalf("GetAsset: %v", err)
			}
			if !bytes.Equal(got.GetData(), tc.data) || got.GetContentType() != tc.wantType ||
				got.GetFilename() != tc.name || got.GetServeAs() != "file" || got.GetKind() != tc.wantFileKind {
				t.Errorf("served %+v, want the same bytes, type, name and kind", got)
			}
			if len(sink.evs) != 1 || sink.evs[0].Action != "asset.uploaded" {
				t.Errorf("audit = %+v, want one asset.uploaded", sink.evs)
			}
		})
	}
}

// Mock: "keeps serving an image inline".
func TestUploadAsset_ImageIsKindImage(t *testing.T) {
	h, _, _ := newFileHandler(t)
	resp, err := upload(h, pngFixture, "logo.png", "image/png")
	if err != nil {
		t.Fatal(err)
	}
	if a := resp.GetAsset(); a.GetServeAs() != "image" || a.GetKind() != "image" {
		t.Errorf("asset serve_as/kind = %s/%s, want image/image", a.GetServeAs(), a.GetKind())
	}
	got, _ := h.GetAsset(context.Background(), &corev1.GetAssetRequest{Id: resp.GetAsset().GetId()})
	if got.GetServeAs() != "image" {
		t.Errorf("served serve_as = %q, want image", got.GetServeAs())
	}
}

// Mock: "takes the kind from the bytes, and refuses a name that says something else",
// "refuses never-allowed kinds, even renamed", "refuses a file whose bytes don't match its name".
func TestUploadAsset_RefusalsAreInvalidArgumentWithAReason(t *testing.T) {
	docx := zipBytes(t, "[Content_Types].xml", "word/document.xml")
	macro := zipBytes(t, "[Content_Types].xml", "word/document.xml", "word/vbaProject.bin")
	cases := []struct {
		label, name, declared, say string
		data                       []byte
	}{
		{"docx named pdf", "letter.pdf", "application/pdf", "do not match", docx},
		{"pdf named txt", "notes.txt", "text/plain", "do not match", pdfFixture},
		{"ole named docx", "letter.docx", "application/octet-stream", "do not match", oleFixture},
		{"text named png", "notes.png", "image/png", "do not match", []byte("just some words")},
		{"declared type disagrees", "a.png", "application/pdf", "do not match", pngFixture},
		{"html", "page.html", "text/html", "not allowed", []byte("<!DOCTYPE html><html></html>")},
		{"html as txt", "notes.txt", "", "not allowed", []byte("<html><script>alert(1)</script></html>")},
		{"html as pdf", "report.pdf", "", "not allowed", []byte("<html><body>not a pdf</body></html>")},
		{"svg", "image.svg", "image/svg+xml", "not allowed", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"xml", "data.xml", "", "not allowed", []byte(`<?xml version="1.0"?><a/>`)},
		{"script", "run.sh", "", "not allowed", []byte("#!/bin/sh\necho hi\n")},
		{"script as txt", "notes.txt", "", "not allowed", []byte("#!/bin/sh\necho hi\n")},
		{"exe", "setup.exe", "", "not allowed", []byte("MZ\x90\x00\x03\x00")},
		{"exe as pdf", "report.pdf", "", "not allowed", []byte("MZ\x90\x00\x03\x00")},
		{"elf", "tool.bin", "", "not allowed", []byte("\x7fELF\x02\x01\x01")},
		{"docm", "macro.docm", "", "not allowed", macro},
		{"macro in docx", "letter.docx", "", "not allowed", macro},
		{"zip", "bundle.zip", "", "not allowed", zipBytes(t, "data.txt")},
		{"empty", "a.png", "", "empty", nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			h, _, sink := newFileHandler(t)
			_, err := upload(h, tc.data, tc.name, tc.declared)
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("code = %v, want InvalidArgument (err=%v)", status.Code(err), err)
			}
			if !strings.Contains(status.Convert(err).Message(), tc.say) {
				t.Errorf("message %q should say %q", status.Convert(err).Message(), tc.say)
			}
			if len(sink.evs) != 0 {
				t.Errorf("a refused upload must not audit an upload: %+v", sink.evs)
			}
		})
	}
}

// Mock: "applies the same size cap to files" and "refuses a file over the size cap" (at the cap is kept).
func TestUploadAsset_SizeCapsPerKind(t *testing.T) {
	h, _, _ := newFileHandler(t)
	pad := func(head []byte, n int64) []byte {
		return append(append([]byte{}, head...), make([]byte, int(n)-len(head))...)
	}
	for _, tc := range []struct {
		label, name string
		head        []byte
		max         int64
	}{
		{"image", "a.png", pngFixture, domain.MaxAssetBytes},
		{"pdf", "a.pdf", pdfFixture, domain.MaxFileBytes},
		{"text", "a.txt", []byte("x"), domain.MaxTextBytes},
	} {
		data := pad(tc.head, tc.max)
		if tc.label == "text" {
			data = bytes.Repeat([]byte("a"), int(tc.max))
		}
		if _, err := upload(h, data, tc.name, ""); err != nil {
			t.Errorf("%s at its cap: %v", tc.label, err)
		}
		over := append(data, 'a')
		_, err := upload(h, over, tc.name, "")
		if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "exceeds maximum size") {
			t.Errorf("%s over its cap: err = %v, want InvalidArgument exceeds maximum size", tc.label, err)
		}
	}
	if domain.MaxFileBytes <= domain.MaxAssetBytes {
		t.Errorf("the file cap is set on its own and is larger than the image cap")
	}
}

// Mock: "makes uploads fail the way a store that is down does".
func TestUploadAsset_ObjectStoreDownIsInternal(t *testing.T) {
	settings := newFakeAssetSettings()
	h := NewAssetHandler(newFakeAssetBackend(), failingObjects{}, nil).WithSettings(settings)
	_, err := upload(h, pdfFixture, "a.pdf", "application/pdf")
	if status.Code(err) != codes.Internal {
		t.Fatalf("code = %v, want Internal", status.Code(err))
	}
}

type failingObjects struct{}

func (failingObjects) Put(context.Context, string, io.Reader, objectstore.PutOptions) (objectstore.Info, error) {
	return objectstore.Info{}, errors.New("store down")
}

func (failingObjects) Get(context.Context, string, objectstore.GetOptions) (*objectstore.Object, error) {
	return nil, errors.New("store down")
}

// The admin setting, issue 36: the kinds that are on decide what is stored.
func TestAssetSettings_DefaultListsEveryKindOnWithRefusedExtensions(t *testing.T) {
	h, _, _ := newFileHandler(t)
	resp, err := h.GetAssetSettings(context.Background(), &corev1.GetAssetSettingsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	s := resp.GetSettings()
	if len(s.GetKinds()) != len(domain.AllAssetKinds()) {
		t.Fatalf("kinds = %d, want %d", len(s.GetKinds()), len(domain.AllAssetKinds()))
	}
	for _, k := range s.GetKinds() {
		if !k.GetEnabled() || k.GetLabel() == "" || len(k.GetExtensions()) == 0 || k.GetMaxBytes() == 0 {
			t.Errorf("kind %+v should be on with a label, extensions and a cap", k)
		}
	}
	if s.GetMaxFileBytes() != domain.MaxFileBytes {
		t.Errorf("max_file_bytes = %d, want %d", s.GetMaxFileBytes(), domain.MaxFileBytes)
	}
	refused := strings.Join(s.GetRefusedExtensions(), " ")
	for _, want := range []string{"exe", "html", "svg", "js", "docm"} {
		if !strings.Contains(refused, want) {
			t.Errorf("refused extensions %q should list %q", refused, want)
		}
	}
}

func TestAssetSettings_SetTurnsKindsOffAndUploadsFollow(t *testing.T) {
	h, _, sink := newFileHandler(t)
	ctx := context.Background()
	resp, err := h.SetAssetSettings(ctx, &corev1.SetAssetSettingsRequest{AllowedKinds: []string{"image", "pdf"}, ActorUserId: "admin-1"})
	if err != nil {
		t.Fatalf("SetAssetSettings: %v", err)
	}
	on := map[string]bool{}
	for _, k := range resp.GetSettings().GetKinds() {
		on[k.GetKind()] = k.GetEnabled()
	}
	if !on["image"] || !on["pdf"] || on["video"] || on["text"] || resp.GetSettings().GetUpdatedBy() != "admin-1" {
		t.Errorf("settings after set = %+v", on)
	}
	if _, err := upload(h, pdfFixture, "a.pdf", ""); err != nil {
		t.Errorf("pdf is on: %v", err)
	}
	_, err = upload(h, mp4Fixture, "a.mp4", "")
	if status.Code(err) != codes.InvalidArgument || !strings.Contains(err.Error(), "turned off") {
		t.Errorf("video is off: err = %v, want InvalidArgument turned off", err)
	}
	var changed []audit.Event
	for _, ev := range sink.evs {
		if ev.Action == "asset.settings.updated" {
			changed = append(changed, ev)
		}
	}
	if len(changed) != 1 || changed[0].ActorUserID != "admin-1" || changed[0].Attributes["allowed_kinds"] != "pdf,image" {
		t.Errorf("settings audit = %+v, want one entry by admin-1 with the kinds", changed)
	}

	// Everything off is allowed and refuses even an image.
	if _, err := h.SetAssetSettings(ctx, &corev1.SetAssetSettingsRequest{ActorUserId: "admin-1"}); err != nil {
		t.Fatalf("all off: %v", err)
	}
	if _, err := upload(h, pngFixture, "a.png", ""); status.Code(err) != codes.InvalidArgument {
		t.Errorf("all off, image: err = %v, want InvalidArgument", err)
	}
}

func TestAssetSettings_Validation(t *testing.T) {
	h, _, _ := newFileHandler(t)
	ctx := context.Background()
	for label, req := range map[string]*corev1.SetAssetSettingsRequest{
		"no actor":     {AllowedKinds: []string{"pdf"}},
		"unknown kind": {AllowedKinds: []string{"pdf", "html"}, ActorUserId: "a"},
		"svg kind":     {AllowedKinds: []string{"svg"}, ActorUserId: "a"},
	} {
		if _, err := h.SetAssetSettings(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Errorf("%s: code = %v, want InvalidArgument", label, status.Code(err))
		}
	}
	bare := NewAssetHandler(newFakeAssetBackend(), newFakeObjectStore(), nil)
	if _, err := bare.GetAssetSettings(ctx, &corev1.GetAssetSettingsRequest{}); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("no settings store: code = %v, want FailedPrecondition", status.Code(err))
	}
}
