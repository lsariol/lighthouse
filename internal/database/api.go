package database

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/lsariol/lighthouse/internal/api"
	"github.com/lsariol/lighthouse/internal/projects"
)

var _ api.Reader = (*Database)(nil)

// matchProject is true for a project named $n or whose compose project is $n, or for every project when $n is ”.
const matchProject = `($%[1]d = '' OR lower(p.compose_project) = lower($%[1]d) OR lower(p.name) = lower($%[1]d))`

const apiDeploymentColumns = `d.id, coalesce(p.compose_project, p.name), nullif(d.version, ''), nullif(d.sha, ''), d.trigger, d.status,
	nullif(d.failure_kind, ''), nullif(d.failed_step, ''), nullif(d.error, ''), d.started_at, d.finished_at`

func scanAPIDeployment(row pgx.Row) (api.Deployment, error) {
	var d api.Deployment
	err := row.Scan(&d.ID, &d.Project, &d.Version, &d.SHA, &d.Trigger, &d.Status,
		&d.FailureKind, &d.FailedStep, &d.Error, &d.StartedAt, &d.FinishedAt)
	d.StartedAt, d.FinishedAt = d.StartedAt.UTC(), d.FinishedAt.UTC()
	return d, err
}

// RecentDeployments returns the deployments q picks, newest first, for lighthouse-api.
func (d *Database) RecentDeployments(ctx context.Context, q api.DeploymentQuery) ([]api.Deployment, error) {
	afterAt, afterID := any(nil), int64(0)
	if q.After != nil {
		afterAt, afterID = q.After.StartedAt, q.After.ID
	}
	rows, err := d.pool.Query(ctx, `SELECT `+apiDeploymentColumns+`
		FROM lighthouse.deployments d JOIN lighthouse.projects p ON p.id = d.project_id
		WHERE d.started_at >= $1 AND d.started_at <= $2 AND `+fmt.Sprintf(matchProject, 3)+`
		  AND ($4::timestamptz IS NULL OR (d.started_at, d.id) < ($4::timestamptz, $5))
		ORDER BY d.started_at DESC, d.id DESC
		LIMIT $6`, q.From, q.To, q.Project, afterAt, afterID, q.Limit)
	if err != nil {
		return nil, fmt.Errorf("database: list deployments: %w", err)
	}
	defer rows.Close()

	var list []api.Deployment
	for rows.Next() {
		dep, err := scanAPIDeployment(rows)
		if err != nil {
			return nil, fmt.Errorf("database: list deployments: %w", err)
		}
		list = append(list, dep)
	}
	return list, rows.Err()
}

// DeploymentByID returns one deployment with its steps, or projects.ErrNoDeployment, for lighthouse-api.
func (d *Database) DeploymentByID(ctx context.Context, id int64) (api.DeploymentDetail, error) {
	dep, err := scanAPIDeployment(d.pool.QueryRow(ctx, `SELECT `+apiDeploymentColumns+`
		FROM lighthouse.deployments d JOIN lighthouse.projects p ON p.id = d.project_id
		WHERE d.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return api.DeploymentDetail{}, projects.ErrNoDeployment
	}
	if err != nil {
		return api.DeploymentDetail{}, fmt.Errorf("database: get deployment %d: %w", id, err)
	}

	rows, err := d.pool.Query(ctx, `SELECT step, status, started_at, finished_at, nullif(log, '')
		FROM lighthouse.deployment_steps WHERE deployment_id = $1 ORDER BY position`, id)
	if err != nil {
		return api.DeploymentDetail{}, fmt.Errorf("database: get deployment %d's steps: %w", id, err)
	}
	defer rows.Close()

	out := api.DeploymentDetail{Deployment: dep, Steps: []api.Step{}}
	for rows.Next() {
		var s api.Step
		if err := rows.Scan(&s.Step, &s.Status, &s.StartedAt, &s.FinishedAt, &s.Log); err != nil {
			return api.DeploymentDetail{}, fmt.Errorf("database: get deployment %d's steps: %w", id, err)
		}
		s.StartedAt, s.FinishedAt = s.StartedAt.UTC(), s.FinishedAt.UTC()
		out.Steps = append(out.Steps, s)
	}
	return out, rows.Err()
}

// RecentReleases returns the newest limit releases of project, or of every project for "", for lighthouse-api.
func (d *Database) RecentReleases(ctx context.Context, project string, limit int) ([]api.Release, error) {
	if project != "" {
		var exists bool
		err := d.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM lighthouse.projects p WHERE `+fmt.Sprintf(matchProject, 1)+`)`, project).Scan(&exists)
		if err != nil {
			return nil, fmt.Errorf("database: find project %q: %w", project, err)
		}
		if !exists {
			return nil, projects.ErrNotFound
		}
	}
	rows, err := d.pool.Query(ctx, `SELECT coalesce(p.compose_project, p.name), r.tag, r.sha, r.status, nullif(r.note, ''),
			r.published_at, nullif(r.url, ''), r.created_at
		FROM lighthouse.releases r JOIN lighthouse.projects p ON p.id = r.project_id
		WHERE `+fmt.Sprintf(matchProject, 1)+`
		ORDER BY r.created_at DESC, r.id DESC
		LIMIT $2`, project, limit)
	if err != nil {
		return nil, fmt.Errorf("database: list releases: %w", err)
	}
	defer rows.Close()

	var list []api.Release
	for rows.Next() {
		var r api.Release
		if err := rows.Scan(&r.Project, &r.Tag, &r.SHA, &r.Status, &r.Note, &r.PublishedAt, &r.URL, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("database: list releases: %w", err)
		}
		r.CreatedAt = r.CreatedAt.UTC()
		if r.PublishedAt != nil {
			t := r.PublishedAt.UTC()
			r.PublishedAt = &t
		}
		list = append(list, r)
	}
	return list, rows.Err()
}
