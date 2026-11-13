// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

func tplRef(id uuid.UUID, secs ...domain.Section) *domain.TemplateRef {
	return &domain.TemplateRef{TemplateID: id, Sections: secs}
}

func TestClassifyPolicyChange(t *testing.T) {
	tid := uuid.New()
	purpose := domain.Section{Key: "purpose", Title: "Purpose", Required: true}
	scope := domain.Section{Key: "scope", Title: "Scope", Required: true}
	notes := domain.Section{Key: "notes", Title: "Notes"}
	base := makeContent(map[string]string{"purpose": "Keep records of access reviews carefully and share them with the team.", "scope": "All staff."})
	long := strings.Repeat("word ", 11)

	cases := []struct {
		name     string
		from, to domain.PolicyRevision
		want     domain.ChangeType
	}{
		{"identical content is a fix",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)}, domain.ChangeTypeFix},
		{"a small wording change is a fix",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Keep records of access reviews carefully and share them with the group.", "scope": "All staff."}), Template: tplRef(tid, purpose, scope)},
			domain.ChangeTypeFix},
		{"more than ten changed words is a minor",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Keep records of access reviews carefully and share them with the team. " + long, "scope": "All staff."}), Template: tplRef(tid, purpose, scope)},
			domain.ChangeTypeMinor},
		{"an optional section added is a minor",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope, notes)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Keep records of access reviews carefully and share them with the team.", "scope": "All staff.", "notes": "Background reading."}), Template: tplRef(tid, purpose, scope, notes)},
			domain.ChangeTypeMinor},
		{"a changed number is a major",
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Review access every quarter."}), Template: tplRef(tid, purpose)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Review access every 30 quarter."}), Template: tplRef(tid, purpose)}, domain.ChangeTypeMajor},
		{"must is an obligation word",
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Staff should review access."}), Template: tplRef(tid, purpose)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Staff must review access."}), Template: tplRef(tid, purpose)}, domain.ChangeTypeMajor},
		{"a deadline word is an obligation word",
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Report incidents promptly."}), Template: tplRef(tid, purpose)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Report incidents immediately."}), Template: tplRef(tid, purpose)}, domain.ChangeTypeMajor},
		{"an amount is an obligation word",
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Spend up to the limit."}), Template: tplRef(tid, purpose)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Spend up to the $limit."}), Template: tplRef(tid, purpose)}, domain.ChangeTypeMajor},
		{"obligation words that did not change do not count",
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Staff must review access within 30 days and log it."}), Template: tplRef(tid, purpose)},
			domain.PolicyRevision{ContentJSON: makeContent(map[string]string{"purpose": "Staff must review access within 30 days and record it."}), Template: tplRef(tid, purpose)}, domain.ChangeTypeFix},
		{"a required section added to the template is a major",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose)},
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)}, domain.ChangeTypeMajor},
		{"a required section removed from the template is a major",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose)}, domain.ChangeTypeMajor},
		{"an optional section added to the template alone is a minor",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope, notes)}, domain.ChangeTypeMinor},
		{"a different template is a major",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(uuid.New(), purpose, scope)}, domain.ChangeTypeMajor},
		{"going from a template to freeform is a major",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: base}, domain.ChangeTypeMajor},
		{"unreadable content is treated as a major",
			domain.PolicyRevision{ContentJSON: base, Template: tplRef(tid, purpose, scope)},
			domain.PolicyRevision{ContentJSON: "{not json", Template: tplRef(tid, purpose, scope)}, domain.ChangeTypeMajor},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := domain.ClassifyPolicyChange(c.from, c.to)
			if got.Type != c.want {
				t.Fatalf("type = %q (%v), want %q", got.Type, got.Reasons, c.want)
			}
			if got.Type == domain.ChangeTypeMajor && len(got.Reasons) == 0 {
				t.Fatal("a major change must name its reason")
			}
		})
	}
}

func TestClassifyTemplateChange(t *testing.T) {
	req := domain.Section{Key: "a", Title: "A", Required: true}
	opt := domain.Section{Key: "b", Title: "B"}
	boiler := func(s string) domain.Section {
		x := opt
		x.Blocks = []domain.Block{{Type: domain.BlockTypeBoilerplate, ContentJSON: s}}
		return x
	}
	cases := []struct {
		name     string
		from, to []domain.Section
		want     domain.ChangeType
	}{
		{"no change is a fix", []domain.Section{req}, []domain.Section{req}, domain.ChangeTypeFix},
		{"a required section added is a major", []domain.Section{req}, []domain.Section{req, {Key: "c", Title: "C", Required: true}}, domain.ChangeTypeMajor},
		{"a required section removed is a major", []domain.Section{req, opt}, []domain.Section{opt}, domain.ChangeTypeMajor},
		{"an optional section added is a minor", []domain.Section{req}, []domain.Section{req, opt}, domain.ChangeTypeMinor},
		{"an optional section removed is a minor", []domain.Section{req, opt}, []domain.Section{req}, domain.ChangeTypeMinor},
		{"a section turned required is a major", []domain.Section{req, opt}, []domain.Section{req, {Key: "b", Title: "B", Required: true}}, domain.ChangeTypeMajor},
		{"boilerplate text changed is a minor", []domain.Section{req, boiler("x")}, []domain.Section{req, boiler("y")}, domain.ChangeTypeMinor},
		{"a retitled section is a fix", []domain.Section{req, opt}, []domain.Section{req, {Key: "b", Title: "Bee"}}, domain.ChangeTypeFix},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := domain.ClassifyTemplateChange(c.from, c.to); got.Type != c.want {
				t.Fatalf("type = %q (%v), want %q", got.Type, got.Reasons, c.want)
			}
		})
	}
}
