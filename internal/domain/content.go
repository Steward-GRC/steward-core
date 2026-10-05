// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import "fmt"

// ContentValidator validates opaque Lexical JSON content against a
// TemplateVersion's section structure.
//
// The flat heading and paragraph vocabulary is the content model on every
// write path. Section-shaped validation would break diffing and AI indexing
// (both split on headings) and needs editor nodes the web app can't render,
// so strict structural checks apply only to content that already declares
// itself section-shaped.
type ContentValidator interface {
	Validate(contentJSON string, tv TemplateVersion) error
}

// NoopValidator accepts any non-empty content. It is the validator the server
// runs with, by design (see ContentValidator).
type NoopValidator struct{}

func (NoopValidator) Validate(contentJSON string, _ TemplateVersion) error {
	if contentJSON == "" {
		return fmt.Errorf("content must not be empty")
	}
	return nil
}
