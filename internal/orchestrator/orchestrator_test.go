package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/deploy"
	"github.com/lsariol/lighthouse/internal/docker"
	"github.com/lsariol/lighthouse/internal/github"
	"github.com/lsariol/lighthouse/internal/projects"
	"github.com/lsariol/lighthouse/internal/projects/projectstest"
	"github.com/lsariol/lighthouse/internal/release"
	"github.com/lsariol/lighthouse/internal/settings"
)

// fakeRepo is one repository on the fake GitHub.
type fakeRepo struct {
	head      string
	tags      []github.Tag
	compose   string
	down      bool
	offBranch map[string]bool   // commits not on the default branch
	files     map[string]string // other files at the top, the same at every commit
	releases  []github.Release  // its GitHub Releases
}

// fakeGitHub serves repositories by name, honouring ETags, and counts
// requests.
type fakeGitHub struct {
	mu    sync.Mutex
	repos map[string]*fakeRepo
	calls map[string]int
}

func newGitHub(heads map[string]string) *fakeGitHub {
	g := &fakeGitHub{repos: map[string]*fakeRepo{}, calls: map[string]int{}}
	for name, sha := range heads {
		g.repos[name] = &fakeRepo{head: sha}
	}
	return g
}

func (g *fakeGitHub) repo(r github.Repo, token string) (*fakeRepo, error) {
	if token == "" {
		return nil, errors.New("no token")
	}
	rp, ok := g.repos[r.Name]
	if !ok {
		return nil, &github.Error{Status: 404, Err: errors.New("404 Not Found")}
	}
	if rp.down {
		return nil, &github.Error{Status: 502, Err: errors.New("502 Bad Gateway")}
	}
	return rp, nil
}

func (g *fakeGitHub) LatestCommit(ctx context.Context, r github.Repo, token string) (string, error) {
	sha, _, err := g.CheckCommit(ctx, r, token, "")
	return sha, err
}

func (g *fakeGitHub) CheckCommit(ctx context.Context, r github.Repo, token string, etag string) (string, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return "", "", err
	}
	if tag := `"` + rp.head + `"`; etag == tag {
		g.calls["commits 304"]++
		return "", "", github.ErrNotModified
	}
	g.calls["commits"]++
	return rp.head, `"` + rp.head + `"`, nil
}

func (g *fakeGitHub) Tags(ctx context.Context, r github.Repo, token string, etag string) ([]github.Tag, string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return nil, "", err
	}
	tag := fmt.Sprintf("%q", fmt.Sprint(rp.tags))
	if etag == tag {
		g.calls["tags 304"]++
		return nil, "", github.ErrNotModified
	}
	g.calls["tags"]++
	return append([]github.Tag(nil), rp.tags...), tag, nil
}

func (g *fakeGitHub) ResolveCommit(ctx context.Context, r github.Repo, ref string, token string) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, err := g.repo(r, token); err != nil {
		return "", err
	}
	for _, sha := range []string{"aaa1111bbb", "old0000ccc"} {
		if strings.HasPrefix(sha, ref) {
			return sha, nil
		}
	}
	return "", &github.Error{Status: 404, Err: errors.New("404 Not Found")}
}

func (g *fakeGitHub) ComposeFile(ctx context.Context, r github.Repo, sha string, token string) (string, []byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return "", nil, err
	}
	g.calls["compose"]++
	text := rp.compose
	if text == "" {
		text = "services: {}\n"
	}
	return "compose.yaml", []byte(text), nil
}

func (g *fakeGitHub) File(ctx context.Context, r github.Repo, sha string, name string, token string) ([]byte, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return nil, err
	}
	text, ok := rp.files[name]
	if !ok {
		return nil, nil
	}
	return []byte(text), nil
}

func (g *fakeGitHub) Contains(ctx context.Context, r github.Repo, head string, sha string, token string) (bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return false, err
	}
	g.calls["compare"]++
	return !rp.offBranch[sha], nil
}

func (g *fakeGitHub) TagCommit(ctx context.Context, r github.Repo, tag string, token string) (string, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return "", false, err
	}
	t, ok := release.Find(rp.tags, tag)
	return t.SHA, ok, nil
}

func (g *fakeGitHub) ReleaseByTag(ctx context.Context, r github.Repo, tag string, token string) (github.Release, bool, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return github.Release{}, false, err
	}
	for _, rel := range rp.releases {
		if rel.Tag == tag {
			return rel, true, nil
		}
	}
	return github.Release{}, false, nil
}

func (g *fakeGitHub) Releases(ctx context.Context, r github.Repo, token string) ([]github.Release, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	rp, err := g.repo(r, token)
	if err != nil {
		return nil, err
	}
	return append([]github.Release(nil), rp.releases...), nil
}

// fakePublisher creates Releases on the fake GitHub, or fails with err.
type fakePublisher struct {
	gh      *fakeGitHub
	err     error
	created []github.NewRelease
}

func (f *fakePublisher) Publish(ctx context.Context, r github.Repo, rel github.NewRelease) (github.Release, error) {
	if f.err != nil {
		return github.Release{}, f.err
	}
	f.gh.mu.Lock()
	defer f.gh.mu.Unlock()
	f.created = append(f.created, rel)
	out := github.Release{ID: int64(len(f.created)), Tag: rel.Tag, URL: "https://github.com/o/" + r.Name + "/releases/tag/" + rel.Tag, Prerelease: rel.Prerelease}
	f.gh.repos[r.Name].releases = append(f.gh.repos[r.Name].releases, out)
	return out, nil
}

