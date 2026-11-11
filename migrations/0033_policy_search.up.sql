-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Full-text search over the published text of each policy (steward-core#30).
--
-- policy_search holds one row per policy that has a live published version: the plain text of that
-- version and of the latest published row of each live appendix (what a reader is served), with a
-- GIN-indexed tsvector over it. Drafts, versions in review, superseded versions and retired
-- policies have no row. The title and number are not stored: they change with a rename, a move or
-- a category code change, so the search reads them from policies and categories.
--
-- Triggers keep the row current from every write path, so no code path can forget to reindex.
-- The 'simple' configuration matches whole words, case-insensitively, with no stemming and no
-- stop words, so a common word still matches and the hit marks the word the reader typed.

CREATE TABLE policy_search (
  policy_id  UUID PRIMARY KEY REFERENCES policies(id) ON DELETE CASCADE,
  version_id UUID NOT NULL,
  body       TEXT NOT NULL,
  -- A tsvector is capped near 1 MB, so a pathological body is indexed up to its first 250000
  -- characters rather than failing the publish that fed it.
  tsv        TSVECTOR GENERATED ALWAYS AS (to_tsvector('simple'::regconfig, left(body, 250000))) STORED
);
CREATE INDEX policy_search_tsv_idx ON policy_search USING GIN (tsv);

-- The plain text of a content document, one passage per line: the values of the older
-- {"sections": {key: text}} envelope, otherwise every string under a "text" key of the Lexical
-- tree, in document order. U+E000 and U+E001 are the marks a snippet is cut with, so they are
-- blanked out of the text.
CREATE FUNCTION policy_search_text(doc JSONB) RETURNS TEXT
LANGUAGE plpgsql STABLE AS $$
BEGIN
  IF jsonb_typeof(doc -> 'sections') = 'object' THEN
    RETURN COALESCE((
      SELECT string_agg(t, E'\n' ORDER BY k)
        FROM (SELECT key AS k, translate(value, chr(57344) || chr(57345), '  ') AS t
                FROM jsonb_each_text(doc -> 'sections')) s
       WHERE btrim(t) <> ''), '');
  END IF;
  RETURN COALESCE((
    SELECT string_agg(t, E'\n' ORDER BY n)
      FROM (SELECT translate(x.v #>> '{}', chr(57344) || chr(57345), '  ') AS t, x.n
              FROM jsonb_path_query(doc, 'strict $.**.text ? (@.type() == "string")')
                   WITH ORDINALITY AS x(v, n)) s
     WHERE btrim(t) <> ''), '');
END
$$;

-- Rebuilds, or removes, the search row of one policy from the current state of the tables.
CREATE FUNCTION policy_search_refresh(pid UUID) RETURNS VOID
LANGUAGE plpgsql AS $$
DECLARE
  vid     UUID;
  content JSONB;
  appx    TEXT;
BEGIN
  SELECT p.current_published_version_id INTO vid
    FROM policies p
   WHERE p.id = pid AND p.retired_at IS NULL;
  IF vid IS NOT NULL THEN
    SELECT pv.content INTO content
      FROM policy_versions pv
     WHERE pv.id = vid AND pv.status = 'published';
  END IF;
  IF content IS NULL THEN
    DELETE FROM policy_search WHERE policy_id = pid;
    RETURN;
  END IF;

  SELECT string_agg(s.t, E'\n' ORDER BY s.order_index) INTO appx
    FROM (SELECT a.order_index, policy_search_text(av.content_json) AS t
            FROM policy_owned_appendices a
            JOIN LATERAL (SELECT v.content_json
                            FROM policy_appendix_versions v
                           WHERE v.appendix_id = a.id AND v.state = 'PUBLISHED'
                           ORDER BY v.version_no DESC
                           LIMIT 1) av ON TRUE
           WHERE a.policy_id = pid AND a.retired_at IS NULL) s
   WHERE s.t <> '';

  INSERT INTO policy_search (policy_id, version_id, body)
  VALUES (pid, vid, concat_ws(E'\n', NULLIF(policy_search_text(content), ''), appx))
  ON CONFLICT (policy_id) DO UPDATE SET version_id = EXCLUDED.version_id, body = EXCLUDED.body;
END
$$;

CREATE FUNCTION policy_search_on_policy() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  PERFORM policy_search_refresh(NEW.id);
  RETURN NULL;
END
$$;
CREATE TRIGGER policy_search_policy
  AFTER UPDATE OF current_published_version_id, retired_at ON policies
  FOR EACH ROW
  WHEN (OLD.current_published_version_id IS DISTINCT FROM NEW.current_published_version_id
        OR OLD.retired_at IS DISTINCT FROM NEW.retired_at)
  EXECUTE FUNCTION policy_search_on_policy();

CREATE FUNCTION policy_search_on_version() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  PERFORM policy_search_refresh(NEW.policy_id);
  RETURN NULL;
END
$$;
CREATE TRIGGER policy_search_version
  AFTER UPDATE OF content, status ON policy_versions
  FOR EACH ROW
  WHEN (OLD.status = 'published' OR NEW.status = 'published')
  EXECUTE FUNCTION policy_search_on_version();

-- Appendices belong to the policy. The row follows a retire or restore, and each publish of an
-- appendix row. A draft save is not a change a reader sees, so it fires nothing.
CREATE FUNCTION policy_search_on_appendix() RETURNS TRIGGER LANGUAGE plpgsql AS $$
BEGIN
  PERFORM policy_search_refresh(NEW.policy_id);
  RETURN NULL;
END
$$;
CREATE TRIGGER policy_search_appendix
  AFTER UPDATE OF retired_at ON policy_owned_appendices
  FOR EACH ROW
  WHEN (OLD.retired_at IS DISTINCT FROM NEW.retired_at)
  EXECUTE FUNCTION policy_search_on_appendix();

CREATE FUNCTION policy_search_on_appendix_version() RETURNS TRIGGER LANGUAGE plpgsql AS $$
DECLARE
  pid UUID;
BEGIN
  SELECT a.policy_id INTO pid FROM policy_owned_appendices a WHERE a.id = NEW.appendix_id;
  IF pid IS NOT NULL THEN
    PERFORM policy_search_refresh(pid);
  END IF;
  RETURN NULL;
END
$$;
CREATE TRIGGER policy_search_appendix_version_insert
  AFTER INSERT ON policy_appendix_versions
  FOR EACH ROW
  WHEN (NEW.state = 'PUBLISHED')
  EXECUTE FUNCTION policy_search_on_appendix_version();
CREATE TRIGGER policy_search_appendix_version_update
  AFTER UPDATE OF state, content_json ON policy_appendix_versions
  FOR EACH ROW
  WHEN (OLD.state = 'PUBLISHED' OR NEW.state = 'PUBLISHED')
  EXECUTE FUNCTION policy_search_on_appendix_version();

-- Index what is published today.
SELECT policy_search_refresh(id) FROM policies WHERE current_published_version_id IS NOT NULL;
