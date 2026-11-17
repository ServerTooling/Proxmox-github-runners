package images

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/pve/pvetest"
	"github.com/ServerTooling/proxmox-github-runners/internal/update"
)

const now = 1790000000

type fixture struct {
	srv       *httptest.Server
	priv      ed25519.PrivateKey
	files     map[string][]byte
	rels      []update.Release
	offline   bool
	downloads atomic.Int32
	pve       *pvetest.Fake
	m         *Manager
}

func newFixture(t *testing.T, guests ...pve.Guest) *fixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	f := &fixture{priv: priv, files: map[string][]byte{}, pve: pvetest.New(guests...)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if f.offline {
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/releases") {
			_ = json.NewEncoder(w).Encode(f.rels)
			return
		}
		if tag, ok := strings.CutPrefix(r.URL.Path, "/repos/example/repo/releases/tags/"); ok {
			for _, rel := range f.rels {
				if rel.Tag == tag {
					_ = json.NewEncoder(w).Encode(rel)
					return
				}
			}
			http.NotFound(w, r)
			return
		}
		b, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".tar.zst") {
			f.downloads.Add(1)
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	f.m = &Manager{Repo: "example/repo", APIBase: f.srv.URL, HTTP: f.srv.Client(),
		PublicKey: base64.StdEncoding.EncodeToString(der), GOARCH: "amd64",
		Host: &pve.Host{R: f.pve, Node: "pve1", VMIDRange: [2]int{9000, 9010}}, Range: [2]int{8900, 8905},
		CacheDir: t.TempDir(), CacheStorage: "local", Storage: "local-lvm", Pool: "gh-runners", Bridge: "vmbr0",
		RootfsGB: 16, Retention: 30 * 24 * time.Hour, Now: func() time.Time { return time.Unix(now, 0) }, StateDir: t.TempDir()}
	return f
}

// publish adds an image release; tamper serves an archive that doesn't match the signed sums.
func (f *fixture) publish(tag, name string, at int64, tamper bool) {
	archive := "rootfs of " + tag
	asset := AssetName(name, "amd64")
	sum := sha256.Sum256([]byte(archive))
	sums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), asset)
	base := "/dl/" + tag + "/"
	if tamper {
		archive += "evil"
	}
	f.files[base+asset] = []byte(archive)
	f.files[base+update.SumsAsset] = []byte(sums)
	f.files[base+update.SigAsset] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(f.priv, []byte(sums))))
	var assets []update.Asset
	for _, n := range []string{asset, update.SumsAsset, update.SigAsset} {
		assets = append(assets, update.Asset{Name: n, URL: f.srv.URL + base + n})
	}
	f.rels = append(f.rels, update.Release{Tag: tag, Published: time.Unix(at, 0), Assets: assets})
}

func TestEnsureDownloadsVerifiesAndCreatesTemplate(t *testing.T) {
	f := newFixture(t)
	f.publish("template-ubuntu-latest-20261001-1", "ubuntu-latest", now-700000, false)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, false)
	// A different image whose name merely starts the same, and a tool release, are never picked.
	f.publish("template-ubuntu-latest-gpu-20261009-3", "ubuntu-latest-gpu", now-50000, false)
	f.rels = append(f.rels, update.Release{Tag: "build-9", Published: time.Unix(now, 0)})

	id, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"pct create 8900 local:vztmpl/pve-runners-template-ubuntu-latest-20261008-2-amd64.tar.zst --hostname gh-template-ubuntu-latest " +
			"--pool gh-runners --rootfs local-lvm:16 --unprivileged 1 --features nesting=1,keyctl=1 --cores 2 --memory 2048 " +
			"--net0 name=eth0,bridge=vmbr0,ip=dhcp,firewall=1 --onboot 0 " +
			"--tags gh-template;image-ubuntu-latest;rel-ubuntu-latest-20261008-2;store-local-lvm;created-1790000000 " +
			"--description Runner template template-ubuntu-latest-20261008-2 managed by pve-runners",
		"pct template 8900",
	}
	if m := f.pve.Mutations(); id != 8900 || !reflect.DeepEqual(m, want) {
		t.Fatalf("id %d, commands\n got %q\nwant %q", id, m, want)
	}
	b, err := os.ReadFile(filepath.Join(f.m.CacheDir, "pve-runners-template-ubuntu-latest-20261008-2-amd64.tar.zst"))
	if err != nil || string(b) != "rootfs of template-ubuntu-latest-20261008-2" {
		t.Fatalf("cached archive: %q %v", b, err)
	}

	// Already there: no download, no new guest.
	if id2, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err != nil || id2 != 8900 {
		t.Fatalf("second ensure: %d %v", id2, err)
	}
	if len(f.pve.Mutations()) != 2 || f.downloads.Load() != 1 {
		t.Fatalf("second ensure did work: %q, %d downloads", f.pve.Mutations(), f.downloads.Load())
	}

	// A pinned ref gets exactly that release, as a second template.
	id3, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest@template-ubuntu-latest-20261001-1"))
	if err != nil || id3 != 8901 {
		t.Fatalf("pinned: %d %v", id3, err)
	}
	if m := f.pve.Mutations(); !strings.Contains(m[2], "pve-runners-template-ubuntu-latest-20261001-1-amd64.tar.zst") {
		t.Fatalf("pinned create: %q", m[2])
	}
}

