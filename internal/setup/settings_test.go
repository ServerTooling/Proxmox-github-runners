package setup

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
)

// currentValues is what the app sends back unchanged: every field's current value.
func currentValues(c *config.Config) url.Values {
	v := url.Values{}
	for _, sec := range hostFields() {
		for _, f := range sec.Fields {
			v.Set(f.Name, f.get(c))
		}
	}
	for _, name := range c.PoolNames() {
		for _, f := range poolFields() {
			v.Set("pool."+name+"."+f.Key, f.get(c.Pools[name]))
		}
	}
	for i, t := range c.Targets {
		v.Set("target."+strconv.Itoa(i)+".pools", strings.Join(t.Pools, ", "))
	}
	return v
}

// settingsServer is the service with a config holding one org; goodToken passes its credential check.
func settingsServer(t *testing.T) (*Server, *httptest.Server, *int, string) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	cfg := config.Initial("pve1")
	cfg.Targets = []config.Target{{Org: "example-org", AppID: 1, InstallationID: 2, Pools: []string{"default"}}}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := 0
	s, _ := NewServer(cfg, path)
	s.Service = &Service{SecretDir: dir, Saved: func() { saved++ },
		Check: func(_ context.Context, auth string) error {
			if auth == goodToken {
				return nil
			}
			return errors.New("no")
		}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv, &saved, path
}

// save sends values as the app's settings save does.
func save(t *testing.T, srv *httptest.Server, values url.Values) (int, string) {
	t.Helper()
	m := map[string]string{}
	for k := range values {
		m[k] = values.Get(k)
	}
	code, b := apiDo(t, "PUT", srv.URL+"/api/v1/settings", goodToken, map[string]any{"values": m})
	return code, string(b)
}

// A browser gets a pointer to the app and nothing else: no login, no settings, no cookies.
func TestServiceServesNoSettingsPage(t *testing.T) {
	s, srv, saved, path := settingsServer(t)
	old := filepath.Join(s.Service.SecretDir, oldSessionFile)
	before, _ := os.ReadFile(path)
	c := client(t)
	resp, err := c.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), "Dell Server Tools app") || strings.Contains(string(b), "<form") ||
		len(resp.Cookies()) != 0 || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("/: %d %v\n%s", resp.StatusCode, resp.Cookies(), b)
	}
	for _, p := range []string{"/settings", "/login", "/logout", "/action/pull", "/app/new"} {
		if resp, _ := c.PostForm(srv.URL+p, url.Values{"org": {"example-org"}}); resp.StatusCode != http.StatusNotFound &&
			resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: %d", p, resp.StatusCode)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || *saved != 0 {
		t.Fatal("config changed without the API")
	}
	// The old page's login file goes when the service starts.
	os.WriteFile(old, []byte("{}"), 0o600)
	s.Handler()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old session file kept: %v", err)
	}
}

func TestSettingsSaveWritesConfigAndSecretsThenRestarts(t *testing.T) {
	s, srv, saved, path := settingsServer(t)
	form := currentValues(s.Cfg)
	form.Set("proxmox.max_runners", "8")
	form.Set("pool.default.max", "8")
	form.Set("network.allow", "udp/53, tcp/53@192.0.2.1")
	form.Set("logs.url", "https://logs.example.com")
	form.Set("logs.user", "123")
	form.Set("loki_token", "glc_secret")
	form.Set("new_pool", "big")
	form.Set("autoscale.enabled", "false")
	if code, b := save(t, srv, form); code != 200 || *saved != 1 {
		t.Fatalf("save: %d, restarts %d: %s", code, *saved, b)
	}
	got, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Proxmox.MaxRunners != 8 || got.Pools["default"].Max != 8 || got.Autoscale.Enabled || got.Logs.User != "123" ||
		got.Network.Allow[1].Dest != "192.0.2.1" || got.Pools["big"].Labels[0] != "big" || len(got.Targets) != 1 ||
		got.Pools["default"].Cores != 4 || got.Images.CheckInterval != s.Cfg.Images.CheckInterval ||
		got.GitHub.Pinned() {
		t.Fatalf("saved config: %+v %+v", got.Proxmox, got.Pools)
	}
	tok := got.Logs.TokenFile
	if b, err := os.ReadFile(tok); err != nil || string(b) != "glc_secret\n" {
		t.Fatalf("token file %s: %q %v", tok, b, err)
	}
	if fi, _ := os.Stat(tok); fi.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode %v", fi.Mode())
	}
	if data, _ := os.ReadFile(path); strings.Contains(string(data), "glc_secret") {
		t.Fatal("the token leaked into the config file")
	}
}

func TestSettingsRejectInvalidSaves(t *testing.T) {
	s, srv, saved, path := settingsServer(t)
	before, _ := os.ReadFile(path)
	form := currentValues(s.Cfg)
	form.Set("proxmox.max_runners", "100000")
	if code, b := save(t, srv, form); code != 422 || !strings.Contains(b, "max_runners") {
		t.Fatalf("too many runners: %d %s", code, b)
	}
	form.Set("proxmox.max_runners", "abc")
	if code, b := save(t, srv, form); code != 422 || !strings.Contains(b, "whole number") {
		t.Fatalf("not a number: %d %s", code, b)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || *saved != 0 {
		t.Fatal("config changed by a rejected save")
	}
}

func TestSettingsRemoveTargetAndDeletePool(t *testing.T) {
	s, srv, _, path := settingsServer(t)
	s.Cfg.Pools["extra"] = &config.Pool{Type: "lxc", Image: "ubuntu-latest", Mode: "ephemeral", Labels: []string{"x"},
		Cores: 1, MemoryMB: 512, DiskGB: 20, Max: 1}
	form := currentValues(s.Cfg)
	form.Set("pool.extra.delete", "true")
	form.Set("target.0.remove", "true")
	if code, b := save(t, srv, form); code != 200 {
		t.Fatalf("save: %d %s", code, b)
	}
	got, err := config.Load(path)
	if err != nil || len(got.Targets) != 0 || got.Pools["extra"] != nil {
		t.Fatalf("%v %+v", err, got)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"Logs (Grafana Loki)": "logs-grafana-loki", "This host": "this-host", "GitHub": "github"} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

// The CLI prints the report as it always has.
func TestStatusReportPrint(t *testing.T) {
	r := &StatusReport{Version: "build-7", Host: "pve1", Max: 4, Storage: "local-zfs", Pending: []string{"9001 (expired)"},
		Warnings: []string{"no gateway"}, Runners: []RunnerRow{{9000, "gh-pve1-default-9000", "running", "default", "example-org", "5s"}}}
	var b strings.Builder
	r.Print(&b)
	want := `pve-runners build-7 on pve1: 1/4 runners
ID    NAME                  STATUS   POOL     OWNER        AGE
9000  gh-pve1-default-9000  running  default  example-org  5s
pending cleanup: 9001 (expired)
container storage: local-zfs
warning: no gateway
`
	if b.String() != want {
		t.Fatalf("got\n%s\nwant\n%s", b.String(), want)
	}
	if r.UsedPercent() != 25 {
		t.Errorf("used %d%%", r.UsedPercent())
	}
}
