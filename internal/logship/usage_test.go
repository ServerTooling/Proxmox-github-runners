package logship

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

func TestUsageSamplesEachRunningRunnerWithItsJob(t *testing.T) {
	busy := pve.Guest{VMID: 9001, Name: "gh-pve-default-9001", Type: "lxc", Status: "running", RawTags: "gh-runner;pool-default",
		CPU: 0.5, MaxCPU: 4, Mem: 2 << 30, MaxMem: 6 << 30, Disk: 5 << 30, MaxDisk: 20 << 30,
		NetIn: 1000, NetOut: 500, DiskRead: 0, DiskWrite: 4000}
	idle := pve.Guest{VMID: 9002, Name: "gh-pve-default-9002", Type: "lxc", Status: "running", RawTags: "gh-runner;pool-default",
		MaxCPU: 4, Mem: 300 << 20, MaxMem: 6 << 30}
	stopped := pve.Guest{VMID: 9003, Name: "gh-pve-default-9003", Type: "lxc", Status: "stopped", RawTags: "gh-runner;pool-default"}
	s, f, l := setup(t, busy, idle, stopped)
	quiet(f.Outputs, 9001, 9002)
	s.Usage = true
	s.Jobs = func(runner string) (Job, bool) {
		if runner != "gh-pve-default-9001" {
			return Job{}, false
		}
		return Job{Repo: "example-org/app", Name: "unit (2, 4)", Workflow: "example-org/app/.github/workflows/ci.yml@refs/heads/main", RunID: 7}, true
	}
	clock := now
	s.Now = func() time.Time { return clock }
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Ten seconds later the busy runner has moved 10 kB in and written 40 kB.
	clock = now.Add(10 * time.Second)
	f.Guests[0].NetIn, f.Guests[0].DiskWrite = 11000, 44000
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	usage := usageStreams(t, l.pushes[1])
	if len(usage) != 2 {
		t.Fatalf("usage streams: %+v", usage)
	}
	wantLabels := map[string]any{"service": "pve-runners-usage", "host": "pve1", "vmid": "9001", "runner": "gh-pve-default-9001",
		"pool": "default", "repo": "example-org/app"}
	if !reflect.DeepEqual(usage[0]["stream"], wantLabels) {
		t.Errorf("labels %v", usage[0]["stream"])
	}
	var got sample
	line := usage[0]["values"].([]any)[0].([]any)
	if line[0] != ns(-10*time.Second) {
		t.Errorf("timestamp %v", line[0])
	}
	if err := json.Unmarshal([]byte(line[1].(string)), &got); err != nil {
		t.Fatal(err)
	}
	want := sample{CPUCores: 2, CPULimit: 4, MemBytes: 2 << 30, MemLimit: 6 << 30, DiskBytes: 5 << 30, DiskLimit: 20 << 30,
		NetInBps: 1000, DiskWriteBps: 4000, Job: "unit (2, 4)", Workflow: "example-org/app/.github/workflows/ci.yml@refs/heads/main", RunID: 7}
	if got != want {
		t.Errorf("sample\n got %+v\nwant %+v", got, want)
	}
	// A runner with no job yet (a warm spare) is still sampled, without job labels.
	if _, ok := usage[1]["stream"].(map[string]any)["repo"]; ok {
		t.Errorf("idle runner labelled with a repo: %v", usage[1]["stream"])
	}
}

func TestUsageOffSendsNoSamples(t *testing.T) {
	s, f, l := setup(t, pve.Guest{VMID: 9001, Name: "gh-pve-default-9001", Type: "lxc", Status: "running", RawTags: "gh-runner"})
	quiet(f.Outputs, 9001)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(l.pushes) != 0 {
		t.Fatalf("pushed %v with usage off and no log lines", l.pushes)
	}
}

func TestRateIgnoresCounterResets(t *testing.T) {
	if r := rate(5000, 100, 10); r != 0 {
		t.Errorf("rate after reset = %v", r)
	}
}

// quiet makes the service's and the guests' journals empty, so only usage is pushed.
func quiet(out map[string]string, ids ...int) {
	out["journalctl -u pve-runners.service -o json --no-pager -n 1000"] = ""
	for _, id := range ids {
		out[fmt.Sprintf("pct exec %d -- journalctl -u gh-runner.service -o json --no-pager -n 1000", id)] = ""
	}
}

func usageStreams(t *testing.T, push map[string]any) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, st := range push["streams"].([]any) {
		m := st.(map[string]any)
		if m["stream"].(map[string]any)["service"] == UsageService {
			out = append(out, m)
		}
	}
	return out
}
