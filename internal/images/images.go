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
	"log/slog"
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
	// DockerImages are pulled into each new template (images.docker_images); Nameservers are the DNS
	// servers it uses for that (network.dns).
	DockerImages []string
	Nameservers  []string
	Retention    time.Duration
	Now          func() time.Time
}

// syncMu allows one image download or template creation at a time across every Manager: the image
// sync and each spawn build their own, and two of them must not both create the same template.
var syncMu sync.Mutex

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
// StorageTag records which storage a template's root disk is on. Storage IDs are already tag-safe.
func StorageTag(storage string) string { return "store-" + storage }

// onStorage keeps the templates whose root disk is on storage. Templates made before the tag
// existed count as elsewhere, so they are replaced once.
func onStorage(tpls []pve.Guest, storage string) []pve.Guest {
	var out []pve.Guest
	for _, g := range tpls {
		if g.TagValue("store") == storage {
			out = append(out, g)
		}
	}
	return out
}

// DockerTag records which docker_images a template was made with: a short hash of the sorted list,
// or "" for none (templates made before the setting existed have no tag either).
func DockerTag(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	sorted := slices.Clone(refs)
	slices.Sort(sorted)
	sum := sha256.Sum256([]byte(strings.Join(slices.Compact(sorted), "\n")))
	return "docker-" + hex.EncodeToString(sum[:4])
}

// withDocker keeps the templates made with this docker_images list.
func withDocker(tpls []pve.Guest, refs []string) []pve.Guest {
	want := strings.TrimPrefix(DockerTag(refs), "docker-")
	var out []pve.Guest
	for _, g := range tpls {
		if g.TagValue("docker") == want {
			out = append(out, g)
		}
	}
	return out
}

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
	if !syncMu.TryLock() {
		// Another call is downloading or creating a template, which can take minutes (docker_images
		// pulls). A spawn shouldn't wait for it while a finished template of this image is there.
		if tpls, err := m.Host.Templates(ctx, m.Range); err == nil {
			if g := newestTemplate(onStorage(tpls, m.Storage), ref.Name, ref.Release); g != nil {
				return g.VMID, nil
			}
		}
		syncMu.Lock()
	}
	defer syncMu.Unlock()
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
	// Runners are linked clones, which Proxmox keeps on the template's own storage, so after
	// proxmox.storage changes the image gets a fresh template there (from the cached archive). A
	// changed docker_images list also makes a new one; Prune then drops the old one as a duplicate.
	if g := newestTemplate(withDocker(onStorage(tpls, m.Storage), m.DockerImages), ref.Name, rel.Tag); g != nil {
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
		Tags: []string{pve.TemplateTag, "image-" + name, "rel-" + relTag(release), StorageTag(m.Storage),
			"created-" + strconv.FormatInt(m.now().Unix(), 10)},
		Description: "Runner template " + release + " managed by pve-runners",
	}
	if t := DockerTag(m.DockerImages); t != "" {
		spec.Tags = append(spec.Tags, t)
	}
	if err := m.Host.EnsurePool(ctx, m.Pool); err != nil {
		return 0, err
	}
	if err := m.Host.Exec(ctx, pve.LXCCreate(id, spec)); err != nil {
		return 0, err
	}
	if len(m.DockerImages) > 0 {
		// A failed pull still leaves a working template: jobs then pull what is missing themselves,
		// which beats stopping every runner while a registry is down. The next release tries again.
		if err := m.preloadDocker(ctx, id); err != nil {
			slog.Warn("docker images not preloaded into the template; jobs will pull them", "template", id, "err", err)
		}
	}
	if err := m.Host.Exec(ctx, pve.LXCTemplate(id)); err != nil {
		cctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		return 0, errors.Join(err, m.Host.DestroyTemplate(cctx, id, m.Range))
	}
	return id, nil
}

// preloadTimeout bounds pulling docker_images into a new template.
var preloadTimeout = 20 * time.Minute

