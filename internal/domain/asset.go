// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// MaxAssetBytes is the hard cap for an image; MaxFileBytes is the separate,
// larger cap for the other kinds (text and Markdown are capped lower, see
// MaxBytesFor). Both are above gRPC's default 4 MiB message limit, so the
// server raises its limits for one file to fit in one message. AssetKeyPrefix
// keeps editor assets apart from other objects in a shared bucket.
// AssetURLPrefix is the gateway path the editor embeds.
const (
	MaxAssetBytes  = 10 << 20 // 10 MiB, images
	MaxFileBytes   = 25 << 20 // 25 MiB, documents and video
	MaxTextBytes   = 5 << 20  // 5 MiB, text and Markdown
	AssetKeyPrefix = "editor-assets/"
	AssetURLPrefix = "/api/assets/"
)

// Asset is a stored durable editor asset: an image or a file. The bytes live in object storage
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

// Class is "image" or "file", from the stored content type.
func (a Asset) Class() string { return AssetClassOf(a.ContentType) }

// URL returns the gateway path the editor embeds for the asset.
func (a Asset) URL() string { return AssetURLPrefix + a.ID.String() }

// StorageKeyFor returns the object-storage key for an asset id.
func StorageKeyFor(id uuid.UUID) string { return AssetKeyPrefix + id.String() }

// The asset validation errors.
var (
	ErrAssetEmpty           = errors.New("asset: empty upload")
	ErrAssetTooLarge        = errors.New("asset: exceeds maximum size")
	ErrAssetUnsupportedType = errors.New("asset: unsupported content type")
	// ErrAssetMismatch is a name or declared type that disagrees with the bytes.
	ErrAssetMismatch = errors.New("asset: contents do not match")
	// ErrAssetKindDisabled is a kind the admin setting has switched off.
	ErrAssetKindDisabled = errors.New("asset: file kind is turned off")
)

// ValidateAssetSize refuses empty uploads and anything over the ceiling for
// the largest kind. The per-kind cap is checked once the kind is known
// (ValidateKindSize).
func ValidateAssetSize(size int) error {
	if size == 0 {
		return ErrAssetEmpty
	}
	if size > MaxFileBytes {
		return fmt.Errorf("%w: %d bytes (max %d)", ErrAssetTooLarge, size, MaxFileBytes)
	}
	return nil
}

// AssetClassOf says whether a stored content type is served inline ("image")
// or only as a download ("file"). The gateway sends a "file" with
// Content-Disposition: attachment and nosniff, never inline.
func AssetClassOf(contentType string) string {
	if strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return AssetClassImage
	}
	return AssetClassFile
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
