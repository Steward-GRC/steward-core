// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"strings"
	"testing"

	pg "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"

	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/store"
)

type linksEnv struct {
	db     *pg.DB
	assets *store.AssetStore
	ps     *store.PolicyStore
	cat    domain.Category
	tv     domain.TemplateVersion
}

func newLinksEnv(t *testing.T) *linksEnv {
	t.Helper()
	db := newTestDB(t)
	g, tv := setupCategoryAndTemplate(t, store.NewCategoryStore(db), store.NewTemplateStore(db))
	return &linksEnv{db: db, assets: store.NewAssetStore(db), ps: store.NewPolicyStore(db), cat: g, tv: tv}
}

func (e *linksEnv) asset(t *testing.T, name string) domain.Asset {
	t.Helper()
	id := uuid.New()
	a, err := e.assets.Create(context.Background(), domain.Asset{
		ID: id, StorageKey: domain.StorageKeyFor(id), ContentType: "application/pdf",
		SizeBytes: 10, Filename: name, CreatedByUserID: "uploader-1",
	})
	if err != nil {
		t.Fatalf("create asset: %v", err)
	}
	return a
}

func fileDoc(urls ...string) string {
	cards := make([]string, 0, len(urls)+1)
	cards = append(cards, `{"type":"paragraph","version":1,"children":[{"type":"text","text":"body","version":1}]}`)
	for _, u := range urls {
		cards = append(cards, `{"type":"file","src":"`+u+`","fileName":"x.pdf","children":[],"version":1}`)
	}
	return `{"root":{"type":"root","version":1,"children":[` + strings.Join(cards, ",") + `]}}`
}

