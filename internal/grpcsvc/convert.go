// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import "math"

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
