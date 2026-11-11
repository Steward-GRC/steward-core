// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"archive/zip"
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// zipWith builds a real zip whose entries carry the given names, the shape of
// an Office or OpenDocument container. An entry named "mimetype" holds the
// body given in mimetype.
func zipWith(t *testing.T, mimetype string, names ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	if mimetype != "" {
		w, err := zw.Create("mimetype")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(mimetype))
	}
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
	pdfBytes  = []byte("%PDF-1.4\n%\xe2\xe3\xcf\xd3\n1 0 obj")
	oleBytes  = []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1\x00\x00")
	mp4Bytes  = []byte("\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00")
	movBytes  = []byte("\x00\x00\x00\x14ftypqt  \x00\x00\x00\x00")
	webmBytes = []byte("\x1aE\xdf\xa3\x01\x00")
	rtfBytes  = []byte(`{\rtf1\ansi hello}`)
)

// Mock tests: "keeps <name> and serves it back as a download" (the type half).
func TestDetectAsset_AcceptedFiles(t *testing.T) {
	const wordType = "application/vnd.openxmlformats-officedocument.wordprocessingml.document"
	const sheetType = "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
	const deckType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"
	cases := []struct {
		name     string
		data     []byte
		declared string
		kind     domain.AssetKind
		want     string
	}{
		{"report.pdf", pdfBytes, "application/pdf", domain.KindPDF, "application/pdf"},
		{"letter.doc", oleBytes, "application/msword", domain.KindWord, "application/msword"},
		{"sheet.xls", oleBytes, "application/vnd.ms-excel", domain.KindOffice, "application/vnd.ms-excel"},
		{"deck.ppt", oleBytes, "application/vnd.ms-powerpoint", domain.KindOffice, "application/vnd.ms-powerpoint"},
		{"letter.docx", zipWith(t, "", "[Content_Types].xml", "word/document.xml"), "application/octet-stream", domain.KindWord, wordType},
		{"sheet.xlsx", zipWith(t, "", "[Content_Types].xml", "xl/workbook.xml"), "application/octet-stream", domain.KindOffice, sheetType},
		{"deck.pptx", zipWith(t, "", "[Content_Types].xml", "ppt/presentation.xml"), "", domain.KindOffice, deckType},
		{"notes.odt", zipWith(t, "application/vnd.oasis.opendocument.text"), "", domain.KindOffice, "application/vnd.oasis.opendocument.text"},
		{"rows.ods", zipWith(t, "application/vnd.oasis.opendocument.spreadsheet"), "", domain.KindOffice, "application/vnd.oasis.opendocument.spreadsheet"},
		{"show.odp", zipWith(t, "application/vnd.oasis.opendocument.presentation"), "", domain.KindOffice, "application/vnd.oasis.opendocument.presentation"},
		{"memo.rtf", rtfBytes, "application/rtf", domain.KindOffice, "application/rtf"},
		{"clip.mp4", mp4Bytes, "video/mp4", domain.KindVideo, "video/mp4"},
		{"clip.mov", movBytes, "video/quicktime", domain.KindVideo, "video/quicktime"},
		{"clip.webm", webmBytes, "video/webm", domain.KindVideo, "video/webm"},
		{"notes.txt", []byte("Visitors sign in.\n"), "text/plain", domain.KindText, "text/plain; charset=utf-8"},
		{"notes.md", []byte("# Title\n\n- one\n"), "text/markdown", domain.KindMarkdown, "text/markdown; charset=utf-8"},
		{"rows.csv", []byte("a,b\n1,2\n"), "text/csv", domain.KindOffice, "text/csv; charset=utf-8"},
		{"rows.csv", []byte("a,b\n1,2\n"), "application/vnd.ms-excel", domain.KindOffice, "text/csv; charset=utf-8"},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/"+tc.declared, func(t *testing.T) {
			got, err := domain.DetectAsset(tc.data, tc.name, tc.declared, domain.AllAssetKinds())
			if err != nil {
				t.Fatalf("DetectAsset: %v", err)
			}
			if got.Kind != tc.kind || got.ContentType != tc.want {
				t.Errorf("got %s %q, want %s %q", got.Kind, got.ContentType, tc.kind, tc.want)
			}
			if got.IsImage() {
				t.Errorf("a file must not report as an image")
			}
		})
	}
}

