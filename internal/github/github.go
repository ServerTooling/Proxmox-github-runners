// Package github talks to the GitHub REST API as a GitHub App: JWT signing, installation tokens,
// the App-manifest conversion used by setup, and just-in-time (JIT) runner configs.
//
// Hand-written on net/http rather than go-github: the tool needs a handful of endpoints, and keeping
// the request bodies explicit lets tests assert exactly what is sent.
package github

import (
	"bytes"
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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const DefaultAPI = "https://api.github.com"

// ParsePrivateKey accepts the PKCS#1 PEM GitHub issues for Apps (and PKCS#8 for good measure).
func ParsePrivateKey(pemData []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemData)
	if block == nil {
		return nil, errors.New("app private key: no PEM block found")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("app private key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("app private key is not RSA")
	}
	return rk, nil
}

// AppJWT signs the short-lived RS256 token that authenticates as the App itself. iat is backdated
// 60s to tolerate clock drift, as GitHub recommends; exp stays under GitHub's 10-minute cap.
func AppJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-60 * time.Second).Unix(),
		"exp": now.Add(9 * time.Minute).Unix(),
		"iss": fmt.Sprint(appID),
	})
	signing := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + enc.EncodeToString(sig), nil
}

// Client is an App-authenticated GitHub client. Installation tokens are cached until shortly
// before they expire.
type Client struct {
	BaseURL string
	HTTP    *http.Client
	AppID   int64
	Key     *rsa.PrivateKey
	Now     func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token   string
	expires time.Time
}

func NewClient(baseURL string, appID int64, key *rsa.PrivateKey) *Client {
	if baseURL == "" {
		baseURL = DefaultAPI
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), HTTP: &http.Client{Timeout: 30 * time.Second},
		AppID: appID, Key: key, Now: time.Now, tokens: map[int64]cachedToken{}}
}

// APIError carries GitHub's status and message so callers can tell auth problems from others.
type APIError struct {
	Status  int
	Method  string
	Path    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

func do(ctx context.Context, hc *http.Client, method, base, path, auth string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "pve-runners")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &m)
		return &APIError{Status: resp.StatusCode, Method: method, Path: path, Message: m.Message}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func (c *Client) asApp(ctx context.Context, method, path string, body, out any) error {
	jwt, err := AppJWT(c.AppID, c.Key, c.Now())
	if err != nil {
		return err
	}
	return do(ctx, c.HTTP, method, c.BaseURL, path, "Bearer "+jwt, body, out)
}

func (c *Client) asInstallation(ctx context.Context, inst int64, method, path string, body, out any) error {
	tok, err := c.InstallationToken(ctx, inst)
	if err != nil {
		return err
	}
	return do(ctx, c.HTTP, method, c.BaseURL, path, "Bearer "+tok, body, out)
}

