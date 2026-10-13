package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github/githubtest"
)

const goodToken = "PVEAPIToken=root@pam!app=secret"

// apiServer is settingsServer with the app's credential check: goodToken passes, anything else fails.
func apiServer(t *testing.T) (*Server, *httptest.Server, *int, string, *int) {
	s, srv, saved, path := settingsServer(t)
	checks := 0
	s.Service.Check = func(_ context.Context, auth string) error {
		checks++
		if auth == goodToken {
			return nil
		}
		return errors.New("no")
	}
	return s, srv, saved, path, &checks
}

func apiDo(t *testing.T, method, u, auth string, body any) (int, []byte) {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, u, r)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func TestAPINeedsProxmoxCredentials(t *testing.T) {
	_, srv, _, _, checks := apiServer(t)
	for _, auth := range []string{"", "PVEAPIToken=root@pam!app=wrong"} {
		if code, _ := apiDo(t, "GET", srv.URL+"/api/v1/settings", auth, nil); code != 401 {
			t.Errorf("auth %q: %d", auth, code)
		}
	}
	// A page login is no way into the API: it takes no cookies, so nothing can be forged cross-site.
	c := client(t)
	c.PostForm(srv.URL+"/login", url.Values{"user": {"root@pam"}, "password": {"pw"}})
	if resp, _ := c.Get(srv.URL + "/api/v1/settings"); resp.StatusCode != 401 {
		t.Errorf("session cookie: %d", resp.StatusCode)
	}
	*checks = 0
	for i := 0; i < 3; i++ {
		if code, b := apiDo(t, "GET", srv.URL+"/api/v1/settings", goodToken, nil); code != 200 {
			t.Fatalf("token: %d %s", code, b)
		}
	}
	// Polling doesn't ask Proxmox each time.
	if *checks != 1 {
		t.Errorf("asked Proxmox %d times", *checks)
	}
}

