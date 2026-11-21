// Package dockertest is an in-memory Docker machine for tests: it answers the commands package docker
// builds and records each one.
package dockertest

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
)

type Container struct {
	ID    string // pve-runners.id label
	Name  string
	State string // running | exited | created
	Tags  string
	Image string
}

type Fake struct {
	mu         sync.Mutex
	Containers []*Container
	Images     []string // newest first, like docker image ls
	Runtimes   []string
	Bridge     string // network's bridge; empty = no network
	Calls      []string
	Stdin      map[string]string // first argv words -> what came on stdin, keyed by Key
	// Fail makes a command fail when its line contains the key.
	Fail map[string]error
	// Output overrides the reply to a command whose line contains the key.
	Output map[string]string
}

func New() *Fake {
	return &Fake{Runtimes: []string{"io.containerd.runc.v2", "runc", "sysbox-runc"}, Stdin: map[string]string{},
		Fail: map[string]error{}, Output: map[string]string{}}
}

func (f *Fake) Run(_ context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	line := strings.Join(args, " ")
	f.Calls = append(f.Calls, line)
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		f.Stdin[line] = string(b)
	}
	for k, err := range f.Fail {
		if strings.Contains(line, k) {
			return nil, err
		}
	}
	for k, out := range f.Output {
		if strings.Contains(line, k) {
			return []byte(out), nil
		}
	}
	if len(args) < 2 || args[0] != "docker" {
		return nil, fmt.Errorf("unexpected command %q", line)
	}
	switch {
	case args[1] == "ps":
		var b strings.Builder
		for _, c := range f.Containers {
			fmt.Fprintf(&b, "%s\t%s\t%s\t%s\n", c.ID, c.Name, c.State, c.Tags)
		}
		return []byte(b.String()), nil
	case args[1] == "run" && slices.Contains(args, "--name"):
		c := &Container{State: "running", Image: args[len(args)-1]}
		for i := 2; i < len(args)-1; i++ {
			switch args[i] {
			case "--name":
				c.Name = args[i+1]
			case "--label":
				k, v, _ := strings.Cut(args[i+1], "=")
				if k == "pve-runners.id" {
					c.ID = v
				} else if k == "pve-runners.tags" {
					c.Tags = v
				}
			}
		}
		f.Containers = append(f.Containers, c)
		return []byte("abc123\n"), nil
	case args[1] == "run": // the isolation helper
		return []byte("isolated\n"), nil
	case args[1] == "rm":
		f.Containers = slices.DeleteFunc(f.Containers, func(c *Container) bool { return c.Name == args[len(args)-1] })
		return nil, nil
	case args[1] == "exec":
		if i := slices.Index(args, "is-active"); i > 0 {
			return []byte("active\n"), nil
		}
		return nil, nil
	case args[1] == "info":
		return []byte(strings.Join(f.Runtimes, " ") + " \n"), nil
	case args[1] == "image" && args[2] == "inspect":
		if slices.Contains(f.Images, args[len(args)-1]) {
			return []byte("sha256:1\n"), nil
		}
		return nil, fmt.Errorf("Error: No such image: %s", args[len(args)-1])
	case args[1] == "import":
		f.Images = append([]string{args[len(args)-1]}, f.Images...)
		return []byte("sha256:2\n"), nil
	case args[1] == "image" && args[2] == "ls":
		repo := args[len(args)-1]
		var b strings.Builder
		for _, img := range f.Images {
			if r, t, _ := strings.Cut(img, ":"); r == repo {
				b.WriteString(t + "\n")
			}
		}
		return []byte(b.String()), nil
	case args[1] == "image" && args[2] == "rm":
		f.Images = slices.DeleteFunc(f.Images, func(s string) bool { return s == args[len(args)-1] })
		return nil, nil
	case args[1] == "network" && args[2] == "inspect":
		if f.Bridge == "" {
			return nil, fmt.Errorf("Error response from daemon: network gh-runners not found")
		}
		return []byte(f.Bridge + "\n"), nil
	case args[1] == "network" && args[2] == "create":
		f.Bridge = "ghr0"
		return []byte("netid\n"), nil
	}
	return nil, fmt.Errorf("unexpected command %q", line)
}

// Mutations are the calls that change something, in order.
func (f *Fake) Mutations() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.Calls {
		if strings.HasPrefix(c, "docker ps") || strings.HasPrefix(c, "docker info") ||
			strings.HasPrefix(c, "docker image inspect") || strings.HasPrefix(c, "docker image ls") ||
			strings.HasPrefix(c, "docker network inspect") {
			continue
		}
		out = append(out, c)
	}
	return out
}
