// Package logship sends journald logs to Grafana Loki's push API: this service's own log, and the
// gh-runner unit's log inside each running runner guest. Runner guests are destroyed right after their
// job, taking their journal with them, so shipping is the only way to read a job's runner log later
// without being at the host.
package logship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ServerTooling/proxmox-github-runners/internal/pve"
)

// Batch caps the lines read per source per round.
const Batch = 1000

// FirstWindow limits the first round after a start to recent lines: Loki rejects lines far older than
// what a stream already holds, and earlier lines were shipped by the previous run.
const FirstWindow = 30 * time.Minute

type Host interface {
	Runners(ctx context.Context) ([]pve.Guest, error)
	Output(ctx context.Context, c pve.Cmd) ([]byte, error)
}

type Shipper struct {
	Host   Host
	URL    string // Loki base URL
	User   string // basic-auth user; empty sends Token as a bearer token
	Token  string
	HostID string
	HTTP   *http.Client
	Now    func() time.Time
	// Usage also sends each running runner's resource use (see usage.go); Jobs names the job a runner
	// is running, when known.
	Usage bool
	Jobs  func(runner string) (Job, bool)

	mu      sync.Mutex
	cursors map[string]string // source ("service" or a VMID) -> journal cursor of the last line read
	prev    map[int]counters  // usage: each guest's previous I/O counters
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// PushError is a rejected push. Client errors (bad lines, auth) are not retried with the same lines:
// resending them would fail the same way and stall shipping for good.
type PushError struct {
	Status int
	Body   string
}

func (e *PushError) Error() string { return fmt.Sprintf("loki push: %d %s", e.Status, e.Body) }

func (e *PushError) retryable() bool {
	return e.Status == http.StatusTooManyRequests || e.Status >= 500
}

// Once reads what each source logged since the last round and pushes it in one request. Read cursors
// advance only once Loki has the lines, or rejected them for good.
func (s *Shipper) Once(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cursors == nil {
		s.cursors = map[string]string{}
	}
	var errs []error
	var streams []stream
	next := map[string]string{}
	read := func(key string, cmd pve.Cmd, labels map[string]string) error {
		cur, seen := s.cursors[key]
		var cutoff time.Time
		if !seen && key == "service" {
			// Only the service's first read after a start is limited; a guest's whole journal is new.
			cutoff = s.Now().Add(-FirstWindow)
		}
		out, err := s.Host.Output(ctx, cmd)
		vals, last := parse(out, cutoff)
		if last == "" {
			last = cur
		}
		if last != "" {
			next[key] = last
		}
		if len(vals) > 0 {
			streams = append(streams, stream{labels, vals})
		}
		return err
	}
	if err := read("service", pve.ServiceJournalCmd(s.cursors["service"], Batch),
		map[string]string{"service": "pve-runners", "host": s.HostID, "unit": "pve-runners"}); err != nil {
		errs = append(errs, fmt.Errorf("service journal: %w", err))
	}
	runners, err := s.Host.Runners(ctx)
	if err != nil {
		errs = append(errs, err)
	}
	for _, g := range runners {
		if g.Status != "running" {
			continue
		}
		key := strconv.Itoa(g.VMID)
		// A guest that is still booting or already powering off has no journal to read; that is
		// normal, so it is skipped quietly and tried again next round.
		_ = read(key, pve.RunnerJournalCmd(g.VMID, s.cursors[key], Batch), map[string]string{"service": "pve-runners",
			"host": s.HostID, "unit": "gh-runner", "vmid": key, "runner": g.Name, "pool": g.TagValue("pool")})
	}
	if s.Usage && err == nil {
		streams = append(streams, s.usage(runners)...)
	}
	if len(streams) > 0 {
		if err := s.push(ctx, streams); err != nil {
			var pe *PushError
			if !errors.As(err, &pe) || pe.retryable() {
				return errors.Join(append(errs, err)...)
			}
			errs = append(errs, err)
		}
	}
	// Guests that are gone drop out of the map along with their cursors.
	s.cursors = next
	return errors.Join(errs...)
}

func (s *Shipper) push(ctx context.Context, streams []stream) error {
	body, err := json.Marshal(map[string]any{"streams": streams})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.URL, "/")+"/loki/api/v1/push",
		bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "pve-runners")
	if s.User != "" {
		req.SetBasicAuth(s.User, s.Token)
	} else {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return &PushError{resp.StatusCode, strings.TrimSpace(string(msg))}
	}
	return nil
}

// parse turns `journalctl -o json` output into Loki values (nanosecond timestamp, redacted line) and
// returns the cursor of the last entry read. Entries before cutoff are read but not sent.
func parse(out []byte, cutoff time.Time) ([][2]string, string) {
	var vals [][2]string
	var last string
	for _, ln := range bytes.Split(out, []byte("\n")) {
		var e struct {
			Cursor  string          `json:"__CURSOR"`
			Micros  string          `json:"__REALTIME_TIMESTAMP"`
			Message json.RawMessage `json:"MESSAGE"`
		}
		if len(bytes.TrimSpace(ln)) == 0 || json.Unmarshal(ln, &e) != nil || e.Cursor == "" {
			continue
		}
		last = e.Cursor
		us, err := strconv.ParseInt(e.Micros, 10, 64)
		if err != nil || (!cutoff.IsZero() && time.UnixMicro(us).Before(cutoff)) {
			continue
		}
		vals = append(vals, [2]string{strconv.FormatInt(us*1000, 10), Redact(message(e.Message))})
	}
	return vals, last
}

// message decodes journald's MESSAGE, which is a byte array instead of a string when it is not UTF-8.
func message(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var b []byte
	var ints []int
	if json.Unmarshal(raw, &ints) == nil {
		for _, i := range ints {
			b = append(b, byte(i))
		}
	}
	return string(b)
}

var (
	secretRes = []*regexp.Regexp{
		regexp.MustCompile(`gh[opsur]_[A-Za-z0-9]{20,}`),
		regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}`),
		// JIT configs, JWTs and other long encoded blobs.
		regexp.MustCompile(`[A-Za-z0-9+/=_.-]{100,}`),
	}
	// Only the value after these is masked, so the line still says what was there.
	secretValueRes = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(bearer\s+)[^\s"]+`),
		regexp.MustCompile(`(?i)((?:token|password|secret)"?\s*[:=]\s*"?)[^\s"]+`),
	}
)

// Redact masks anything that looks like a credential. The service never logs one on purpose; this
// guards against a library or a job printing one into a log that leaves the host.
func Redact(line string) string {
	for _, re := range secretRes {
		line = re.ReplaceAllString(line, "[redacted]")
	}
	for _, re := range secretValueRes {
		line = re.ReplaceAllString(line, "${1}[redacted]")
	}
	return line
}
