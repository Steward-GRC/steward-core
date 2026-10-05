// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package validate_test

import (
	"encoding/json"
	"testing"

	"github.com/Steward-GRC/steward-core/internal/validate"
)

// devTemplateSections mirrors the REAL dev template TPL-001
// (11d0189d-54af-4a35-8c7f-064330b14824) as stored in core's template_versions
// row: four sections, and — because domain.Block has no RegionKey field and
// templateSectionsForValidate maps an editable block's ContentJSON to RegionKey —
// every editable block carries an EMPTY region key. Pinned here so the fixture
// keeps matching the shape the product actually stores.
func devTemplateSections() []validate.TemplateSection {
	return []validate.TemplateSection{
		{Key: "purpose", Order: 0, Blocks: []validate.TemplateBlock{
			{Type: "boilerplate", Content: "This policy establishes the requirements for ..."},
			{Type: "editable", RegionKey: ""},
		}},
		{Key: "scope", Order: 0, Blocks: []validate.TemplateBlock{
			{Type: "editable", RegionKey: ""},
		}},
		{Key: "policy-statement", Order: 0, Blocks: []validate.TemplateBlock{
			{Type: "editable", RegionKey: ""},
		}},
		{Key: "enforcement", Order: 0, Blocks: []validate.TemplateBlock{
			{Type: "boilerplate", Content: "Violations may result in disciplinary action."},
		}},
	}
}

// uiHeadingParagraphDoc is what the editor actually serializes: a flat root of
// heading/paragraph nodes, one heading+paragraph pair per template section.
// This is the exact shape apps/staff/src/authoring.ts scaffoldFromTemplate
// produces and that PolicyService.SaveDraft accepts.
func uiHeadingParagraphDoc() map[string]any {
	heading := func(text string) map[string]any {
		return map[string]any{
			"type": "heading", "tag": "h1", "version": 1,
			"format": "", "indent": 0, "direction": "ltr",
			"children": []any{map[string]any{
				"type": "text", "text": text, "version": 1,
				"detail": 0, "format": 0, "mode": "normal", "style": "",
			}},
		}
	}
	paragraph := func(text string) map[string]any {
		return map[string]any{
			"type": "paragraph", "version": 1,
			"format": "", "indent": 0, "direction": "ltr",
			"children": []any{map[string]any{
				"type": "text", "text": text, "version": 1,
				"detail": 0, "format": 0, "mode": "normal", "style": "",
			}},
		}
	}
	return map[string]any{
		"root": map[string]any{
			"type": "root", "version": 1, "format": "", "indent": 0,
			"children": []any{
				heading("Purpose"), paragraph("Type here to collaborate."),
				heading("Scope"), paragraph("Applies to all staff."),
				heading("Policy Statement"), paragraph("Do the right thing."),
				heading("Enforcement"), paragraph("Violations are handled by HR."),
			},
		},
	}
}

// sectionShapedDevDoc is the fully template-structured vocabulary for the dev
// template: policy-section roots with policy-boilerplate / policy-editable-region
// children. This is what the strict validator demands and what the editor cannot
// currently produce (the web editor's node registry is fixed at 15 types).
func sectionShapedDevDoc() map[string]any {
	editable := func() map[string]any {
		return map[string]any{
			"type": "policy-editable-region", "regionKey": "",
			"children": []any{map[string]any{
				"type": "paragraph",
				"children": []any{map[string]any{
					"type": "text", "text": "Author text.",
				}},
			}},
		}
	}
	return map[string]any{
		"root": map[string]any{
			"type": "root",
			"children": []any{
				map[string]any{"type": "policy-section", "sectionKey": "purpose", "order": 0, "children": []any{
					map[string]any{"type": "policy-boilerplate", "content": "This policy establishes the requirements for ..."},
					editable(),
				}},
				map[string]any{"type": "policy-section", "sectionKey": "scope", "order": 0, "children": []any{editable()}},
				map[string]any{"type": "policy-section", "sectionKey": "policy-statement", "order": 0, "children": []any{editable()}},
				map[string]any{"type": "policy-section", "sectionKey": "enforcement", "order": 0, "children": []any{
					map[string]any{"type": "policy-boilerplate", "content": "Violations may result in disciplinary action."},
				}},
			},
		},
	}
}

// --- ACCEPTED shapes -------------------------------------------------------