// releases is a compose file that deploys release tags.
func releases(tier string) string {
	return "x-lighthouse:\n  deploy: releases\n  tier: " + tier + "\nservices: {}\n"
}

// fakeDeployer records deploys; result decides how each goes (success if nil).
type fakeDeployer struct {
	mu       sync.Mutex
	deployed []string
	checked  []string
	result   func(req deploy.Request) deploy.Result
	handOffs []deploy.HandOff
	forgot   int
	updating map[string]bool
}

func (f *fakeDeployer) HandOffInProgress(project string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.updating[project]
}

func (f *fakeDeployer) Deploy(ctx context.Context, req deploy.Request) deploy.Result {
	f.mu.Lock()
	what := req.SHA
	if req.Version != "" {
		what = req.Version
	}
	f.deployed = append(f.deployed, req.Project.Name+"@"+what)
	f.mu.Unlock()

	compose := strings.ToLower(req.Project.Repo.Name)
	if req.Claim != nil {
		if err := req.Claim(ctx, compose); err != nil {
			return deploy.Result{Status: projects.StatusFailed, FailureKind: projects.FailurePermanent, FailedStep: deploy.StepInspect, Err: err}
		}
	}
	if f.result != nil {
		return f.result(req)
	}
	return deploy.Result{Status: projects.StatusSucceeded, ComposeProject: compose,
		Steps: []projects.Step{{Name: deploy.StepFetch, Status: projects.StepSucceeded}}}
}

func (f *fakeDeployer) Check(ctx context.Context, req deploy.Request) deploy.Result {
	f.mu.Lock()
	f.checked = append(f.checked, req.Project.Name+"@"+req.SHA)
	f.mu.Unlock()
	if req.Claim != nil {
		if err := req.Claim(ctx, strings.ToLower(req.Project.Repo.Name)); err != nil {
			return deploy.Result{Status: projects.StatusFailed, FailureKind: projects.FailurePermanent, FailedStep: deploy.StepInspect, Err: err}
		}
	}
	return deploy.Result{Status: projects.StatusSucceeded}
}

func (f *fakeDeployer) HandOffs() ([]deploy.HandOff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handOffs, nil
}

func (f *fakeDeployer) Forget(h deploy.HandOff) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handOffs = nil
	f.forgot++
	return nil
}

func (f *fakeDeployer) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.deployed)
}

func (f *fakeDeployer) list() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.deployed, ",")
}

func failing(kind string) func(deploy.Request) deploy.Result {
	return func(deploy.Request) deploy.Result {
		return deploy.Result{Status: projects.StatusFailed, FailureKind: kind, FailedStep: deploy.StepBuild,
			Err: errors.New("build: exit status 1")}
	}
}

