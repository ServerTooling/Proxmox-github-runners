package setup

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The JSON API under /api/v1 serves the DellServerTools app the same status, settings and actions as
// the page. It takes no cookies: every request carries the app's Proxmox credentials in the
// Authorization header, which a browser never sends to another site on its own, so there is nothing
// to forge cross-site and no session to steal.

// apiCheckTTL is how long an accepted credential is trusted before Proxmox is asked again, so that an
// app polling a running action doesn't hit pveproxy on every request. A revoked token or logged-out
// ticket stops working within this time.
const apiCheckTTL = time.Minute

// apiMaxBody bounds a settings save; the whole config is a few kilobytes.
const apiMaxBody = 1 << 20

// linkTTL is how long a link to add an org or account works; it is opened right after it is made.
const linkTTL = 15 * time.Minute

func (s *Server) apiHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/status", s.apiStatus)
	mux.HandleFunc("GET /api/v1/settings", s.apiSettings)
	mux.HandleFunc("PUT /api/v1/settings", s.apiSaveSettings)
	mux.HandleFunc("GET /api/v1/actions", s.apiActions)
	mux.HandleFunc("GET /api/v1/actions/{name}", s.apiAction)
	mux.HandleFunc("POST /api/v1/actions/{name}", s.apiRunAction)
	mux.HandleFunc("POST /api/v1/targets", s.apiAddTarget)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if !s.apiAuthorized(r) {
			time.Sleep(s.Service.FailDelay)
			apiError(w, http.StatusUnauthorized, "a Proxmox API token or ticket with Sys.Modify is needed")
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) apiAuthorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	if auth == "" || s.Service.Check == nil {
		return false
	}
	key, now := credKey(auth), time.Now()
	s.mu.Lock()
	until, ok := s.apiOK[key]
	s.mu.Unlock()
	if ok && now.Before(until) {
		return true
	}
	if s.Service.Check(r.Context(), auth) != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, u := range s.apiOK {
		if now.After(u) {
			delete(s.apiOK, k)
		}
	}
	s.apiOK[key] = now.Add(apiCheckTTL)
	return true
}

// credKey keeps accepted credentials in memory only as a hash.
func credKey(auth string) string {
	sum := sha256.Sum256([]byte(auth))
	return hex.EncodeToString(sum[:])
}

func apiJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func apiError(w http.ResponseWriter, status int, msg string) {
	apiJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) apiStatus(w http.ResponseWriter, r *http.Request) {
	report, errMsg := s.status(r.Context())
	if errMsg != "" {
		apiError(w, http.StatusBadGateway, errMsg)
		return
	}
	apiJSON(w, http.StatusOK, report)
}

// apiField is one input. Names are the page's form names, and a save sends every one back. Kind is
// text, number, bool, select or secret; a secret's value is never sent, only whether it is set.
type apiField struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Help    string   `json:"help,omitempty"`
	Kind    string   `json:"kind"`
	Value   string   `json:"value"`
	Options []string `json:"options,omitempty"`
	Set     *bool    `json:"set,omitempty"`
}

type apiGroup struct {
	ID     string     `json:"id"`
	Title  string     `json:"title"`
	Fields []apiField `json:"fields"`
}

type apiSettingsView struct {
	HostID   string     `json:"host_id"`
	Sections []apiGroup `json:"sections"`
	// Pools each end with a pool.<name>.delete box; new_pool adds one by name.
	Pools []apiGroup `json:"pools"`
	// Targets each end with a target.<i>.remove box. Adding one goes through POST /api/v1/targets.
	Targets []apiGroup `json:"targets"`
}

// secretFields are the inputs saved to their own files, by the section they appear in. A blank value
// keeps the current secret and <name>_clear removes it, as on the page.
var secretFields = map[string]apiField{
	"Alerts":              {Name: "notify_url", Label: "Webhook URL", Help: "ntfy, Discord or Slack; kept in its own file."},
	"Logs (Grafana Loki)": {Name: "loki_token", Label: "Token", Help: "Access policy token with logs:write; kept in its own file."},
}

