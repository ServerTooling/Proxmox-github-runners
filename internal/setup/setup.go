// Package setup serves the one-time setup page that connects this host to GitHub.
//
// For each org (or the personal account) the page creates a private GitHub App through GitHub's
// App-manifest flow, so every permission is pre-filled and the user only clicks "Create" and
// "Install" on github.com. The App key goes straight to a 0600 file; the page then writes the config
// and shuts down. It never stays listening.
package setup

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/github"
)

const RepoURL = "https://github.com/ServerTooling/proxmox-github-runners"

// Manifest builds the App manifest for one owner. Orgs get org-level runner permissions; a personal
// account has no org runners, so it gets repo administration (needed for repo-level runners).
func Manifest(name, baseURL string, org bool) map[string]any {
	perms := map[string]string{"metadata": "read", "actions": "read"}
	if org {
		perms["organization_self_hosted_runners"] = "write"
	} else {
		perms["administration"] = "write"
	}
	return map[string]any{
		"name":                name,
		"url":                 RepoURL,
		"public":              false, // installable only on the account that owns it
		"redirect_url":        baseURL + "/app/created",
		"setup_url":           baseURL + "/app/installed",
		"hook_attributes":     map[string]any{"url": RepoURL, "active": false}, // no webhooks: outbound only
		"default_permissions": perms,
		"default_events":      []string{},
	}
}

// AppName makes a GitHub App name (max 34 chars, globally unique) from host and owner plus a random
// suffix, so re-running setup or a second host never collides.
func AppName(hostID, owner string) string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	n := "pve-runners-" + hostID + "-" + owner
	if max := 34 - 7; len(n) > max {
		n = n[:max]
	}
	return n + "-" + hex.EncodeToString(b)
}

type appState struct {
	Owner string
	Org   bool
	ID    int64
	Slug  string
}

// Server holds setup state between the browser's round trips to GitHub.
type Server struct {
	Cfg        *config.Config
	ConfigPath string
	KeyDir     string // where App keys are written (config.KeyPath's directory)
	GitHubWeb  string // https://github.com
	API        string // https://api.github.com
	HTTP       *http.Client
	Token      string
	Done       chan struct{}
	// Storages are the node's storages that can hold containers, best first, offered for
	// proxmox.storage.
	Storages []string

	// Service, when set, makes this the always-on settings page instead of one-time setup.
	Service *Service

	mu       sync.Mutex
	apps     map[string]*appState // by owner (lowercase)
	errMsg   string
	sessions map[string]time.Time  // settings page logins: SHA-256 of session ID -> expiry
	runs     map[string]*actionRun // latest run of each settings page action
}

func NewServer(cfg *config.Config, configPath string) (*Server, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return &Server{Cfg: cfg, ConfigPath: configPath, KeyDir: filepath.Dir(config.KeyPath("x")),
		GitHubWeb: "https://github.com", API: cfg.GitHub.APIURL, HTTP: &http.Client{},
		Token: hex.EncodeToString(b), Done: make(chan struct{}), apps: map[string]*appState{},
		sessions: map[string]time.Time{}, runs: map[string]*actionRun{}}, nil
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/new", s.newApp)
	mux.HandleFunc("GET /app/created", s.appCreated)
	mux.HandleFunc("GET /app/installed", s.appInstalled)
	if s.Service != nil {
		mux.HandleFunc("GET /{$}", s.settings)
		return s.serviceHandler(mux)
	}
	mux.HandleFunc("GET /{$}", s.index)
	mux.HandleFunc("POST /host", s.saveHost)
	mux.HandleFunc("POST /finish", s.finish)
	return s.auth(mux)
}

const cookieName = "pve_runners_setup"

// auth requires the one-time token, first from the printed URL (?t=), then from a cookie, so that
// nobody else on the LAN can drive setup while it is open.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := r.URL.Query().Get("t")
		if tok == "" {
			if c, err := r.Cookie(cookieName); err == nil {
				tok = c.Value
			}
		}
		if subtle.ConstantTimeCompare([]byte(tok), []byte(s.Token)) != 1 {
			http.Error(w, "open the setup link printed by `pve-runners setup`", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
		next.ServeHTTP(w, r)
	})
}

