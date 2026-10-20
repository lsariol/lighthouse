package orchestrator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/projects/projectstest"
	"github.com/lsariol/lighthouse/internal/release"
)

// releaseOf returns name's tracked tag.
func releaseOf(t *testing.T, s projects.Store, name string, tag string) projects.Release {
	t.Helper()
	list, _ := s.Releases(context.Background(), name)
	for _, r := range list {
		if r.Tag == tag {
			return r
		}
	}
	t.Fatalf("%s has no release %s", name, tag)
	return projects.Release{}
}

func publishing(t *testing.T) (*Orchestrator, *projectstest.Store, *fakeGitHub, *fakePublisher, *clock) {
	t.Helper()
	gh := newGitHub(map[string]string{"cove": "h1"})
	gh.repos["cove"].compose = releases("app")
	gh.repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "s100"}}
	gh.repos["cove"].files = map[string]string{"CHANGELOG.md": "## v1.1.0 (2026-10-21)\n\n### Fixed\n- A bug.\n\n## v1.0.0 (2026-10-20)\n\n### Added\n- Everything.\n"}
	o, store, _, c := setup(t, gh, "cove")
	pub := &fakePublisher{gh: gh}
	o.SetPublisher(pub, "")
	return o, store, gh, pub, c
}

func TestPublishAfterDeploy(t *testing.T) {
	o, store, gh, pub, _ := publishing(t)
	ctx := context.Background()

	o.Scan(ctx)
	o.PublishDue(ctx)
	r := releaseOf(t, store, "cove", "v1.0.0")
	if r.Status != projects.ReleasePublished || r.URL != "https://github.com/o/cove/releases/tag/v1.0.0" || r.PublishedAt == nil || r.Note != "" {
		t.Fatalf("after the deploy: %+v", r)
	}
	if c := pub.created[0]; c.Body != "## v1.0.0 (2026-10-20)\n\n### Added\n- Everything.\n" || !c.Latest || c.Prerelease {
		t.Errorf("created %+v", c)
	}

	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.1.0", SHA: "s110"}, github.Tag{Name: "v0.9.0", SHA: "s090"})
	o.Scan(ctx)
	o.PublishDue(ctx)
	if r := releaseOf(t, store, "cove", "v1.1.0"); r.Status != projects.ReleasePublished || !pub.created[1].Latest {
		t.Errorf("v1.1.0: %+v, latest %v", r, pub.created[1].Latest)
	}
	o.PublishDue(ctx)
	if len(pub.created) != 2 {
		t.Errorf("published again: %+v", pub.created)
	}

	// A backfill of an older tag: not verified, keeps its date, never latest.
	r, err := o.Backfill(ctx, "cove", "v0.9.0", "2025-05-01", "")
	if err != nil || r.Status != projects.ReleasePublished || !r.Backfill || !strings.Contains(r.Note, "not verified by a deploy") {
		t.Fatalf("Backfill = %+v, %v", r, err)
	}
	if c := pub.created[2]; c.Latest || !strings.HasPrefix(c.Body, "## v0.9.0 (2025-05-01)\n\nNo notes") {
		t.Errorf("backfill created %+v", c)
	}
	if !strings.Contains(r.Note, "no section for v0.9.0") {
		t.Errorf("the missing section isn't noted: %q", r.Note)
	}
	if _, err := o.Backfill(ctx, "cove", "v0.9.0", "", ""); err == nil || !strings.Contains(err.Error(), "already published") {
		t.Errorf("a second backfill = %v", err)
	}
	if _, err := o.Backfill(ctx, "cove", "v7.0.0", "", ""); err == nil || !strings.Contains(err.Error(), `no tag "v7.0.0"`) {
		t.Errorf("a backfill of a missing tag = %v", err)
	}
	if _, err := o.Backfill(ctx, "cove", "v1.0.0", "2025-13-01", ""); err == nil || !strings.Contains(err.Error(), "YYYY-MM-DD") {
		t.Errorf("a bad date = %v", err)
	}
}

