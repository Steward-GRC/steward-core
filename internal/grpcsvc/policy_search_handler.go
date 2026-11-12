// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc

import (
	"context"
	"errors"
	"strings"
	"time"

	log "github.com/Bugs5382/go-log"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

const (
	// searchQueryMaxChars caps a search query, in characters.
	searchQueryMaxChars = 256
	// searchDefaultLimit is the page size when the request names none; searchMaxLimit the most
	// one page holds.
	searchDefaultLimit = 20
	searchMaxLimit     = 100
)

// SearchPolicies handles PolicyService.SearchPolicies (steward-core#30): a full-text search over
// the published text of documents. It does not decide who may read what; the gateway filters the
// hits by the reader's read decision. The query text and the policy text are never logged, only
// the query's length.
func (h *PolicyHandler) SearchPolicies(ctx context.Context, req *corev1.SearchPoliciesRequest) (*corev1.SearchPoliciesResponse, error) {
	start := time.Now()
	lg := logger.Ctx(ctx)
	query := req.GetQuery()
	chars := len([]rune(query))
	lg.Debug("core: search policies", log.F("query_chars", chars), log.F("document_type", req.GetDocumentType().String()),
		log.F("limit", req.GetLimit()), log.F("offset", req.GetOffset()))

	if chars > searchQueryMaxChars {
		return nil, status.Errorf(codes.InvalidArgument, "query is longer than %d characters", searchQueryMaxChars)
	}
	if req.GetLimit() < 0 {
		return nil, status.Error(codes.InvalidArgument, "limit must not be negative")
	}
	if req.GetOffset() < 0 {
		return nil, status.Error(codes.InvalidArgument, "offset must not be negative")
	}
	limit := int(req.GetLimit())
	if limit == 0 {
		limit = searchDefaultLimit
	}
	limit = min(limit, searchMaxLimit)

	if strings.TrimSpace(query) == "" {
		lg.Debug("core: search policies, blank query", log.F("query_chars", chars))
		return &corev1.SearchPoliciesResponse{}, nil
	}

	// Unspecified searches both kinds, unlike the listings, which read it as policies.
	var docType domain.DocumentType
	switch req.GetDocumentType() {
	case corev1.DocumentType_DOCUMENT_TYPE_POLICY:
		docType = domain.DocumentTypePolicy
	case corev1.DocumentType_DOCUMENT_TYPE_PROCEDURE:
		docType = domain.DocumentTypeProcedure
	}

	hits, more, err := h.store.SearchPolicies(ctx, query, docType, limit, int(req.GetOffset()))
	if err != nil {
		return nil, storeUnavailable(ctx, "search_policies", err)
	}

	out := make([]*corev1.PolicySearchHit, 0, len(hits))
	for _, hit := range hits {
		p, err := h.store.GetPolicy(ctx, hit.PolicyID)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				// Deleted between the search and this read.
				lg.Debug("core: search hit vanished", log.F("policy_id", hit.PolicyID.String()))
				continue
			}
			return nil, storeUnavailable(ctx, "search_policies_get", err)
		}
		out = append(out, searchHitToProto(hit, p))
	}
	lg.Debug("core: search policies done", log.F("query_chars", chars), log.F("hits", len(out)),
		log.F("has_more", more), log.F("duration_ms", time.Since(start).Milliseconds()))
	return &corev1.SearchPoliciesResponse{Hits: out, HasMore: more}, nil
}

func searchHitToProto(hit store.PolicySearchHit, p domain.Policy) *corev1.PolicySearchHit {
	out := &corev1.PolicySearchHit{
		Policy:     policyToProto(p),
		Rank:       hit.Rank,
		TitleMatch: hit.TitleMatch,
		TitleRank:  hit.TitleRank,
	}
	if hit.Snippet != nil {
		sn := &corev1.PolicySearchSnippet{Text: hit.Snippet.Text}
		for _, m := range hit.Snippet.Matches {
			sn.Matches = append(sn.Matches, &corev1.SearchMatch{Start: int32(m.Start), Length: int32(m.Length)}) // #nosec G115 -- offsets are bounded by the 170-character snippet
		}
		out.Snippet = sn
	}
	return out
}
