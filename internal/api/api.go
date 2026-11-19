// Package api is lighthouse-api: read-only answers about projects, deploys and releases, for consumers on spark.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lsariol/lighthouse/internal/projects"
)

const (
	// Port is where the API listens, inside its container on spark.
	Port = 4000

	queryTimeout   = 3 * time.Second
	requestTimeout = 5 * time.Second
	connectRetry   = 5 * time.Second

	defaultLimit  = 100
	maxLimit      = 500
	defaultWindow = 30 * 24 * time.Hour // deployments' range when from isn't given
)

// paths are the endpoints the API serves, for its 404 and 405 answers.
var paths = []string{"/healthz", "/v1/projects", "/v1/deployments", "/v1/deployments/{id}", "/v1/releases"}

// Options are the server's parts; tests swap them for fakes.
type Options struct {
	Version string
	Getenv  func(string) string // where tokens are read from
	Log     *slog.Logger
	Now     func() time.Time // time.Now unless set

	// OpenReader connects to the database; the server retries until it works.
	OpenReader func(ctx context.Context) (Reader, error)
}

// Server answers the API.
type Server struct {
	opts  Options
	creds []credential

	mu    sync.Mutex
	db    Reader
	dbErr error

	refused refusals
}

// New returns a server, or an error when a consumer's token is missing.
func New(opts Options) (*Server, error) {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	creds, err := loadCredentials(opts.Getenv, consumers)
	if err != nil {
		return nil, err
	}
	return &Server{opts: opts, creds: creds, dbErr: errors.New("not connected yet")}, nil
}

// Handler routes the API.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /v1/projects", s.require(s.projects))
	mux.HandleFunc("GET /v1/deployments", s.require(s.deployments))
	mux.HandleFunc("GET /v1/deployments/{id}", s.require(s.deployment))
	mux.HandleFunc("GET /v1/releases", s.require(s.releases))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if known(r.URL.Path) {
			w.Header().Set("Allow", "GET, HEAD")
			writeError(w, http.StatusMethodNotAllowed, "only GET is supported")
			return
		}
		writeError(w, http.StatusNotFound, "no such endpoint: the API serves "+strings.Join(paths, ", "))
	})
	return http.TimeoutHandler(securityHeaders(mux), requestTimeout, `{"error":"the request took too long"}`)
}

// known reports whether path is one of the API's endpoints.
func known(path string) bool {
	if rest, ok := strings.CutPrefix(path, "/v1/deployments/"); ok && rest != "" && !strings.Contains(rest, "/") {
		return true
	}
	return slices.Contains(paths, path)
}

// Serve listens on addr until ctx ends, connecting to the database in the background.
func (s *Server) Serve(ctx context.Context, addr string) error {
	go s.connect(ctx)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("api: listening on %s: %w", addr, err)
	}
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			s.opts.Log.Warn("API shutdown cut short", "err", err)
		}
	}()
	s.opts.Log.Info("API listening", "addr", addr, "consumers", len(s.creds))
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("api: %w", err)
	}
	return nil
}

// connect opens the database, retrying until it has the schema this build needs.
func (s *Server) connect(ctx context.Context) {
	for {
		db, err := s.opts.OpenReader(ctx)
		if err == nil {
			cctx, cancel := context.WithTimeout(ctx, queryTimeout)
			err = db.CheckSchemaVersion(cctx)
			cancel()
		}
		s.mu.Lock()
		if err == nil {
			s.db, s.dbErr = db, nil
		} else if s.dbErr == nil || s.dbErr.Error() != err.Error() {
			s.opts.Log.Warn("waiting for the database", "err", err)
		}
		if err != nil {
			s.dbErr = err
		}
		s.mu.Unlock()
		if err == nil {
			s.opts.Log.Info("database ready")
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(connectRetry):
		}
	}
}

func (s *Server) reader() (Reader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil, s.dbErr
	}
	return s.db, nil
}

