// Package update keeps the installed binary current with this repo's releases.
//
// Flow: find the newest release on the configured channel, download the binary for this arch plus
// SHA256SUMS and its ed25519 signature, verify both against the public key compiled into the running
// binary, swap the binary atomically (keeping the old one as .prev), and restart. A verifier started
// from the old binary checks that the new one reported healthy in time and rolls back if not.
package update

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	SumsAsset = "SHA256SUMS"
	SigAsset  = "SHA256SUMS.sig"
	// HealthTimeout is how long a freshly installed binary has to confirm it is healthy.
	HealthTimeout = 90 * time.Second
)

func BinaryAsset(goarch string) string { return "pve-runners-linux-" + goarch }

type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
}

type Release struct {
	Tag        string    `json:"tag_name"`
	Draft      bool      `json:"draft"`
	Prerelease bool      `json:"prerelease"`
	Published  time.Time `json:"published_at"`
	Assets     []Asset   `json:"assets"`
}

func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// OnChannel reports whether a release tag belongs to a channel. Edge takes every tool build
// (build-N and vX.Y.Z); stable only tagged versions. Template image releases are never tool updates.
func OnChannel(tag, channel string) bool {
	isBuild, isStable := strings.HasPrefix(tag, "build-"), strings.HasPrefix(tag, "v")
	if channel == "stable" {
		return isStable
	}
	return isBuild || isStable
}

// Exec runs a host command (systemctl, systemd-run); swapped out in tests.
type Exec func(ctx context.Context, name string, args ...string) error

type Updater struct {
	Repo      string // owner/name
	APIBase   string
	HTTP      *http.Client
	Channel   string
	Version   string
	GOARCH    string
	PublicKey string // base64 DER SPKI
	BinPath   string
	StateDir  string
	Exec      Exec
	Now       func() time.Time
}

// ParsePublicKey decodes the base64 DER (SPKI) ed25519 key produced by `openssl pkey -pubout -outform DER`.
func ParsePublicKey(b64 string) (ed25519.PublicKey, error) {
	if b64 == "" {
		return nil, errors.New("this build has no update signing key; self-update is disabled")
	}
	der, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return nil, fmt.Errorf("update key: %w", err)
	}
	k, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, fmt.Errorf("update key: %w", err)
	}
	ek, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("update key is not ed25519")
	}
	return ek, nil
}

func (u *Updater) get(ctx context.Context, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pve-runners")
	resp, err := u.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

func (u *Updater) skipFile() string    { return filepath.Join(u.StateDir, "update-skip") }
func (u *Updater) pendingFile() string { return filepath.Join(u.StateDir, "update-pending.json") }

func (u *Updater) skipped() []string {
	b, _ := os.ReadFile(u.skipFile())
	return strings.Fields(string(b))
}

// Latest returns the newest release on the channel that this host should move to, or nil if the
// running build is current. Dev builds never update.
func (u *Updater) Latest(ctx context.Context) (*Release, error) {
	if u.Version == "dev" || u.Version == "" {
		return nil, nil
	}
	body, err := u.get(ctx, strings.TrimRight(u.APIBase, "/")+"/repos/"+u.Repo+"/releases?per_page=30", 4<<20)
	if err != nil {
		return nil, err
	}
	var rels []Release
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}
	skip := u.skipped()
	var best *Release
	for i := range rels {
		r := &rels[i]
		if r.Draft || r.Prerelease || !OnChannel(r.Tag, u.Channel) || slices.Contains(skip, r.Tag) {
			continue
		}
		if _, ok := r.Asset(BinaryAsset(u.GOARCH)); !ok {
			continue
		}
		if best == nil || r.Published.After(best.Published) {
			best = r
		}
	}
	if best == nil || best.Tag == u.Version {
		return nil, nil
	}
	return best, nil
}

