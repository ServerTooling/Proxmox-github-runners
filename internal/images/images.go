// Package images turns published runner images (template-<name>-<date>-<run> releases built from
// templates/ in this repo) into Proxmox template containers that pools clone from.
//
// An image is downloaded once, verified against the signed SHA256SUMS (same key as tool updates),
// cached in the vztmpl directory of a Proxmox storage, and created as a template container in its own
// ID range. Everything is recorded in the template's tags, so a restart picks up where it left off.
package images

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
	"github.com/ServerTooling/proxmox-github-runners/internal/update"
)

// TagPrefix starts every image release tag; tool updates never consider these releases.
const TagPrefix = "template-"

// maxImageBytes bounds a download; images are a few hundred MB compressed.
const maxImageBytes = 8 << 30

// Ref names an image: the newest release of Name, or exactly Release when pinned (name@release).
type Ref struct {
	Name    string
	Release string
}

func ParseRef(s string) Ref {
	name, rel, _ := strings.Cut(s, "@")
	return Ref{Name: name, Release: rel}
}

func (r Ref) String() string {
	if r.Release == "" {
		return r.Name
	}
	return r.Name + "@" + r.Release
}

// AssetName is the archive file in an image release for one architecture.
func AssetName(name, goarch string) string { return name + "-" + goarch + ".tar.zst" }

// relTag turns a release tag into a Proxmox tag value (tags allow only [a-z0-9_+.-]).
func relTag(release string) string { return strings.TrimPrefix(release, TagPrefix) }

type Manager struct {
	Repo      string // owner/name publishing the images
	APIBase   string
	HTTP      *http.Client
	PublicKey string // base64 DER ed25519, the same key that signs tool updates
	GOARCH    string

	Host         *pve.Host
	Range        [2]int // images.vmid_range
	CacheDir     string // the storage's vztmpl directory on disk
	CacheStorage string // storage ID for the archive volume, e.g. "local"
	Storage      string // where template root disks go (proxmox.storage)
	Pool         string // Proxmox resource pool
	Bridge       string
	RootfsGB     int
	Retention    time.Duration
	Now          func() time.Time

	mu sync.Mutex // one image download or template creation at a time
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) get(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pve-runners")
	resp, err := m.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("GET %s: HTTP %d", url, resp.StatusCode)
	}
	return resp, nil
}