// Mock tests: "accepts JPEG, GIF and WebP, and takes the type from the bytes"
// and "keeps an image and serves the same bytes back".
func TestDetectAsset_ImagesTakeTheTypeFromTheBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		data []byte
		file string
		want string
	}{
		"png":  {pngBytes, "logo.png", "image/png"},
		"jpeg": {jpegBytes, "x.jpeg", "image/jpeg"},
		"jpg":  {jpegBytes, "x.jpg", "image/jpeg"},
		"gif":  {gifBytes, "x", "image/gif"},
		"webp": {webpBytes, "x.webp", "image/webp"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := domain.DetectAsset(tc.data, tc.file, "application/octet-stream", domain.AllAssetKinds())
			if err != nil {
				t.Fatalf("DetectAsset: %v", err)
			}
			if got.Kind != domain.KindImage || !got.IsImage() || got.ContentType != tc.want {
				t.Errorf("got %s %q, want image %q", got.Kind, got.ContentType, tc.want)
			}
		})
	}
}

// Mock test: "takes the kind from the bytes, and refuses a name that says
// something else", and "refuses a file whose bytes don't match its name".
func TestDetectAsset_NameOrDeclaredTypeThatDisagreesWithTheBytes(t *testing.T) {
	docx := zipWith(t, "", "[Content_Types].xml", "word/document.xml")
	cases := map[string]struct {
		data     []byte
		name     string
		declared string
	}{
		"text named png":      {[]byte("just some words"), "notes.png", "image/png"},
		"docx named pdf":      {docx, "letter.pdf", "application/pdf"},
		"pdf named txt":       {pdfBytes, "notes.txt", "text/plain"},
		"ole named docx":      {oleBytes, "letter.docx", "application/octet-stream"},
		"png named pdf":       {pngBytes, "a.pdf", ""},
		"png declared as pdf": {pngBytes, "a.png", "application/pdf"},
		"pdf declared as png": {pdfBytes, "a.pdf", "image/png"},
		"mp4 named mov":       {mp4Bytes, "a.mov", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := domain.DetectAsset(tc.data, tc.name, tc.declared, domain.AllAssetKinds())
			if !errors.Is(err, domain.ErrAssetMismatch) {
				t.Fatalf("err = %v, want ErrAssetMismatch", err)
			}
			if !strings.Contains(err.Error(), "do not match") {
				t.Errorf("message %q must say the contents do not match", err)
			}
		})
	}
}

// Mock test: "refuses never-allowed kinds, even renamed".
func TestDetectAsset_NeverAllowedKinds(t *testing.T) {
	macroDocx := zipWith(t, "", "[Content_Types].xml", "word/document.xml", "word/vbaProject.bin")
	macroXlsm := zipWith(t, "", "[Content_Types].xml", "xl/workbook.xml", "xl/vbaProject.bin")
	macroType := zipWith(t, "", "word/document.xml") // renamed .docm, bytes look clean
	oleMacro := append(append([]byte{}, oleBytes...), []byte("_VBA_PROJECT")...)
	cases := []struct {
		name string
		data []byte
	}{
		{"page.html", []byte("<!DOCTYPE html><html><body>hi</body></html>")},
		{"notes.txt", []byte("<html><script>alert(1)</script></html>")},
		{"notes.md", []byte("hello <script>alert(1)</script>")},
		{"report.pdf", []byte("<html><body>not a pdf</body></html>")},
		{"image.svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"notes.txt", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`)},
		{"data.xml", []byte(`<?xml version="1.0"?><a/>`)},
		{"app.js", []byte("alert(1)")},
		{"site.css", []byte("a{color:red}")},
		{"run.sh", []byte("#!/bin/sh\necho hi\n")},
		{"notes.txt", []byte("#!/bin/sh\necho hi\n")},
		{"setup.exe", []byte("MZ\x90\x00\x03\x00")},
		{"report.pdf", []byte("MZ\x90\x00\x03\x00")},
		{"tool.bin", []byte("\x7fELF\x02\x01\x01")},
		{"tool.dylib", []byte("\xcf\xfa\xed\xfe\x07\x00")},
		{"macro.docm", macroType},
		{"book.xlsm", macroXlsm},
		{"letter.docx", macroDocx},
		{"old.doc", oleMacro},
		{"bundle.zip", zipWith(t, "", "data.txt")},
		{"bundle.zip", []byte("PK\x03\x04data.txt")},
		{"blob.bin", []byte{0x00, 0x01, 0x02, 0xff, 0xfe}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := domain.DetectAsset(tc.data, tc.name, "application/octet-stream", domain.AllAssetKinds())
			if !errors.Is(err, domain.ErrAssetUnsupportedType) && !errors.Is(err, domain.ErrAssetMismatch) {
				t.Fatalf("err = %v, want a refusal", err)
			}
			if errors.Is(err, domain.ErrAssetKindDisabled) {
				t.Errorf("a never-allowed kind is refused outright, not as a switched-off kind")
			}
		})
	}
}

