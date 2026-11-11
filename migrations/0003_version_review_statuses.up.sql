-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- A version under review and a version that was rejected are stored states.
-- The new values cannot be used in the transaction that adds them, so the
-- indexes that name them are in 0004.
ALTER TYPE policy_version_status ADD VALUE IF NOT EXISTS 'in_review';
ALTER TYPE policy_version_status ADD VALUE IF NOT EXISTS 'rejected';
