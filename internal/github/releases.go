package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Release is a GitHub Release.
type Release struct {
	ID         int64  `json:"id"`
	Tag        string `json:"tag_name"`
	URL        string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
}

// NewRelease is a Release to create for a tag that already exists.
type NewRelease struct {
	Tag        string
	Body       string
	Prerelease bool
	Latest     bool // GitHub's "latest" Release
}

// TagCommit returns the commit a tag points at (through an annotated tag);
// found is false if there's no such tag.
func (c Client) TagCommit(ctx context.Context, repo Repo, tag string, token string) (string, bool, error) {
	op := fmt.Sprintf("find the commit of tag %s in %s", tag, repo)
	var ref struct {
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	err := c.call(ctx, http.MethodGet, op, c.url(repo, "/git/ref/tags/"+url.PathEscape(tag)), token, nil, &ref, http.StatusOK)
	var gh *Error
	if errors.As(err, &gh) && gh.Status == http.StatusNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	for range 5 {
		if ref.Object.Type != "tag" {
			break
		}
		if err := c.call(ctx, http.MethodGet, op, c.url(repo, "/git/tags/"+ref.Object.SHA), token, nil, &ref, http.StatusOK); err != nil {
			return "", false, err
		}
	}
	if ref.Object.Type != "commit" || ref.Object.SHA == "" {
		return "", false, &Error{Op: op, Status: http.StatusUnprocessableEntity, Err: fmt.Errorf("the tag points at a %s, not a commit", ref.Object.Type)}
	}
	return ref.Object.SHA, true, nil
}

// ReleaseByTag returns the published Release for tag; found is false if
// there's none.
func (c Client) ReleaseByTag(ctx context.Context, repo Repo, tag string, token string) (Release, bool, error) {
	var r Release
	err := c.call(ctx, http.MethodGet, fmt.Sprintf("find the Release of %s in %s", tag, repo),
		c.url(repo, "/releases/tags/"+url.PathEscape(tag)), token, nil, &r, http.StatusOK)
	var gh *Error
	if errors.As(err, &gh) && gh.Status == http.StatusNotFound {
		return Release{}, false, nil
	}
	return r, err == nil, err
}

// Releases lists the repository's Releases, newest first (up to 1,000).
func (c Client) Releases(ctx context.Context, repo Repo, token string) ([]Release, error) {
	op := "list " + repo.String() + "'s Releases"
	var all []Release
	for page := 1; page <= maxTagPages; page++ {
		var list []Release
		if err := c.call(ctx, http.MethodGet, op, c.url(repo, fmt.Sprintf("/releases?per_page=100&page=%d", page)), token, nil, &list, http.StatusOK); err != nil {
			return nil, err
		}
		all = append(all, list...)
		if len(list) < 100 {
			break
		}
	}
	return all, nil
}

// CreateRelease publishes a Release for an existing tag. If one already
// exists (another attempt got there first), it returns that one.
func (c Client) CreateRelease(ctx context.Context, repo Repo, r NewRelease, token string) (Release, error) {
	latest := "false"
	if r.Latest {
		latest = "true"
	}
	body := map[string]any{"tag_name": r.Tag, "name": r.Tag, "body": r.Body, "draft": false, "prerelease": r.Prerelease, "make_latest": latest}
	var created Release
	err := c.call(ctx, http.MethodPost, fmt.Sprintf("publish the Release of %s in %s", r.Tag, repo), c.url(repo, "/releases"), token, body, &created, http.StatusCreated)
	var gh *Error
	if errors.As(err, &gh) && gh.Status == http.StatusUnprocessableEntity && strings.Contains(gh.Err.Error(), "already_exists") {
		existing, found, err := c.ReleaseByTag(ctx, repo, r.Tag, token)
		if err == nil && found {
			return existing, nil
		}
	}
	return created, err
}
