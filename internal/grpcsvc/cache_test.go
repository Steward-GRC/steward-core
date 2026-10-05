// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"slices"
	"testing"

	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
	"github.com/google/uuid"
)

// fakeCache records deletes and hit, miss and set counts, so cache-aside and
// invalidation on write can be checked without Redis.
type fakeCache struct {
	data   map[string][]byte
	dels   []string
	hits   int
	misses int
	sets   int
}

func newFakeCache() *fakeCache { return &fakeCache{data: map[string][]byte{}} }

func (c *fakeCache) GetBytes(_ context.Context, key string) ([]byte, bool) {
	b, ok := c.data[key]
	if ok {
		c.hits++
	} else {
		c.misses++
	}
	return b, ok
}

func (c *fakeCache) SetBytes(_ context.Context, key string, val []byte) {
	c.sets++
	c.data[key] = append([]byte(nil), val...)
}

func (c *fakeCache) Del(_ context.Context, key string) {
	c.dels = append(c.dels, key)
	delete(c.data, key)
}

func (c *fakeCache) deleted(key string) bool {
	return slices.Contains(c.dels, key)
}

func seedPublishedPair(fakePS *fakePolicyStore, fakeTS *fakeTemplateStore) (fromID, toID, tvID uuid.UUID) {
	tvID = uuid.New()
	fakeTS.versions[tvID] = domain.TemplateVersion{
		ID:       tvID,
		Status:   domain.TemplateVersionStatusPublished,
		Sections: []domain.Section{{Key: "body", Title: "Body", Order: 0}},
	}
	policyID := uuid.New()
	fromID = uuid.New()
	toID = uuid.New()
	fakePS.versions[fromID] = domain.PolicyVersion{
		ID: fromID, PolicyID: policyID, Status: domain.PolicyVersionStatusPublished,
		TemplateVersionID: tvID, ContentJSON: `{"sections":{"body":"old text"}}`,
	}
	fakePS.versions[toID] = domain.PolicyVersion{
		ID: toID, PolicyID: policyID, Status: domain.PolicyVersionStatusPublished,
		TemplateVersionID: tvID, ContentJSON: `{"sections":{"body":"new text"}}`,
	}
	return fromID, toID, tvID
}

func TestDiffVersionsCachesPublishedPair(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	c := newFakeCache()
	fromID, toID, _ := seedPublishedPair(fakePS, fakeTS)

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), fakeTS, domain.NoopValidator{}, nil).
		WithVersionCache(c)
	req := &corev1.DiffVersionsRequest{FromVersionId: fromID.String(), ToVersionId: toID.String()}

	first, err := h.DiffVersions(context.Background(), req)
	if err != nil {
		t.Fatalf("DiffVersions (miss): %v", err)
	}
	key := "poldiff:" + fromID.String() + ":" + toID.String()
	if _, ok := c.data[key]; !ok {
		t.Fatalf("expected diff cached under %q", key)
	}
	if c.sets != 1 || c.misses != 1 {
		t.Fatalf("want 1 set / 1 miss, got sets=%d misses=%d", c.sets, c.misses)
	}

	// With the rows gone, only the cache can answer.
	delete(fakePS.versions, fromID)
	delete(fakePS.versions, toID)

	second, err := h.DiffVersions(context.Background(), req)
	if err != nil {
		t.Fatalf("DiffVersions (hit): %v", err)
	}
	if c.hits != 1 {
		t.Fatalf("want 1 cache hit, got %d", c.hits)
	}
	if len(first.GetDiffs()) != len(second.GetDiffs()) {
		t.Fatalf("cached diff mismatch: first=%d second=%d", len(first.GetDiffs()), len(second.GetDiffs()))
	}
}

func TestDiffVersionsDoesNotCacheDraftSide(t *testing.T) {
	fakePS := newFakePolicyStore()
	fakeTS := newFakeTemplateStore()
	c := newFakeCache()
	fromID, toID, _ := seedPublishedPair(fakePS, fakeTS)
	toPV := fakePS.versions[toID]
	toPV.Status = domain.PolicyVersionStatusDraft
	fakePS.versions[toID] = toPV

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), fakeTS, domain.NoopValidator{}, nil).
		WithVersionCache(c)
	if _, err := h.DiffVersions(context.Background(), &corev1.DiffVersionsRequest{
		FromVersionId: fromID.String(), ToVersionId: toID.String(),
	}); err != nil {
		t.Fatalf("DiffVersions: %v", err)
	}
	if c.sets != 0 || len(c.data) != 0 {
		t.Fatalf("diff involving a draft must not be cached: sets=%d keys=%d", c.sets, len(c.data))
	}
}

func seedPublishedList(fakePS *fakePolicyStore) (policyID uuid.UUID) {
	policyID = uuid.New()
	for i := range 2 {
		id := uuid.New()
		fakePS.versions[id] = domain.PolicyVersion{
			ID: id, PolicyID: policyID, Status: domain.PolicyVersionStatusPublished, VersionNo: i + 1,
		}
	}
	return policyID
}

func TestListPolicyVersionsCacheHit(t *testing.T) {
	fakePS := newFakePolicyStore()
	c := newFakeCache()
	policyID := seedPublishedList(fakePS)

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil).
		WithVersionCache(c)
	req := &corev1.ListPolicyVersionsRequest{PolicyId: policyID.String()}

	first, err := h.ListPolicyVersions(context.Background(), req)
	if err != nil {
		t.Fatalf("ListPolicyVersions (miss): %v", err)
	}
	if len(first.GetVersions()) != 2 {
		t.Fatalf("want 2 versions, got %d", len(first.GetVersions()))
	}
	key := "polvers:" + policyID.String()
	if _, ok := c.data[key]; !ok {
		t.Fatalf("expected list cached under %q", key)
	}

	// With the rows gone, only the cache can answer.
	fakePS.versions = map[uuid.UUID]domain.PolicyVersion{}

	second, err := h.ListPolicyVersions(context.Background(), req)
	if err != nil {
		t.Fatalf("ListPolicyVersions (hit): %v", err)
	}
	if c.hits != 1 || len(second.GetVersions()) != 2 {
		t.Fatalf("want cache hit with 2 versions, got hits=%d versions=%d", c.hits, len(second.GetVersions()))
	}
}

