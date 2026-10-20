-- The version tags Lighthouse tracks, so it can publish GitHub Releases
-- (DOCUMENTATION.md §4.7). v1.1.0 only tracks them; nothing is published yet.
--
-- projects.releases_tracked_at: when the project's existing tags were first
--                  recorded (its baseline); NULL until then.
-- releases:        one row per version tag of a project, and what Lighthouse
--                  made of it (projects.Release* in the code says what each
--                  status means). Rows go when the project is removed.
-- deployments.trigger: 'retry' (`retry <name>`), apart from 'manual', since a
--                  successful retry of a ready release publishes it.

-- +goose Up
ALTER TABLE lighthouse.deployments DROP CONSTRAINT deployments_trigger_check;
ALTER TABLE lighthouse.deployments
    ADD CONSTRAINT deployments_trigger_check CHECK (trigger IN ('check', 'manual', 'retry', 'reconcile'));

ALTER TABLE lighthouse.projects
    ADD COLUMN releases_tracked_at timestamptz;

CREATE TABLE lighthouse.releases (
    id            bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    project_id    bigint      NOT NULL REFERENCES lighthouse.projects (id) ON DELETE CASCADE,
    tag           text        NOT NULL,
    sha           text        NOT NULL,
    status        text        NOT NULL CHECK (status IN ('known', 'waiting', 'ready', 'pending', 'published', 'failed', 'superseded', 'moved')),
    note          text,
    -- The deploy that made it pending.
    deployment_id bigint      REFERENCES lighthouse.deployments (id) ON DELETE SET NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (project_id, tag)
);

-- The reader gets it through the default privileges from 00002.
GRANT SELECT, INSERT, UPDATE ON lighthouse.releases TO lighthouse_app;
