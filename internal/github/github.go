// Package github is what Lighthouse needs from GitHub: parsing repository
// URLs, finding a repository's latest commit, and downloading a commit.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// ErrInvalidURL means a string isn't a GitHub repository URL.
var ErrInvalidURL = errors.New("invalid GitHub repository URL")

// Repo is a repository on GitHub.
type Repo struct {
	Owner string
	Name  string
}

func (r Repo) URL() string    { return "https://github.com/" + r.Owner + "/" + r.Name }
func (r Repo) String() string { return r.Owner + "/" + r.Name }

var namePart = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// ParseRepoURL accepts https://github.com/owner/repo in its common forms.
func ParseRepoURL(raw string) (Repo, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimPrefix(s, "http://")
	s = strings.TrimPrefix(s, "www.")
	if !strings.HasPrefix(strings.ToLower(s), "github.com/") {
		return Repo{}, ErrInvalidURL
	}
	s = s[len("github.com/"):]
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")

	parts := strings.Split(s, "/")
	if len(parts) != 2 || !namePart.MatchString(parts[0]) || !namePart.MatchString(parts[1]) {
		return Repo{}, ErrInvalidURL
	}
	return Repo{Owner: parts[0], Name: parts[1]}, nil
}

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// Client calls the GitHub REST API. The zero value uses http.DefaultClient
// and DefaultAPI; set HTTP for timeouts.
type Client struct {
	HTTP *http.Client
	API  string

	Token string // the token's name in errors; "" for Lighthouse's own (LIGHTHOUSE_GITHUB_TOKEN)
	Needs string // the permissions it needs, in errors; "" for read-only
}

func (c Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c Client) url(repo Repo, path string) string {
	return c.apiURL("/repos/" + repo.Owner + "/" + repo.Name + path)
}

func (c Client) apiURL(path string) string {
	base := c.API
	if base == "" {
		base = DefaultAPI
	}
	return strings.TrimRight(base, "/") + path
}

// Error is a failed GitHub request; Temporary says whether retrying may help.
type Error struct {
	Op     string
	Status int
	Err    error
}

func (e *Error) Error() string {
	if e.Op == "" {
		return "GitHub: " + e.Err.Error()
	}
	return "GitHub: " + e.Op + ": " + e.Err.Error()
}
func (e *Error) Unwrap() error { return e.Err }

func (e *Error) Temporary() bool {
	return e.Status == 0 || e.Status == http.StatusTooManyRequests || e.Status >= 500 ||
		// GitHub answers 403 when a rate limit is exhausted.
		e.Status == http.StatusForbidden && strings.Contains(e.Err.Error(), "rate limit")
}

// ErrNotModified means GitHub's answer hasn't changed since the ETag given.
// Such a request doesn't count against the rate limit.
var ErrNotModified = errors.New("not modified")

// get sends an authenticated GET and returns the response if it's 200 OK.
func (c Client) get(ctx context.Context, op string, url string, token string, accept string, etag string) (*http.Response, error) {
	return c.send(ctx, http.MethodGet, op, url, token, accept, etag, nil, http.StatusOK)
}

// send sends an authenticated request, with body as JSON unless it's nil,
// and returns the response if its status is one of ok.
func (c Client) send(ctx context.Context, method string, op string, url string, token string, accept string, etag string, body any, ok ...int) (*http.Response, error) {
	if token == "" {
		return nil, &Error{Op: op, Err: errors.New("no GitHub token yet (Lighthouse is still waiting for Cove)")}
	}
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, &Error{Op: op, Err: err}
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, &Error{Op: op, Err: err}
	}
	if resp.StatusCode == http.StatusNotModified && etag != "" {
		resp.Body.Close()
		return nil, ErrNotModified
	}
	if !slices.Contains(ok, resp.StatusCode) {
		defer resp.Body.Close()
		var answer struct {
			Message string
			Errors  []struct{ Code string }
		}
		json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&answer)
		msg := resp.Status
		if answer.Message != "" {
			msg += " (" + answer.Message + ")"
		}
		for _, e := range answer.Errors {
			if e.Code != "" {
				msg += " [" + e.Code + "]"
			}
		}
		return nil, &Error{Op: op, Status: resp.StatusCode, Err: errors.New(msg + c.hint(resp.StatusCode, answer.Message))}
	}
	return resp, nil
}

