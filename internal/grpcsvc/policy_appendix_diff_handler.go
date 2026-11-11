// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
)

// DiffPolicyAppendix handles PolicyAppendixService.DiffPolicyAppendix (#86).
// The from side is the published row's content, empty when none is published.
// The to side is the supplied content_json (text the author has not saved),
// else the draft's content, else the published row's. An appendix has no
// template, so the sections are the union of the keys of both sides. It is a
// read: nothing is saved, no draft appears, no audit event is written and
// nothing is cached. A retired appendix can still be diffed.
func (h *PolicyAppendixHandler) DiffPolicyAppendix(ctx context.Context, req *corev1.DiffPolicyAppendixRequest) (*corev1.DiffPolicyAppendixResponse, error) {
	l := logger.Ctx(ctx)
	id, err := uuid.Parse(req.GetId())
	if err != nil {
		l.Debug("diff policy appendix refused: bad id")
		return nil, status.Errorf(codes.InvalidArgument, "invalid id: %v", err)
	}
	live := req.ContentJson != nil
	if live {
		if err := domain.ValidateAppendixContent(req.GetContentJson()); err != nil {
			l.Debug("diff policy appendix refused: bad content", log.F("appendix_id", id.String()))
			return nil, status.Error(codes.InvalidArgument, err.Error())
		}
		if _, err := domain.FreeformSections("", req.GetContentJson()); err != nil {
			l.Debug("diff policy appendix refused: content cannot be read", log.F("appendix_id", id.String()))
			return nil, status.Error(codes.InvalidArgument, "content_json is not readable appendix content")
		}
	}
	l.Debug("diff policy appendix", log.F("appendix_id", id.String()), log.F("live_content", live))
	a, err := h.store.Get(ctx, id)
	if err != nil {
		return nil, policyAppendixErr(ctx, "diff_policy_appendix", err)
	}

	from, to, toSide := "", "", "none"
	pub, draft := a.Published(), a.Draft()
	if pub != nil {
		from, to, toSide = pub.ContentJSON, pub.ContentJSON, "published"
	}
	switch {
	case live:
		to, toSide = req.GetContentJson(), "supplied"
	case draft != nil:
		to, toSide = draft.ContentJSON, "draft"
	}
	l.Debug("diff policy appendix sides", log.F("appendix_id", id.String()),
		log.F("has_published", pub != nil), log.F("to_side", toSide))

	defs, err := domain.FreeformSections(from, to)
	if err != nil {
		l.Error(err, "diff policy appendix: read sections failed", log.F("appendix_id", id.String()))
		return nil, status.Error(codes.Internal, "read sections failed")
	}
	diffs, err := domain.DiffVersions(from, to, defs, false)
	if err != nil {
		l.Error(err, "diff policy appendix failed", log.F("appendix_id", id.String()))
		return nil, status.Error(codes.Internal, "diff failed")
	}
	out := make([]*corev1.SectionDiff, 0, len(diffs))
	changed := 0
	for _, d := range diffs {
		if d.ChangeType != domain.DiffChangeUnchanged {
			changed++
		}
		out = append(out, &corev1.SectionDiff{
			SectionKey:    d.SectionKey,
			SectionTitle:  d.SectionTitle,
			ChangeType:    string(d.ChangeType),
			WordDiffHtml:  d.WordDiffHTML,
			IsBoilerplate: d.IsBoilerplate,
		})
	}
	l.Debug("diff policy appendix done", log.F("appendix_id", id.String()),
		log.F("sections", len(out)), log.F("changed", changed))
	return &corev1.DiffPolicyAppendixResponse{Diffs: out}, nil
}
