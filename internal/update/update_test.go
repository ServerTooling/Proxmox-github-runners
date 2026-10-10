package update

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixture struct {
	srv     *httptest.Server
	priv    ed25519.PrivateKey
	pubB64  string
	files   map[string][]byte
	rels    []Release
	cmds    []string
	updater *Updater
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKIXPublicKey(pub)
	f := &fixture{priv: priv, pubB64: base64.StdEncoding.EncodeToString(der), files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/releases") {
			_ = json.NewEncoder(w).Encode(f.rels)
			return
		}
		b, ok := f.files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	}))
	t.Cleanup(f.srv.Close)
	dir := t.TempDir()
	bin := filepath.Join(dir, "pve-runners")
	_ = os.WriteFile(bin, []byte("old-binary"), 0o755)
	f.updater = &Updater{Repo: "example/repo", APIBase: f.srv.URL, HTTP: f.srv.Client(), Channel: "edge",
		Version: "build-1", GOARCH: "amd64", PublicKey: f.pubB64, BinPath: bin, StateDir: filepath.Join(dir, "state"),
		Now: func() time.Time { return time.Unix(1700000000, 0) },
		Exec: func(_ context.Context, name string, args ...string) error {
			f.cmds = append(f.cmds, name+" "+strings.Join(args, " "))
			return nil
		}}
	return f
}

// publish adds a release with a correctly signed binary (or a tampered one).
func (f *fixture) publish(tag string, at int64, bin string, tamper bool) {
	sum := sha256.Sum256([]byte(bin))
	sums := fmt.Sprintf("%s  pve-runners-linux-amd64\n", hex.EncodeToString(sum[:]))
	sig := ed25519.Sign(f.priv, []byte(sums))
	served := bin
	if tamper {
		served = bin + "evil"
	}
	base := "/dl/" + tag + "/"
	f.files[base+"pve-runners-linux-amd64"] = []byte(served)
	f.files[base+SumsAsset] = []byte(sums)
	f.files[base+SigAsset] = []byte(base64.StdEncoding.EncodeToString(sig))
	var assets []Asset
	for _, n := range []string{"pve-runners-linux-amd64", SumsAsset, SigAsset} {
		assets = append(assets, Asset{Name: n, URL: f.srv.URL + base + n})
	}
	f.rels = append(f.rels, Release{Tag: tag, Published: time.Unix(at, 0), Assets: assets})
}

func TestLatestPicksNewestOnChannel(t *testing.T) {
	f := newFixture(t)
	f.publish("build-2", 100, "b2", false)
	f.publish("v1.0.0", 50, "v1", false)
	f.publish("template-ubuntu-latest-20261010-1", 300, "img", false)
	ctx := context.Background()
	r, err := f.updater.Latest(ctx)
	if err != nil || r == nil || r.Tag != "build-2" {
		t.Fatalf("edge: %+v %v", r, err)
	}
	f.updater.Channel = "stable"
	if r, _ := f.updater.Latest(ctx); r == nil || r.Tag != "v1.0.0" {
		t.Fatalf("stable: %+v", r)
	}
	f.updater.Version = "v1.0.0"
	if r, _ := f.updater.Latest(ctx); r != nil {
		t.Fatalf("already current, got %+v", r)
	}
	f.updater.Version = "dev"
	if r, _ := f.updater.Latest(ctx); r != nil {
		t.Fatal("dev builds must not update")
	}
}

func TestDownloadRejectsTamperingAndWrongKey(t *testing.T) {
	f := newFixture(t)
	f.publish("build-2", 100, "good", true)
	ctx := context.Background()
	r, _ := f.updater.Latest(ctx)
	if _, err := f.updater.Download(ctx, r); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("tampered binary: %v", err)
	}
	other := newFixture(t)
	f.updater.PublicKey = other.pubB64
	if _, err := f.updater.Download(ctx, r); err == nil || !strings.Contains(err.Error(), "signature") {
		t.Fatalf("wrong key: %v", err)
	}
	f.updater.PublicKey = ""
	if _, err := f.updater.Download(ctx, r); err == nil {
		t.Fatal("no key must refuse")
	}
}

func TestInstallConfirmAndRollback(t *testing.T) {
	f := newFixture(t)
	f.publish("build-2", 100, "new-binary", false)
	ctx := context.Background()
	u := f.updater
	r, _ := u.Latest(ctx)
	bin, err := u.Download(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if err := u.Install(ctx, r.Tag, bin); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(u.BinPath); string(b) != "new-binary" {
		t.Fatal("binary not swapped")
	}
	if b, _ := os.ReadFile(u.BinPath + ".prev"); string(b) != "old-binary" {
		t.Fatal("previous binary not kept")
	}
	want := []string{
		"systemd-run --unit=pve-runners-update-verify --collect --on-active=90 " + u.BinPath + ".prev update verify",
		"systemctl --no-block restart pve-runners.service",
	}
	if strings.Join(f.cmds, "|") != strings.Join(want, "|") {
		t.Fatalf("commands %q", f.cmds)
	}

	// Healthy new binary confirms: verifier then does nothing.
	if err := u.Confirm("build-2"); err != nil {
		t.Fatal(err)
	}
	if tag, err := u.Verify(ctx); err != nil || tag != "" {
		t.Fatalf("no rollback expected: %q %v", tag, err)
	}

	// Unhealthy: no confirm, verifier restores .prev and skips the tag.
	if err := u.Install(ctx, r.Tag, bin); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(u.BinPath+".prev", []byte("old-binary"), 0o755)
	tag, err := u.Verify(ctx)
	if err != nil || tag != "build-2" {
		t.Fatalf("rollback: %q %v", tag, err)
	}
	if b, _ := os.ReadFile(u.BinPath); string(b) != "old-binary" {
		t.Fatal("not rolled back")
	}
	u.Version = "build-1"
	if r, _ := u.Latest(ctx); r != nil {
		t.Fatalf("a rolled-back build must be skipped, got %+v", r)
	}
}
