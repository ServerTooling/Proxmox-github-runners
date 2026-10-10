package runnerpkg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func serve(t *testing.T, body string) (*httptest.Server, *atomic.Int32) {
	var hits atomic.Int32
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/v2.0.0/actions-runner-linux-x64-2.0.0.tar.gz" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s, &hits
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func TestFetchVerifiesAndCaches(t *testing.T) {
	s, hits := serve(t, "tarball")
	f := &Fetcher{BaseURL: s.URL, HTTP: s.Client(), CacheDir: t.TempDir(), GOARCH: "amd64"}
	p, err := f.Fetch(context.Background(), "2.0.0", strings.ToUpper(sum("tarball")))
	if err != nil || !strings.HasSuffix(p, "actions-runner-linux-x64-2.0.0.tar.gz") {
		t.Fatalf("got %q %v", p, err)
	}
	if _, err := f.Fetch(context.Background(), "2.0.0", sum("tarball")); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected one download, got %d", hits.Load())
	}
}

func TestFetchRejectsChecksumMismatch(t *testing.T) {
	s, _ := serve(t, "tampered")
	f := &Fetcher{BaseURL: s.URL, HTTP: s.Client(), CacheDir: t.TempDir(), GOARCH: "amd64"}
	if _, err := f.Fetch(context.Background(), "2.0.0", sum("tarball")); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v", err)
	}
}

func TestFetchNeedsPin(t *testing.T) {
	f := &Fetcher{CacheDir: t.TempDir(), GOARCH: "amd64"}
	if _, err := f.Fetch(context.Background(), "", ""); err == nil {
		t.Fatal("expected error without a pin")
	}
	if _, err := Arch("386"); err == nil {
		t.Fatal("expected unsupported arch")
	}
}