func TestAPISettingsSaveThroughTheSamePath(t *testing.T) {
	s, srv, saved, path, _ := apiServer(t)
	code, b := apiDo(t, "GET", srv.URL+"/api/v1/settings", goodToken, nil)
	if code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	var before []byte
	var view apiSettingsView
	if err := json.Unmarshal(b, &view); err != nil {
		t.Fatal(err)
	}
	values := map[string]string{}
	kinds := map[string]string{}
	for _, groups := range [][]apiGroup{view.Sections, view.Pools, view.Targets} {
		for _, g := range groups {
			for _, f := range g.Fields {
				values[f.Name], kinds[f.Name] = f.Value, f.Kind
			}
		}
	}
	// Every setting the page has is in the API, with the same name.
	for name := range currentValues(s.Cfg) {
		if _, ok := values[name]; !ok {
			t.Errorf("API lacks %s", name)
		}
	}
	if kinds["loki_token"] != "secret" || kinds["autoscale.enabled"] != "bool" || len(view.Targets) != 1 || view.Targets[0].Title != "Org example-org" {
		t.Fatalf("view: %s", b)
	}

	// Sending back what was shown changes nothing on disk (the first save fills defaults, as the page's does).
	for i := 0; i < 2; i++ {
		if code, b := apiDo(t, "PUT", srv.URL+"/api/v1/settings", goodToken, map[string]any{"values": values}); code != 200 {
			t.Fatalf("unchanged save: %d %s", code, b)
		}
		if i == 0 {
			before, _ = os.ReadFile(path)
		}
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(before, after) {
		t.Fatalf("an unchanged save rewrote the config:\n%s\n---\n%s", before, after)
	}

	values["proxmox.max_runners"] = "8"
	values["pool.default.max"] = "8"
	values["pool.default.cache"] = "true"
	values["autoscale.enabled"] = "false"
	values["loki_token"] = "glc_secret"
	values["new_pool"] = "big"
	if code, b := apiDo(t, "PUT", srv.URL+"/api/v1/settings", goodToken, map[string]any{"values": values}); code != 200 ||
		string(b) != "{\"restarting\":true}\n" {
		t.Fatalf("save: %d %s", code, b)
	}
	if *saved != 3 {
		t.Fatalf("restarts: %d", *saved)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Proxmox.MaxRunners != 8 || !cfg.Pools["default"].Cache || cfg.Autoscale.Enabled || cfg.Pools["big"] == nil {
		t.Fatalf("saved config: %+v", cfg)
	}
	if tok, _ := os.ReadFile(cfg.Logs.TokenFile); string(tok) != "glc_secret\n" {
		t.Fatalf("token file: %q", tok)
	}
	// A secret is never sent back, only that it is set.
	_, b = apiDo(t, "GET", srv.URL+"/api/v1/settings", goodToken, nil)
	if strings.Contains(string(b), "glc_secret") || !strings.Contains(string(b), `"name":"loki_token","label":"Token","help":"Access policy token with logs:write; kept in its own file.","kind":"secret","value":"","set":true`) {
		t.Fatalf("secret in view: %s", b)
	}

	// An invalid save says why and writes nothing.
	mid, _ := os.ReadFile(path)
	values["proxmox.max_runners"] = "lots"
	if code, b := apiDo(t, "PUT", srv.URL+"/api/v1/settings", goodToken, map[string]any{"values": values}); code != 422 ||
		!strings.Contains(string(b), "proxmox.max_runners must be a whole number") {
		t.Fatalf("invalid save: %d %s", code, b)
	}
	if after, _ := os.ReadFile(path); !bytes.Equal(mid, after) || *saved != 3 {
		t.Fatal("an invalid save changed the config or restarted")
	}
	if code, _ := apiDo(t, "PUT", srv.URL+"/api/v1/settings", goodToken, map[string]any{}); code != 400 {
		t.Fatalf("empty body: %d", code)
	}
}

func TestAPIActionsAndStatus(t *testing.T) {
	s, srv, _, _, _ := apiServer(t)
	s.Service.Status = func(context.Context) (*StatusReport, error) {
		return &StatusReport{Version: "build-7", Host: "pve1", Max: 4, Runners: []RunnerRow{{ID: 9000, Name: "gh-pve1-default-9000", Status: "running"}}}, nil
	}
	release := make(chan struct{})
	runs := 0
	s.Service.Actions = []Action{{Name: "pull", Label: "Pull images", Group: "Images", Args: []string{"pool"}, Confirm: "Sure?",
		Run: func(_ context.Context, w io.Writer, args map[string]string) error {
			runs++
			io.WriteString(w, "pulling for "+args["pool"]+"\n")
			<-release
			return errors.New("disk full")
		}}}

	code, b := apiDo(t, "GET", srv.URL+"/api/v1/status", goodToken, nil)
	if code != 200 || !strings.Contains(string(b), `"runners":[{"id":9000,"name":"gh-pve1-default-9000","status":"running"`) {
		t.Fatalf("status: %d %s", code, b)
	}
	code, b = apiDo(t, "GET", srv.URL+"/api/v1/actions", goodToken, nil)
	if code != 200 || string(b) != `[{"name":"pull","label":"Pull images","group":"Images","args":["pool"],"confirm":"Sure?"}]`+"\n" {
		t.Fatalf("actions: %d %s", code, b)
	}
	if code, _ := apiDo(t, "POST", srv.URL+"/api/v1/actions/nope", goodToken, nil); code != 404 {
		t.Fatalf("unknown action: %d", code)
	}
	if code, b := apiDo(t, "POST", srv.URL+"/api/v1/actions/pull", goodToken, map[string]any{"args": map[string]string{"pool": " default "}}); code != 202 {
		t.Fatalf("start: %d %s", code, b)
	}
	last := func() apiRun {
		_, b := apiDo(t, "GET", srv.URL+"/api/v1/actions/pull", goodToken, nil)
		var v apiActionView
		if json.Unmarshal(b, &v) != nil || v.Last == nil {
			t.Fatalf("action: %s", b)
		}
		return *v.Last
	}
	deadline := time.Now().Add(5 * time.Second)
	for last().Output != "pulling for default\n" {
		if time.Now().After(deadline) {
			t.Fatalf("no output: %+v", last())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := apiDo(t, "POST", srv.URL+"/api/v1/actions/pull", goodToken, nil); code != 409 {
		t.Fatalf("second start while running: %d", code)
	}
	close(release)
	for last().Running {
		if time.Now().After(deadline) {
			t.Fatal("never finished")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if l := last(); l.Err != "disk full" || l.Ended == "" || runs != 1 {
		t.Fatalf("finished: %+v, runs %d", l, runs)
	}
}

// fakePermissions answers /access/permissions like pveproxy: the caller's own privileges on the path.
func fakePermissions(t *testing.T) *httptest.Server {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.URL.Path != "/api2/json/access/permissions" || r.URL.Query().Get("path") != "/nodes/pve1" {
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
		cookie, _ := r.Cookie("PVEAuthCookie")
		switch {
		case r.Header.Get("Authorization") == "PVEAPIToken=root@pam!app=secret", cookie != nil && cookie.Value == "PVE:root@pam:ticket":
			fmt.Fprint(w, `{"data":{"/nodes/pve1":{"Sys.Modify":1,"Sys.Audit":1}}}`)
		case r.Header.Get("Authorization") == "PVEAPIToken=viewer@pve!app=secret":
			fmt.Fprint(w, `{"data":{"/nodes/pve1":{"Sys.Audit":1}}}`)
		default:
			w.WriteHeader(401)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPVECheckNeedsSysModify(t *testing.T) {
	srv := fakePermissions(t)
	check := PVECheck(srv.URL, "pve1", PinnedClient(srv.Certificate().Raw))
	ctx := context.Background()
	for auth, ok := range map[string]bool{
		"PVEAPIToken=root@pam!app=secret":   true,
		"PVEAuthCookie=PVE:root@pam:ticket": true,
		"PVEAPIToken=root@pam!app=wrong":    false,
		"PVEAPIToken=viewer@pve!app=secret": false,
		"Bearer PVE:root@pam:ticket":        false,
	} {
		if err := check(ctx, auth); (err == nil) != ok {
			t.Errorf("%q: %v", auth, err)
		}
	}
}

// The API saves secrets where the page does; make sure a test's temp dir got them, not the host's.
func TestAPISecretsStayInSecretDir(t *testing.T) {
	s, srv, _, _, _ := apiServer(t)
	values := map[string]string{}
	for k, v := range currentValues(s.Cfg) {
		values[k] = v[0]
	}
	values["notify_url"] = "https://ntfy.example.com/topic"
	if code, b := apiDo(t, "PUT", srv.URL+"/api/v1/settings", goodToken, map[string]any{"values": values}); code != 200 {
		t.Fatalf("%d %s", code, b)
	}
	if filepath.Dir(s.Cfg.Notify.WebhookURLFile) != s.Service.SecretDir {
		t.Fatalf("notify file at %s", s.Cfg.Notify.WebhookURLFile)
	}
}

// Adding an org from the app: the API hands out a one-time link, the browser opens it and goes through
// GitHub's App pages, and the service saves the new org and restarts.
func TestAddTargetThroughAOneTimeLink(t *testing.T) {
	gh := githubtest.New(42)
	defer gh.Close()
	gh.ManifestCodes["code123"] = true
	gh.Installations = []map[string]any{{"id": 77, "account": map[string]string{"login": "example-org", "type": "Organization"}}}
	s, srv, saved, path, _ := apiServer(t)
	s.KeyDir = filepath.Join(t.TempDir(), "apps")
	s.GitHubWeb = "https://github.example"
	s.API = gh.URL
	s.Cfg.Targets[0].Org = "first-org" // the fake GitHub creates Apps under example-org

	if code, _ := apiDo(t, "POST", srv.URL+"/api/v1/targets", "PVEAPIToken=root@pam!app=wrong", map[string]string{"org": "example-org"}); code != 401 {
		t.Fatalf("without credentials: %d", code)
	}
	if code, b := apiDo(t, "POST", srv.URL+"/api/v1/targets", goodToken, map[string]string{"org": "not an org"}); code != 422 {
		t.Fatalf("bad name: %d %s", code, b)
	}
	code, b := apiDo(t, "POST", srv.URL+"/api/v1/targets", goodToken, map[string]string{"org": "example-org"})
	var link struct{ URL string }
	if code != 201 || json.Unmarshal(b, &link) != nil || !strings.HasPrefix(link.URL, srv.URL+"/app/new?k=") {
		t.Fatalf("link: %d %s", code, b)
	}

	c := client(t)
	if resp, _ := c.Get(srv.URL + "/app/new?k=guess"); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("made-up link: %d", resp.StatusCode)
	}
	resp, _ := c.Get(link.URL)
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(html.UnescapeString(string(body)), `action="https://github.example/organizations/example-org/settings/apps/new?state=`+s.Token+`"`) {
		t.Fatalf("form to GitHub missing:\n%s", body)
	}
	if resp, _ := c.Get(link.URL); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("link reused: %d", resp.StatusCode)
	}

	c.Get(srv.URL + "/app/created?code=code123&state=" + s.Token)
	resp, _ = c.Get(srv.URL + "/app/installed?installation_id=77&setup_action=install")
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 200 || !strings.Contains(string(b), "go back to the app") || *saved != 1 {
		t.Fatalf("installed: %d, restarts %d\n%s", resp.StatusCode, *saved, b)
	}
	got, err := config.Load(path)
	if err != nil || len(got.Targets) != 2 || got.Targets[1].Org != "example-org" || got.Targets[1].InstallationID != 77 {
		t.Fatalf("config: %v %+v", err, got.Targets)
	}
}

// An error in the GitHub App pages is shown on the page they send the browser back to.
func TestServicePageShowsAppErrors(t *testing.T) {
	_, srv, _, _, _ := apiServer(t)
	c := client(t)
	resp, _ := c.Get(srv.URL + "/app/installed?installation_id=nope")
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("redirect: %q", loc)
	}
	resp, _ = c.Get(srv.URL + "/")
	if b, _ := io.ReadAll(resp.Body); !strings.Contains(string(b), "GitHub did not return an installation.") {
		t.Fatalf("error not shown:\n%s", b)
	}
}
