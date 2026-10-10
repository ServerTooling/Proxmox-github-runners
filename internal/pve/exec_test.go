package pve

import (
	"context"
	"testing"
	"time"
)

// A cancelled command returns even when a process it started still holds its output, as pct exec's
// command inside the guest does; otherwise stopping the service mid-spawn hangs until systemd kills it.
func TestCancelledCommandDoesNotWaitForItsChildren(t *testing.T) {
	old := waitDelay
	waitDelay = 100 * time.Millisecond
	defer func() { waitDelay = old }()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := (ExecRunner{}).Run(ctx, "sh", "-c", "sleep 30 & wait"); err == nil {
		t.Fatal("a cancelled command reported success")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("Run took %s after cancel", d)
	}
}
