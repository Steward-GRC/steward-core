// Copyright 2026 The Steward Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	postgres "github.com/Bugs5382/go-postgres"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Steward-GRC/steward-core/internal/domain"
)

// ErrAssetNotFound is returned when an asset lookup targets a missing id.
// Callers map it to codes.NotFound.
var ErrAssetNotFound = errors.New("asset not found")

// AssetStore persists editor-image metadata. The bytes live in object storage
// under StorageKey.
type AssetStore struct{ db *postgres.DB }

// NewAssetStore returns an AssetStore on db.
func NewAssetStore(db *postgres.DB) *AssetStore { return &AssetStore{db: db} }

const assetCols = `id, storage_key, content_type, size_bytes, filename, created_by_user_id, created_at`

func scanAsset(row pgx.Row) (domain.Asset, error) {
	var a domain.Asset
	if err := row.Scan(&a.ID, &a.StorageKey, &a.ContentType, &a.SizeBytes, &a.Filename, &a.CreatedByUserID, &a.CreatedAt); err != nil {
		return domain.Asset{}, err
	}
	return a, nil
}

// Create inserts an asset row. The caller picks the id, which StorageKey is
// built from, so the object can be written before the row exists.
func (s *AssetStore) Create(ctx context.Context, a domain.Asset) (domain.Asset, error) {
	created, err := scanAsset(s.db.Querier().QueryRow(ctx, `
		INSERT INTO editor_assets (id, storage_key, content_type, size_bytes, filename, created_by_user_id)
		VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING `+assetCols,
		a.ID, a.StorageKey, a.ContentType, a.SizeBytes, a.Filename, a.CreatedByUserID))
	if err != nil {
		return domain.Asset{}, fmt.Errorf("insert editor_asset: %w", err)
	}
	return created, nil
}

// Get loads an asset metadata row by id, returning ErrAssetNotFound when absent.
func (s *AssetStore) Get(ctx context.Context, id uuid.UUID) (domain.Asset, error) {
	a, err := scanAsset(s.db.Querier().QueryRow(ctx, `SELECT `+assetCols+` FROM editor_assets WHERE id = $1`, id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.Asset{}, ErrAssetNotFound
		}
		return domain.Asset{}, fmt.Errorf("get editor_asset: %w", err)
	}
	return a, nil
}

// assetRefPattern matches the url the editor embeds for a stored file and
// captures its id. The url carries a fixed-length id, so a match is exact.
const assetRefPattern = `/api/assets/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})`

// Which documents link to which file is read from the stored content itself,
// not from a table kept beside it, so it can never fall behind a save: every
// path that writes content (a draft save, a save in review, an appendix, a
// publish, a copy forward) is covered without being touched.
//
// assetRefsSQL returns one row per (file, document, place). The legacy
// per-version appendices and the policy-owned appendix versions are both
// live, so both are read.
const assetRefsSQL = `
SELECT DISTINCT r.asset_id, d.policy_id, d.title, d.loc FROM (
  SELECT p.id AS policy_id, p.title, 'policy' AS loc, pv.content::text AS body
    FROM policy_versions pv JOIN policies p ON p.id = pv.policy_id
  UNION ALL
  SELECT p.id, p.title, 'appendix', a.content::text
    FROM policy_appendices a
    JOIN policy_versions pv ON pv.id = a.policy_version_id
    JOIN policies p ON p.id = pv.policy_id
  UNION ALL
  SELECT p.id, p.title, 'appendix', av.content_json::text
    FROM policy_appendix_versions av
    JOIN policy_owned_appendices oa ON oa.id = av.appendix_id
    JOIN policies p ON p.id = oa.policy_id
) d,
LATERAL (SELECT lower(m[1])::uuid AS asset_id FROM regexp_matches(d.body, $1, 'g') AS m) r`

