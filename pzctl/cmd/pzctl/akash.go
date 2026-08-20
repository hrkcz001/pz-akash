package main

// `pzctl akash` — the operator's hand on the wallet.
//
// The controller drives every one of these calls by itself; this command exists for
// the two situations where nobody is driving it. The first is the one that cost v1
// real money: a controller that died between creating a deployment and recording it
// leaves a lease billing with no process watching, and the only way to find it is to
// ask the network what is open. `leases` asks, `escrow` prices it, `close` stops it.
//
// The second is a first run against the live API. Every wire shape in internal/akash
// is tested against a fake, and a fake is only ever as right as the person who wrote
// it: `providers` and `deploy` are how those shapes get confronted with the real
// Console API, on a deployment created to be thrown away.
//
// Everything here spends or reads real money, so nothing here guesses. A deploy that
// fails after creating something prints the dseq it created, on the same line as the
// error, because the next thing the operator has to do is close it.

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	"os"
	"text/tabwriter"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/akash"
	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

func cmdAkash(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("akash: want one of providers, deploy, leases, escrow, close")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("akash "+sub, flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	dseq := fs.String("dseq", "", "the deployment sequence number to act on")
	role := fs.String("role", "server", "which SDL to deploy: server or controller")
	controllerURL := fs.String("controller-url", "",
		"public controller URL baked into a server deploy (default: the agent discovers it from git)")
	closeAfter := fs.Bool("close", false,
		"close the deployment again as soon as it is routable — for a throwaway first run")
	timeout := fs.Duration("timeout", 0,
		"give up after this long; 0 uses the akash.timeouts values from the config")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	// Only `deploy` renders an SDL, and only a rendered SDL needs the deploy key and
	// the storage passwords. Demanding them for `leases` would make the one command
	// that finds a stranded lease unavailable from a laptop.
	d, cfg, logf, err := openDriver(*path, sub == "deploy")
	if err != nil {
		return err
	}

	ctx, stop := rootContext(*timeout)
	defer stop()

	switch sub {
	case "providers":
		return akashProviders(ctx, d, cfg, *role)
	case "deploy":
		return akashDeploy(ctx, d, *role, *controllerURL, *closeAfter, logf)
	case "leases":
		return akashLeases(ctx, d)
	case "escrow":
		return akashEscrow(ctx, d, *dseq)
	case "close":
		return akashClose(ctx, d, *dseq, logf)
	default:
		return fmt.Errorf("akash: unknown subcommand %q "+
			"(want providers, deploy, leases, escrow or close)", sub)
	}
}

// openDriver builds a live driver against the configured API base.
//
// withSDLSecrets separates the two kinds of caller. A read costs the API key and
// nothing else; a deploy bakes secrets into a manifest and has to have all of them,
// which is checked here rather than at the POST — the alternative is a funded
// deployment holding an SDL with a placeholder where the deploy key should be.
func openDriver(path string, withSDLSecrets bool) (*akash.Driver, *config.Config, func(string, ...any), error) {
	resolved, err := config.Find(path)
	if err != nil {
		return nil, nil, nil, err
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return nil, nil, nil, err
	}
	logf := func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, time.Now().In(cfg.Location()).Format("15:04:05")+" "+f+"\n", a...)
	}

	var set *secrets.Set
	if withSDLSecrets {
		if set, err = secrets.Load(secrets.RoleController, secretRequirements(cfg)); err != nil {
			return nil, nil, nil, fmt.Errorf("akash: %w", err)
		}
	} else {
		set = secrets.LoadOptional()
		if set.AkashAPIKey == "" {
			return nil, nil, nil, fmt.Errorf("akash: set PZ_AKASH_API_KEY (the managed-wallet key from Akash Console)")
		}
	}

	cl, err := akash.New(akash.Options{
		APIBase:   cfg.Akash.APIBase,
		APIKey:    set.AkashAPIKey,
		HTTP:      &http.Client{Timeout: cfg.Akash.API.Timeout.D()},
		Retries:   cfg.Akash.API.Retries,
		RetryWait: cfg.Akash.API.RetryWait.D(),
		Logf:      logf,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	d, err := akash.NewDriver(akash.DriverOptions{
		Client:  cl,
		Cfg:     cfg,
		Secrets: set,
		Logf:    logf,
		Now:     time.Now,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	return d, cfg, logf, nil
}

// resourcesFor picks the resource block a role deploys against, and says whether it
// needs a dedicated IP. The two travel together: the IP requirement is a placement
// filter, not a size, and getting it wrong is a server nobody can join.
func resourcesFor(cfg *config.Config, role string) (config.Resources, bool, error) {
	switch role {
	case "server":
		return cfg.Server.Resources, true, nil
	case "controller":
		return cfg.Controller.Resources, false, nil
	default:
		return config.Resources{}, false, fmt.Errorf("akash: unknown role %q (want server or controller)", role)
	}
}

// akashProviders answers "could this deploy anywhere", without deploying.
//
// It is the free half of a deploy: the same provider list, through the same
// placement filter, with the rejection reasons that v1 replaced with the words "no
// bids found". When a deploy fails to find a provider this is the command that says
// which condition did the rejecting.
func akashProviders(ctx context.Context, d *akash.Driver, cfg *config.Config, role string) error {
	res, requireIP, err := resourcesFor(cfg, role)
	if err != nil {
		return err
	}
	// A dollar-pegged denomination needs no rate, and this command must not fail
	// because a price oracle is down: the fallback is only consulted for a
	// denomination that cannot be priced without one.
	cr, err := akash.CriteriaFor(cfg, res, requireIP, cfg.Akash.Price.AKTUSDFallback)
	if err != nil {
		return err
	}
	ok, err := d.EligibleProviders(ctx, cr)
	if err != nil {
		return err
	}
	if len(ok) == 0 {
		return fmt.Errorf("akash: no provider meets the placement criteria for the %s", role)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "PROVIDER\tCOUNTRY\tUPTIME30D\tHOST")
	for _, p := range ok {
		fmt.Fprintf(w, "%s\t%s\t%.3f\t%s\n", p.Owner, orDash(p.CountryCode()), p.Uptime30d.F(), p.HostURI)
	}
	return w.Flush()
}

// akashLeases lists what is still billing — invariant I1's last line of defence.
func akashLeases(ctx context.Context, d *akash.Driver) error {
	leases, err := d.Adopt(ctx)
	if err != nil {
		return err
	}
	if len(leases) == 0 {
		fmt.Println("nothing open that looks like ours")
		return nil
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "DSEQ\tGSEQ\tOSEQ\tPROVIDER")
	for _, l := range leases {
		fmt.Fprintf(w, "%s\t%d\t%d\t%s\n", l.DSeq, l.GSeq, l.OSeq, orDash(l.Provider))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Printf("\n%d open — close one with `pzctl akash close --dseq DSEQ`\n", len(leases))
	return nil
}

func akashEscrow(ctx context.Context, d *akash.Driver, dseq string) error {
	if dseq == "" {
		return fmt.Errorf("akash escrow: want --dseq DSEQ")
	}
	e, err := d.Escrow(ctx, dseq)
	if err != nil {
		return err
	}
	if !e.Known {
		// Explicitly not zero: a top-up decided from a wrong zero spends real money on
		// a deployment that needed nothing.
		fmt.Printf("dseq %s holds no funds in %s — this build cannot price what it does hold\n", dseq, e.Denom)
		return nil
	}
	fmt.Printf("dseq %s: $%.2f left, $%.2f spent (%s)\n", dseq, e.RemainingUSD, e.SpentUSD, e.Denom)
	return nil
}

func akashClose(ctx context.Context, d *akash.Driver, dseq string, logf func(string, ...any)) error {
	if dseq == "" {
		return fmt.Errorf("akash close: want --dseq DSEQ")
	}
	// Deliberately not gated on Alive: err toward believing money is still being
	// spent. A close against a deployment that is already gone answers 404, which the
	// driver treats as the outcome we wanted.
	if err := d.Close(ctx, state.Lease{DSeq: dseq}); err != nil {
		return err
	}
	logf("akash: dseq %s is closed", dseq)
	return nil
}

// akashDeploy creates one deployment and reports what it got.
//
// The dseq is printed before the error is returned, and that ordering is the whole
// point of the function. From the POST onward there is a funded escrow; a deploy
// that dies while waiting for the endpoint has a provider billing against it, and an
// operator who cannot see the dseq cannot stop it. This is invariant I1 at the CLI,
// and it is the same reason akash.run carries the lease out on its failure paths.
//
// --close makes it a round trip: create, lease, wait, close. That is the shape of a
// first live run — it proves every wire format in the package against the real API
// and leaves nothing behind but the seconds of billing it took.
func akashDeploy(ctx context.Context, d *akash.Driver, role, controllerURL string,
	closeAfter bool, logf func(string, ...any)) error {

	if _, _, err := resourcesFor(d.Cfg, role); err != nil {
		return err
	}

	var (
		res akash.Result
		err error
	)
	switch role {
	case "server":
		res, err = d.DeployServer(ctx, akash.DeployOptions{ControllerURL: controllerURL, Attempt: 1})
	case "controller":
		res, err = d.DeployController(ctx)
	}

	if res.Lease.DSeq != "" {
		fmt.Printf("dseq      %s\n", res.Lease.DSeq)
	}
	if res.Lease.Provider != "" {
		fmt.Printf("provider  %s (gseq %d, oseq %d)\n", res.Lease.Provider, res.Lease.GSeq, res.Lease.OSeq)
	}
	if p := res.Price; p.AmountPerBlock > 0 {
		fmt.Printf("price     %d %s/block = $%.4f/hour = $%.2f/day\n",
			p.AmountPerBlock, p.Denom, p.USDPerHour, p.USDPerDay)
	}
	if res.Endpoint.Ready() {
		fmt.Printf("endpoint  %s game %d udp %d rcon %d\n",
			res.Endpoint.IP, res.Endpoint.GamePort, res.Endpoint.UDPPort, res.Endpoint.RCONPort)
	}
	if res.URL != "" {
		fmt.Printf("url       %s\n", res.URL)
	}

	if err != nil {
		if res.Lease.DSeq != "" {
			// Named on the error line as well as above it: whatever swallows the output,
			// the number needed to stop the billing travels with the failure.
			return fmt.Errorf("%w — dseq %s exists and is billing; close it with "+
				"`pzctl akash close --dseq %s`", err, res.Lease.DSeq, res.Lease.DSeq)
		}
		return err
	}

	if !closeAfter {
		fmt.Printf("\nstill open — close it with `pzctl akash close --dseq %s`\n", res.Lease.DSeq)
		return nil
	}
	logf("akash: closing dseq %s again, as asked", res.Lease.DSeq)
	return d.Close(ctx, res.Lease)
}
