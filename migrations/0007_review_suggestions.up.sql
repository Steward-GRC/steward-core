-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- Suggested changes on a review thread (core#54). A thread may carry one
-- suggestion: a proposed replacement for the quoted text, answered by counters
-- in turn (review_suggestion_rounds, seq 1 is the proposal) and ended by one
-- outcome. The current text and the turn are derived from the last round, not
-- stored. outcome is NULL while the suggestion is open; closed_text is the text
-- at close on every outcome. A closed suggestion keeps its outcome when its
-- thread is reopened. Core stores ids and text only, never a user's details.
CREATE TYPE review_side AS ENUM ('author', 'reviewer');
CREATE TYPE suggestion_outcome AS ENUM ('changed', 'agreed', 'removed', 'withdrawn', 'not_agreed');

CREATE TABLE review_suggestions (
  thread_id    UUID PRIMARY KEY REFERENCES review_threads(id) ON DELETE CASCADE,
  outcome      suggestion_outcome,
  reason       TEXT,
  closed_by    UUID,
  closed_text  TEXT,
  closed_at    TIMESTAMPTZ,
  CONSTRAINT review_suggestions_closed_all_or_none CHECK (
    (outcome IS NULL AND closed_by IS NULL AND closed_text IS NULL AND closed_at IS NULL)
    OR (outcome IS NOT NULL AND closed_by IS NOT NULL AND closed_text IS NOT NULL AND closed_at IS NOT NULL)),
  CONSTRAINT review_suggestions_not_agreed_reason CHECK (
    outcome IS DISTINCT FROM 'not_agreed' OR btrim(coalesce(reason, '')) <> '')
);

CREATE TABLE review_suggestion_rounds (
  thread_id   UUID NOT NULL REFERENCES review_suggestions(thread_id) ON DELETE CASCADE,
  seq         INTEGER NOT NULL CHECK (seq >= 1),
  side        review_side NOT NULL,
  user_id     UUID NOT NULL,
  text        TEXT NOT NULL CHECK (btrim(text) <> ''),
  created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (thread_id, seq)
);
