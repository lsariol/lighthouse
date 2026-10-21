package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/lsariol/lighthouse/internal/changelog"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/release"
)

// Publisher creates GitHub Releases with write access; *github.App
// implements it. github.ErrNotInstalled if it can't write to a repository.
type Publisher interface {
	Publish(ctx context.Context, repo github.Repo, r github.NewRelease) (github.Release, error)
}

// PublishEvery is how often due Releases are looked for, besides right
// after a deploy that queued one.
const PublishEvery = time.Minute

// Waits before publishing is tried again: after a permanent failure (a
// permission, a key), and while the App isn't installed on the repository.
const (
	permanentRetry    = time.Hour
	notInstalledRetry = time.Hour
)

// SetPublisher turns publishing on with p, or, with p nil, off, saying why.
// Call it before Run.
func (o *Orchestrator) SetPublisher(p Publisher, off string) {
	o.publisher, o.publishOff = p, off
}

// Publishing says whether Releases are published, and if not, why.
func (o *Orchestrator) Publishing() (bool, string) {
	if o.publisher == nil && o.publishOff == "" {
		return false, "no GitHub App is set up"
	}
	return o.publisher != nil, o.publishOff
}

// RunPublisher publishes due Releases every PublishEvery and after each
// deploy that queues one, until ctx is cancelled. Pausing doesn't stop it.
func (o *Orchestrator) RunPublisher(ctx context.Context) {
	if o.publisher == nil {
		return
	}
	ticker := time.NewTicker(PublishEvery)
	defer ticker.Stop()
	for {
		o.PublishDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-o.publishWake:
		}
	}
}

// wakePublisher makes RunPublisher look for due Releases now.
func (o *Orchestrator) wakePublisher() {
	select {
	case o.publishWake <- struct{}{}:
	default:
	}
}

// PublishDue tries to publish every Release that's due.
func (o *Orchestrator) PublishDue(ctx context.Context) {
	due, err := o.store.DueReleases(ctx, o.now())
	if err != nil {
		slog.Error("publishing: can't read the releases to publish", "err", err)
		return
	}
	for _, r := range due {
		if ctx.Err() != nil {
			return
		}
		o.publish(ctx, r)
	}
}

// publish tries once to publish r and records how it went.
func (o *Orchestrator) publish(ctx context.Context, r projects.Release) projects.Published {
	o.publishing.Lock()
	defer o.publishing.Unlock()
	p, err := o.store.Get(ctx, r.Project)
	if err != nil {
		return projects.Published{}
	}
	res := o.tryPublish(ctx, p, r)
	switch {
	case res.Status == projects.ReleasePublished:
		slog.Info("published a GitHub Release", "project", p.Name, "tag", r.Tag, "url", res.URL, "note", res.Note)
	case res.Error != "":
		slog.Warn("publishing a GitHub Release failed", "project", p.Name, "tag", r.Tag, "status", res.Status, "err", res.Error)
	}
	if err := o.store.RecordPublish(context.WithoutCancel(ctx), p.Name, r.Tag, res); err != nil {
		slog.Error("publishing: recording the attempt failed; it's tried again", "project", p.Name, "tag", r.Tag, "err", err)
	}
	return res
}

// tryPublish publishes r's GitHub Release, unless its tag is gone or moved,
// or one already exists (then it's adopted).
func (o *Orchestrator) tryPublish(ctx context.Context, p projects.Project, r projects.Release) projects.Published {
	now := o.now()
	again := func(err error) projects.Published {
		if transientErr(err) {
			next := now.Add(min(minBackoff<<min(r.Attempts, 5), maxBackoff))
			return projects.Published{Status: projects.ReleasePending, Note: r.Note, Error: projects.ErrorText(err), NextAttempt: &next, At: now}
		}
		next := now.Add(permanentRetry)
		return projects.Published{Status: projects.ReleaseFailed, Note: r.Note, Error: projects.ErrorText(err), NextAttempt: &next, At: now}
	}

	sha, found, err := o.github.TagCommit(ctx, p.Repo, r.Tag, o.gitToken)
	switch {
	case err != nil:
		return again(err)
	case !found:
		return projects.Published{Status: projects.ReleaseFailed, Error: "the tag " + r.Tag + " no longer exists on GitHub, so it isn't published", At: now}
	case sha != r.SHA:
		return projects.Published{Status: projects.ReleaseMoved, At: now,
			Note: fmt.Sprintf("the tag moved from %s to %s, so it isn't published", short(r.SHA), short(sha))}
	}

	existing, found, err := o.github.ReleaseByTag(ctx, p.Repo, r.Tag, o.gitToken)
	if err != nil {
		return again(err)
	}
	if found {
		return projects.Published{Status: projects.ReleasePublished, URL: existing.URL, At: existing.When(now), Note: "its Release already existed, so Lighthouse adopted it"}
	}

	body, warnings, err := o.notes(ctx, p, r, now)
	if err != nil {
		return again(err)
	}
	all, err := o.github.Releases(ctx, p.Repo, o.gitToken)
	if err != nil {
		return again(err)
	}
	v, _ := release.ParseTag(r.Tag)
	created, err := o.publisher.Publish(ctx, p.Repo, github.NewRelease{Tag: r.Tag, Body: body, Prerelease: v.Prerelease(), Latest: latest(v, all)})
	if errors.Is(err, github.ErrNotInstalled) {
		next := now.Add(notInstalledRetry)
		return projects.Published{Status: projects.ReleasePending, NextAttempt: &next, At: now,
			Note: "not published: the GitHub App isn't installed on " + p.Repo.String() + "; install it there and it's published within the hour"}
	}
	if err != nil {
		return again(err)
	}

	var notes []string
	if r.Backfill {
		notes = append(notes, "published with `publish`, not verified by a deploy")
	}
	return projects.Published{Status: projects.ReleasePublished, URL: created.URL, At: created.When(now), Note: strings.Join(append(notes, warnings...), "; ")}
}

