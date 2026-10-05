// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package validate_test

import (
	"encoding/json"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/validate"
)

// TemplateSection mirrors the relevant fields from TemplateVersion.sections.
func tmplSections() []validate.TemplateSection {
	return []validate.TemplateSection{
		{
			Key:   "purpose",
			Order: 0,
			Blocks: []validate.TemplateBlock{
				{Type: "boilerplate", Content: "This policy applies to all staff."},
				{Type: "editable", RegionKey: "purpose-body"},
			},
		},
		{
			Key:   "scope",
			Order: 1,
			Blocks: []validate.TemplateBlock{
				{Type: "editable", RegionKey: "scope-body"},
			},
		},
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func validDoc() map[string]any {
	return map[string]any{
		"root": map[string]any{
			"type": "root",
			"children": []any{
				map[string]any{
					"type":       "policy-section",
					"sectionKey": "purpose",
					"order":      0,
					"children": []any{
						map[string]any{"type": "policy-boilerplate", "content": "This policy applies to all staff."},
						map[string]any{"type": "policy-editable-region", "regionKey": "purpose-body", "children": []any{}},
					},
				},
				map[string]any{
					"type":       "policy-section",
					"sectionKey": "scope",
					"order":      1,
					"children": []any{
						map[string]any{"type": "policy-editable-region", "regionKey": "scope-body", "children": []any{}},
					},
				},
			},
		},
	}
}

func TestValidate_ValidDoc(t *testing.T) {
	err := validate.Validate(mustJSON(validDoc()), tmplSections())
	if err != nil {
		t.Fatalf("expected valid, got: %v", err)
	}
}

func TestValidate_WrongSectionCount(t *testing.T) {
	doc := validDoc()
	doc["root"].(map[string]any)["children"] = []any{} // no sections
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for wrong section count")
	}
}

func TestValidate_WrongSectionKey(t *testing.T) {
	doc := validDoc()
	children := doc["root"].(map[string]any)["children"].([]any)
	children[0].(map[string]any)["sectionKey"] = "wrong-key"
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for wrong sectionKey")
	}
}

func TestValidate_WrongSectionOrder(t *testing.T) {
	doc := validDoc()
	children := doc["root"].(map[string]any)["children"].([]any)
	// swap order values
	children[0].(map[string]any)["order"] = 1
	children[1].(map[string]any)["order"] = 0
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for wrong section order")
	}
}

func TestValidate_MutatedBoilerplate(t *testing.T) {
	doc := validDoc()
	children := doc["root"].(map[string]any)["children"].([]any)
	sectionChildren := children[0].(map[string]any)["children"].([]any)
	sectionChildren[0].(map[string]any)["content"] = "TAMPERED"
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for mutated boilerplate")
	}
}

func TestValidate_UnknownNodeAtRoot(t *testing.T) {
	doc := validDoc()
	children := doc["root"].(map[string]any)["children"].([]any)
	doc["root"].(map[string]any)["children"] = append(children,
		map[string]any{"type": "unknown-node"})
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for unknown node at root")
	}
}

func TestValidate_SpuriousEditableRegion(t *testing.T) {
	// The doc has an extra editable region whose regionKey does not match any
	// template block — the validator must reject it.
	doc := validDoc()
	children := doc["root"].(map[string]any)["children"].([]any)
	sectionChildren := children[0].(map[string]any)["children"].([]any)
	children[0].(map[string]any)["children"] = append(sectionChildren,
		map[string]any{"type": "policy-editable-region", "regionKey": "spurious-region", "children": []any{}})
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for spurious editable region not in template")
	}
}

func TestValidate_EditableRegionKeyMismatch(t *testing.T) {
	// The doc has the right number of editable regions but one has a wrong regionKey.
	doc := validDoc()
	children := doc["root"].(map[string]any)["children"].([]any)
	sectionChildren := children[0].(map[string]any)["children"].([]any)
	sectionChildren[1].(map[string]any)["regionKey"] = "wrong-key"
	err := validate.Validate(mustJSON(doc), tmplSections())
	if err == nil {
		t.Fatal("expected error for regionKey mismatch")
	}
}

func TestValidate_EmptyJSON(t *testing.T) {
	err := validate.Validate("", tmplSections())
	if err == nil {
		t.Fatal("expected error for empty JSON")
	}
}

func TestValidate_MalformedJSON(t *testing.T) {
	err := validate.Validate("{not json}", tmplSections())
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}
