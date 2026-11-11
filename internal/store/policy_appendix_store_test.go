// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"os"
	"sync"
	"testing"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// The Go counterparts of the mock server's appendices.test.ts and
// appendix-titles-contributors.test.ts. Each test names the mock test it ports.

const lexical = `{"root":{"children":[],"type":"root"}}`

func content(text string) string { return `{"text":"` + text + `"}` }

type appendixEnv struct {
	ctx    context.Context
	pool   *postgres.DB
	st     *store.PolicyAppendixStore
	policy uuid.UUID
	bob    uuid.UUID
	alice  uuid.UUID
}

func newAppendixEnv(t *testing.T) appendixEnv {
	t.Helper()
	pool := newTestDB(t)
	policy, _ := makeVersion(t, store.NewPolicyStore(pool), pool, "draft")
	return appendixEnv{
		ctx: context.Background(), pool: pool, st: store.NewPolicyAppendixStore(pool),
		policy: policy, bob: uuid.New(), alice: uuid.New(),
	}
}

func (e appendixEnv) create(t *testing.T, title string) domain.PolicyAppendix {
	t.Helper()
	a, err := e.st.Create(e.ctx, e.policy, e.bob, title, content("Terms."))
	require.NoError(t, err)
	return a
}

func (e appendixEnv) list(t *testing.T, includeRetired bool) []domain.PolicyAppendix {
	t.Helper()
	out, err := e.st.List(e.ctx, e.policy, includeRetired)
	require.NoError(t, err)
	return out
}

func versionNos(a domain.PolicyAppendix) [][2]any {
	var out [][2]any
	for _, v := range a.Versions {
		out = append(out, [2]any{v.VersionNo, v.State})
	}
	return out
}

// appendices.test.ts "starts a new appendix as a draft only an editor sees".
func TestPolicyAppendix_NewIsADraftV1(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Glossary")
	require.Equal(t, e.policy, a.PolicyID)
	require.Equal(t, "A", a.Letter)
	require.Equal(t, 0, a.OrderIndex)
	require.Nil(t, a.Published())
	d := a.Draft()
	require.NotNil(t, d)
	require.Equal(t, 1, d.VersionNo)
	require.Equal(t, "Glossary", d.Title)
	require.Equal(t, []uuid.UUID{e.bob}, d.Editors)
	require.Equal(t, e.bob, d.CreatedBy)
	require.JSONEq(t, content("Terms."), d.ContentJSON)
	require.Len(t, e.list(t, false), 1)
}

// appendix-titles-contributors.test.ts "is Appendix B, then Appendix C, when no title is given"
// (here the first is A: the Go repo has no seeded appendix).
func TestPolicyAppendix_DefaultTitleByLetter(t *testing.T) {
	e := newAppendixEnv(t)
	first, second := e.create(t, ""), e.create(t, "")
	require.Equal(t, []string{"A", "Appendix A", "B", "Appendix B"},
		[]string{first.Letter, first.Draft().Title, second.Letter, second.Draft().Title})
}

// "treats a blank title as no title" and "keeps a title that was given".
func TestPolicyAppendix_BlankTitleIsNoTitle(t *testing.T) {
	e := newAppendixEnv(t)
	require.Equal(t, "Appendix A", e.create(t, "   ").Draft().Title)
	require.Equal(t, "Glossary", e.create(t, "Glossary").Draft().Title)
}

// "skips a name another appendix of the policy already uses".
func TestPolicyAppendix_DefaultTitleSkipsUsedNames(t *testing.T) {
	e := newAppendixEnv(t)
	e.create(t, "Appendix B")
	next := e.create(t, "")
	require.Equal(t, "B", next.Letter)
	require.Equal(t, "Appendix C", next.Draft().Title, "B is taken, so the second place is titled C")
}

// "skips the name of a retired appendix too".
func TestPolicyAppendix_DefaultTitleSkipsRetiredNames(t *testing.T) {
	e := newAppendixEnv(t)
	first := e.create(t, "")
	require.Equal(t, "Appendix A", first.Draft().Title)
	_, _, err := e.st.Retire(e.ctx, first.ID, e.bob)
	require.NoError(t, err)
	next := e.create(t, "")
	require.Equal(t, "A", next.Letter)
	require.Equal(t, "Appendix B", next.Draft().Title)
}

