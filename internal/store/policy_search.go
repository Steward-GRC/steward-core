// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf16"

	log "github.com/Bugs5382/go-log"
	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// Policy text search (steward-core#30). The index is policy_search (migration 0027), kept by
// triggers; this file reads it. Neither the query text nor any policy text is ever logged.

const (
	// snippetMark and snippetUnmark bracket the matched words in the text Postgres cuts the
	// snippet from. They are private-use characters the index text can never contain.
	snippetMark   = ''
	snippetUnmark = ''

	// SnippetMaxChars is the longest snippet text returned, ellipses included.
	SnippetMaxChars = 170
	// snippetLead is how much of the passage a cut snippet keeps before its first match.
	snippetLead = 40
	// snippetWindow is the longest run of passage text a snippet keeps, leaving room for an
	// ellipsis at each end.
	snippetWindow = SnippetMaxChars - 2
)

// SearchSpan is a run of a snippet's text, counted in UTF-16 code units.
type SearchSpan struct {
	Start  int
	Length int
}

// PolicySearchSnippet is a passage of the published text with the matched words marked. The text
// is plain; an ellipsis sits at a cut end.
type PolicySearchSnippet struct {
	Text    string
	Matches []SearchSpan
}

// PolicySearchHit is one matching policy. The text a hit was found in is published text, so the
// caller decides, per reader, whether to show any of it.
type PolicySearchHit struct {
	PolicyID uuid.UUID
	// Rank is the combined rank: a title or number hit outranks a text-only hit, then more
	// matches rank higher. Only comparable within one search.
	Rank float64
	// TitleMatch says every word of the query is in the title and number alone.
	TitleMatch bool
	// TitleRank is Rank counting the title and number alone, zero unless TitleMatch.
	TitleRank float64
	// Snippet is nil when no word matched the text.
	Snippet *PolicySearchSnippet
}

// searchQuery ranks, pages and cuts the snippets in one statement. $1 is the query, $2 the
// document type (” for both), $3 the row limit, $4 the offset, $5 the headline options and $6
// the two mark characters.
//
// The number is derived the way renderDocNumber does it: the type prefix, the category code (or
// the code its slug derives, PolicyNumberCode) and the sequence, zero-padded to six digits.
const searchQuery = `
WITH q AS (
  SELECT plainto_tsquery('simple'::regconfig, $1) AS q,
         -- ts_headline marks nothing in a passage that lacks a word of an AND query, so the
         -- snippet is cut with the same words ORed together.
         (SELECT string_agg('''' || replace(replace(w, E'\\', E'\\\\'), '''', '''''') || '''', ' | ')
            FROM unnest(tsvector_to_array(to_tsvector('simple'::regconfig, $1))) w)::tsquery AS hq
),
found AS (
  SELECT p.id, p.title, s.body, q.hq,
         hv.h @@ q.q AS title_match,
         ts_rank(hv.h, q.q, 32)::float8 AS title_score,
         ts_rank(hv.h || s.tsv, q.q, 32)::float8 AS score,
         ((CASE WHEN hv.h @@ q.q THEN 1 ELSE 0 END) + ts_rank(hv.h || s.tsv, q.q, 32))::float8 AS rank
    FROM q, policies p
    JOIN categories c ON c.id = p.home_category_id
    JOIN policy_search s ON s.policy_id = p.id AND s.version_id = p.current_published_version_id
    CROSS JOIN LATERAL (
      SELECT setweight(to_tsvector('simple'::regconfig, p.title), 'A')
          || setweight(to_tsvector('simple'::regconfig,
               CASE WHEN p.document_type::text = 'procedure' THEN 'PRC' ELSE 'POL' END || '-'
               || COALESCE(NULLIF(c.code, ''),
                           COALESCE(NULLIF(left(upper(regexp_replace(c.slug, '[^a-zA-Z0-9]', '', 'g')), 6), ''), 'GEN'))
               || '-' || repeat('0', greatest(0, 6 - length(p.sequence::text))) || p.sequence::text), 'A') AS h
    ) hv
   WHERE p.retired_at IS NULL
     AND numnode(q.q) > 0
     AND ($2::text = '' OR p.document_type::text = $2::text)
     AND (hv.h || s.tsv) @@ q.q
   ORDER BY rank DESC, p.title, p.id
   LIMIT $3 OFFSET $4
),
cut AS (
  SELECT f.*, ts_headline('simple'::regconfig, f.body, f.hq, $5) AS headline FROM found f
)
SELECT c.id, c.title_match, c.title_score, c.rank, c.headline,
       position(translate(c.headline, $6::text, '') in c.body),
       char_length(translate(c.headline, $6::text, '')),
       char_length(c.body)
  FROM cut c
 ORDER BY c.rank DESC, c.title, c.id
`

