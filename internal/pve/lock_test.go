package pve

import (
	"context"
	"errors"
	"testing"
	"time"
)

// failing fails its first n calls with err, then succeeds.
type failing struct {
	n     int
	err   error
	calls int
}

func (f *failing) Run(context.Context, string, ...string) ([]byte, error) {
	f.calls++
	if f.calls <= f.n {
		return nil, f.err
	}
	return nil, nil
}

var lockErr = errors.New("exit status 255: can't lock file '/var/lock/pve-manager/pve-storage-local-zfs' - got timeout")

func TestStorageLockTimeoutIsRetried(t *testing.T) {
	old := lockRetryDelay
	lockRetryDelay = 0
	t.Cleanup(func() { lockRetryDelay = old })

	f := &failing{n: 2, err: lockErr}
	if err := (&Host{R: f}).Exec(context.Background(), LXCDestroy(9006)); err != nil || f.calls != 3 {
		t.Fatalf("err %v after %d calls, want success on the 3rd", err, f.calls)
	}

	f = &failing{n: 100, err: lockErr}
	if err := (&Host{R: f}).Exec(context.Background(), LXCClone(8901, 9006, "x", "", "", false)); err == nil ||
		f.calls != lockRetries+1 {
		t.Fatalf("err %v after %d calls, want the lock error after %d", err, f.calls, lockRetries+1)
	}

	// Any other failure is returned at once.
	f = &failing{n: 1, err: errors.New("exit status 2: unable to parse")}
	if err := (&Host{R: f}).Exec(context.Background(), LXCDestroy(9006)); err == nil || f.calls != 1 {
		t.Fatalf("err %v after %d calls, want no retry", err, f.calls)
	}
}

func TestStorageLockRetryStopsWhenCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := &failing{n: 100, err: lockErr}
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	if err := (&Host{R: f}).Exec(ctx, LXCDestroy(9006)); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
}