// "leaves renaming open": a saved title may be anything, even a used one.
func TestPolicyAppendix_RenamingIsOpen(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "")
	saved, changed, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Glossary of terms", content("Terms."))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "Glossary of terms", saved.Draft().Title)
}

// appendices.test.ts "autosaves into the draft, and shows readers nothing until it is published":
// a save with a draft open updates that draft.
func TestPolicyAppendix_SaveUpdatesTheOpenDraft(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Glossary")
	saved, changed, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Glossary of terms", content("More terms."))
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, saved.Versions, 1)
	require.Equal(t, "Glossary of terms", saved.Draft().Title)
	require.JSONEq(t, content("More terms."), saved.Draft().ContentJSON)
	require.Nil(t, saved.Published())
}

// "publishes to the version readers see, with who and when".
func TestPolicyAppendix_PublishPromotesTheDraft(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Glossary")
	pub, err := e.st.Publish(e.ctx, a.ID, e.alice)
	require.NoError(t, err)
	row := pub.Published()
	require.NotNil(t, row)
	require.Equal(t, 1, row.VersionNo)
	require.NotNil(t, row.PublishedAt)
	require.NotNil(t, row.PublishedBy)
	require.Equal(t, e.alice, *row.PublishedBy)
	require.Nil(t, pub.Draft(), "publishing leaves no draft")
	require.Equal(t, "Glossary", row.Title)
}

// "keeps showing the published appendix while a newer draft is being written".
func TestPolicyAppendix_SaveAfterPublishMakesDraftNextVersion(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	_, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)

	saved, changed, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Approval limits (draft)", content("Changed."))
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, "Per-diem rates", saved.Published().Title)
	require.Equal(t, "Approval limits (draft)", saved.Draft().Title)
	require.Equal(t, 2, saved.Draft().VersionNo)

	pub, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	require.Equal(t, "Approval limits (draft)", pub.Published().Title)
	require.Equal(t, [][2]any{{1, domain.AppendixPublished}, {2, domain.AppendixPublished}}, versionNos(pub))
	require.Nil(t, pub.Draft())
}

// "ignores a save of the published title and content" (bug log 246).
func TestPolicyAppendix_UnchangedSaveMakesNoDraft(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Glossary")
	pub, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	p := pub.Published()
	// The same JSON laid out differently still counts as unchanged.
	saved, changed, err := e.st.SaveDraft(e.ctx, a.ID, e.alice, p.Title, "  "+p.ContentJSON+" ")
	require.NoError(t, err)
	require.False(t, changed)
	require.Nil(t, saved.Draft())
	require.Nil(t, e.list(t, false)[0].Draft())
	require.Equal(t, []uuid.UUID{e.bob}, e.list(t, false)[0].ContributorIDs(), "a no-op save adds no contributor")
}

// "makes no draft on a reorder, a retire or a restore" (reorder is row 2, #69).
func TestPolicyAppendix_RetireAndRestoreMakeNoDraft(t *testing.T) {
	e := newAppendixEnv(t)
	var ids []uuid.UUID
	for _, title := range []string{"One", "Two", "Three"} {
		a := e.create(t, title)
		_, err := e.st.Publish(e.ctx, a.ID, e.bob)
		require.NoError(t, err)
		ids = append(ids, a.ID)
	}
	_, _, err := e.st.Retire(e.ctx, ids[0], e.bob)
	require.NoError(t, err)
	_, _, err = e.st.Restore(e.ctx, ids[0])
	require.NoError(t, err)
	for _, a := range e.list(t, true) {
		require.Nil(t, a.Draft(), "%s has a draft", a.ID)
	}
}

func threeAppendices(t *testing.T, e appendixEnv) []uuid.UUID {
	t.Helper()
	var ids []uuid.UUID
	for _, title := range []string{"Per-diem rates", "Quote checklist", "Supplier terms"} {
		a := e.create(t, title)
		_, err := e.st.Publish(e.ctx, a.ID, e.bob)
		require.NoError(t, err)
		ids = append(ids, a.ID)
	}
	return ids
}

