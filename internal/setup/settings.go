package setup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
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
	// Saved runs after a successful save, to restart the service with the new settings.
	Saved func()
	// SecretDir holds the files the page writes secrets to (the notify URL and the Loki token).
	SecretDir string
	// FailDelay slows down wrong passwords.
	FailDelay time.Duration
	// Status prints what `pve-runners status` does, shown at the top of the page.
	Status func(ctx context.Context, w io.Writer) error
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

func (s *Server) settings(w http.ResponseWriter, r *http.Request) {
	status := s.statusText(r.Context())
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Cfg
	var sections []struct {
		Title  string
		Fields []fieldView
	}
	for _, sec := range hostFields() {
		v := struct {
			Title  string
			Fields []fieldView
		}{Title: sec.Title}
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
		"Status": status, "Actions": actions, "Busy": busy,
		"NotifySet": c.Notify.WebhookURLFile != "", "LokiSet": c.Logs.TokenFile != ""})
	s.errMsg = ""
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
	render(w, savedTmpl, nil)
	if s.Service.Saved != nil {
		s.Service.Saved()
	}
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
		if form.Get("pool."+name+".delete") == "on" {
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
		if form.Get(key+"remove") == "on" {
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
	if form.Get(name+"_clear") == "on" {
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

var loginTmpl = template.Must(template.New("login").Parse(page + `
<h1>pve-runners on {{.Host}}</h1>
{{if .Error}}<p class="err">Wrong user or password, or the account cannot change this host (it needs Sys.Modify).</p>{{end}}
<form method="post" action="/login"><fieldset><legend>Log in with your Proxmox account</legend>
<p><label>User <input name="user" value="root@pam" autocomplete="username" required></label></p>
<p><label>Password <input name="password" type="password" autocomplete="current-password" required></label></p>
<button>Log in</button></fieldset></form></body></html>`))

var savedTmpl = template.Must(template.New("saved").Parse(page + `<meta http-equiv="refresh" content="15;url=/">
<h1>Saved</h1><p>The service is restarting with the new settings. This page reloads in 15 seconds.</p></body></html>`))

var settingsTmpl = template.Must(template.New("settings").Parse(page + `{{if .Busy}}<meta http-equiv="refresh" content="5">{{end}}
<form method="post" action="/logout" style="float:right"><button>Log out</button></form>
<h1>pve-runners</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
{{if .Status}}<fieldset><legend>Status</legend><pre>{{.Status}}</pre></fieldset>{{end}}
<fieldset id="actions"><legend>Actions</legend>
{{range .Actions}}<form method="post" action="/action/{{.Name}}" id="action-{{.Name}}"{{if .Confirm}} onsubmit="return confirm('{{.Confirm}}')"{{end}}>
<p>{{range .Args}}<label>{{.}} <input name="{{.}}" size="10"></label> {{end}}<button{{if and .Last .Last.Running}} disabled{{end}}>{{.Label}}</button>
{{if .Help}}<br><small>{{.Help}}</small>{{end}}</p>
{{with .Last}}<p><small>{{if .Running}}running since {{.Started}}…{{else}}ran {{.Started}}-{{.Ended}}{{if .Err}}: <b>failed</b>{{end}}{{end}}</small></p>
{{if or .Output .Err}}<pre>{{.Output}}{{if .Err}}error: {{.Err}}{{end}}</pre>{{end}}{{end}}
</form>{{end}}</fieldset>
<h2>Settings</h2>
<fieldset><legend>GitHub orgs and accounts</legend>
<p>Each has its own private GitHub App. Adding one saves and restarts right away.</p>
<form method="post" action="/app/new"><label>Org <input name="org" placeholder="your-org"></label> <button>Add org</button></form>
<form method="post" action="/app/new"><p><label>Or your username <input name="user"></label> <button>Add my account</button></p></form>
</fieldset>
<form method="post" action="/settings">
{{range $i, $t := .Cfg.Targets}}<fieldset><legend>{{if .Org}}Org {{.Org}}{{else}}User {{.User}}{{end}}</legend>
<p><label>Pools <input name="target.{{$i}}.pools" value="{{range $j, $p := .Pools}}{{if $j}}, {{end}}{{$p}}{{end}}"></label></p>
{{if .Org}}<p><label>Runner group <input name="target.{{$i}}.runner_group" value="{{.RunnerGroup}}" placeholder="Default"></label></p>
{{else}}<p><label>Repositories <input name="target.{{$i}}.repos" value="{{range $j, $r := .Repos}}{{if $j}}, {{end}}{{$r}}{{end}}"></label></p>{{end}}
<p><label><input type="checkbox" name="target.{{$i}}.remove"> Remove (the GitHub App stays until you delete it on GitHub)</label></p>
</fieldset>{{end}}
{{range .Sections}}<fieldset><legend>{{.Title}}</legend>{{range .Fields}}{{template "field" .}}{{end}}
{{if eq .Title "Alerts"}}<p><label>Webhook URL <input name="notify_url" type="password" autocomplete="off"
 placeholder="{{if $.NotifySet}}set; leave empty to keep{{else}}ntfy, Discord or Slack URL{{end}}"></label>
{{if $.NotifySet}}<label><input type="checkbox" name="notify_url_clear"> turn off</label>{{end}}</p>{{end}}
{{if eq .Title "Logs (Grafana Loki)"}}<p><label>Token <input name="loki_token" type="password" autocomplete="off"
 placeholder="{{if $.LokiSet}}set; leave empty to keep{{else}}access policy token with logs:write{{end}}"></label>
{{if $.LokiSet}}<label><input type="checkbox" name="loki_token_clear"> remove</label>{{end}}</p>{{end}}
</fieldset>{{end}}
{{range .Pools}}<fieldset><legend>Pool {{.Name}}</legend>{{range .Fields}}{{template "field" .}}{{end}}
<p><label><input type="checkbox" name="pool.{{.Name}}.delete"> Delete this pool</label></p></fieldset>{{end}}
<fieldset><legend>New pool</legend><p><label>Name <input name="new_pool" placeholder="optional"></label><br>
<small>Starts from the published image with its name as the label; edit it after saving.</small></p></fieldset>
<p><button>Save and restart</button></p>
</form></body></html>
{{define "field"}}<p><label>{{.Label}} {{if eq .Kind "bool"}}<input type="checkbox" name="{{.Name}}"{{if eq .Value "true"}} checked{{end}}>
{{else if eq .Kind "select"}}<select name="{{.Name}}">{{$v := .Value}}{{range .Options}}<option{{if eq . $v}} selected{{end}}>{{.}}</option>{{end}}</select>
{{else}}<input name="{{.Name}}" value="{{.Value}}"{{if eq .Kind "number"}} inputmode="numeric" size="8"{{end}}>{{end}}</label>
{{if .Help}}<br><small>{{.Help}}</small>{{end}}</p>{{end}}`))
