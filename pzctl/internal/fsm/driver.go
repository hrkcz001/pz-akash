package fsm

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Akash is everything the state machine needs from the deployment provider.
//
// It is an interface for one reason above the usual ones: the expensive mistakes
// in this system are all in the interaction between the lifecycle and the
// provider, and a stub lets the whole lifecycle be walked — start, backup, halt,
// restart mid-deploy, close — without spending money or waiting on bids. Step 5
// swaps in the real client behind this same interface.
type Akash interface {
	// Deploy creates a deployment, waits for bids, takes a lease and waits for
	// the endpoint to become routable.
	//
	// The result may carry a lease even when the error is non-nil, and callers
	// must record it before they act on the error. A deploy that creates a lease
	// and then fails while waiting for the endpoint is the expensive failure: the
	// escrow is already funded and the provider is already billing, so a lease we
	// forget is a lease that bills until the deposit is exhausted.
	Deploy(ctx context.Context, req DeployRequest) (DeployResult, error)

	// Close closes the deployment. It must be idempotent: the controller calls it
	// again after a restart, because "closing" is a status it can wake up in.
	Close(ctx context.Context, l state.Lease) error

	// Alive reports whether the lease still exists provider-side. Used to
	// reconcile a document against reality at startup, which is how a lease that
	// outlived its controller gets noticed.
	Alive(ctx context.Context, l state.Lease) (bool, error)

	// Adopt lists deployments that look like ours. It is the last line of defence
	// for invariant I1: if the state document is unreadable, this is the only way
	// to discover a lease that is still billing.
	Adopt(ctx context.Context) ([]state.Lease, error)
}

// DeployRequest is what the FSM knows at deploy time. The driver renders the SDL
// from config; only the values that vary per deploy appear here.
type DeployRequest struct {
	// ControllerURL is baked into the server's environment so the agent can
	// reach storage before it has read anything from git.
	ControllerURL string
	// RestoreTarget is passed for logging and for the SDL's environment; the
	// agent reads the authoritative value from the controller's state branch.
	RestoreTarget string
	// Attempt counts from 1, for logs and for provider skip-listing.
	Attempt int
}

// DeployResult is a created lease and where it can be reached.
type DeployResult struct {
	Lease    state.Lease
	Endpoint state.Endpoint
	Price    state.Price
}

// --- dry run ---

// DryRun is an Akash driver that creates nothing and bills nothing. It exists so
// `pzctl controller --dry-run` can walk a complete lifecycle against a real git
// repository and real trigger files, which is the only way to test the parts that
// have historically been wrong: the ordering of a halt, the handling of a
// duplicate trigger, and what happens to a lease when a deploy is cancelled.
//
// The numbers it reports are plausible placeholders drawn from config, not a
// simulation of the market. The addresses come from 203.0.113.0/24, which RFC
// 5737 reserves for documentation, so a dry-run endpoint that escapes into a
// dashboard or a DNS record cannot be mistaken for a real server.
type DryRun struct {
	Cfg  *config.Config
	Now  func() time.Time
	Logf func(format string, args ...any)
	// Delay is slept inside Deploy and Close, so a test can exercise
	// cancellation and the "duplicate trigger while busy" path.
	Delay time.Duration
	// FailDeploy makes Deploy fail after creating the lease, which is the shape
	// of failure that leaks money.
	FailDeploy bool

	// StateFile makes the simulated provider durable across processes.
	//
	// A real provider outlives the controller — that is the entire reason
	// reconcileLease exists — so an in-memory registry makes every fresh process
	// look like a provider that has forgotten every lease, and the controller
	// correctly but uselessly declares each one vanished. That makes
	// `--dry-run --once` unable to walk a lifecycle across passes, which is the
	// shape an operator actually drives it in.
	//
	// Empty keeps the registry in memory, which is what the tests want: a fake
	// that writes to disk between subtests is a fake that leaks between them.
	StateFile string

	mu     sync.Mutex
	seq    int
	live   map[string]state.Lease
	loaded bool
}

// dryState is the durable form of the simulated provider.
type dryState struct {
	Seq  int                    `json:"seq"`
	Live map[string]state.Lease `json:"live"`
}

// ensureLoaded reads the registry once, and must be called with mu held.
//
// A missing file means a provider that has never been asked for anything, which
// is an empty registry rather than an error. An unreadable one is logged and
// treated the same way: refusing to start would leave the operator with no way to
// run the stub at all, and the file is ours alone.
func (d *DryRun) ensureLoaded() {
	if d.loaded {
		return
	}
	d.loaded = true
	if d.live == nil {
		d.live = map[string]state.Lease{}
	}
	if d.StateFile == "" {
		return
	}
	raw, err := os.ReadFile(d.StateFile)
	if err != nil {
		return
	}
	var on dryState
	if err := json.Unmarshal(raw, &on); err != nil {
		d.logf("dry-run: %s is unreadable (%v); starting with no leases", d.StateFile, err)
		return
	}
	d.seq = on.Seq
	for k, v := range on.Live {
		d.live[k] = v
	}
	if len(d.live) > 0 {
		d.logf("dry-run: %d lease(s) carried over from %s", len(d.live), d.StateFile)
	}
}

