package setup

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// restarted is a new server for the same config and secret dir, as after a save or an update.
func restarted(t *testing.T, s *Server) *httptest.Server {
	n, _ := NewServer(s.Cfg, s.ConfigPath)
	n.Service = &Service{SecretDir: s.Service.SecretDir, Login: func(context.Context, string, string) error { return nil }}
	srv := httptest.NewServer(n.Handler())
	t.Cleanup(srv.Close)
	return srv
}

func login(t *testing.T, srv *httptest.Server) string {
	c := client(t)
	resp, err := c.PostForm(srv.URL+"/login", url.Values{"user": {"root"}, "password": {"pw"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == sessionCookie {
			return ck.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func get(t *testing.T, srv *httptest.Server, path, id string) *http.Response {
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	resp, err := client(t).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestLoginSurvivesARestartAndLogoutDoesToo(t *testing.T) {
	s, srv, _, _ := settingsServer(t)
	id := login(t, srv)

	file := filepath.Join(s.Service.SecretDir, sessionFile)
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), id) {
		t.Fatal("the session file holds the session ID itself, not its hash")
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o600 {
		t.Fatalf("session file mode %v", st.Mode().Perm())
	}

	srv2 := restarted(t, s)
	if resp := get(t, srv2, "/", id); resp.StatusCode != 200 {
		t.Fatalf("logged out by the restart: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if resp := get(t, srv2, "/", "not-a-session"); resp.Header.Get("Location") != "/login" {
		t.Fatal("an unknown session was let in")
	}

	req, _ := http.NewRequest("POST", srv2.URL+"/logout", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	if _, err := client(t).Do(req); err != nil {
		t.Fatal(err)
	}
	if resp := get(t, restarted(t, s), "/", id); resp.Header.Get("Location") != "/login" {
		t.Fatal("still logged in after logout and a restart")
	}
}

func TestSessionsExpireUnlessUsed(t *testing.T) {
	s, srv, _, _ := settingsServer(t)
	id := login(t, srv)
	key := sessionKey(id)

	// A visit after the renewal interval pushes the expiry out and refreshes the cookie.
	s.mu.Lock()
	s.sessions[key] = time.Now().Add(sessionTTL - 2*sessionRenew)
	s.mu.Unlock()
	resp := get(t, srv, "/", id)
	if resp.StatusCode != 200 || len(resp.Cookies()) != 1 || resp.Cookies()[0].MaxAge != int(sessionTTL.Seconds()) {
		t.Fatalf("not renewed: %d %v", resp.StatusCode, resp.Cookies())
	}
	s.mu.Lock()
	exp := s.sessions[key]
	s.mu.Unlock()
	if time.Until(exp) < sessionTTL-time.Minute {
		t.Fatalf("expiry not extended: %v", exp)
	}

	// Left unused past its expiry, it is gone, also for the next run.
	s.mu.Lock()
	s.sessions[key] = time.Now().Add(-time.Second)
	s.mu.Unlock()
	if resp := get(t, srv, "/", id); resp.Header.Get("Location") != "/login" {
		t.Fatal("an expired session was let in")
	}
	if resp := get(t, restarted(t, s), "/", id); resp.Header.Get("Location") != "/login" {
		t.Fatal("an expired session came back after a restart")
	}
}
