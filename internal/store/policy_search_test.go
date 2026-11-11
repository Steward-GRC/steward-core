// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/google/uuid"

	pg "github.com/Bugs5382/go-postgres"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// searchEnv is a migrated database with one category and a published template, for the search
// tests (steward-core#30).
type searchEnv struct {
	pool *pg.DB
	ps   *store.PolicyStore
	as   *store.PolicyAppendixStore
	cat  domain.Category
	tv   domain.TemplateVersion
}

func newSearchEnv(t *testing.T) searchEnv {
	t.Helper()
	pool := newTestDB(t)
	cat, tv := setupCategoryAndTemplate(t, store.NewCategoryStore(pool), store.NewTemplateStore(pool))
	return searchEnv{pool: pool, ps: store.NewPolicyStore(pool), as: store.NewPolicyAppendixStore(pool), cat: cat, tv: tv}
}

// searchDoc is a Lexical document with one paragraph per text.
func searchDoc(texts ...string) string {
	children := make([]map[string]any, 0, len(texts))
	for _, text := range texts {
		children = append(children, map[string]any{
			"type":     "paragraph",
			"children": []map[string]any{{"type": "text", "text": text}},
		})
	}
	b, _ := json.Marshal(map[string]any{"root": map[string]any{"type": "root", "children": children}})
	return string(b)
}