func TestEnsureCreatesMissingResourcePool(t *testing.T) {
	f := newFixture(t)
	f.pve.ResourcePools = nil
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, false)
	if _, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err != nil {
		t.Fatal(err)
	}
	m := f.pve.Mutations()
	if m[0] != "pvesh create /pools --poolid gh-runners --comment GitHub runners managed by pve-runners" ||
		!strings.HasPrefix(m[1], "pct create 8900 ") {
		t.Fatalf("commands: %q", m)
	}
}

func TestEnsureRefusesUnverifiedImages(t *testing.T) {
	f := newFixture(t)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now, true)
	if _, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered archive: %v", err)
	}
	f = newFixture(t)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now, false)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	f.priv = other
	f.rels = nil
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now, false) // signed with the wrong key
	if _, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("wrong key: %v", err)
	}
	if len(f.pve.Mutations()) != 0 {
		t.Fatalf("nothing may be created from an unverified image: %q", f.pve.Mutations())
	}
	if files, _ := os.ReadDir(f.m.CacheDir); len(files) != 0 {
		t.Fatalf("unverified download left in cache: %v", files)
	}
}

func TestEnsureFallsBackToLocalTemplateWhenOffline(t *testing.T) {
	tpl := func(id int, rel string, at int64) pve.Guest {
		return pve.Guest{VMID: id, Type: "lxc", Template: 1,
			RawTags: fmt.Sprintf("gh-template;image-ubuntu-latest;rel-%s;created-%d", rel, at)}
	}
	f := newFixture(t, tpl(8900, "ubuntu-latest-20261001-1", now-1000), tpl(8901, "ubuntu-latest-20261008-2", now-10))
	f.offline = true
	if id, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err != nil || id != 8901 {
		t.Fatalf("offline: %d %v", id, err)
	}
	if id, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest@template-ubuntu-latest-20261001-1")); err != nil || id != 8900 {
		t.Fatalf("offline pinned: %d %v", id, err)
	}
	if _, err := f.m.Ensure(context.Background(), ParseRef("other")); err == nil {
		t.Fatal("offline with no local template must fail")
	}
}