// save persists the registry, and must be called with mu held. A write failure is
// logged rather than returned: the caller is a provider call whose real-world
// counterpart has already succeeded, so failing it here would misreport what
// happened.
func (d *DryRun) save() {
	if d.StateFile == "" {
		return
	}
	raw, err := state.Marshal(dryState{Seq: d.seq, Live: d.live})
	if err == nil {
		err = state.WriteFile(d.StateFile, raw)
	}
	if err != nil {
		d.logf("dry-run: could not persist %s: %v", d.StateFile, err)
	}
}

func (d *DryRun) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DryRun) logf(f string, a ...any) {
	if d.Logf != nil {
		d.Logf(f, a...)
	}
}

func (d *DryRun) Deploy(ctx context.Context, req DeployRequest) (DeployResult, error) {
	d.mu.Lock()
	d.ensureLoaded()
	d.seq++
	n := d.seq
	d.mu.Unlock()

	now := d.now()
	lease := state.Lease{
		// A real dseq is a block height. Seconds since the epoch is the same
		// shape — monotonic and numeric — which keeps anything that sorts or
		// formats a dseq honest.
		DSeq:      strconv.FormatInt(now.Unix()+int64(n), 10),
		GSeq:      1,
		OSeq:      1,
		Provider:  fmt.Sprintf("akash1dryrun%02d", n),
		CreatedAt: state.At(now),
	}
	res := DeployResult{
		Lease: lease,
		Endpoint: state.Endpoint{
			IP:       fmt.Sprintf("203.0.113.%d", 1+n%254),
			GamePort: d.Cfg.Server.Ports.Game,
			UDPPort:  d.Cfg.Server.Ports.UDP,
		},
		Price: state.Price{
			UAKTPerBlock: d.Cfg.Server.PricingUAKT,
			AKTUSD:       d.Cfg.Akash.Price.AKTUSDFallback,
			USDPerDay:    d.Cfg.Akash.Price.MaxUSDPerDay / 2,
			USDPerHour:   d.Cfg.Akash.Price.MaxUSDPerDay / 48,
			QuotedAt:     state.At(now),
		},
	}
	if d.Cfg.Server.RCON.Enabled {
		res.Endpoint.RCONPort = d.Cfg.Server.RCON.Port
	}

	// The lease is registered before the delay, so a cancelled deploy leaves the
	// same debris a real one would: something the controller has to go and close.
	d.mu.Lock()
	d.ensureLoaded()
	d.live[lease.DSeq] = lease
	d.save()
	d.mu.Unlock()

	d.logf("dry-run: created dseq %s on %s (attempt %d, restore %q)",
		lease.DSeq, lease.Provider, req.Attempt, req.RestoreTarget)

	if err := d.sleep(ctx); err != nil {
		return res, err
	}
	if d.FailDeploy {
		return res, fmt.Errorf("dry-run: deploy failed after creating dseq %s", lease.DSeq)
	}
	return res, nil
}

func (d *DryRun) Close(ctx context.Context, l state.Lease) error {
	if err := d.sleep(ctx); err != nil {
		return err
	}
	d.mu.Lock()
	d.ensureLoaded()
	_, existed := d.live[l.DSeq]
	delete(d.live, l.DSeq)
	d.save()
	d.mu.Unlock()
	if !existed {
		// Idempotent on purpose: closing an already-closed lease is the normal
		// outcome of a controller restart, not an error to escalate.
		d.logf("dry-run: dseq %s was already closed", l.DSeq)
		return nil
	}
	d.logf("dry-run: closed dseq %s", l.DSeq)
	return nil
}

func (d *DryRun) Alive(_ context.Context, l state.Lease) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureLoaded()
	_, ok := d.live[l.DSeq]
	return ok, nil
}

func (d *DryRun) Adopt(context.Context) ([]state.Lease, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureLoaded()
	out := make([]state.Lease, 0, len(d.live))
	for _, l := range d.live {
		out = append(out, l)
	}
	return out, nil
}

// sleep waits Delay, or returns early if the context is cancelled — which is what
// makes a cancelled deploy testable.
func (d *DryRun) sleep(ctx context.Context) error {
	if d.Delay <= 0 {
		// Still check: a context cancelled before we started must not look like
		// a success.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	t := time.NewTimer(d.Delay)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