// Download fetches and verifies the release binary, returning its bytes.
func (u *Updater) Download(ctx context.Context, r *Release) ([]byte, error) {
	key, err := ParsePublicKey(u.PublicKey)
	if err != nil {
		return nil, err
	}
	name := BinaryAsset(u.GOARCH)
	assets := map[string]Asset{}
	for _, n := range []string{name, SumsAsset, SigAsset} {
		a, ok := r.Asset(n)
		if !ok {
			return nil, fmt.Errorf("release %s has no %s", r.Tag, n)
		}
		assets[n] = a
	}
	sums, err := u.get(ctx, assets[SumsAsset].URL, 1<<20)
	if err != nil {
		return nil, err
	}
	sig, err := u.get(ctx, assets[SigAsset].URL, 4<<10)
	if err != nil {
		return nil, err
	}
	if err := VerifySums(key, sums, sig); err != nil {
		return nil, fmt.Errorf("release %s: %w; refusing to install", r.Tag, err)
	}
	want := SumFor(sums, name)
	if want == "" {
		return nil, fmt.Errorf("release %s: %s not listed in SHA256SUMS", r.Tag, name)
	}
	bin, err := u.get(ctx, assets[name].URL, 200<<20)
	if err != nil {
		return nil, err
	}
	got := sha256.Sum256(bin)
	if hex.EncodeToString(got[:]) != want {
		return nil, fmt.Errorf("release %s: %s checksum mismatch", r.Tag, name)
	}
	return bin, nil
}

// VerifySums checks the base64 ed25519 signature over a SHA256SUMS file. Tool updates and template
// images share the key, so a host trusts exactly what this repo's CI signed.
func VerifySums(key ed25519.PublicKey, sums, sig []byte) error {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil || !ed25519.Verify(key, sums, b) {
		return errors.New("SHA256SUMS signature does not verify")
	}
	return nil
}

// SumFor returns the hex SHA-256 listed for name in a SHA256SUMS file, or "".
func SumFor(sums []byte, name string) string {
	for _, line := range strings.Split(string(sums), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && strings.TrimPrefix(f[1], "*") == name {
			return f[0]
		}
	}
	return ""
}

type pending struct {
	From     string    `json:"from"`
	To       string    `json:"to"`
	Deadline time.Time `json:"deadline"`
}

// Install swaps in a verified binary, keeps the current one as .prev, records the pending update,
// schedules the verifier (run from the old, known-good binary) and restarts the service.
func (u *Updater) Install(ctx context.Context, tag string, bin []byte) error {
	if err := os.MkdirAll(u.StateDir, 0o700); err != nil {
		return err
	}
	cur, err := os.ReadFile(u.BinPath)
	if err != nil {
		return err
	}
	prev := u.BinPath + ".prev"
	if err := writeAtomic(prev, cur, 0o755); err != nil {
		return err
	}
	p, _ := json.Marshal(pending{From: u.Version, To: tag, Deadline: u.Now().Add(HealthTimeout)})
	if err := writeAtomic(u.pendingFile(), p, 0o600); err != nil {
		return err
	}
	if err := writeAtomic(u.BinPath, bin, 0o755); err != nil {
		return err
	}
	if err := u.Exec(ctx, "systemd-run", "--unit=pve-runners-update-verify", "--collect",
		fmt.Sprintf("--on-active=%d", int(HealthTimeout.Seconds())), prev, "update", "verify"); err != nil {
		return fmt.Errorf("schedule update verifier: %w", err)
	}
	return u.Exec(ctx, "systemctl", "--no-block", "restart", "pve-runners.service")
}

// Pending reports whether an installed update is still waiting for its health confirmation; no new
// update starts until it is resolved.
func (u *Updater) Pending() bool {
	_, err := os.Stat(u.pendingFile())
	return err == nil
}

// Confirm is called by a freshly started binary once its self-check passes.
func (u *Updater) Confirm(version string) error {
	b, err := os.ReadFile(u.pendingFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var p pending
	if json.Unmarshal(b, &p) == nil && p.To != version {
		return nil // a different update is pending; leave it to the verifier
	}
	return os.Remove(u.pendingFile())
}

// Verify runs from the previous binary after the health window. If the new binary never confirmed,
// it is replaced by .prev, its tag is skipped from now on, and the service restarts on the old build.
func (u *Updater) Verify(ctx context.Context) (rolledBack string, err error) {
	b, err := os.ReadFile(u.pendingFile())
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var p pending
	if err := json.Unmarshal(b, &p); err != nil {
		return "", err
	}
	prev, err := os.ReadFile(u.BinPath + ".prev")
	if err != nil {
		return "", fmt.Errorf("cannot roll back, no previous binary: %w", err)
	}
	if err := writeAtomic(u.BinPath, prev, 0o755); err != nil {
		return "", err
	}
	f, err := os.OpenFile(u.skipFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_, _ = fmt.Fprintln(f, p.To)
	f.Close()
	_ = os.Remove(u.pendingFile())
	return p.To, u.Exec(ctx, "systemctl", "--no-block", "restart", "pve-runners.service")
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
