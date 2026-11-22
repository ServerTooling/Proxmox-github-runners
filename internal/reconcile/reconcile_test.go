package reconcile

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
)

func TestOnceDestroysOnlyFinishedOrExpiredOwnedGuests(t *testing.T) {
	now := time.Unix(1700010000, 0)
	old := "created-1700000000"   // ~2.8h ago
	fresh := "created-1700009990" // 10s ago
	f := pvetest.New(
		pve.Guest{VMID: 9000, Type: "lxc", Status: "stopped", RawTags: "gh-runner;pool-small;mode-ephemeral;" + old},
		pve.Guest{VMID: 9001, Type: "lxc", Status: "stopped", RawTags: "gh-runner;pool-small;mode-ephemeral;" + fresh},
		pve.Guest{VMID: 9002, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-small;mode-ephemeral;" + fresh},
		pve.Guest{VMID: 9003, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-small;mode-ephemeral;" + old},
		pve.Guest{VMID: 9004, Type: "lxc", Status: "stopped", RawTags: "gh-runner;pool-perm;mode-permanent;" + old},
		pve.Guest{VMID: 9005, Type: "lxc", Status: "stopped", RawTags: "pool-small;mode-ephemeral;" + old}, // not ours
		pve.Guest{VMID: 120, Type: "lxc", Status: "stopped", RawTags: "gh-runner;mode-ephemeral;" + old},   // out of range
	)
	cfg := config.Default()
	cfg.Pools["small"] = &config.Pool{MaxLifetime: 2 * time.Hour}
	cfg.Pools["perm"] = &config.Pool{}
	h := &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9010}}

	done, err := Once(context.Background(), cfg, h, now)
	if err != nil {
		t.Fatal(err)
	}
	got := []int{}
	for _, a := range done {
		got = append(got, a.VMID)
	}
	if !reflect.DeepEqual(got, []int{9000, 9003}) {
		t.Fatalf("destroyed %v, want [9000 9003]", got)
	}
	want := []string{"pct destroy 9000 --purge 1 --force 1", "pct stop 9003", "pct destroy 9003 --purge 1 --force 1"}
	if !reflect.DeepEqual(f.Mutations(), want) {
		t.Fatalf("got %q", f.Mutations())
	}
}

func TestOnceDestroysRunningGuestWhoseRunnerEnded(t *testing.T) {
	now := time.Unix(1700010000, 0)
	old := "created-1700000000"   // ~2.8h ago, past StartGrace
	young := "created-1700009900" // 100s ago, still starting
	f := pvetest.New(
		pve.Guest{VMID: 9000, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-small;mode-ephemeral;" + old},
		pve.Guest{VMID: 9001, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-small;mode-ephemeral;" + old},
		pve.Guest{VMID: 9002, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-small;mode-ephemeral;" + old},
		pve.Guest{VMID: 9003, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-small;mode-ephemeral;" + young},
		pve.Guest{VMID: 9004, Type: "lxc", Status: "running", RawTags: "gh-runner;pool-perm;mode-permanent;" + old},
	)
	// 9000 was started again after its runner exited; 9001 is running a job; 9002's runner crashed;
	// 9003 is too new to judge; permanent runners are never checked.
	f.UnitState = map[int]string{9000: "inactive", 9002: "failed", 9003: "inactive", 9004: "inactive"}
	cfg := config.Default()
	cfg.Pools["small"] = &config.Pool{}
	cfg.Pools["perm"] = &config.Pool{}
	h := &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9010}}

	done, err := Once(context.Background(), cfg, h, now)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{{9000, "runner service inactive"}, {9002, "runner service failed"}}
	if !reflect.DeepEqual(done, want) {
		t.Fatalf("destroyed %+v, want %+v", done, want)
	}
	wantCmds := []string{
		"pct exec 9000 -- systemctl is-active gh-runner.service",
		"pct exec 9001 -- systemctl is-active gh-runner.service",
		"pct exec 9002 -- systemctl is-active gh-runner.service",
		"pct stop 9000", "pct destroy 9000 --purge 1 --force 1",
		"pct stop 9002", "pct destroy 9002 --purge 1 --force 1",
	}
	if !reflect.DeepEqual(f.Mutations(), wantCmds) {
		t.Fatalf("got %q", f.Mutations())
	}
}

// A reused runner's unit is stopped while it waits; only a kept guest past its time is cleaned up.
func TestOnceLeavesKeptAndArmedRunnersAlone(t *testing.T) {
	now := time.Unix(1700010000, 0)
	old := "gh-runner;pool-small;mode-ephemeral;created-1700000000;"
	f := pvetest.New(
		pve.Guest{VMID: 9000, Type: "lxc", Status: "running", RawTags: old + "kept-1700010600"},
		pve.Guest{VMID: 9001, Type: "lxc", Status: "running", RawTags: old + "kept-1700009000"},
		pve.Guest{VMID: 9002, Type: "lxc", Status: "running", RawTags: old + "armed-1700009990"},
		pve.Guest{VMID: 9003, Type: "lxc", Status: "running", RawTags: old + "armed-1700000000"},
	)
	f.UnitState = map[int]string{9000: "inactive", 9001: "inactive", 9002: "inactive", 9003: "inactive"}
	cfg := config.Default()
	cfg.Pools["small"] = &config.Pool{ReuseFor: 30 * time.Minute}
	h := &pve.Host{R: f, Node: "pve1", VMIDRange: [2]int{9000, 9010}}
	destroyed := func(now time.Time) []int {
		t.Helper()
		done, err := Once(context.Background(), cfg, h, now)
		if err != nil {
			t.Fatal(err)
		}
		got := []int{}
		for _, a := range done {
			got = append(got, a.VMID)
		}
		return got
	}
	// 9003's unit stopped without a kept tag: its runner may have just finished a job, and the
	// autoscaler tags it kept within seconds, so it gets ReuseSettle before it counts as dead.
	if got := destroyed(now); !reflect.DeepEqual(got, []int{9001}) {
		t.Fatalf("destroyed %v, want [9001]", got)
	}
	if got := destroyed(now.Add(ReuseSettle - time.Second)); len(got) != 0 {
		t.Fatalf("destroyed %v before ReuseSettle", got)
	}
	if got := destroyed(now.Add(ReuseSettle)); !reflect.DeepEqual(got, []int{9003}) {
		t.Fatalf("destroyed %v, want [9003]", got)
	}
}
