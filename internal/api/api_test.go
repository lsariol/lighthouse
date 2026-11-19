package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lsariol/lighthouse/internal/projects"
)

const vistaToken = "vista-token-0123456789abcdef0123456789"

var now = time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)

// fakeReader answers from its fields and remembers the last deployment query.
type fakeReader struct {
	projects    []projects.Project
	deployments []Deployment // newest first
	details     map[int64]DeploymentDetail
	releases    map[string][]Release
	err         error
	last        DeploymentQuery
}

func (f *fakeReader) List(ctx context.Context) ([]projects.Project, error) { return f.projects, f.err }

func (f *fakeReader) RecentDeployments(ctx context.Context, q DeploymentQuery) ([]Deployment, error) {
	f.last = q
	var out []Deployment
	for _, d := range f.deployments {
		if d.StartedAt.Before(q.From) || d.StartedAt.After(q.To) || (q.Project != "" && d.Project != q.Project) {
			continue
		}
		if q.After != nil && !(d.StartedAt.Before(q.After.StartedAt) || (d.StartedAt.Equal(q.After.StartedAt) && d.ID < q.After.ID)) {
			continue
		}
		out = append(out, d)
		if len(out) == q.Limit {
			break
		}
	}
	return out, f.err
}

func (f *fakeReader) DeploymentByID(ctx context.Context, id int64) (DeploymentDetail, error) {
	d, ok := f.details[id]
	if !ok {
		return DeploymentDetail{}, projects.ErrNoDeployment
	}
	return d, f.err
}

func (f *fakeReader) RecentReleases(ctx context.Context, project string, limit int) ([]Release, error) {
	if project == "" {
		var all []Release
		for _, rs := range f.releases {
			all = append(all, rs...)
		}
		return all, f.err
	}
	rs, ok := f.releases[project]
	if !ok {
		return nil, projects.ErrNotFound
	}
	return rs, f.err
}

func (f *fakeReader) CheckSchemaVersion(ctx context.Context) error { return nil }
func (f *fakeReader) Ping(ctx context.Context) error               { return f.err }

func getenv(key string) string {
	if key == "SHARED_LIGHTHOUSE_VISTA_TOKEN" {
		return vistaToken
	}
	return ""
}