// clock is a time the test moves forward.
type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func setup(t *testing.T, gh *fakeGitHub, names ...string) (*Orchestrator, *projectstest.Store, *fakeDeployer, *clock) {
	t.Helper()
	store := &projectstest.Store{}
	for _, name := range names {
		if _, err := store.Add(context.Background(), name, github.Repo{Owner: "o", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	d := &fakeDeployer{}
	o := New(store, gh, d, nil, "t")
	c := &clock{t: time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)}
	o.now = c.now
	return o, store, d, c
}

func get(t *testing.T, s projects.Store, name string) projects.Project {
	t.Helper()
	p, err := s.Get(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanDeploysNewCommitsAndSkipsFailures(t *testing.T) {
	gh := newGitHub(map[string]string{"a": "aaa", "c": "ccc"})
	o, store, d, _ := setup(t, gh, "a", "b", "c")
	ctx := context.Background()

	err := o.Scan(ctx)
	if err == nil || !strings.Contains(err.Error(), "b") {
		t.Errorf("Scan = %v, want an error naming b", err)
	}
	if d.list() != "a@aaa,c@ccc" {
		t.Errorf("deployed %v", d.deployed)
	}
	if b := get(t, store, "b"); b.LastError == "" || b.Checks != 1 {
		t.Errorf("b: %+v", b)
	}
	a := get(t, store, "a")
	if a.DeployedSHA != "aaa" || a.ComposeProject != "a" || a.LastError != "" || a.Mode != "branch" {
		t.Errorf("a: %+v", a)
	}
	if h, _ := store.Deployment(ctx, "a", 1); h.Trigger != projects.TriggerCheck || len(h.Steps) != 1 {
		t.Errorf("a's deployment = %+v", h)
	}

	o.Scan(ctx)
	if d.count() != 2 {
		t.Errorf("second scan deployed %v", d.deployed)
	}
	if gh.calls["commits"] != 2 || gh.calls["commits 304"] != 2 || gh.calls["compose"] != 2 {
		t.Errorf("GitHub calls: %v", gh.calls)
	}
}

func TestBrokenAfterRepeatedFailures(t *testing.T) {
	gh := newGitHub(map[string]string{"a": "aaa"})
	o, store, d, _ := setup(t, gh, "a")
	d.result = failing(projects.FailurePermanent)
	ctx := context.Background()

	for i := 1; i <= projects.BrokenAfter; i++ {
		err := o.Scan(ctx)
		if i < projects.BrokenAfter && err == nil {
			t.Fatalf("scan %d succeeded", i)
		}
	}
	a := get(t, store, "a")
	if !a.Broken || a.FailureCount != projects.BrokenAfter || !strings.Contains(a.LastError, "broken after 3") {
		t.Fatalf("after %d failures: %+v", projects.BrokenAfter, a)
	}

	if err := o.Scan(ctx); err != nil {
		t.Errorf("scan of a broken project = %v", err)
	}
	if d.count() != projects.BrokenAfter {
		t.Errorf("deployed %d times, want %d", d.count(), projects.BrokenAfter)
	}
	if a := get(t, store, "a"); !strings.Contains(a.LastError, "build: exit status 1") {
		t.Errorf("the error that broke it was lost: %q", a.LastError)
	}

	gh.repos["a"].head = "bbb"
	d.result = nil
	if err := o.Scan(ctx); err != nil {
		t.Fatal(err)
	}
	if a := get(t, store, "a"); a.Broken || a.DeployedSHA != "bbb" || a.FailureCount != 0 {
		t.Errorf("after a new commit deployed: %+v", a)
	}
}

func TestTransientFailuresWait(t *testing.T) {
	gh := newGitHub(map[string]string{"a": "aaa"})
	o, store, d, clk := setup(t, gh, "a")
	d.result = failing(projects.FailureTransient)
	ctx := context.Background()

	o.Scan(ctx)
	for _, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute} {
		before := d.count()
		clk.advance(wait - time.Second)
		o.Scan(ctx)
		if d.count() != before {
			t.Fatalf("deployed again before %s", wait)
		}
		clk.advance(time.Second)
		o.Scan(ctx)
		if d.count() != before+1 {
			t.Fatalf("not deployed again after %s", wait)
		}
	}
	if a := get(t, store, "a"); a.Broken || a.FailureCount != 0 {
		t.Errorf("transient failures broke it: %+v", a)
	}

	for i := 0; i < 10; i++ {
		clk.advance(maxBackoff)
		o.Scan(ctx)
	}
	if w := o.watchOf("a"); w.backoff != maxBackoff {
		t.Errorf("backoff = %s", w.backoff)
	}

	d.result = nil
	if err := o.Deploy(ctx, "a", ""); err != nil {
		t.Fatal(err)
	}
	if w := o.watchOf("a"); !w.nextAttempt.IsZero() {
		t.Error("a success didn't clear the wait")
	}

	gh.repos["a"].down = true
	o.Scan(ctx)
	if w := o.watchOf("a"); w.backoff != minBackoff {
		t.Errorf("GitHub down: backoff %s", w.backoff)
	}
}

func TestRetry(t *testing.T) {
	o, store, d, _ := setup(t, newGitHub(map[string]string{"a": "aaa"}), "a")
	d.result = failing(projects.FailurePermanent)
	for i := 0; i < projects.BrokenAfter; i++ {
		o.Scan(context.Background())
	}

	d.result = nil
	if err := o.Retry(context.Background(), "A"); err != nil {
		t.Fatal(err)
	}
	if a := get(t, store, "a"); a.Broken || a.DeployedSHA != "aaa" {
		t.Errorf("after retry: %+v", a)
	}
	if h, _ := store.Deployment(context.Background(), "a", 1); h.Trigger != projects.TriggerRetry {
		t.Errorf("retry's trigger = %q", h.Trigger)
	}
}

// A release that broke a project and then deployed with `retry` is published
// like one a check deployed.
func TestRetryPublishes(t *testing.T) {
	gh := newGitHub(map[string]string{"cove": "h1"})
	gh.repos["cove"].compose = releases("app")
	gh.repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "s100"}}
	o, store, d, _ := setup(t, gh, "cove")
	ctx := context.Background()
	d.result = failing(projects.FailurePermanent)
	for i := 0; i < projects.BrokenAfter; i++ {
		o.Scan(ctx)
	}
	if p := get(t, store, "cove"); !p.Broken || statuses(t, store, "cove") != "v1.0.0=ready" {
		t.Fatalf("broken %v, %s", p.Broken, statuses(t, store, "cove"))
	}

	d.result = nil
	if err := o.Retry(ctx, "cove"); err != nil {
		t.Fatal(err)
	}
	if got := statuses(t, store, "cove"); got != "v1.0.0=pending" {
		t.Errorf("after a successful retry: %s", got)
	}
}

func TestReleaseMode(t *testing.T) {
	gh := newGitHub(map[string]string{"cove": "head1"})
	gh.repos["cove"].compose = releases("infra")
	gh.repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "s100"}, {Name: "v1.1.0", SHA: "s110"}, {Name: "latest", SHA: "head1"}}
	o, store, d, _ := setup(t, gh, "cove")
	ctx := context.Background()

	o.Scan(ctx)
	p := get(t, store, "cove")
	if d.list() != "cove@v1.1.0" || p.DeployedSHA != "s110" || p.DeployedVersion != "v1.1.0" || p.Mode != "releases" || p.Tier != "infra" {
		t.Fatalf("deployed %s; project %+v", d.list(), p)
	}

	gh.repos["cove"].head = "head2"
	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.0.5", SHA: "s105"}, github.Tag{Name: "v2.0.0-rc.1", SHA: "rc"})
	o.Scan(ctx)
	if d.count() != 1 {
		t.Errorf("deployed %s", d.list())
	}

	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.2.0", SHA: "s120"})
	o.Scan(ctx)
	if d.list() != "cove@v1.1.0,cove@v1.2.0" {
		t.Errorf("deployed %s", d.list())
	}

	gh.repos["cove"].tags[len(gh.repos["cove"].tags)-1].SHA = "moved"
	o.Scan(ctx)
	if d.count() != 2 {
		t.Errorf("a moved tag was deployed: %s", d.list())
	}

	if err := o.Deploy(ctx, "cove", "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	o.Scan(ctx)
	if p := get(t, store, "cove"); d.list() != "cove@v1.1.0,cove@v1.2.0,cove@v1.0.0" || p.DeployedVersion != "v1.0.0" {
		t.Errorf("after going back: deployed %s, running %s", d.list(), p.DeployedVersion)
	}

	if err := o.Deploy(ctx, "cove", ""); err != nil || !strings.HasSuffix(d.list(), "cove@v1.2.0") {
		t.Errorf("deploy: %v, deployed %s", err, d.list())
	}
	if err := o.Deploy(ctx, "cove", "v9.9.9"); err == nil || !strings.Contains(err.Error(), `no tag "v9.9.9"`) {
		t.Errorf("deploy of an unknown tag = %v", err)
	}
	if h, _ := store.History(ctx, "cove", 1); h[0].Version != "v1.2.0" {
		t.Errorf("history: %+v", h[0])
	}
}

