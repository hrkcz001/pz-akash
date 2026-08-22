package main

// Where the Console API client meets the state machine.
//
// internal/fsm declares what it needs from a provider (fsm.Akash); internal/akash
// knows how to talk to Console. Neither imports the other, and this file — the
// only place that knows both vocabularies — is the seam. That is not tidiness for
// its own sake: an import from akash to fsm would make the deploy path untestable
// without a state machine, and one from fsm to akash would put an HTTP client
// behind every lifecycle test. Both packages' test suites depend on staying free
// of the other.

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/akash"
	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/dns"
	"github.com/hrkcz001/pz-akash/pzctl/internal/fsm"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// akashDriver adapts *akash.Driver to fsm.Akash, and hangs the zone off the same
// seam.
//
// Alive, Adopt and TopUp have the signatures the interface asks for and are promoted
// from the embedded driver — deliberately, so that the calls deciding whether a lease
// is still billing, and the one that pays for it, have no translation layer to get
// wrong. Deploy, Close and Escrow are translated: Deploy because the FSM's request and
// the driver's options are different types on purpose, so the FSM cannot reach the
// parts of a deploy that config owns; Close because the game record has to outlive
// neither the lease nor a controller restart; and Escrow because akash.Escrow is a
// type the FSM must not be able to name. All three hand the driver's own error back
// unchanged; DNS is never allowed to change what the FSM believes about a lease.
type akashDriver struct {
	*akash.Driver
	// zone is nil when dns.enabled is off, and every method on it is nil-safe, so
	// nothing below has to ask.
	zone *dns.Cloudflare
	logf func(string, ...any)
}

// Deploy runs a server deploy.
//
// The result is returned alongside the error, never instead of it. A deploy that
// took a lease and then failed while waiting for the endpoint has already funded
// an escrow and started a provider billing, so the lease has to reach the caller
// even on the failure path — that is invariant I1, and the FSM records what it is
// given before it acts on the error.
func (a akashDriver) Deploy(ctx context.Context, req fsm.DeployRequest) (fsm.DeployResult, error) {
	res, err := a.Driver.DeployServer(ctx, akash.DeployOptions{
		ControllerURL: req.ControllerURL,
		RestoreTarget: req.RestoreTarget,
		Attempt:       req.Attempt,
	})
	out := fsm.DeployResult{
		Lease:    res.Lease,
		Endpoint: res.Endpoint,
		Price:    res.Price,
	}
	if err == nil && out.Endpoint.Ready() {
		a.syncGameRecord(ctx, out.Endpoint.IP)
	}
	return out, err
}

// Close closes the deployment and then takes the game record down.
//
// Order matters in one direction only: the record is cleared after billing has
// actually stopped. Clearing first would point players at nothing while the server
// is still running, and a Close that then failed would leave a live server with no
// name — whereas a lease that is gone with the record still up costs a name that
// resolves to an address the provider will hand to somebody else.
func (a akashDriver) Close(ctx context.Context, l state.Lease) error {
	if err := a.Driver.Close(ctx, l); err != nil {
		return err
	}
	changes, err := a.zone.ClearGame(ctx)
	if err != nil {
		// Not returned: the lease is closed, and reporting a failure here would make
		// the FSM retry a close that already succeeded.
		a.logf("dns: could not clear the game record — it still points at a lease that is gone: %v", err)
	}
	for _, ch := range changes {
		a.logf("dns: %s", ch)
	}
	return nil
}

// Escrow flattens the driver's reading into the three values the FSM asked for.
//
// Known is carried through untouched, and that is the whole reason this wrapper is
// worth reading: it is the difference between "the deposit is empty" and "we could
// not tell what the deposit holds", and only the first is a reason to spend money.
// Collapsing the two here — by returning 0 with a nil error on an unpriceable
// balance, say — would move that decision into a place nobody looks at it.
func (a akashDriver) Escrow(ctx context.Context, dseq string) (float64, bool, error) {
	e, err := a.Driver.Escrow(ctx, dseq)
	if err != nil {
		return 0, false, err
	}
	return e.RemainingUSD, e.Known, nil
}

