// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

// FixWordLimit is the most words a change may add or drop and still be a Fix. A change of more
// words, or one that adds or drops a section, is a Minor at least.
const FixWordLimit = 10

// Classification is the change type core suggests for a version, with the reasons, so a
// reviewer can see why a change reads as Major.
type Classification struct {
	Type    ChangeType
	Reasons []string
}

func (c *Classification) raise(t ChangeType, reason string) {
	if t.Rank() > c.Type.Rank() {
		c.Type = t
	}
	if reason != "" {
		c.Reasons = append(c.Reasons, reason)
	}
}

// TemplateRef is the template version a document is pinned to, as far as the classifier reads
// it. A nil *TemplateRef is a freeform document.
type TemplateRef struct {
	TemplateID uuid.UUID
	Sections   []Section
}

// PolicyRevision is one side of a comparison: the stored content and the template pin.
type PolicyRevision struct {
	ContentJSON string
	Template    *TemplateRef
}

// obligationWords are the words that carry an obligation, a permission or a deadline. A change
// that adds or drops one of them changes what readers must do.
var obligationWords = map[string]bool{
	"must": true, "shall": true, "may": true, "should": true, "required": true, "requires": true,
	"prohibited": true, "mandatory": true, "not": true, "never": true, "cannot": true, "no": true,
	"within": true, "immediately": true, "deadline": true, "annually": true, "quarterly": true,
	"monthly": true, "weekly": true, "daily": true, "hourly": true,
	"day": true, "days": true, "week": true, "weeks": true, "month": true, "months": true,
	"year": true, "years": true, "hour": true, "hours": true,
	"january": true, "february": true, "march": true, "april": true, "june": true, "july": true,
	"august": true, "september": true, "october": true, "november": true, "december": true,
}

// isObligationWord reports whether w, a word from a diff, is a number or date, an amount, or a
// word that sets an obligation or a deadline. Case and edge punctuation are ignored.
func isObligationWord(w string) bool {
	if strings.ContainsAny(w, "$€£%") {
		return true
	}
	for _, r := range w {
		if unicode.IsDigit(r) {
			return true
		}
	}
	return obligationWords[strings.ToLower(strings.TrimFunc(w, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}))]
}

func requiredKeys(secs []Section) map[string]bool {
	out := map[string]bool{}
	for _, s := range secs {
		if s.Required {
			out[s.Key] = true
		}
	}
	return out
}

func sectionKeys(secs []Section) map[string]bool {
	out := map[string]bool{}
	for _, s := range secs {
		out[s.Key] = true
	}
	return out
}

// classifySections compares two section lists of one template and raises c: a required section
// added, dropped or turned required is Major; any other section added or dropped, or changed
// boilerplate text, is Minor. A retitle or reorder alone changes nothing here.
func classifySections(c *Classification, from, to []Section) {
	fromKeys, toKeys := sectionKeys(from), sectionKeys(to)
	fromReq, toReq := requiredKeys(from), requiredKeys(to)
	for k := range toKeys {
		switch {
		case !fromKeys[k] && toReq[k]:
			c.raise(ChangeTypeMajor, "required section added: "+k)
		case !fromKeys[k]:
			c.raise(ChangeTypeMinor, "optional section added: "+k)
		case toReq[k] && !fromReq[k]:
			c.raise(ChangeTypeMajor, "section turned required: "+k)
		}
	}
	for k := range fromKeys {
		switch {
		case !toKeys[k] && fromReq[k]:
			c.raise(ChangeTypeMajor, "required section removed: "+k)
		case !toKeys[k]:
			c.raise(ChangeTypeMinor, "optional section removed: "+k)
		case fromReq[k] && !toReq[k]:
			c.raise(ChangeTypeMinor, "section no longer required: "+k)
		}
	}
	fromBoiler := map[string]string{}
	for _, s := range from {
		fromBoiler[s.Key] = boilerplateText(s)
	}
	for _, s := range to {
		if old, ok := fromBoiler[s.Key]; ok && old != boilerplateText(s) {
			c.raise(ChangeTypeMinor, "boilerplate text changed: "+s.Key)
		}
	}
}

func boilerplateText(s Section) string {
	var sb strings.Builder
	for _, b := range s.Blocks {
		if b.Type == BlockTypeBoilerplate {
			sb.WriteString(b.ContentJSON)
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// ClassifyTemplateChange suggests the change type of a template version against the one before
// it. A required section added, dropped or turned required is Major, because authors owe
// something new; an optional section added or dropped, or changed boilerplate, is Minor; the
// rest (titles, order, heading level) is a Fix.
func ClassifyTemplateChange(from, to []Section) Classification {
	c := Classification{Type: ChangeTypeFix}
	classifySections(&c, from, to)
	return c
}

// ClassifyPolicyChange suggests the change type of a document version against the one it
// replaces. Structure first: a different template, or a switch between a template and freeform,
// is Major; so is a required template section added or dropped, and an optional one is Minor.
// Then the text: a changed word that is a number, a date, an amount or an obligation word (must,
// may, within, days and so on) is Major, whatever else changed. Past that, a section added or
// dropped, or more than FixWordLimit changed words, is Minor, and anything smaller is a Fix.
// Content that cannot be read is Major, the cautious side. Scope, audience and sensitivity are
// kept on the document, not on its versions, so they are not compared.
func ClassifyPolicyChange(from, to PolicyRevision) Classification {
	c := Classification{Type: ChangeTypeFix}

	switch {
	case (from.Template == nil) != (to.Template == nil):
		c.raise(ChangeTypeMajor, "template added or removed")
	case from.Template != nil && from.Template.TemplateID != to.Template.TemplateID:
		c.raise(ChangeTypeMajor, "a different template")
	case from.Template != nil:
		classifySections(&c, from.Template.Sections, to.Template.Sections)
	}

	fromText, err := parseContent(from.ContentJSON)
	if err != nil {
		c.raise(ChangeTypeMajor, "the earlier content could not be read")
		return c
	}
	toText, err := parseContent(to.ContentJSON)
	if err != nil {
		c.raise(ChangeTypeMajor, "the new content could not be read")
		return c
	}

	required := map[string]bool{}
	for _, side := range []*TemplateRef{from.Template, to.Template} {
		if side != nil {
			for k := range requiredKeys(side.Sections) {
				required[k] = true
			}
		}
	}

	changedWords := 0
	obligation := ""
	keys := map[string]bool{}
	for k := range fromText {
		keys[k] = true
	}
	for k := range toText {
		keys[k] = true
	}
	for k := range keys {
		before, after := fromText[k], toText[k]
		if before == after {
			continue
		}
		switch {
		case before == "":
			if required[k] {
				c.raise(ChangeTypeMajor, "required section filled: "+k)
			} else {
				c.raise(ChangeTypeMinor, "section added: "+k)
			}
		case after == "":
			if required[k] {
				c.raise(ChangeTypeMajor, "required section emptied: "+k)
			} else {
				c.raise(ChangeTypeMinor, "section removed: "+k)
			}
		}
		for _, o := range wordOps(before, after) {
			if o.kind == "eq" {
				continue
			}
			changedWords++
			if obligation == "" && isObligationWord(o.word) {
				obligation = o.word
			}
		}
	}
	if obligation != "" {
		c.raise(ChangeTypeMajor, fmt.Sprintf("the wording changes an obligation, date, number or amount (%q)", obligation))
	}
	if changedWords > FixWordLimit {
		c.raise(ChangeTypeMinor, fmt.Sprintf("%d words changed", changedWords))
	}
	return c
}
