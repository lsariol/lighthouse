package github

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAppGitHub is GitHub for the App: it checks the sign-in token against
// key, has the App installed on o/good only, and keeps Releases.
type fakeAppGitHub struct {
	mu       sync.Mutex
	key      *rsa.PublicKey
	tokens   int
	releases map[string]Release // by tag
	created  []map[string]any
}

func (f *fakeAppGitHub) signedIn(r *http.Request) bool {
	jwt := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err != nil || rsa.VerifyPKCS1v15(f.key, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss      int64
		Iat, Exp int64
	}
	json.Unmarshal(claims, &c)
	return c.Iss == 42 && c.Exp > c.Iat
}

func (f *fakeAppGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.URL.Path == "/repos/o/good/installation" || r.URL.Path == "/repos/o/other/installation":
		if !f.signedIn(r) {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(`{"message":"A JSON web token could not be decoded"}`))
			return
		}
		if strings.Contains(r.URL.Path, "other") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"id":7}`))
	case r.URL.Path == "/app/installations/7/access_tokens" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		if !f.signedIn(r) || !strings.Contains(string(body), `"repositories":["good"]`) || !strings.Contains(string(body), `"contents":"write"`) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		f.tokens++
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"token":"inst","expires_at":"` + time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `"}`))
	case r.URL.Path == "/repos/o/good/releases" && r.Method == http.MethodPost:
		if r.Header.Get("Authorization") != "Bearer inst" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		tag := body["tag_name"].(string)
		if _, ok := f.releases[tag]; ok {
			w.WriteHeader(http.StatusUnprocessableEntity)
			w.Write([]byte(`{"message":"Validation Failed","errors":[{"resource":"Release","code":"already_exists","field":"tag_name"}]}`))
			return
		}
		f.created = append(f.created, body)
		rel := Release{ID: int64(len(f.created)), Tag: tag, URL: "https://github.com/o/good/releases/tag/" + tag}
		f.releases[tag] = rel
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(rel)
	case strings.HasPrefix(r.URL.Path, "/repos/o/good/releases/tags/"):
		rel, ok := f.releases[strings.TrimPrefix(r.URL.Path, "/repos/o/good/releases/tags/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewEncoder(w).Encode(rel)
	case r.URL.Path == "/repos/o/good/releases":
		var list []Release
		if r.URL.Query().Get("page") == "1" {
			for _, rel := range f.releases {
				list = append(list, rel)
			}
		}
		json.NewEncoder(w).Encode(list)
	case r.URL.Path == "/repos/o/good/git/ref/tags/v1.0.0":
		w.Write([]byte(`{"object":{"type":"commit","sha":"c100"}}`))
	case r.URL.Path == "/repos/o/good/git/ref/tags/v2.0.0":
		w.Write([]byte(`{"object":{"type":"tag","sha":"t200"}}`))
	case r.URL.Path == "/repos/o/good/git/tags/t200":
		w.Write([]byte(`{"object":{"type":"commit","sha":"c200"}}`))
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"message":"Not Found"}`))
	}
}

func newApp(t *testing.T) (*App, *fakeAppGitHub) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAppGitHub{key: &key.PublicKey, releases: map[string]Release{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	pemText := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	app := &App{ID: "42", HTTP: srv.Client(), API: srv.URL,
		Key: func(ctx context.Context) ([]byte, error) { return pemText, nil }}
	return app, f
}

func TestAppPublishes(t *testing.T) {
	app, f := newApp(t)
	ctx := context.Background()
	good, other := Repo{Owner: "o", Name: "good"}, Repo{Owner: "o", Name: "other"}

	rel, err := app.Publish(ctx, good, NewRelease{Tag: "v1.0.0", Body: "## v1.0.0 (2026-10-20)\n", Latest: true})
	if err != nil || rel.URL != "https://github.com/o/good/releases/tag/v1.0.0" {
		t.Fatalf("Publish = %+v, %v", rel, err)
	}
	if c := f.created[0]; c["make_latest"] != "true" || c["prerelease"] != false || c["draft"] != false || c["name"] != "v1.0.0" || c["target_commitish"] != nil {
		t.Errorf("created %v", c)
	}

	again, err := app.Publish(ctx, good, NewRelease{Tag: "v1.0.0"})
	if err != nil || again.ID != rel.ID || f.tokens != 1 {
		t.Errorf("publishing twice = %+v, %v; %d tokens (want the cached one)", again, err, f.tokens)
	}

	if _, err := app.Publish(ctx, other, NewRelease{Tag: "v1.0.0"}); !errors.Is(err, ErrNotInstalled) || !strings.Contains(err.Error(), "o/other") {
		t.Errorf("a repository without the App = %v", err)
	}

	wrong, _ := rsa.GenerateKey(rand.Reader, 2048)
	app2 := &App{ID: "42", HTTP: app.HTTP, API: app.API, Key: func(ctx context.Context) ([]byte, error) {
		return []byte(base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(wrong)}))), nil
	}}
	_, err = app2.Publish(ctx, good, NewRelease{Tag: "v2.0.0"})
	var gh *Error
	if !errors.As(err, &gh) || gh.Status != http.StatusUnauthorized || !strings.Contains(err.Error(), "GITHUB_APP_ID 42") || app2.signer != nil {
		t.Errorf("a wrong key (in base64) = %v", err)
	}
}

func TestAppKey(t *testing.T) {
	app := &App{ID: "42", Key: func(ctx context.Context) ([]byte, error) { return nil, errors.New("Cove: not found") }}
	if _, err := app.Token(context.Background(), Repo{Owner: "o", Name: "good"}); err == nil || !temporary(err) {
		t.Errorf("no key in Cove = %v; want a temporary error", err)
	}
	for _, text := range []string{"", "not a key", "-----BEGIN RSA PRIVATE KEY-----\nAAAA\n-----END RSA PRIVATE KEY-----"} {
		if _, err := ParseAppKey([]byte(text)); err == nil || !strings.Contains(err.Error(), "LIGHTHOUSE_GITHUB_APP_SECRET") {
			t.Errorf("ParseAppKey(%q) = %v", text, err)
		}
	}
}

func TestReleaseReads(t *testing.T) {
	app, f := newApp(t)
	c := Client{HTTP: app.HTTP, API: app.API}
	ctx := context.Background()
	good := Repo{Owner: "o", Name: "good"}

	for tag, want := range map[string]string{"v1.0.0": "c100", "v2.0.0": "c200"} {
		if sha, found, err := c.TagCommit(ctx, good, tag, "t"); err != nil || !found || sha != want {
			t.Errorf("TagCommit(%s) = %q, %v, %v", tag, sha, found, err)
		}
	}
	if _, found, err := c.TagCommit(ctx, good, "v9.0.0", "t"); err != nil || found {
		t.Errorf("TagCommit of a missing tag = %v, %v", found, err)
	}

	if _, found, err := c.ReleaseByTag(ctx, good, "v1.0.0", "t"); err != nil || found {
		t.Errorf("ReleaseByTag before publishing = %v, %v", found, err)
	}
	f.releases["v1.0.0"] = Release{ID: 1, Tag: "v1.0.0", URL: "u"}
	if r, found, err := c.ReleaseByTag(ctx, good, "v1.0.0", "t"); err != nil || !found || r.URL != "u" {
		t.Errorf("ReleaseByTag = %+v, %v, %v", r, found, err)
	}
	if list, err := c.Releases(ctx, good, "t"); err != nil || len(list) != 1 {
		t.Errorf("Releases = %+v, %v", list, err)
	}
}