// ListWithLinks returns every stored file, newest first, with the documents
// that link to it. With unlinkedOnly it returns only files no document links
// to.
func (s *AssetStore) ListWithLinks(ctx context.Context, unlinkedOnly bool) ([]domain.LinkedAsset, error) {
	rows, err := s.db.Querier().Query(ctx, `SELECT `+assetCols+` FROM editor_assets ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, fmt.Errorf("list editor_assets: %w", err)
	}
	var out []domain.LinkedAsset
	for rows.Next() {
		a, err := scanAsset(rows)
		if err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan editor_asset: %w", err)
		}
		out = append(out, domain.LinkedAsset{Asset: a})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list editor_assets: %w", err)
	}

	refs, err := s.db.Querier().Query(ctx, assetRefsSQL+` ORDER BY d.title, d.policy_id, d.loc`, assetRefPattern)
	if err != nil {
		return nil, fmt.Errorf("list asset links: %w", err)
	}
	defer refs.Close()
	links := map[uuid.UUID][]domain.AssetDocLink{}
	for refs.Next() {
		var assetID uuid.UUID
		var l domain.AssetDocLink
		if err := refs.Scan(&assetID, &l.PolicyID, &l.Title, &l.Location); err != nil {
			return nil, fmt.Errorf("scan asset link: %w", err)
		}
		links[assetID] = append(links[assetID], l)
	}
	if err := refs.Err(); err != nil {
		return nil, fmt.Errorf("list asset links: %w", err)
	}

	filtered := out[:0]
	for _, la := range out {
		la.Links = links[la.Asset.ID]
		if unlinkedOnly && la.Linked() {
			continue
		}
		filtered = append(filtered, la)
	}
	return filtered, nil
}

// contentTarget is one place editor content is stored. Reads lock the rows
// that hold the file's url; ctid names a locked row for the write back.
type contentTarget struct {
	table, column string
	// selectSQL returns (ctid::text, content::text, policy_id, title, loc)
	// for rows whose content holds $1.
	selectSQL string
	// reported is false for copies (review snapshots) that are rewritten but
	// are not a place the document is shown.
	reported bool
}

var contentTargets = []contentTarget{
	{table: "policy_versions", column: "content", reported: true, selectSQL: `
		SELECT pv.ctid::text, pv.content::text, p.id, p.title, 'policy'
		  FROM policy_versions pv JOIN policies p ON p.id = pv.policy_id
		 WHERE position($1 in pv.content::text) > 0 FOR UPDATE OF pv`},
	{table: "policy_appendices", column: "content", reported: true, selectSQL: `
		SELECT a.ctid::text, a.content::text, p.id, p.title, 'appendix'
		  FROM policy_appendices a
		  JOIN policy_versions pv ON pv.id = a.policy_version_id
		  JOIN policies p ON p.id = pv.policy_id
		 WHERE position($1 in a.content::text) > 0 FOR UPDATE OF a`},
	{table: "policy_appendix_versions", column: "content_json", reported: true, selectSQL: `
		SELECT av.ctid::text, av.content_json::text, p.id, p.title, 'appendix'
		  FROM policy_appendix_versions av
		  JOIN policy_owned_appendices oa ON oa.id = av.appendix_id
		  JOIN policies p ON p.id = oa.policy_id
		 WHERE position($1 in av.content_json::text) > 0 FOR UPDATE OF av`},
	{table: "review_snapshots", column: "content", reported: false, selectSQL: `
		SELECT rs.ctid::text, rs.content::text, pv.policy_id, '', 'policy'
		  FROM review_snapshots rs JOIN policy_versions pv ON pv.id = rs.policy_version_id
		 WHERE position($1 in rs.content::text) > 0 FOR UPDATE OF rs`},
}

// DeleteWithLinks deletes the file's record and takes its card out of every
// document that links to it, in drafts, published versions and appendices, in
// one transaction. It writes no new version and touches no history: only the
// content columns change. It returns the file and the documents it was
// removed from, or ErrAssetNotFound. The caller removes the object bytes.
func (s *AssetStore) DeleteWithLinks(ctx context.Context, id uuid.UUID) (domain.LinkedAsset, error) {
	var res domain.LinkedAsset
	err := s.db.RunInTx(ctx, func(tx pgx.Tx) error {
		a, err := scanAsset(tx.QueryRow(ctx, `SELECT `+assetCols+` FROM editor_assets WHERE id = $1 FOR UPDATE`, id))
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrAssetNotFound
			}
			return fmt.Errorf("lock editor_asset: %w", err)
		}
		res.Asset = a
		url := a.URL()

		seen := map[domain.AssetDocLink]bool{}
		for _, tgt := range contentTargets {
			type hit struct {
				ctid, content string
				link          domain.AssetDocLink
			}
			rows, err := tx.Query(ctx, tgt.selectSQL, url)
			if err != nil {
				return fmt.Errorf("find %s links: %w", tgt.table, err)
			}
			var hits []hit
			for rows.Next() {
				var h hit
				if err := rows.Scan(&h.ctid, &h.content, &h.link.PolicyID, &h.link.Title, &h.link.Location); err != nil {
					rows.Close()
					return fmt.Errorf("scan %s link: %w", tgt.table, err)
				}
				hits = append(hits, h)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				return fmt.Errorf("find %s links: %w", tgt.table, err)
			}
			for _, h := range hits {
				rewritten, changed, err := domain.RemoveAssetFromContent(h.content, url)
				if err != nil {
					return fmt.Errorf("rewrite %s content: %w", tgt.table, err)
				}
				if changed {
					if _, err := tx.Exec(ctx,
						`UPDATE `+tgt.table+` SET `+tgt.column+` = $2::jsonb WHERE ctid = $1::tid`,
						h.ctid, rewritten); err != nil {
						return fmt.Errorf("update %s content: %w", tgt.table, err)
					}
				}
				if tgt.reported && !seen[h.link] {
					seen[h.link] = true
					res.Links = append(res.Links, h.link)
				}
			}
		}

		if _, err := tx.Exec(ctx, `DELETE FROM editor_assets WHERE id = $1`, id); err != nil {
			return fmt.Errorf("delete editor_asset: %w", err)
		}
		return nil
	})
	if err != nil {
		return domain.LinkedAsset{}, err
	}
	return res, nil
}