// "keeps a retired appendix, hides it from readers, and re-letters the rest".
func TestPolicyAppendix_RetireHidesAndReLetters(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	retired, changed, err := e.st.Retire(e.ctx, ids[0], e.bob)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotNil(t, retired.RetiredAt)
	require.NotNil(t, retired.RetiredBy)
	require.Equal(t, "", retired.Letter, "a retired appendix has no letter")

	live := e.list(t, false)
	require.Len(t, live, 2)
	require.Equal(t, "A", live[0].Letter)
	require.Equal(t, "Quote checklist", live[0].Published().Title)
	require.Equal(t, "B", live[1].Letter)
	require.Equal(t, "Supplier terms", live[1].Published().Title)

	all := e.list(t, true)
	require.Len(t, all, 3, "retired is kept with its history")
	require.Equal(t, ids[0], all[2].ID, "retired ones come last")
	require.Equal(t, "", all[2].Letter)
	require.Len(t, all[2].Versions, 1)

	again, changed, err := e.st.Retire(e.ctx, ids[0], e.bob)
	require.NoError(t, err)
	require.False(t, changed, "retiring twice is a no-op")
	require.Equal(t, retired.RetiredAt, again.RetiredAt)
}

// "restores it to the end of the list".
func TestPolicyAppendix_RestoreGoesLast(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	_, _, err := e.st.Retire(e.ctx, ids[0], e.bob)
	require.NoError(t, err)
	restored, changed, err := e.st.Restore(e.ctx, ids[0])
	require.NoError(t, err)
	require.True(t, changed)
	require.Nil(t, restored.RetiredAt)
	require.Equal(t, "C", restored.Letter)
	require.Equal(t, 2, restored.OrderIndex)

	var got []uuid.UUID
	for _, a := range e.list(t, false) {
		got = append(got, a.ID)
	}
	require.Equal(t, []uuid.UUID{ids[1], ids[2], ids[0]}, got)

	_, changed, err = e.st.Restore(e.ctx, ids[0])
	require.NoError(t, err)
	require.False(t, changed, "restoring a live appendix is a no-op")
}

// "keeps everyone who saved or published it, in the order they first did".
func TestPolicyAppendix_Contributors(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "")
	_, _, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Appendix A", content("Edited."))
	require.NoError(t, err)
	pub, err := e.st.Publish(e.ctx, a.ID, e.alice) // publishing counts as contributing
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{e.bob, e.alice}, pub.ContributorIDs())
	require.Equal(t, []uuid.UUID{e.bob, e.alice}, pub.Versions[0].Editors)
}

// "keeps each version's own contributors".
func TestPolicyAppendix_ContributorsPerVersion(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "")
	_, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	saved, _, err := e.st.SaveDraft(e.ctx, a.ID, e.alice, "Appendix A", content("Second edition."))
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{e.bob}, saved.Versions[0].Editors)
	require.Equal(t, []uuid.UUID{e.alice}, saved.Versions[1].Editors)
	require.Equal(t, []uuid.UUID{e.bob, e.alice}, saved.ContributorIDs())
}

// "lists the published versions newest first, leaving the draft out".
func TestPolicyAppendix_PublishedVersionsNewestFirst(t *testing.T) {
	e := newAppendixEnv(t)
	a := e.create(t, "Per-diem rates")
	for _, text := range []string{"First.", "Second.", "Third."} {
		_, _, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Per-diem rates", content(text))
		require.NoError(t, err)
		_, err = e.st.Publish(e.ctx, a.ID, e.bob)
		require.NoError(t, err)
	}
	_, _, err := e.st.SaveDraft(e.ctx, a.ID, e.bob, "Per-diem rates", content("Fourth, unpublished."))
	require.NoError(t, err)

	got := e.list(t, false)[0]
	var pubNos, allNos []int
	for _, v := range got.PublishedVersions() {
		pubNos = append(pubNos, v.VersionNo)
	}
	for _, v := range got.Versions {
		allNos = append(allNos, v.VersionNo)
	}
	require.Equal(t, []int{3, 2, 1}, pubNos)
	require.Equal(t, []int{1, 2, 3, 4}, allNos, "the version table stays oldest first")
}

