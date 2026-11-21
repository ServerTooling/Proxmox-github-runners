package docker

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// SSH runs commands on the Docker machine with the system's ssh client: key auth only, the machine's
// host key pinned in its own known_hosts file, and one shared connection reused between commands.
type SSH struct {
	Target     string // user@host
	Port       int
	KeyFile    string
	KnownHosts string
	// ControlPath is the socket the shared connection lives on; empty opens one connection per command.
	ControlPath string
}

// Args is the ssh command line for one remote command. ssh hands the remote side a single string
// that its shell splits, so every argument is quoted.
func (s SSH) Args(args ...string) []string {
	out := []string{"-i", s.KeyFile, "-p", strconv.Itoa(s.Port),
		"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
		"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + s.KnownHosts,
		"-o", "ConnectTimeout=15", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=4"}
	if s.ControlPath != "" {
		out = append(out, "-o", "ControlMaster=auto", "-o", "ControlPath="+s.ControlPath, "-o", "ControlPersist=60s")
	}
	quoted := make([]string, len(args))
	for i, a := range args {
		quoted[i] = Quote(a)
	}
	return append(out, "-T", s.Target, "--", strings.Join(quoted, " "))
}

// Quote makes a string one word for a POSIX shell.
func Quote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./=:@,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// waitDelay bounds how long a cancelled ssh may keep its output open.
var waitDelay = 5 * time.Second

func (s SSH) Run(ctx context.Context, stdin io.Reader, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "ssh", s.Args(args...)...)
	cmd.WaitDelay = waitDelay
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s on %s: %w: %s", strings.Join(firstWords(args), " "), s.Target, err,
			strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// firstWords names a command in errors without its later arguments, which can be long scripts.
func firstWords(args []string) []string {
	if len(args) > 3 {
		return args[:3]
	}
	return args
}