// The strict validator rejected this with `root has 8 children; template
// requires 4 sections`, so no collaborative edit could persist.
func TestValidateDraft_AcceptsUIHeadingParagraphShape(t *testing.T) {
	if err := validate.ValidateDraft(mustJSON(uiHeadingParagraphDoc()), devTemplateSections()); err != nil {
		t.Fatalf("UI heading/paragraph content must be accepted on the draft path, got: %v", err)
	}
}

// The other half of the original bug report: 4 root children of the WRONG node
// type. The strict validator failed this with a node-type error
// (`root[0]: expected policy-section, got "heading"`), proving the mismatch was
// never about the child count.
func TestValidateDraft_AcceptsFourHeadingChildren(t *testing.T) {
	doc := uiHeadingParagraphDoc()
	root := doc["root"].(map[string]any)
	root["children"] = root["children"].([]any)[:4]
	if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err != nil {
		t.Fatalf("four heading/paragraph children must be accepted, got: %v", err)
	}
}

// Section-shaped content keeps getting the FULL strict check, so the day the
// editor gains real section nodes it is strictly validated with no change here.
func TestValidateDraft_AcceptsSectionShapedContent(t *testing.T) {
	if err := validate.ValidateDraft(mustJSON(sectionShapedDevDoc()), devTemplateSections()); err != nil {
		t.Fatalf("section-shaped content must still be accepted, got: %v", err)
	}
}

// A single empty paragraph is Lexical's canonical empty document and is what
// authoring.ts normalizeEditorState produces for a brand-new draft. It must be
// storable; the caller's text-preservation guard is what stops it overwriting a
// draft that already has text (see TestHasTextContent_* below).
func TestValidateDraft_AcceptsCanonicalEmptyDocument(t *testing.T) {
	doc := map[string]any{"root": map[string]any{
		"type": "root",
		"children": []any{map[string]any{
			"type": "paragraph", "children": []any{},
		}},
	}}
	if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err != nil {
		t.Fatalf("canonical empty document must be accepted, got: %v", err)
	}
}

// Rich content the editor's 15-node registry can produce (lists, tables, links,
// images, code) is not a template concern and must pass the draft path.
func TestValidateDraft_AcceptsRichEditorNodes(t *testing.T) {
	for _, nodeType := range []string{
		"heading", "paragraph", "quote", "list", "listitem", "link", "image",
		"table", "tablerow", "tablecell", "code", "code-highlight",
		"horizontalrule", "embed", "footnote", "mention",
	} {
		doc := map[string]any{"root": map[string]any{
			"type":     "root",
			"children": []any{map[string]any{"type": nodeType}},
		}}
		if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err != nil {
			t.Errorf("node type %q must be accepted on the draft path, got: %v", nodeType, err)
		}
	}
}

// A draft may be pinned to a template whose section list is empty, and content
// must not then be forced to be childless.
func TestValidateDraft_AcceptsContentWhenTemplateHasNoSections(t *testing.T) {
	if err := validate.ValidateDraft(mustJSON(uiHeadingParagraphDoc()), nil); err != nil {
		t.Fatalf("content must be accepted against a section-less template, got: %v", err)
	}
}

// --- REJECTED shapes -------------------------------------------------------

func TestValidateDraft_RejectsEmptyString(t *testing.T) {
	if err := validate.ValidateDraft("", devTemplateSections()); err == nil {
		t.Fatal("empty content JSON must be rejected")
	}
}

func TestValidateDraft_RejectsMalformedJSON(t *testing.T) {
	for name, body := range map[string]string{
		"truncated":    `{"root":{"children":[`,
		"not-json":     `this is not json`,
		"null":         `null`,
		"array":        `[]`,
		"scalar":       `42`,
		"empty-object": `{}`,
	} {
		if err := validate.ValidateDraft(body, devTemplateSections()); err == nil {
			t.Errorf("%s: must be rejected", name)
		}
	}
}

