package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/api"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
)

// openReader connects as lighthouse_reader, as lighthouse-api does, after open has emptied the tables.
func openReader(t *testing.T) *Database {
	t.Helper()
	if readerURL == "" {
		t.Skip("LIGHTHOUSE_TEST_DATABASE_READER_URL isn't set (see scripts/test-db.sh)")
	}
	r, err := ConnectReader(context.Background(), readerURL)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r
}

func TestAPIReads(t *testing.T) {
	db := open(t)
	reader := openReader(t)
	ctx := context.Background()

	for _, name := range []string{"plop", "other"} {
		if _, err := db.Add(ctx, name, github.Repo{Owner: "o", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetComposeProject(ctx, "plop", "plop-site"); err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for i, status := range []string{projects.StatusSucceeded, projects.StatusFailed, projects.StatusSucceeded} {
		at := start.Add(time.Duration(i) * time.Hour)
		d := projects.Deployment{Project: "plop", SHA: "abc", Trigger: projects.TriggerCheck, Status: status,
			StartedAt: at, FinishedAt: at.Add(time.Minute),
			Steps: []projects.Step{{Name: "build", Status: projects.StepSucceeded, StartedAt: at, FinishedAt: at, Log: "built\n"}}}
		if status == projects.StatusFailed {
			d.FailureKind, d.FailedStep, d.Error = projects.FailurePermanent, "test", "test: failed"
		}
		if err := db.RecordDeployment(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.RecordDeployment(ctx, projects.Deployment{Project: "other", Trigger: projects.TriggerManual, Status: projects.StatusSucceeded,
		StartedAt: start, FinishedAt: start}); err != nil {
		t.Fatal(err)
	}

	q := api.DeploymentQuery{From: start.Add(-time.Hour), To: start.Add(3 * time.Hour), Project: "PLOP-SITE", Limit: 2}
	page, err := reader.RecentDeployments(ctx, q)
	if err != nil || len(page) != 2 || page[0].Project != "plop-site" || !page[0].StartedAt.Equal(start.Add(2*time.Hour)) {
		t.Fatalf("first page = %+v, %v", page, err)
	}
	if page[1].Status != projects.StatusFailed || *page[1].FailedStep != "test" || page[0].Version != nil || page[0].Error != nil {
		t.Errorf("first page = %+v (want the failure's details, and nulls where there's nothing)", page)
	}
	q.After = &api.Cursor{StartedAt: page[1].StartedAt, ID: page[1].ID}
	rest, err := reader.RecentDeployments(ctx, q)
	if err != nil || len(rest) != 1 || !rest[0].StartedAt.Equal(start) {
		t.Errorf("second page = %+v, %v", rest, err)
	}
	all, err := reader.RecentDeployments(ctx, api.DeploymentQuery{From: start, To: start, Limit: 10})
	if err != nil || len(all) != 2 {
		t.Errorf("every project's at %s = %+v, %v", start, all, err)
	}

	detail, err := reader.DeploymentByID(ctx, page[1].ID)
	if err != nil || len(detail.Steps) != 1 || *detail.Steps[0].Log != "built\n" || detail.Error == nil {
		t.Errorf("deployment %d = %+v, %v", page[1].ID, detail, err)
	}
	if _, err := reader.DeploymentByID(ctx, 1<<40); !errors.Is(err, projects.ErrNoDeployment) {
		t.Errorf("a missing deployment = %v", err)
	}

	if err := db.TrackReleases(ctx, "plop", []projects.Release{{Tag: "v1.0.0", SHA: "abc", Status: projects.ReleaseKnown}}); err != nil {
		t.Fatal(err)
	}
	rs, err := reader.RecentReleases(ctx, "plop-site", 10)
	if err != nil || len(rs) != 1 || rs[0].Tag != "v1.0.0" || rs[0].Project != "plop-site" || rs[0].Note != nil || rs[0].PublishedAt != nil {
		t.Errorf("plop's releases = %+v, %v", rs, err)
	}
	if rs, err := reader.RecentReleases(ctx, "other", 10); err != nil || len(rs) != 0 {
		t.Errorf("other's releases = %+v, %v", rs, err)
	}
	if _, err := reader.RecentReleases(ctx, "nobody", 10); !errors.Is(err, projects.ErrNotFound) {
		t.Errorf("an unknown project's releases = %v", err)
	}
	if err := reader.CheckSchemaVersion(ctx); err != nil {
		t.Errorf("the reader can't check the schema: %v", err)
	}
}
