package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/release"
)

// The Database is the projects.Store.
var _ projects.Store = (*Database)(nil)

// Postgres error codes and constraint names the store turns into
// projects errors.
const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
	nameKey         = "projects_name_key"            // unique index on lower(name)
	repoKey         = "projects_repo_key"            // unique index on the repository
	composeKey      = "projects_compose_project_key" // unique index on compose_project
	nameCheck       = "projects_name_check"          // the name's format
)

// constraintError maps a unique or check violation to a projects error.
func constraintError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch {
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == nameKey:
		return projects.ErrNameTaken
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == repoKey:
		return projects.ErrRepoWatched
	case pgErr.Code == uniqueViolation && pgErr.ConstraintName == composeKey:
		return projects.ErrComposeProjectTaken
	case pgErr.Code == checkViolation && pgErr.ConstraintName == nameCheck:
		return projects.ErrInvalidName
	}
	return err
}

const projectColumns = `name, repo_owner, repo_name, created_at, coalesce(compose_project, ''),
	coalesce(deployed_sha, ''), coalesce(deployed_version, ''), coalesce(highest_version, ''), deployed_at, last_checked_at, check_count, coalesce(last_error, ''), last_error_at,
	failure_count, coalesce(failing_sha, ''), broken, deploy_mode, tier, stopped, coalesce(held_sha, ''), releases_tracked_at IS NOT NULL`

func scanProject(row pgx.Row) (projects.Project, error) {
	var p projects.Project
	err := row.Scan(&p.Name, &p.Repo.Owner, &p.Repo.Name, &p.CreatedAt, &p.ComposeProject,
		&p.DeployedSHA, &p.DeployedVersion, &p.HighestVersion, &p.DeployedAt, &p.LastCheckedAt, &p.Checks, &p.LastError, &p.LastErrorAt,
		&p.FailureCount, &p.FailingSHA, &p.Broken, &p.Mode, &p.Tier, &p.Stopped, &p.HeldSHA, &p.ReleasesTracked)
	return p, err
}

func (d *Database) List(ctx context.Context) ([]projects.Project, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+projectColumns+` FROM lighthouse.projects ORDER BY lower(name)`)
	if err != nil {
		return nil, fmt.Errorf("database: list projects: %w", err)
	}
	defer rows.Close()

	var list []projects.Project
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("database: list projects: %w", err)
		}
		list = append(list, p)
	}
	return list, rows.Err()
}

func (d *Database) Get(ctx context.Context, name string) (projects.Project, error) {
	p, err := scanProject(d.pool.QueryRow(ctx,
		`SELECT `+projectColumns+` FROM lighthouse.projects WHERE lower(name) = lower($1)`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return projects.Project{}, projects.ErrNotFound
	}
	if err != nil {
		return projects.Project{}, fmt.Errorf("database: get project %q: %w", name, err)
	}
	return p, nil
}

func (d *Database) Add(ctx context.Context, name string, repo github.Repo) (projects.Project, error) {
	if err := projects.ValidateName(name); err != nil {
		return projects.Project{}, err
	}
	p, err := scanProject(d.pool.QueryRow(ctx,
		`INSERT INTO lighthouse.projects (name, repo_owner, repo_name) VALUES ($1, $2, $3)
		 RETURNING `+projectColumns, name, repo.Owner, repo.Name))
	if err != nil {
		return projects.Project{}, constraintError(err)
	}
	return p, nil
}

// exec runs a statement that changes one project, and returns ErrNotFound if
// it matched none.
func (d *Database) exec(ctx context.Context, query string, args ...any) error {
	tag, err := d.pool.Exec(ctx, query, args...)
	if err != nil {
		return constraintError(err)
	}
	if tag.RowsAffected() == 0 {
		return projects.ErrNotFound
	}
	return nil
}

func (d *Database) Remove(ctx context.Context, name string) error {
	return d.exec(ctx, `DELETE FROM lighthouse.projects WHERE lower(name) = lower($1)`, name)
}

func (d *Database) Rename(ctx context.Context, name string, newName string) error {
	if err := projects.ValidateName(newName); err != nil {
		return err
	}
	return d.exec(ctx, `UPDATE lighthouse.projects SET name = $2, updated_at = now() WHERE lower(name) = lower($1)`, name, newName)
}

func (d *Database) SetRepo(ctx context.Context, name string, repo github.Repo) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET repo_owner = $2, repo_name = $3, updated_at = now()
		WHERE lower(name) = lower($1)`, name, repo.Owner, repo.Name)
}

func (d *Database) SetComposeProject(ctx context.Context, name string, composeProject string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET compose_project = $2 WHERE lower(name) = lower($1)`, name, composeProject)
}