// The anti-corruption rule. A root child that is not an object, or that has no
// usable node type, is the signature of a mangled editor state — exactly what a
// Lexical instance produces when it cannot parse its input. Rejecting it here
// mirrors @policy/ui's renderableEditorState admission predicate, so the server
// refuses to store what the renderer would refuse to display.
func TestValidateDraft_RejectsMangledRootChildren(t *testing.T) {
	cases := map[string]any{
		"missing type":    map[string]any{"children": []any{}},
		"empty type":      map[string]any{"type": ""},
		"non-string":      map[string]any{"type": 7},
		"null type":       map[string]any{"type": nil},
		"child is null":   nil,
		"child is array":  []any{},
		"child is string": "paragraph",
		"child is number": 3,
	}
	for name, child := range cases {
		doc := map[string]any{"root": map[string]any{
			"type":     "root",
			"children": []any{child},
		}}
		if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
			t.Errorf("%s: mangled root child must be rejected", name)
		}
	}
}

// An emptied root is the signature of an editor that destroyed its state, and
// would land a blank checkpoint on a real draft. The editor's own normalizer guarantees
// at least one child, so this can only arrive from a broken client.
func TestValidateDraft_RejectsEmptiedRoot(t *testing.T) {
	for name, body := range map[string]string{
		"empty children array": `{"root":{"type":"root","children":[]}}`,
		"no children key":      `{"root":{"type":"root"}}`,
		"null children":        `{"root":{"type":"root","children":null}}`,
	} {
		if err := validate.ValidateDraft(body, devTemplateSections()); err == nil {
			t.Errorf("%s: an emptied root must be rejected", name)
		}
	}
}

func TestValidateDraft_RejectsNonArrayChildren(t *testing.T) {
	doc := map[string]any{"root": map[string]any{
		"type":     "root",
		"children": "nope",
	}}
	if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
		t.Fatal("non-array root.children must be rejected")
	}
}

// Once content declares itself section-shaped it is held to the strict contract:
// a wrong sectionKey, a wrong count, or edited boilerplate is rejected. This is
// what keeps ValidateDraft shape-AWARE rather than merely lax.
func TestValidateDraft_SectionShapedContentStillStrictlyValidated(t *testing.T) {
	t.Run("wrong section count", func(t *testing.T) {
		doc := sectionShapedDevDoc()
		root := doc["root"].(map[string]any)
		root["children"] = root["children"].([]any)[:2]
		if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
			t.Fatal("section-shaped content with too few sections must be rejected")
		}
	})

	t.Run("wrong section key", func(t *testing.T) {
		doc := sectionShapedDevDoc()
		root := doc["root"].(map[string]any)
		root["children"].([]any)[0].(map[string]any)["sectionKey"] = "not-a-template-key"
		if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
			t.Fatal("section-shaped content with an unknown sectionKey must be rejected")
		}
	})

	t.Run("boilerplate edited", func(t *testing.T) {
		doc := sectionShapedDevDoc()
		root := doc["root"].(map[string]any)
		sec := root["children"].([]any)[0].(map[string]any)
		sec["children"].([]any)[0].(map[string]any)["content"] = "I rewrote the locked text."
		if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
			t.Fatal("edited boilerplate must be rejected")
		}
	})

	t.Run("unknown node inside a section", func(t *testing.T) {
		doc := sectionShapedDevDoc()
		root := doc["root"].(map[string]any)
		sec := root["children"].([]any)[1].(map[string]any)
		sec["children"] = []any{map[string]any{"type": "heading"}}
		if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
			t.Fatal("a non-section node inside a policy-section must be rejected")
		}
	})
}

// Mixed content that merely STARTS with a section node is treated as
// section-shaped and therefore strictly rejected, rather than smuggling
// half-structured content through the lax branch.
// TestValidateDraft_SectionShapedContentWithNoTemplateIsRejected pins the
// free-form boundary. A free-form draft passes no sections, and
// free-form content — the flat heading/paragraph vocabulary the editor
// actually emits — is validated on envelope invariants. But section-shaped
// content still routes to the strict validator, and with no template there is
// nothing to check it against, so it is refused rather than waved through.
// Free-form must not become a way to submit unvalidated section structure.
func TestValidateDraft_SectionShapedContentWithNoTemplateIsRejected(t *testing.T) {
	err := validate.ValidateDraft(mustJSON(sectionShapedDevDoc()), nil)
	if err == nil {
		t.Fatal("expected section-shaped content with no template to be rejected, got nil")
	}
}

