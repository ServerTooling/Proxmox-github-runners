package scaleset

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/scaleset/scalesettest"
)

func newClient(f *scalesettest.Fake) *Client {
	return &Client{APIBase: f.URL, ConfigURL: "https://github.com/example-org", HTTP: f.Client(),
		RegToken: func(context.Context) (string, error) { return f.RegToken, nil }}
}

func TestConnectTradesRegistrationToken(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	c := newClient(f)
	ctx := context.Background()
	if _, err := c.RunnerGroupID(ctx, "builders"); err == nil {
		t.Fatal("unknown group should fail")
	}
	f.Groups["builders"] = 7
	id, err := c.RunnerGroupID(ctx, "builders")
	if err != nil || id != 7 {
		t.Fatalf("group id = %d, %v", id, err)
	}
	reg := f.CallsTo("POST", "/actions/runner-registration")
	if len(reg) != 1 {
		t.Fatalf("registered %d times; the admin token should be reused until it nears expiry", len(reg))
	}
	if reg[0].Auth != "RemoteAuth regtok" {
		t.Errorf("auth = %q", reg[0].Auth)
	}
	var body map[string]string
	json.Unmarshal([]byte(reg[0].Body), &body)
	if body["url"] != "https://github.com/example-org" || body["runner_event"] != "register" {
		t.Errorf("body = %v", body)
	}
	groups := f.CallsTo("GET", "/svc/_apis/runtime/runnergroups/")
	if len(groups) != 2 || groups[1].Query != "api-version=6.0-preview&groupName=builders" {
		t.Fatalf("group calls = %+v", groups)
	}
	if !strings.HasPrefix(groups[1].Auth, "Bearer ") {
		t.Errorf("service auth = %q", groups[1].Auth)
	}
}

func TestDefaultGroupNeedsNoLookup(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	for _, name := range []string{"", "Default"} {
		id, err := newClient(f).RunnerGroupID(context.Background(), name)
		if err != nil || id != DefaultGroupID {
			t.Fatalf("%q: %d, %v", name, id, err)
		}
	}
	if len(f.Calls()) != 0 {
		t.Errorf("calls = %+v", f.Calls())
	}
}

func TestAdminTokenRenewedBeforeExpiry(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	c := newClient(f)
	now := time.Now()
	c.Now = func() time.Time { return now }
	ctx := context.Background()
	f.Groups["g"] = 2
	c.RunnerGroupID(ctx, "g")
	now = now.Add(59*time.Minute + 30*time.Second) // inside the last minute of the hour-long token
	c.RunnerGroupID(ctx, "g")
	if n := len(f.CallsTo("POST", "/actions/runner-registration")); n != 2 {
		t.Fatalf("registered %d times, want 2", n)
	}
}

func TestBadRegistrationTokenFails(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	c := newClient(f)
	c.RegToken = func(context.Context) (string, error) { return "wrong", nil }
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.EnsureScaleSet(ctx, "proxmox", 1, nil); err == nil {
		t.Fatal("expected an error")
	}
}

func TestEnsureScaleSetCreatesOnce(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	c := newClient(f)
	ctx := context.Background()
	ss, err := c.EnsureScaleSet(ctx, "proxmox", 3, []string{"proxmox", "linux"})
	if err != nil {
		t.Fatal(err)
	}
	again, err := c.EnsureScaleSet(ctx, "proxmox", 3, []string{"proxmox", "linux"})
	if err != nil || again.ID != ss.ID {
		t.Fatalf("second ensure = %+v, %v", again, err)
	}
	posts := f.CallsTo("POST", "/svc/_apis/runtime/runnerscalesets")
	if len(posts) != 1 {
		t.Fatalf("created %d scale sets", len(posts))
	}
	want := `{"name":"proxmox","runnerGroupId":3,"labels":[{"type":"System","name":"proxmox"},{"type":"System","name":"linux"}],` +
		`"RunnerSetting":{"ephemeral":true,"disableUpdate":true}}`
	if posts[0].Body != want {
		t.Errorf("body\n got %s\nwant %s", posts[0].Body, want)
	}
	gets := f.CallsTo("GET", "/svc/_apis/runtime/runnerscalesets")
	if gets[0].Query != "api-version=6.0-preview&name=proxmox&runnerGroupId=3" {
		t.Errorf("query = %s", gets[0].Query)
	}
}

