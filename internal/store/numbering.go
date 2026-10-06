// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// docNumberPrefix maps a document type to its number prefix: POL for
// policies, PRC for procedures. An unknown type renders as a policy.
func docNumberPrefix(docType domain.DocumentType) string {
	if docType == domain.DocumentTypeProcedure {
		return "PRC"
	}
	return "POL"
}

// docTypeFromPrefix is the inverse of docNumberPrefix, for parsing a rendered
// number back apart. ok is false for anything else.
func docTypeFromPrefix(prefix string) (domain.DocumentType, bool) {
	switch prefix {
	case "POL":
		return domain.DocumentTypePolicy, true
	case "PRC":
		return domain.DocumentTypeProcedure, true
	default:
		return "", false
	}
}

// renderDocNumber formats <PREFIX>-<code>-<zero-padded sequence>. The number
// is derived on every read from the category's current code, never stored, so
// a code change reaches every document in the category.
func renderDocNumber(docType domain.DocumentType, code string, sequence int) string {
	return fmt.Sprintf("%s-%s-%06d", docNumberPrefix(docType), code, sequence)
}

// codeMaxLen caps a derived category code. A disambiguation suffix may push a
// stored code slightly past it.
const codeMaxLen = 6

// PolicyNumberCode derives the candidate code segment from a category slug:
// uppercase alphanumerics, capped at codeMaxLen, "GEN" for a slug with none.
// Two slugs can share a candidate; NextFreeCode makes the stored code unique.
func PolicyNumberCode(slug string) string {
	var b strings.Builder
	for _, r := range slug {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - ('a' - 'A'))
		case (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
		}
	}
	code := b.String()
	if code == "" {
		return "GEN"
	}
	if len(code) > codeMaxLen {
		code = code[:codeMaxLen]
	}
	return code
}

// NextFreeCode returns candidate if it isn't in taken, otherwise the first
// free "candidate<N>" from N=2. The unique constraint on categories.code is
// only a backstop.
func NextFreeCode(candidate string, taken map[string]bool) string {
	if !taken[candidate] {
		return candidate
	}
	for n := 2; ; n++ {
		alt := candidate + strconv.Itoa(n)
		if !taken[alt] {
			return alt
		}
	}
}
