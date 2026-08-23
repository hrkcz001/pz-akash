package akash

// Deploying, which is where the money is.
//
// The sequence is fixed by the API and every step of it can fail in a way that
// leaves something behind:
//
//	POST /v1/deployments  -> a deployment and a manifest, escrow already funded
//	GET  /v1/bids         -> poll until providers respond
//	POST /v1/leases       -> a lease; the provider starts billing
//	GET  /v1/deployments/{dseq} -> poll until the service is ready and routable
//
// The rule that shapes all of it: from the first call onward there is something
// to clean up, so the caller must be handed the dseq even when the error is
// non-nil. v1 returned a bare exit code from a shell function here, and a deploy
// that died between the lease and the endpoint left a lease nobody was watching,
// billing until its deposit ran out. Every return path below carries the identity
// of whatever it created.

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
	"github.com/hrkcz001/pz-akash/pzctl/internal/sdl"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Driver creates and destroys deployments. It is the real implementation of the
// interface the state machine drives, and the only place in the system that
// spends money.
type Driver struct {
	Client *Client
	Cfg    *config.Config
	// Secrets are baked into the rendered SDL. A deploy without them produces a
	// server that cannot clone the save repository, so New refuses it.
	Secrets *secrets.Set
	Oracle  Oracle
	Logf    func(string, ...any)
	// Now and Loc are how timestamps get made. Loc comes from identity.timezone,
	// never from the host: a provider's clock is not ours to trust and a backup
	// named in the provider's local time is a backup nobody can order by hand.
	Now func() time.Time
	Loc *time.Location

	// sleep is overridable so tests can walk the polling loops without waiting.
	sleep func(context.Context, time.Duration) error

	mu    sync.Mutex
	skips map[string]time.Time
}

// DriverOptions is what NewDriver needs. Everything optional has a default that
// is correct in production, so a test only states what it is testing.
type DriverOptions struct {
	Client  *Client
	Cfg     *config.Config
	Secrets *secrets.Set
	Logf    func(string, ...any)
	Now     func() time.Time
	Sleep   func(context.Context, time.Duration) error
}