// statuses is each tracked tag's status, "tag=status" in tag order.
func statuses(t *testing.T, s projects.Store, name string) string {
	t.Helper()
	list, err := s.Releases(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, r := range list {
		out = append(out, r.Tag+"="+r.Status)
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

func TestReleaseTracking(t *testing.T) {
	gh := newGitHub(map[string]string{"cove": "h1"})
	gh.repos["cove"].compose = releases("infra")
	gh.repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "s100"}, {Name: "v1.1.0", SHA: "s110"}, {Name: "v1.2.0-rc.1", SHA: "rc"}, {Name: "nightly", SHA: "h1"}}
	gh.repos["cove"].offBranch = map[string]bool{}
	o, store, d, _ := setup(t, gh, "cove")
	ctx := context.Background()
	now := time.Now()
	// Before tracking: v1.1.0 already runs.
	store.RecordDeployment(ctx, projects.Deployment{Project: "cove", SHA: "s110", Version: "v1.1.0", Trigger: projects.TriggerManual,
		Status: projects.StatusSucceeded, StartedAt: now, FinishedAt: now})

	o.Scan(ctx)
	if got := statuses(t, store, "cove"); d.count() != 0 || got != "v1.0.0=known v1.1.0=known v1.2.0-rc.1=known" || gh.calls["compare"] != 0 {
		t.Fatalf("baseline: %s; deployed %s, %d compares", got, d.list(), gh.calls["compare"])
	}

	// Tagged before main was pushed: it waits, and isn't checked again until main moves.
	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.2.0", SHA: "s120"})
	gh.repos["cove"].offBranch["s120"] = true
	o.Scan(ctx)
	o.Scan(ctx)
	if got := statuses(t, store, "cove"); d.count() != 0 || !strings.Contains(got, "v1.2.0=waiting") || gh.calls["compare"] != 1 {
		t.Fatalf("not on main: %s; deployed %s, %d compares", got, d.list(), gh.calls["compare"])
	}
	if p := get(t, store, "cove"); p.LastError != "" {
		t.Errorf("waiting isn't an error: %q", p.LastError)
	}
	gh.repos["cove"].head, gh.repos["cove"].offBranch["s120"] = "h2", false
	o.Scan(ctx)
	if got := statuses(t, store, "cove"); d.list() != "cove@v1.2.0" || !strings.Contains(got, "v1.2.0=pending") {
		t.Fatalf("on main: %s; deployed %s", got, d.list())
	}

	// Two at once: the newest deploys, the other is skipped.
	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.3.0", SHA: "s130"}, github.Tag{Name: "v1.4.0", SHA: "s140"})
	o.Scan(ctx)
	if got := statuses(t, store, "cove"); d.list() != "cove@v1.2.0,cove@v1.4.0" || !strings.Contains(got, "v1.3.0=superseded v1.4.0=pending") {
		t.Fatalf("two at once: %s; deployed %s", got, d.list())
	}

	// Lower than what's deployed, and a moved tag: neither deploys.
	gh.repos["cove"].tags = append(gh.repos["cove"].tags, github.Tag{Name: "v1.3.5", SHA: "s135"})
	gh.repos["cove"].tags[6].SHA = "elsewhere"
	o.Scan(ctx)
	list, _ := store.Releases(ctx, "cove")
	for _, r := range list {
		switch {
		case r.Tag == "v1.3.5" && (r.Status != projects.ReleaseSuperseded || !strings.Contains(r.Note, "only move forward")),
			r.Tag == "v1.4.0" && (r.Status != projects.ReleaseMoved || r.SHA != "s140" || !strings.Contains(r.Note, "elsewh")):
			t.Errorf("%+v", r)
		}
	}
	if d.count() != 2 {
		t.Errorf("deployed %s", d.list())
	}
}

func TestReleaseNotes(t *testing.T) {
	gh := newGitHub(map[string]string{"cove": "h1"})
	gh.repos["cove"].compose = releases("app")
	gh.repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "s100"}}
	gh.repos["cove"].files = map[string]string{"CHANGELOG.md": "## v1.0.0\n\n### Added\n- Everything.\n"}
	o, _, _, c := setup(t, gh, "cove")
	ctx := context.Background()
	o.Scan(ctx)

	n, err := o.ReleaseNotes(ctx, "cove", "v1.0.0")
	if err != nil || n.Body != "## v1.0.0 (2026-10-06)\n\n### Added\n- Everything.\n" || n.Release.Status != projects.ReleasePending ||
		len(n.Warnings) != 1 || !strings.Contains(n.Warnings[0], "no date") {
		t.Errorf("ReleaseNotes = %+v, %v (today is %s)", n, err, c.now().Format(time.DateOnly))
	}
	if _, err := o.ReleaseNotes(ctx, "cove", "v9.9.9"); !errors.Is(err, projects.ErrNoRelease) {
		t.Errorf("an untracked tag = %v", err)
	}
}

