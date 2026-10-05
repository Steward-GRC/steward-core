// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxAssetBytes is above gRPC's default 4 MiB message limit, so the server
// raises its limits for one image to fit in one message. AssetKeyPrefix keeps
// editor images apart from other objects in a shared bucket. AssetURLPrefix is
// the gateway path the editor embeds.
const (
	MaxAssetBytes  = 10 << 20 // 10 MiB
	AssetKeyPrefix = "editor-assets/"
	AssetURLPrefix = "/api/assets/"
)

// Asset is a stored durable editor asset. The bytes live in object storage
// under StorageKey; this struct is the persisted metadata row.
type Asset struct {
	ID              uuid.UUID
	StorageKey      string
	ContentType     string
	SizeBytes       int64
	Filename        string
	CreatedByUserID string
	CreatedAt       time.Time
}

// URL returns the gateway path the editor embeds for the asset.
func (a Asset) URL() string { return AssetURLPrefix + a.ID.String() }

// StorageKeyFor returns the object-storage key for an asset id.
func StorageKeyFor(id uuid.UUID) string { return AssetKeyPrefix + id.String() }

// allowedImageTypes is the MIME allowlist for editor image assets. SVG is
// deliberately excluded: served inline it is a stored-XSS vector, and the
// editor only produces raster images. Every entry here is also a type Go's
// http.DetectContentType can sniff, so the declared and sniffed types can be
// cross-checked.
var allowedImageTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// AllowedAssetContentTypes returns the sorted allowlist for error messages and
// tests.
func AllowedAssetContentTypes() []string {
	return []string{"image/gif", "image/jpeg", "image/png", "image/webp"}
}

// The asset validation errors.
var (
	ErrAssetEmpty           = errors.New("asset: empty upload")
	ErrAssetTooLarge        = errors.New("asset: exceeds maximum size")
	ErrAssetUnsupportedType = errors.New("asset: unsupported content type")
)

// DetectAssetContentType sniffs the bytes and returns the image type when it
// is allowed. The stored type comes from the bytes, never the declared one,
// so a mislabelled upload can't smuggle a non-image past the allowlist.
func DetectAssetContentType(data []byte) (string, error) {
	if len(data) == 0 {
		return "", ErrAssetEmpty
	}
	ct := http.DetectContentType(data)
	if i := strings.IndexByte(ct, ';'); i >= 0 { // strip "; charset=..."
		ct = strings.TrimSpace(ct[:i])
	}
	if !allowedImageTypes[ct] {
		return "", fmt.Errorf("%w: %q (allowed: %s)", ErrAssetUnsupportedType, ct, strings.Join(AllowedAssetContentTypes(), ", "))
	}
	return ct, nil
}

// ValidateAssetSize refuses empty and oversized uploads.
func ValidateAssetSize(size int) error {
	if size == 0 {
		return ErrAssetEmpty
	}
	if size > MaxAssetBytes {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrAssetTooLarge, size, MaxAssetBytes)
	}
	return nil
}

// SanitizeFilename reduces a client filename to a short, path-free label,
// kept as metadata only and never used as the storage key.
func SanitizeFilename(name string) string {
	name = strings.TrimSpace(name)
	if slash := strings.LastIndexAny(name, `/\`); slash >= 0 {
		name = name[slash+1:]
	}
	name = strings.TrimSpace(name)
	const maxLen = 255
	if len(name) > maxLen {
		name = name[:maxLen]
	}
	return name
}
