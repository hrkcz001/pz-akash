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
	"github.com/hrkcz001/pz-akash/pzctl/internal/fsm"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// akashDriver adapts *akash.Driver to fsm.Akash.
//
// Close, Alive and Adopt already have the signatures the interface asks for and
// are promoted from the embedded driver — deliberately, so that the two most
// dangerous calls in the system have no translation layer to get wrong. Only
// Deploy needs one, because the FSM's request and the driver's options are
// different types on purpose: the FSM must not be able to reach the parts of a
// deploy that config owns.
type akashDriver struct{ *akash.Driver }

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
	return fsm.DeployResult{
		Lease:    res.Lease,
		Endpoint: res.Endpoint,
		Price:    res.Price,
	}, err
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
	return akashDriver{Driver: d}, nil
}