func TestPublishDraftInvalidatesVersionList(t *testing.T) {
	fakePS := newFakePolicyStore()
	c := newFakeCache()

	p, err := domain.NewPolicy("Cache Policy", uuid.New(), domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	fakePS.policies[p.ID] = p
	draft, err := domain.NewPolicyVersionDraft(p.ID, uuid.New(), uuid.New(), `{"sections":{}}`)
	if err != nil {
		t.Fatalf("NewPolicyVersionDraft: %v", err)
	}
	fakePS.draftByPolicy[p.ID] = draft

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil).
		WithVersionCache(c)
	if _, err := h.ListPolicyVersions(context.Background(), &corev1.ListPolicyVersionsRequest{PolicyId: p.ID.String()}); err != nil {
		t.Fatalf("prime ListPolicyVersions: %v", err)
	}
	if _, err := h.PublishDraft(context.Background(), &corev1.PublishDraftRequest{PolicyId: p.ID.String(), ActorUserId: uuid.New().String()}); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	if !c.deleted("polvers:" + p.ID.String()) {
		t.Fatalf("PublishDraft must bust polvers:%s (dels=%v)", p.ID, c.dels)
	}
}

func TestSetVersionStatusInvalidatesCaches(t *testing.T) {
	fakePS := newFakePolicyStore()
	c := newFakeCache()

	policyID := uuid.New()
	verID := uuid.New()
	fakePS.policies[policyID] = domain.Policy{ID: policyID, CurrentPublishedVersionID: verID}
	fakePS.versions[verID] = domain.PolicyVersion{
		ID: verID, PolicyID: policyID, Status: domain.PolicyVersionStatusPublished, VersionNo: 1,
	}

	h := grpcsvc.NewPolicyHandler(fakePS, newFakeCategoryStore(), newFakeTemplateStore(), domain.NoopValidator{}, nil).
		WithVersionCache(c)
	if _, err := h.SetVersionStatus(context.Background(), &corev1.SetVersionStatusRequest{
		PolicyVersionId: verID.String(),
		Status:          string(domain.PolicyVersionStatusSuperseded),
		ActorUserId:     uuid.New().String(),
	}); err != nil {
		t.Fatalf("SetVersionStatus: %v", err)
	}
	if !c.deleted("polver:" + verID.String()) {
		t.Fatalf("expected polver:%s busted (dels=%v)", verID, c.dels)
	}
	if !c.deleted("polvers:" + policyID.String()) {
		t.Fatalf("expected polvers:%s busted (dels=%v)", policyID, c.dels)
	}
}

// fakeSettingsStore counts reads, so cache-aside is observable.
type fakeSettingsStore struct {
	gs    store.GlobalSettings
	reads int
}

func (f *fakeSettingsStore) GetGlobalSettings(_ context.Context) (store.GlobalSettings, error) {
	f.reads++
	return f.gs, nil
}

func (f *fakeSettingsStore) SetGlobalSettings(_ context.Context, gs store.GlobalSettings) (store.GlobalSettings, error) {
	f.gs = gs
	return gs, nil
}

func TestGetGlobalSettingsCacheAsideAndInvalidation(t *testing.T) {
	fss := &fakeSettingsStore{gs: store.GlobalSettings{AnnouncementLevel: "info", AnnouncementMessage: "v1"}}
	c := newFakeCache()
	h := grpcsvc.NewSettingsHandler(fss, nil).WithCache(c)

	if _, err := h.GetGlobalSettings(context.Background(), &corev1.GetGlobalSettingsRequest{}); err != nil {
		t.Fatalf("GetGlobalSettings (miss): %v", err)
	}
	if fss.reads != 1 || c.sets != 1 {
		t.Fatalf("want 1 store read / 1 set, got reads=%d sets=%d", fss.reads, c.sets)
	}

	if _, err := h.GetGlobalSettings(context.Background(), &corev1.GetGlobalSettingsRequest{}); err != nil {
		t.Fatalf("GetGlobalSettings (hit): %v", err)
	}
	if fss.reads != 1 {
		t.Fatalf("cache hit must not read the store: reads=%d", fss.reads)
	}
	if c.hits != 1 {
		t.Fatalf("want 1 cache hit, got %d", c.hits)
	}

	if _, err := h.SetGlobalSettings(context.Background(), &corev1.SetGlobalSettingsRequest{
		Settings: &corev1.GlobalSettings{Announcement: &corev1.Announcement{Level: "warning", Message: "v2"}},
	}); err != nil {
		t.Fatalf("SetGlobalSettings: %v", err)
	}
	if !c.deleted("polsettings:global") {
		t.Fatalf("SetGlobalSettings must bust polsettings:global (dels=%v)", c.dels)
	}

	resp, err := h.GetGlobalSettings(context.Background(), &corev1.GetGlobalSettingsRequest{})
	if err != nil {
		t.Fatalf("GetGlobalSettings (post-write): %v", err)
	}
	if fss.reads != 2 {
		t.Fatalf("post-invalidation read must hit the store: reads=%d", fss.reads)
	}
	if got := resp.GetSettings().GetAnnouncement().GetMessage(); got != "v2" {
		t.Fatalf("want fresh message v2, got %q", got)
	}
}
