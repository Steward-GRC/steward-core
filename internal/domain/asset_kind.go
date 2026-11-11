// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"strings"
	"time"
)

// AssetKind is a kind of stored file the admin setting turns on or off.
type AssetKind string

// The kinds Steward can accept. Kinds that are never accepted (web pages,
// scripts, executables, macro-enabled Office files, archives) are not here:
// they have no switch and are refused whatever the setting says.
const (
	KindPDF      AssetKind = "pdf"
	KindImage    AssetKind = "image"
	KindVideo    AssetKind = "video"
	KindWord     AssetKind = "word"
	KindText     AssetKind = "text"
	KindMarkdown AssetKind = "markdown"
	KindOffice   AssetKind = "office"
)

// AssetClassImage and AssetClassFile are what a stored asset reports so the
// gateway knows whether to serve it inline or as a download.
const (
	AssetClassImage = "image"
	AssetClassFile  = "file"
)

// AssetKindInfo describes one kind for the admin screen and the file picker.
type AssetKindInfo struct {
	Kind       AssetKind
	Label      string
	Extensions []string
}

var assetKinds = []AssetKindInfo{
	{KindPDF, "PDF", []string{"pdf"}},
	{KindImage, "Images", []string{"png", "jpg", "jpeg", "gif", "webp"}},
	{KindVideo, "Video", []string{"mp4", "m4v", "webm", "mov"}},
	{KindWord, "Word documents", []string{"doc", "docx"}},
	{KindText, "Text", []string{"txt"}},
	{KindMarkdown, "Markdown", []string{"md"}},
	{KindOffice, "Other office documents", []string{"xls", "xlsx", "csv", "ppt", "pptx", "odt", "ods", "odp", "rtf"}},
}

// AssetKinds returns every kind that has a switch, in display order.
func AssetKinds() []AssetKindInfo {
	out := make([]AssetKindInfo, len(assetKinds))
	for i, k := range assetKinds {
		k.Extensions = append([]string(nil), k.Extensions...)
		out[i] = k
	}
	return out
}

// AllAssetKinds is the default setting: every kind on.
func AllAssetKinds() []AssetKind {
	out := make([]AssetKind, len(assetKinds))
	for i, k := range assetKinds {
		out[i] = k.Kind
	}
	return out
}

// RefusedAssetExtensions lists, for display, the extensions that are always
// refused whatever the setting says.
func RefusedAssetExtensions() []string {
	out := make([]string, 0, len(neverExtensions))
	for ext := range neverExtensions {
		out = append(out, ext)
	}
	sortStrings(out)
	return out
}

// ParseAssetKinds validates names from a caller and returns them in display
// order without duplicates. An unknown name, including a never-allowed kind,
// is an error.
func ParseAssetKinds(names []string) ([]AssetKind, error) {
	want := map[AssetKind]bool{}
	for _, n := range names {
		k := AssetKind(strings.ToLower(strings.TrimSpace(n)))
		if _, ok := kindLabel(k); !ok {
			return nil, fmt.Errorf("asset: %q is not a kind that can be turned on", n)
		}
		want[k] = true
	}
	var out []AssetKind
	for _, k := range assetKinds {
		if want[k.Kind] {
			out = append(out, k.Kind)
		}
	}
	return out, nil
}

func kindLabel(k AssetKind) (string, bool) {
	for _, i := range assetKinds {
		if i.Kind == k {
			return i.Label, true
		}
	}
	return "", false
}

// KindLabel is the display name of a kind, or the kind itself when unknown.
func KindLabel(k AssetKind) string {
	if l, ok := kindLabel(k); ok {
		return l
	}
	return string(k)
}

// MaxBytesFor is the hard size cap for a kind: images 10 MiB, text and
// Markdown 5 MiB, everything else 25 MiB.
func MaxBytesFor(k AssetKind) int64 {
	switch k {
	case KindImage:
		return MaxAssetBytes
	case KindText, KindMarkdown:
		return MaxTextBytes
	default:
		return MaxFileBytes
	}
}

// ValidateKindSize refuses a file over its kind's cap.
func ValidateKindSize(k AssetKind, size int) error {
	if size == 0 {
		return ErrAssetEmpty
	}
	if max := MaxBytesFor(k); int64(size) > max {
		return fmt.Errorf("%w: %s file of %d bytes (max %d)", ErrAssetTooLarge, KindLabel(k), size, max)
	}
	return nil
}

// KindOfContentType maps a stored content type back to its kind. An
// unrecognised type reports KindOffice, the catch-all.
func KindOfContentType(contentType string) AssetKind {
	ct := strings.ToLower(contentType)
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	switch {
	case strings.HasPrefix(ct, "image/"):
		return KindImage
	case strings.HasPrefix(ct, "video/"):
		return KindVideo
	case ct == "application/pdf":
		return KindPDF
	case ct == "application/msword", strings.HasSuffix(ct, "wordprocessingml.document"):
		return KindWord
	case ct == "text/plain":
		return KindText
	case ct == "text/markdown":
		return KindMarkdown
	default:
		return KindOffice
	}
}

// AssetSettings is the admin setting: which kinds of file are accepted.
type AssetSettings struct {
	AllowedKinds []AssetKind
	UpdatedBy    string
	UpdatedAt    time.Time
}

// DefaultAssetSettings has every kind on.
func DefaultAssetSettings() AssetSettings {
	return AssetSettings{AllowedKinds: AllAssetKinds()}
}

// Allows reports whether kind is on.
func (s AssetSettings) Allows(k AssetKind) bool {
	for _, a := range s.AllowedKinds {
		if a == k {
			return true
		}
	}
	return false
}