// newServer returns a server already connected to db, or not connected when db is nil.
func newServer(t *testing.T, db Reader) *Server {
	t.Helper()
	s, err := New(Options{Version: "v1.6.0", Getenv: getenv, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if db != nil {
		s.db, s.dbErr = db, nil
	}
	return s
}

// get requests path with token ("" for none) and returns the status and the decoded body.
func get(t *testing.T, s *Server, path string, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: not JSON: %q", path, rec.Body.String())
	}
	return rec.Code, body
}

func TestNewRefusesAMissingOrShortToken(t *testing.T) {
	for _, token := range []string{"", "short"} {
		_, err := New(Options{Getenv: func(string) string { return token }})
		if err == nil || !strings.Contains(err.Error(), "SHARED_LIGHTHOUSE_VISTA_TOKEN") {
			t.Errorf("token %q: New = %v", token, err)
		}
	}
}

func TestHealthNeedsNoTokenAndSaysOnlyWhetherItWorks(t *testing.T) {
	code, body := get(t, newServer(t, &fakeReader{}), "/healthz", "")
	if code != http.StatusOK || body["status"] != "ok" || body["version"] != "v1.6.0" || len(body) != 2 {
		t.Errorf("healthz = %d %v", code, body)
	}
	code, body = get(t, newServer(t, nil), "/healthz", "")
	if code != http.StatusServiceUnavailable || body["status"] != "unavailable" {
		t.Errorf("healthz without a database = %d %v", code, body)
	}
}

func TestEveryEndpointNeedsAToken(t *testing.T) {
	s := newServer(t, &fakeReader{})
	for _, path := range []string{"/v1/projects", "/v1/deployments", "/v1/deployments/1", "/v1/releases"} {
		for _, token := range []string{"", "not-the-token"} {
			if code, body := get(t, s, path, token); code != http.StatusUnauthorized || body["error"] == nil {
				t.Errorf("%s with token %q = %d %v", path, token, code, body)
			}
		}
	}
}

func TestProjectsAndTheirStates(t *testing.T) {
	deployedAt := time.Date(2026, 11, 19, 7, 0, 0, 0, time.FixedZone("EST", -5*3600))
	db := &fakeReader{projects: []projects.Project{
		{Name: "lys", ComposeProject: "lys", Kind: projects.KindDeploy, Mode: "releases", DeployedSHA: "abc", DeployedVersion: "v0.6.0", DeployedAt: &deployedAt},
		{Name: "dibs", ComposeProject: "dibs", Kind: projects.KindDeploy, Mode: "releases", DeployedSHA: "abc", FailureCount: 2, LastError: "test: failed"},
		{Name: "plop", Kind: projects.KindDeploy, Mode: "branch", DeployedSHA: "abc", FailureCount: 3, Broken: true},
		{Name: "marquee", Kind: projects.KindDeploy, Mode: "releases", DeployedSHA: "abc", Stopped: true},
		{Name: "vista", Kind: projects.KindDeploy, Mode: "releases"},
		{Name: "coveclient", Kind: projects.KindPublish, Mode: "branch"},
	}}
	code, body := get(t, newServer(t, db), "/v1/projects", vistaToken)
	if code != http.StatusOK || body["generated_at"] != "2026-11-19T12:00:00Z" {
		t.Fatalf("projects = %d %v", code, body)
	}
	list := body["projects"].([]any)
	want := []string{"ok", "failing", "broken", "stopped", "not_deployed", "ok"}
	for i, p := range list {
		if got := p.(map[string]any)["state"]; got != want[i] {
			t.Errorf("project %d state = %v, want %s", i, got, want[i])
		}
	}
	lys := list[0].(map[string]any)
	if lys["deployed_at"] != "2026-11-19T12:00:00Z" || lys["deployed_version"] != "v0.6.0" || lys["last_error"] != nil || len(lys) != 12 {
		t.Errorf("lys = %v (want UTC times, nulls for empty fields, and exactly the 12 fields)", lys)
	}
	if vista := list[4].(map[string]any); vista["compose_project"] != nil || vista["deployed_sha"] != nil {
		t.Errorf("a project never deployed = %v, want null compose_project and deployed_sha", vista)
	}
}

func deploymentsAt(n int) []Deployment {
	var out []Deployment
	for i := n; i >= 1; i-- {
		out = append(out, Deployment{ID: int64(i), Project: "lys", Trigger: "check", Status: "succeeded",
			StartedAt: now.Add(-time.Duration(n-i+1) * time.Hour), FinishedAt: now.Add(-time.Duration(n-i+1)*time.Hour + time.Minute)})
	}
	return out
}

func TestDeploymentsPageThroughWithTheCursor(t *testing.T) {
	db := &fakeReader{deployments: deploymentsAt(5)}
	s := newServer(t, db)
	var ids []float64
	path := "/v1/deployments?limit=2"
	for pages := 0; pages < 5; pages++ {
		code, body := get(t, s, path, vistaToken)
		if code != http.StatusOK {
			t.Fatalf("%s = %d %v", path, code, body)
		}
		for _, d := range body["deployments"].([]any) {
			ids = append(ids, d.(map[string]any)["id"].(float64))
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		path = "/v1/deployments?limit=2&cursor=" + next
	}
	if len(ids) != 5 || ids[0] != 5 || ids[4] != 1 {
		t.Errorf("paged ids = %v, want 5 down to 1 once each", ids)
	}
	if !db.last.To.Equal(now) || !db.last.From.Equal(now.Add(-defaultWindow)) {
		t.Errorf("default range = %s to %s", db.last.From, db.last.To)
	}
}

func TestDeploymentsEmptyIsAList(t *testing.T) {
	code, body := get(t, newServer(t, &fakeReader{}), "/v1/deployments?project=nobody", vistaToken)
	if list, ok := body["deployments"].([]any); code != http.StatusOK || !ok || len(list) != 0 || body["next_cursor"] != nil {
		t.Errorf("no deployments = %d %v", code, body)
	}
}

func TestBadParametersSayWhich(t *testing.T) {
	s := newServer(t, &fakeReader{})
	for path, word := range map[string]string{
		"/v1/deployments?limit=0":                                           "limit",
		"/v1/deployments?limit=501":                                         "limit",
		"/v1/deployments?from=yesterday":                                    "from",
		"/v1/deployments?from=2026-11-19T00:00:00Z&to=2026-11-18T00:00:00Z": "from must not be after to",
		"/v1/deployments?cursor=nope":                                       "cursor",
		"/v1/deployments?projcet=lys":                                       "projcet",
		"/v1/deployments/abc":                                               "id",
		"/v1/releases?limit=x":                                              "limit",
		"/v1/projects?all=1":                                                "all",
	} {
		code, body := get(t, s, path, vistaToken)
		if msg, _ := body["error"].(string); code != http.StatusBadRequest || !strings.Contains(msg, word) {
			t.Errorf("%s = %d %v, want a 400 naming %q", path, code, body, word)
		}
	}
}

func TestOneDeploymentWithItsSteps(t *testing.T) {
	log := "built\n"
	db := &fakeReader{details: map[int64]DeploymentDetail{
		7: {Deployment: deploymentsAt(1)[0], Steps: []Step{{Step: "build", Status: "succeeded", Log: &log}}},
	}}
	s := newServer(t, db)
	code, body := get(t, s, "/v1/deployments/7", vistaToken)
	steps, _ := body["steps"].([]any)
	if code != http.StatusOK || len(steps) != 1 || steps[0].(map[string]any)["log"] != "built\n" || body["project"] != "lys" {
		t.Errorf("deployment 7 = %d %v", code, body)
	}
	if code, body := get(t, s, "/v1/deployments/8", vistaToken); code != http.StatusNotFound || body["error"] != "no deployment 8" {
		t.Errorf("deployment 8 = %d %v", code, body)
	}
}

func TestReleasesOfAnUnknownProjectAre404(t *testing.T) {
	db := &fakeReader{releases: map[string][]Release{"lys": {{Project: "lys", Tag: "v0.6.0", SHA: "abc", Status: "published", CreatedAt: now}}}}
	s := newServer(t, db)
	code, body := get(t, s, "/v1/releases?project=lys", vistaToken)
	if list, _ := body["releases"].([]any); code != http.StatusOK || len(list) != 1 {
		t.Errorf("lys's releases = %d %v", code, body)
	}
	if code, body := get(t, s, "/v1/releases?project=nobody", vistaToken); code != http.StatusNotFound {
		t.Errorf("nobody's releases = %d %v", code, body)
	}
}

func TestDatabaseTroubleIs503(t *testing.T) {
	if code, body := get(t, newServer(t, nil), "/v1/projects", vistaToken); code != http.StatusServiceUnavailable || body["error"] == nil {
		t.Errorf("before the database = %d %v", code, body)
	}
	if code, body := get(t, newServer(t, &fakeReader{err: errors.New("connection reset")}), "/v1/projects", vistaToken); code != http.StatusServiceUnavailable {
		t.Errorf("a failing query = %d %v", code, body)
	}
}

func TestUnknownPathsAndMethods(t *testing.T) {
	s := newServer(t, &fakeReader{})
	if code, _ := get(t, s, "/v2/projects", vistaToken); code != http.StatusNotFound {
		t.Errorf("/v2/projects = %d", code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/deployments/3", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /v1/deployments/3 = %d", rec.Code)
	}
}

func TestCursorRoundTrips(t *testing.T) {
	c := Cursor{StartedAt: time.Date(2026, 11, 19, 1, 2, 3, 456789000, time.UTC), ID: 42}
	got, err := parseCursor(formatCursor(c))
	if err != nil || !got.StartedAt.Equal(c.StartedAt) || got.ID != 42 {
		t.Errorf("round trip = %+v, %v", got, err)
	}
}
