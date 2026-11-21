package docker_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/docker"
	"github.com/ServerTooling/proxmox-github-runners/internal/docker/dockertest"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

func host(f *dockertest.Fake) *docker.Host { return &docker.Host{R: f, IDRange: [2]int{9500, 9502}} }

func TestRunCmdIsExact(t *testing.T) {
	got := docker.RunCmd(docker.Spec{ID: 9500, Name: "gh-pve1-nas-9500", Image: "pve-runners/ubuntu-latest:x", Cores: 4,
		MemoryMB: 8192, Tags: []string{"gh-runner", "pool-nas"}, DNS: []string{"192.0.2.1"}})
	want := []string{"docker", "run", "-d", "--name", "gh-pve1-nas-9500", "--hostname", "gh-pve1-nas-9500",
		"--runtime", "sysbox-runc", "--label", "pve-runners.id=9500", "--label", "pve-runners.tags=gh-runner;pool-nas",
		"--cpus", "4", "--memory", "8192m", "--network", "gh-runners", "--restart", "no", "--dns", "192.0.2.1",
		"pve-runners/ubuntu-latest:x"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
}

func TestParseListMapsStates(t *testing.T) {
	gs := docker.ParseList([]byte("9500\tgh-a\trunning\tgh-runner;pool-nas\n9501\tgh-b\texited\tgh-runner\nbad line\n" +
		"x\tgh-c\trunning\tgh-runner\n"))
	if len(gs) != 2 || gs[0].Status != "running" || gs[1].Status != "stopped" || gs[0].TagValue("pool") != "nas" ||
		gs[0].Type != "docker" {
		t.Fatalf("got %+v", gs)
	}
}

func TestRunnersAndDestroyOnlyTouchOwned(t *testing.T) {
	f := dockertest.New()
	f.Containers = []*dockertest.Container{
		{ID: "9500", Name: "gh-a", State: "running", Tags: "gh-runner;pool-nas"},
		{ID: "9600", Name: "outside", State: "running", Tags: "gh-runner"}, // outside id_range
		{ID: "9501", Name: "untagged", State: "running", Tags: "pool-nas"},
	}
	h := host(f)
	rs, err := h.Runners(context.Background())
	if err != nil || len(rs) != 1 || rs[0].Name != "gh-a" {
		t.Fatalf("runners %+v %v", rs, err)
	}
	for _, id := range []int{9600, 9501} {
		if err := h.Destroy(context.Background(), id); !errors.Is(err, pve.ErrNotOwned) {
			t.Fatalf("destroy %d: %v", id, err)
		}
	}
	if err := h.Destroy(context.Background(), 9500); err != nil {
		t.Fatal(err)
	}
	if err := h.Destroy(context.Background(), 9502); err != nil { // already gone
		t.Fatal(err)
	}
	if m := f.Mutations(); !reflect.DeepEqual(m, []string{"docker rm -f gh-a"}) {
		t.Fatalf("mutations %q", m)
	}
	// Free IDs skip every labelled container, owned or not.
	if id, err := h.NextFreeID(context.Background()); err != nil || id != 9500 {
		t.Fatalf("next id %d %v", id, err)
	}
}

func TestPushSendsTheSecretOnStdinOnly(t *testing.T) {
	f := dockertest.New()
	if err := host(f).Push(context.Background(), "gh-a", "s3cret", "/etc/gh-runner/jitconfig", "0600"); err != nil {
		t.Fatal(err)
	}
	for line, in := range f.Stdin {
		if strings.Contains(line, "s3cret") || in != "s3cret" {
			t.Fatalf("%q <- %q", line, in)
		}
	}
	if len(f.Stdin) != 1 {
		t.Fatalf("stdin %v", f.Stdin)
	}
}

func TestCheckRuntimeNeedsSysbox(t *testing.T) {
	f := dockertest.New()
	if err := host(f).CheckRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.Runtimes = []string{"runc"}
	if err := host(f).CheckRuntime(context.Background()); !errors.Is(err, docker.ErrNoSysbox) {
		t.Fatal(err)
	}
}

func TestEnsureNetwork(t *testing.T) {
	f := dockertest.New()
	h := host(f)
	if err := h.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.EnsureNetwork(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"docker network create --driver bridge --opt com.docker.network.bridge.name=ghr0 " +
		"--opt com.docker.network.bridge.enable_icc=false --label pve-runners=1 gh-runners"}
	if m := f.Mutations(); !reflect.DeepEqual(m, want) {
		t.Fatalf("mutations %q", m)
	}
	f.Bridge = "br-123"
	if err := h.EnsureNetwork(context.Background()); err == nil {
		t.Fatal("a network on another bridge must be refused: the isolation rules match ghr0")
	}
}

func TestIsolationRules(t *testing.T) {
	rules := pve.EgressRules([]string{"10.0.0.0/8", "fc00::/7"}, []pve.AllowRule{{Proto: "udp", Port: 53}})
	want := `*mangle
:PVE-RUNNERS-FWD - [0:0]
-F PVE-RUNNERS-FWD
:PVE-RUNNERS-IN - [0:0]
-F PVE-RUNNERS-IN
-A PVE-RUNNERS-FWD -d 10.0.0.0/8 -p udp --dport 53 -j RETURN
-A PVE-RUNNERS-IN -d 10.0.0.0/8 -p udp --dport 53 -j RETURN
-A PVE-RUNNERS-FWD -d 10.0.0.0/8 -j DROP
-A PVE-RUNNERS-IN -j DROP
COMMIT
`
	if got := docker.IsolationRules(rules); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
}

func TestIsolateRunsTheHelperWithRulesOnStdin(t *testing.T) {
	f := dockertest.New()
	err := host(f).Isolate(context.Background(), "pve-runners/ubuntu-latest:x", pve.EgressRules([]string{"10.0.0.0/8"}, nil))
	if err != nil {
		t.Fatal(err)
	}
	m := f.Mutations()
	if len(m) != 1 || !strings.HasPrefix(m[0], "docker run --rm -i --network host --cap-add NET_ADMIN --cap-add NET_RAW "+
		"--runtime runc --entrypoint /bin/sh pve-runners/ubuntu-latest:x -c set -e") || !strings.Contains(f.Stdin[m[0]], "-j DROP") {
		t.Fatalf("mutations %q stdin %q", m, f.Stdin)
	}
	f.Output["--network host"] = "something else"
	if err := host(f).Isolate(context.Background(), "img", nil); err == nil {
		t.Fatal("a helper that didn't confirm must fail the spawn")
	}
}

func TestSSHArgsQuoteTheRemoteCommand(t *testing.T) {
	s := docker.SSH{Target: "ghrunner@192.0.2.20", Port: 2222, KeyFile: "/k", KnownHosts: "/kh"}
	got := s.Args("docker", "exec", "gh-a", "sh", "-c", `echo "$1" it's`)
	want := []string{"-i", "/k", "-p", "2222", "-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=/kh", "-o", "ConnectTimeout=15",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4", "-T", "ghrunner@192.0.2.20", "--",
		`docker exec gh-a sh -c 'echo "$1" it'\''s'`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %q\nwant %q", got, want)
	}
	if q := docker.Quote(""); q != "''" {
		t.Fatalf("empty: %s", q)
	}
}
