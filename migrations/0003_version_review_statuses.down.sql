-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- PostgreSQL cannot drop a value from an enum type, so the two values stay.
-- Nothing reads them once the application is rolled back.
SELECT 1;
