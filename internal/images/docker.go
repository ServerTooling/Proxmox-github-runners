package images

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"sync"

	"github.com/ServerTooling/proxmox-github-runners/internal/docker"
)

// DockerMachine is the slice of docker.Host that importing images needs.
type DockerMachine interface {
	HasImage(ctx context.Context, tag string) bool
	Import(ctx context.Context, tag string, tar io.Reader) error
	ImageTags(ctx context.Context, repo string) ([]string, error)
	RemoveImage(ctx context.Context, tag string) error
}

// dockerMu serializes imports: a second spawn waits for the import of the same release instead of
// copying it again.
var dockerMu sync.Mutex

func dockerRepo(name string) string { return docker.ImageRepo + name }

// DockerImageTag is the local image a release is imported as on the Docker machine.
func DockerImageTag(name, release string) string { return dockerRepo(name) + ":" + relTag(release) }

// Decompress streams a verified archive as a plain tar; it is a variable so tests need no zstd.
var Decompress = func(ctx context.Context, path string) (io.ReadCloser, func() error, error) {
	cmd := exec.CommandContext(ctx, "zstd", "-dc", "--", path)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, fmt.Errorf("zstd: %w", err)
	}
	return out, cmd.Wait, nil
}

// EnsureDocker returns the Docker machine's image for a ref, importing the release from the same
// signed archive LXC templates use when it isn't there yet. If GitHub can't be reached, an
// unpinned ref falls back to the newest imported release so runners keep working offline. After an
// import, older imports of the image are removed, except pinned ones and any a container still uses
// (Docker refuses those).
func (m *Manager) EnsureDocker(ctx context.Context, ref Ref, d DockerMachine, pinned []Ref) (string, error) {
	dockerMu.Lock()
	defer dockerMu.Unlock()
	rel, err := m.Resolve(ctx, ref)
	if err != nil {
		if tags, terr := d.ImageTags(ctx, dockerRepo(ref.Name)); terr == nil {
			for _, t := range tags { // newest first
				if ref.Release == "" || t == relTag(ref.Release) {
					return dockerRepo(ref.Name) + ":" + t, nil
				}
			}
		}
		return "", err
	}
	tag := DockerImageTag(ref.Name, rel.Tag)
	if d.HasImage(ctx, tag) {
		return tag, nil
	}
	archive, err := m.Fetch(ctx, rel, ref.Name)
	if err != nil {
		return "", err
	}
	tar, wait, err := Decompress(ctx, archive)
	if err != nil {
		return "", err
	}
	err = d.Import(ctx, tag, tar)
	tar.Close()
	if werr := wait(); err == nil && werr != nil {
		err = fmt.Errorf("zstd: %w", werr)
	}
	if err != nil {
		return "", fmt.Errorf("import %s on the docker machine: %w", tag, err)
	}
	keep := map[string]bool{relTag(rel.Tag): true}
	for _, p := range pinned {
		if p.Name == ref.Name && p.Release != "" {
			keep[relTag(p.Release)] = true
		}
	}
	// A removal that fails (an image a container still uses) only leaves disk space taken until the next import.
	if tags, err := d.ImageTags(ctx, dockerRepo(ref.Name)); err == nil {
		for _, t := range tags {
			if !keep[t] {
				_ = d.RemoveImage(ctx, dockerRepo(ref.Name)+":"+t)
			}
		}
	}
	return tag, nil
}
