package notify

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestDetect(t *testing.T) {
	for in, want := range map[string]string{
		"https://discord.com/api/webhooks/1/abc": "discord",
		"https://hooks.slack.com/services/T/B/x": "slack",
		"https://ntfy.sh/some-topic":             "ntfy",
		"https://ntfy.example.com/runners":       "ntfy",
	} {
		if got := Detect(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestSendPostsFormattedAlert(t *testing.T) {
	var body, title, ctype string
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body, title, ctype = string(b), r.Header.Get("Title"), r.Header.Get("Content-Type")
	}))
	defer s.Close()
	f := filepath.Join(t.TempDir(), "hook")
	_ = os.WriteFile(f, []byte(s.URL+"\n"), 0o600)
	n, err := FromFile(f, "auto", "pve1")
	if err != nil {
		t.Fatal(err)
	}
	n.Send(context.Background(), SpawnFailed, "pool default: boom")
	if body != "pool default: boom" || title != "pve-runners on pve1 (spawn_failed)" || ctype != "text/plain" {
		t.Fatalf("got %q %q %q", body, title, ctype)
	}
	b, ct := Payload("discord", "t", "m")
	if string(b) != `{"content":"t: m"}` || ct != "application/json" {
		t.Fatalf("discord payload %s", b)
	}
}

func TestDisabledIsNoop(t *testing.T) {
	n, err := FromFile("", "", "")
	if err != nil || n != nil {
		t.Fatal("empty path should disable")
	}
	n.Send(context.Background(), UpdateFailed, "x") // must not panic
}