// call sends a request and decodes a JSON answer into out (if not nil).
func (c Client) call(ctx context.Context, method string, op string, url string, token string, body any, out any, ok ...int) error {
	resp, err := c.send(ctx, method, op, url, token, "application/vnd.github+json", "", body, ok...)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out); err != nil {
		return &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("unexpected answer: %v", err)}
	}
	return nil
}

// hint says what usually causes a status.
func (c Client) hint(status int, message string) string {
	token, needs, missing := c.Token, c.Needs, "the App must be installed on the repository"
	if token == "" {
		token, missing = "Lighthouse's GitHub token (LIGHTHOUSE_GITHUB_TOKEN in Cove)", "a fine-grained token must list the repository"
	}
	if needs == "" {
		needs = "Contents: read and Metadata: read"
	}
	switch {
	case status == http.StatusUnauthorized:
		return ". " + token + " was rejected: it may have expired or been revoked"
	case status == http.StatusNotFound && c.Token == "":
		return ". The repository, commit or file doesn't exist, or Lighthouse's token can't read it (" + missing + ")"
	case status == http.StatusNotFound:
		return ". The repository, commit or file doesn't exist, or " + token + " can't reach it (" + missing + ")"
	case status == http.StatusTooManyRequests, status == http.StatusForbidden && strings.Contains(message, "rate limit"):
		return ". GitHub's rate limit: it's tried again after a wait"
	case status == http.StatusForbidden:
		return ". " + token + " lacks a permission (it needs " + needs + ")"
	}
	return ""
}

// ErrEmptyRepository means a repository has no commits yet.
var ErrEmptyRepository = errors.New("the repository has no commits yet")

// LatestCommit returns the newest commit on the repository's default branch.
func (c Client) LatestCommit(ctx context.Context, repo Repo, token string) (string, error) {
	sha, _, err := c.CheckCommit(ctx, repo, token, "")
	return sha, err
}

// CheckCommit is LatestCommit for polling: given the ETag of the last
// answer, it returns ErrNotModified if nothing changed, and the new ETag.
func (c Client) CheckCommit(ctx context.Context, repo Repo, token string, etag string) (string, string, error) {
	op := "check " + repo.String() + " for its newest commit"
	resp, err := c.get(ctx, op, c.url(repo, "/commits?per_page=1"), token, "application/vnd.github+json", etag)
	var gh *Error
	if errors.As(err, &gh) && gh.Status == http.StatusConflict {
		return "", "", &Error{Op: op, Status: gh.Status, Err: ErrEmptyRepository}
	}
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()

	var commits []struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&commits); err != nil {
		return "", "", &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("unexpected answer: %v", err)}
	}
	if len(commits) == 0 || commits[0].SHA == "" {
		return "", "", &Error{Op: op, Status: http.StatusNotFound, Err: ErrEmptyRepository}
	}
	return commits[0].SHA, resp.Header.Get("ETag"), nil
}