func baseURL(r *http.Request) string {
	if r.TLS != nil {
		return "https://" + r.Host
	}
	return "http://" + r.Host
}

var ownerRe = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

func (s *Server) fail(w http.ResponseWriter, r *http.Request, msg string) {
	s.mu.Lock()
	s.errMsg = msg
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) saveHost(w http.ResponseWriter, r *http.Request) {
	// An empty template ID means the published ubuntu-latest image, which the service downloads.
	tpl, err1 := 0, error(nil)
	if v := strings.TrimSpace(r.FormValue("template")); v != "" {
		tpl, err1 = strconv.Atoi(v)
	}
	maxR, err2 := strconv.Atoi(r.FormValue("max_runners"))
	if err1 != nil || err2 != nil {
		s.fail(w, r, "Template ID and max runners must be numbers.")
		return
	}
	s.mu.Lock()
	s.Cfg.Proxmox.Node = strings.TrimSpace(r.FormValue("node"))
	s.Cfg.Proxmox.MaxRunners = maxR
	if b := strings.TrimSpace(r.FormValue("bridge")); b != "" {
		s.Cfg.Proxmox.Bridge = b
	}
	// Empty means pick automatically at run time.
	s.Cfg.Proxmox.Storage = strings.TrimSpace(r.FormValue("storage"))
	p := s.Cfg.Pools["default"]
	if p == nil {
		p = &config.Pool{Type: "lxc", Mode: "ephemeral", Labels: []string{"proxmox"}}
		s.Cfg.Pools["default"] = p
	}
	p.Template, p.Image = tpl, ""
	if tpl == 0 {
		p.Image = config.DefaultImage
	}
	s.errMsg = ""
	s.mu.Unlock()
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// newApp answers with a self-submitting form that posts the manifest to GitHub's "create App" page
// under the chosen org (or the user's own account when org is left empty).
func (s *Server) newApp(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.FormValue("org"))
	personal := owner == ""
	if personal {
		owner = strings.TrimSpace(r.FormValue("user"))
	}
	if !ownerRe.MatchString(owner) {
		s.fail(w, r, "Enter a GitHub org name, or your username for personal repositories.")
		return
	}
	hostID := s.Cfg.HostID
	if hostID == "" {
		hostID = s.Cfg.Proxmox.Node
	}
	manifest, _ := json.Marshal(Manifest(AppName(hostID, owner), baseURL(r), !personal))
	action := s.GitHubWeb + "/settings/apps/new"
	if !personal {
		action = s.GitHubWeb + "/organizations/" + url.PathEscape(owner) + "/settings/apps/new"
	}
	s.mu.Lock()
	s.apps[strings.ToLower(owner)] = &appState{Owner: owner, Org: !personal}
	s.mu.Unlock()
	render(w, redirectTmpl, map[string]any{"Action": action + "?state=" + url.QueryEscape(s.Token), "Manifest": string(manifest)})
}

func (s *Server) appCreated(w http.ResponseWriter, r *http.Request) {
	if subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("state")), []byte(s.Token)) != 1 {
		http.Error(w, "state mismatch", http.StatusBadRequest)
		return
	}
	creds, err := github.ConvertManifest(r.Context(), s.HTTP, s.API, r.URL.Query().Get("code"))
	if err != nil {
		s.fail(w, r, "GitHub did not accept the App code: "+err.Error())
		return
	}
	s.mu.Lock()
	app := s.apps[strings.ToLower(creds.Owner.Login)]
	s.mu.Unlock()
	if app == nil {
		s.fail(w, r, "GitHub created the App under "+creds.Owner.Login+", which setup did not ask for. Delete it on GitHub and try again.")
		return
	}
	if err := os.MkdirAll(s.KeyDir, 0o700); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	keyPath := filepath.Join(s.KeyDir, strings.ToLower(creds.Owner.Login)+".pem")
	if err := os.WriteFile(keyPath, []byte(creds.PEM), 0o600); err != nil {
		s.fail(w, r, err.Error())
		return
	}
	s.mu.Lock()
	app.ID, app.Slug = creds.ID, creds.Slug
	s.mu.Unlock()
	http.Redirect(w, r, s.GitHubWeb+"/apps/"+url.PathEscape(creds.Slug)+"/installations/new", http.StatusSeeOther)
}