func TestGenerateJIT(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	jit, err := newClient(f).GenerateJIT(context.Background(), 4, "gh-pve1-default-9000", "_work")
	if err != nil {
		t.Fatal(err)
	}
	if jit.EncodedJITConfig != "jit-gh-pve1-default-9000" || jit.Runner.Name != "gh-pve1-default-9000" {
		t.Errorf("jit = %+v", jit)
	}
	calls := f.CallsTo("POST", "/svc/_apis/runtime/runnerscalesets/4/generatejitconfig")
	if len(calls) != 1 || calls[0].Body != `{"name":"gh-pve1-default-9000","workFolder":"_work"}` {
		t.Errorf("calls = %+v", calls)
	}
}

func TestGenerateJITReplacesStaleRunner(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	// A runner whose container crashed before taking a job is still registered under the reused name.
	f.AddRunner("gh-pve1-default-9000", 55)
	jit, err := newClient(f).GenerateJIT(context.Background(), 4, "gh-pve1-default-9000", "_work")
	if err != nil {
		t.Fatal(err)
	}
	if jit.Runner.Name != "gh-pve1-default-9000" || jit.Runner.ID == 55 {
		t.Errorf("jit = %+v", jit)
	}
	if got := f.Removed(); len(got) != 1 || got[0] != 55 {
		t.Errorf("removed = %v", got)
	}
	gets := f.CallsTo("GET", "/svc/_apis/distributedtask/pools/0/agents")
	if len(gets) != 1 || !strings.Contains(gets[0].Query, "agentName=gh-pve1-default-9000") {
		t.Errorf("lookups = %+v", gets)
	}
	if n := len(f.CallsTo("POST", "/svc/_apis/runtime/runnerscalesets/4/generatejitconfig")); n != 2 {
		t.Errorf("generatejitconfig calls = %d, want 2", n)
	}
}

func TestGenerateJITFallsBackToUniqueNameWhenLookupFails(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	f.AddRunner("gh-pve1-default-9000", 55)
	f.NoRunnerLookup = true
	jit, err := newClient(f).GenerateJIT(context.Background(), 4, "gh-pve1-default-9000", "_work")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(jit.Runner.Name, "gh-pve1-default-9000-") {
		t.Errorf("jit = %+v", jit)
	}
}

func openSession(t *testing.T, f *scalesettest.Fake) (*Client, *Session) {
	t.Helper()
	c := newClient(f)
	id := f.AddScaleSet("proxmox", 1)
	s, err := c.CreateSession(context.Background(), id, "pve1")
	if err != nil {
		t.Fatal(err)
	}
	return c, s
}

