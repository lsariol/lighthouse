-- Publish-only projects (DOCUMENTATION.md §4.8): repositories Lighthouse
-- publishes GitHub Releases for but never deploys, such as Go libraries.
--
-- projects.kind: 'deploy' (every project until now) or 'publish', set when
--                the project is added.

-- +goose Up
ALTER TABLE lighthouse.projects
    ADD COLUMN kind text NOT NULL DEFAULT 'deploy' CHECK (kind IN ('deploy', 'publish'));
