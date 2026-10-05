// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package domain

import (
	"slices"
	"strings"

	"github.com/google/uuid"
)

// EffectiveSlugPath derives a category's full inherited slug from its ancestor
// chain: the root segment first, joined by '-' down to the leaf (parent 'it'
// and child 'security' give 'it-security'). Only each category's own segment
// is stored, so a rename or move flows to the whole subtree.
//
// chain is leaf-first, as CategoryStore.AncestorChain returns it.
func EffectiveSlugPath(chain []Category) string {
	if len(chain) == 0 {
		return ""
	}
	parts := make([]string, 0, len(chain))
	for _, c := range slices.Backward(chain) {
		parts = append(parts, c.Slug)
	}
	return strings.Join(parts, "-")
}

// TemplateResolution is the 3-way outcome of resolving a policy's effective
// template through the tri-state (inherit / explicit-none / specific) at both
// the policy and category levels.
type TemplateResolution int

const (
	// TemplateInheritNotFound: nothing in the policy or the category chain set a
	// template (every level is "inherit / unset"). There is no effective
	// template and none was explicitly requested.
	TemplateInheritNotFound TemplateResolution = iota
	// TemplateExplicitNone: the policy — or the nearest category in the chain that
	// pins the state — is explicitly "none" (freeform). Effective template = none.
	TemplateExplicitNone
	// TemplateResolved: a specific template id was resolved.
	TemplateResolved
)

// EffectiveTemplate resolves a policy's effective template as a clean 3-way
// result following the tri-state at both levels.
//
// Resolution order (policy overrides the category chain; within the chain the
// nearest category that pins the state wins — leaf-wins, like exclusion inheritance):
//  1. The policy itself: templateNone=true → (none). A specific policyOverride
//     (non-nil) → (resolved, that id).
//  2. Walk the chain leaf-first: the FIRST category that is either explicit-none
//     (DefaultTemplateNone) or has a specific DefaultTemplateID wins. Explicit
//     none stops the walk with (none); a specific id resolves.
//  3. Nothing pinned anywhere → (inherit-not-found).
//
// chain must be ordered leaf-first (home category at index 0, root at index len-1).
func EffectiveTemplate(policyOverride uuid.UUID, policyNone bool, chain []Category) (TemplateResolution, uuid.UUID) {
	// Policy-level override wins over the whole category chain.
	if policyNone {
		return TemplateExplicitNone, uuid.Nil
	}
	if policyOverride != uuid.Nil {
		return TemplateResolved, policyOverride
	}
	// Walk the chain leaf-first; the first category that pins the state wins.
	for _, g := range chain {
		if g.DefaultTemplateNone {
			return TemplateExplicitNone, uuid.Nil
		}
		if g.DefaultTemplateID != uuid.Nil {
			return TemplateResolved, g.DefaultTemplateID
		}
	}
	return TemplateInheritNotFound, uuid.Nil
}

// EffectiveTemplateID returns the template that governs a policy, or uuid.Nil
// for freeform and unresolved alike. Use EffectiveTemplate to tell them apart.
func EffectiveTemplateID(policyOverride uuid.UUID, chain []Category) uuid.UUID {
	_, id := EffectiveTemplate(policyOverride, false, chain)
	return id
}

// EffectiveWorkflowID resolves the inherited default workflow: a policy
// override wins, else the nearest ancestor with a DefaultWorkflowID. Nothing
// calls it yet; workflow resolution will.
func EffectiveWorkflowID(policyOverride uuid.UUID, chain []Category) uuid.UUID {
	if policyOverride != uuid.Nil {
		return policyOverride
	}
	for _, g := range chain {
		if g.DefaultWorkflowID != uuid.Nil {
			return g.DefaultWorkflowID
		}
	}
	return uuid.Nil
}

// EffectiveAudienceGroups resolves the acknowledgement audience group names for a category
// using leaf-wins inheritance: the first category in chain whose AudienceGroupIDs is
// non-nil (even if empty) wins. An empty non-nil slice is a valid "no
// audience" override; nil means "not set — keep walking up".
//
// chain must be ordered leaf-first (home category at index 0, root at index
// len-1). If no category in the chain has AudienceGroupIDs set (all nil), nil is
// returned (caller treats as "unset").
func EffectiveAudienceGroups(chain []Category) []string {
	for _, g := range chain {
		if g.AudienceGroupIDs != nil {
			return *g.AudienceGroupIDs
		}
	}
	return nil
}

// EffectiveExclusionGroups resolves the excluded group names for a category
// using the same leaf-wins rule as EffectiveAudienceGroups. A nil
// ExclusionGroupIDs pointer means "not set"; a non-nil pointer (even to an
// empty slice) is an explicit override.
//
// chain must be ordered leaf-first.
func EffectiveExclusionGroups(chain []Category) []string {
	for _, g := range chain {
		if g.ExclusionGroupIDs != nil {
			return *g.ExclusionGroupIDs
		}
	}
	return nil
}

// EffectiveAckEveryone resolves the "Everyone" catch-all ack-audience flag for a
// category using the same nearest-ancestor-wins rule as EffectiveAudienceGroups:
// the first category in chain whose AckEveryone pointer is non-nil wins. A nil
// pointer means "not set — keep walking up". Returns nil when no category in the
// chain sets it (caller treats the effective value as false / unset).
//
// chain must be ordered leaf-first.
func EffectiveAckEveryone(chain []Category) *bool {
	for _, g := range chain {
		if g.AckEveryone != nil {
			return g.AckEveryone
		}
	}
	return nil
}