// healthz is unauthenticated, so it says only whether the API works.
func (s *Server) healthz(w http.ResponseWriter, r *http.Request) {
	db, err := s.reader()
	if err == nil {
		err = db.Ping(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable", "version": s.opts.Version})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.opts.Version})
}

// require lets a request through only with a consumer's token.
func (s *Server) require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := bearer(r.Header.Get("Authorization"))
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="lighthouse"`)
			writeError(w, http.StatusUnauthorized, "a token is required: Authorization: Bearer <token>")
			return
		}
		if _, ok := identify(s.creds, token); !ok {
			s.refused.note(s.opts.Log, s.opts.Now(), r)
			w.Header().Set("WWW-Authenticate", `Bearer realm="lighthouse", error="invalid_token"`)
			writeError(w, http.StatusUnauthorized, "unknown token")
			return
		}
		next(w, r)
	}
}

// refusals logs refused requests at most once a minute, so a flood can't flood the log.
type refusals struct {
	mu      sync.Mutex
	logged  time.Time
	pending int
}

func (f *refusals) note(log *slog.Logger, now time.Time, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending++
	if now.Sub(f.logged) < time.Minute {
		return
	}
	log.Warn("refused requests", "count", f.pending, "since", f.logged.UTC().Format(time.RFC3339),
		"latest", "an unknown token", "path", r.URL.Path, "from", r.RemoteAddr)
	f.logged, f.pending = now, 0
}

func bearer(header string) (string, bool) {
	scheme, token, ok := strings.Cut(strings.TrimSpace(header), " ")
	token = strings.TrimSpace(token)
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return "", false
	}
	return token, true
}

// projects answers GET /v1/projects: every project and its state.
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	if !onlyParams(w, r) {
		return
	}
	s.answer(w, r, func(ctx context.Context, db Reader) (int, any, error) {
		list, err := db.List(ctx)
		if err != nil {
			return 0, nil, err
		}
		out := make([]Project, 0, len(list))
		for _, p := range list {
			out = append(out, projectAnswer(p))
		}
		return http.StatusOK, struct {
			GeneratedAt time.Time `json:"generated_at"`
			Projects    []Project `json:"projects"`
		}{s.opts.Now().UTC(), out}, nil
	})
}

// deployments answers GET /v1/deployments, newest first, a page at a time.
func (s *Server) deployments(w http.ResponseWriter, r *http.Request) {
	if !onlyParams(w, r, "from", "to", "project", "limit", "cursor") {
		return
	}
	q := r.URL.Query()
	to, ok := timeParam(w, q, "to", s.opts.Now())
	if !ok {
		return
	}
	from, ok := timeParam(w, q, "from", to.Add(-defaultWindow))
	if !ok {
		return
	}
	if from.After(to) {
		writeError(w, http.StatusBadRequest, "from must not be after to")
		return
	}
	limit, ok := limitParam(w, q)
	if !ok {
		return
	}
	var after *Cursor
	if raw := q.Get("cursor"); raw != "" {
		c, err := parseCursor(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, "cursor isn't one this API gave: use next_cursor from the previous page")
			return
		}
		after = &c
	}
	query := DeploymentQuery{From: from, To: to, Project: q.Get("project"), Limit: limit + 1, After: after}
	s.answer(w, r, func(ctx context.Context, db Reader) (int, any, error) {
		list, err := db.RecentDeployments(ctx, query)
		if err != nil {
			return 0, nil, err
		}
		var next *string
		if len(list) > limit {
			list = list[:limit]
			last := list[len(list)-1]
			c := formatCursor(Cursor{StartedAt: last.StartedAt, ID: last.ID})
			next = &c
		}
		if list == nil {
			list = []Deployment{}
		}
		return http.StatusOK, struct {
			Deployments []Deployment `json:"deployments"`
			NextCursor  *string      `json:"next_cursor"`
		}{list, next}, nil
	})
}

// deployment answers GET /v1/deployments/{id}: one deploy with its steps.
func (s *Server) deployment(w http.ResponseWriter, r *http.Request) {
	if !onlyParams(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "id must be a deployment's number, as in /v1/deployments")
		return
	}
	s.answer(w, r, func(ctx context.Context, db Reader) (int, any, error) {
		d, err := db.DeploymentByID(ctx, id)
		if errors.Is(err, projects.ErrNoDeployment) {
			return http.StatusNotFound, errorBody(fmt.Sprintf("no deployment %d", id)), nil
		}
		if err != nil {
			return 0, nil, err
		}
		if d.Steps == nil {
			d.Steps = []Step{}
		}
		return http.StatusOK, d, nil
	})
}

// releases answers GET /v1/releases: the newest version tags, of one project or every one.
func (s *Server) releases(w http.ResponseWriter, r *http.Request) {
	if !onlyParams(w, r, "project", "limit") {
		return
	}
	q := r.URL.Query()
	limit, ok := limitParam(w, q)
	if !ok {
		return
	}
	project := q.Get("project")
	s.answer(w, r, func(ctx context.Context, db Reader) (int, any, error) {
		list, err := db.RecentReleases(ctx, project, limit)
		if errors.Is(err, projects.ErrNotFound) {
			return http.StatusNotFound, errorBody(fmt.Sprintf("no project %q", project)), nil
		}
		if err != nil {
			return 0, nil, err
		}
		if list == nil {
			list = []Release{}
		}
		return http.StatusOK, struct {
			Releases []Release `json:"releases"`
		}{list}, nil
	})
}

// answer runs build against the database and writes its answer, or a 503 when the database can't answer.
func (s *Server) answer(w http.ResponseWriter, r *http.Request, build func(ctx context.Context, db Reader) (int, any, error)) {
	db, err := s.reader()
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "the database isn't ready yet; try again shortly")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), queryTimeout)
	defer cancel()
	status, body, err := build(ctx, db)
	if err != nil {
		s.opts.Log.Error("building an answer failed", "path", r.URL.Path, "err", err)
		writeError(w, http.StatusServiceUnavailable, "the database didn't answer; try again shortly")
		return
	}
	writeJSON(w, status, body)
}

// onlyParams answers 400 for a parameter the endpoint doesn't take, so a typo isn't silently ignored.
func onlyParams(w http.ResponseWriter, r *http.Request, allowed ...string) bool {
	for name := range r.URL.Query() {
		if !slices.Contains(allowed, name) {
			if len(allowed) == 0 {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown parameter %q: this endpoint takes none", name))
			} else {
				writeError(w, http.StatusBadRequest, fmt.Sprintf("unknown parameter %q: this endpoint takes %s", name, strings.Join(allowed, ", ")))
			}
			return false
		}
	}
	return true
}

// timeParam is the RFC 3339 time in parameter name, or def when it's missing.
func timeParam(w http.ResponseWriter, q url.Values, name string, def time.Time) (time.Time, bool) {
	raw := q.Get(name)
	if raw == "" {
		return def, true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("%s must be an RFC 3339 time, such as 2026-11-19T12:00:00Z", name))
		return time.Time{}, false
	}
	return t, true
}

// limitParam is the limit parameter, defaultLimit when missing.
func limitParam(w http.ResponseWriter, q url.Values) (int, bool) {
	raw := q.Get("limit")
	if raw == "" {
		return defaultLimit, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > maxLimit {
		writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be a number from 1 to %d", maxLimit))
		return 0, false
	}
	return n, true
}

// formatCursor is c as next_cursor: the start in Unix microseconds and the ID.
func formatCursor(c Cursor) string {
	return fmt.Sprintf("%d.%d", c.StartedAt.UnixMicro(), c.ID)
}

func parseCursor(raw string) (Cursor, error) {
	us, id, ok := strings.Cut(raw, ".")
	if !ok {
		return Cursor{}, errors.New("no separator")
	}
	micros, err := strconv.ParseInt(us, 10, 64)
	if err != nil {
		return Cursor{}, err
	}
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil || n < 1 {
		return Cursor{}, errors.New("bad id")
	}
	return Cursor{StartedAt: time.UnixMicro(micros).UTC(), ID: n}, nil
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Debug("writing an answer failed", "err", err)
	}
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorBody(msg))
}