// notes is r's Release text: its CHANGELOG.md section at its commit (or its
// NotesRef), dated as the CHANGELOG says, or r's NotesDate, or now.
func (o *Orchestrator) notes(ctx context.Context, p projects.Project, r projects.Release, now time.Time) (string, []string, error) {
	ref := r.SHA
	if r.NotesRef != "" {
		ref = r.NotesRef
	}
	text, err := o.github.File(ctx, p.Repo, ref, changelog.File, o.gitToken)
	if err != nil {
		return "", nil, err
	}
	if date, err := time.Parse(time.DateOnly, r.NotesDate); err == nil {
		body, warnings := changelog.NotesDated(text, r.Tag, date)
		return body, warnings, nil
	}
	body, warnings := changelog.Notes(text, r.Tag, now)
	return body, warnings, nil
}

// latest reports whether v becomes GitHub's "latest" Release: a stable
// version higher than every stable Release published.
func latest(v release.Version, published []github.Release) bool {
	if v.Prerelease() {
		return false
	}
	for _, r := range published {
		if w, ok := release.Parse(r.Tag); ok && !r.Draft && !r.Prerelease && !v.Newer(w) {
			return false
		}
	}
	return true
}

// ErrPublishingOff means Lighthouse has no GitHub App to publish with.
var ErrPublishingOff = errors.New("publishing is off")

// Backfill publishes a Release for an existing tag now, without deploying
// it: its notes from notesRef's CHANGELOG.md if given, dated date if given.
// If the attempt fails, the release stays queued and is tried again.
func (o *Orchestrator) Backfill(ctx context.Context, name string, tag string, date string, notesRef string) (projects.Release, error) {
	if on, why := o.Publishing(); !on {
		return projects.Release{}, fmt.Errorf("%w: %s", ErrPublishingOff, why)
	}
	if date != "" {
		if _, err := time.Parse(time.DateOnly, date); err != nil {
			return projects.Release{}, fmt.Errorf("the date %q isn't YYYY-MM-DD", date)
		}
	}
	if _, ok := release.ParseTag(tag); !ok {
		return projects.Release{}, fmt.Errorf("%q isn't a version tag (such as v1.2.3 or v1.2.3-rc.1)", tag)
	}
	p, err := o.store.Get(ctx, name)
	if err != nil {
		return projects.Release{}, err
	}
	sha, found, err := o.github.TagCommit(ctx, p.Repo, tag, o.gitToken)
	if err != nil {
		return projects.Release{}, err
	}
	if !found {
		return projects.Release{}, fmt.Errorf("%s has no tag %q", p.Repo, tag)
	}
	rows, err := o.store.Releases(ctx, p.Name)
	if err != nil {
		return projects.Release{}, err
	}
	for _, r := range rows {
		switch {
		case r.Tag != tag:
		case r.Status == projects.ReleasePublished:
			return r, fmt.Errorf("%s %s is already published: %s", p.Name, tag, r.URL)
		case r.SHA != sha:
			return r, fmt.Errorf("%s %s moved from %s to %s since Lighthouse first saw it, so it isn't published", p.Name, tag, short(r.SHA), short(sha))
		}
	}

	if err := o.store.QueueBackfill(ctx, p.Name, projects.Release{Tag: tag, SHA: sha, NotesRef: notesRef, NotesDate: date}); err != nil {
		return projects.Release{}, err
	}
	rows, err = o.store.Releases(ctx, p.Name)
	if err != nil {
		return projects.Release{}, err
	}
	for _, r := range rows {
		if r.Tag == tag {
			o.publish(ctx, r)
			break
		}
	}
	rows, err = o.store.Releases(ctx, p.Name)
	for _, r := range rows {
		if r.Tag == tag {
			return r, err
		}
	}
	return projects.Release{}, projects.ErrNoRelease
}
