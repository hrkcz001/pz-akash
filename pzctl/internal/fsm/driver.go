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

	// Escrow reports what a deployment's deposit has left to spend, in USD.
	//
	// known is false when the escrow holds only denominations the provider client
	// cannot price. That is emphatically not the same as a remaining balance of
	// zero: a top-up decided from a wrong zero spends real money on a deployment
	// that needed nothing, and a horizon computed from one would be nonsense. The
	// funds loop treats unknown as "do not act".
	//
	// Three plain values rather than a struct on purpose. internal/akash has a
	// perfectly good Escrow type, and naming it here would be the import this
	// interface exists to avoid — see cmd/pzctl/driver.go.
	Escrow(ctx context.Context, dseq string) (remainingUSD float64, known bool, err error)

	// TopUp adds usd to a deployment's escrow and reports what was actually
	// deposited, which may be more than asked: the provider has a minimum deposit,
	// and rounding up to it is the safe direction.
	TopUp(ctx context.Context, dseq string, usd float64) (float64, error)

	// SelfURL is the provider's own address for this controller, or "" when it
	// cannot be established.
	//
	// The controller is not told where it is — the provider picks the host and port
	// after the SDL was submitted — so this looks itself up the way Adopt looks up a
	// lease. It exists because the DNS name goes through Cloudflare, whose free plan
	// refuses a request body over 100 MB, and a backup upload is one large request
	// body. Bulk traffic needs a route that is not the proxy.
	//
	// "" and a nil error is the ordinary answer off a lease, and callers must treat
	// it as "keep using the name you had" rather than as a failure.
	SelfURL(ctx context.Context) (string, error)
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

	mu       sync.Mutex
	seq      int
	live     map[string]state.Lease
	deposits map[string]float64
	loaded   bool
}

// dryState is the durable form of the simulated provider.
type dryState struct {
	Seq  int                    `json:"seq"`
	Live map[string]state.Lease `json:"live"`
	// Deposits is what has been paid into each escrow. What is left is that minus
	// the burn since the lease was created, computed on read — see Escrow.
	Deposits map[string]float64 `json:"deposits,omitempty"`
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
	if d.deposits == nil {
		d.deposits = map[string]float64{}
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
	for k, v := range on.Deposits {
		d.deposits[k] = v
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
	raw, err := state.Marshal(dryState{Seq: d.seq, Live: d.live, Deposits: d.deposits})
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
			AmountPerBlock: d.Cfg.Server.PricingAmount,
			Denom:          d.Cfg.Akash.Price.Denom,
			AKTUSD:         d.Cfg.Akash.Price.AKTUSDFallback,
			USDPerDay:      d.pricePerDay(),
			USDPerHour:     d.pricePerDay() / 24,
			QuotedAt:       state.At(now),
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
	// Funded the way a real deploy funds it: initial_deposit_days at the *ceiling*
	// price, not at the price the lease was won for. That gap is the whole reason the
	// funds loop exists, and a stub that seeded the full horizon would hide it.
	d.deposits[lease.DSeq] = float64(d.Cfg.Akash.InitialDepositDays) * d.Cfg.Akash.Price.MaxUSDPerDay
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
	delete(d.deposits, l.DSeq)
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

// SelfURL answers with a documentation address, so a dry run exercises the
// direct-route code path without ever pointing it at something real.
//
// http and a high port rather than the DNS name, because the whole point of the
// value is that it is not the proxied name — a stub that returned the public URL
// would let a bug that ignores the direct route pass every dry run.
func (d *DryRun) SelfURL(context.Context) (string, error) {
	return fmt.Sprintf("http://203.0.113.1:%d", d.Cfg.Controller.HTTPPort), nil
}

// pricePerDay is what a simulated lease costs. Deploy quotes it and Escrow drains
// at it, from one place on purpose: a stub whose quoted price and burn rate disagree
// would let the funds loop compute a horizon that is never reached, or one that is
// reached and never left.
func (d *DryRun) pricePerDay() float64 { return d.Cfg.Akash.Price.MaxUSDPerDay / 2 }

// Escrow reports the simulated deposit, drained at the price Deploy quoted.
//
// It drains on purpose. A stub that answered with a constant would let the funds
// loop pass a whole dry run without ever deciding to top up, and deciding to top up
// is the only thing it does.
func (d *DryRun) Escrow(_ context.Context, dseq string) (float64, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureLoaded()
	l, ok := d.live[dseq]
	if !ok {
		// A closed deployment has no escrow to read. Unknown rather than zero: zero
		// is a number the caller would act on, and a top-up against a lease that no
		// longer exists is money in a stranger's escrow.
		return 0, false, nil
	}
	var spent float64
	if !l.CreatedAt.Zero() {
		if age := d.now().Sub(l.CreatedAt.Time); age > 0 {
			spent = d.pricePerDay() * age.Hours() / 24
		}
	}
	remaining := d.deposits[dseq] - spent
	if remaining < 0 {
		// A real escrow does not go negative; it hits zero and the provider closes
		// the lease. Reporting a negative balance would make every caller's
		// arithmetic ask for a top-up bigger than the horizon it wants.
		remaining = 0
	}
	return remaining, true, nil
}

// TopUp adds to the simulated escrow, with the same minimum the real API enforces so
// that a caller relying on the floor behaves identically here.
func (d *DryRun) TopUp(_ context.Context, dseq string, usd float64) (float64, error) {
	if usd <= 0 {
		return 0, nil
	}
	if floor := d.Cfg.Akash.Funds.MinTopupUSD; usd < floor {
		usd = floor
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.ensureLoaded()
	if _, ok := d.live[dseq]; !ok {
		// Refused rather than recorded. Silently accepting a deposit into a closed
		// deployment is precisely the mistake worth failing loudly on, because the
		// real API would take the money.
		return 0, fmt.Errorf("dry-run: dseq %s is not open; nothing to top up", dseq)
	}
	d.deposits[dseq] += usd
	d.save()
	d.logf("dry-run: added $%.2f to the escrow on dseq %s", usd, dseq)
	return usd, nil
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