// InstallationToken returns a cached 1-hour installation token, refreshing it 5 minutes early.
func (c *Client) InstallationToken(ctx context.Context, inst int64) (string, error) {
	c.mu.Lock()
	if t, ok := c.tokens[inst]; ok && c.Now().Before(t.expires.Add(-5*time.Minute)) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := c.asApp(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst), nil, &out); err != nil {
		return "", err
	}
	c.mu.Lock()
	c.tokens[inst] = cachedToken{out.Token, out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

type Account struct {
	Login string `json:"login"`
	Type  string `json:"type"` // Organization | User
}

type Installation struct {
	ID                  int64   `json:"id"`
	Account             Account `json:"account"`
	RepositorySelection string  `json:"repository_selection"` // all | selected
}

// Installations lists every account the App is installed on.
func (c *Client) Installations(ctx context.Context) ([]Installation, error) {
	var out []Installation
	err := c.asApp(ctx, http.MethodGet, "/app/installations?per_page=100", nil, &out)
	return out, err
}

type Repo struct {
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// InstallationRepos lists repos an installation can see (first 100; enough for setup's picker).
func (c *Client) InstallationRepos(ctx context.Context, inst int64) ([]Repo, error) {
	var out struct {
		Repositories []Repo `json:"repositories"`
	}
	err := c.asInstallation(ctx, inst, http.MethodGet, "/installation/repositories?per_page=100", nil, &out)
	return out.Repositories, err
}

type RunnerGroup struct {
	ID                       int64  `json:"id"`
	Name                     string `json:"name"`
	AllowsPublicRepositories bool   `json:"allows_public_repositories"`
}

// ErrPublicRepo is returned whenever a runner would become reachable from a public repository. The
// tool only serves private repos; public ones can use GitHub-hosted runners, and a fork PR on a public
// repo could otherwise run arbitrary code on the home network.
var ErrPublicRepo = errors.New("public repositories are not served")

// RunnerGroupID resolves an org runner group by name ("" means the built-in Default group) and
// refuses any group that lets public repositories use its runners.
func (c *Client) RunnerGroupID(ctx context.Context, inst int64, org, name string) (int64, error) {
	if name == "" {
		name = "Default"
	}
	var out struct {
		RunnerGroups []RunnerGroup `json:"runner_groups"`
	}
	path := "/orgs/" + url.PathEscape(org) + "/actions/runner-groups?per_page=100"
	if err := c.asInstallation(ctx, inst, http.MethodGet, path, nil, &out); err != nil {
		return 0, err
	}
	for _, g := range out.RunnerGroups {
		if strings.EqualFold(g.Name, name) {
			if g.AllowsPublicRepositories {
				return 0, fmt.Errorf("%w: runner group %q in %s allows public repositories; turn that off under Settings > Actions > Runner groups", ErrPublicRepo, g.Name, org)
			}
			return g.ID, nil
		}
	}
	return 0, fmt.Errorf("runner group %q not found in org %s; create it under Settings > Actions > Runner groups", name, org)
}

// JITRequest is the body GitHub expects for generate-jitconfig.
type JITRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder"`
}

type JITConfig struct {
	Runner struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// OrgJITConfig creates a single-use runner registration for an org. The runner removes itself from
// GitHub after one job, which is what makes it ephemeral.
func (c *Client) OrgJITConfig(ctx context.Context, inst int64, org string, req JITRequest) (*JITConfig, error) {
	var out JITConfig
	path := "/orgs/" + url.PathEscape(org) + "/actions/runners/generate-jitconfig"
	return &out, c.asInstallation(ctx, inst, http.MethodPost, path, req, &out)
}

// RepoPrivate reports whether a repository is private (or internal), as seen by the installation.
func (c *Client) RepoPrivate(ctx context.Context, inst int64, owner, repo string) (bool, error) {
	var out struct {
		Private    bool   `json:"private"`
		Visibility string `json:"visibility"`
	}
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
	if err := c.asInstallation(ctx, inst, http.MethodGet, path, nil, &out); err != nil {
		return false, err
	}
	return out.Private || out.Visibility == "internal", nil
}

// RepoJITConfig is the repo-level variant, used for personal accounts (which have no org runners).
// Repo runners have no groups; GitHub requires runner_group_id 1. Public repos are refused.
func (c *Client) RepoJITConfig(ctx context.Context, inst int64, owner, repo string, req JITRequest) (*JITConfig, error) {
	private, err := c.RepoPrivate(ctx, inst, owner, repo)
	if err != nil {
		return nil, err
	}
	if !private {
		return nil, fmt.Errorf("%w: %s/%s is public", ErrPublicRepo, owner, repo)
	}
	req.RunnerGroupID = 1
	var out JITConfig
	path := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/actions/runners/generate-jitconfig"
	return &out, c.asInstallation(ctx, inst, http.MethodPost, path, req, &out)
}

// AppCredentials is what GitHub returns once when an App is created from a manifest.
type AppCredentials struct {
	ID            int64   `json:"id"`
	Slug          string  `json:"slug"`
	Name          string  `json:"name"`
	HTMLURL       string  `json:"html_url"`
	PEM           string  `json:"pem"`
	WebhookSecret string  `json:"webhook_secret"`
	ClientID      string  `json:"client_id"`
	ClientSecret  string  `json:"client_secret"`
	Owner         Account `json:"owner"`
}

// ConvertManifest exchanges the one-time code from the manifest flow redirect for the App's
// credentials. It is unauthenticated: the code itself is the credential and expires after an hour.
func ConvertManifest(ctx context.Context, hc *http.Client, baseURL, code string) (*AppCredentials, error) {
	if baseURL == "" {
		baseURL = DefaultAPI
	}
	var out AppCredentials
	err := do(ctx, hc, http.MethodPost, strings.TrimRight(baseURL, "/"), "/app-manifests/"+url.PathEscape(code)+"/conversions", "", nil, &out)
	return &out, err
}
