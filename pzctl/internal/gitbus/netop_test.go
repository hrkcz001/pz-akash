package gitbus

import (
	"context"
	"errors"
	"testing"
	"time"
)

// The watchdog around every remote operation. These tests drive netOp directly
// with a stand-in for go-git rather than going through Fetch, because the two
// properties worth pinning are about an operation that ignores its context — and
// a real fetch against a local remote cannot be made to do that on demand.

func testRepo(timeout time.Duration) *Repo {
	return &Repo{opts: Options{NetTimeout: timeout, Logf: func(string, ...any) {}}}
}

// The gate must be free the moment netOp returns. It was not: the release sat in
// a defer that ran after the send that wakes the caller, so a caller returning
// from a finished fetch and immediately pushing — which both sides do on every
// reconcile — could be told the repository was busy. That cost a reconcile for no
// reason, and on the agent it dropped a phase change the controller was waiting
// for.
func TestNetOpReleasesTheGateBeforeItReturns(t *testing.T) {
	t.Parallel()
	r := testRepo(time.Minute)
	for i := 0; i < 100; i++ {
		if err := r.netOp(t.Context(), "op", func(context.Context) error { return nil }); err != nil {
			t.Fatalf("op %d: %v", i, err)
		}
		if !r.gate.TryLock() {
			t.Fatalf("op %d returned while the gate was still held; the next caller would get ErrBusy for an operation that has finished", i)
		}
		r.gate.Unlock()
	}
}

// An operation that overruns is abandoned rather than waited for — the agent's
// fetching goroutine is the one that has to stop the game and save the world when
// the lease closes. Until it finishes it keeps the gate, because go-git is not
// safe for concurrent use of one repository.
func TestNetOpAbandonsAnOverrunAndStaysBusyUntilItFinishes(t *testing.T) {
	t.Parallel()
	r := testRepo(50 * time.Millisecond)

	release := make(chan struct{})
	// Deliberately ignores the context it is handed: that is the whole failure mode
	// being defended against, a transport blocked in a read that cancellation
	// cannot reach.
	err := r.netOp(t.Context(), "wedged", func(context.Context) error {
		<-release
		return nil
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("the overrunning operation returned %v, want ErrTimeout", err)
	}

	if err := r.netOp(t.Context(), "next", func(context.Context) error {
		t.Error("a second operation ran while the abandoned one was still going")
		return nil
	}); !errors.Is(err, ErrBusy) {
		t.Fatalf("the next operation returned %v, want ErrBusy", err)
	}

	close(release)

	deadline := time.Now().Add(5 * time.Second)
	for {
		err := r.netOp(t.Context(), "after", func(context.Context) error { return nil })
		if err == nil {
			return
		}
		if !errors.Is(err, ErrBusy) {
			t.Fatalf("after the abandoned operation finished: %v, want nil or ErrBusy", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("the gate was never released after the abandoned operation returned")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A caller shutting down must get its goroutine back, and must be able to tell
// that from a timeout: one means "we are stopping", the other means "the remote
// is slow and the next attempt will be busy".
func TestNetOpReturnsTheCallersOwnCancellation(t *testing.T) {
	t.Parallel()
	r := testRepo(time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	defer close(release)

	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	err := r.netOp(ctx, "wedged", func(context.Context) error {
		<-release
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("netOp returned %v, want context.Canceled", err)
	}
	if errors.Is(err, ErrTimeout) {
		t.Error("a cancelled operation must not be reported as a timeout")
	}
}
