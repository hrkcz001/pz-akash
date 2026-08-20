package main

// `pzctl dns` — the operator's entry point to the Cloudflare zone, and v2's
// replacement for running update_cloudflare.py by hand at boot.
//
// The controller record is not automatic, and that is deliberate rather than
// unfinished: the controller does not know its own public address. Akash assigns the
// provider hostname and port at lease time to the deployment being created, so the
// value is known to whoever created the controller's own lease — CI, or the operator
// — and not to the process running inside it. v1 had the same shape; it took the
// target as argv[1]. The game record, by contrast, is a fact the controller learns
// every time it deploys a server, and it is written automatically there.

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/dns"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

func cmdDNS(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("dns: want one of zones, sync, clear-game")
	}
	sub, rest := args[0], args[1:]

	fs := flag.NewFlagSet("dns "+sub, flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	controller := fs.String("controller", "",
		"where the controller answers, as a URL or host:port — the apex and www are pointed here")
	game := fs.String("game", "",
		"the game server's address — dns.game_record is pointed here, DNS-only")
	dryRun := fs.Bool("dry-run", false, "read the zone and report what would change, writing nothing")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	resolved, err := config.Find(*path)
	if err != nil {
		return err
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return err
	}
	if !cfg.DNS.Enabled {
		return fmt.Errorf("dns: dns.enabled is false in %s — nothing to do", resolved)
	}

	// One secret, demanded on its own. secrets.Load(RoleController) would require
	// the nine a controller needs to run, and none of them has anything to do with
	// a zone: this command renders no SDL and creates no lease, and asking for the
	// Akash key and the deploy key to read a DNS record would make it unusable from
	// a laptop — which is exactly where an operator runs it.
	set := secrets.LoadOptional()
	if set.CloudflareAPIToken == "" {
		return fmt.Errorf("dns: set PZ_CLOUDFLARE_API_TOKEN (the token needs Zone.DNS:Edit, " +
			"and Zone Settings:Edit plus Zone.Config:Edit for the zone settings and the origin rule)")
	}

	logf := func(format string, a ...any) { fmt.Fprintf(os.Stderr, format+"\n", a...) }
	zone, err := dns.New(dns.Options{
		Zone:   cfg.DNS,
		Token:  set.CloudflareAPIToken,
		Logf:   logf,
		DryRun: *dryRun,
	})
	if err != nil {
		return err
	}

	ctx := context.Background()
	switch sub {
	case "zones":
		return dnsZones(ctx, zone)
	case "sync":
		return dnsSync(ctx, zone, *controller, *game)
	case "clear-game":
		return printChanges(zone.ClearGame(ctx))
	default:
		return fmt.Errorf("dns: unknown subcommand %q (want zones, sync or clear-game)", sub)
	}
}

// dnsZones answers "what do I put in dns.zone_id". v1 guessed by taking the first
// zone the token could see, which is right until the account holds two domains and
// then silently reconfigures the wrong one — so the id is config here, and this is
// how an operator finds it.
func dnsZones(ctx context.Context, zone *dns.Cloudflare) error {
	zones, err := zone.Zones(ctx)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return fmt.Errorf("dns: the token can see no zones at all — check its scope")
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 8, 2, ' ', 0)
	fmt.Fprintln(w, "ZONE ID\tNAME\tSTATUS")
	for _, z := range zones {
		fmt.Fprintf(w, "%s\t%s\t%s\n", z.ID, z.Name, z.Status)
	}
	return w.Flush()
}

func dnsSync(ctx context.Context, zone *dns.Cloudflare, controller, game string) error {
	if controller == "" && game == "" {
		return fmt.Errorf("dns sync: want --controller URL, --game ADDRESS, or both")
	}
	// Both halves are attempted even when the first fails, for the same reason
	// SyncController attempts all of its own steps: a half-applied zone is the state
	// v1 could reach and could not report.
	var errs []error
	if controller != "" {
		if err := printChanges(zone.SyncController(ctx, controller)); err != nil {
			errs = append(errs, err)
		}
	}
	if game != "" {
		if err := printChanges(zone.SyncGame(ctx, game)); err != nil {
			errs = append(errs, err)
		}
	}
	switch len(errs) {
	case 0:
		return nil
	case 1:
		return errs[0]
	default:
		return fmt.Errorf("dns sync: %v; %v", errs[0], errs[1])
	}
}

// printChanges prints what a sync did, including the records it found already
// correct: an operator running this against a zone v1 managed needs to see that a
// record was checked, not infer it from silence.
func printChanges(changes []dns.Change, err error) error {
	for _, ch := range changes {
		fmt.Println(ch)
	}
	if len(changes) == 0 && err == nil {
		fmt.Println("nothing to do")
	}
	return err
}