func TestSelfUpdateQueuesItsRelease(t *testing.T) {
	o, store, d, _ := setup(t, newGitHub(map[string]string{"lighthouse": "h"}), "lighthouse")
	ctx := context.Background()
	now := time.Now()
	d.handOffs = []deploy.HandOff{{Project: projects.Project{Name: "lighthouse"}, SHA: "s110", Version: "v1.1.0", Trigger: projects.TriggerCheck,
		Done: true, Status: projects.StatusSucceeded, StartedAt: now, FinishedAt: now}}
	o.RecordHandOffs(ctx)
	if got := statuses(t, store, "lighthouse"); got != "v1.1.0=pending" {
		t.Errorf("after the hand-off is recorded: %s", got)
	}
}

func TestSettingsComeFromTheBranch(t *testing.T) {
	gh := newGitHub(map[string]string{"cove": "head"})
	gh.repos["cove"].compose = releases("infra")
	gh.repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "old"}}
	o, store, d, _ := setup(t, gh, "cove")
	ctx := context.Background()
	d.result = func(req deploy.Request) deploy.Result {
		return deploy.Result{Status: projects.StatusSucceeded, Settings: settings.Default}
	}

	o.Scan(ctx)
	o.Scan(ctx)
	p := get(t, store, "cove")
	if d.list() != "cove@v1.0.0" || p.Mode != "releases" || p.Tier != "infra" {
		t.Errorf("deployed %s; mode %q, tier %q", d.list(), p.Mode, p.Tier)
	}

	o2, store2, d2, _ := setup(t, newGitHub(map[string]string{"cove": "head"}), "cove")
	o2.github.(*fakeGitHub).repos["cove"].compose = releases("infra")
	o2.github.(*fakeGitHub).repos["cove"].tags = []github.Tag{{Name: "v1.0.0", SHA: "old"}}
	if err := o2.Deploy(ctx, "cove", ""); err != nil || d2.list() != "cove@v1.0.0" || get(t, store2, "cove").Mode != "releases" {
		t.Errorf("deploy before any check: %v, deployed %s", err, d2.list())
	}
}

func TestReleaseModeWithoutReleases(t *testing.T) {
	gh := newGitHub(map[string]string{"cove": "head1"})
	gh.repos["cove"].compose = releases("app")
	o, store, d, _ := setup(t, gh, "cove")
	if err := o.Scan(context.Background()); err != nil || d.count() != 0 {
		t.Errorf("Scan = %v, deployed %s", err, d.list())
	}
	if p := get(t, store, "cove"); p.LastError != "" {
		t.Errorf("no releases yet isn't an error: %q", p.LastError)
	}
	if err := o.Deploy(context.Background(), "cove", ""); err == nil || !strings.Contains(err.Error(), "no releases yet") {
		t.Errorf("Deploy = %v", err)
	}
}

func TestSettingsFollowTheBranch(t *testing.T) {
	gh := newGitHub(map[string]string{"site": "aaa"})
	o, store, d, _ := setup(t, gh, "site")
	ctx := context.Background()
	o.Scan(ctx)

	gh.repos["site"].head = "bbb"
	gh.repos["site"].compose = releases("app")
	o.Scan(ctx)
	if p := get(t, store, "site"); p.Mode != "releases" || d.count() != 1 {
		t.Errorf("mode %q, deployed %s", p.Mode, d.list())
	}

	gh.repos["site"].head = "ccc"
	gh.repos["site"].compose = "x-lighthouse:\n  deploy: sometimes\n"
	if err := o.Scan(ctx); err == nil {
		t.Error("a bad setting didn't fail the check")
	}
	if p := get(t, store, "site"); !strings.Contains(p.LastError, "deploy can be branch or releases") {
		t.Errorf("last error %q", p.LastError)
	}
}

func TestOrder(t *testing.T) {
	gh := newGitHub(map[string]string{"app": "a1", "cove": "c1", "sparkdb": "s1"})
	gh.repos["cove"].compose = "x-lighthouse: {tier: infra}\n"
	gh.repos["sparkdb"].compose = "x-lighthouse: {tier: data}\n"
	o, store, d, _ := setup(t, gh, "app", "cove", "sparkdb")
	ctx := context.Background()
	o.Scan(ctx)
	for _, name := range []string{"app", "cove", "sparkdb"} {
		gh.repos[name].head += "-2"
	}
	d.deployed = nil
	o.Scan(ctx)
	if d.list() != "sparkdb@s1-2,cove@c1-2,app@a1-2" {
		t.Errorf("order: %s", d.list())
	}
	if p := get(t, store, "sparkdb"); p.Tier != "data" {
		t.Errorf("sparkdb's tier %q", p.Tier)
	}
}

