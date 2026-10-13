package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// The settings page is the setup page served permanently by the service. Instead of the one-time
// setup token it asks for a Proxmox login, since anyone who can change these settings controls a root
// service; Login checks the credentials with the local Proxmox API.

const sessionCookie = "pve_runners_session"

// sessionTTL is how long a login lasts without being used; each visit extends it, so a browser that
// keeps using the page stays logged in until it logs out. Logins survive restarts (see sessions.go).
const sessionTTL = 7 * 24 * time.Hour

// LoginFunc returns nil when user and password are a Proxmox account allowed to change this host.
type LoginFunc func(ctx context.Context, user, password string) error

// Service turns the server into the always-on settings page.
type Service struct {
	Login LoginFunc
	// Check accepts the app's Proxmox credentials for the JSON API (see api.go).
	Check CheckFunc
	// Saved runs after a successful save, to restart the service with the new settings.
	Saved func()
	// SecretDir holds the files the page writes secrets to (the notify URL and the Loki token).
	SecretDir string
	// FailDelay slows down wrong passwords.
	FailDelay time.Duration
	// Status reports what `pve-runners status` does, shown on the page's overview.
	Status func(ctx context.Context) (*StatusReport, error)
	// Actions are the CLI commands offered as buttons.
	Actions []Action
	// Ctx ends running actions when the service stops.
	Ctx context.Context
}

func (s *Server) serviceHandler(mux *http.ServeMux) http.Handler {
	s.loadSessions(time.Now())
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("POST /settings", s.saveSettings)
	mux.HandleFunc("POST /action/{name}", s.runAction)
	api := s.apiHandler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && !sameOrigin(r) {
			http.Error(w, "cross-site request refused", http.StatusForbidden)
			return
		}
		if r.URL.Path == "/login" || s.validSession(w, r) {
			w.Header().Set("X-Frame-Options", "DENY")
			mux.ServeHTTP(w, r)
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}

// sameOrigin refuses form posts from other sites. The session cookie is SameSite=Lax, which already
// keeps it off cross-site posts; this also covers browsers that ignore SameSite.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		o = r.Header.Get("Referer")
	}
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host == r.Host
}

func (s *Server) validSession(w http.ResponseWriter, r *http.Request) bool {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	key, now := sessionKey(c.Value), time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[key]
	if !ok {
		return false
	}
	if now.After(exp) {
		delete(s.sessions, key)
		_ = s.saveSessionsLocked()
		return false
	}
	if exp.Sub(now) < sessionTTL-sessionRenew {
		s.sessions[key] = now.Add(sessionTTL)
		_ = s.saveSessionsLocked()
		setSessionCookie(w, r, c.Value)
	}
	return true
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, id string) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil,
		SameSite: http.SameSiteLaxMode, MaxAge: int(sessionTTL.Seconds())})
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	render(w, loginTmpl, map[string]any{"Error": r.URL.Query().Get("e") != "", "Host": s.Cfg.Proxmox.Node})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	user := strings.TrimSpace(r.FormValue("user"))
	if user != "" && !strings.Contains(user, "@") {
		user += "@pam"
	}
	if err := s.Service.Login(r.Context(), user, r.FormValue("password")); err != nil {
		time.Sleep(s.Service.FailDelay)
		http.Redirect(w, r, "/login?e=1", http.StatusSeeOther)
		return
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id := hex.EncodeToString(b)
	s.mu.Lock()
	s.sessions[sessionKey(id)] = time.Now().Add(sessionTTL)
	_ = s.saveSessionsLocked()
	s.mu.Unlock()
	setSessionCookie(w, r, id)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.mu.Lock()
		delete(s.sessions, sessionKey(c.Value))
		_ = s.saveSessionsLocked()
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

type fieldView struct {
	Name, Label, Help, Kind, Value string
	Options                        []string
}

type poolView struct {
	Name   string
	Fields []fieldView
}

type sectionView struct {
	ID, Title string
	Fields    []fieldView
}

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	report, statusErr := s.status(r.Context())
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Cfg
	var sections []sectionView
	for _, sec := range hostFields() {
		v := sectionView{ID: "s-" + slug(sec.Title), Title: sec.Title}
		for _, f := range sec.Fields {
			v.Fields = append(v.Fields, fieldView{f.Name, f.Label, f.Help, f.Kind, f.get(c), f.Options})
		}
		sections = append(sections, v)
	}
	var pools []poolView
	for _, name := range c.PoolNames() {
		pv := poolView{Name: name}
		for _, f := range poolFields() {
			pv.Fields = append(pv.Fields, fieldView{"pool." + name + "." + f.Key, f.Label, "", f.Kind, f.get(c.Pools[name]), f.Options})
		}
		pools = append(pools, pv)
	}
	actions, busy := s.actionViews()
	render(w, settingsTmpl, map[string]any{"Cfg": c, "Sections": sections, "Pools": pools, "Error": s.errMsg,
		"Report": report, "StatusErr": statusErr, "Actions": actions, "Busy": busy,
		"NotifySet": c.Notify.WebhookURLFile != "", "LokiSet": c.Logs.TokenFile != ""})
	s.errMsg = ""
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

// saveSettings applies the whole form to a copy of the config, validates it, writes it and any new
// secrets, then restarts the service. Nothing changes on disk unless the result is valid.
func (s *Server) saveSettings(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	s.mu.Lock()
	next, err := s.apply(r.PostForm)
	if err == nil {
		err = next.Save(s.ConfigPath)
	}
	if err != nil {
		s.errMsg = err.Error()
		s.mu.Unlock()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	s.Cfg = next
	s.mu.Unlock()
	// Saved only schedules the restart, so it runs before the response: the browser (and a test) may
	// act on the "saved" page as soon as it arrives.
	if s.Service.Saved != nil {
		s.Service.Saved()
	}
	render(w, savedTmpl, nil)
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