// A never-allowed kind stays refused when every switch is on, and there is no
// switch for it.
func TestAssetKinds_HaveNoSwitchForNeverAllowed(t *testing.T) {
	for _, k := range domain.AssetKinds() {
		for _, ext := range k.Extensions {
			for _, never := range []string{"exe", "html", "htm", "svg", "xml", "js", "css", "docm", "xlsm", "pptm", "sh", "zip"} {
				if ext == never {
					t.Errorf("kind %s offers never-allowed extension %q", k.Kind, ext)
				}
			}
		}
	}
	if _, err := domain.ParseAssetKinds([]string{"html"}); err == nil {
		t.Errorf("ParseAssetKinds accepted a kind that does not exist")
	}
}

// The admin setting: only kinds that are on are stored.
func TestDetectAsset_SwitchedOffKindIsRefused(t *testing.T) {
	onlyImages := []domain.AssetKind{domain.KindImage}
	if _, err := domain.DetectAsset(pngBytes, "a.png", "", onlyImages); err != nil {
		t.Fatalf("image with images on: %v", err)
	}
	for name, tc := range map[string]struct {
		data []byte
		file string
		off  string
	}{
		"pdf":      {pdfBytes, "a.pdf", "PDF"},
		"video":    {mp4Bytes, "a.mp4", "Video"},
		"text":     {[]byte("hello"), "a.txt", "Text"},
		"markdown": {[]byte("# hi"), "a.md", "Markdown"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := domain.DetectAsset(tc.data, tc.file, "", onlyImages)
			if !errors.Is(err, domain.ErrAssetKindDisabled) {
				t.Fatalf("err = %v, want ErrAssetKindDisabled", err)
			}
			if !strings.Contains(err.Error(), tc.off) {
				t.Errorf("message %q should name the kind %q", err, tc.off)
			}
		})
	}
	if _, err := domain.DetectAsset(pngBytes, "a.png", "", nil); !errors.Is(err, domain.ErrAssetKindDisabled) {
		t.Errorf("every kind off: err = %v, want ErrAssetKindDisabled", err)
	}
}

// Mock test: "applies the same size cap to files" and "refuses a file over the
// size cap".
func TestAssetSizeCaps(t *testing.T) {
	if domain.MaxAssetBytes != 10<<20 {
		t.Errorf("images stay at 10 MiB, got %d", domain.MaxAssetBytes)
	}
	for _, k := range domain.AllAssetKinds() {
		max := domain.MaxBytesFor(k)
		if k == domain.KindImage && max != domain.MaxAssetBytes {
			t.Errorf("image cap = %d", max)
		}
		if err := domain.ValidateKindSize(k, int(max)); err != nil {
			t.Errorf("%s at its cap: %v", k, err)
		}
		if err := domain.ValidateKindSize(k, int(max)+1); !errors.Is(err, domain.ErrAssetTooLarge) {
			t.Errorf("%s over its cap: err = %v, want ErrAssetTooLarge", k, err)
		}
	}
	if domain.MaxBytesFor(domain.KindPDF) <= domain.MaxAssetBytes {
		t.Errorf("files have their own cap, larger than the image cap")
	}
	if domain.MaxBytesFor(domain.KindText) >= domain.MaxBytesFor(domain.KindPDF) {
		t.Errorf("text and markdown are capped lower than documents")
	}
	if err := domain.ValidateAssetSize(domain.MaxFileBytes + 1); !errors.Is(err, domain.ErrAssetTooLarge) {
		t.Errorf("over the ceiling: err = %v", err)
	}
	if err := domain.ValidateAssetSize(0); !errors.Is(err, domain.ErrAssetEmpty) {
		t.Errorf("empty: err = %v", err)
	}
}

func TestDetectAsset_Empty(t *testing.T) {
	if _, err := domain.DetectAsset(nil, "a.png", "", domain.AllAssetKinds()); !errors.Is(err, domain.ErrAssetEmpty) {
		t.Fatalf("err = %v, want ErrAssetEmpty", err)
	}
}

func TestAssetKindOf(t *testing.T) {
	for ct, want := range map[string]string{
		"image/png": "image", "application/pdf": "file", "text/plain; charset=utf-8": "file", "video/mp4": "file",
	} {
		if got := domain.AssetClassOf(ct); got != want {
			t.Errorf("AssetClassOf(%q) = %q, want %q", ct, got, want)
		}
	}
}