func TestTurn(t *testing.T) {
	var tr turn
	ctx := context.Background()
	if err := tr.acquire(ctx, 2); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	var wg sync.WaitGroup
	start := func(name string, o int) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tr.acquire(ctx, o)
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			tr.release()
		}()
	}
	start("app", 2)
	waitFor(t, &tr, 1)
	start("data", 0)
	waitFor(t, &tr, 2)

	cctx, cancel := context.WithCancel(ctx)
	errc := make(chan error)
	go func() { errc <- tr.acquire(cctx, 1) }()
	waitFor(t, &tr, 3)
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled wait = %v", err)
	}

	tr.release()
	wg.Wait()
	if strings.Join(order, ",") != "data,app" {
		t.Errorf("order %v", order)
	}
	if tr.busy || len(tr.waiting) != 0 {
		t.Error("the turn wasn't freed")
	}
}

func waitFor(t *testing.T, tr *turn, waiting int) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		tr.mu.Lock()
		n := len(tr.waiting)
		tr.mu.Unlock()
		if n == waiting {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("never %d waiting", waiting)
}

func TestRollbackOnTheBranch(t *testing.T) {
	gh := newGitHub(map[string]string{"site": "old0000ccc"})
	o, store, d, _ := setup(t, gh, "site")
	ctx := context.Background()
	o.Scan(ctx)
	gh.repos["site"].head = "aaa1111bbb"
	o.Scan(ctx)

	back, err := o.Rollback(ctx, "site")
	if err != nil || back != "old0000" || get(t, store, "site").DeployedSHA != "old0000ccc" {
		t.Fatalf("Rollback = %q, %v; deployed %s", back, err, get(t, store, "site").DeployedSHA)
	}
	if p := get(t, store, "site"); p.HeldSHA != "aaa1111bbb" {
		t.Errorf("held %q", p.HeldSHA)
	}
	o.Scan(ctx)
	if d.list() != "site@old0000ccc,site@aaa1111bbb,site@old0000ccc" {
		t.Errorf("the held commit was redeployed: %s", d.list())
	}

	gh.repos["site"].head = "new"
	o.Scan(ctx)
	if !strings.HasSuffix(d.list(), "site@new") {
		t.Errorf("deployed %s", d.list())
	}

	if err := o.Deploy(ctx, "site", "aaa1111"); err != nil || !strings.HasSuffix(d.list(), "site@aaa1111bbb") {
		t.Errorf("deploy of a short SHA: %v, %s", err, d.list())
	}
	if p := get(t, store, "site"); p.HeldSHA != "new" {
		t.Errorf("held %q, want the head it went back from", p.HeldSHA)
	}
	if err := o.Deploy(ctx, "site", ""); err != nil || get(t, store, "site").HeldSHA != "" {
		t.Errorf("deploy <name>: %v, held %q", err, get(t, store, "site").HeldSHA)
	}
	if err := o.Deploy(ctx, "site", "fffffff"); err == nil || !strings.Contains(err.Error(), `no tag or commit "fffffff"`) {
		t.Errorf("an unknown commit = %v", err)
	}
	if err := o.Deploy(ctx, "site", "not-a-tag"); err == nil || !strings.Contains(err.Error(), `no tag "not-a-tag"`) {
		t.Errorf("an unknown tag = %v", err)
	}

	o2, _, _, _ := setup(t, newGitHub(map[string]string{"x": "aaa"}), "x")
	o2.Scan(ctx)
	if _, err := o2.Rollback(ctx, "x"); !errors.Is(err, ErrNothingToRollBackTo) {
		t.Errorf("Rollback with one deploy = %v", err)
	}
}

func TestSelfUpdate(t *testing.T) {
	o, store, d, _ := setup(t, newGitHub(map[string]string{"lighthouse": "new", "app": "a1"}), "app", "lighthouse")
	ctx := context.Background()
	d.result = func(req deploy.Request) deploy.Result {
		if req.Project.Name == "lighthouse" {
			return deploy.Result{HandedOff: true}
		}
		return deploy.Result{Status: projects.StatusSucceeded}
	}

	if err := o.Deploy(ctx, "lighthouse", ""); !errors.Is(err, ErrHandedOff) {
		t.Fatalf("Deploy = %v, want ErrHandedOff", err)
	}
	if h, _ := store.History(ctx, "lighthouse", 10); len(h) != 0 || !o.IsPaused() {
		t.Errorf("history %v, paused %v", h, o.IsPaused())
	}
	cctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := o.Deploy(cctx, "app", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a deploy after the hand-off = %v, want it to wait", err)
	}

	o3, store3, d3, _ := setup(t, newGitHub(map[string]string{"lighthouse": "newer"}), "lighthouse")
	d3.updating = map[string]bool{"lighthouse": true}
	if err := o3.Scan(ctx); err != nil || d3.count() != 0 {
		t.Errorf("Scan during a self-update = %v, deployed %s", err, d3.list())
	}
	if p := get(t, store3, "lighthouse"); p.LastError != "" {
		t.Errorf("last error %q", p.LastError)
	}
	if err := o3.Deploy(ctx, "lighthouse", ""); !errors.Is(err, ErrUpdating) {
		t.Errorf("Deploy during a self-update = %v, want ErrUpdating", err)
	}

	o2, _, d2, _ := setup(t, newGitHub(map[string]string{"lighthouse": "new"}))
	o2.store = store
	now := time.Now()
	d2.handOffs = []deploy.HandOff{{Project: projects.Project{Name: "lighthouse"}, SHA: "new", Trigger: projects.TriggerManual,
		Done: true, Status: projects.StatusSucceeded, StartedAt: now, FinishedAt: now,
		Steps: []projects.Step{{Name: "swap", Status: projects.StepSucceeded}}}}
	o2.RecordHandOffs(ctx)
	if p := get(t, store, "lighthouse"); p.DeployedSHA != "new" || d2.forgot != 1 {
		t.Errorf("after recording: deployed %q, forgot %d", p.DeployedSHA, d2.forgot)
	}
	if dep, _ := store.Deployment(ctx, "lighthouse", 1); dep.Trigger != projects.TriggerManual || len(dep.Steps) != 1 {
		t.Errorf("recorded %+v", dep)
	}
}

