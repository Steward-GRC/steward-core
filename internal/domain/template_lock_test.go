// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Mock tests: "holds under a category that inherits the template", "lets any template through
// once the lock is lifted" (steward-mock-server src/product/category-template-lock.test.ts).
func TestLockedTemplate(t *testing.T) {
	tpl, other := uuid.New(), uuid.New()
	root := domain.Category{ID: uuid.New(), DefaultTemplateID: tpl, DefaultTemplateLocked: true}
	child := domain.Category{ID: uuid.New(), ParentID: root.ID}

	cases := []struct {
		name  string
		chain []domain.Category
		want  uuid.UUID
	}{
		{"locked at the home category", []domain.Category{root}, tpl},
		{"locked above, inherited by a child", []domain.Category{child, root}, tpl},
		{"named but not locked", []domain.Category{{DefaultTemplateID: tpl}}, uuid.Nil},
		{"nothing pinned", []domain.Category{child}, uuid.Nil},
		{"a child's own template wins and is not locked", []domain.Category{{DefaultTemplateID: other}, root}, uuid.Nil},
		{"a child's own locked template wins", []domain.Category{{DefaultTemplateID: other, DefaultTemplateLocked: true}, root}, other},
		{"freeform below stops the lock", []domain.Category{{DefaultTemplateNone: true}, root}, uuid.Nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.LockedTemplate(c.chain); got != c.want {
				t.Fatalf("LockedTemplate = %v, want %v", got, c.want)
			}
		})
	}
}

func TestTemplateLockViolation(t *testing.T) {
	tpl, other := uuid.New(), uuid.New()
	locked := []domain.Category{{DefaultTemplateID: tpl, DefaultTemplateLocked: true}}
	open := []domain.Category{{DefaultTemplateID: tpl}}

	cases := []struct {
		name      string
		chain     []domain.Category
		requested uuid.UUID
		none      bool
		want      uuid.UUID
	}{
		{"inherit follows the locked template", locked, uuid.Nil, false, uuid.Nil},
		{"the locked template itself", locked, tpl, false, uuid.Nil},
		{"another template", locked, other, false, tpl},
		{"freeform", locked, uuid.Nil, true, tpl},
		{"no lock allows another template", open, other, false, uuid.Nil},
		{"no lock allows freeform", open, uuid.Nil, true, uuid.Nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.TemplateLockViolation(c.chain, c.requested, c.none); got != c.want {
				t.Fatalf("TemplateLockViolation = %v, want %v", got, c.want)
			}
		})
	}
}
