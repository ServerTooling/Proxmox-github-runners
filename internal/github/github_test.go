package github_test

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/github"
	"github.com/ServerTooling/proxmox-github-runners/internal/github/githubtest"
)

func TestParsePrivateKeyRoundTrip(t *testing.T) {
	k := githubtest.GenerateKey()
	got, err := github.ParsePrivateKey(githubtest.KeyPEM(k))
	if err != nil || !got.Equal(k) {
		t.Fatalf("round trip failed: %v", err)
	}
	if _, err := github.ParsePrivateKey([]byte("nope")); err == nil {
		t.Fatal("expected error for non-PEM")
	}
}

func TestOrgJITConfigSendsExactBody(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	f.RunnerGroups["example-org"] = []map[string]any{{"id": 7, "name": "proxmox"}}
	c := github.NewClient(f.URL, 42, f.Key)
	ctx := context.Background()

	gid, err := c.RunnerGroupID(ctx, 5, "example-org", "Proxmox")
	if err != nil || gid != 7 {
		t.Fatalf("group: %d %v", gid, err)
	}
	jit, err := c.OrgJITConfig(ctx, 5, "example-org", github.JITRequest{
		Name: "gh-lxc-9001", RunnerGroupID: gid, Labels: []string{"proxmox", "lxc-small"}, WorkFolder: "_work"})
	if err != nil {
		t.Fatal(err)
	}
	if jit.EncodedJITConfig == "" || jit.Runner.ID == 0 {
		t.Fatalf("empty jit: %+v", jit)
	}
	got := f.JITRequests[0]
	if got.Path != "/orgs/example-org/actions/runners/generate-jitconfig" {
		t.Fatalf("path %s", got.Path)
	}
	want := map[string]any{"name": "gh-lxc-9001", "runner_group_id": float64(7),
		"labels": []any{"proxmox", "lxc-small"}, "work_folder": "_work"}
	if !reflect.DeepEqual(got.Body, want) {
		t.Fatalf("body\n got %v\nwant %v", got.Body, want)
	}
}

func TestPublicReposAndGroupsAreRefused(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	f.RunnerGroups["example-org"] = []map[string]any{
		{"id": 1, "name": "Default", "allows_public_repositories": false},
		{"id": 9, "name": "open", "allows_public_repositories": true}}
	f.RepoVisibility["someone/oss"] = "public"
	c := github.NewClient(f.URL, 42, f.Key)
	ctx := context.Background()
	if id, err := c.RunnerGroupID(ctx, 5, "example-org", ""); err != nil || id != 1 {
		t.Fatalf("default group: %d %v", id, err)
	}
	if _, err := c.RunnerGroupID(ctx, 5, "example-org", "open"); !errors.Is(err, github.ErrPublicRepo) {
		t.Fatalf("got %v", err)
	}
	if _, err := c.RepoJITConfig(ctx, 5, "someone", "oss", github.JITRequest{Name: "r"}); !errors.Is(err, github.ErrPublicRepo) {
		t.Fatalf("got %v", err)
	}
	if len(f.JITRequests) != 0 {
		t.Fatal("no runner may be registered for a public repo")
	}
}

func TestRunnerGroupsOfAPersonalAccountExplainsTheFix(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	_, err := github.NewClient(f.URL, 42, f.Key).RunnerGroupID(context.Background(), 5, "someone", "")
	if err == nil || !strings.Contains(err.Error(), "404") || !strings.Contains(err.Error(), "is someone a personal account?") {
		t.Fatalf("err = %v", err)
	}
}

func TestRepoJITConfigForcesDefaultGroup(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	f.RepoVisibility["someone/side-project"] = "private"
	c := github.NewClient(f.URL, 42, f.Key)
	if _, err := c.RepoJITConfig(context.Background(), 5, "someone", "side-project",
		github.JITRequest{Name: "r", RunnerGroupID: 99, Labels: []string{"x"}, WorkFolder: "_work"}); err != nil {
		t.Fatal(err)
	}
	got := f.JITRequests[0]
	if got.Path != "/repos/someone/side-project/actions/runners/generate-jitconfig" || got.Body["runner_group_id"] != float64(1) {
		t.Fatalf("got %+v", got)
	}
}