func TestPublishRetries(t *testing.T) {
	o, store, gh, pub, c := publishing(t)
	ctx := context.Background()
	o.Scan(ctx)

	pub.err = &github.Error{Status: 502, Err: errors.New("502 Bad Gateway")}
	o.PublishDue(ctx)
	r := releaseOf(t, store, "cove", "v1.0.0")
	if r.Status != projects.ReleasePending || r.Attempts != 1 || !strings.Contains(r.LastError, "502") || !r.NextAttempt.Equal(c.now().Add(time.Minute)) {
		t.Fatalf("after a passing failure: %+v", r)
	}
	o.PublishDue(ctx)
	if releaseOf(t, store, "cove", "v1.0.0").Attempts != 1 {
		t.Error("tried again before the wait was over")
	}

	// Lighthouse restarts: another orchestrator on the same store carries on.
	c.advance(time.Minute)
	pub.err = &github.Error{Status: 403, Err: errors.New("403 Forbidden")}
	o.PublishDue(ctx)
	if r := releaseOf(t, store, "cove", "v1.0.0"); r.Status != projects.ReleaseFailed || r.Attempts != 2 || !r.NextAttempt.Equal(c.now().Add(time.Hour)) {
		t.Fatalf("after a permanent failure: %+v", r)
	}

	pub.err = github.ErrNotInstalled
	c.advance(time.Hour)
	o2 := New(store, gh, &fakeDeployer{}, nil, "t")
	o2.now = c.now
	o2.SetPublisher(pub, "")
	o2.PublishDue(ctx)
	if r := releaseOf(t, store, "cove", "v1.0.0"); r.Status != projects.ReleasePending || r.LastError != "" || !strings.Contains(r.Note, "isn't installed on o/cove") {
		t.Fatalf("without the App installed: %+v", r)
	}

	pub.err = nil
	c.advance(time.Hour)
	o2.PublishDue(ctx)
	if r := releaseOf(t, store, "cove", "v1.0.0"); r.Status != projects.ReleasePublished || r.Attempts != 0 || r.Note != "" {
		t.Errorf("once it works: %+v", r)
	}
}

func TestPublishChecksTheTag(t *testing.T) {
	o, store, gh, pub, _ := publishing(t)
	ctx := context.Background()
	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.1.0", SHA: "s110"}, github.Tag{Name: "v1.2.0", SHA: "s120"})
	store.TrackReleases(ctx, "cove", nil)
	store.TrackReleases(ctx, "cove", []projects.Release{
		{Tag: "v1.0.0", SHA: "s100", Status: projects.ReleasePending},
		{Tag: "v1.1.0", SHA: "s110", Status: projects.ReleasePending},
		{Tag: "v1.2.0", SHA: "s120", Status: projects.ReleasePending},
	})
	gh.repos["cove"].tags[0].SHA = "elsewhere"
	gh.repos["cove"].tags = gh.repos["cove"].tags[:2]
	gh.repos["cove"].releases = []github.Release{{Tag: "v1.1.0", URL: "https://made/by/hand"}}

	o.PublishDue(ctx)
	if r := releaseOf(t, store, "cove", "v1.0.0"); r.Status != projects.ReleaseMoved || !strings.Contains(r.Note, "elsewhe") {
		t.Errorf("a moved tag: %+v", r)
	}
	if r := releaseOf(t, store, "cove", "v1.1.0"); r.Status != projects.ReleasePublished || r.URL != "https://made/by/hand" || !strings.Contains(r.Note, "adopted") {
		t.Errorf("an existing Release: %+v", r)
	}
	if r := releaseOf(t, store, "cove", "v1.2.0"); r.Status != projects.ReleaseFailed || r.NextAttempt != nil || !strings.Contains(r.LastError, "no longer exists") {
		t.Errorf("a deleted tag: %+v", r)
	}
	if len(pub.created) != 0 {
		t.Errorf("published %+v", pub.created)
	}
}

func TestPublishingOff(t *testing.T) {
	o, _, _, _ := setup(t, newGitHub(map[string]string{"cove": "h1"}), "cove")
	o.SetPublisher(nil, "GITHUB_APP_ID isn't set")
	if on, why := o.Publishing(); on || why != "GITHUB_APP_ID isn't set" {
		t.Errorf("Publishing = %v, %q", on, why)
	}
	if _, err := o.Backfill(context.Background(), "cove", "v1.0.0", "", ""); !errors.Is(err, ErrPublishingOff) || !strings.Contains(err.Error(), "GITHUB_APP_ID") {
		t.Errorf("Backfill while off = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	o.RunPublisher(ctx)
}

func TestLatest(t *testing.T) {
	published := []github.Release{{Tag: "v1.2.0"}, {Tag: "v2.0.0", Draft: true}, {Tag: "v3.0.0-rc.1", Prerelease: true}, {Tag: "nightly"}}
	for tag, want := range map[string]bool{"v1.3.0": true, "v1.1.0": false, "v1.2.0": false, "v2.0.0-rc.2": false} {
		v, _ := release.ParseTag(tag)
		if got := latest(v, published); got != want {
			t.Errorf("latest(%s) = %v, want %v", tag, got, want)
		}
	}
}