func TestHandOffWithoutReplacement(t *testing.T) {
	o, store, d, _ := setup(t, newGitHub(map[string]string{"lighthouse": "new", "app": "a1"}), "app", "lighthouse")
	o.handOffPoll = 5 * time.Millisecond
	ctx := context.Background()
	d.result = func(req deploy.Request) deploy.Result {
		if req.Project.Name == "lighthouse" {
			d.updating = map[string]bool{"lighthouse": true}
			return deploy.Result{HandedOff: true}
		}
		return deploy.Result{Status: projects.StatusSucceeded}
	}
	if err := o.Deploy(ctx, "lighthouse", ""); !errors.Is(err, ErrHandedOff) || !o.IsPaused() {
		t.Fatalf("Deploy = %v, paused %v", err, o.IsPaused())
	}

	now := time.Now()
	d.mu.Lock()
	d.updating = nil
	d.handOffs = []deploy.HandOff{{Project: projects.Project{Name: "lighthouse"}, SHA: "new", Trigger: projects.TriggerManual,
		Done: true, Status: projects.StatusSucceeded, StartedAt: now, FinishedAt: now}}
	d.mu.Unlock()

	for i := 0; i < 200 && (o.IsPaused() || get(t, store, "lighthouse").DeployedSHA != "new"); i++ {
		time.Sleep(5 * time.Millisecond)
	}
	if o.IsPaused() || get(t, store, "lighthouse").DeployedSHA != "new" {
		t.Fatalf("paused %v, deployed %q", o.IsPaused(), get(t, store, "lighthouse").DeployedSHA)
	}
	if err := o.Deploy(ctx, "app", ""); err != nil {
		t.Errorf("a deploy after the hand-off was recorded = %v", err)
	}

	o2, _, d2, _ := setup(t, newGitHub(map[string]string{"lighthouse": "new"}), "lighthouse")
	o2.handOffPoll = 5 * time.Millisecond
	d2.result = func(deploy.Request) deploy.Result { return deploy.Result{HandedOff: true} }
	o2.Pause()
	o2.Deploy(ctx, "lighthouse", "")
	time.Sleep(50 * time.Millisecond)
	if !o2.IsPaused() {
		t.Error("a hand-off undid the operator's pause")
	}
}

func TestStoppedProjectsAreLeftAlone(t *testing.T) {
	gh := newGitHub(map[string]string{"a": "aaa"})
	o, store, d, _ := setup(t, gh, "a")
	ctx := context.Background()
	store.SetStopped(ctx, "a", true)
	o.Scan(ctx)
	if d.count() != 0 {
		t.Errorf("a stopped project was deployed: %s", d.list())
	}
	if err := o.Deploy(ctx, "a", ""); err != nil || get(t, store, "a").Stopped {
		t.Errorf("deploy of a stopped project: %v, stopped %v", err, get(t, store, "a").Stopped)
	}
}

// fakeContainers answers the reconcile loop.
type fakeContainers struct {
	byProject map[string][]docker.Container
	policy    string
}

func (f *fakeContainers) ProjectContainers(ctx context.Context, project string) ([]docker.Container, error) {
	return f.byProject[project], nil
}
func (f *fakeContainers) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	return docker.Detail{RestartPolicy: f.policy}, nil
}

func TestReconcile(t *testing.T) {
	gh := newGitHub(map[string]string{"app": "a1", "sparkdb": "s1", "job": "j1", "idle": "i1"})
	gh.repos["sparkdb"].compose = "x-lighthouse: {tier: data}\n"
	o, store, d, clk := setup(t, gh, "app", "idle", "job", "sparkdb")
	ctx := context.Background()
	o.Scan(ctx)
	store.SetStopped(ctx, "idle", true)
	d.deployed = nil

	cs := &fakeContainers{policy: "unless-stopped", byProject: map[string][]docker.Container{
		"app":     {{ID: "a", Service: "web", State: "running"}},
		"sparkdb": nil,
		"idle":    nil,
		"job":     {{ID: "j", Service: "migrate", State: "exited"}},
	}}
	o.containers = &policyByID{cs, map[string]string{"j": "no"}}

	if err := o.Reconcile(ctx); err != nil || d.count() != 0 {
		t.Fatalf("first pass: %v, deployed %s", err, d.list())
	}
	clk.advance(ReconcileEvery)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if d.list() != "sparkdb@s1" {
		t.Errorf("deployed %s, want only sparkdb (app runs, idle is stopped, job is a finished one-off)", d.list())
	}
	if h, _ := store.History(ctx, "sparkdb", 1); h[0].Trigger != projects.TriggerReconcile {
		t.Errorf("trigger %q", h[0].Trigger)
	}

	cs.byProject["sparkdb"] = []docker.Container{{ID: "s", Service: "db", State: "running"}}
	cs.byProject["app"] = []docker.Container{{ID: "a", Service: "web", State: "exited"}}
	o.Reconcile(ctx)
	clk.advance(ReconcileEvery)
	o.Reconcile(ctx)
	if d.list() != "sparkdb@s1,app@a1" {
		t.Errorf("deployed %s", d.list())
	}

	cs.byProject["app"] = []docker.Container{{ID: "a", Service: "web", State: "exited"}}
	o.Reconcile(ctx)
	cs.byProject["app"] = []docker.Container{{ID: "a", Service: "web", State: "running"}}
	clk.advance(ReconcileEvery)
	o.Reconcile(ctx)
	if d.count() != 2 {
		t.Errorf("a passing stop was reconciled: %s", d.list())
	}
}

