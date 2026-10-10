// Package notify sends short alerts to an optional webhook (ntfy, Discord or Slack). It is off unless
// configured; everything is also logged to the journal regardless.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Events worth waking someone for.
const (
	SpawnFailed    = "spawn_failed"
	UpdateFailed   = "update_failed"
	UpdateRollback = "update_rolled_back"
	ImageFailed    = "image_failed"
)

type Notifier struct {
	URL    string
	Format string // ntfy | discord | slack
	HTTP   *http.Client
	Host   string // short host label, e.g. the Proxmox node name
}

// FromFile reads the webhook URL from a 0600 file (the URL is a credential for Discord and Slack).
// An empty path disables notifications.
func FromFile(path, format, host string) (*Notifier, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read notify webhook file: %w", err)
	}
	u := strings.TrimSpace(string(b))
	if format == "" || format == "auto" {
		format = Detect(u)
	}
	return &Notifier{URL: u, Format: format, HTTP: &http.Client{Timeout: 10 * time.Second}, Host: host}, nil
}

// Detect picks the payload format from the webhook host.
func Detect(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "ntfy"
	}
	switch h := strings.ToLower(u.Hostname()); {
	case h == "discord.com" || h == "discordapp.com":
		return "discord"
	case h == "hooks.slack.com":
		return "slack"
	}
	return "ntfy"
}

// Payload builds the request body and content type for a format.
func Payload(format, title, msg string) ([]byte, string) {
	text := title + ": " + msg
	switch format {
	case "discord":
		b, _ := json.Marshal(map[string]string{"content": text})
		return b, "application/json"
	case "slack":
		b, _ := json.Marshal(map[string]string{"text": text})
		return b, "application/json"
	}
	return []byte(msg), "text/plain"
}

// Send posts an alert. A nil Notifier is valid and only logs. Delivery failures are logged, never
// returned: an alert must not break the operation that triggered it.
func (n *Notifier) Send(ctx context.Context, event, msg string) {
	slog.Warn("alert", "event", event, "msg", msg)
	if n == nil || n.URL == "" {
		return
	}
	title := "pve-runners"
	if n.Host != "" {
		title += " on " + n.Host
	}
	title += " (" + event + ")"
	body, ctype := Payload(n.Format, title, msg)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL, bytes.NewReader(body))
	if err != nil {
		slog.Error("notify", "err", err)
		return
	}
	req.Header.Set("Content-Type", ctype)
	if n.Format == "ntfy" {
		req.Header.Set("Title", title)
		req.Header.Set("Tags", "warning")
	}
	resp, err := n.HTTP.Do(req)
	if err != nil {
		slog.Error("notify", "err", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Error("notify", "status", resp.StatusCode)
	}
}