// syncGameRecord points the game name at a fresh lease. Log-only by construction:
// see the package comment on internal/dns. A record that did not get written costs
// an address the operator reads off the dashboard, which is where v1 left it
// permanently; a deploy failed over a Cloudflare 502 costs a redeploy and a world
// rollback.
func (a akashDriver) syncGameRecord(ctx context.Context, ip string) {
	changes, err := a.zone.SyncGame(ctx, ip)
	if err != nil {
		a.logf("dns: could not point the game record at %s: %v", ip, err)
		return
	}
	for _, ch := range changes {
		a.logf("dns: %s", ch)
	}
}

// secretRequirements maps the config switches that decide which secrets are
// mandatory. It is the same mapping the SDL renderer applies, and it lives next to
// each caller rather than in config, so that internal/config keeps knowing nothing
// about secrets.
func secretRequirements(cfg *config.Config) secrets.Requirements {
	return secrets.Requirements{
		RCON:         cfg.Server.RCON.Enabled,
		DNS:          cfg.DNS.Enabled,
		JoinPassword: cfg.Game.PasswordProtected,
	}
}

// newAkashDriver builds the live driver: the API client, the secrets that get
// baked into a rendered SDL, and the clock the timestamps come from.
//
// Secrets are demanded here, at startup, rather than at the first deploy. A
// controller that runs for an hour and then discovers it cannot render a server
// SDL has already accepted a start trigger it cannot honour, and the operator
// finds out from a failed status instead of from a refusal to boot.
func newAkashDriver(cfg *config.Config, logf func(string, ...any)) (fsm.Akash, error) {
	set, err := secrets.Load(secrets.RoleController, secretRequirements(cfg))
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}

	cl, err := akash.New(akash.Options{
		APIBase: cfg.Akash.APIBase,
		APIKey:  set.AkashAPIKey,
		// One request's bound. The polling loops above it carry their own, longer,
		// deadlines from akash.timeouts.
		HTTP:      &http.Client{Timeout: cfg.Akash.API.Timeout.D()},
		Retries:   cfg.Akash.API.Retries,
		RetryWait: cfg.Akash.API.RetryWait.D(),
		Logf:      logf,
	})
	if err != nil {
		return nil, err
	}

	d, err := akash.NewDriver(akash.DriverOptions{
		Client:  cl,
		Cfg:     cfg,
		Secrets: set,
		Logf:    logf,
		// Deliberately not time.Now: every timestamp the driver writes is in
		// identity.timezone, and NewDriver takes the location from config. Passing
		// the clock explicitly keeps the two halves of that in one place.
		Now: time.Now,
	})
	if err != nil {
		return nil, err
	}

	// nil when dns.enabled is off, which is a supported configuration: without a
	// zone the endpoint reaches players off the dashboard, exactly as in v1.
	zone, err := newZone(cfg, set, logf)
	if err != nil {
		return nil, err
	}
	return akashDriver{Driver: d, zone: zone, logf: logf}, nil
}

// newZone builds the Cloudflare client from config plus the one secret it needs.
//
// A misconfigured zone is refused here, at startup, and not at the first deploy:
// dns.zone_id pointing at a zone the token cannot see is an operator mistake, and
// discovering it from a deploy that half-worked is how v1's DNS failures stayed
// invisible. Once past this point, nothing DNS does can fail a deploy.
func newZone(cfg *config.Config, set *secrets.Set, logf func(string, ...any)) (*dns.Cloudflare, error) {
	zone, err := dns.New(dns.Options{
		Zone:  cfg.DNS,
		Token: set.CloudflareAPIToken,
		HTTP:  &http.Client{Timeout: cfg.DNS.Timeout.D()},
		Logf:  logf,
	})
	if err != nil {
		return nil, fmt.Errorf("controller: %w", err)
	}
	return zone, nil
}
