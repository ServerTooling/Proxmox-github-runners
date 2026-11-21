package images

import (
	"context"
	"io"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/ServerTooling/proxmox-github-runners/internal/docker"
	"github.com/ServerTooling/proxmox-github-runners/internal/docker/dockertest"
)

// plainDecompress stands in for zstd: the fixture's archives are plain text.
func plainDecompress(t *testing.T) {
	old := Decompress
	Decompress = func(_ context.Context, path string) (io.ReadCloser, func() error, error) {
		f, err := os.Open(path)
		return f, func() error { return nil }, err
	}
	t.Cleanup(func() { Decompress = old })
}

func TestEnsureDockerImportsTheVerifiedArchiveOnce(t *testing.T) {
	plainDecompress(t)
	f := newFixture(t)
	f.publish("template-ubuntu-latest-20261001-1", "ubuntu-latest", now-700000, false)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, false)
	d := dockertest.New()
	d.Images = []string{"pve-runners/ubuntu-latest:ubuntu-latest-20260901-7", "pve-runners/ubuntu-latest:ubuntu-latest-20260801-3",
		"postgres:16"}
	h := &docker.Host{R: d, IDRange: [2]int{9500, 9599}}
	pinned := []Ref{ParseRef("ubuntu-latest@template-ubuntu-latest-20260801-3")}
	tag, err := f.m.EnsureDocker(context.Background(), ParseRef("ubuntu-latest"), h, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "pve-runners/ubuntu-latest:ubuntu-latest-20261008-2" {
		t.Fatalf("tag %s", tag)
	}
	imp := "docker import --change ENTRYPOINT [\"/sbin/init\"] --change STOPSIGNAL SIGRTMIN+3 - " + tag
	want := []string{imp, "docker image rm pve-runners/ubuntu-latest:ubuntu-latest-20260901-7"}
	if m := d.Mutations(); !reflect.DeepEqual(m, want) {
		t.Fatalf("mutations\n got %q\nwant %q", m, want)
	}
	if d.Stdin[imp] != "rootfs of template-ubuntu-latest-20261008-2" {
		t.Fatalf("imported %q", d.Stdin[imp])
	}
	// The pinned release and other repositories stay.
	if !reflect.DeepEqual(d.Images, []string{tag, "pve-runners/ubuntu-latest:ubuntu-latest-20260801-3", "postgres:16"}) {
		t.Fatalf("images %q", d.Images)
	}
	if _, err := f.m.EnsureDocker(context.Background(), ParseRef("ubuntu-latest"), h, pinned); err != nil {
		t.Fatal(err)
	}
	if n := len(d.Mutations()); n != 2 || f.downloads.Load() != 1 {
		t.Fatalf("second call imported again: %d mutations, %d downloads", n, f.downloads.Load())
	}
}

func TestEnsureDockerRefusesUnverifiedImages(t *testing.T) {
	plainDecompress(t)
	f := newFixture(t)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, true)
	d := dockertest.New()
	_, err := f.m.EnsureDocker(context.Background(), ParseRef("ubuntu-latest"), &docker.Host{R: d}, nil)
	if err == nil || !strings.Contains(err.Error(), "checksum") || len(d.Mutations()) != 0 {
		t.Fatalf("err %v, mutations %q", err, d.Mutations())
	}
}

func TestEnsureDockerFallsBackToTheNewestImportWhenOffline(t *testing.T) {
	f := newFixture(t)
	f.offline = true
	d := dockertest.New()
	d.Images = []string{"pve-runners/ubuntu-latest:ubuntu-latest-20261008-2", "pve-runners/ubuntu-latest:ubuntu-latest-20261001-1"}
	h := &docker.Host{R: d}
	tag, err := f.m.EnsureDocker(context.Background(), ParseRef("ubuntu-latest"), h, nil)
	if err != nil || tag != "pve-runners/ubuntu-latest:ubuntu-latest-20261008-2" {
		t.Fatalf("%s %v", tag, err)
	}
	tag, err = f.m.EnsureDocker(context.Background(), ParseRef("ubuntu-latest@template-ubuntu-latest-20261001-1"), h, nil)
	if err != nil || tag != "pve-runners/ubuntu-latest:ubuntu-latest-20261001-1" {
		t.Fatalf("pinned: %s %v", tag, err)
	}
	if _, err := f.m.EnsureDocker(context.Background(), ParseRef("other"), h, nil); err == nil {
		t.Fatal("an image never imported can't be used offline")
	}
}
