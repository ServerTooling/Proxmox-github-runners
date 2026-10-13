package setup

import (
	"bytes"
	"context"
	"io"
	"sync"
	"time"
)

// Action is a CLI command offered as a button in the app. It runs in the background, since some (image
// pulls, template prep) take minutes; the app polls for its output as it arrives.
type Action struct {
	Name, Label, Help string
	// Group is the card the button sits in, such as "Images".
	Group string
	// Args are inputs passed to Run by name, such as "pool" for spawn.
	Args []string
	// Confirm, when set, is asked in the app before running.
	Confirm string
	Run     func(ctx context.Context, w io.Writer, args map[string]string) error
}

// actionTimeout bounds one run; an image download is the slowest thing an action does.
const actionTimeout = time.Hour

type actionRun struct {
	mu       sync.Mutex
	out      bytes.Buffer
	running  bool
	err      string
	started  time.Time
	finished time.Time
}

func (r *actionRun) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.out.Write(p)
}

type runView struct {
	Running        bool
	Output, Err    string
	Started, Ended string
}

func (r *actionRun) view() runView {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := runView{Running: r.running, Output: r.out.String(), Err: r.err, Started: r.started.Format(time.TimeOnly)}
	if !r.finished.IsZero() {
		v.Ended = r.finished.Format(time.TimeOnly)
	}
	return v
}

func (s *Server) action(name string) *Action {
	for i := range s.Service.Actions {
		if s.Service.Actions[i].Name == name {
			return &s.Service.Actions[i]
		}
	}
	return nil
}

// startAction runs a in the background unless it is already running, and reports whether it started;
// a second click must not start a second image pull or update.
func (s *Server) startAction(a *Action, args map[string]string) bool {
	s.mu.Lock()
	run := s.runs[a.Name]
	if run != nil && run.view().Running {
		s.mu.Unlock()
		return false
	}
	run = &actionRun{running: true, started: time.Now()}
	s.runs[a.Name] = run
	s.mu.Unlock()
	base := s.Service.Ctx
	if base == nil {
		base = context.Background()
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, actionTimeout)
		defer cancel()
		err := a.Run(ctx, run, args)
		run.mu.Lock()
		defer run.mu.Unlock()
		run.running, run.finished = false, time.Now()
		if err != nil {
			run.err = err.Error()
		}
	}()
	return true
}

// status runs the status command for the overview, briefly enough not to hold the request up.
func (s *Server) status(ctx context.Context) (*StatusReport, string) {
	if s.Service.Status == nil {
		return nil, ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	r, err := s.Service.Status(ctx)
	if err != nil {
		return nil, err.Error()
	}
	return r, ""
}