// SearchPolicies finds policies whose title, number or published text (appendices included)
// holds every word of query, whole words, ignoring case, best first. docType restricts the
// search to one kind; empty searches both. It reports whether more hits follow the page.
// The caller applies who may read what.
func (s *PolicyStore) SearchPolicies(ctx context.Context, query string, docType domain.DocumentType, limit, offset int) ([]PolicySearchHit, bool, error) {
	start := time.Now()
	lg := logger.Ctx(ctx)
	if strings.TrimSpace(query) == "" {
		lg.Debug("policy search skipped, empty query", log.F("query_len", len(query)))
		return nil, false, nil
	}
	if limit < 1 {
		return nil, false, fmt.Errorf("PolicyStore.SearchPolicies: limit must be at least 1")
	}
	if offset < 0 {
		return nil, false, fmt.Errorf("PolicyStore.SearchPolicies: offset must not be negative")
	}
	lg.Debug("policy search start", log.F("query_len", len(query)), log.F("document_type", string(docType)),
		log.F("limit", limit), log.F("offset", offset))

	options := fmt.Sprintf("StartSel=%c, StopSel=%c, MaxFragments=1, MaxWords=24, MinWords=8, ShortWord=0",
		snippetMark, snippetUnmark)
	rows, err := s.db.Querier().Query(ctx, searchQuery, query, string(docType), limit+1, offset, options, string([]rune{snippetMark, snippetUnmark}))
	if err != nil {
		lg.Error(err, "policy search failed", log.F("query_len", len(query)))
		return nil, false, fmt.Errorf("PolicyStore.SearchPolicies: %w", err)
	}
	defer rows.Close()

	var hits []PolicySearchHit
	for rows.Next() {
		var (
			h          PolicySearchHit
			titleScore float64
			headline   string
			at, n, all int
		)
		if err := rows.Scan(&h.PolicyID, &h.TitleMatch, &titleScore, &h.Rank, &headline, &at, &n, &all); err != nil {
			return nil, false, fmt.Errorf("PolicyStore.SearchPolicies scan: %w", err)
		}
		if h.TitleMatch {
			h.TitleRank = 1 + titleScore
		}
		// at is where the cut sits in the passage text (1-based, 0 when Postgres changed it).
		h.Snippet = buildSnippet(headline, at > 1, at > 0 && at-1+n < all)
		hits = append(hits, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("PolicyStore.SearchPolicies rows: %w", err)
	}
	more := len(hits) > limit
	if more {
		hits = hits[:limit]
	}
	lg.Debug("policy search done", log.F("query_len", len(query)), log.F("hits", len(hits)), log.F("more", more),
		log.F("duration_ms", time.Since(start).Milliseconds()))
	return hits, more, nil
}

// buildSnippet turns Postgres's marked headline into a plain snippet with UTF-16 spans, or nil
// when no word of the text matched (the headline then is only the passage's opening). cutFront
// and cutBack say the headline starts or ends inside the passage text.
func buildSnippet(headline string, cutFront, cutBack bool) *PolicySearchSnippet {
	type span struct{ start, length int } // in runes
	var (
		text  []rune
		spans []span
		open  = -1
	)
	for _, r := range headline {
		switch r {
		case snippetMark:
			open = len(text)
		case snippetUnmark:
			if open >= 0 && len(text) > open {
				spans = append(spans, span{open, len(text) - open})
			}
			open = -1
		case '\n', '\r', '\t':
			text = append(text, ' ')
		default:
			text = append(text, r)
		}
	}
	if len(spans) == 0 {
		return nil
	}

	// Trim the blanks around the headline, moving the spans with it.
	from, to := 0, len(text)
	for from < to && text[from] == ' ' {
		from++
	}
	for to > from && text[to-1] == ' ' {
		to--
	}

	// A long headline is cut to a window around its first match, at word edges.
	if to-from > snippetWindow {
		first := spans[0].start
		lo := max(from, first-snippetLead)
		if lo > from {
			for lo < first && text[lo-1] != ' ' {
				lo++
			}
			cutFront = true
		}
		hi := min(to, lo+snippetWindow)
		if hi < to {
			for hi > first+spans[0].length && text[hi] != ' ' {
				hi--
			}
			cutBack = true
		}
		from, to = lo, hi
		for to > from && text[to-1] == ' ' {
			to--
		}
	}

	var out []rune
	shift := -from
	if cutFront {
		out = append(out, '…')
		shift++
	}
	out = append(out, text[from:to]...)
	if cutBack {
		out = append(out, '…')
	}

	// Offsets are counted in UTF-16 code units, as JavaScript, NSString and Kotlin index strings.
	units := func(upto int) int { return len(utf16.Encode(out[:upto])) }
	snip := &PolicySearchSnippet{Text: string(out)}
	for _, sp := range spans {
		a, b := sp.start+shift, sp.start+sp.length+shift
		if sp.start < from || sp.start+sp.length > to || b > len(out) {
			continue
		}
		snip.Matches = append(snip.Matches, SearchSpan{Start: units(a), Length: units(b) - units(a)})
	}
	if len(snip.Matches) == 0 {
		return nil
	}
	return snip
}
