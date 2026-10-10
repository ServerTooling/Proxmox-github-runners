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
	"strings"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
)

// browserForm is what the settings form posts unchanged: every field's current value, with checkboxes
// present only when ticked.
func browserForm(c *config.Config) url.Values {
	v := url.Values{}
	for _, sec := range hostFields() {
		for _, f := range sec.Fields {
			if val := f.get(c); f.Kind != "bool" {
				v.Set(f.Name, val)
			} else if val == "true" {
				v.Set(f.Name, "on")
			}
		}
	}
	for _, name := range c.PoolNames() {
		for _, f := range poolFields() {
			if val := f.get(c.Pools[name]); f.Kind != "bool" {
				v.Set("pool."+name+"."+f.Key, val)
			} else if val == "true" {
				v.Set("pool."+name+"."+f.Key, "on")
			}
		}
	}
	for i, t := range c.Targets {
		v.Set("target."+string(rune('0'+i))+".pools", strings.Join(t.Pools, ", "))
	}
	return v
}

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
		Login: func(_ context.Context, user, pw string) error {
			if user == "root@pam" && pw == "pw" {
				return nil
			}
			return errors.New("no")
		}}
	srv := httptest.NewServer(s.Handler())
	t.Cleanup(srv.Close)
	return s, srv, &saved, path
}

