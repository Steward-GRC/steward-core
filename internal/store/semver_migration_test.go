// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	pg "github.com/Bugs5382/go-postgres"
)

// TestMigration0036BackfillsNumberedVersions applies every migration before 0036, stores
// versions the way the older schema did, then applies 0036 and checks the numbered ones read as
// version_no.0.0 and the unnumbered ones as 0.0.0, which is what the web already showed.
func TestMigration0036BackfillsNumberedVersions(t *testing.T) {
	ctx := context.Background()
	container, err := postgres.Run(ctx, "postgres:16",
		postgres.WithDatabase("migrate_test"), postgres.WithUsername("test"), postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(wait.ForLog("database system is ready to accept connections").
			WithOccurrence(2).WithStartupTimeout(60*time.Second)))
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() { _ = container.Terminate(context.Background()) })
	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	// The migrations before 0036, copied to a folder of their own.
	all, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	before := t.TempDir()
	entries, err := os.ReadDir(all)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "0036_") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(all, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(before, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pg.Migrate(dsn, before); err != nil {
		t.Fatalf("migrate to 0035: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	mustExec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	mustExec(`INSERT INTO categories (id, name, slug) VALUES ('00000000-0000-0000-0000-0000000000c1', 'C', 'c')`)
	mustExec(`INSERT INTO templates (id, name, code) VALUES ('00000000-0000-0000-0000-0000000000a1', 'T', 'TPL-901')`)
	mustExec(`INSERT INTO template_versions (id, template_id, version_no, status, sections) VALUES
	  ('00000000-0000-0000-0000-0000000000b1', '00000000-0000-0000-0000-0000000000a1', 1, 'published', '[]'),
	  ('00000000-0000-0000-0000-0000000000b2', '00000000-0000-0000-0000-0000000000a1', 2, 'draft', '[]')`)
	mustExec(`INSERT INTO policies (id, home_category_id, title, owner_user_id, sequence)
	  VALUES ('00000000-0000-0000-0000-0000000000d1', '00000000-0000-0000-0000-0000000000c1', 'P', gen_random_uuid(), 1)`)
	mustExec(`INSERT INTO policy_versions (id, policy_id, version_no, status, created_by) VALUES
	  ('00000000-0000-0000-0000-0000000000e1', '00000000-0000-0000-0000-0000000000d1', 1, 'superseded', gen_random_uuid()),
	  ('00000000-0000-0000-0000-0000000000e2', '00000000-0000-0000-0000-0000000000d1', 2, 'published', gen_random_uuid()),
	  ('00000000-0000-0000-0000-0000000000e3', '00000000-0000-0000-0000-0000000000d1', 0, 'draft', gen_random_uuid())`)

	if err := pg.Migrate(dsn, all); err != nil {
		t.Fatalf("migrate 0036: %v", err)
	}

	type row struct{ major, minor, fix int }
	want := map[string]row{
		"00000000-0000-0000-0000-0000000000e1": {1, 0, 0},
		"00000000-0000-0000-0000-0000000000e2": {2, 0, 0},
		"00000000-0000-0000-0000-0000000000e3": {0, 0, 0},
	}
	for id, w := range want {
		var got row
		if err := pool.QueryRow(ctx, `SELECT major, minor, fix FROM policy_versions WHERE id = $1`, id).Scan(&got.major, &got.minor, &got.fix); err != nil {
			t.Fatal(err)
		}
		if got != w {
			t.Fatalf("policy version %s = %+v, want %+v", id, got, w)
		}
	}
	var pubMajor int
	var published *time.Time
	if err := pool.QueryRow(ctx, `SELECT major, published_at FROM template_versions WHERE id = '00000000-0000-0000-0000-0000000000b1'`).Scan(&pubMajor, &published); err != nil {
		t.Fatal(err)
	}
	if pubMajor != 1 || published == nil {
		t.Fatalf("published template version = major %d, published_at %v", pubMajor, published)
	}
	var draftMajor int
	var draftPub *time.Time
	if err := pool.QueryRow(ctx, `SELECT major, published_at FROM template_versions WHERE id = '00000000-0000-0000-0000-0000000000b2'`).Scan(&draftMajor, &draftPub); err != nil {
		t.Fatal(err)
	}
	if draftMajor != 0 || draftPub != nil {
		t.Fatalf("draft template version = major %d, published_at %v", draftMajor, draftPub)
	}

	// The down migration undoes it.
	down, err := os.ReadFile(filepath.Join(all, "0036_semantic_versions.down.sql"))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(string(down))
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM information_schema.columns WHERE table_name IN ('policy_versions', 'template_versions') AND column_name IN ('major', 'minor', 'fix', 'change_type')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("columns left after the down migration: %d, %v", n, err)
	}
}