func (d *Database) SetSettings(ctx context.Context, name string, mode string, tier string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET deploy_mode = $2, tier = $3 WHERE lower(name) = lower($1)`, name, mode, tier)
}

func (d *Database) SetHeld(ctx context.Context, name string, sha string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET held_sha = nullif($2, '') WHERE lower(name) = lower($1)`, name, sha)
}

func (d *Database) SetStopped(ctx context.Context, name string, stopped bool) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET stopped = $2, updated_at = now() WHERE lower(name) = lower($1)`, name, stopped)
}

func (d *Database) RecordCheck(ctx context.Context, name string, checkErr error) error {
	return d.exec(ctx, `UPDATE lighthouse.projects
		SET last_checked_at = now(),
		    check_count     = check_count + 1,
		    last_error      = nullif($2, ''),
		    last_error_at   = CASE WHEN $2 = '' THEN NULL ELSE now() END
		WHERE lower(name) = lower($1)`, name, projects.ErrorText(checkErr))
}

func (d *Database) RecordDeployment(ctx context.Context, dep projects.Deployment) error {
	errText := projects.ErrorText(projects.ErrorString(dep.Error))

	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var id int64
		var highest string
		err := tx.QueryRow(ctx, `SELECT id, coalesce(highest_version, '') FROM lighthouse.projects WHERE lower(name) = lower($1) FOR UPDATE`,
			dep.Project).Scan(&id, &highest)
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("database: record deployment: %w", err)
		}

		var depID int64
		err = tx.QueryRow(ctx, `INSERT INTO lighthouse.deployments
			(project_id, sha, version, trigger, status, failure_kind, failed_step, started_at, finished_at, error)
			VALUES ($1, nullif($2, ''), nullif($3, ''), $4, $5, nullif($6, ''), nullif($7, ''), $8, $9, nullif($10, ''))
			RETURNING id`,
			id, dep.SHA, dep.Version, dep.Trigger, dep.Status, dep.FailureKind, dep.FailedStep, dep.StartedAt, dep.FinishedAt, errText).Scan(&depID)
		if err != nil {
			return fmt.Errorf("database: record deployment: %w", err)
		}

		for i, st := range dep.Steps {
			if _, err := tx.Exec(ctx, `INSERT INTO lighthouse.deployment_steps
				(deployment_id, position, step, status, started_at, finished_at, log)
				VALUES ($1, $2, $3, $4, $5, $6, nullif($7, ''))`,
				depID, i+1, st.Name, st.Status, st.StartedAt, st.FinishedAt, projects.LogText(st.Log)); err != nil {
				return fmt.Errorf("database: record deployment step %s: %w", st.Name, err)
			}
		}

		switch {
		case dep.Status == projects.StatusSucceeded:
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects
				SET deployed_sha = $2, deployed_version = nullif($4, ''), highest_version = nullif($5, ''), deployed_at = $3,
				    last_error = NULL, last_error_at = NULL, failure_count = 0, failing_sha = NULL, broken = false
				WHERE id = $1`, id, dep.SHA, dep.FinishedAt, dep.Version, release.Higher(highest, dep.Version))
			if err == nil {
				err = recordDeployedReleases(ctx, tx, id, depID, dep)
			}
		case dep.FailureKind == projects.FailurePermanent:
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects
				SET last_error = nullif($2, ''), last_error_at = $3,
				    failure_count = CASE WHEN failing_sha IS NOT DISTINCT FROM nullif($4, '') THEN failure_count + 1 ELSE 1 END,
				    failing_sha   = nullif($4, ''),
				    broken        = (CASE WHEN failing_sha IS NOT DISTINCT FROM nullif($4, '') THEN failure_count + 1 ELSE 1 END) >= $5
				WHERE id = $1`, id, errText, dep.FinishedAt, dep.SHA, projects.BrokenAfter)
		default:
			_, err = tx.Exec(ctx, `UPDATE lighthouse.projects SET last_error = nullif($2, ''), last_error_at = $3 WHERE id = $1`,
				id, errText, dep.FinishedAt)
		}
		if err != nil {
			return fmt.Errorf("database: record deployment: %w", err)
		}
		return nil
	})
}

// recordDeployedReleases applies projects.DeployedReleases in a
// RecordDeployment transaction.
func recordDeployedReleases(ctx context.Context, tx pgx.Tx, projectID int64, depID int64, dep projects.Deployment) error {
	if _, ok := release.Parse(dep.Version); !ok {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT `+releaseColumns+` FROM lighthouse.releases r JOIN lighthouse.projects p ON p.id = r.project_id
		WHERE r.project_id = $1 ORDER BY r.id FOR UPDATE OF r`, projectID)
	if err != nil {
		return fmt.Errorf("database: record deployment: read releases: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanRelease)
	if err != nil {
		return fmt.Errorf("database: record deployment: read releases: %w", err)
	}
	for _, r := range projects.DeployedReleases(list, dep) {
		if _, err := tx.Exec(ctx, `INSERT INTO lighthouse.releases (project_id, tag, sha, status, note, deployment_id)
			VALUES ($1, $2, $3, $4, nullif($5, ''), CASE WHEN $4 = 'pending' THEN $6::bigint END)
			ON CONFLICT (project_id, tag) DO UPDATE
			SET status = excluded.status, note = excluded.note,
			    deployment_id = coalesce(excluded.deployment_id, releases.deployment_id), updated_at = now()`,
			projectID, r.Tag, r.SHA, r.Status, r.Note, depID); err != nil {
			return fmt.Errorf("database: record deployment: release %s: %w", r.Tag, err)
		}
	}
	return nil
}

const releaseColumns = `p.name, r.tag, r.sha, r.status, coalesce(r.note, ''), r.created_at, r.updated_at,
	r.attempts, coalesce(r.last_error, ''), r.next_attempt_at, r.published_at, coalesce(r.url, ''),
	r.backfill, coalesce(r.notes_ref, ''), coalesce(to_char(r.notes_date, 'YYYY-MM-DD'), '')`

func scanRelease(row pgx.CollectableRow) (projects.Release, error) {
	var r projects.Release
	err := row.Scan(&r.Project, &r.Tag, &r.SHA, &r.Status, &r.Note, &r.CreatedAt, &r.UpdatedAt,
		&r.Attempts, &r.LastError, &r.NextAttempt, &r.PublishedAt, &r.URL,
		&r.Backfill, &r.NotesRef, &r.NotesDate)
	return r, err
}

func (d *Database) DueReleases(ctx context.Context, now time.Time) ([]projects.Release, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+releaseColumns+` FROM lighthouse.releases r JOIN lighthouse.projects p ON p.id = r.project_id
		WHERE (r.status = 'pending' AND (r.next_attempt_at IS NULL OR r.next_attempt_at <= $1))
		   OR (r.status = 'failed' AND r.next_attempt_at <= $1)
		ORDER BY r.id`, now)
	if err != nil {
		return nil, fmt.Errorf("database: releases to publish: %w", err)
	}
	list, err := pgx.CollectRows(rows, scanRelease)
	if err != nil {
		return nil, fmt.Errorf("database: releases to publish: %w", err)
	}
	return list, nil
}