// NewDriver wires a driver from config.
func NewDriver(o DriverOptions) (*Driver, error) {
	if o.Client == nil {
		return nil, fmt.Errorf("akash: driver needs a client")
	}
	if o.Cfg == nil {
		return nil, fmt.Errorf("akash: driver needs a config")
	}
	d := &Driver{
		Client:  o.Client,
		Cfg:     o.Cfg,
		Secrets: o.Secrets,
		Logf:    o.Logf,
		Now:     o.Now,
		Loc:     o.Cfg.Location(),
		sleep:   o.Sleep,
		skips:   map[string]time.Time{},
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.sleep == nil {
		d.sleep = sleepCtx
	}
	d.Oracle = Oracle{
		URL:      o.Cfg.Akash.Price.PriceOracleURL,
		Timeout:  time.Duration(o.Cfg.Akash.Timeouts.BidPoll) * 3,
		Fallback: o.Cfg.Akash.Price.AKTUSDFallback,
		Logf:     d.Logf,
	}
	return d, nil
}

// now is the current instant in identity.timezone, from the driver's own clock.
// Both halves matter: the location is config so a backup is never named in a
// provider's local time, and the instant comes from d.Now so a test can drive a
// whole deploy without the wall clock — and so the two stamps a deploy writes
// cannot disagree by the seconds the polling loops take.
func (d *Driver) now() state.Stamp { return state.At(d.Now().In(d.Loc)) }

// Result is a created deployment: what it is, where it is, and what it costs.
type Result struct {
	Lease    state.Lease
	Endpoint state.Endpoint
	Price    state.Price
	// URL is set for shared-endpoint deployments (the controller), where there is
	// no dedicated IP and the provider assigns a hostname and port.
	URL string
}

// DeployOptions are the per-deploy values. Everything else comes from config.
// (Options, without the prefix, is the client's — a deploy and a client are
// configured from different places and must not share a name.)
type DeployOptions struct {
	// ControllerURL is baked into the server's environment so the agent can reach
	// storage before it has read anything from git.
	ControllerURL string
	// RestoreTarget travels in the environment for logging; the agent reads the
	// authoritative value from the controller's state branch.
	RestoreTarget string
	// Attempt counts from 1 and only affects logs and skip-listing.
	Attempt int
}

// --- the skip list ---

// Skip puts a provider out of consideration for akash.placement.skip_ttl.
//
// The list is in memory, so a controller restart forgets it. That is a deliberate
// trade: persisting it means either another file in the git bus or a field in the
// shared state document, and the cost of forgetting is one wasted deploy attempt
// against a provider that will probably fail us again within the minute — which
// puts it right back on the list.
func (d *Driver) Skip(owner, why string) {
	if strings.TrimSpace(owner) == "" {
		return
	}
	ttl := time.Duration(d.Cfg.Akash.Placement.SkipTTL)
	if ttl <= 0 {
		return
	}
	d.mu.Lock()
	d.skips[owner] = d.Now().Add(ttl)
	d.mu.Unlock()
	d.Logf("akash: skipping %s for %s: %s", owner, ttl, why)
}

// Skipped reports whether a provider is currently skip-listed, dropping entries
// whose TTL has passed.
func (d *Driver) Skipped(owner string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	until, ok := d.skips[owner]
	if !ok {
		return false
	}
	if d.Now().After(until) {
		delete(d.skips, owner)
		return false
	}
	return true
}

// --- pricing ---

// rate returns the AKT/USD rate if the configured denomination needs one, and 0
// otherwise. This is the whole benefit of bidding in a dollar-pegged credit: on
// the normal path the oracle is never called, so it can never be the reason a
// server fails to start.
func (d *Driver) rate(ctx context.Context) (float64, error) {
	if !denom.NeedsOracle(d.Cfg.Akash.Price.Denom) {
		return 0, nil
	}
	rate, source, err := d.Oracle.Rate(ctx)
	if err != nil {
		return 0, err
	}
	d.Logf("akash: AKT/USD = %g (%s)", rate, source)
	return rate, nil
}

// ceiling is the per-block bid ceiling written into the SDL.
func (d *Driver) ceiling(aktUSD float64) (int, error) {
	return denom.CeilingPerBlock(
		d.Cfg.Akash.Price.MaxUSDPerDay,
		d.Cfg.Akash.Price.Denom,
		d.Cfg.Akash.BlocksPerDay,
		aktUSD,
	)
}

// deposit is the initial escrow, in dollars. The Console API takes deposits in
// dollars whatever the bid denomination is.
func (d *Driver) deposit() float64 {
	return sdl.InitialDepositUSD(
		d.Cfg.Akash.Price.MaxUSDPerDay,
		d.Cfg.Akash.InitialDepositDays,
		d.Cfg.Akash.Funds.Margin,
		d.Cfg.Akash.Funds.MinTopupUSD,
	)
}

// --- deploying ---

// plan is one deployment's worth of decisions, made before anything is created.
type plan struct {
	role     string
	service  string
	doc      []byte
	criteria Criteria
	ceiling  int
	deposit  float64
	// kind is the address to wait for. It is derived from the same config flag as
	// criteria.RequireIP and must agree with it: filtering for IP-capable providers
	// and then waiting for a shared endpoint would time out on a billing lease.
	kind   addrKind
	aktUSD float64
}

// DeployServer deploys the game server, with a bid ceiling computed from
// akash.price.max_usd_per_day.
//
// server.ip_lease decides both the market and the address. With it on we filter for
// the handful of providers that sell dedicated IPs and wait for one; with it off the
// market is every provider and the address is the provider's own hostname plus a
// port it picks. The flag has to reach both places from one read — asking for one
// shape and waiting for the other is a lease that bills while it never becomes
// routable.
func (d *Driver) DeployServer(ctx context.Context, o DeployOptions) (Result, error) {
	aktUSD, err := d.rate(ctx)
	if err != nil {
		return Result{}, err
	}
	ceiling, err := d.ceiling(aktUSD)
	if err != nil {
		return Result{}, err
	}
	wantIP := d.Cfg.Server.IPLease
	cr, err := CriteriaFor(d.Cfg, d.Cfg.Server.Resources, wantIP, aktUSD)
	if err != nil {
		return Result{}, err
	}
	doc, err := sdl.RenderServer(sdl.Input{
		Cfg:              d.Cfg,
		Secrets:          d.Secrets,
		ControllerURL:    o.ControllerURL,
		MaxPricePerBlock: ceiling,
	})
	if err != nil {
		return Result{}, fmt.Errorf("rendering the server SDL: %w", err)
	}
	kind := addrSharedGame
	if wantIP {
		kind = addrDedicatedIP
	}
	d.Logf("akash: deploying the server (attempt %d, restore %q): ceiling %d %s/block, deposit $%.2f, ip_lease %v",
		o.Attempt, o.RestoreTarget, ceiling, d.Cfg.Akash.Price.Denom, d.deposit(), wantIP)
	return d.run(ctx, plan{
		role:     "server",
		service:  sdl.ServerService,
		doc:      doc,
		criteria: cr,
		ceiling:  ceiling,
		deposit:  d.deposit(),
		kind:     kind,
		aktUSD:   aktUSD,
	})
}

// DeployController deploys the controller itself: shared endpoints, and the
// ceiling from controller.pricing_amount because there is no deploy-time
// computation to do — an operator ran this by hand.
func (d *Driver) DeployController(ctx context.Context) (Result, error) {
	aktUSD, err := d.rate(ctx)
	if err != nil {
		return Result{}, err
	}
	cr, err := CriteriaFor(d.Cfg, d.Cfg.Controller.Resources, false, aktUSD)
	if err != nil {
		return Result{}, err
	}
	// The controller runs the whole system, so its own placement is not restricted
	// by the players' geography: latency to a dashboard is nobody's problem.
	cr.Countries = nil
	doc, err := sdl.RenderController(sdl.Input{Cfg: d.Cfg, Secrets: d.Secrets})
	if err != nil {
		return Result{}, fmt.Errorf("rendering the controller SDL: %w", err)
	}
	d.Logf("akash: deploying the controller: ceiling %d %s/block, deposit $%.2f",
		d.Cfg.Controller.PricingAmount, d.Cfg.Akash.Price.Denom, d.deposit())
	return d.run(ctx, plan{
		role:     "controller",
		service:  sdl.ControllerService,
		doc:      doc,
		criteria: cr,
		ceiling:  d.Cfg.Controller.PricingAmount,
		deposit:  d.deposit(),
		kind:     addrSharedURL,
		aktUSD:   aktUSD,
	})
}

// run executes a plan.
//
// The provider list is fetched before the deployment is created, which is not
// merely an optimisation: if nothing on the network can host us, creating the
// deployment first funds an escrow we then have to close and wait to reclaim. v1
// created first and discovered the empty market afterwards, every time.
func (d *Driver) run(ctx context.Context, p plan) (res Result, err error) {
	providers, err := d.EligibleProviders(ctx, p.criteria)
	if err != nil {
		return res, err
	}
	if len(providers) == 0 {
		return res, fmt.Errorf("no provider on the network meets the placement criteria for the %s", p.role)
	}

	dseq, manifest, err := d.create(ctx, p.doc, p.deposit)
	if dseq != "" {
		// Recorded before the error is checked. From here on there is a funded
		// escrow, and a caller that does not know its dseq cannot close it.
		res.Lease.DSeq = dseq
		res.Lease.CreatedAt = d.now()
	}
	if err != nil {
		return res, err
	}
	d.Logf("akash: created dseq %s (deposit $%.2f)", dseq, p.deposit)

	choice, err := d.waitForBid(ctx, dseq, p.criteria, providers)
	if err != nil {
		return res, err
	}
	res.Lease.GSeq = choice.Bid.ID.GSeq
	res.Lease.OSeq = choice.Bid.ID.OSeq
	res.Lease.Provider = choice.Provider.Owner
	// Recorded now because this is the only moment the provider record is in hand;
	// after this the lease is identified by address alone.
	res.Lease.Location = choice.Provider.Where()
	res.Price = state.Price{
		AmountPerBlock: choice.AmountPerBlock,
		Denom:          choice.Denom,
		AKTUSD:         p.aktUSD,
		USDPerHour:     choice.USDPerHour,
		USDPerDay:      choice.USDPerDay,
		QuotedAt:       d.now(),
	}
	d.Logf("akash: chose %s", choice)

	if err := d.lease(ctx, res.Lease, manifest); err != nil {
		// The provider bid and then would not lease. It gets a rest, so the next
		// attempt does not walk into the same wall.
		d.Skip(choice.Provider.Owner, "would not accept the lease")
		return res, err
	}
	d.Logf("akash: leased dseq %s from %s", dseq, choice.Provider.Owner)

	// choice.Provider travels along because the endpoint wait may ask the provider
	// directly, and only the provider record knows its hostUri.
	ep, url, err := d.waitForEndpoint(ctx, res.Lease, choice.Provider, p.service, p.kind)
	if err != nil {
		d.Skip(choice.Provider.Owner, "leased but never became routable")
		return res, err
	}
	res.Endpoint, res.URL = ep, url
	return res, nil
}

// EligibleProviders fetches the provider list and applies the placement filter,
// logging why each rejection happened. The reasons are the point: "no bids found"
// with no explanation is the single least useful line in v1's logs.
func (d *Driver) EligibleProviders(ctx context.Context, cr Criteria) ([]Provider, error) {
	var list providerList
	if err := d.Client.do(ctx, "GET", "/v1/providers?scope=all", nil, &list); err != nil {
		return nil, err
	}
	ok, bad := SelectProviders(cr, list, d.Skipped)
	d.Logf("akash: %d of %d providers eligible; rejected: %s", len(ok), len(list), Reasons(bad))
	return ok, nil
}

// create posts the SDL and returns the dseq and the manifest.
//
// The manifest exists only in this response and is required by the lease call, so
// losing it means a funded deployment that can never be leased — a slow leak with
// no symptom until the escrow drains. It is returned rather than stored.
func (d *Driver) create(ctx context.Context, doc []byte, depositUSD float64) (dseq, manifest string, err error) {
	body := map[string]any{"data": map[string]any{
		"sdl":     string(doc),
		"deposit": depositUSD,
	}}
	var out createDeploymentResponse
	if err := d.Client.do(ctx, "POST", "/v1/deployments", body, &out); err != nil {
		// A dseq can come back alongside a failure; hand it up either way.
		return out.Data.DSeq, out.Data.Manifest, fmt.Errorf("creating the deployment: %w", err)
	}
	if out.Data.DSeq == "" {
		return "", "", fmt.Errorf("the API accepted the deployment but returned no dseq")
	}
	if out.Data.Manifest == "" {
		return out.Data.DSeq, "", fmt.Errorf("dseq %s was created without a manifest, so it cannot be leased", out.Data.DSeq)
	}
	return out.Data.DSeq, out.Data.Manifest, nil
}

// lease accepts a bid. The manifest goes back verbatim.
func (d *Driver) lease(ctx context.Context, l state.Lease, manifest string) error {
	body := map[string]any{
		"manifest": manifest,
		"leases": []map[string]any{{
			"dseq":     l.DSeq,
			"gseq":     l.GSeq,
			"oseq":     l.OSeq,
			"provider": l.Provider,
		}},
	}
	var out struct {
		Data struct {
			Deployment struct {
				State string `json:"state"`
			} `json:"deployment"`
		} `json:"data"`
	}
	if err := d.Client.do(ctx, "POST", "/v1/leases", body, &out); err != nil {
		return fmt.Errorf("taking the lease on dseq %s: %w", l.DSeq, err)
	}
	// A 200 here is not success on its own: the API reports the deployment state,
	// and anything but active means the lease did not take.
	if got := strings.ToLower(strings.TrimSpace(out.Data.Deployment.State)); got != leaseStateActive {
		return fmt.Errorf("dseq %s is %q after leasing, want %q", l.DSeq, got, leaseStateActive)
	}
	return nil
}