func (e *linksEnv) policyWithDraft(t *testing.T, title, content string) (domain.Policy, domain.PolicyVersion) {
	t.Helper()
	ctx := context.Background()
	p, _ := domain.NewPolicy(title, e.cat.ID, domain.SensitivityStandard, uuid.New())
	p, err := e.ps.CreatePolicy(ctx, p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	d, _ := domain.NewPolicyVersionDraft(p.ID, e.tv.ID, uuid.New(), content)
	saved, _, err := e.ps.UpsertDraft(ctx, d)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	return p, saved
}

func (e *linksEnv) versionCount(t *testing.T, policyID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.db.Querier().QueryRow(context.Background(),
		`SELECT count(*) FROM policy_versions WHERE policy_id=$1`, policyID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *linksEnv) contentOf(t *testing.T, versionID uuid.UUID) string {
	t.Helper()
	v, err := e.ps.GetPolicyVersion(context.Background(), versionID)
	if err != nil {
		t.Fatal(err)
	}
	return v.ContentJSON
}

func TestAssetStoreListWithLinks_LinkedAndUnlinked(t *testing.T) {
	ctx := context.Background()
	e := newLinksEnv(t)
	linked, unlinked := e.asset(t, "policy.pdf"), e.asset(t, "orphan.pdf")
	p, _ := e.policyWithDraft(t, "Access Policy", fileDoc(linked.URL()))

	all, err := e.assets.ListWithLinks(ctx, false)
	if err != nil {
		t.Fatalf("ListWithLinks: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d files, want 2", len(all))
	}
	by := map[uuid.UUID]domain.LinkedAsset{}
	for _, la := range all {
		by[la.Asset.ID] = la
	}
	if got := by[linked.ID]; len(got.Links) != 1 || got.Links[0].PolicyID != p.ID || got.Links[0].Title != "Access Policy" || got.Links[0].Location != domain.LinkLocationPolicy {
		t.Errorf("linked file links = %+v, want the one policy", got.Links)
	}
	if got := by[unlinked.ID]; len(got.Links) != 0 || got.Asset.CreatedByUserID != "uploader-1" || got.Asset.Filename != "orphan.pdf" {
		t.Errorf("unlinked file = %+v", got)
	}

	only, err := e.assets.ListWithLinks(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 1 || only[0].Asset.ID != unlinked.ID {
		t.Errorf("unlinked filter returned %+v, want only the orphan", only)
	}
}

func TestAssetStoreListWithLinks_PublishedAndAppendixLinksCount(t *testing.T) {
	ctx := context.Background()
	e := newLinksEnv(t)
	inPublished, inAppendix := e.asset(t, "pub.pdf"), e.asset(t, "app.pdf")
	p, _ := e.policyWithDraft(t, "Published", fileDoc(inPublished.URL()))
	if _, err := e.ps.PublishDraft(ctx, p.ID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	var appID uuid.UUID
	if err := e.db.Querier().QueryRow(ctx,
		`INSERT INTO policy_owned_appendices (policy_id, order_index, created_by) VALUES ($1,0,$2) RETURNING id`,
		p.ID, uuid.New()).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Querier().Exec(ctx,
		`INSERT INTO policy_appendix_versions (appendix_id, version_no, state, title, content_json, created_by, updated_by)
		 VALUES ($1,1,'DRAFT','Annex',$2::jsonb,$3,$3)`, appID, fileDoc(inAppendix.URL()), uuid.New()); err != nil {
		t.Fatal(err)
	}

	only, err := e.assets.ListWithLinks(ctx, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(only) != 0 {
		t.Errorf("unlinked = %+v, want none: both files are used", only)
	}
	all, _ := e.assets.ListWithLinks(ctx, false)
	for _, la := range all {
		if la.Asset.ID == inAppendix.ID && (len(la.Links) != 1 || la.Links[0].Location != domain.LinkLocationAppendix || la.Links[0].PolicyID != p.ID) {
			t.Errorf("appendix link = %+v", la.Links)
		}
	}
}

func TestAssetStoreDeleteWithLinks_UnlinkedFile(t *testing.T) {
	ctx := context.Background()
	e := newLinksEnv(t)
	a := e.asset(t, "gone.pdf")

	got, err := e.assets.DeleteWithLinks(ctx, a.ID)
	if err != nil {
		t.Fatalf("DeleteWithLinks: %v", err)
	}
	if got.Asset.ID != a.ID || got.Asset.Filename != "gone.pdf" || len(got.Links) != 0 {
		t.Errorf("result = %+v", got)
	}
	if _, err := e.assets.Get(ctx, a.ID); err != store.ErrAssetNotFound {
		t.Errorf("Get after delete = %v, want ErrAssetNotFound", err)
	}
	if _, err := e.assets.DeleteWithLinks(ctx, a.ID); err != store.ErrAssetNotFound {
		t.Errorf("second delete = %v, want ErrAssetNotFound", err)
	}
}

func TestAssetStoreDeleteWithLinks_LinkedFileLeavesDraftAndPublishedWithoutANewVersion(t *testing.T) {
	ctx := context.Background()
	e := newLinksEnv(t)
	doomed, other := e.asset(t, "leak.pdf"), e.asset(t, "fine.pdf")

	p, draft := e.policyWithDraft(t, "Has Both", fileDoc(doomed.URL(), other.URL()))
	pub, err := e.ps.PublishDraft(ctx, p.ID, uuid.New())
	if err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	// A new draft beside the published version, also linking the file.
	nd, _ := domain.NewPolicyVersionDraft(p.ID, e.tv.ID, uuid.New(), fileDoc(doomed.URL(), other.URL()))
	newDraft, _, err := e.ps.UpsertDraft(ctx, nd)
	if err != nil {
		t.Fatalf("UpsertDraft: %v", err)
	}
	_ = draft
	before := e.versionCount(t, p.ID)
	var updatedBefore string
	_ = e.db.Querier().QueryRow(ctx, `SELECT updated_at::text FROM policies WHERE id=$1`, p.ID).Scan(&updatedBefore)

	res, err := e.assets.DeleteWithLinks(ctx, doomed.ID)
	if err != nil {
		t.Fatalf("DeleteWithLinks: %v", err)
	}
	if len(res.Links) != 1 || res.Links[0].PolicyID != p.ID {
		t.Errorf("removed from %+v, want the one policy once", res.Links)
	}
	for name, id := range map[string]uuid.UUID{"published": pub.ID, "draft": newDraft.ID} {
		c := e.contentOf(t, id)
		if strings.Contains(c, doomed.ID.String()) {
			t.Errorf("%s still links the deleted file: %s", name, c)
		}
		if !strings.Contains(c, other.ID.String()) || !strings.Contains(c, "body") {
			t.Errorf("%s lost other content: %s", name, c)
		}
	}
	if after := e.versionCount(t, p.ID); after != before {
		t.Errorf("version rows %d -> %d: a delete must create no version", before, after)
	}
	var updatedAfter string
	_ = e.db.Querier().QueryRow(ctx, `SELECT updated_at::text FROM policies WHERE id=$1`, p.ID).Scan(&updatedAfter)
	if updatedAfter != updatedBefore {
		t.Errorf("policy updated_at moved: the delete is not a document change")
	}
	if _, err := e.assets.Get(ctx, doomed.ID); err != store.ErrAssetNotFound {
		t.Errorf("row survived: %v", err)
	}
	if _, err := e.assets.Get(ctx, other.ID); err != nil {
		t.Errorf("another file was touched: %v", err)
	}
}

func TestAssetStoreDeleteWithLinks_RewritesAppendixAndReviewSnapshot(t *testing.T) {
	ctx := context.Background()
	e := newLinksEnv(t)
	doomed := e.asset(t, "leak.pdf")
	p, v := e.policyWithDraft(t, "With Annex", fileDoc(doomed.URL()))

	var appID uuid.UUID
	if err := e.db.Querier().QueryRow(ctx,
		`INSERT INTO policy_owned_appendices (policy_id, order_index, created_by) VALUES ($1,0,$2) RETURNING id`,
		p.ID, uuid.New()).Scan(&appID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Querier().Exec(ctx,
		`INSERT INTO policy_appendix_versions (appendix_id, version_no, state, title, content_json, created_by, updated_by)
		 VALUES ($1,1,'DRAFT','Annex',$2::jsonb,$3,$3)`, appID, fileDoc(doomed.URL()), uuid.New()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Querier().Exec(ctx,
		`INSERT INTO review_snapshots (policy_version_id, user_id, content) VALUES ($1,$2,$3::jsonb)`,
		v.ID, uuid.New(), fileDoc(doomed.URL())); err != nil {
		t.Fatal(err)
	}

	res, err := e.assets.DeleteWithLinks(ctx, doomed.ID)
	if err != nil {
		t.Fatalf("DeleteWithLinks: %v", err)
	}
	locs := map[string]bool{}
	for _, l := range res.Links {
		locs[l.Location] = true
	}
	if !locs[domain.LinkLocationPolicy] || !locs[domain.LinkLocationAppendix] {
		t.Errorf("links = %+v, want the policy and its appendix", res.Links)
	}
	for _, q := range []string{
		`SELECT count(*) FROM policy_appendix_versions WHERE content_json::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM review_snapshots WHERE content::text LIKE '%' || $1 || '%'`,
		`SELECT count(*) FROM policy_versions WHERE content::text LIKE '%' || $1 || '%'`,
	} {
		var n int
		if err := e.db.Querier().QueryRow(ctx, q, doomed.ID.String()).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%d rows still hold the link (%s)", n, q)
		}
	}
}
