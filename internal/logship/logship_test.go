package logship

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
)

var now = time.Unix(1_760_000_000, 0)

// entry is one `journalctl -o json` line logged ago before now.
func entry(cursor string, ago time.Duration, msg string) string {
	b, _ := json.Marshal(map[string]string{"__CURSOR": cursor, "MESSAGE": msg,
		"__REALTIME_TIMESTAMP": fmt.Sprint(now.Add(-ago).UnixMicro())})
	return string(b) + "\n"
}

func ns(ago time.Duration) string { return fmt.Sprint(now.Add(-ago).UnixNano()) }

type loki struct {
	*httptest.Server
	mu     sync.Mutex
	pushes []map[string]any
	auth   []string
	status int
}

func newLoki(t *testing.T) *loki {
	l := &loki{status: 204}
	l.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		l.mu.Lock()
		defer l.mu.Unlock()
		if r.Method != "POST" || r.URL.Path != "/loki/api/v1/push" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var m map[string]any
		_ = json.NewDecoder(r.Body).Decode(&m)
		l.pushes = append(l.pushes, m)
		l.auth = append(l.auth, r.Header.Get("Authorization"))
		w.WriteHeader(l.status)
	}))
	t.Cleanup(l.Close)
	return l
}

func setup(t *testing.T, guests ...pve.Guest) (*Shipper, *pvetest.Fake, *loki) {
	f := pvetest.New(guests...)
	f.Outputs = map[string]string{}
	l := newLoki(t)
	s := &Shipper{Host: &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9499}}, URL: l.URL + "/", User: "123",
		Token: "tok", HostID: "pve1", HTTP: l.Client(), Now: func() time.Time { return now }}
	return s, f, l
}

func TestShipsServiceAndRunnerLogsThenOnlyNewLines(t *testing.T) {
	s, f, l := setup(t,
		pve.Guest{VMID: 9001, Name: "gh-pve-default-9001", Type: "lxc", Status: "running", RawTags: "gh-runner;pool-default"},
		pve.Guest{VMID: 9002, Name: "gh-pve-default-9002", Type: "lxc", Status: "stopped", RawTags: "gh-runner;pool-default"})
	f.Outputs["journalctl -u pve-runners.service -o json --no-pager -n 1000"] =
		entry("s1", 2*time.Hour, "from the previous run") + entry("s2", time.Minute, "started") + "not json\n"
	f.Outputs["pct exec 9001 -- journalctl -u gh-runner.service -o json --no-pager -n 1000"] =
		entry("r1", 3*time.Hour, "Listening for Jobs")
	ctx := context.Background()
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"streams": []any{
		map[string]any{"stream": map[string]any{"service": "pve-runners", "host": "pve1", "unit": "pve-runners"},
			"values": []any{[]any{ns(time.Minute), "started"}}},
		map[string]any{"stream": map[string]any{"service": "pve-runners", "host": "pve1", "unit": "gh-runner",
			"vmid": "9001", "runner": "gh-pve-default-9001", "pool": "default"},
			"values": []any{[]any{ns(3 * time.Hour), "Listening for Jobs"}}},
	}}
	if !reflect.DeepEqual(l.pushes[0], want) {
		t.Fatalf("push\n got %v\nwant %v", l.pushes[0], want)
	}
	if l.auth[0] != "Basic MTIzOnRvaw==" {
		t.Fatalf("auth %q", l.auth[0])
	}

	// The next round reads after each cursor; with nothing new, nothing is pushed.
	f.Outputs["journalctl -u pve-runners.service -o json --no-pager -n 1000 --after-cursor s2"] = ""
	f.Calls = nil
	if err := s.Once(ctx); err != nil || len(l.pushes) != 1 {
		t.Fatalf("pushed %d times: %v", len(l.pushes), err)
	}
	for _, c := range []string{"journalctl -u pve-runners.service -o json --no-pager -n 1000 --after-cursor s2",
		"pct exec 9001 -- journalctl -u gh-runner.service -o json --no-pager -n 1000 --after-cursor r1"} {
		if !strings.Contains(strings.Join(f.Calls, "\n"), c) {
			t.Errorf("missing %q in %v", c, f.Calls)
		}
	}
}

func TestRetryableFailureKeepsLinesAndClientErrorDropsThem(t *testing.T) {
	s, f, l := setup(t)
	s.User = ""
	first := "journalctl -u pve-runners.service -o json --no-pager -n 1000"
	f.Outputs[first] = entry("s1", time.Minute, "one")
	ctx := context.Background()

	l.status = 503
	if err := s.Once(ctx); err == nil {
		t.Fatal("want an error")
	}
	l.status = 204
	if err := s.Once(ctx); err != nil {
		t.Fatal(err)
	}
	if len(l.pushes) != 2 || !reflect.DeepEqual(l.pushes[0], l.pushes[1]) || l.auth[1] != "Bearer tok" {
		t.Fatalf("the failed lines were not resent: %v %v", l.pushes, l.auth)
	}

	f.Outputs[first+" --after-cursor s1"] = entry("s2", 0, "bad")
	l.status = 400
	if err := s.Once(ctx); err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want the 400 reported, got %v", err)
	}
	l.status = 204
	f.Outputs[first+" --after-cursor s2"] = ""
	f.Calls = nil
	_ = s.Once(ctx)
	if !strings.Contains(strings.Join(f.Calls, "\n"), "--after-cursor s2") || len(l.pushes) != 3 {
		t.Fatalf("rejected lines should be skipped, not retried: %v", f.Calls)
	}
}

func TestRedact(t *testing.T) {
	long := strings.Repeat("eyJhbGciOiJSUzI1NiJ9", 8)
	for in, want := range map[string]string{
		"token ghs_abcdefghijklmnopqrstuvwxyz0123": "token [redacted]",
		"Authorization: Bearer abc.def":            "Authorization: Bearer [redacted]",
		`password="hunter2" next`:                  `password="[redacted]" next`,
		"jit " + long + " end":                     "jit [redacted] end",
		"runner started vmid=9000 pool=default":    "runner started vmid=9000 pool=default",
		"registration token request failed":        "registration token request failed",
		"token=abc&x":                              "token=[redacted]",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNonUTF8MessageIsDecoded(t *testing.T) {
	vals, last := parse([]byte(`{"__CURSOR":"c","__REALTIME_TIMESTAMP":"1","MESSAGE":[104,105]}`), time.Time{})
	if last != "c" || len(vals) != 1 || vals[0][1] != "hi" {
		t.Fatalf("%v %q", vals, last)
	}
}