func (d *Database) RecordPublish(ctx context.Context, name string, tag string, pub projects.Published) error {
	if _, err := d.Get(ctx, name); err != nil {
		return err
	}
	tagged, err := d.pool.Exec(ctx, `UPDATE lighthouse.releases r
		SET status = $3, note = nullif($4, ''), last_error = nullif($5, ''), next_attempt_at = $6,
		    attempts = CASE WHEN $5 = '' THEN 0 ELSE r.attempts + 1 END,
		    url = CASE WHEN $3 = 'published' THEN $7 ELSE r.url END,
		    published_at = CASE WHEN $3 = 'published' THEN $8 ELSE r.published_at END,
		    updated_at = now()
		FROM lighthouse.projects p WHERE p.id = r.project_id AND lower(p.name) = lower($1) AND r.tag = $2`,
		name, tag, pub.Status, pub.Note, projects.ErrorText(projects.ErrorString(pub.Error)), pub.NextAttempt, pub.URL, pub.At)
	if err != nil {
		return fmt.Errorf("database: record publishing %s of %q: %w", tag, name, err)
	}
	if tagged.RowsAffected() == 0 {
		return projects.ErrNoRelease
	}
	return nil
}

func (d *Database) QueueBackfill(ctx context.Context, name string, r projects.Release) error {
	tagged, err := d.pool.Exec(ctx, `INSERT INTO lighthouse.releases (project_id, tag, sha, status, backfill, notes_ref, notes_date, next_attempt_at)
		SELECT id, $2, $3, 'pending', true, nullif($4, ''), nullif($5, '')::date, now() FROM lighthouse.projects WHERE lower(name) = lower($1)
		ON CONFLICT (project_id, tag) DO UPDATE
		SET status = 'pending', note = NULL, backfill = true, notes_ref = excluded.notes_ref, notes_date = excluded.notes_date,
		    attempts = 0, last_error = NULL, next_attempt_at = now(), updated_at = now()`,
		name, r.Tag, r.SHA, r.NotesRef, r.NotesDate)
	if err != nil {
		return fmt.Errorf("database: queue %s of %q to publish: %w", r.Tag, name, err)
	}
	if tagged.RowsAffected() == 0 {
		return projects.ErrNotFound
	}
	return nil
}

