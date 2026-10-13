package setup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
)

// In the service the server is the JSON API the DellServerTools app uses for status, settings and
// actions (api.go). A browser only gets a note at / pointing to the app, and the GitHub App pages for
// adding an org or account, which start from a one-time link the app asks the API for: creating the App
// happens on github.com, so it needs a browser where the user is logged in to GitHub.

// Service turns the server into the always-on API.
type Service struct {
	// Check accepts the app's Proxmox credentials for the JSON API (see api.go).
	Check CheckFunc
	// Saved runs after a successful save, to restart the service with the new settings.
	Saved func()
	// SecretDir holds the files secrets are written to (the notify URL and the Loki token).
	SecretDir string
	// FailDelay slows down wrong credentials.
	FailDelay time.Duration
	// Status reports what `pve-runners status` does, for the app's overview.
	Status func(ctx context.Context) (*StatusReport, error)
	// Actions are the CLI commands the app offers as buttons.
	Actions []Action
	// Ctx ends running actions when the service stops.
	Ctx context.Context
}

// oldSessionFile held the logins of the browser settings page this replaced; nothing reads it now.
const oldSessionFile = "web-sessions.json"

func (s *Server) serviceHandler(mux *http.ServeMux) http.Handler {
	if s.Service.SecretDir != "" {
		_ = os.Remove(filepath.Join(s.Service.SecretDir, oldSessionFile))
	}
	mux.HandleFunc("GET /{$}", s.servicePage)
	mux.HandleFunc("GET /app/new", s.newAppFromLink)
	api := s.apiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		w.Header().Set("X-Frame-Options", "DENY")
		mux.ServeHTTP(w, r)
	})
}

// servicePage tells a browser where the settings went, and shows why adding an org failed, since the
// GitHub App pages send errors back here.
func (s *Server) servicePage(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	msg := s.errMsg
	s.errMsg = ""
	s.mu.Unlock()
	render(w, serviceTmpl, map[string]any{"Host": s.Cfg.HostID, "Error": msg})
}

// slug turns a section title into an anchor, such as "Logs (Grafana Loki)" into "logs-grafana-loki".
func slug(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			b.WriteRune(r)
			dash = false
		} else {
			dash = true
		}
	}
	return b.String()
}

func (s *Server) apply(form url.Values) (*config.Config, error) {
	c, err := clone(s.Cfg)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, sec := range hostFields() {
		for _, f := range sec.Fields {
			errs = append(errs, f.set(c, strings.TrimSpace(form.Get(f.Name))))
		}
	}
	for _, name := range c.PoolNames() {
		if checked(form.Get("pool." + name + ".delete")) {
			delete(c.Pools, name)
			continue
		}
		for _, f := range poolFields() {
			errs = append(errs, f.set(c.Pools[name], strings.TrimSpace(form.Get("pool."+name+"."+f.Key))))
		}
	}
	if name := strings.TrimSpace(form.Get("new_pool")); name != "" {
		if _, ok := c.Pools[name]; ok {
			errs = append(errs, fmt.Errorf("pool %q already exists", name))
		} else {
			c.Pools[name] = &config.Pool{Image: config.DefaultImage, Labels: []string{name}}
		}
	}
	var targets []config.Target
	for i, t := range c.Targets {
		key := fmt.Sprintf("target.%d.", i)
		if checked(form.Get(key + "remove")) {
			continue
		}
		t.Pools = splitList(form.Get(key + "pools"))
		t.RunnerGroup = strings.TrimSpace(form.Get(key + "runner_group"))
		if t.User != "" {
			t.Repos = splitList(form.Get(key + "repos"))
		}
		targets = append(targets, t)
	}
	c.Targets = targets
	errs = append(errs, s.secret(form, "notify_url", "notify-url", &c.Notify.WebhookURLFile))
	errs = append(errs, s.secret(form, "loki_token", "loki-token", &c.Logs.TokenFile))
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	// Render and parse again: that fills defaults for emptied fields and validates the whole config.
	data, err := c.Render()
	if err != nil {
		return nil, err
	}
	next, _, err := config.Parse(data)
	return next, err
}

// secret writes a newly entered secret to its own 0600 file and points the config at it. A blank
// input keeps the current one; the clear box removes it from the config.
func (s *Server) secret(form url.Values, name, file string, path *string) error {
	if checked(form.Get(name + "_clear")) {
		*path = ""
		return nil
	}
	v := strings.TrimSpace(form.Get(name))
	if v == "" {
		return nil
	}
	if *path == "" {
		*path = filepath.Join(s.Service.SecretDir, file)
	}
	if err := os.MkdirAll(filepath.Dir(*path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(*path, []byte(v+"\n"), 0o600)
}

func clone(c *config.Config) (*config.Config, error) {
	data, err := c.Render()
	if err != nil {
		return nil, err
	}
	out, _, err := config.Parse(data)
	return out, err
}