func TestPolicyAppendix_Errors(t *testing.T) {
	e := newAppendixEnv(t)
	missing := uuid.New()

	_, err := e.st.Create(e.ctx, missing, e.bob, "x", content("x"))
	require.ErrorIs(t, err, store.ErrAppendixPolicyNotFound)
	_, _, err = e.st.SaveDraft(e.ctx, missing, e.bob, "x", content("x"))
	require.ErrorIs(t, err, store.ErrPolicyAppendixNotFound)
	_, err = e.st.Publish(e.ctx, missing, e.bob)
	require.ErrorIs(t, err, store.ErrPolicyAppendixNotFound)
	_, _, err = e.st.Retire(e.ctx, missing, e.bob)
	require.ErrorIs(t, err, store.ErrPolicyAppendixNotFound)
	_, _, err = e.st.Restore(e.ctx, missing)
	require.ErrorIs(t, err, store.ErrPolicyAppendixNotFound)

	a := e.create(t, "Glossary")
	pub, err := e.st.Publish(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	_, err = e.st.Publish(e.ctx, pub.ID, e.bob)
	require.ErrorIs(t, err, store.ErrAppendixNoDraft, "nothing left to publish")

	_, _, err = e.st.Retire(e.ctx, a.ID, e.bob)
	require.NoError(t, err)
	_, _, err = e.st.SaveDraft(e.ctx, a.ID, e.bob, "x", content("x"))
	require.ErrorIs(t, err, store.ErrAppendixRetired)
	_, err = e.st.Publish(e.ctx, a.ID, e.bob)
	require.ErrorIs(t, err, store.ErrAppendixRetired)

	list, err := e.st.List(e.ctx, uuid.New(), true)
	require.NoError(t, err)
	require.Empty(t, list, "a policy with no appendices lists none")
}

// Two creates at once get different places and titles: the policy row is locked.
func TestPolicyAppendix_ConcurrentCreatesAreSerialised(t *testing.T) {
	e := newAppendixEnv(t)
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.st.Create(e.ctx, e.policy, e.bob, "", content("x"))
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	titles := map[string]bool{}
	for i, a := range e.list(t, false) {
		require.Equal(t, i, a.OrderIndex)
		titles[a.Draft().Title] = true
	}
	require.Len(t, titles, n)
}

// The database allows one draft per appendix, whatever the code does.
func TestPolicyAppendix_OneDraftPerAppendixInTheSchema(t *testing.T) {
	pool := newTestDB(t)
	policy, _ := makeVersion(t, store.NewPolicyStore(pool), pool, "draft")
	st := store.NewPolicyAppendixStore(pool)
	a, err := st.Create(context.Background(), policy, uuid.New(), "x", lexical)
	require.NoError(t, err)
	_, err = pool.Pool().Exec(context.Background(),
		`INSERT INTO policy_appendix_versions (appendix_id, version_no, state, title, content_json, created_by, updated_by)
		 VALUES ($1, 2, 'DRAFT', 'y', '{}', $2, $2)`, a.ID, uuid.New())
	require.Error(t, err, "a second DRAFT row must be refused")
}

func readMigration(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("../../migrations/" + name)
	require.NoError(t, err)
	return string(b)
}

// Migration 0010: down drops the new tables, up carries the version-bound
// appendices over (the mock's migrateAppendices), and the old table is left alone.
func TestMigration0010_UpDownAndCarry(t *testing.T) {
	ctx := context.Background()
	pool := newTestDB(t)
	exec := func(sql string, args ...any) {
		t.Helper()
		_, err := pool.Pool().Exec(ctx, sql, args...)
		require.NoError(t, err)
	}
	tableExists := func(name string) bool {
		var ok bool
		require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, name).Scan(&ok))
		return ok
	}
	up, down := readMigration(t, "0010_policy_appendices.up.sql"), readMigration(t, "0010_policy_appendices.down.sql")

	require.True(t, tableExists("policy_owned_appendices"))
	require.True(t, tableExists("policy_appendix_versions"))
	exec(down)
	require.False(t, tableExists("policy_owned_appendices"))
	require.False(t, tableExists("policy_appendix_versions"))
	require.True(t, tableExists("policy_appendices"), "the version-bound table is untouched")

	// Policy 1: an old published version with one appendix, then a newer published
	// version with two. Only the newer version's appendices carry over, published.
	// Policy 2: a rejected version that never published, with one appendix: it carries as a draft.
	// Policy 3: no appendices at all.
	ps := store.NewPolicyStore(pool)
	p1, _ := makeVersion(t, ps, pool, "draft")
	p2, _ := makeVersion(t, ps, pool, "draft")
	p3, _ := makeVersion(t, ps, pool, "draft")
	author := uuid.New()
	version := func(policy uuid.UUID, no int, status, created string, published bool) uuid.UUID {
		id := uuid.New()
		pub := "NULL"
		if published {
			pub = "'" + created + "'"
		}
		exec(`INSERT INTO policy_versions (id, policy_id, version_no, status, content, created_at, published_at, created_by)
		      VALUES ($1,$2,$3,$4,'{}',$5,`+pub+`,$6)`, id, policy, no, status, created, author)
		return id
	}
	appendix := func(ver uuid.UUID, idx int, title string) uuid.UUID {
		id := uuid.New()
		exec(`INSERT INTO policy_appendices (id, policy_version_id, title, content, order_index) VALUES ($1,$2,$3,$4::jsonb,$5)`,
			id, ver, title, `{"text":"`+title+`"}`, idx)
		return id
	}
	old := version(p1, 1, "superseded", "2026-01-01T00:00:00Z", true)
	appendix(old, 0, "Old appendix")
	cur := version(p1, 2, "published", "2026-02-01T00:00:00Z", true)
	second := appendix(cur, 1, "Second")
	first := appendix(cur, 0, "First")
	unpublished := version(p2, 0, "rejected", "2026-03-01T00:00:00Z", false)
	only := appendix(unpublished, 0, "Only")

	exec(up)

	type row struct {
		id    uuid.UUID
		idx   int
		state string
		title string
		pubBy *uuid.UUID
		eds   []uuid.UUID
	}
	read := func(policy uuid.UUID) []row {
		rows, err := pool.Pool().Query(ctx,
			`SELECT a.id, a.order_index, v.state, v.title, v.published_by, v.editors
			   FROM policy_owned_appendices a JOIN policy_appendix_versions v ON v.appendix_id = a.id
			  WHERE a.policy_id = $1 AND v.version_no = 1 ORDER BY a.order_index`, policy)
		require.NoError(t, err)
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			require.NoError(t, rows.Scan(&r.id, &r.idx, &r.state, &r.title, &r.pubBy, &r.eds))
			out = append(out, r)
		}
		require.NoError(t, rows.Err())
		return out
	}
	got := read(p1)
	require.Len(t, got, 2, "only the newest version that has appendices carries over")
	require.Equal(t, row{first, 0, "PUBLISHED", "First", &author, []uuid.UUID{author}}, got[0], "same id, order from the old order")
	require.Equal(t, row{second, 1, "PUBLISHED", "Second", &author, []uuid.UUID{author}}, got[1])
	d := read(p2)
	require.Len(t, d, 1)
	require.Equal(t, row{only, 0, "DRAFT", "Only", nil, []uuid.UUID{author}}, d[0], "never published: a draft")
	require.Empty(t, read(p3))

	// The carried rows read through the store like any other, and the old table still holds its rows.
	list, err := store.NewPolicyAppendixStore(pool).List(ctx, p1, false)
	require.NoError(t, err)
	require.Equal(t, []string{"A", "B"}, []string{list[0].Letter, list[1].Letter})
	require.Equal(t, "First", list[0].Published().Title)
	var legacy int
	require.NoError(t, pool.Pool().QueryRow(ctx, `SELECT count(*) FROM policy_appendices`).Scan(&legacy))
	require.Equal(t, 4, legacy)

	exec(down)
	require.False(t, tableExists("policy_owned_appendices"))
	exec(up) // and up again, from the carried data's source
	require.True(t, tableExists("policy_appendix_versions"))
}

