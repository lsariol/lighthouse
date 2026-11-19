package api

import (
	"context"
	"time"

	"github.com/lsariol/lighthouse/internal/projects"
)

// Project is one project in GET /v1/projects.
type Project struct {
	Name            string     `json:"name"`
	ComposeProject  *string    `json:"compose_project"`
	Kind            string     `json:"kind"`
	DeployMode      string     `json:"deploy_mode"`
	State           string     `json:"state"`
	DeployedVersion *string    `json:"deployed_version"`
	DeployedSHA     *string    `json:"deployed_sha"`
	DeployedAt      *time.Time `json:"deployed_at"`
	FailureCount    int        `json:"failure_count"`
	LastError       *string    `json:"last_error"`
	LastErrorAt     *time.Time `json:"last_error_at"`
	LastCheckedAt   *time.Time `json:"last_checked_at"`
}

// Deployment is one deploy attempt in GET /v1/deployments; Project is its compose project.
type Deployment struct {
	ID          int64     `json:"id"`
	Project     string    `json:"project"`
	Version     *string   `json:"version"`
	SHA         *string   `json:"sha"`
	Trigger     string    `json:"trigger"`
	Status      string    `json:"status"`
	FailureKind *string   `json:"failure_kind"`
	FailedStep  *string   `json:"failed_step"`
	Error       *string   `json:"error"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}

// Step is one step of a deploy, with the end of its output, already scrubbed of secrets.
type Step struct {
	Step       string    `json:"step"`
	Status     string    `json:"status"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Log        *string   `json:"log"`
}

// DeploymentDetail is GET /v1/deployments/{id}: one deploy with its steps.
type DeploymentDetail struct {
	Deployment
	Steps []Step `json:"steps"`
}

// Release is one version tag in GET /v1/releases; Project is its compose project.
type Release struct {
	Project     string     `json:"project"`
	Tag         string     `json:"tag"`
	SHA         string     `json:"sha"`
	Status      string     `json:"status"`
	Note        *string    `json:"note"`
	PublishedAt *time.Time `json:"published_at"`
	URL         *string    `json:"url"`
	CreatedAt   time.Time  `json:"created_at"`
}

// Cursor is where a page of deployments ends: the last one's start and ID.
type Cursor struct {
	StartedAt time.Time
	ID        int64
}

// DeploymentQuery picks deployments that started between From and To, newest first.
type DeploymentQuery struct {
	From    time.Time
	To      time.Time
	Project string  // a compose project or a project's name; "" for every project
	Limit   int     // at most this many
	After   *Cursor // only those after this one in the order; nil from the start
}

// Reader is the database as lighthouse_reader sees it.
type Reader interface {
	List(ctx context.Context) ([]projects.Project, error)
	RecentDeployments(ctx context.Context, q DeploymentQuery) ([]Deployment, error)
	// DeploymentByID returns one deployment with its steps, or projects.ErrNoDeployment.
	DeploymentByID(ctx context.Context, id int64) (DeploymentDetail, error)
	// RecentReleases returns the newest releases, of one project or all; projects.ErrNotFound for an unknown one.
	RecentReleases(ctx context.Context, project string, limit int) ([]Release, error)
	CheckSchemaVersion(ctx context.Context) error
	Ping(ctx context.Context) error
}

// The states a project can be in, Lighthouse's own word for it.
const (
	StateOK          = "ok"
	StateFailing     = "failing"      // failures since its last success
	StateBroken      = "broken"       // its commit failed projects.BrokenAfter times
	StateStopped     = "stopped"      // stopped on purpose
	StateNotDeployed = "not_deployed" // never deployed yet
)

// projectState is p's state; a publish-only project, never deployed by design, is ok unless failing.
func projectState(p projects.Project) string {
	switch {
	case p.Stopped:
		return StateStopped
	case p.Broken:
		return StateBroken
	case p.FailureCount > 0:
		return StateFailing
	case p.DeployedSHA == "" && !p.PublishOnly():
		return StateNotDeployed
	}
	return StateOK
}

// projectAnswer copies p field by field, so a field added to projects.Project never leaks out.
func projectAnswer(p projects.Project) Project {
	return Project{
		Name:            p.Name,
		ComposeProject:  text(p.ComposeProject),
		Kind:            p.Kind,
		DeployMode:      p.Mode,
		State:           projectState(p),
		DeployedVersion: text(p.DeployedVersion),
		DeployedSHA:     text(p.DeployedSHA),
		DeployedAt:      utc(p.DeployedAt),
		FailureCount:    p.FailureCount,
		LastError:       text(p.LastError),
		LastErrorAt:     utc(p.LastErrorAt),
		LastCheckedAt:   utc(p.LastCheckedAt),
	}
}

// text is s, or nil when it's empty.
func text(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// utc is t in UTC, or nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}
