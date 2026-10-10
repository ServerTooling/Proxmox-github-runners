// Package githubtest is an in-process fake of the GitHub REST endpoints this tool uses. It verifies
// App JWTs and installation tokens the way GitHub would, and records request bodies for assertions.
package githubtest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

type Fake struct {
	*httptest.Server
	Key   *rsa.PrivateKey
	AppID int64

	mu            sync.Mutex
	Installations []map[string]any
	Repos         map[int64][]map[string]any
	RunnerGroups  map[string][]map[string]any
	JITRequests   []Recorded
	// RegTokens records registration-token requests by path; each returns "regtok-<installation>".
	RegTokens     []string
	ManifestCodes map[string]bool
	// RepoVisibility maps "owner/repo" to public | private | internal.
	RepoVisibility map[string]string
	tokens         map[string]int64
	nextRunner     int64
}

type Recorded struct {
	Path string
	Body map[string]any
}

func GenerateKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

func KeyPEM(k *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)})
}

func New(appID int64) *Fake {
	f := &Fake{Key: GenerateKey(), AppID: appID, Repos: map[int64][]map[string]any{},
		RunnerGroups: map[string][]map[string]any{}, ManifestCodes: map[string]bool{}, RepoVisibility: map[string]string{}, tokens: map[string]int64{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	return f
}

func (f *Fake) verifyJWT(auth string) bool {
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return false
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return false
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(&f.Key.PublicKey, crypto.SHA256, sum[:], sig) != nil {
		return false
	}
	claims, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
	}
	_ = json.Unmarshal(claims, &c)
	return c.Iss == fmt.Sprint(f.AppID)
}

func (f *Fake) installationFor(auth string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.tokens[strings.TrimPrefix(auth, "Bearer ")]
	return id, ok
}

func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	auth := r.Header.Get("Authorization")
	path := r.URL.Path
	switch {
	case r.Method == "POST" && strings.HasPrefix(path, "/app-manifests/") && strings.HasSuffix(path, "/conversions"):
		code := strings.TrimSuffix(strings.TrimPrefix(path, "/app-manifests/"), "/conversions")
		f.mu.Lock()
		ok := f.ManifestCodes[code]
		delete(f.ManifestCodes, code)
		f.mu.Unlock()
		if !ok {
			write(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		write(w, 201, map[string]any{"id": f.AppID, "slug": "pve-runners-test", "name": "pve-runners-test",
			"pem": string(KeyPEM(f.Key)), "webhook_secret": "whsec", "client_id": "cid", "client_secret": "csecret",
			"owner": map[string]string{"login": "example-org", "type": "Organization"}})
		return
	case strings.HasPrefix(path, "/app/"):
		if !f.verifyJWT(auth) {
			write(w, 401, map[string]string{"message": "A JSON web token could not be decoded"})
			return
		}
		if r.Method == "GET" && path == "/app/installations" {
			write(w, 200, f.Installations)
			return
		}
		var inst int64
		if _, err := fmt.Sscanf(path, "/app/installations/%d/access_tokens", &inst); err == nil && r.Method == "POST" {
			tok := fmt.Sprintf("ghs_fake_%d_%d", inst, len(f.tokens))
			f.mu.Lock()
			f.tokens[tok] = inst
			f.mu.Unlock()
			write(w, 201, map[string]any{"token": tok, "expires_at": "2099-01-01T00:00:00Z"})
			return
		}
	}
	inst, ok := f.installationFor(auth)
	if !ok {
		write(w, 401, map[string]string{"message": "Bad credentials"})
		return
	}
	switch {
	case r.Method == "GET" && path == "/installation/repositories":
		write(w, 200, map[string]any{"repositories": f.Repos[inst]})
	case r.Method == "GET" && strings.HasPrefix(path, "/repos/") && strings.Count(path, "/") == 3:
		name := strings.TrimPrefix(path, "/repos/")
		vis, ok := f.RepoVisibility[name]
		if !ok {
			write(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		write(w, 200, map[string]any{"full_name": name, "private": vis != "public", "visibility": vis})
	case r.Method == "GET" && strings.HasSuffix(path, "/actions/runner-groups"):
		org := strings.Split(path, "/")[2]
		groups, ok := f.RunnerGroups[org]
		if !ok {
			// Like GitHub for a name that isn't an org (a personal account, or a typo).
			write(w, 404, map[string]any{"message": "Not Found"})
			return
		}
		write(w, 200, map[string]any{"runner_groups": groups})
	case r.Method == "POST" && strings.HasSuffix(path, "/actions/runners/registration-token"):
		f.mu.Lock()
		f.RegTokens = append(f.RegTokens, path)
		f.mu.Unlock()
		write(w, 201, map[string]any{"token": fmt.Sprintf("regtok-%d", inst), "expires_at": "2099-01-01T00:00:00Z"})
	case r.Method == "POST" && strings.HasSuffix(path, "/actions/runners/generate-jitconfig"):
		body, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		f.mu.Lock()
		f.JITRequests = append(f.JITRequests, Recorded{path, m})
		f.nextRunner++
		id := f.nextRunner
		f.mu.Unlock()
		write(w, 201, map[string]any{"runner": map[string]any{"id": id, "name": m["name"]},
			"encoded_jit_config": base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("jit-%d", id)))})
	default:
		write(w, 404, map[string]string{"message": "Not Found"})
	}
}