func (d *Database) Releases(ctx context.Context, name string) ([]projects.Release, error) {
	if name != "" {
		if _, err := d.Get(ctx, name); err != nil {
			return nil, err
		}
	}
	rows, err := d.pool.Query(ctx, `SELECT `+releaseColumns+` FROM lighthouse.releases r JOIN lighthouse.projects p ON p.id = r.project_id
		WHERE $1 = '' OR lower(p.name) = lower($1) ORDER BY r.id`, name)
	if err != nil {
		return nil, fmt.Errorf("database: releases of %q: %w", name, err)
	}
	list, err := pgx.CollectRows(rows, scanRelease)
	if err != nil {
		return nil, fmt.Errorf("database: releases of %q: %w", name, err)
	}
	return list, nil
}

func (d *Database) TrackReleases(ctx context.Context, name string, rs []projects.Release) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `UPDATE lighthouse.projects SET releases_tracked_at = coalesce(releases_tracked_at, now())
			WHERE lower(name) = lower($1) RETURNING id`, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return projects.ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("database: track releases of %q: %w", name, err)
		}
		for _, r := range rs {
			if _, err := tx.Exec(ctx, `INSERT INTO lighthouse.releases (project_id, tag, sha, status, note)
				VALUES ($1, $2, $3, $4, nullif($5, '')) ON CONFLICT (project_id, tag) DO NOTHING`,
				id, r.Tag, r.SHA, r.Status, r.Note); err != nil {
				return fmt.Errorf("database: track release %s of %q: %w", r.Tag, name, err)
			}
		}
		return nil
	})
}