// ResolveCommit returns the full SHA of a commit given as a full or short
// SHA (or any ref GitHub understands).
func (c Client) ResolveCommit(ctx context.Context, repo Repo, ref string, token string) (string, error) {
	op := fmt.Sprintf("find commit %q in %s", ref, repo)
	resp, err := c.get(ctx, op, c.url(repo, "/commits/"+url.PathEscape(ref)), token, "application/vnd.github+json", "")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var commit struct {
		SHA string `json:"sha"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&commit); err != nil || commit.SHA == "" {
		return "", &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("unexpected answer: %v", err)}
	}
	return commit.SHA, nil
}

// Archive returns the repository's files at commit sha, as a gzipped tarball
// whose entries all sit in one top-level folder.
func (c Client) Archive(ctx context.Context, repo Repo, sha string, token string) (io.ReadCloser, error) {
	op := fmt.Sprintf("download %s at %.7s", repo, sha)
	resp, err := c.get(ctx, op, c.url(repo, "/tarball/"+sha), token, "application/vnd.github+json", "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Tag is a tag and the commit it points at.
type Tag struct {
	Name string
	SHA  string
}

// maxTagPages caps how many pages of 100 tags are read.
const maxTagPages = 10

// Tags lists the repository's tags, with the commit each points at (annotated
// tags included).
func (c Client) Tags(ctx context.Context, repo Repo, token string, etag string) ([]Tag, string, error) {
	op := "list " + repo.String() + "'s tags"
	page := c.url(repo, "/tags?per_page=100")
	var tags []Tag
	newETag := ""
	for n := 0; n < maxTagPages && page != ""; n++ {
		pageETag := ""
		if n == 0 {
			pageETag = etag
		}
		resp, err := c.get(ctx, op, page, token, "application/vnd.github+json", pageETag)
		if err != nil {
			return nil, "", err
		}
		if n == 0 {
			newETag = resp.Header.Get("ETag")
		}
		var list []struct {
			Name   string `json:"name"`
			Commit struct {
				SHA string `json:"sha"`
			} `json:"commit"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&list)
		resp.Body.Close()
		if err != nil {
			return nil, "", &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("unexpected answer: %v", err)}
		}
		for _, t := range list {
			tags = append(tags, Tag{Name: t.Name, SHA: t.Commit.SHA})
		}
		page = nextPage(resp.Header.Get("Link"))
	}
	return tags, newETag, nil
}

// nextPage finds the rel="next" URL in a Link header.
func nextPage(link string) string {
	for _, part := range strings.Split(link, ",") {
		url, rel, ok := strings.Cut(part, ";")
		if ok && strings.Contains(rel, `rel="next"`) {
			return strings.Trim(strings.TrimSpace(url), "<>")
		}
	}
	return ""
}

// ComposeFiles are the names Compose looks for, in its order.
var ComposeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// ComposeFile returns the compose file at the top of the repository at a
// commit, and its name, without downloading the rest.
func (c Client) ComposeFile(ctx context.Context, repo Repo, sha string, token string) (string, []byte, error) {
	op := fmt.Sprintf("read the compose file of %s at %.7s", repo, sha)
	for _, name := range ComposeFiles {
		text, err := c.file(ctx, op, repo, sha, name, token)
		var gh *Error
		if errors.As(err, &gh) && gh.Status == http.StatusNotFound {
			continue
		}
		return name, text, err
	}
	return "", nil, &Error{Op: op, Status: http.StatusNotFound, Err: fmt.Errorf("there's no compose file at the top of the repository (%s)", strings.Join(ComposeFiles, ", "))}
}

// File returns a file at the top of the repository at a commit, or nil if
// there's no such file.
func (c Client) File(ctx context.Context, repo Repo, sha string, name string, token string) ([]byte, error) {
	text, err := c.file(ctx, fmt.Sprintf("read %s of %s at %.7s", name, repo, sha), repo, sha, name, token)
	var gh *Error
	if errors.As(err, &gh) && gh.Status == http.StatusNotFound {
		return nil, nil
	}
	return text, err
}

func (c Client) file(ctx context.Context, op string, repo Repo, sha string, name string, token string) ([]byte, error) {
	resp, err := c.get(ctx, op, c.url(repo, "/contents/"+name+"?ref="+sha), token, "application/vnd.github.raw+json", "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	text, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, &Error{Op: op, Err: err}
	}
	return text, nil
}

// Contains reports whether commit sha is in the history of head (a commit
// or a branch), such as a tag's commit on the default branch.
func (c Client) Contains(ctx context.Context, repo Repo, head string, sha string, token string) (bool, error) {
	op := fmt.Sprintf("check whether %.7s is in the history of %.7s in %s", sha, head, repo)
	resp, err := c.get(ctx, op, c.url(repo, "/compare/"+url.PathEscape(sha)+"..."+url.PathEscape(head)+"?per_page=1"), token, "application/vnd.github+json", "")
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	var compare struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&compare); err != nil || compare.Status == "" {
		return false, &Error{Op: op, Status: resp.StatusCode, Err: fmt.Errorf("unexpected answer: %v", err)}
	}
	return compare.Status == "ahead" || compare.Status == "identical", nil
}