func TestSettingsNeedAProxmoxLogin(t *testing.T) {
	_, srv, _, _ := settingsServer(t)
	c := client(t)
	if resp, _ := c.Get(srv.URL + "/"); resp.Header.Get("Location") != "/login" {
		t.Fatalf("not logged in: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// The one-time setup token is not a way in.
	if resp, _ := c.PostForm(srv.URL+"/settings", url.Values{}); resp.Header.Get("Location") != "/login" {
		t.Fatalf("save without login: %d", resp.StatusCode)
	}
	if resp, _ := c.PostForm(srv.URL+"/login", url.Values{"user": {"root"}, "password": {"nope"}}); resp.Header.Get("Location") != "/login?e=1" {
		t.Fatalf("wrong password: %q", resp.Header.Get("Location"))
	}
	// A bare user name means the pam realm, as in the Proxmox UI.
	if resp, _ := c.PostForm(srv.URL+"/login", url.Values{"user": {"root"}, "password": {"pw"}}); resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %q", resp.Header.Get("Location"))
	}
	resp, _ := c.Get(srv.URL + "/")
	body, _ := io.ReadAll(resp.Body)
	for _, want := range []string{`name="proxmox.max_runners" value="4"`, `name="pool.default.cores" value="4"`, "Org example-org",
		`name="loki_token"`} {
		if !strings.Contains(string(body), want) {
			t.Errorf("page lacks %s", want)
		}
	}
	c.PostForm(srv.URL+"/logout", nil)
	if resp, _ := c.Get(srv.URL + "/"); resp.Header.Get("Location") != "/login" {
		t.Fatal("still logged in after logout")
	}
}

func TestSettingsSaveWritesConfigAndSecretsThenRestarts(t *testing.T) {
	s, srv, saved, path := settingsServer(t)
	c := client(t)
	c.PostForm(srv.URL+"/login", url.Values{"user": {"root@pam"}, "password": {"pw"}})

	form := browserForm(s.Cfg)
	form.Set("proxmox.max_runners", "8")
	form.Set("pool.default.max", "8")
	form.Set("network.allow", "udp/53, tcp/53@192.0.2.1")
	form.Set("logs.url", "https://logs.example.com")
	form.Set("logs.user", "123")
	form.Set("loki_token", "glc_secret")
	form.Set("new_pool", "big")
	form.Del("autoscale.enabled") // unticked
	resp, _ := c.PostForm(srv.URL+"/settings", form)
	if resp.StatusCode != 200 || *saved != 1 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("save: %d, restarts %d, error %q\n%s", resp.StatusCode, *saved, s.errMsg, b)
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
	// The page shows the token is set, never the token itself.
	resp, _ = c.Get(srv.URL + "/")
	if b, _ := io.ReadAll(resp.Body); strings.Contains(string(b), "glc_secret") || !strings.Contains(string(b), "set; leave empty to keep") {
		t.Fatal("secret shown on the page, or its state missing")
	}
}

func TestSettingsRejectInvalidAndCrossSiteSaves(t *testing.T) {
	s, srv, saved, path := settingsServer(t)
	before, _ := os.ReadFile(path)
	c := client(t)
	c.PostForm(srv.URL+"/login", url.Values{"user": {"root@pam"}, "password": {"pw"}})

	form := browserForm(s.Cfg)
	form.Set("proxmox.max_runners", "100000")
	c.PostForm(srv.URL+"/settings", form)
	resp, _ := c.Get(srv.URL + "/")
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "max_runners") || *saved != 0 {
		t.Fatalf("invalid save accepted (restarts %d)", *saved)
	}
	form.Set("proxmox.max_runners", "abc")
	c.PostForm(srv.URL+"/settings", form)
	if !strings.Contains(s.errMsg, "whole number") {
		t.Fatalf("error %q", s.errMsg)
	}

	req, _ := http.NewRequest("POST", srv.URL+"/settings", strings.NewReader(browserForm(s.Cfg).Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := c.Do(req); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site save: %d", resp.StatusCode)
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) || *saved != 0 {
		t.Fatal("config changed by a rejected save")
	}
}

func TestSettingsRemoveTargetAndDeletePool(t *testing.T) {
	s, srv, _, path := settingsServer(t)
	s.Cfg.Pools["extra"] = &config.Pool{Type: "lxc", Image: "ubuntu-latest", Mode: "ephemeral", Labels: []string{"x"},
		Cores: 1, MemoryMB: 512, DiskGB: 20, Max: 1}
	c := client(t)
	c.PostForm(srv.URL+"/login", url.Values{"user": {"root@pam"}, "password": {"pw"}})
	form := browserForm(s.Cfg)
	form.Set("pool.extra.delete", "on")
	form.Set("target.0.remove", "on")
	c.PostForm(srv.URL+"/settings", form)
	got, err := config.Load(path)
	if err != nil || len(got.Targets) != 0 || got.Pools["extra"] != nil {
		t.Fatalf("%v %q %+v", err, s.errMsg, got)
	}
}

func TestActionsRunInTheBackgroundAndShowOutput(t *testing.T) {
	s, srv, _, _ := settingsServer(t)
	release := make(chan struct{})
	runs := 0
	s.Service.Status = func(_ context.Context, w io.Writer) error { _, err := io.WriteString(w, "2/4 runners"); return err }
	s.Service.Actions = []Action{{Name: "pull", Label: "Pull images", Args: []string{"pool"},
		Run: func(_ context.Context, w io.Writer, args map[string]string) error {
			runs++
			io.WriteString(w, "pulling for "+args["pool"]+"\n")
			<-release
			return errors.New("disk full")
		}}}
	c := client(t)
	c.PostForm(srv.URL+"/login", url.Values{"user": {"root@pam"}, "password": {"pw"}})
	if resp, _ := c.PostForm(srv.URL+"/action/nope", nil); resp.StatusCode != 404 {
		t.Fatalf("unknown action: %d", resp.StatusCode)
	}
	if resp, _ := c.PostForm(srv.URL+"/action/pull", url.Values{"pool": {"default"}}); resp.Header.Get("Location") != "/#action-pull" {
		t.Fatalf("start: %q", resp.Header.Get("Location"))
	}
	page := func() string {
		resp, _ := c.Get(srv.URL + "/")
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	waitFor := func(want string) {
		deadline := time.Now().Add(5 * time.Second)
		for !strings.Contains(page(), want) {
			if time.Now().After(deadline) {
				t.Fatalf("page never showed %q:\n%s", want, page())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitFor("pulling for default")
	// While it runs, the page refreshes itself and a second click starts nothing.
	if p := page(); !strings.Contains(p, `http-equiv="refresh"`) || !strings.Contains(p, "running since") || !strings.Contains(p, "2/4 runners") {
		t.Fatalf("running page:\n%s", p)
	}
	c.PostForm(srv.URL+"/action/pull", url.Values{"pool": {"default"}})
	close(release)
	waitFor("error: disk full")
	if runs != 1 {
		t.Fatalf("ran %d times", runs)
	}
}