func (s *Server) apiSettings(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.Cfg
	v := apiSettingsView{HostID: c.HostID, Sections: []apiGroup{}, Pools: []apiGroup{}, Targets: []apiGroup{}}
	for _, sec := range hostFields() {
		g := apiGroup{ID: "s-" + slug(sec.Title), Title: sec.Title}
		for _, f := range sec.Fields {
			g.Fields = append(g.Fields, apiField{Name: f.Name, Label: f.Label, Help: f.Help, Kind: f.Kind, Value: f.get(c), Options: f.Options})
		}
		if sf, ok := secretFields[sec.Title]; ok {
			set := (sf.Name == "notify_url" && c.Notify.WebhookURLFile != "") || (sf.Name == "loki_token" && c.Logs.TokenFile != "")
			sf.Kind, sf.Set = "secret", &set
			g.Fields = append(g.Fields, sf)
		}
		v.Sections = append(v.Sections, g)
	}
	for _, name := range c.PoolNames() {
		g := apiGroup{ID: "pool." + name, Title: "Pool " + name}
		for _, f := range poolFields() {
			g.Fields = append(g.Fields, apiField{Name: "pool." + name + "." + f.Key, Label: f.Label, Kind: f.Kind,
				Value: f.get(c.Pools[name]), Options: f.Options})
		}
		g.Fields = append(g.Fields, apiField{Name: "pool." + name + ".delete", Label: "Delete this pool on save", Kind: "bool", Value: "false"})
		v.Pools = append(v.Pools, g)
	}
	for i, t := range c.Targets {
		key := "target." + strconv.Itoa(i) + "."
		g := apiGroup{ID: "target." + strconv.Itoa(i), Fields: []apiField{
			{Name: key + "pools", Label: "Pools", Help: "Comma-separated pool names.", Kind: "text", Value: strings.Join(t.Pools, ", ")}}}
		if t.Org != "" {
			g.Title = "Org " + t.Org
			g.Fields = append(g.Fields, apiField{Name: key + "runner_group", Label: "Runner group", Help: "Empty for Default.", Kind: "text", Value: t.RunnerGroup})
		} else {
			g.Title = "User " + t.User
			g.Fields = append(g.Fields, apiField{Name: key + "repos", Label: "Repositories",
				Help: "Comma-separated; empty for all private ones.", Kind: "text", Value: strings.Join(t.Repos, ", ")})
		}
		g.Fields = append(g.Fields, apiField{Name: key + "remove", Label: "Remove on save (the GitHub App stays until you delete it on GitHub)",
			Kind: "bool", Value: "false"})
		v.Targets = append(v.Targets, g)
	}
	apiJSON(w, http.StatusOK, v)
}