func appendixIDs(list []domain.PolicyAppendix) []uuid.UUID {
	out := make([]uuid.UUID, len(list))
	for i, a := range list {
		out[i] = a.ID
	}
	return out
}

// appendices.test.ts "order" / "reorders the appendices and re-letters them".
func TestPolicyAppendix_ReorderReLetters(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	want := []uuid.UUID{ids[2], ids[1], ids[0]}
	got, err := e.st.ReorderAppendices(e.ctx, e.policy, want)
	require.NoError(t, err)
	require.Equal(t, want, appendixIDs(got))
	for i, a := range got {
		require.Equal(t, string(rune('A'+i)), a.Letter)
		require.Equal(t, i, a.OrderIndex)
	}
	require.Equal(t, want, appendixIDs(e.list(t, false)), "the order is stored")
}

// appendices.test.ts "makes no draft on a reorder, a retire or a restore".
func TestPolicyAppendix_ReorderMakesNoDraftAndKeepsVersions(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	before := map[uuid.UUID][][2]any{}
	for _, a := range e.list(t, true) {
		before[a.ID] = versionNos(a)
	}
	_, err := e.st.ReorderAppendices(e.ctx, e.policy, []uuid.UUID{ids[2], ids[0], ids[1]})
	require.NoError(t, err)
	for _, a := range e.list(t, true) {
		require.Nil(t, a.Draft(), "%s has a draft", a.ID)
		require.Equal(t, before[a.ID], versionNos(a))
	}
}