// policyByID gives some containers another restart policy.
type policyByID struct {
	*fakeContainers
	policies map[string]string
}

func (p *policyByID) Inspect(ctx context.Context, id string) (docker.Detail, error) {
	if pol, ok := p.policies[id]; ok {
		return docker.Detail{RestartPolicy: pol}, nil
	}
	return p.fakeContainers.Inspect(ctx, id)
}

func TestComposeProjectConflict(t *testing.T) {
	store := &projectstest.Store{}
	ctx := context.Background()
	store.Add(ctx, "first", github.Repo{Owner: "x", Name: "site"})
	store.Add(ctx, "second", github.Repo{Owner: "y", Name: "site"})
	d := &fakeDeployer{}
	o := New(store, newGitHub(map[string]string{"site": "aaa"}), d, nil, "t")

	if err := o.Deploy(ctx, "first", ""); err != nil {
		t.Fatal(err)
	}
	err := o.Deploy(ctx, "second", "")
	if err == nil || !strings.Contains(err.Error(), `"site" belongs to first already`) {
		t.Errorf("second deploy = %v, want it to name first", err)
	}
}

func TestCheck(t *testing.T) {
	store := &projectstest.Store{}
	ctx := context.Background()
	store.Add(ctx, "first", github.Repo{Owner: "x", Name: "site"})
	store.Add(ctx, "second", github.Repo{Owner: "y", Name: "site"})
	d := &fakeDeployer{}
	o := New(store, newGitHub(map[string]string{"site": "aaa"}), d, nil, "t")

	res, sha, err := o.Check(ctx, "first")
	if err != nil || sha != "aaa" || res.Status != projects.StatusSucceeded {
		t.Fatalf("Check = %+v, %q, %v", res, sha, err)
	}
	if len(d.deployed) != 0 || get(t, store, "first").ComposeProject != "" {
		t.Errorf("Check deployed %v or claimed %q", d.deployed, get(t, store, "first").ComposeProject)
	}
	if h, _ := store.History(ctx, "first", 10); len(h) != 0 {
		t.Errorf("Check was recorded: %+v", h)
	}

	if err := o.Deploy(ctx, "first", ""); err != nil {
		t.Fatal(err)
	}
	if res, _, _ := o.Check(ctx, "second"); res.Err == nil || !strings.Contains(res.Err.Error(), "belongs to first") {
		t.Errorf("Check of a clashing project = %v", res.Err)
	}
	if res, _, _ := o.Check(ctx, "first"); res.Err != nil {
		t.Errorf("Check of the project that has it = %v", res.Err)
	}
}

func TestNoDeployWithoutToken(t *testing.T) {
	store := &projectstest.Store{}
	store.Add(context.Background(), "a", github.Repo{Owner: "o", Name: "a"})
	d := &fakeDeployer{}
	o := New(store, newGitHub(map[string]string{"a": "aaa"}), d, nil, "")

	if err := o.Scan(context.Background()); err == nil {
		t.Error("Scan without a GitHub token succeeded")
	}
	if d.count() != 0 {
		t.Error("deployed without a GitHub token")
	}
}

func TestOneScanAtATime(t *testing.T) {
	o, _, _, _ := setup(t, newGitHub(nil))
	o.scanning.Lock()
	defer o.scanning.Unlock()
	if err := o.Scan(context.Background()); !errors.Is(err, ErrScanRunning) {
		t.Errorf("Scan during a scan = %v, want ErrScanRunning", err)
	}
}

// Removing a project while it deploys must not fail the scan or touch
// another project.
func TestRemoveDuringScan(t *testing.T) {
	o, store, d, _ := setup(t, newGitHub(map[string]string{"a": "aaa", "b": "bbb"}), "a", "b")
	d.result = func(req deploy.Request) deploy.Result {
		if req.Project.Name == "a" {
			store.Remove(context.Background(), "a")
		}
		return deploy.Result{Status: projects.StatusSucceeded}
	}

	if err := o.Scan(context.Background()); err != nil {
		t.Errorf("Scan = %v", err)
	}
	list, _ := store.List(context.Background())
	if len(list) != 1 || list[0].Name != "b" || list[0].DeployedSHA != "bbb" {
		t.Fatalf("projects = %+v, want only b at bbb", list)
	}
	if !slices.Contains(d.deployed, "b@bbb") {
		t.Errorf("deployed %v", d.deployed)
	}
}
