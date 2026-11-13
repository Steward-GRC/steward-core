// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"math"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
)

// toInt32 narrows a domain int (version numbers, order indexes, levels) to
// the API's int32, clamping so a corrupt value can't wrap negative. Keeping
// every narrowing here gives gosec's G115 one audited place.
func toInt32(n int) int32 {
	switch {
	case n < 0:
		return 0
	case n > math.MaxInt32:
		return math.MaxInt32
	default:
		return int32(n) //nosec G115 -- bounded by the guards above
	}
}

// toUint32 is toInt32's unsigned counterpart, for count fields.
func toUint32(n int) uint32 {
	switch {
	case n < 0:
		return 0
	case n > math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(n) //nosec G115 -- bounded by the guards above
	}
}

// changeTypeToProto maps a change type to the wire enum; unset maps to UNSPECIFIED.
func changeTypeToProto(c domain.ChangeType) corev1.ChangeType {
	switch c {
	case domain.ChangeTypeMajor:
		return corev1.ChangeType_CHANGE_TYPE_MAJOR
	case domain.ChangeTypeMinor:
		return corev1.ChangeType_CHANGE_TYPE_MINOR
	case domain.ChangeTypeFix:
		return corev1.ChangeType_CHANGE_TYPE_FIX
	default:
		return corev1.ChangeType_CHANGE_TYPE_UNSPECIFIED
	}
}

// changeTypeFromProto maps the wire enum to a change type; UNSPECIFIED maps to "".
func changeTypeFromProto(c corev1.ChangeType) domain.ChangeType {
	switch c {
	case corev1.ChangeType_CHANGE_TYPE_MAJOR:
		return domain.ChangeTypeMajor
	case corev1.ChangeType_CHANGE_TYPE_MINOR:
		return domain.ChangeTypeMinor
	case corev1.ChangeType_CHANGE_TYPE_FIX:
		return domain.ChangeTypeFix
	default:
		return ""
	}
}

// semverToProto renders a semantic version, or nil for the zero version so a reader can tell
// "no number" from 0.0.0.
func semverToProto(v domain.SemVer) *corev1.SemanticVersion {
	if v.IsZero() {
		return nil
	}
	return &corev1.SemanticVersion{Major: toInt32(v.Major), Minor: toInt32(v.Minor), Fix: toInt32(v.Fix), Text: v.String()}
}
