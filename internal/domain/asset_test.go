// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Minimal magic-byte fixtures for the sniffable image types. http.DetectContentType
// keys off these leading signatures, so a few bytes suffice.
var (
	pngBytes  = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	jpegBytes = []byte("\xff\xd8\xff\xe0\x00\x10JFIF")
	gifBytes  = []byte("GIF89a\x01\x00\x01\x00")
	webpBytes = []byte("RIFF\x00\x00\x00\x00WEBPVP8 ")
)

func TestDetectAssetContentType_Allowed(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want string
	}{
		"png":  {pngBytes, "image/png"},
		"jpeg": {jpegBytes, "image/jpeg"},
		"gif":  {gifBytes, "image/gif"},
		"webp": {webpBytes, "image/webp"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := domain.DetectAssetContentType(tc.data)
			if err != nil {
				t.Fatalf("DetectAssetContentType: unexpected err: %v", err)
			}
			if got != tc.want {
				t.Errorf("content type = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDetectAssetContentType_Empty(t *testing.T) {
	_, err := domain.DetectAssetContentType(nil)
	if !errors.Is(err, domain.ErrAssetEmpty) {
		t.Fatalf("err = %v, want ErrAssetEmpty", err)
	}
}

func TestDetectAssetContentType_RejectsNonImage(t *testing.T) {
	// Plain text and an SVG (XML) both sniff outside the raster allowlist and
	// must be rejected — SVG especially, as an inline-served XSS vector.
	for name, data := range map[string][]byte{
		"text": []byte("just some plain text, definitely not an image"),
		"svg":  []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`),
		"pdf":  []byte("%PDF-1.4\n%dummy"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := domain.DetectAssetContentType(data)
			if !errors.Is(err, domain.ErrAssetUnsupportedType) {
				t.Fatalf("err = %v, want ErrAssetUnsupportedType", err)
			}
		})
	}
}

func TestValidateAssetSize(t *testing.T) {
	if err := domain.ValidateAssetSize(0); !errors.Is(err, domain.ErrAssetEmpty) {
		t.Errorf("size 0: err = %v, want ErrAssetEmpty", err)
	}
	if err := domain.ValidateAssetSize(1024); err != nil {
		t.Errorf("size 1024: unexpected err %v", err)
	}
	if err := domain.ValidateAssetSize(domain.MaxAssetBytes); err != nil {
		t.Errorf("size == max: unexpected err %v", err)
	}
	if err := domain.ValidateAssetSize(domain.MaxAssetBytes + 1); !errors.Is(err, domain.ErrAssetTooLarge) {
		t.Errorf("over max: err = %v, want ErrAssetTooLarge", err)
	}
}

func TestSanitizeFilename(t *testing.T) {
	cases := map[string]string{
		"logo.png":                 "logo.png",
		"  spaced.jpg  ":           "spaced.jpg",
		"/etc/passwd":              "passwd",
		`C:\Users\Public\evil.png`: "evil.png",
		"../../traversal.gif":      "traversal.gif",
		"":                         "",
	}
	for in, want := range cases {
		if got := domain.SanitizeFilename(in); got != want {
			t.Errorf("SanitizeFilename(%q) = %q, want %q", in, got, want)
		}
	}
	if got := domain.SanitizeFilename(strings.Repeat("a", 300)); len(got) != 255 {
		t.Errorf("overlong filename not truncated to 255: got len %d", len(got))
	}
}

func TestAssetURLAndKey(t *testing.T) {
	id := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	a := domain.Asset{ID: id}
	if got, want := a.URL(), domain.AssetURLPrefix+id.String(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
	if got, want := domain.StorageKeyFor(id), domain.AssetKeyPrefix+id.String(); got != want {
		t.Errorf("StorageKeyFor() = %q, want %q", got, want)
	}
}
