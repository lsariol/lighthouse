// Package projectstest has an in-memory Store and the tests every Store must pass.
package projectstest

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/release"
)

// Store is an in-memory projects.Store. The zero value is ready to use.
type Store struct {
	mu          sync.Mutex
	projects    []projects.Project
	deployments []projects.Deployment
	releases    []projects.Release
}

var _ projects.Store = (*Store)(nil)

func (s *Store) index(name string) int {
	for i, p := range s.projects {
		if strings.EqualFold(p.Name, name) {
			return i
		}
	}
	return -1
}

func (s *Store) repoIndex(repo github.Repo) int {
	for i, p := range s.projects {
		if strings.EqualFold(p.Repo.Owner, repo.Owner) && strings.EqualFold(p.Repo.Name, repo.Name) {
			return i
		}
	}
	return -1
}

func (s *Store) List(ctx context.Context) ([]projects.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := append([]projects.Project(nil), s.projects...)
	sort.Slice(list, func(i, j int) bool { return strings.ToLower(list[i].Name) < strings.ToLower(list[j].Name) })
	return list, nil
}

func (s *Store) Get(ctx context.Context, name string) (projects.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if i := s.index(name); i >= 0 {
		return s.projects[i], nil
	}
	return projects.Project{}, projects.ErrNotFound
}

func (s *Store) Add(ctx context.Context, name string, repo github.Repo) (projects.Project, error) {
	return s.add(name, repo, projects.KindDeploy)
}

func (s *Store) AddPublishOnly(ctx context.Context, name string, repo github.Repo) (projects.Project, error) {
	return s.add(name, repo, projects.KindPublish)
}

func (s *Store) add(name string, repo github.Repo, kind string) (projects.Project, error) {
	if err := projects.ValidateName(name); err != nil {
		return projects.Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.index(name) >= 0 {
		return projects.Project{}, projects.ErrNameTaken
	}
	if s.repoIndex(repo) >= 0 {
		return projects.Project{}, projects.ErrRepoWatched
	}
	p := projects.Project{Name: name, Repo: repo, CreatedAt: time.Now(), Mode: "branch", Tier: "app", Kind: kind}
	s.projects = append(s.projects, p)
	return p, nil
}

func (s *Store) Remove(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	removed := s.projects[i].Name
	s.projects = append(s.projects[:i], s.projects[i+1:]...)

	kept := s.deployments[:0]
	for _, d := range s.deployments {
		if d.Project != removed {
			kept = append(kept, d)
		}
	}
	s.deployments = kept

	keptReleases := s.releases[:0]
	for _, r := range s.releases {
		if r.Project != removed {
			keptReleases = append(keptReleases, r)
		}
	}
	s.releases = keptReleases
	return nil
}

func (s *Store) Rename(ctx context.Context, name string, newName string) error {
	if err := projects.ValidateName(newName); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	if j := s.index(newName); j >= 0 && j != i {
		return projects.ErrNameTaken
	}
	old := s.projects[i].Name
	s.projects[i].Name = newName
	for k := range s.deployments {
		if s.deployments[k].Project == old {
			s.deployments[k].Project = newName
		}
	}
	for k := range s.releases {
		if s.releases[k].Project == old {
			s.releases[k].Project = newName
		}
	}
	return nil
}

func (s *Store) SetRepo(ctx context.Context, name string, repo github.Repo) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	if j := s.repoIndex(repo); j >= 0 && j != i {
		return projects.ErrRepoWatched
	}
	s.projects[i].Repo = repo
	return nil
}

func (s *Store) SetComposeProject(ctx context.Context, name string, composeProject string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	for j, p := range s.projects {
		if j != i && p.ComposeProject == composeProject {
			return projects.ErrComposeProjectTaken
		}
	}
	s.projects[i].ComposeProject = composeProject
	return nil
}

func (s *Store) SetSettings(ctx context.Context, name string, mode string, tier string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	s.projects[i].Mode, s.projects[i].Tier = mode, tier
	return nil
}

func (s *Store) SetHeld(ctx context.Context, name string, sha string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	s.projects[i].HeldSHA = sha
	return nil
}

func (s *Store) SetStopped(ctx context.Context, name string, stopped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	s.projects[i].Stopped = stopped
	return nil
}

func (s *Store) RecordCheck(ctx context.Context, name string, checkErr error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	p := &s.projects[i]
	now := time.Now()
	p.LastCheckedAt = &now
	p.Checks++
	if checkErr == nil {
		p.LastError, p.LastErrorAt = "", nil
	} else {
		p.LastError, p.LastErrorAt = projects.ErrorText(checkErr), &now
	}
	return nil
}

func (s *Store) RecordDeployment(ctx context.Context, d projects.Deployment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(d.Project)
	if i < 0 {
		return projects.ErrNotFound
	}
	p := &s.projects[i]
	d.Project = p.Name
	d.Error = projects.ErrorText(projects.ErrorString(d.Error))
	steps := make([]projects.Step, len(d.Steps))
	for k, st := range d.Steps {
		st.Log = projects.LogText(st.Log)
		steps[k] = st
	}
	d.Steps = steps
	s.deployments = append(s.deployments, d)

	finished := d.FinishedAt
	if d.Status == projects.StatusSucceeded {
		p.DeployedSHA, p.DeployedVersion, p.DeployedAt = d.SHA, d.Version, &finished
		p.HighestVersion = release.Higher(p.HighestVersion, d.Version)
		p.LastError, p.LastErrorAt = "", nil
		p.FailureCount, p.FailingSHA, p.Broken = 0, "", false
		for _, r := range projects.DeployedReleases(s.releasesOf(p.Name), d) {
			s.putRelease(r, false)
		}
		return nil
	}

	p.LastError, p.LastErrorAt = d.Error, &finished
	if d.FailureKind == projects.FailurePermanent {
		if p.FailingSHA == d.SHA {
			p.FailureCount++
		} else {
			p.FailureCount, p.FailingSHA = 1, d.SHA
		}
		p.Broken = p.FailureCount >= projects.BrokenAfter
	}
	return nil
}