func TestPruneKeepsNewestPinnedAndInUse(t *testing.T) {
	day := int64(24 * 3600)
	g := func(id, tpl int, tags string) pve.Guest {
		return pve.Guest{VMID: id, Type: "lxc", Template: tpl, RawTags: tags}
	}
	f := newFixture(t,
		g(8900, 1, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;rel-ubuntu-latest-20260101-1;created-%d", now-90*day)), // old: removed
		g(8901, 1, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;rel-ubuntu-latest-20260201-2;created-%d", now-60*day)), // runners use it
		g(8902, 1, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;rel-ubuntu-latest-20260301-3;created-%d", now-50*day)), // pinned
		g(8903, 1, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;rel-ubuntu-latest-20261001-4;created-%d", now-5*day)),  // recent
		g(8904, 1, fmt.Sprintf("gh-template;store-local-lvm;image-other;rel-other-20250101-1;created-%d", now-400*day)),                // newest of its name
		g(8905, 0, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;rel-ubuntu-latest-20261009-5;created-%d", now-3600)),   // never finished
		// Just converted, but /cluster/resources still shows it as a container: not "incomplete" yet.
		g(8906, 0, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;rel-ubuntu-latest-20261009-6;created-%d", now-5)),
		g(8000, 1, fmt.Sprintf("gh-template;store-local-lvm;image-ubuntu-latest;created-%d", now-900*day)), // outside range
	)
	f.m.Range = [2]int{8900, 8906}
	old := time.Unix(now-90*day, 0)
	for _, rel := range []string{"20260101-1", "20260301-3", "20261001-4"} {
		p := filepath.Join(f.m.CacheDir, "pve-runners-template-ubuntu-latest-"+rel+"-amd64.tar.zst")
		_ = os.WriteFile(p, []byte("x"), 0o644)
		_ = os.Chtimes(p, old, old)
	}
	removed, err := f.m.Prune(context.Background(), []Ref{ParseRef("ubuntu-latest@template-ubuntu-latest-20260301-3")}, []int{8901})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"pct destroy 8900 --purge 1 --force 1", "pct destroy 8905 --purge 1 --force 1"}
	if m := f.pve.Mutations(); !reflect.DeepEqual(m, want) {
		t.Fatalf("commands\n got %q\nwant %q", m, want)
	}
	var files []string
	for _, r := range removed {
		if strings.HasSuffix(r.What, ".tar.zst") {
			files = append(files, r.What)
		}
	}
	if !reflect.DeepEqual(files, []string{"pve-runners-template-ubuntu-latest-20260101-1-amd64.tar.zst"}) {
		t.Fatalf("archives removed: %v", removed)
	}
	if _, err := os.Stat(filepath.Join(f.m.CacheDir, "pve-runners-template-ubuntu-latest-20261001-4-amd64.tar.zst")); err != nil {
		t.Fatal("archive of a kept template was deleted")
	}
}

func TestStorageChangeMovesTheTemplate(t *testing.T) {
	tpl := func(id int, tags string) pve.Guest {
		return pve.Guest{VMID: id, Type: "lxc", Template: 1,
			RawTags: fmt.Sprintf("gh-template;image-ubuntu-latest;rel-ubuntu-latest-20261008-2;%screated-%d", tags, now-10)}
	}
	// 8900 predates the store tag, 8901 is on the old storage; runners cloned from 8901 still run.
	f := newFixture(t, tpl(8900, ""), tpl(8901, "store-local-zfs;"))
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, false)
	id, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest"))
	if err != nil || id != 8902 {
		t.Fatalf("ensure: %d %v", id, err)
	}
	if m := f.pve.Mutations(); len(m) != 2 || !strings.Contains(m[0], "--rootfs local-lvm:16 ") ||
		!strings.Contains(m[0], ";store-local-lvm;") {
		t.Fatalf("new template not made on local-lvm: %q", m)
	}
	if id, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err != nil || id != 8902 {
		t.Fatalf("second ensure: %d %v", id, err)
	}
	removed, err := f.m.Prune(context.Background(), []Ref{ParseRef("ubuntu-latest")}, []int{8901})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].What != "template 8900 (ubuntu-latest-20261008-2)" ||
		removed[0].Reason != "not on proxmox.storage local-lvm" {
		t.Fatalf("removed %+v; the template in use must stay until its runners are gone", removed)
	}
}

func TestConcurrentManagersCreateOneTemplate(t *testing.T) {
	f := newFixture(t)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, false)
	other := *f.m // the image sync and a spawn each build their own Manager
	ids := make(chan int, 2)
	for _, m := range []*Manager{f.m, &other} {
		go func() {
			id, err := m.Ensure(context.Background(), ParseRef("ubuntu-latest"))
			if err != nil {
				t.Error(err)
			}
			ids <- id
		}()
	}
	if a, b := <-ids, <-ids; a != 8900 || b != 8900 {
		t.Fatalf("templates %d and %d", a, b)
	}
	creates := 0
	for _, m := range f.pve.Mutations() {
		if strings.HasPrefix(m, "pct create ") {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("%d templates created: %q", creates, f.pve.Mutations())
	}
}

func TestPruneRemovesDuplicateTemplates(t *testing.T) {
	tpl := func(id int, at int64) pve.Guest {
		return pve.Guest{VMID: id, Type: "lxc", Template: 1,
			RawTags: fmt.Sprintf("gh-template;image-ubuntu-latest;rel-ubuntu-latest-20261008-2;store-local-lvm;created-%d", at)}
	}
	f := newFixture(t, tpl(8900, now-60), tpl(8901, now-10), tpl(8902, now-100))
	removed, err := f.m.Prune(context.Background(), []Ref{ParseRef("ubuntu-latest")}, []int{8902})
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 1 || removed[0].What != "template 8900 (ubuntu-latest-20261008-2)" ||
		removed[0].Reason != "a newer copy of the same release exists" {
		t.Fatalf("removed %+v", removed)
	}
}

func TestDestroyTemplateRefusesGuestsItDoesNotOwn(t *testing.T) {
	f := newFixture(t, pve.Guest{VMID: 8900, Type: "lxc", Template: 1, RawTags: "something-else"},
		pve.Guest{VMID: 9001, Type: "lxc", Template: 1, RawTags: "gh-template"})
	for _, id := range []int{8900, 9001} {
		if err := f.m.Host.DestroyTemplate(context.Background(), id, f.m.Range); err == nil {
			t.Fatalf("destroyed %d", id)
		}
	}
	if len(f.pve.Mutations()) != 0 {
		t.Fatalf("%q", f.pve.Mutations())
	}
}

func TestInUse(t *testing.T) {
	got := InUse([]pve.Guest{{RawTags: "gh-runner;from-8901"}, {RawTags: "gh-runner"}})
	if !reflect.DeepEqual(got, []int{8901}) {
		t.Fatalf("%v", got)
	}
}

// interrupted is what pct create leaves when it is killed before writing the config.
func interrupted(id int) pve.Guest {
	return pve.Guest{VMID: id, Type: "lxc", Status: "stopped", Lock: "create"}
}

func TestFailedCreateRemovesItsLockedContainer(t *testing.T) {
	f := newFixture(t)
	f.publish("template-ubuntu-latest-20261008-2", "ubuntu-latest", now-100000, false)
	f.pve.FailOn["pct create 8900"] = errors.New("extracting archive failed")
	f.pve.Before = func(line string) {
		if strings.HasPrefix(line, "pct create 8900 ") {
			f.pve.Guests = append(f.pve.Guests, interrupted(8900))
		}
	}
	if _, err := f.m.Ensure(context.Background(), ParseRef("ubuntu-latest")); err == nil {
		t.Fatal("ensure succeeded")
	}
	m := f.pve.Mutations()
	if want := []string{"pct unlock 8900", "pct destroy 8900 --purge 1 --force 1"}; len(m) != 3 || !reflect.DeepEqual(m[1:], want) {
		t.Fatalf("got %q, want the create then %q", m, want)
	}
	if _, err := os.Stat(f.m.creatingFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mark left behind: %v", err)
	}
}

func TestPruneRemovesACreateCutOffByARestart(t *testing.T) {
	// 8901 looks the same but this tool never recorded creating it, so it stays.
	f := newFixture(t, interrupted(8900), interrupted(8901))
	if err := os.WriteFile(f.m.creatingFile(), []byte("8900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Prune(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if m, want := f.pve.Mutations(), []string{"pct unlock 8900", "pct destroy 8900 --purge 1 --force 1"}; !reflect.DeepEqual(m, want) {
		t.Fatalf("got %q, want %q", m, want)
	}
	if _, ok := f.pve.Guest(8901); !ok {
		t.Fatal("8901 was removed")
	}
	if _, err := os.Stat(f.m.creatingFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mark left behind: %v", err)
	}
}

func TestRecordedCreateThatFinishedIsLeftToPrune(t *testing.T) {
	// The create finished (tags written) but the service stopped before clearing the mark.
	g := pve.Guest{VMID: 8900, Type: "lxc", Template: 1,
		RawTags: fmt.Sprintf("gh-template;image-ubuntu-latest;rel-ubuntu-latest-20261008-2;store-local-lvm;created-%d", now-60)}
	f := newFixture(t, g)
	if err := os.WriteFile(f.m.creatingFile(), []byte("8900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.m.Prune(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if m := f.pve.Mutations(); len(m) != 0 {
		t.Fatalf("finished template touched: %q", m)
	}
	if _, err := os.Stat(f.m.creatingFile()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mark left behind: %v", err)
	}
}

func TestPruneUnlocksAnUnconvertedTemplateBeforeDestroying(t *testing.T) {
	g := pve.Guest{VMID: 8900, Type: "lxc", Lock: "create",
		RawTags: fmt.Sprintf("gh-template;image-ubuntu-latest;rel-ubuntu-latest-20261008-2;store-local-lvm;created-%d", now-3600)}
	f := newFixture(t, g)
	if _, err := f.m.Prune(context.Background(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if m, want := f.pve.Mutations(), []string{"pct unlock 8900", "pct destroy 8900 --purge 1 --force 1"}; !reflect.DeepEqual(m, want) {
		t.Fatalf("got %q, want %q", m, want)
	}
}