// apiSaveSettings takes {"values": {name: value}} with every field, like the page's form, and saves it
// through the same path: validate the whole result, write it and the secrets, then restart. A field
// left out counts as emptied, so the app sends back everything it was shown.
func (s *Server) apiSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values map[string]string `json:"values"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, apiMaxBody)).Decode(&body); err != nil || body.Values == nil {
		apiError(w, http.StatusBadRequest, "expected {\"values\": {...}}")
		return
	}
	form := url.Values{}
	for k, v := range body.Values {
		form.Set(k, v)
	}
	s.mu.Lock()
	next, err := s.apply(form)
	if err == nil {
		err = next.Save(s.ConfigPath)
	}
	if err != nil {
		s.mu.Unlock()
		apiError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	s.Cfg = next
	s.mu.Unlock()
	if s.Service.Saved != nil {
		s.Service.Saved()
	}
	apiJSON(w, http.StatusOK, map[string]bool{"restarting": true})
}

type apiRun struct {
	Running bool   `json:"running"`
	Output  string `json:"output"`
	Err     string `json:"error,omitempty"`
	Started string `json:"started"`
	Ended   string `json:"ended,omitempty"`
}

type apiActionView struct {
	Name    string   `json:"name"`
	Label   string   `json:"label"`
	Help    string   `json:"help,omitempty"`
	Group   string   `json:"group"`
	Args    []string `json:"args"`
	Confirm string   `json:"confirm,omitempty"`
	Last    *apiRun  `json:"last,omitempty"`
}

func (s *Server) actionView(a *Action) apiActionView {
	v := apiActionView{Name: a.Name, Label: a.Label, Help: a.Help, Group: a.Group, Args: a.Args, Confirm: a.Confirm}
	if v.Group == "" {
		v.Group = "Other"
	}
	if v.Args == nil {
		v.Args = []string{}
	}
	s.mu.Lock()
	run := s.runs[a.Name]
	s.mu.Unlock()
	if run != nil {
		rv := run.view()
		v.Last = &apiRun{Running: rv.Running, Output: rv.Output, Err: rv.Err, Started: rv.Started, Ended: rv.Ended}
	}
	return v
}

func (s *Server) apiActions(w http.ResponseWriter, r *http.Request) {
	out := []apiActionView{}
	for i := range s.Service.Actions {
		out = append(out, s.actionView(&s.Service.Actions[i]))
	}
	apiJSON(w, http.StatusOK, out)
}

func (s *Server) apiAction(w http.ResponseWriter, r *http.Request) {
	a := s.action(r.PathValue("name"))
	if a == nil {
		apiError(w, http.StatusNotFound, "no such action")
		return
	}
	apiJSON(w, http.StatusOK, s.actionView(a))
}

// apiRunAction takes {"args": {name: value}} (optional) and answers 202 with the action, or 409 while
// it is still running.
func (s *Server) apiRunAction(w http.ResponseWriter, r *http.Request) {
	a := s.action(r.PathValue("name"))
	if a == nil {
		apiError(w, http.StatusNotFound, "no such action")
		return
	}
	var body struct {
		Args map[string]string `json:"args"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, apiMaxBody)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		apiError(w, http.StatusBadRequest, "expected {\"args\": {...}}")
		return
	}
	args := map[string]string{}
	for _, k := range a.Args {
		args[k] = strings.TrimSpace(body.Args[k])
	}
	if !s.startAction(a, args) {
		apiJSON(w, http.StatusConflict, s.actionView(a))
		return
	}
	apiJSON(w, http.StatusAccepted, s.actionView(a))
}

// appLink is a pending "add an org or account" that the app asked for and the browser has yet to open.
type appLink struct {
	Owner string
	Org   bool
	Until time.Time
}

// apiAddTarget takes {"org": name} or {"user": name} and answers 201 with {"url": ...}, a one-time link
// for the browser that starts creating the GitHub App, as the setup page's buttons do. The link is the
// authorization: only someone the API accepted gets one, and it works once, briefly.
func (s *Server) apiAddTarget(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Org  string `json:"org"`
		User string `json:"user"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, apiMaxBody)).Decode(&body); err != nil {
		apiError(w, http.StatusBadRequest, "expected {\"org\": ...} or {\"user\": ...}")
		return
	}
	owner, org := strings.TrimSpace(body.Org), true
	if owner == "" {
		owner, org = strings.TrimSpace(body.User), false
	}
	if !ownerRe.MatchString(owner) {
		apiError(w, http.StatusUnprocessableEntity, badOwner)
		return
	}
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	key, now := hex.EncodeToString(b), time.Now()
	s.mu.Lock()
	for k, l := range s.links {
		if now.After(l.Until) {
			delete(s.links, k)
		}
	}
	s.links[key] = appLink{Owner: owner, Org: org, Until: now.Add(linkTTL)}
	s.mu.Unlock()
	apiJSON(w, http.StatusCreated, map[string]string{"url": baseURL(r) + "/app/new?k=" + key})
}

// newAppFromLink opens a link from apiAddTarget in the browser, once.
func (s *Server) newAppFromLink(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("k")
	s.mu.Lock()
	l, ok := s.links[key]
	delete(s.links, key)
	s.mu.Unlock()
	if !ok || time.Now().After(l.Until) {
		http.Error(w, "This link has expired or was already used. Ask for a new one in the app.", http.StatusForbidden)
		return
	}
	s.startApp(w, r, l.Owner, !l.Org)
}
