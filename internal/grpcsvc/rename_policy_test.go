// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package grpcsvc_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pg "github.com/Bugs5382/go-postgres"
	corev1 "github.com/Steward-GRC/steward-core/gen/go/steward/core/v1"
	"github.com/Steward-GRC/steward-core/internal/audit"
	"github.com/Steward-GRC/steward-core/internal/domain"
	"github.com/Steward-GRC/steward-core/internal/grpcsvc"
	"github.com/Steward-GRC/steward-core/internal/store"
)

// RenamePolicy runs against real Postgres: the fake store can't show that no
// new version row appears or that publish clears proposed_title.

func renameTestDB(t *testing.T) *pg.DB {
	t.Helper()
	if dsn := os.Getenv("DATABASE_TEST_DSN"); dsn != "" {
		return renameTestDBFromEnv(t, dsn)
	}
	return renameTestDBFromContainer(t)
}

func renameTestDBFromEnv(t *testing.T, baseDSN string) *pg.DB {
	t.Helper()
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, baseDSN)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	defer admin.Close()
	var b [6]byte
	_, _ = rand.Read(b[:])
	dbName := "test_" + hex.EncodeToString(b[:])
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %q`, dbName)); err != nil {
		t.Fatalf("create db %s: %v", dbName, err)
	}
	u, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatalf("parse base dsn: %v", err)
	}
	u.Path = "/" + dbName
	testDSN := u.String()
	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if err := pg.Migrate(testDSN, migrationsDir); err != nil {
		t.Fatalf("migrate %s: %v", dbName, err)
	}
	pool, err := pg.New(ctx, testDSN)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		dropAdmin, err := pgxpool.New(dropCtx, baseDSN)
		if err != nil {
			return
		}
		defer dropAdmin.Close()
		_, _ = dropAdmin.Exec(dropCtx, fmt.Sprintf(`DROP DATABASE %q WITH (FORCE)`, dbName))
	})
	return pool
}

func renameTestDBFromContainer(t *testing.T) *pg.DB {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("policy_test"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get connection string: %v", err)
	}
	migrationsDir, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatalf("resolve migrations dir: %v", err)
	}
	if err := pg.Migrate(dsn, migrationsDir); err != nil {
		t.Fatalf("run migrations: %v", err)
	}
	pool, err := pg.New(ctx, dsn)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

type renameEnv struct {
	pool *pg.DB
	h    *grpcsvc.PolicyHandler
	cap  *capturePublisher
	ps   *store.PolicyStore
	gid  uuid.UUID
}

func setupRenameEnv(t *testing.T) renameEnv {
	t.Helper()
	ctx := context.Background()
	pool := renameTestDB(t)
	gs := store.NewCategoryStore(pool)
	ts := store.NewTemplateStore(pool)
	ps := store.NewPolicyStore(pool)
	as := store.NewAppendixStore(pool)

	g, err := domain.NewCategory("IT", "it-"+uuid.New().String()[:8], uuid.Nil)
	if err != nil {
		t.Fatalf("NewCategory: %v", err)
	}
	g, err = gs.Create(ctx, g)
	if err != nil {
		t.Fatalf("gs.Create: %v", err)
	}

	cap := &capturePublisher{}
	em := audit.New(cap)
	h := grpcsvc.NewPolicyHandler(ps, gs, ts, domain.NoopValidator{}, em).WithAppendixCopier(as)
	return renameEnv{pool: pool, h: h, cap: cap, ps: ps, gid: g.ID}
}

func (e renameEnv) createPolicy(t *testing.T, title string) domain.Policy {
	t.Helper()
	p, err := domain.NewPolicy(title, e.gid, domain.SensitivityStandard, uuid.New())
	if err != nil {
		t.Fatalf("NewPolicy: %v", err)
	}
	p, err = e.ps.CreatePolicy(context.Background(), p)
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	return p
}

func (e renameEnv) versionCount(t *testing.T, policyID uuid.UUID) int {
	t.Helper()
	var n int
	if err := e.pool.Pool().QueryRow(context.Background(),
		`SELECT count(*) FROM policy_versions WHERE policy_id = $1`, policyID).Scan(&n); err != nil {
		t.Fatalf("count versions: %v", err)
	}
	return n
}

func (e renameEnv) hasAudit(action string) bool {
	for _, c := range e.cap.calls {
		if c.event.Action == action {
			return true
		}
	}
	return false
}

func TestRenamePolicyNeverPublishedInPlace(t *testing.T) {
	e := setupRenameEnv(t)
	ctx := context.Background()
	p := e.createPolicy(t, "Old Title")

	before := e.versionCount(t, p.ID) // the seeded draft

	resp, err := e.h.RenamePolicy(ctx, &corev1.RenamePolicyRequest{
		PolicyId: p.ID.String(), NewTitle: "New Title", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("RenamePolicy: %v", err)
	}
	if resp.GetStaged() {
		t.Fatalf("expected staged=false for never-published policy")
	}
	if resp.GetDraftVersionId() != "" {
		t.Fatalf("expected empty draft_version_id, got %q", resp.GetDraftVersionId())
	}
	if resp.GetPolicy().GetTitle() != "New Title" {
		t.Fatalf("response title: got %q want %q", resp.GetPolicy().GetTitle(), "New Title")
	}
	got, err := e.ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Title != "New Title" {
		t.Fatalf("persisted title: got %q want %q", got.Title, "New Title")
	}
	if after := e.versionCount(t, p.ID); after != before {
		t.Fatalf("version count changed: before=%d after=%d (expected no new version)", before, after)
	}
	if !e.hasAudit("policy.renamed") {
		t.Fatalf("expected policy.renamed audit event, got %v", e.cap.calls)
	}
}

func (e renameEnv) publish(t *testing.T, policyID uuid.UUID) {
	t.Helper()
	if _, err := e.ps.PublishDraft(context.Background(), policyID, uuid.New()); err != nil {
		t.Fatalf("PublishDraft(store): %v", err)
	}
}

func TestRenamePolicyPublishedStagesOnNewDraft(t *testing.T) {
	e := setupRenameEnv(t)
	ctx := context.Background()
	p := e.createPolicy(t, "Live Title")
	e.publish(t, p.ID) // draft -> published, no draft remains

	before := e.versionCount(t, p.ID) // 1 published

	resp, err := e.h.RenamePolicy(ctx, &corev1.RenamePolicyRequest{
		PolicyId: p.ID.String(), NewTitle: "Proposed Title", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("RenamePolicy: %v", err)
	}
	if !resp.GetStaged() {
		t.Fatalf("expected staged=true for published policy")
	}
	if resp.GetDraftVersionId() == "" {
		t.Fatalf("expected non-empty draft_version_id")
	}
	got, err := e.ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Title != "Live Title" {
		t.Fatalf("title should be unchanged: got %q want %q", got.Title, "Live Title")
	}
	if after := e.versionCount(t, p.ID); after != before+1 {
		t.Fatalf("expected one new draft row: before=%d after=%d", before, after)
	}
	draftID, _ := uuid.Parse(resp.GetDraftVersionId())
	draft, err := e.ps.GetPolicyVersion(ctx, draftID)
	if err != nil {
		t.Fatalf("GetPolicyVersion(draft): %v", err)
	}
	if draft.Status != domain.PolicyVersionStatusDraft {
		t.Fatalf("expected draft status, got %q", draft.Status)
	}
	if draft.ProposedTitle == nil || *draft.ProposedTitle != "Proposed Title" {
		t.Fatalf("proposed_title: got %v want %q", draft.ProposedTitle, "Proposed Title")
	}
	if !e.hasAudit("policy.rename_staged") {
		t.Fatalf("expected policy.rename_staged audit, got %v", e.cap.calls)
	}
}

func TestRenamePolicyPublishedReusesExistingDraft(t *testing.T) {
	e := setupRenameEnv(t)
	ctx := context.Background()
	p := e.createPolicy(t, "Live Title")
	e.publish(t, p.ID)

	sd, err := e.h.SaveDraft(ctx, &corev1.SaveDraftRequest{
		PolicyId: p.ID.String(), ContentJson: "{}", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	existingDraftID := sd.GetVersion().GetId()
	before := e.versionCount(t, p.ID)

	resp, err := e.h.RenamePolicy(ctx, &corev1.RenamePolicyRequest{
		PolicyId: p.ID.String(), NewTitle: "Renamed", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("RenamePolicy: %v", err)
	}
	if resp.GetDraftVersionId() != existingDraftID {
		t.Fatalf("expected reuse of existing draft %q, got %q", existingDraftID, resp.GetDraftVersionId())
	}
	if after := e.versionCount(t, p.ID); after != before {
		t.Fatalf("no new draft expected: before=%d after=%d", before, after)
	}
	draftID, _ := uuid.Parse(existingDraftID)
	draft, err := e.ps.GetPolicyVersion(ctx, draftID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	if draft.ProposedTitle == nil || *draft.ProposedTitle != "Renamed" {
		t.Fatalf("proposed_title on existing draft: got %v want %q", draft.ProposedTitle, "Renamed")
	}
}

func TestPublishDraftAppliesProposedTitle(t *testing.T) {
	e := setupRenameEnv(t)
	ctx := context.Background()
	p := e.createPolicy(t, "Live Title")
	e.publish(t, p.ID)

	resp, err := e.h.RenamePolicy(ctx, &corev1.RenamePolicyRequest{
		PolicyId: p.ID.String(), NewTitle: "Approved Title", ActorUserId: uuid.New().String(),
	})
	if err != nil {
		t.Fatalf("RenamePolicy: %v", err)
	}
	draftID, _ := uuid.Parse(resp.GetDraftVersionId())

	e.cap.calls = nil

	if _, err := e.h.PublishDraft(ctx, &corev1.PublishDraftRequest{
		PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}

	got, err := e.ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Title != "Approved Title" {
		t.Fatalf("title after publish: got %q want %q", got.Title, "Approved Title")
	}
	pub, err := e.ps.GetPolicyVersion(ctx, draftID)
	if err != nil {
		t.Fatalf("GetPolicyVersion: %v", err)
	}
	if pub.ProposedTitle != nil {
		t.Fatalf("proposed_title should be cleared on publish, got %v", *pub.ProposedTitle)
	}
	if !e.hasAudit("policy.renamed") {
		t.Fatalf("expected policy.renamed audit on publish, got %v", e.cap.calls)
	}
}

func TestPublishDraftNoProposedTitleKeepsTitle(t *testing.T) {
	e := setupRenameEnv(t)
	ctx := context.Background()
	p := e.createPolicy(t, "Stable Title")
	e.publish(t, p.ID)

	if _, err := e.h.SaveDraft(ctx, &corev1.SaveDraftRequest{
		PolicyId: p.ID.String(), ContentJson: "{}", ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("SaveDraft: %v", err)
	}
	e.cap.calls = nil

	if _, err := e.h.PublishDraft(ctx, &corev1.PublishDraftRequest{
		PolicyId: p.ID.String(), ActorUserId: uuid.New().String(),
	}); err != nil {
		t.Fatalf("PublishDraft: %v", err)
	}
	got, err := e.ps.GetPolicy(ctx, p.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.Title != "Stable Title" {
		t.Fatalf("title should be unchanged: got %q want %q", got.Title, "Stable Title")
	}
	if e.hasAudit("policy.renamed") {
		t.Fatalf("did not expect policy.renamed audit for a plain publish")
	}
}
