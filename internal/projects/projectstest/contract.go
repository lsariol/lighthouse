package projectstest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
)

// RunStoreTests runs the behaviour every projects.Store must have.
func RunStoreTests(t *testing.T, open func(t *testing.T) projects.Store) {
	ctx := context.Background()
	plop := github.Repo{Owner: "LSariol", Name: "Plop"}
	cove := github.Repo{Owner: "LSariol", Name: "Cove"}

	t.Run("AddGetList", func(t *testing.T) {
		s := open(t)
		p, err := s.Add(ctx, "plop", plop)
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "plop" || p.Repo != plop || p.CreatedAt.IsZero() || p.DeployedSHA != "" || p.Checks != 0 {
			t.Errorf("Add returned %+v", p)
		}
		if p.ComposeName() != "plop" {
			t.Errorf("ComposeName() = %q", p.ComposeName())
		}

		got, err := s.Get(ctx, "PLOP")
		if err != nil || got.Name != "plop" || got.Repo != plop {
			t.Errorf("Get (other case) = %+v, %v", got, err)
		}

		s.Add(ctx, "cove", cove)
		list, err := s.List(ctx)
		if err != nil || len(list) != 2 || list[0].Name != "cove" || list[1].Name != "plop" {
			t.Errorf("List = %+v, %v (want cove, plop)", list, err)
		}

		if _, err := s.Get(ctx, "nope"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("Get of a missing project = %v", err)
		}
	})

	t.Run("Uniqueness", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		if _, err := s.Add(ctx, "PLOP", cove); !errors.Is(err, projects.ErrNameTaken) {
			t.Errorf("same name in another case = %v, want ErrNameTaken", err)
		}
		if _, err := s.Add(ctx, "other", github.Repo{Owner: "lsariol", Name: "plop"}); !errors.Is(err, projects.ErrRepoWatched) {
			t.Errorf("same repository in another case = %v, want ErrRepoWatched", err)
		}
		if _, err := s.Add(ctx, "bad name", cove); !errors.Is(err, projects.ErrInvalidName) {
			t.Errorf("invalid name = %v, want ErrInvalidName", err)
		}
	})

	t.Run("RenameSetRepoRemove", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		s.Add(ctx, "cove", cove)

		if err := s.Rename(ctx, "plop", "Cove"); !errors.Is(err, projects.ErrNameTaken) {
			t.Errorf("rename onto another project = %v", err)
		}
		if err := s.Rename(ctx, "plop", "x y"); !errors.Is(err, projects.ErrInvalidName) {
			t.Errorf("rename to an invalid name = %v", err)
		}
		if err := s.Rename(ctx, "PLOP", "Plop"); err != nil {
			t.Errorf("rename to a different case of itself: %v", err)
		}
		if err := s.Rename(ctx, "nope", "x"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("rename of a missing project = %v", err)
		}

		site := github.Repo{Owner: "LSariol", Name: "plop-site"}
		if err := s.SetRepo(ctx, "plop", site); err != nil {
			t.Fatal(err)
		}
		if p, _ := s.Get(ctx, "plop"); p.Repo != site || p.ComposeName() != "plop-site" {
			t.Errorf("after SetRepo: %+v", p)
		}
		if err := s.SetRepo(ctx, "plop", cove); !errors.Is(err, projects.ErrRepoWatched) {
			t.Errorf("SetRepo onto another project's repository = %v", err)
		}
		if err := s.SetRepo(ctx, "plop", site); err != nil {
			t.Errorf("SetRepo to its own repository: %v", err)
		}

		if err := s.Remove(ctx, "PLOP"); err != nil {
			t.Fatal(err)
		}
		if err := s.Remove(ctx, "plop"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("second Remove = %v", err)
		}
		if _, err := s.Add(ctx, "again", site); err != nil {
			t.Errorf("re-adding a removed project's repository: %v", err)
		}
	})

	t.Run("RecordCheck", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)

		if err := s.RecordCheck(ctx, "Plop", errors.New("GitHub: 404 Not Found")); err != nil {
			t.Fatal(err)
		}
		p, _ := s.Get(ctx, "plop")
		if p.Checks != 1 || p.LastCheckedAt == nil || p.LastError != "GitHub: 404 Not Found" || p.LastErrorAt == nil {
			t.Errorf("after a failed check: %+v", p)
		}

		s.RecordCheck(ctx, "plop", nil)
		p, _ = s.Get(ctx, "plop")
		if p.Checks != 2 || p.LastError != "" || p.LastErrorAt != nil {
			t.Errorf("after a good check: %+v", p)
		}

		if err := s.RecordCheck(ctx, "gone", nil); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("RecordCheck of a missing project = %v", err)
		}

		long := strings.Repeat("é", projects.MaxErrorLength)
		s.RecordCheck(ctx, "plop", errors.New(long))
		p, _ = s.Get(ctx, "plop")
		if len(p.LastError) > projects.MaxErrorLength || !strings.HasSuffix(p.LastError, "…") {
			t.Errorf("a long error is stored at %d bytes", len(p.LastError))
		}
	})

	t.Run("Deployments", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)

		failed := projects.Deployment{Project: "PLOP", SHA: "aaa", Trigger: projects.TriggerCheck, Status: projects.StatusFailed,
			StartedAt: start, FinishedAt: start.Add(time.Minute), Error: "build failed"}
		if err := s.RecordDeployment(ctx, failed); err != nil {
			t.Fatal(err)
		}
		p, _ := s.Get(ctx, "plop")
		if p.DeployedSHA != "" || p.LastError != "build failed" {
			t.Errorf("after a failed deploy: deployed %q, error %q", p.DeployedSHA, p.LastError)
		}

		ok := projects.Deployment{Project: "plop", SHA: "bbb", Trigger: projects.TriggerManual, Status: projects.StatusSucceeded,
			StartedAt: start.Add(2 * time.Minute), FinishedAt: start.Add(3 * time.Minute)}
		if err := s.RecordDeployment(ctx, ok); err != nil {
			t.Fatal(err)
		}
		p, _ = s.Get(ctx, "plop")
		if p.DeployedSHA != "bbb" || p.DeployedAt == nil || !p.DeployedAt.Equal(ok.FinishedAt) || p.LastError != "" {
			t.Errorf("after a good deploy: %+v", p)
		}

		history, err := s.History(ctx, "plop", 10)
		if err != nil || len(history) != 2 {
			t.Fatalf("History = %+v, %v", history, err)
		}
		if history[0].SHA != "bbb" || history[1].SHA != "aaa" || history[1].Error != "build failed" ||
			history[0].Project != "plop" || history[0].Trigger != projects.TriggerManual || !history[0].StartedAt.Equal(ok.StartedAt) {
			t.Errorf("History (newest first) = %+v", history)
		}
		if h, _ := s.History(ctx, "plop", 1); len(h) != 1 || h[0].SHA != "bbb" {
			t.Errorf("History limit 1 = %+v", h)
		}

		s.Rename(ctx, "plop", "plop-web")
		if h, _ := s.History(ctx, "plop-web", 10); len(h) != 2 || h[0].Project != "plop-web" {
			t.Errorf("History after rename = %+v", h)
		}
		if err := s.RecordDeployment(ctx, projects.Deployment{Project: "gone", Trigger: projects.TriggerCheck, Status: projects.StatusFailed,
			StartedAt: start, FinishedAt: start}); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("RecordDeployment for a missing project = %v", err)
		}
		if _, err := s.History(ctx, "gone", 10); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("History of a missing project = %v", err)
		}
		s.Remove(ctx, "plop-web")
		s.Add(ctx, "plop-web", plop)
		if h, _ := s.History(ctx, "plop-web", 10); len(h) != 0 {
			t.Errorf("a re-added project inherited history: %+v", h)
		}
	})

	t.Run("ReleasesSettingsStopped", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "cove", plop)
		p, _ := s.Get(ctx, "cove")
		if p.Mode != "branch" || p.Tier != "app" || p.Stopped || p.DeployedVersion != "" {
			t.Errorf("a new project: mode %q, tier %q, stopped %v, version %q", p.Mode, p.Tier, p.Stopped, p.DeployedVersion)
		}

		if err := s.SetSettings(ctx, "COVE", "releases", "infra"); err != nil {
			t.Fatal(err)
		}
		if err := s.SetStopped(ctx, "cove", true); err != nil {
			t.Fatal(err)
		}
		p, _ = s.Get(ctx, "cove")
		if p.Mode != "releases" || p.Tier != "infra" || !p.Stopped {
			t.Errorf("after SetSettings and SetStopped: %+v", p)
		}
		if err := s.SetHeld(ctx, "cove", "fff"); err != nil {
			t.Fatal(err)
		}
		if p, _ = s.Get(ctx, "cove"); p.HeldSHA != "fff" {
			t.Errorf("held %q", p.HeldSHA)
		}
		s.SetHeld(ctx, "cove", "")
		if p, _ = s.Get(ctx, "cove"); p.HeldSHA != "" {
			t.Errorf("held %q after clearing", p.HeldSHA)
		}
		if err := s.SetHeld(ctx, "gone", "x"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("SetHeld of a missing project = %v", err)
		}
		if err := s.SetStopped(ctx, "gone", true); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("SetStopped of a missing project = %v", err)
		}
		if err := s.SetSettings(ctx, "gone", "branch", "app"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("SetSettings of a missing project = %v", err)
		}

		now := time.Now().Truncate(time.Millisecond)
		dep := projects.Deployment{Project: "cove", SHA: "ccc", Version: "v1.2.0", Trigger: projects.TriggerReconcile,
			Status: projects.StatusSucceeded, StartedAt: now, FinishedAt: now}
		if err := s.RecordDeployment(ctx, dep); err != nil {
			t.Fatal(err)
		}
		p, _ = s.Get(ctx, "cove")
		if p.DeployedVersion != "v1.2.0" || p.DeployedSHA != "ccc" {
			t.Errorf("deployed %q at %q", p.DeployedVersion, p.DeployedSHA)
		}
		if h, _ := s.History(ctx, "cove", 1); len(h) != 1 || h[0].Version != "v1.2.0" || h[0].Trigger != projects.TriggerReconcile {
			t.Errorf("History = %+v", h)
		}

		dep.SHA, dep.Version = "bbb", "v1.0.0"
		s.RecordDeployment(ctx, dep)
		if p, _ = s.Get(ctx, "cove"); p.DeployedVersion != "v1.0.0" || p.HighestVersion != "v1.2.0" {
			t.Errorf("after going back: deployed %q, highest %q", p.DeployedVersion, p.HighestVersion)
		}
		failed := dep
		failed.SHA, failed.Version, failed.Status, failed.FailureKind = "eee", "v1.3.0", projects.StatusFailed, projects.FailureTransient
		s.RecordDeployment(ctx, failed)
		if p, _ = s.Get(ctx, "cove"); p.HighestVersion != "v1.2.0" {
			t.Errorf("a failed deploy raised the highest to %q", p.HighestVersion)
		}

		dep.SHA, dep.Version = "ddd", ""
		s.RecordDeployment(ctx, dep)
		if p, _ = s.Get(ctx, "cove"); p.DeployedVersion != "" || p.HighestVersion != "v1.2.0" {
			t.Errorf("a branch deploy: version %q, highest %q", p.DeployedVersion, p.HighestVersion)
		}
	})

	t.Run("TrackedReleases", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "cove", cove)
		s.Add(ctx, "plop", plop)
		if p, _ := s.Get(ctx, "cove"); p.ReleasesTracked {
			t.Error("a new project has its baseline taken")
		}
		if list, err := s.Releases(ctx, "cove"); err != nil || len(list) != 0 {
			t.Errorf("Releases of a new project = %+v, %v", list, err)
		}

		baseline := []projects.Release{
			{Tag: "v1.0.0", SHA: "s100", Status: projects.ReleaseKnown, Note: "there before"},
			{Tag: "v1.1.0", SHA: "s110", Status: projects.ReleaseReady},
			{Tag: "v1.2.0", SHA: "s120", Status: projects.ReleaseWaiting},
			{Tag: "v1.3.0", SHA: "s130", Status: projects.ReleaseWaiting},
		}
		if err := s.TrackReleases(ctx, "COVE", baseline); err != nil {
			t.Fatal(err)
		}
		if err := s.TrackReleases(ctx, "cove", []projects.Release{{Tag: "v1.0.0", SHA: "other", Status: projects.ReleaseReady}}); err != nil {
			t.Fatal(err)
		}
		list, err := s.Releases(ctx, "cove")
		if err != nil || len(list) != 4 || list[0].Tag != "v1.0.0" || list[0].SHA != "s100" || list[0].Status != projects.ReleaseKnown ||
			list[0].Note != "there before" || list[0].Project != "cove" || list[0].CreatedAt.IsZero() {
			t.Errorf("after tracking twice: %+v, %v", list, err)
		}
		if p, _ := s.Get(ctx, "cove"); !p.ReleasesTracked {
			t.Error("the baseline isn't recorded")
		}
		s.TrackReleases(ctx, "plop", []projects.Release{{Tag: "v0.1.0", SHA: "p010", Status: projects.ReleaseReady}})
		if all, _ := s.Releases(ctx, ""); len(all) != 5 {
			t.Errorf("every project's releases: %+v", all)
		}

		if err := s.SetReleaseStatus(ctx, "cove", "v1.2.0", projects.ReleaseReady, ""); err != nil {
			t.Fatal(err)
		}
		if err := s.SetReleaseStatus(ctx, "cove", "v9.0.0", projects.ReleaseReady, ""); !errors.Is(err, projects.ErrNoRelease) {
			t.Errorf("SetReleaseStatus of a missing tag = %v", err)
		}
		if err := s.SetReleaseStatus(ctx, "gone", "v1.0.0", projects.ReleaseReady, ""); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("SetReleaseStatus of a missing project = %v", err)
		}
		if _, err := s.Releases(ctx, "gone"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("Releases of a missing project = %v", err)
		}

		now := time.Now().Truncate(time.Millisecond)
		dep := projects.Deployment{Project: "cove", SHA: "s120", Version: "v1.2.0", Trigger: projects.TriggerCheck,
			Status: projects.StatusSucceeded, StartedAt: now, FinishedAt: now}
		if err := s.RecordDeployment(ctx, dep); err != nil {
			t.Fatal(err)
		}
		status := func(tag string) string {
			list, _ := s.Releases(ctx, "cove")
			for _, r := range list {
				if r.Tag == tag {
					return r.Status
				}
			}
			return "missing"
		}
		for tag, want := range map[string]string{"v1.0.0": projects.ReleaseKnown, "v1.1.0": projects.ReleaseSuperseded,
			"v1.2.0": projects.ReleasePending, "v1.3.0": projects.ReleaseWaiting} {
			if got := status(tag); got != want {
				t.Errorf("after deploying v1.2.0: %s is %s, want %s", tag, got, want)
			}
		}

		dep.SHA, dep.Version, dep.Trigger = "s140", "v1.4.0", projects.TriggerCheck
		s.RecordDeployment(ctx, dep)
		dep.SHA, dep.Version, dep.Trigger = "s130", "v1.3.0", projects.TriggerManual
		s.RecordDeployment(ctx, dep)
		if status("v1.4.0") != projects.ReleasePending || status("v1.3.0") != projects.ReleaseSuperseded {
			t.Errorf("an untracked release deployed by a check: v1.4.0 %s, v1.3.0 %s", status("v1.4.0"), status("v1.3.0"))
		}

		s.TrackReleases(ctx, "cove", []projects.Release{{Tag: "v1.5.0", SHA: "s150", Status: projects.ReleaseReady},
			{Tag: "v1.5.1", SHA: "s151", Status: projects.ReleaseReady}, {Tag: "v1.5.2", SHA: "s152", Status: projects.ReleaseWaiting}})
		dep.SHA, dep.Version, dep.Trigger = "s150", "v1.5.0", projects.TriggerManual
		s.RecordDeployment(ctx, dep)
		dep.SHA, dep.Version, dep.Trigger = "s151", "v1.5.1", projects.TriggerRetry
		s.RecordDeployment(ctx, dep)
		dep.SHA, dep.Version, dep.Trigger = "s152", "v1.5.2", projects.TriggerRetry
		s.RecordDeployment(ctx, dep)
		if status("v1.5.1") != projects.ReleasePending || status("v1.5.2") != projects.ReleaseKnown {
			t.Errorf("retried: ready v1.5.1 is %s (want pending), waiting v1.5.2 is %s (want known)", status("v1.5.1"), status("v1.5.2"))
		}
		failed := dep
		failed.SHA, failed.Version, failed.Trigger, failed.Status, failed.FailureKind = "s160", "v1.6.0", projects.TriggerCheck, projects.StatusFailed, projects.FailurePermanent
		s.RecordDeployment(ctx, failed)
		if status("v1.5.0") != projects.ReleaseKnown || status("v1.6.0") != "missing" {
			t.Errorf("deployed by hand: v1.5.0 %s; a failed deploy added v1.6.0: %s", status("v1.5.0"), status("v1.6.0"))
		}

		s.Rename(ctx, "cove", "vault")
		if list, err := s.Releases(ctx, "vault"); err != nil || len(list) != 8 || list[0].Project != "vault" {
			t.Errorf("after a rename: %d releases, %v", len(list), err)
		}
		s.Remove(ctx, "vault")
		s.Add(ctx, "vault", cove)
		if list, _ := s.Releases(ctx, "vault"); len(list) != 0 {
			t.Errorf("a re-added project inherited releases: %+v", list)
		}
	})

	t.Run("PublishOnly", func(t *testing.T) {
		s := open(t)
		p, err := s.AddPublishOnly(ctx, "coveclient", github.Repo{Owner: "LSariol", Name: "CoveClient"})
		if err != nil || p.Kind != projects.KindPublish || !p.PublishOnly() {
			t.Fatalf("AddPublishOnly = %+v, %v", p, err)
		}
		if got, _ := s.Get(ctx, "coveclient"); !got.PublishOnly() {
			t.Errorf("Get: kind %q", got.Kind)
		}
		s.Add(ctx, "cove", cove)
		if got, _ := s.Get(ctx, "cove"); got.Kind != projects.KindDeploy || got.PublishOnly() {
			t.Errorf("a project added with Add: kind %q", got.Kind)
		}
		if _, err := s.AddPublishOnly(ctx, "COVE", plop); !errors.Is(err, projects.ErrNameTaken) {
			t.Errorf("AddPublishOnly with a taken name = %v", err)
		}
		if _, err := s.AddPublishOnly(ctx, "bad name", plop); !errors.Is(err, projects.ErrInvalidName) {
			t.Errorf("AddPublishOnly with an invalid name = %v", err)
		}
	})

	t.Run("Publishing", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "cove", cove)
		s.TrackReleases(ctx, "cove", []projects.Release{
			{Tag: "v1.0.0", SHA: "s100", Status: projects.ReleaseKnown},
			{Tag: "v1.1.0", SHA: "s110", Status: projects.ReleasePending},
			{Tag: "v1.2.0", SHA: "s120", Status: projects.ReleasePending},
		})
		now := time.Now().Truncate(time.Second)
		tags := func(list []projects.Release) string {
			var out []string
			for _, r := range list {
				out = append(out, r.Tag)
			}
			return strings.Join(out, ",")
		}
		if due, err := s.DueReleases(ctx, now); err != nil || tags(due) != "v1.1.0,v1.2.0" {
			t.Fatalf("due = %s, %v", tags(due), err)
		}

		later := now.Add(time.Hour)
		if err := s.RecordPublish(ctx, "COVE", "v1.1.0", projects.Published{Status: projects.ReleasePending, Error: "502 Bad Gateway", NextAttempt: &later, At: now}); err != nil {
			t.Fatal(err)
		}
		s.RecordPublish(ctx, "cove", "v1.1.0", projects.Published{Status: projects.ReleasePending, Error: "502 again", NextAttempt: &later, At: now})
		s.RecordPublish(ctx, "cove", "v1.2.0", projects.Published{Status: projects.ReleasePublished, Note: "adopted", URL: "https://r/v1.2.0", At: now})
		if due, _ := s.DueReleases(ctx, now); len(due) != 0 {
			t.Errorf("due after the attempts: %s", tags(due))
		}
		if due, _ := s.DueReleases(ctx, later); tags(due) != "v1.1.0" || due[0].Attempts != 2 || due[0].LastError != "502 again" || !due[0].NextAttempt.Equal(later) {
			t.Errorf("due an hour later: %+v", due)
		}
		list, _ := s.Releases(ctx, "cove")
		if r := list[2]; r.Status != projects.ReleasePublished || r.URL != "https://r/v1.2.0" || r.PublishedAt == nil || !r.PublishedAt.Equal(now) ||
			r.Note != "adopted" || r.Attempts != 0 || r.LastError != "" {
			t.Errorf("published: %+v", r)
		}

		s.RecordPublish(ctx, "cove", "v1.1.0", projects.Published{Status: projects.ReleaseFailed, Error: "403", NextAttempt: &later, At: now})
		if due, _ := s.DueReleases(ctx, later); tags(due) != "v1.1.0" {
			t.Errorf("a failed release isn't due again: %s", tags(due))
		}
		s.RecordPublish(ctx, "cove", "v1.1.0", projects.Published{Status: projects.ReleaseFailed, Error: "the tag is gone", At: now})
		if due, _ := s.DueReleases(ctx, later.Add(24*time.Hour)); len(due) != 0 {
			t.Errorf("a failed release with no next attempt is due: %s", tags(due))
		}
		if err := s.RecordPublish(ctx, "cove", "v9.0.0", projects.Published{Status: projects.ReleasePublished}); !errors.Is(err, projects.ErrNoRelease) {
			t.Errorf("RecordPublish of a missing tag = %v", err)
		}

		if err := s.QueueBackfill(ctx, "cove", projects.Release{Tag: "v1.0.0", SHA: "s100", NotesRef: "main", NotesDate: "2025-01-02"}); err != nil {
			t.Fatal(err)
		}
		s.QueueBackfill(ctx, "cove", projects.Release{Tag: "v0.9.0", SHA: "s090"})
		due, _ := s.DueReleases(ctx, time.Now().Add(time.Second))
		if tags(due) != "v1.0.0,v0.9.0" || !due[0].Backfill || due[0].NotesRef != "main" || due[0].NotesDate != "2025-01-02" || due[1].SHA != "s090" || due[1].NotesDate != "" {
			t.Errorf("backfills: %+v", due)
		}
		if err := s.QueueBackfill(ctx, "gone", projects.Release{Tag: "v1.0.0"}); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("QueueBackfill of a missing project = %v", err)
		}
	})

	t.Run("ComposeProject", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "personalWebsite", github.Repo{Owner: "lsariol", Name: "Landing"})
		s.Add(ctx, "cove", cove)

		p, _ := s.Get(ctx, "personalWebsite")
		if p.ComposeProject != "" || p.ComposeName() != "landing" {
			t.Errorf("before a deploy: %q, ComposeName %q", p.ComposeProject, p.ComposeName())
		}

		if err := s.SetComposeProject(ctx, "personalwebsite", "website"); err != nil {
			t.Fatal(err)
		}
		if p, _ := s.Get(ctx, "personalWebsite"); p.ComposeProject != "website" || p.ComposeName() != "website" {
			t.Errorf("after SetComposeProject: %+v", p)
		}
		if err := s.SetComposeProject(ctx, "personalWebsite", "website"); err != nil {
			t.Errorf("setting the same compose project again: %v", err)
		}
		if err := s.SetComposeProject(ctx, "cove", "website"); !errors.Is(err, projects.ErrComposeProjectTaken) {
			t.Errorf("another project's compose project = %v, want ErrComposeProjectTaken", err)
		}
		if err := s.SetComposeProject(ctx, "gone", "x"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("SetComposeProject of a missing project = %v", err)
		}
	})

	t.Run("FailuresAndBroken", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		start := time.Now().Add(-time.Hour)
		fail := func(sha, kind string) {
			t.Helper()
			start = start.Add(time.Minute)
			if err := s.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: sha, Trigger: projects.TriggerCheck,
				Status: projects.StatusFailed, FailureKind: kind, FailedStep: "build", StartedAt: start, FinishedAt: start, Error: "no"}); err != nil {
				t.Fatal(err)
			}
		}

		fail("aaa", projects.FailureTransient)
		fail("aaa", projects.FailureTransient)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 0 || p.Broken {
			t.Errorf("after transient failures: count %d, broken %v", p.FailureCount, p.Broken)
		}

		fail("aaa", projects.FailurePermanent)
		fail("aaa", projects.FailurePermanent)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 2 || p.FailingSHA != "aaa" || p.Broken {
			t.Errorf("after two permanent failures: %+v", p)
		}

		fail("bbb", projects.FailurePermanent)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 1 || p.FailingSHA != "bbb" {
			t.Errorf("after a new commit failed: %+v", p)
		}
		fail("bbb", projects.FailurePermanent)
		fail("bbb", projects.FailurePermanent)
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != projects.BrokenAfter || !p.Broken {
			t.Errorf("after %d failures: %+v", projects.BrokenAfter, p)
		}

		if err := s.ClearFailures(ctx, "PLOP"); err != nil {
			t.Fatal(err)
		}
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 0 || p.Broken || p.FailingSHA != "" {
			t.Errorf("after ClearFailures: %+v", p)
		}
		fail("bbb", projects.FailurePermanent)
		s.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: "ccc", Trigger: projects.TriggerCheck,
			Status: projects.StatusSucceeded, StartedAt: start, FinishedAt: start})
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 0 || p.FailingSHA != "" || p.DeployedSHA != "ccc" {
			t.Errorf("after a success: %+v", p)
		}

		s.RecordDeployment(ctx, projects.Deployment{Project: "plop", SHA: "ddd", Trigger: projects.TriggerCheck,
			Status: projects.StatusRolledBack, FailureKind: projects.FailurePermanent, FailedStep: "verify",
			StartedAt: start.Add(time.Minute), FinishedAt: start.Add(time.Minute), Error: "web didn't become healthy"})
		if p, _ := s.Get(ctx, "plop"); p.FailureCount != 1 || p.DeployedSHA != "ccc" || p.LastError == "" {
			t.Errorf("after a rollback: %+v", p)
		}
		if err := s.ClearFailures(ctx, "gone"); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("ClearFailures of a missing project = %v", err)
		}
	})

	t.Run("DeploymentSteps", func(t *testing.T) {
		s := open(t)
		s.Add(ctx, "plop", plop)
		start := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
		long := strings.Repeat("x", projects.MaxLogLength) + "THE END"

		older := projects.Deployment{Project: "plop", SHA: "aaa", Trigger: projects.TriggerCheck, Status: projects.StatusSucceeded,
			StartedAt: start, FinishedAt: start.Add(time.Minute),
			Steps: []projects.Step{{Name: "fetch", Status: projects.StepSucceeded, StartedAt: start, FinishedAt: start}}}
		newer := projects.Deployment{Project: "plop", SHA: "bbb", Trigger: projects.TriggerManual, Status: projects.StatusFailed,
			FailureKind: projects.FailurePermanent, FailedStep: "build", Error: "build failed",
			StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour + time.Minute),
			Steps: []projects.Step{
				{Name: "fetch", Status: projects.StepSucceeded, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour), Log: "ok"},
				{Name: "build", Status: projects.StepFailed, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour), Log: long},
				{Name: "swap", Status: projects.StepSkipped, StartedAt: start.Add(time.Hour), FinishedAt: start.Add(time.Hour)},
			}}
		for _, d := range []projects.Deployment{older, newer} {
			if err := s.RecordDeployment(ctx, d); err != nil {
				t.Fatal(err)
			}
		}

		d, err := s.Deployment(ctx, "PLOP", 1)
		if err != nil {
			t.Fatal(err)
		}
		if d.SHA != "bbb" || d.FailedStep != "build" || d.FailureKind != projects.FailurePermanent || len(d.Steps) != 3 {
			t.Fatalf("Deployment 1 = %+v", d)
		}
		if d.Steps[0].Name != "fetch" || d.Steps[0].Log != "ok" || d.Steps[1].Status != projects.StepFailed || d.Steps[2].Status != projects.StepSkipped {
			t.Errorf("steps = %+v", d.Steps)
		}
		if l := d.Steps[1].Log; len(l) > projects.MaxLogLength || !strings.HasSuffix(l, "THE END") {
			t.Errorf("a long log is stored at %d bytes, ending %q", len(l), l[len(l)-10:])
		}
		if d, _ := s.Deployment(ctx, "plop", 2); d.SHA != "aaa" || len(d.Steps) != 1 {
			t.Errorf("Deployment 2 = %+v", d)
		}
		if _, err := s.Deployment(ctx, "plop", 3); !errors.Is(err, projects.ErrNoDeployment) {
			t.Errorf("Deployment 3 = %v, want ErrNoDeployment", err)
		}
		if _, err := s.Deployment(ctx, "gone", 1); !errors.Is(err, projects.ErrNotFound) {
			t.Errorf("Deployment of a missing project = %v", err)
		}
		if h, _ := s.History(ctx, "plop", 10); len(h) != 2 || h[0].FailedStep != "build" || h[0].Steps != nil {
			t.Errorf("History = %+v", h)
		}
	})
}
