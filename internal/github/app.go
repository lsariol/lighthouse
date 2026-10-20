package github

import (
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrNotInstalled means the GitHub App isn't installed on a repository, so
// nothing is published there.
var ErrNotInstalled = errors.New("the GitHub App isn't installed on the repository")

// App signs in as a GitHub App Lighthouse's owner created, and publishes
// Releases with short-lived installation tokens, one repository at a time.
type App struct {
	ID   string                                    // the App ID
	Key  func(ctx context.Context) ([]byte, error) // its private key: the .pem file, or that file in base64
	HTTP *http.Client
	API  string

	Now func() time.Time // time.Now if nil

	mu     sync.Mutex
	signer *rsa.PrivateKey
	tokens map[string]appToken
}

type appToken struct {
	value   string
	expires time.Time
}

// tokenMargin is how long before it expires an installation token is
// replaced.
const tokenMargin = 5 * time.Minute

func (a *App) client() Client {
	return Client{HTTP: a.HTTP, API: a.API,
		Token: "the GitHub App (GITHUB_APP_ID " + a.ID + ", its key LIGHTHOUSE_GITHUB_APP_SECRET in Cove)", Needs: "Contents: read and write"}
}

func (a *App) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

// Publish creates a Release in repo with an installation token.
// ErrNotInstalled if the App isn't installed there.
func (a *App) Publish(ctx context.Context, repo Repo, r NewRelease) (Release, error) {
	token, err := a.Token(ctx, repo)
	if err != nil {
		return Release{}, err
	}
	return a.client().CreateRelease(ctx, repo, r, token)
}

// Token returns an installation token for repo that can write its contents.
func (a *App) Token(ctx context.Context, repo Repo) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	key := strings.ToLower(repo.String())
	if t, ok := a.tokens[key]; ok && a.now().Before(t.expires.Add(-tokenMargin)) {
		return t.value, nil
	}

	jwt, err := a.jwt(ctx)
	if err != nil {
		return "", err
	}
	c := a.client()
	var installation struct {
		ID int64 `json:"id"`
	}
	err = c.call(ctx, http.MethodGet, "find the GitHub App's installation on "+repo.String(), c.url(repo, "/installation"), jwt, nil, &installation, http.StatusOK)
	var gh *Error
	switch {
	case errors.As(err, &gh) && gh.Status == http.StatusNotFound:
		return "", fmt.Errorf("%w %s: install it there to publish its Releases", ErrNotInstalled, repo)
	case errors.As(err, &gh) && gh.Status == http.StatusUnauthorized:
		a.signer = nil // read the key again next time: it may have been replaced
		return "", err
	case err != nil:
		return "", err
	}

	var t struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	body := map[string]any{"repositories": []string{repo.Name}, "permissions": map[string]string{"contents": "write"}}
	if err := c.call(ctx, http.MethodPost, "get a GitHub App token for "+repo.String(),
		c.apiURL(fmt.Sprintf("/app/installations/%d/access_tokens", installation.ID)), jwt, body, &t, http.StatusCreated); err != nil {
		return "", err
	}
	if a.tokens == nil {
		a.tokens = map[string]appToken{}
	}
	a.tokens[key] = appToken{value: t.Token, expires: t.ExpiresAt}
	return t.Token, nil
}

// jwt signs the App's short-lived sign-in token (RS256, valid 9 minutes,
// backdated one for clock drift).
func (a *App) jwt(ctx context.Context) (string, error) {
	if a.signer == nil {
		pemText, err := a.Key(ctx)
		if err != nil {
			return "", &Error{Op: "read the GitHub App's key", Err: err}
		}
		signer, err := ParseAppKey(pemText)
		if err != nil {
			return "", err
		}
		a.signer = signer
	}
	var iss any = a.ID
	if n, err := strconv.ParseInt(a.ID, 10, 64); err == nil {
		iss = n
	}
	now := a.now()
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": iss})
	enc := base64.RawURLEncoding
	signed := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`)) + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(nil, a.signer, crypto.SHA256, sum[:])
	if err != nil {
		return "", fmt.Errorf("GitHub App: sign in: %w", err)
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// ParseAppKey reads a GitHub App's private key: the .pem file GitHub gives,
// or that file encoded in base64.
func ParseAppKey(text []byte) (*rsa.PrivateKey, error) {
	text = []byte(strings.TrimSpace(string(text)))
	if !strings.HasPrefix(string(text), "-----BEGIN") {
		decoded, err := base64.StdEncoding.DecodeString(string(text))
		if err != nil {
			return nil, errors.New("GitHub App: LIGHTHOUSE_GITHUB_APP_SECRET in Cove isn't a private key: store the whole .pem file GitHub gave you, or it in base64")
		}
		text = decoded
	}
	block, _ := pem.Decode(text)
	if block == nil {
		return nil, errors.New("GitHub App: LIGHTHOUSE_GITHUB_APP_SECRET in Cove isn't a PEM private key: store the whole .pem file, from -----BEGIN to -----END")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if rsaKey, ok := k.(*rsa.PrivateKey); err == nil && ok {
		return rsaKey, nil
	}
	return nil, errors.New("GitHub App: LIGHTHOUSE_GITHUB_APP_SECRET in Cove isn't an RSA private key: generate a new private key in the App's settings and store it")
}
