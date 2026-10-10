package setup

import (
	"encoding/json"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github/githubtest"
)

func TestManifestPermissions(t *testing.T) {
	org := Manifest("n", "http://192.0.2.10:8765", true)
	perms := org["default_permissions"].(map[string]string)
	if perms["organization_self_hosted_runners"] != "write" || perms["administration"] != "" || org["public"] != false {
		t.Fatalf("org manifest: %v", org)
	}
	if org["redirect_url"] != "http://192.0.2.10:8765/app/created" || org["setup_url"] != "http://192.0.2.10:8765/app/installed" {
		t.Fatalf("urls: %v", org)
	}
	user := Manifest("n", "http://h", false)["default_permissions"].(map[string]string)
	if user["administration"] != "write" || user["organization_self_hosted_runners"] != "" {
		t.Fatalf("user manifest: %v", user)
	}
	if n := AppName("pve1", "a-very-long-organization-name-indeed"); len(n) > 34 {
		t.Fatalf("app name too long: %q", n)
	}
}

func client(t *testing.T) *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func TestFullSetupFlowForAnOrg(t *testing.T) {
	gh := githubtest.New(42)
	defer gh.Close()
	gh.ManifestCodes["code123"] = true
	gh.Installations = []map[string]any{{"id": 77, "account": map[string]string{"login": "example-org", "type": "Organization"}}}

	dir := t.TempDir()
	cfg := config.Default()
	s, err := NewServer(cfg, filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	s.KeyDir = filepath.Join(dir, "apps")
	s.GitHubWeb = "https://github.example"
	s.API = gh.URL
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c := client(t)

	// Without the token nothing is reachable.
	if resp, _ := c.Get(srv.URL + "/"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	if resp, _ := c.Get(srv.URL + "/?t=" + s.Token); resp.StatusCode != 200 {
		t.Fatalf("index: %d", resp.StatusCode)
	}
	c.PostForm(srv.URL+"/host", url.Values{"node": {"pve1"}, "template": {"8000"}, "max_runners": {"4"}})
	if p := s.Cfg.Pools["default"]; p.Template != 8000 || p.Image != "" {
		t.Fatalf("hand-made template: %+v", p)
	}
	// Leaving the template empty switches the pool to the published image.
	c.PostForm(srv.URL+"/host", url.Values{"node": {"pve1"}, "template": {""}, "max_runners": {"4"}, "bridge": {"vmbr4"}})

	resp, _ := c.PostForm(srv.URL+"/app/new", url.Values{"org": {"example-org"}})
	body, _ := io.ReadAll(resp.Body)
	page := html.UnescapeString(string(body))
	if !strings.Contains(page, `action="https://github.example/organizations/example-org/settings/apps/new?state=`+s.Token+`"`) {
		t.Fatalf("form action missing:\n%s", page)
	}
	m := regexp.MustCompile(`name="manifest" value="([^"]*)"`).FindStringSubmatch(string(body))
	var manifest map[string]any
	if m == nil || json.Unmarshal([]byte(html.UnescapeString(m[1])), &manifest) != nil || manifest["redirect_url"] != srv.URL+"/app/created" {
		t.Fatalf("manifest: %v", m)
	}

	// Wrong state is rejected; the right one stores the key and sends the user to install.
	if resp, _ := c.Get(srv.URL + "/app/created?code=code123&state=nope"); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad state: %d", resp.StatusCode)
	}
	resp, _ = c.Get(srv.URL + "/app/created?code=code123&state=" + s.Token)
	if loc := resp.Header.Get("Location"); loc != "https://github.example/apps/pve-runners-test/installations/new" {
		t.Fatalf("install redirect: %q", loc)
	}
	if fi, err := os.Stat(filepath.Join(dir, "apps", "example-org.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v", err)
	}

	resp, _ = c.Get(srv.URL + "/app/installed?installation_id=77&setup_action=install")
	if resp.Header.Get("Location") != "/" {
		t.Fatalf("installed: %q", resp.Header.Get("Location"))
	}
	c.PostForm(srv.URL+"/finish", nil)
	select {
	case <-s.Done:
	default:
		t.Fatal("setup should finish")
	}
	got, err := config.Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tg := got.Targets[0]
	if tg.Org != "example-org" || tg.AppID != 42 || tg.InstallationID != 77 || got.Pools["default"].Template != 0 ||
		got.Pools["default"].Image != "ubuntu-latest" || got.Proxmox.Bridge != "vmbr4" || got.Pools["default"].Bridge != "vmbr4" ||
		got.Pools["default"].Labels[0] != "proxmox" || got.Proxmox.Node != "pve1" {
		t.Fatalf("config: %+v %+v", tg, got.Pools["default"])
	}
}

func TestPersonalAccountKeepsOnlyPrivateRepos(t *testing.T) {
	gh := githubtest.New(42)
	defer gh.Close()
	gh.ManifestCodes["c"] = true
	gh.Installations = []map[string]any{{"id": 5, "account": map[string]string{"login": "example-org", "type": "User"}}}
	gh.Repos[5] = []map[string]any{{"name": "secret", "private": true}, {"name": "oss", "private": false}}
	dir := t.TempDir()
	s, _ := NewServer(config.Default(), filepath.Join(dir, "c.yaml"))
	s.KeyDir, s.API = dir, gh.URL
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	c := client(t)
	c.Get(srv.URL + "/?t=" + s.Token)
	// The fake always reports owner "example-org"; register that name as the personal account.
	c.PostForm(srv.URL+"/app/new", url.Values{"user": {"example-org"}})
	c.Get(srv.URL + "/app/created?code=c&state=" + s.Token)
	c.Get(srv.URL + "/app/installed?installation_id=5")
	tg := s.Cfg.Targets[0]
	if tg.User != "example-org" || len(tg.Repos) != 1 || tg.Repos[0] != "secret" {
		t.Fatalf("target %+v", tg)
	}
}