func TestSessionMessagesAndAcquire(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	_, s := openSession(t, f)
	ctx := context.Background()
	if c := f.CallsTo("POST", "/svc/_apis/runtime/runnerscalesets/1/sessions"); c[0].Body != `{"ownerName":"pve1"}` {
		t.Errorf("session body = %s", c[0].Body)
	}

	msg, err := s.GetMessage(ctx, 0, 4)
	if err != nil || msg != nil {
		t.Fatalf("empty queue: %+v, %v", msg, err)
	}
	f.Push(1, scalesettest.Job{Type: "JobAvailable", RunnerRequestID: 55, RepositoryName: "app", OwnerName: "example-org"})
	msg, err = s.GetMessage(ctx, 0, 4)
	if err != nil || msg == nil {
		t.Fatalf("message: %+v, %v", msg, err)
	}
	jobs, err := msg.Jobs()
	if err != nil || len(jobs) != 1 || jobs[0].RunnerRequestID != 55 || jobs[0].RepositoryName != "app" {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	if msg.Statistics == nil || msg.Statistics.TotalAssignedJobs != 1 {
		t.Errorf("stats = %+v", msg.Statistics)
	}
	poll := f.CallsTo("GET", "/queue/*")
	h := poll[len(poll)-1].Header
	if h.Get("Accept") != "application/json; api-version=6.0-preview" || h.Get("X-ScaleSetMaxCapacity") != "4" ||
		h.Get("Authorization") != "Bearer queue" {
		t.Errorf("poll headers = %v", h)
	}
	if err := s.DeleteMessage(ctx, msg.ID); err != nil {
		t.Fatal(err)
	}
	if f.Pending() != 0 {
		t.Error("message not deleted")
	}
	got, err := s.AcquireJobs(ctx, []int64{55})
	if err != nil || len(got) != 1 || got[0] != 55 {
		t.Fatalf("acquired = %v, %v", got, err)
	}
	acq := f.CallsTo("POST", "/svc/_apis/runtime/runnerscalesets/1/acquirejobs")
	if acq[0].Body != "[55]" || acq[0].Auth != "Bearer queue" {
		t.Errorf("acquire call = %+v", acq[0])
	}

	f.Push(0)
	if _, err := s.GetMessage(ctx, msg.ID, 4); err != nil {
		t.Fatal(err)
	}
	if q := f.CallsTo("GET", "/queue/*"); q[len(q)-1].Query != "lastMessageId=1" {
		t.Errorf("poll query = %q", q[len(q)-1].Query)
	}

	if err := s.Close(ctx); err != nil || f.Sessions() != 0 {
		t.Fatalf("close: %v, %d sessions left", err, f.Sessions())
	}
}

func TestQueueTokenRefreshedOn401(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	_, s := openSession(t, f)
	f.ExpireQueueToken()
	if _, err := s.GetMessage(context.Background(), 0, 1); err != nil {
		t.Fatal(err)
	}
	if n := len(f.CallsTo("PATCH", "/svc/_apis/runtime/runnerscalesets/1/sessions/*")); n != 1 {
		t.Fatalf("refreshed %d times", n)
	}
	q := f.CallsTo("GET", "/queue/*")
	if q[len(q)-1].Auth != "Bearer queue-refreshed" {
		t.Errorf("retry auth = %q", q[len(q)-1].Auth)
	}
}

func TestSessionConflict(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	f.Conflict(1)
	c := newClient(f)
	_, err := c.CreateSession(context.Background(), f.AddScaleSet("proxmox", 1), "pve1")
	if !errors.Is(err, ErrSessionConflict) {
		t.Fatalf("err = %v", err)
	}
}

func TestServiceErrorDetails(t *testing.T) {
	f := scalesettest.New()
	defer f.Close()
	err := newClient(f).call(context.Background(), http.MethodGet, "_apis/runtime/nothing", nil, nil, nil)
	var se *ServiceError
	if !errors.As(err, &se) || se.Status != 404 || !strings.Contains(se.Message, "no such route") {
		t.Fatalf("err = %#v", err)
	}
}

func TestMessageJobsRejectsUnknownType(t *testing.T) {
	m := Message{ID: 1, Type: "Other", Body: "[]"}
	if _, err := m.Jobs(); err == nil {
		t.Fatal("expected an error")
	}
}

func TestGitHubURL(t *testing.T) {
	for api, want := range map[string]string{
		"https://api.github.com":          "https://github.com",
		"https://api.github.com/":         "https://github.com",
		"https://ghe.example.com/api/v3":  "https://ghe.example.com",
		"https://ghe.example.com/api/v3/": "https://ghe.example.com",
		"http://192.0.2.10:8080":          "http://192.0.2.10:8080",
	} {
		if got := GitHubURL(api); got != want {
			t.Errorf("GitHubURL(%q) = %q, want %q", api, got, want)
		}
	}
}