// create makes a policy of the given type with a draft holding text.
func (e searchEnv) create(t *testing.T, docType domain.DocumentType, title, text string) domain.Policy {
	t.Helper()
	ctx := context.Background()
	p, err := domain.NewPolicy(title, e.cat.ID, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p.DocumentType = docType
	p, err = e.ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	e.draft(t, p, text)
	return p
}

// draft saves text as the policy's draft.
func (e searchEnv) draft(t *testing.T, p domain.Policy, text string) {
	t.Helper()
	d, err := domain.NewPolicyVersionDraft(p.ID, e.tv.ID, uuid.New(), searchDoc(text))
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	if _, _, err := e.ps.UpsertDraft(context.Background(), d); err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
}

// publish creates a policy and publishes text.
func (e searchEnv) publish(t *testing.T, title, text string) domain.Policy {
	t.Helper()
	p := e.create(t, domain.DocumentTypePolicy, title, text)
	if _, err := e.ps.PublishDraft(context.Background(), p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	p, err := e.ps.GetPolicy(context.Background(), p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	return p
}

func (e searchEnv) search(t *testing.T, query string, docType domain.DocumentType, limit, offset int) ([]store.PolicySearchHit, bool) {
	t.Helper()
	hits, more, err := e.ps.SearchPolicies(context.Background(), query, docType, limit, offset)
	if err != nil {
		t.Fatalf("SearchPolicies(%d chars): %v", len(query), err)
	}
	return hits, more
}

func (e searchEnv) ids(t *testing.T, query string) []uuid.UUID {
	t.Helper()
	hits, _ := e.search(t, query, "", 50, 0)
	out := make([]uuid.UUID, 0, len(hits))
	for _, h := range hits {
		out = append(out, h.PolicyID)
	}
	return out
}

func sameSearchIDs(got []uuid.UUID, want ...uuid.UUID) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// marked is the words a snippet's spans point at.
func marked(s *store.PolicySearchSnippet) []string {
	units := utf16.Encode([]rune(s.Text))
	out := make([]string, 0, len(s.Matches))
	for _, m := range s.Matches {
		out = append(out, string(utf16.Decode(units[m.Start:m.Start+m.Length])))
	}
	return out
}

func TestSearchFindsAWordOnlyInThePublishedText(t *testing.T) {
	e := newSearchEnv(t)
	p := e.publish(t, "Expense Claims Policy", "Staff attach a photo of each receipt to the claim.")
	e.publish(t, "Desk Booking Policy", "Book a desk the day before.")

	hits, more := e.search(t, "PHOTO", "", 20, 0)
	if len(hits) != 1 || hits[0].PolicyID != p.ID || more {
		t.Fatalf("hits = %+v more = %v, want only %s", hits, more, p.ID)
	}
	h := hits[0]
	if h.Rank <= 0 || h.TitleMatch || h.TitleRank != 0 {
		t.Fatalf("rank = %v title match = %v title rank = %v", h.Rank, h.TitleMatch, h.TitleRank)
	}
	if h.Snippet == nil {
		t.Fatal("no snippet for a text match")
	}
	if strings.ContainsAny(h.Snippet.Text, "<>") {
		t.Fatalf("snippet carries markup: %q", h.Snippet.Text)
	}
	if got := marked(h.Snippet); len(got) != 1 || got[0] != "photo" {
		t.Fatalf("marked = %v in %q", got, h.Snippet.Text)
	}
}

func TestSearchMatchesWholeWordsIgnoringCase(t *testing.T) {
	e := newSearchEnv(t)
	e.publish(t, "Photography Policy", "Photographs of the premises are not allowed.")
	if got := e.ids(t, "photo"); len(got) != 0 {
		t.Fatalf("photo matched part of a longer word: %v", got)
	}
	if got := e.ids(t, "PHOTOGRAPHS"); len(got) != 1 {
		t.Fatalf("PHOTOGRAPHS found %d, want 1", len(got))
	}
}

func TestSearchRequiresEveryWordAndKeepsCommonWords(t *testing.T) {
	e := newSearchEnv(t)
	p := e.publish(t, "Travel Policy", "Book the ferry before the quarter ends.")
	if got := e.ids(t, "ferry quarter"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("ferry quarter = %v", got)
	}
	if got := e.ids(t, "ferry zeppelin"); len(got) != 0 {
		t.Fatalf("ferry zeppelin = %v, want none", got)
	}
	if got := e.ids(t, "the"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("a common word must still match, got %v", got)
	}
}

func TestSearchFindsATitleAndANumberWithoutASnippet(t *testing.T) {
	e := newSearchEnv(t)
	p := e.publish(t, "Archive Access Policy", "Only the records team opens the vault.")

	hits, _ := e.search(t, "archive", "", 20, 0)
	if len(hits) != 1 || hits[0].PolicyID != p.ID {
		t.Fatalf("title search = %+v", hits)
	}
	h := hits[0]
	if !h.TitleMatch || h.TitleRank <= 0 || h.Snippet != nil {
		t.Fatalf("title match = %v title rank = %v snippet = %+v", h.TitleMatch, h.TitleRank, h.Snippet)
	}

	for _, q := range []string{p.Number, strings.ToLower(p.Number)} {
		hits, _ = e.search(t, q, "", 20, 0)
		if len(hits) != 1 || hits[0].PolicyID != p.ID || !hits[0].TitleMatch {
			t.Fatalf("number search %q = %+v", q, hits)
		}
	}
}

func TestSearchTitleMatchNeedsEveryWordInTheTitleOrNumber(t *testing.T) {
	e := newSearchEnv(t)
	p := e.publish(t, "Archive Access Policy", "The vault opens at the start of each quarter.")

	hits, _ := e.search(t, "archive quarter", "", 20, 0)
	if len(hits) != 1 || hits[0].PolicyID != p.ID {
		t.Fatalf("archive quarter = %+v", hits)
	}
	h := hits[0]
	if h.TitleMatch || h.TitleRank != 0 {
		t.Fatalf("a word only in the text must not count as a title match: %+v", h)
	}
	if h.Snippet == nil || len(marked(h.Snippet)) == 0 {
		t.Fatalf("snippet = %+v, want the text match marked", h.Snippet)
	}
}

func TestSearchRanksTitleHitsAboveTextHits(t *testing.T) {
	e := newSearchEnv(t)
	text := e.publish(t, "Travel Booking", "Claims go through the portal.")
	more := e.publish(t, "Hotel Stays", "Claims and more claims, as claims go.")
	title := e.publish(t, "Expense Claims Policy", "Keep every receipt.")

	hits, _ := e.search(t, "claims", "", 20, 0)
	var got []uuid.UUID
	for _, h := range hits {
		got = append(got, h.PolicyID)
	}
	if !sameSearchIDs(got, title.ID, more.ID, text.ID) {
		t.Fatalf("order = %v, want title %s, then %s, then %s", got, title.ID, more.ID, text.ID)
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Rank > hits[i-1].Rank {
			t.Fatalf("ranks not descending: %v then %v", hits[i-1].Rank, hits[i].Rank)
		}
	}
}

func TestSearchNeverMatchesADraftASupersededVersionOrARetiredPolicy(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()

	e.create(t, domain.DocumentTypePolicy, "Draft Only", "Zeppelin travel is never reimbursed.")
	if got := e.ids(t, "zeppelin"); len(got) != 0 {
		t.Fatalf("a never-published draft matched: %v", got)
	}

	// A new draft beside a published version does not match until it is published.
	p := e.publish(t, "Carry Over", "Unused leave is lost at year end.")
	e.draft(t, p, "Unused leave carries over for ten days.")
	if got := e.ids(t, "lost"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("published text lost = %v", got)
	}
	if got := e.ids(t, "ten"); len(got) != 0 {
		t.Fatalf("draft text matched: %v", got)
	}

	// Publishing the draft supersedes the old text.
	if _, err := e.ps.PublishDraft(ctx, p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if got := e.ids(t, "lost"); len(got) != 0 {
		t.Fatalf("superseded text matched: %v", got)
	}
	if got := e.ids(t, "ten"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("new published text ten = %v", got)
	}

	// Retiring hides the policy.
	if _, err := e.ps.RetirePolicy(ctx, p.ID); err != nil {
		t.Fatalf("RetirePolicy: %v", err)
	}
	if got := e.ids(t, "ten"); len(got) != 0 {
		t.Fatalf("retired policy matched on text: %v", got)
	}
	if got := e.ids(t, "carry"); len(got) != 0 {
		t.Fatalf("retired policy matched on title: %v", got)
	}
}

func TestSearchFollowsATitleChange(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()
	p := e.publish(t, "Old Name", "Nothing to see.")
	if _, err := e.ps.SetTitle(ctx, p.ID, "Fresh Title"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if got := e.ids(t, "fresh"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("renamed title = %v", got)
	}
	if got := e.ids(t, "old"); len(got) != 0 {
		t.Fatalf("old title still matches: %v", got)
	}
}

func TestSearchFiltersByDocumentType(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()
	pol := e.publish(t, "Lost Device Policy", "Report a lost device at once.")
	proc := e.create(t, domain.DocumentTypeProcedure, "Lost Device Procedure", "Report a lost device at once.")
	if _, err := e.ps.PublishDraft(ctx, proc.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}

	both, _ := e.search(t, "device", "", 20, 0)
	if len(both) != 2 {
		t.Fatalf("both types = %d, want 2", len(both))
	}
	onlyPol, _ := e.search(t, "device", domain.DocumentTypePolicy, 20, 0)
	if len(onlyPol) != 1 || onlyPol[0].PolicyID != pol.ID {
		t.Fatalf("policies only = %+v", onlyPol)
	}
	onlyProc, _ := e.search(t, "device", domain.DocumentTypeProcedure, 20, 0)
	if len(onlyProc) != 1 || onlyProc[0].PolicyID != proc.ID {
		t.Fatalf("procedures only = %+v", onlyProc)
	}
}

func TestSearchPagesWithLimitAndOffset(t *testing.T) {
	e := newSearchEnv(t)
	for _, title := range []string{"Portal One", "Portal Two", "Portal Three"} {
		e.publish(t, title, "Book through the portal.")
	}
	all, more := e.search(t, "portal", "", 50, 0)
	if len(all) != 3 || more {
		t.Fatalf("all = %d more = %v", len(all), more)
	}

	var seen []uuid.UUID
	offset := 0
	for range 3 {
		page, more := e.search(t, "portal", "", 2, offset)
		for _, h := range page {
			seen = append(seen, h.PolicyID)
		}
		offset += len(page)
		if !more {
			break
		}
		if len(page) != 2 {
			t.Fatalf("a page with more behind it held %d hits", len(page))
		}
	}
	var want []uuid.UUID
	for _, h := range all {
		want = append(want, h.PolicyID)
	}
	if !sameSearchIDs(seen, want...) {
		t.Fatalf("paged = %v, want %v", seen, want)
	}
	if page, more := e.search(t, "portal", "", 2, 4); len(page) != 0 || more {
		t.Fatalf("past the end = %d hits more = %v", len(page), more)
	}
}

func TestSearchCutsALongPassageToAWindowAroundTheMatch(t *testing.T) {
	e := newSearchEnv(t)
	filler := strings.Repeat("Staff book travel early and keep every receipt. ", 8)
	e.publish(t, "Long Travel", filler+"Ferries need approval. "+filler)

	hits, _ := e.search(t, "ferries", "", 20, 0)
	if len(hits) != 1 || hits[0].Snippet == nil {
		t.Fatalf("hits = %+v", hits)
	}
	s := hits[0].Snippet
	if n := len([]rune(s.Text)); n > 170 {
		t.Fatalf("snippet is %d characters: %q", n, s.Text)
	}
	if !strings.HasPrefix(s.Text, "…") || !strings.HasSuffix(s.Text, "…") {
		t.Fatalf("a cut snippet needs an ellipsis at each cut end: %q", s.Text)
	}
	if got := marked(s); len(got) != 1 || got[0] != "Ferries" {
		t.Fatalf("marked = %v in %q", got, s.Text)
	}
}

func TestSearchSpansCountUTF16UnitsPastAnAstralCharacter(t *testing.T) {
	e := newSearchEnv(t)
	// U+10348 takes two UTF-16 code units and four UTF-8 bytes.
	e.publish(t, "Runes", "\U00010348\U00010348\U00010348 then the word ferry here")

	hits, _ := e.search(t, "ferry", "", 20, 0)
	if len(hits) != 1 || hits[0].Snippet == nil {
		t.Fatalf("hits = %+v", hits)
	}
	if got := marked(hits[0].Snippet); len(got) != 1 || got[0] != "ferry" {
		t.Fatalf("marked = %v in %q", got, hits[0].Snippet.Text)
	}
}

func TestSearchCoversThePublishedAppendices(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()
	p := e.publish(t, "Vault Access", "Opens at nine.")
	actor := uuid.New()

	kept, err := e.as.Create(ctx, p.ID, actor, "Schedule", searchDoc("Zebra rota for the vault."))
	if err != nil {
		t.Fatalf("Create appendix: %v", err)
	}
	if got := e.ids(t, "zebra"); len(got) != 0 {
		t.Fatalf("a draft appendix matched: %v", got)
	}
	if _, err := e.as.Publish(ctx, kept.ID, actor); err != nil {
		t.Fatalf("Publish appendix: %v", err)
	}
	if got := e.ids(t, "zebra"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("published appendix zebra = %v", got)
	}

	// A newer draft stays out until it is published.
	if _, _, err := e.as.SaveDraft(ctx, kept.ID, actor, "Schedule", searchDoc("Walrus rota for the vault.")); err != nil {
		t.Fatalf("SaveDraft appendix: %v", err)
	}
	if got := e.ids(t, "walrus"); len(got) != 0 {
		t.Fatalf("an appendix draft matched: %v", got)
	}
	if got := e.ids(t, "zebra"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("published text lost while a draft is open: %v", got)
	}
	if _, err := e.as.Publish(ctx, kept.ID, actor); err != nil {
		t.Fatalf("Publish appendix 2: %v", err)
	}
	if got := e.ids(t, "walrus"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("new appendix text walrus = %v", got)
	}
	if got := e.ids(t, "zebra"); len(got) != 0 {
		t.Fatalf("superseded appendix text matched: %v", got)
	}

	// A retired appendix is hidden from readers, so from search.
	if _, _, err := e.as.Retire(ctx, kept.ID, actor); err != nil {
		t.Fatalf("Retire appendix: %v", err)
	}
	if got := e.ids(t, "walrus"); len(got) != 0 {
		t.Fatalf("a retired appendix matched: %v", got)
	}
}

func TestSearchReadsTheLegacySectionEnvelope(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()
	p, _ := domain.NewPolicy("Envelope", e.cat.ID, domain.SensitivityStandard, uuid.New())
	p, err := e.ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, e.tv.ID, uuid.New(), `{"sections":{"purpose":"Pelican rules apply."}}`)
	if _, _, err := e.ps.UpsertDraft(ctx, d); err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	if _, err := e.ps.PublishDraft(ctx, p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if got := e.ids(t, "pelican"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("envelope text = %v", got)
	}
	// Structural words of the editor's JSON are not text.
	if got := e.ids(t, "paragraph"); len(got) != 0 {
		t.Fatalf("markup matched: %v", got)
	}
}

func TestSearchOfNothingFindsNothing(t *testing.T) {
	e := newSearchEnv(t)
	e.publish(t, "Anything", "Some text.")
	for _, q := range []string{"", "   ", "!!! ---"} {
		if hits, more := e.search(t, q, "", 20, 0); len(hits) != 0 || more {
			t.Fatalf("query of %d chars found %d hits", len(q), len(hits))
		}
	}
}

func TestSearchFollowsAnApprovalWrittenByWorkflow(t *testing.T) {
	e := newSearchEnv(t)
	ctx := context.Background()
	p := e.create(t, domain.DocumentTypePolicy, "Workflow Policy", "Pelican rules apply.")
	d, err := e.ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	for _, step := range []domain.PolicyVersionStatus{domain.PolicyVersionStatusInReview, domain.PolicyVersionStatusPublished} {
		if got := e.ids(t, "pelican"); len(got) != 0 {
			t.Fatalf("matched before %s: %v", step, got)
		}
		if _, err := e.pool.Pool().Exec(ctx, `UPDATE policy_versions SET version_no = 1 WHERE id = $1 AND version_no = 0`, d.CurrentDraftVersionID); err != nil {
			t.Fatalf("number the version: %v", err)
		}
		if _, err := e.ps.SetVersionStatus(ctx, d.CurrentDraftVersionID, step); err != nil {
			t.Fatalf("SetVersionStatus(%s): %v", step, err)
		}
	}
	if got := e.ids(t, "pelican"); !sameSearchIDs(got, p.ID) {
		t.Fatalf("approved text = %v", got)
	}
}
