package setup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

// Settings page logins are kept in a file so that a restart (every save, every update) doesn't log
// the browser out. The file holds only SHA-256 hashes of the session IDs: reading it doesn't give a
// way in, though it is still 0600 next to the other secrets.

const sessionFile = "web-sessions.json"

// sessionRenew is how stale an expiry may get before a request pushes it out again, so that a busy
// page doesn't rewrite the file on every request.
const sessionRenew = time.Hour

func sessionKey(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (s *Server) sessionPath() string {
	if s.Service == nil || s.Service.SecretDir == "" {
		return ""
	}
	return filepath.Join(s.Service.SecretDir, sessionFile)
}

// loadSessions reads the logins a previous run left, dropping expired ones. A missing or unreadable
// file only means everyone logs in again.
func (s *Server) loadSessions(now time.Time) {
	p := s.sessionPath()
	if p == "" {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var saved map[string]time.Time
	if json.Unmarshal(b, &saved) != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, exp := range saved {
		if now.Before(exp) {
			s.sessions[k] = exp
		}
	}
}

// saveSessionsLocked writes the logins atomically; s.mu must be held.
func (s *Server) saveSessionsLocked() error {
	p := s.sessionPath()
	if p == "" {
		return nil
	}
	b, err := json.Marshal(s.sessions)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), sessionFile+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}