func (m *Manager) getSmall(ctx context.Context, url string, limit int64) ([]byte, error) {
	resp, err := m.get(ctx, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// Resolve finds the release a ref points to: the pinned tag, or the newest published release of
// that image that has an archive for this host's architecture.
func (m *Manager) Resolve(ctx context.Context, ref Ref) (*update.Release, error) {
	api := strings.TrimRight(m.APIBase, "/") + "/repos/" + m.Repo + "/releases"
	asset := AssetName(ref.Name, m.GOARCH)
	if ref.Release != "" {
		body, err := m.getSmall(ctx, api+"/tags/"+ref.Release, 4<<20)
		if err != nil {
			return nil, fmt.Errorf("image %s: %w", ref, err)
		}
		var r update.Release
		if err := json.Unmarshal(body, &r); err != nil {
			return nil, fmt.Errorf("parse release %s: %w", ref.Release, err)
		}
		if _, ok := r.Asset(asset); !ok {
			return nil, fmt.Errorf("image release %s has no %s", ref.Release, asset)
		}
		return &r, nil
	}
	body, err := m.getSmall(ctx, api+"?per_page=100", 8<<20)
	if err != nil {
		return nil, err
	}
	var rels []update.Release
	if err := json.Unmarshal(body, &rels); err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}
	var best *update.Release
	for i := range rels {
		r := &rels[i]
		if r.Draft || r.Prerelease || !strings.HasPrefix(r.Tag, TagPrefix+ref.Name+"-") {
			continue
		}
		// "ubuntu-latest" must not match "ubuntu-latest-gpu-...": the rest is <date>-<run>.
		if rest := strings.TrimPrefix(r.Tag, TagPrefix+ref.Name+"-"); strings.Count(rest, "-") != 1 {
			continue
		}
		if _, ok := r.Asset(asset); !ok {
			continue
		}
		if best == nil || r.Published.After(best.Published) {
			best = r
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no published image %q for %s in %s", ref.Name, m.GOARCH, m.Repo)
	}
	return best, nil
}

// archiveFile is where a release's archive is cached; the release tag keeps files unique.
func (m *Manager) archiveFile(release string) string {
	return "pve-runners-" + release + "-" + m.GOARCH + ".tar.zst"
}

// Fetch downloads a release's archive into the cache, verifying the signature over SHA256SUMS and
// the archive's hash. A cached archive that still matches is reused.
func (m *Manager) Fetch(ctx context.Context, rel *update.Release, name string) (string, error) {
	key, err := update.ParsePublicKey(m.PublicKey)
	if err != nil {
		return "", err
	}
	asset := AssetName(name, m.GOARCH)
	urls := map[string]string{}
	for _, n := range []string{asset, update.SumsAsset, update.SigAsset} {
		a, ok := rel.Asset(n)
		if !ok {
			return "", fmt.Errorf("image release %s has no %s", rel.Tag, n)
		}
		urls[n] = a.URL
	}
	sums, err := m.getSmall(ctx, urls[update.SumsAsset], 1<<20)
	if err != nil {
		return "", err
	}
	sig, err := m.getSmall(ctx, urls[update.SigAsset], 4<<10)
	if err != nil {
		return "", err
	}
	if err := update.VerifySums(key, sums, sig); err != nil {
		return "", fmt.Errorf("image release %s: %w; refusing to use it", rel.Tag, err)
	}
	want := update.SumFor(sums, asset)
	if want == "" {
		return "", fmt.Errorf("image release %s: %s not listed in SHA256SUMS", rel.Tag, asset)
	}

	path := filepath.Join(m.CacheDir, m.archiveFile(rel.Tag))
	if got, err := fileSHA256(path); err == nil && got == want {
		return path, nil
	}
	if err := os.MkdirAll(m.CacheDir, 0o755); err != nil {
		return "", err
	}
	resp, err := m.get(ctx, urls[asset])
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	tmp, err := os.CreateTemp(m.CacheDir, ".pve-runners-download-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxImageBytes+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("download %s: %w", asset, err)
	}
	if n > maxImageBytes {
		return "", fmt.Errorf("image %s is larger than %d bytes", asset, int64(maxImageBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return "", fmt.Errorf("image release %s: %s checksum mismatch", rel.Tag, asset)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
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

func created(g pve.Guest) int64 {
	n, _ := strconv.ParseInt(g.TagValue("created"), 10, 64)
	return n
}

// newestTemplate returns the most recently created complete template for an image name (and
// release, when given).
func newestTemplate(tpls []pve.Guest, name, release string) *pve.Guest {
	var best *pve.Guest
	for i := range tpls {
		g := &tpls[i]
		if g.Template != 1 || g.TagValue("image") != name || (release != "" && g.TagValue("rel") != relTag(release)) {
			continue
		}
		if best == nil || created(*g) > created(*best) {
			best = g
		}
	}
	return best
}

// Ensure returns the template container for a ref, downloading the image and creating the template
// when needed. If GitHub can't be reached, an unpinned ref falls back to the newest local template
// so runners keep working offline.
func (m *Manager) Ensure(ctx context.Context, ref Ref) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tpls, err := m.Host.Templates(ctx, m.Range)
	if err != nil {
		return 0, err
	}
	rel, err := m.Resolve(ctx, ref)
	if err != nil {
		if g := newestTemplate(tpls, ref.Name, ref.Release); g != nil {
			return g.VMID, nil
		}
		return 0, err
	}
	if g := newestTemplate(tpls, ref.Name, rel.Tag); g != nil {
		return g.VMID, nil
	}
	archive, err := m.Fetch(ctx, rel, ref.Name)
	if err != nil {
		return 0, err
	}
	return m.create(ctx, ref.Name, rel.Tag, filepath.Base(archive))
}

func (m *Manager) create(ctx context.Context, name, release, file string) (id int, err error) {
	id, err = m.Host.NextFreeIDIn(ctx, m.Range)
	if err != nil {
		return 0, err
	}
	spec := pve.CreateSpec{
		Archive:  m.CacheStorage + ":vztmpl/" + file,
		Hostname: "gh-template-" + name,
		Pool:     m.Pool,
		Storage:  m.Storage,
		RootfsGB: m.RootfsGB,
		Bridge:   m.Bridge,
		Tags: []string{pve.TemplateTag, "image-" + name, "rel-" + relTag(release),
			"created-" + strconv.FormatInt(m.now().Unix(), 10)},
		Description: "Runner template " + release + " managed by pve-runners",
	}
	if err := m.Host.EnsurePool(ctx, m.Pool); err != nil {
		return 0, err
	}
	if err := m.Host.Exec(ctx, pve.LXCCreate(id, spec)); err != nil {
		return 0, err
	}
	if err := m.Host.Exec(ctx, pve.LXCTemplate(id)); err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return 0, errors.Join(err, m.Host.DestroyTemplate(cctx, id, m.Range))
	}
	return id, nil
}

// Removed is one thing Prune deleted.
type Removed struct {
	What   string // "template <id>" or the archive file name
	Reason string
}

// Prune deletes templates and cached archives older than the retention period. It never deletes the
// newest template of an image, a template a pinned ref needs, or a template that runners were cloned
// from (inUse, from the runners' from-<id> tags); Proxmox would refuse that last one anyway.
func (m *Manager) Prune(ctx context.Context, refs []Ref, inUse []int) ([]Removed, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	tpls, err := m.Host.Templates(ctx, m.Range)
	if err != nil {
		return nil, err
	}
	cutoff := m.now().Add(-m.Retention).Unix()
	keep := map[int]bool{}
	for _, id := range inUse {
		keep[id] = true
	}
	names := map[string]bool{}
	for _, g := range tpls {
		names[g.TagValue("image")] = true
	}
	for n := range names {
		if g := newestTemplate(tpls, n, ""); g != nil {
			keep[g.VMID] = true
		}
	}
	for _, r := range refs {
		if r.Release == "" {
			continue
		}
		for _, g := range tpls {
			if g.TagValue("image") == r.Name && g.TagValue("rel") == relTag(r.Release) {
				keep[g.VMID] = true
			}
		}
	}
	var removed []Removed
	var errs []error
	keptRels := map[string]bool{}
	sort.Slice(tpls, func(i, j int) bool { return tpls[i].VMID < tpls[j].VMID })
	for _, g := range tpls {
		// A template whose creation was interrupted (never converted) is always cleaned up.
		if keep[g.VMID] || (g.Template == 1 && created(g) > cutoff) {
			keptRels[g.TagValue("rel")] = true
			continue
		}
		if err := m.Host.DestroyTemplate(ctx, g.VMID, m.Range); err != nil {
			errs = append(errs, fmt.Errorf("template %d: %w", g.VMID, err))
			keptRels[g.TagValue("rel")] = true
			continue
		}
		reason := "older than images.retention_days"
		if g.Template != 1 {
			reason = "incomplete template"
		}
		removed = append(removed, Removed{fmt.Sprintf("template %d (%s)", g.VMID, g.TagValue("rel")), reason})
	}

	// Archives are only needed to re-create a template; drop the ones whose template is gone once
	// they are past retention too.
	files, _ := filepath.Glob(filepath.Join(m.CacheDir, "pve-runners-"+TagPrefix+"*.tar.zst"))
	for _, f := range files {
		base := filepath.Base(f)
		rel := strings.TrimSuffix(strings.TrimPrefix(base, "pve-runners-"), "-"+m.GOARCH+".tar.zst")
		if keptRels[relTag(rel)] || slices.ContainsFunc(refs, func(r Ref) bool { return r.Release == rel }) {
			continue
		}
		st, err := os.Stat(f)
		if err != nil || st.ModTime().Unix() > cutoff {
			continue
		}
		if err := os.Remove(f); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, Removed{base, "older than images.retention_days"})
	}
	// Downloads cut short by a crash or restart.
	parts, _ := filepath.Glob(filepath.Join(m.CacheDir, ".pve-runners-download-*"))
	for _, f := range parts {
		if st, err := os.Stat(f); err == nil && m.now().Sub(st.ModTime()) > 24*time.Hour {
			_ = os.Remove(f)
		}
	}
	return removed, errors.Join(errs...)
}

// FromTemplate is the runner tag key recording which template a runner was cloned from.
const FromTemplate = "from"

// InUse returns the template IDs that existing runners were cloned from.
func InUse(runners []pve.Guest) []int {
	var out []int
	for _, g := range runners {
		if id, err := strconv.Atoi(g.TagValue(FromTemplate)); err == nil {
			out = append(out, id)
		}
	}
	return out
}