// TestValidateDraft_FreeFormContentPassesEnvelopeInvariants is the positive
// half: with no template at all, the vocabulary the editor emits is accepted.
func TestValidateDraft_FreeFormContentPassesEnvelopeInvariants(t *testing.T) {
	if err := validate.ValidateDraft(mustJSON(uiHeadingParagraphDoc()), nil); err != nil {
		t.Fatalf("free-form content rejected with no template: %v", err)
	}
}

// TestValidateDraft_FreeFormStillRejectsEmptiedRoot confirms the
// anti-corruption invariants are not skipped when there is no template.
func TestValidateDraft_FreeFormStillRejectsEmptiedRoot(t *testing.T) {
	if err := validate.ValidateDraft(`{"root":{"type":"root","children":[]}}`, nil); err == nil {
		t.Fatal("expected an emptied root to be rejected with no template, got nil")
	}
}

func TestValidateDraft_RejectsPartiallySectionShapedContent(t *testing.T) {
	doc := map[string]any{"root": map[string]any{
		"type": "root",
		"children": []any{
			map[string]any{"type": "policy-section", "sectionKey": "purpose", "order": 0, "children": []any{}},
			map[string]any{"type": "paragraph"},
		},
	}}
	if err := validate.ValidateDraft(mustJSON(doc), devTemplateSections()); err == nil {
		t.Fatal("content that starts with a policy-section must be strictly validated")
	}
}

// --- HasTextContent (the text-preservation guard) --------------------------

func TestHasTextContent_TrueForAuthoredContent(t *testing.T) {
	if !validate.HasTextContent(mustJSON(uiHeadingParagraphDoc())) {
		t.Fatal("authored heading/paragraph content must report text")
	}
	if !validate.HasTextContent(mustJSON(sectionShapedDevDoc())) {
		t.Fatal("authored section-shaped content must report text")
	}
}

// These are the states a broken editor produces, and the ones that must never
// be allowed to replace a draft that has text.
func TestHasTextContent_FalseForEmptyStates(t *testing.T) {
	cases := map[string]string{
		"empty string":     "",
		"malformed":        `{"root":`,
		"no root":          `{}`,
		"root no children": `{"root":{"type":"root"}}`,
		"emptied root":     `{"root":{"type":"root","children":[]}}`,
		"canonical empty":  `{"root":{"type":"root","children":[{"type":"paragraph","children":[]}]}}`,
		"blank text":       `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"   "}]}]}}`,
		"newlines only":    `{"root":{"type":"root","children":[{"type":"paragraph","children":[{"type":"text","text":"\n\t\r"}]}]}}`,
		// NBSP + zero-width space, which is what a browser can leave behind
		// in an "emptied" paragraph.
		"nbsp and zero-width only": "{\"root\":{\"type\":\"root\",\"children\":" +
			"[{\"type\":\"paragraph\",\"children\":" +
			"[{\"type\":\"text\",\"text\":\"\u00a0\u200b\"}]}]}}",
	}
	for name, body := range cases {
		if validate.HasTextContent(body) {
			t.Errorf("%s: must report NO text content", name)
		}
	}
}

func TestHasTextContent_FindsDeeplyNestedText(t *testing.T) {
	// text nested below section -> editable region -> list -> listitem -> text
	body := `{"root":{"type":"root","children":[{"type":"policy-section","children":[
	  {"type":"policy-editable-region","children":[
	    {"type":"list","children":[{"type":"listitem","children":[
	      {"type":"text","text":"nested"}]}]}]}]}]}}`
	if !validate.HasTextContent(body) {
		t.Fatal("deeply nested text must be found")
	}
}

// A pathologically nested document must not blow the stack; it simply reports
// no text beyond the depth bound.
func TestHasTextContent_BoundedDepth(t *testing.T) {
	body := `{"root":{"type":"root","children":`
	// Comfortably above maxTextScanDepth (64) while staying inside
	// encoding/json's own 10000-level nesting cap (each level is an array
	// plus an object, so this is 400 levels of JSON nesting).
	const depth = 200
	for range depth {
		body += `[{"type":"paragraph","children":`
	}
	body += `[{"type":"text","text":"deep"}]`
	for range depth {
		body += `}]`
	}
	body += `}}`
	if !json.Valid([]byte(body)) {
		t.Fatal("test fixture is not valid JSON")
	}
	if validate.HasTextContent(body) {
		t.Fatal("text below the depth bound must not be reported")
	}
}
