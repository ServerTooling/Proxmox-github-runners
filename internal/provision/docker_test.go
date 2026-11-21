package provision

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/config"
	"github.com/ServerTooling/proxmox-github-runners/internal/docker"
	"github.com/ServerTooling/proxmox-github-runners/internal/docker/dockertest"
)

const nasImage = "pve-runners/ubuntu-latest:ubuntu-latest-20261117-9"

func dockerSetup(t *testing.T) (*Provisioner, *dockertest.Fake) {
	t.Helper()
	p, f, gh := setup(t)
	gh.RunnerGroups["Example-Org"] = gh.RunnerGroups["example-org"]
	p.Cfg.Docker.SSH = "ghrunner@192.0.2.20"
	p.Cfg.Pools["nas"] = &config.Pool{Type: "docker", Image: "ubuntu-latest", Mode: "ephemeral", Labels: []string{"nas"},
		Cores: 4, MemoryMB: 8192, Max: 2}
	d := dockertest.New()
	d.Images = []string{nasImage}
	p.Docker = &docker.Host{R: d, IDRange: p.Cfg.Docker.IDRange}
	p.DockerImage = func(context.Context, string) (string, error) { return nasImage, nil }
	t.Cleanup(func() {
		if m := f.Mutations(); len(m) > 0 {
			t.Errorf("a docker spawn changed the Proxmox host: %q", m)
		}
	})
	return p, d
}

func TestDockerSpawnRunsExactSequence(t *testing.T) {
	p, d := dockerSetup(t)
	var steps []string
	res, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org,
		Step: func(id int, s string) { steps = append(steps, s) }})
	if err != nil {
		t.Fatal(err)
	}
	if res.VMID != 9500 || res.Name != "gh-pve1-nas-9500" || res.RunnerID == 0 {
		t.Fatalf("got %+v", res)
	}
	m := d.Mutations()
	if len(m) != 5 {
		t.Fatalf("mutations %q", m)
	}
	if !strings.HasPrefix(m[1], "docker run --rm -i --network host ") {
		t.Fatalf("isolation must come before the runner exists: %q", m)
	}
	want := []string{
		"docker network create --driver bridge --opt com.docker.network.bridge.name=ghr0 " +
			"--opt com.docker.network.bridge.enable_icc=false --label pve-runners=1 gh-runners",
		m[1],
		"docker run -d --name gh-pve1-nas-9500 --hostname gh-pve1-nas-9500 --runtime sysbox-runc " +
			"--label pve-runners.id=9500 --label pve-runners.tags=gh-runner;pool-nas;owner-example-org;mode-ephemeral;" +
			"from-0;created-1700000000;runnerid-1 --cpus 4 --memory 8192m --network gh-runners --restart no " + nasImage,
		"docker exec -i gh-pve1-nas-9500 sh -c " + `umask 077 && mkdir -p "$(dirname "$1")" && cat > "$1" && chmod "$2" "$1"` +
			" sh /etc/gh-runner/jitconfig 0600",
		"docker exec gh-pve1-nas-9500 sh -c " + startRunner,
	}
	if !reflect.DeepEqual(m, want) {
		t.Fatalf("commands\n got %q\nwant %q", m, want)
	}
	if jit := d.Stdin[m[3]]; jit == "" || strings.Contains(strings.Join(d.Calls, "\n"), jit) {
		t.Fatal("the JIT config must reach the container on stdin only")
	}
	if !reflect.DeepEqual(steps, []string{"provisioning", "files pushed", "service started"}) {
		t.Fatalf("steps %q", steps)
	}
}

func TestDockerSpawnRespectsCapacity(t *testing.T) {
	p, d := dockerSetup(t)
	for range 2 {
		if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("pool max: %v", err)
	}
	p.Cfg.Pools["nas"].Max = 5
	p.Cfg.Docker.MaxRunners = 2
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); err == nil ||
		!strings.Contains(err.Error(), "docker.max_runners") {
		t.Fatalf("machine max: %v", err)
	}
	if len(d.Containers) != 2 {
		t.Fatalf("containers %d", len(d.Containers))
	}
}

func TestDockerSpawnRefusesWithoutSysboxOrIsolation(t *testing.T) {
	p, d := dockerSetup(t)
	d.Runtimes = []string{"runc"}
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); !errors.Is(err, docker.ErrNoSysbox) {
		t.Fatalf("no sysbox: %v", err)
	}
	d.Runtimes = []string{"sysbox-runc"}
	d.Fail["--network host"] = errors.New("iptables: permission denied")
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); err == nil ||
		!strings.Contains(err.Error(), "so none start") {
		t.Fatalf("isolation failure: %v", err)
	}
	if len(d.Containers) != 0 {
		t.Fatal("no runner may start when isolation failed")
	}
}

func TestDockerSpawnSkipsIsolationWhenTurnedOff(t *testing.T) {
	p, d := dockerSetup(t)
	off := false
	p.Cfg.Network.Isolate = &off
	p.Cfg.Network.DNS = []string{"192.0.2.53"}
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); err != nil {
		t.Fatal(err)
	}
	for _, c := range d.Calls {
		if strings.Contains(c, "--network host") {
			t.Fatalf("isolation ran though it is off: %q", c)
		}
	}
	if !strings.Contains(strings.Join(d.Calls, "\n"), "--dns 192.0.2.53") {
		t.Fatal("network.dns not passed to the container")
	}
}

func TestDockerSpawnRemovesTheContainerOnFailure(t *testing.T) {
	p, d := dockerSetup(t)
	d.Fail["systemctl start gh-runner"] = errors.New("unit failed")
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); err == nil {
		t.Fatal("expected failure")
	}
	if len(d.Containers) != 0 {
		t.Fatalf("container left behind: %+v", d.Containers[0])
	}
}

func TestDockerSpawnNeedsTheMachine(t *testing.T) {
	p, _ := dockerSetup(t)
	p.Docker = nil
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); !errors.Is(err, ErrNoDocker) {
		t.Fatal(err)
	}
}

func TestDockerSpawnWaitsWhileDraining(t *testing.T) {
	p, d := dockerSetup(t)
	release, err := Drain(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := p.Spawn(context.Background(), Request{Pool: "nas", Target: org}); !errors.Is(err, ErrDraining) {
		t.Fatal(err)
	}
	if len(d.Containers) != 0 {
		t.Fatal("spawned while draining")
	}
}
