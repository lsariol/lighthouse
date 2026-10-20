-- Publishing GitHub Releases (DOCUMENTATION.md §4.7).
--
-- releases: each attempt to publish (failed attempts in a row, the last
--           error, when to try again), the Release once it exists, and for
--           a backfill (`publish`), where its notes and date come from.

-- +goose Up
ALTER TABLE lighthouse.releases
    ADD COLUMN attempts        integer NOT NULL DEFAULT 0,
    ADD COLUMN last_error      text,
    ADD COLUMN next_attempt_at timestamptz,
    ADD COLUMN published_at    timestamptz,
    ADD COLUMN url             text,
    -- Published with `publish`, not verified by a deploy.
    ADD COLUMN backfill        boolean NOT NULL DEFAULT false,
    ADD COLUMN notes_ref       text,
    ADD COLUMN notes_date      date;
