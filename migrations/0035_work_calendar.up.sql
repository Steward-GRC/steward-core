-- Copyright 2026 The Steward Authors
-- SPDX-License-Identifier: Apache-2.0

-- The site work calendar: which weekdays are working days (ISO 1 Monday to 7
-- Sunday), the IANA time zone the dates are read in, and the holidays. One
-- JSON document, read and replaced whole by the settings service, which
-- validates it; the column default is the calendar every site starts with
-- (Monday to Friday, UTC, no holidays), and it fills the existing singleton row.
ALTER TABLE global_settings
  ADD COLUMN work_calendar JSONB NOT NULL
    DEFAULT '{"weekdays":[1,2,3,4,5],"timezone":"UTC","holidays":[]}'::jsonb,
  ADD CONSTRAINT global_settings_work_calendar_object CHECK (jsonb_typeof(work_calendar) = 'object');