// Core rule behind appendices.test.ts "order": every live appendix exactly once.
func TestPolicyAppendix_ReorderRefusesABadListAndChangesNothing(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	other := newAppendixEnv(t)
	foreign := other.create(t, "Foreign")
	_, _, err := e.st.Retire(e.ctx, ids[1], e.bob)
	require.NoError(t, err)
	live := []uuid.UUID{ids[0], ids[2]}
	cases := map[string][]uuid.UUID{
		"missing":                        {ids[0]},
		"empty":                          {},
		"repeated":                       {ids[0], ids[0]},
		"unknown":                        {ids[0], uuid.New()},
		"retired":                        {ids[0], ids[1], ids[2]},
		"retired in place of a live one": {ids[0], ids[1]},
		"foreign":                        {ids[0], foreign.ID},
		"too many":                       {ids[0], ids[2], uuid.New()},
	}
	for name, list := range cases {
		_, err := e.st.ReorderAppendices(e.ctx, e.policy, list)
		require.ErrorIs(t, err, store.ErrAppendixReorderMismatch, name)
	}
	require.Equal(t, live, appendixIDs(e.list(t, false)), "nothing changed")
	require.Equal(t, []uuid.UUID{foreign.ID}, appendixIDs(other.list(t, false)))
}

// appendices.test.ts "keeps a retired appendix ..." with an order: a retired
// appendix keeps no letter and stays out of the order.
func TestPolicyAppendix_ReorderLeavesRetiredOut(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	_, _, err := e.st.Retire(e.ctx, ids[0], e.bob)
	require.NoError(t, err)
	got, err := e.st.ReorderAppendices(e.ctx, e.policy, []uuid.UUID{ids[2], ids[1]})
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{ids[2], ids[1]}, appendixIDs(got))
	require.Equal(t, "A", got[0].Letter)
	require.Equal(t, "B", got[1].Letter)
	all := e.list(t, true)
	require.Equal(t, ids[0], all[2].ID)
	require.Equal(t, "", all[2].Letter)
}

func TestPolicyAppendix_ReorderUnknownPolicyAndNoAppendices(t *testing.T) {
	e := newAppendixEnv(t)
	_, err := e.st.ReorderAppendices(e.ctx, uuid.New(), nil)
	require.ErrorIs(t, err, store.ErrAppendixPolicyNotFound)
	got, err := e.st.ReorderAppendices(e.ctx, e.policy, nil)
	require.NoError(t, err)
	require.Empty(t, got, "a policy with no appendices reorders to nothing")
}

// Two reorders and a create at once leave one consistent order.
func TestPolicyAppendix_ReorderIsSerialisedWithOtherWrites(t *testing.T) {
	e := newAppendixEnv(t)
	ids := threeAppendices(t, e)
	var wg sync.WaitGroup
	for _, order := range [][]uuid.UUID{{ids[2], ids[1], ids[0]}, {ids[1], ids[0], ids[2]}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.st.ReorderAppendices(e.ctx, e.policy, order)
			require.NoError(t, err)
		}()
	}
	wg.Wait()
	for i, a := range e.list(t, false) {
		require.Equal(t, i, a.OrderIndex)
	}
}