func TestWrongKeyIsRejectedAsAuthError(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	c := github.NewClient(f.URL, 42, githubtest.GenerateKey())
	_, err := c.Installations(context.Background())
	var apiErr *github.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 {
		t.Fatalf("want 401 APIError, got %v", err)
	}
}

func TestInstallationTokenIsCached(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	c := github.NewClient(f.URL, 42, f.Key)
	a, _ := c.InstallationToken(context.Background(), 5)
	b, _ := c.InstallationToken(context.Background(), 5)
	if a == "" || a != b {
		t.Fatalf("token not cached: %q %q", a, b)
	}
}

func TestConvertManifestIsSingleUse(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	f.ManifestCodes["abc"] = true
	creds, err := github.ConvertManifest(context.Background(), http.DefaultClient, f.URL, "abc")
	if err != nil || creds.ID != 42 || creds.Slug != "pve-runners-test" || creds.PEM == "" {
		t.Fatalf("got %+v %v", creds, err)
	}
	if _, err := github.ConvertManifest(context.Background(), http.DefaultClient, f.URL, "abc"); err == nil {
		t.Fatal("second conversion should fail")
	}
}

func TestRegistrationTokenPaths(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	c := github.NewClient(f.URL, 42, f.Key)
	org, err := c.RegistrationToken(context.Background(), 5, "example-org", "")
	if err != nil || org != "regtok-5" {
		t.Fatalf("org: %q %v", org, err)
	}
	if _, err := c.RegistrationToken(context.Background(), 6, "someone", "side"); err != nil {
		t.Fatal(err)
	}
	want := []string{"/orgs/example-org/actions/runners/registration-token", "/repos/someone/side/actions/runners/registration-token"}
	if !reflect.DeepEqual(f.RegTokens, want) {
		t.Fatalf("paths %v", f.RegTokens)
	}
}

func TestQueuedJobsReadsQueuedAndInProgressRuns(t *testing.T) {
	f := githubtest.New(42)
	defer f.Close()
	f.Runs = map[string][]map[string]any{"example-org/app": {
		{"id": 10, "status": "queued"}, {"id": 11, "status": "in_progress"}, {"id": 12, "status": "completed"}}}
	f.RunJobs = map[int64][]map[string]any{
		10: {{"id": 100, "run_id": 10, "name": "build", "status": "queued", "labels": []string{"proxmox"},
			"created_at": "2026-01-02T03:04:05Z"}},
		11: {{"id": 110, "run_id": 11, "name": "lint", "status": "in_progress", "labels": []string{"proxmox"}},
			{"id": 111, "run_id": 11, "name": "e2e", "status": "queued", "labels": []string{"proxmox"}}},
	}
	c := github.NewClient(f.URL, 42, f.Key)
	jobs, err := c.QueuedJobs(context.Background(), 5, "example-org", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].ID != 100 || jobs[1].ID != 111 || jobs[0].Repo != "example-org/app" ||
		jobs[0].CreatedAt.Format(time.RFC3339) != "2026-01-02T03:04:05Z" || jobs[1].Labels[0] != "proxmox" {
		t.Fatalf("jobs = %+v", jobs)
	}
	want := []string{
		"/repos/example-org/app/actions/runs?status=queued&per_page=100",
		"/repos/example-org/app/actions/runs/10/jobs?per_page=100",
		"/repos/example-org/app/actions/runs?status=in_progress&per_page=100",
		"/repos/example-org/app/actions/runs/11/jobs?per_page=100",
	}
	if !reflect.DeepEqual(f.RunQueries, want) {
		t.Fatalf("requests\n got %q\nwant %q", f.RunQueries, want)
	}
}