// appInstalled finds which of this session's Apps the installation belongs to and adds a target.
func (s *Server) appInstalled(w http.ResponseWriter, r *http.Request) {
	instID, err := strconv.ParseInt(r.URL.Query().Get("installation_id"), 10, 64)
	if err != nil {
		s.fail(w, r, "GitHub did not return an installation.")
		return
	}
	s.mu.Lock()
	apps := make([]appState, 0, len(s.apps))
	for _, a := range s.apps {
		if a.ID != 0 {
			apps = append(apps, *a)
		}
	}
	s.mu.Unlock()
	for _, a := range apps {
		pem, err := os.ReadFile(filepath.Join(s.KeyDir, strings.ToLower(a.Owner)+".pem"))
		if err != nil {
			continue
		}
		key, err := github.ParsePrivateKey(pem)
		if err != nil {
			continue
		}
		c := github.NewClient(s.API, a.ID, key)
		c.HTTP = s.HTTP
		insts, err := c.Installations(r.Context())
		if err != nil {
			continue
		}
		for _, in := range insts {
			if in.ID != instID {
				continue
			}
			if err := s.addTarget(r.Context(), c, a, in); err != nil {
				s.fail(w, r, err.Error())
				return
			}
			if s.Service != nil {
				s.saveNow(w, r)
				return
			}
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
	}
	s.fail(w, r, "That installation does not belong to an App created by this setup session.")
}

func (s *Server) addTarget(ctx context.Context, c *github.Client, a appState, in github.Installation) error {
	t := config.Target{AppID: a.ID, AppSlug: a.Slug, InstallationID: in.ID, Pools: []string{"default"},
		PrivateKeyFile: filepath.Join(s.KeyDir, strings.ToLower(a.Owner)+".pem")}
	// The org App asks for org runner permissions, which mean nothing on a personal account (and
	// the reverse lacks them), so the account type must match the button that was used.
	if a.Org && in.Account.Type == "User" {
		return fmt.Errorf("%s is a personal account, not an org; remove this App and use \"Create App for my account\" instead", in.Account.Login)
	}
	if !a.Org && in.Account.Type == "Organization" {
		return fmt.Errorf("%s is an org; remove this App and use \"Create App for org\" instead", in.Account.Login)
	}
	if a.Org {
		t.Org = in.Account.Login
	} else {
		// Personal accounts only get repo-level runners, and only for private repos.
		t.User = in.Account.Login
		repos, err := c.InstallationRepos(ctx, in.ID)
		if err != nil {
			return err
		}
		for _, r := range repos {
			if r.Private {
				t.Repos = append(t.Repos, r.Name)
			}
		}
		if len(t.Repos) == 0 {
			return errors.New("the App was installed on " + t.User + " but can see no private repositories; public ones are not served")
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, ex := range s.Cfg.Targets {
		if strings.EqualFold(ex.Owner(), t.Owner()) {
			s.Cfg.Targets[i] = t
			return nil
		}
	}
	s.Cfg.Targets = append(s.Cfg.Targets, t)
	return nil
}

func (s *Server) finish(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := marshalValidate(s.Cfg)
	if err != nil {
		s.errMsg = err.Error()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	if err := data.Save(s.ConfigPath); err != nil {
		s.errMsg = err.Error()
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	render(w, doneTmpl, map[string]any{"Path": s.ConfigPath})
	select {
	case <-s.Done:
	default:
		close(s.Done)
	}
}

// saveNow writes the current config from the settings page (after adding an org) and restarts.
func (s *Server) saveNow(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	err := s.Cfg.Validate()
	if err == nil {
		err = s.Cfg.Save(s.ConfigPath)
	}
	s.mu.Unlock()
	if err != nil {
		s.fail(w, r, err.Error())
		return
	}
	render(w, savedTmpl, nil)
	if s.Service.Saved != nil {
		s.Service.Saved()
	}
}

func marshalValidate(c *config.Config) (*config.Config, error) {
	if len(c.Targets) == 0 {
		return nil, errors.New("add at least one org or account before finishing")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tpl := 0
	if p := s.Cfg.Pools["default"]; p != nil {
		tpl = p.Template
	}
	render(w, indexTmpl, map[string]any{"Cfg": s.Cfg, "Template": tpl, "Error": s.errMsg, "Storages": s.Storages})
}

func render(w http.ResponseWriter, t *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := t.Execute(w, data); err != nil {
		http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
	}
}

var indexTmpl = template.Must(template.New("index").Parse(page + `
<h1>pve-runners setup</h1>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="/host"><fieldset><legend>1. This Proxmox host</legend>
<p><label>Node name <input name="node" value="{{.Cfg.Proxmox.Node}}" required></label></p>
<p><label>Runner template container ID <input name="template" value="{{if .Template}}{{.Template}}{{end}}" size="6" placeholder="optional"></label><br>
<small>Leave empty to use the published <code>ubuntu-latest</code> image (Python, Go, Node, Helm, kubectl, Docker), which is downloaded automatically.
Or enter a container you prepared with <code>pve-runners template prepare</code>, outside IDs {{index .Cfg.Proxmox.VMIDRange 0}}-{{index .Cfg.Proxmox.VMIDRange 1}}.</small></p>
<p><label>Network bridge for runners <input name="bridge" value="{{.Cfg.Proxmox.Bridge}}" size="8"></label><br>
<small>Runners get internet access through this bridge; the LAN stays blocked by the firewall.</small></p>
<p><label>Storage for container disks <input name="storage" value="{{.Cfg.Proxmox.Storage}}" size="12" list="storages" placeholder="automatic"></label><br>
<datalist id="storages">{{range .Storages}}<option value="{{.}}">{{end}}</datalist>
<small>{{if .Storages}}Storages here that can hold containers: {{range $i, $s := .Storages}}{{if $i}}, {{end}}<code>{{$s}}</code>{{end}}.
Leave empty to use <code>{{index .Storages 0}}</code>.{{else}}Leave empty to pick one that can hold containers automatically.{{end}}</small></p>
<p><label>Max runners at once <input name="max_runners" value="{{.Cfg.Proxmox.MaxRunners}}" size="3"></label></p>
<button>Save</button></fieldset></form>

<fieldset><legend>2. Connect GitHub</legend>
<p>For each org you manage, create its own private GitHub App, then install it. Only private repositories are served.</p>
<form method="post" action="/app/new"><label>Org <input name="org" placeholder="your-org"></label> <button>Create App for org</button></form>
<form method="post" action="/app/new"><p><small>Personal repositories instead:</small><br><label>Your username <input name="user"></label> <button>Create App for my account</button></p></form>
{{if .Cfg.Targets}}<p>Connected:</p><ul>{{range .Cfg.Targets}}<li>{{if .Org}}org <b>{{.Org}}</b> (all private repos){{else}}user <b>{{.User}}</b>: {{range .Repos}}{{.}} {{end}}{{end}}</li>{{end}}</ul>{{end}}
</fieldset>

<form method="post" action="/finish"><fieldset><legend>3. Finish</legend>
<p>Writes the config and closes this page. Workflows use <code>runs-on: proxmox</code>.</p><button>Finish setup</button></fieldset></form>
</body></html>`))

var redirectTmpl = template.Must(template.New("redirect").Parse(page + `
<p>Sending you to GitHub to create the App...</p>
<form id="f" method="post" action="{{.Action}}"><input type="hidden" name="manifest" value="{{.Manifest}}"><noscript><button>Continue to GitHub</button></noscript></form>
<script>document.getElementById('f').submit()</script></body></html>`))

var doneTmpl = template.Must(template.New("done").Parse(page + `
<h1>Setup complete</h1><p>Config written to <code>{{.Path}}</code>. You can close this page; the service starts now.</p></body></html>`))

// OrderAddrs puts the preferred addresses (in their order, when the host has them) ahead of the
// rest, so the first setup link is the one most likely to load: the Proxmox web UI's address.
func OrderAddrs(addrs, preferred []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, p := range preferred {
		if p != "" && !seen[p] && slices.Contains(addrs, p) {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, a := range addrs {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	return out
}