// preloadScript waits for the guest's network and Docker, then pulls every image given as an argument.
// It tries them all and fails if any failed, so one bad name doesn't cost the others.
const preloadScript = `i=0
until ip -4 route show default | grep -q . && systemctl is-active -q docker; do
  i=$((i+1)); [ "$i" -ge 120 ] && { echo "no network or docker after 120 s" >&2; exit 1; }; sleep 1
done
rc=0
for ref in "$@"; do docker pull -q "$ref" || rc=1; done
exit $rc`

// sealScript readies the guest to be cloned again: Docker's files flushed, and no machine ID or journal
// that every clone would share (the runner image ships with an empty machine ID for the same reason).
const sealScript = `systemctl stop docker.socket docker containerd 2>/dev/null
rm -rf /var/log/journal/*
truncate -s 0 /etc/machine-id
sync`

// preloadDocker starts the new container, pulls DockerImages into it and stops it again, so every
// linked clone has them on disk for free. Only these fixed commands run in it, never a job.
func (m *Manager) preloadDocker(ctx context.Context, id int) error {
	if len(m.Nameservers) > 0 {
		if err := m.Host.Exec(ctx, pve.LXCNameservers(id, m.Nameservers)); err != nil {
			return err
		}
	}
	if err := m.Host.Exec(ctx, pve.LXCStart(id)); err != nil {
		return err
	}
	pctx, cancel := context.WithTimeout(ctx, preloadTimeout)
	pullErr := m.Host.Exec(pctx, pve.LXCExec(id, append([]string{"sh", "-c", preloadScript, "preload"}, m.DockerImages...)...))
	cancel()
	// Sealed and stopped whatever the pulls did: the template must not keep a running guest's state.
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer scancel()
	sealErr := m.Host.Exec(sctx, pve.LXCExec(id, "sh", "-c", sealScript))
	if err := m.Host.Exec(sctx, pve.LXCStop(id)); err != nil {
		return errors.Join(pullErr, sealErr, err)
	}
	return errors.Join(pullErr, sealErr)
}

// incompleteGrace is how long an unconverted template is left alone by Prune.
const incompleteGrace = 10 * time.Minute

// Removed is one thing Prune deleted.
type Removed struct {
	What   string // "template <id>" or the archive file name
	Reason string
}

// Prune deletes templates and cached archives older than the retention period, and templates left on
// a storage runners no longer use. It never deletes the
// newest template of an image, a template a pinned ref needs, or a template that runners were cloned
// from (inUse, from the runners' from-<id> tags); Proxmox would refuse that last one anyway.
func (m *Manager) Prune(ctx context.Context, refs []Ref, inUse []int) ([]Removed, error) {
	syncMu.Lock()
	defer syncMu.Unlock()
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
	here := onStorage(tpls, m.Storage)
	// Of several finished templates of one release on this storage, only the newest is needed.
	dup := map[int]bool{}
	for _, g := range here {
		if best := newestTemplate(here, g.TagValue("image"), TagPrefix+g.TagValue("rel")); g.Template == 1 && best.VMID != g.VMID {
			dup[g.VMID] = true
		}
	}
	for n := range names {
		g := newestTemplate(here, n, "")
		if g == nil {
			g = newestTemplate(tpls, n, "")
		}
		if g != nil {
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
		// A template whose creation was interrupted (never converted) is always cleaned up, once it is
		// past incompleteGrace: /cluster/resources lags a few seconds behind pct template, so a template
		// made just before this prune can still look unconverted (on 2026-11-17 one was destroyed that way).
		if g.Template != 1 && created(g) > m.now().Add(-incompleteGrace).Unix() {
			continue
		}
		moved := g.TagValue("store") != m.Storage
		if keep[g.VMID] || (g.Template == 1 && created(g) > cutoff && !moved && !dup[g.VMID]) {
			keptRels[g.TagValue("rel")] = true
			continue
		}
		if err := m.Host.DestroyTemplate(ctx, g.VMID, m.Range); err != nil {
			errs = append(errs, fmt.Errorf("template %d: %w", g.VMID, err))
			keptRels[g.TagValue("rel")] = true
			continue
		}
		reason := "older than images.retention_days"
		switch {
		case g.Template != 1:
			reason = "incomplete template"
		case moved:
			reason = "not on proxmox.storage " + m.Storage
		case dup[g.VMID]:
			reason = "a newer copy of the same release exists"
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