func (s *Store) ClearFailures(ctx context.Context, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	p := &s.projects[i]
	p.FailureCount, p.FailingSHA, p.Broken = 0, "", false
	return nil
}

// releasesOf returns the project's tracked tags, oldest first.
func (s *Store) releasesOf(project string) []projects.Release {
	var list []projects.Release
	for _, r := range s.releases {
		if project == "" || r.Project == project {
			list = append(list, r)
		}
	}
	return list
}

// putRelease adds r, or replaces the row for its tag unless keep.
func (s *Store) putRelease(r projects.Release, keep bool) {
	now := time.Now()
	for k, old := range s.releases {
		if old.Project == r.Project && old.Tag == r.Tag {
			if !keep {
				r.CreatedAt, r.UpdatedAt = old.CreatedAt, now
				s.releases[k] = r
			}
			return
		}
	}
	r.CreatedAt, r.UpdatedAt = now, now
	s.releases = append(s.releases, r)
}

func (s *Store) Releases(ctx context.Context, name string) ([]projects.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name == "" {
		return s.releasesOf(""), nil
	}
	i := s.index(name)
	if i < 0 {
		return nil, projects.ErrNotFound
	}
	return s.releasesOf(s.projects[i].Name), nil
}

func (s *Store) TrackReleases(ctx context.Context, name string, rs []projects.Release) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	for _, r := range rs {
		r.Project = s.projects[i].Name
		s.putRelease(r, true)
	}
	s.projects[i].ReleasesTracked = true
	return nil
}

func (s *Store) SetReleaseStatus(ctx context.Context, name string, tag string, status string, note string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	for k, r := range s.releases {
		if r.Project == s.projects[i].Name && r.Tag == tag {
			s.releases[k].Status, s.releases[k].Note, s.releases[k].UpdatedAt = status, note, time.Now()
			return nil
		}
	}
	return projects.ErrNoRelease
}

func (s *Store) DueReleases(ctx context.Context, now time.Time) ([]projects.Release, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var due []projects.Release
	for _, r := range s.releases {
		switch {
		case r.Status == projects.ReleasePending && (r.NextAttempt == nil || !r.NextAttempt.After(now)),
			r.Status == projects.ReleaseFailed && r.NextAttempt != nil && !r.NextAttempt.After(now):
			due = append(due, r)
		}
	}
	return due, nil
}

func (s *Store) RecordPublish(ctx context.Context, name string, tag string, p projects.Published) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	for k, r := range s.releases {
		if r.Project != s.projects[i].Name || r.Tag != tag {
			continue
		}
		r.Status, r.Note, r.LastError, r.NextAttempt, r.UpdatedAt = p.Status, p.Note, p.Error, p.NextAttempt, time.Now()
		r.Attempts++
		if p.Error == "" {
			r.Attempts = 0
		}
		if p.Status == projects.ReleasePublished {
			at := p.At
			r.URL, r.PublishedAt = p.URL, &at
		}
		s.releases[k] = r
		return nil
	}
	return projects.ErrNoRelease
}

func (s *Store) QueueBackfill(ctx context.Context, name string, r projects.Release) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.ErrNotFound
	}
	now := time.Now()
	for k, old := range s.releases {
		if old.Project == s.projects[i].Name && old.Tag == r.Tag {
			old.Status, old.Note, old.Backfill, old.NotesRef, old.NotesDate = projects.ReleasePending, "", true, r.NotesRef, r.NotesDate
			old.Attempts, old.LastError, old.NextAttempt, old.UpdatedAt = 0, "", &now, now
			s.releases[k] = old
			return nil
		}
	}
	s.releases = append(s.releases, projects.Release{Project: s.projects[i].Name, Tag: r.Tag, SHA: r.SHA, Status: projects.ReleasePending,
		Backfill: true, NotesRef: r.NotesRef, NotesDate: r.NotesDate, NextAttempt: &now, CreatedAt: now, UpdatedAt: now})
	return nil
}

// newest returns the project's deployments, newest first.
func (s *Store) newest(project string) []projects.Deployment {
	var list []projects.Deployment
	for k := len(s.deployments) - 1; k >= 0; k-- {
		if s.deployments[k].Project == project {
			list = append(list, s.deployments[k])
		}
	}
	sort.SliceStable(list, func(a, b int) bool { return list[a].StartedAt.After(list[b].StartedAt) })
	return list
}

func (s *Store) History(ctx context.Context, name string, limit int) ([]projects.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return nil, projects.ErrNotFound
	}
	list := s.newest(s.projects[i].Name)
	if len(list) > limit {
		list = list[:limit]
	}
	history := make([]projects.Deployment, len(list))
	for k, d := range list {
		d.Steps = nil
		history[k] = d
	}
	return history, nil
}

func (s *Store) Deployment(ctx context.Context, name string, n int) (projects.Deployment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.index(name)
	if i < 0 {
		return projects.Deployment{}, projects.ErrNotFound
	}
	list := s.newest(s.projects[i].Name)
	if n < 1 || n > len(list) {
		return projects.Deployment{}, projects.ErrNoDeployment
	}
	return list[n-1], nil
}
