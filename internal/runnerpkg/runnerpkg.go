// Package runnerpkg downloads the official actions/runner release tarball for a pinned version and
// verifies its SHA-256 before anything uses it. The runner is a .NET program that runs inside each
// guest, so it is fetched as a release artifact rather than linked into this binary.
package runnerpkg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const DefaultBaseURL = "https://github.com/actions/runner/releases/download"

// Arch maps a Go arch to the runner's tarball naming.
func Arch(goarch string) (string, error) {
	switch goarch {
	case "amd64":
		return "x64", nil
	case "arm64":
		return "arm64", nil
	}
	return "", fmt.Errorf("actions/runner has no linux build for %s", goarch)
}

func TarballName(version, arch string) string {
	return fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", arch, version)
}

type Fetcher struct {
	BaseURL  string
	HTTP     *http.Client
	CacheDir string
	GOARCH   string
}

func NewFetcher(cacheDir string) *Fetcher {
	return &Fetcher{BaseURL: DefaultBaseURL, HTTP: http.DefaultClient, CacheDir: cacheDir, GOARCH: runtime.GOARCH}
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Fetch returns the path of a verified tarball, downloading it only when the cache lacks a copy
// with the expected hash.
func (f *Fetcher) Fetch(ctx context.Context, version, wantSHA string) (string, error) {
	if version == "" || wantSHA == "" {
		return "", errors.New("no actions/runner version pinned; set github.runner_version and github.runner_sha256, or use a release build")
	}
	wantSHA = strings.ToLower(wantSHA)
	arch, err := Arch(f.GOARCH)
	if err != nil {
		return "", err
	}
	name := TarballName(version, arch)
	dst := filepath.Join(f.CacheDir, name)
	if got, err := fileSHA256(dst); err == nil && got == wantSHA {
		return dst, nil
	}
	if err := os.MkdirAll(f.CacheDir, 0o755); err != nil {
		return "", err
	}
	url := fmt.Sprintf("%s/v%s/%s", strings.TrimRight(f.BaseURL, "/"), version, name)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := f.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	tmp, err := os.CreateTemp(f.CacheDir, ".download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantSHA {
		return "", fmt.Errorf("checksum mismatch for %s: got %s, want %s", name, got, wantSHA)
	}
	return dst, os.Rename(tmp.Name(), dst)
}