func (d *Database) SetReleaseStatus(ctx context.Context, name string, tag string, status string, note string) error {
	if _, err := d.Get(ctx, name); err != nil {
		return err
	}
	tagged, err := d.pool.Exec(ctx, `UPDATE lighthouse.releases r SET status = $3, note = nullif($4, ''), updated_at = now()
		FROM lighthouse.projects p WHERE p.id = r.project_id AND lower(p.name) = lower($1) AND r.tag = $2`, name, tag, status, note)
	if err != nil {
		return fmt.Errorf("database: set the status of %s of %q: %w", tag, name, err)
	}
	if tagged.RowsAffected() == 0 {
		return projects.ErrNoRelease
	}
	return nil
}

func (d *Database) ClearFailures(ctx context.Context, name string) error {
	return d.exec(ctx, `UPDATE lighthouse.projects SET failure_count = 0, failing_sha = NULL, broken = false
		WHERE lower(name) = lower($1)`, name)
}

const deploymentColumns = `d.id, coalesce(d.sha, ''), coalesce(d.version, ''), d.trigger, d.status, coalesce(d.failure_kind, ''), coalesce(d.failed_step, ''),
	d.started_at, d.finished_at, coalesce(d.error, '')`

// deployments returns the project's deployments, newest first, with their
// IDs.
func (d *Database) deployments(ctx context.Context, p projects.Project, limit int, offset int) ([]projects.Deployment, []int64, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+deploymentColumns+`
		FROM lighthouse.deployments d JOIN lighthouse.projects p ON p.id = d.project_id
		WHERE lower(p.name) = lower($1)
		ORDER BY d.started_at DESC, d.id DESC
		LIMIT $2 OFFSET $3`, p.Name, limit, offset)
	if err != nil {
		return nil, nil, fmt.Errorf("database: history of %q: %w", p.Name, err)
	}
	defer rows.Close()

	var list []projects.Deployment
	var ids []int64
	for rows.Next() {
		dep := projects.Deployment{Project: p.Name}
		var id int64
		if err := rows.Scan(&id, &dep.SHA, &dep.Version, &dep.Trigger, &dep.Status, &dep.FailureKind, &dep.FailedStep,
			&dep.StartedAt, &dep.FinishedAt, &dep.Error); err != nil {
			return nil, nil, fmt.Errorf("database: history of %q: %w", p.Name, err)
		}
		list = append(list, dep)
		ids = append(ids, id)
	}
	return list, ids, rows.Err()
}

func (d *Database) History(ctx context.Context, name string, limit int) ([]projects.Deployment, error) {
	p, err := d.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	list, _, err := d.deployments(ctx, p, limit, 0)
	return list, err
}

func (d *Database) Deployment(ctx context.Context, name string, n int) (projects.Deployment, error) {
	p, err := d.Get(ctx, name)
	if err != nil {
		return projects.Deployment{}, err
	}
	if n < 1 {
		return projects.Deployment{}, projects.ErrNoDeployment
	}
	list, ids, err := d.deployments(ctx, p, 1, n-1)
	if err != nil {
		return projects.Deployment{}, err
	}
	if len(list) == 0 {
		return projects.Deployment{}, projects.ErrNoDeployment
	}
	dep := list[0]

	rows, err := d.pool.Query(ctx, `SELECT step, status, started_at, finished_at, coalesce(log, '')
		FROM lighthouse.deployment_steps WHERE deployment_id = $1 ORDER BY position`, ids[0])
	if err != nil {
		return projects.Deployment{}, fmt.Errorf("database: steps of a deployment of %q: %w", p.Name, err)
	}
	defer rows.Close()
	for rows.Next() {
		var st projects.Step
		if err := rows.Scan(&st.Name, &st.Status, &st.StartedAt, &st.FinishedAt, &st.Log); err != nil {
			return projects.Deployment{}, fmt.Errorf("database: steps of a deployment of %q: %w", p.Name, err)
		}
		dep.Steps = append(dep.Steps, st)
	}
	return dep, rows.Err()
}
