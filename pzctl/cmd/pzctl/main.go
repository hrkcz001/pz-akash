// Command pzctl is the single binary for the PZ-on-Akash system. It runs the
// controller, runs the in-container server agent, and provides the offline tools
// used to inspect configuration and render Akash SDLs.
//
// Step 1 of the rewrite ships the offline tools only:
//
//	pzctl config validate
//	pzctl config dump
//	pzctl config secrets
//	pzctl sdl render controller|server
//
// Step 2 adds the read-only state inspector:
//
//	pzctl state show
//
// Step 3 adds the controller itself, with the Akash driver stubbed:
//
//	pzctl controller --dry-run
//
// Step 4 adds the in-container agent, which replaces v1's entrypoint.sh:
//
//	pzctl agent
//
// Step 5 adds the live Akash driver and the Cloudflare zone:
//
//	pzctl controller
//	pzctl akash providers|deploy|leases|escrow|close
//	pzctl dns zones|sync|clear-game
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/denom"
	"github.com/hrkcz001/pz-akash/pzctl/internal/sdl"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// version is overridable at build time with -ldflags "-X main.version=...".
var version = "dev"

const usage = `pzctl — Project Zomboid on Akash

Usage:
  pzctl config validate [-c FILE]        check config.yaml and report every problem
  pzctl config dump [-c FILE]            print the effective config, defaults included
  pzctl config secrets                   list the environment variables that hold secrets
  pzctl sdl render <controller|server>   render an Akash SDL from the config
  pzctl state show                       read the live state; --dir reads a v1 checkout
  pzctl controller [--dry-run]           run the controller; --dry-run stubs Akash
  pzctl agent [--controller-url URL]     run the in-container server agent
  pzctl akash providers [--role ROLE]    list the providers that meet the placement rules
  pzctl akash quote [--ip both|yes|no]   what the server would cost, with and without an IP
  pzctl akash deploy --role ROLE         create one deployment; --close closes it again
  pzctl akash leases                     list every open deployment that looks like ours
  pzctl akash escrow --dseq DSEQ         what a deployment has left to spend
  pzctl akash close --dseq DSEQ          close a deployment and stop the billing
  pzctl dns zones                        list the Cloudflare zones the token can see
  pzctl dns sync --controller URL        point the apex (and www) at the controller
  pzctl dns sync --game ADDRESS          point dns.game_record at the game server
  pzctl dns clear-game                   remove the game record
  pzctl version

Config file resolution, in order:
  -c FILE, $PZ_CONFIG, ./config.yaml, ./pzctl/config.yaml

` + "`pzctl dns`" + ` accepts --dry-run, which reads the live zone and reports what it
would change without writing anything. The game record is also written automatically
on every server deploy; the controller record is not, because a controller cannot
know the address Akash assigned to its own lease.

` + "`pzctl akash`" + ` spends real money. Every deployment it creates funds an escrow,
so a deploy that fails partway still prints its dseq — that is the number ` +
	"`akash close`" + ` needs. Run ` + "`pzctl akash leases`" + ` after any crash.

Secrets are never read from the config file. See ` + "`pzctl config secrets`" + `.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pzctl: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "config":
		return cmdConfig(args[1:])
	case "sdl":
		return cmdSDL(args[1:])
	case "state":
		return cmdState(args[1:])
	case "controller":
		return cmdController(args[1:])
	case "agent":
		return cmdAgent(args[1:])
	case "akash":
		return cmdAkash(args[1:])
	case "dns":
		return cmdDNS(args[1:])
	case "version", "--version", "-v":
		fmt.Println("pzctl " + version)
		return nil
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q (run `pzctl help`)", args[0])
	}
}

// --- config ---

func cmdConfig(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("config: want one of validate, dump, secrets")
	}
	sub, rest := args[0], args[1:]

	if sub == "secrets" {
		fmt.Println("Secrets are read from the environment, never from config.yaml:")
		for _, n := range secrets.EnvNames() {
			fmt.Println("  " + n)
		}
		return nil
	}

	fs := flag.NewFlagSet("config "+sub, flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	if err := fs.Parse(rest); err != nil {
		return err
	}
	resolved, err := config.Find(*path)
	if err != nil {
		return err
	}

	switch sub {
	case "validate":
		if _, err := config.Load(resolved); err != nil {
			return err
		}
		fmt.Printf("%s: ok\n", resolved)
		return nil
	case "dump":
		c, err := config.Load(resolved)
		if err != nil {
			return err
		}
		out, err := c.Marshal()
		if err != nil {
			return err
		}
		os.Stdout.Write(out)
		return nil
	default:
		return fmt.Errorf("config: unknown subcommand %q (want validate, dump or secrets)", sub)
	}
}

// --- sdl ---

func cmdSDL(args []string) error {
	if len(args) == 0 || args[0] != "render" {
		return fmt.Errorf("sdl: want `sdl render <controller|server>`")
	}
	args = args[1:]
	if len(args) == 0 {
		return fmt.Errorf("sdl render: want a role, controller or server")
	}
	role, rest := args[0], args[1:]
	if role != "controller" && role != "server" {
		return fmt.Errorf("sdl render: unknown role %q (want controller or server)", role)
	}

	fs := flag.NewFlagSet("sdl render "+role, flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	withSecrets := fs.Bool("with-secrets", false,
		"emit real secret values from the environment instead of placeholders")
	aktUSD := fs.Float64("akt-usd", 0,
		"AKT/USD rate used to compute the server bid ceiling (default: akash.price.akt_usd_fallback)")
	controllerURL := fs.String("controller-url", "",
		"public controller URL to bake into the server SDL (default: agent discovers it from git)")
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

	in := sdl.Input{Cfg: cfg, ControllerURL: *controllerURL}

	if *withSecrets {
		want := secrets.RoleController
		if role == "server" {
			want = secrets.RoleAgent
		}
		set, err := secrets.Load(want, secrets.Requirements{
			RCON:         cfg.Server.RCON.Enabled,
			DNS:          cfg.DNS.Enabled,
			JoinPassword: cfg.Game.PasswordProtected,
		})
		if err != nil {
			return err
		}
		in.Secrets = set
		fmt.Fprintln(os.Stderr, "pzctl: WARNING — output contains real secret values; do not commit or paste it")
	}

	var out []byte
	switch role {
	case "controller":
		out, err = sdl.RenderController(in)
	case "server":
		in.MaxPricePerBlock, err = serverBidCeiling(cfg, *aktUSD)
		if err != nil {
			return err
		}
		out, err = sdl.RenderServer(in)
	}
	if err != nil {
		return err
	}
	os.Stdout.Write(out)
	return nil
}

// serverBidCeiling converts the USD/day ceiling into a per-block bid ceiling in
// the configured denomination. It deliberately refuses to invent an AKT/USD rate:
// an incorrect ceiling either overpays or wins no bids at all. For a
// dollar-pegged denomination no rate is needed, so no rate is asked for.
func serverBidCeiling(cfg *config.Config, flagRate float64) (int, error) {
	d := cfg.Akash.Price.Denom
	rate, source := 0.0, "not needed"
	if denom.NeedsOracle(d) {
		rate, source = flagRate, "--akt-usd"
		if rate <= 0 {
			rate = cfg.Akash.Price.AKTUSDFallback
			source = "akash.price.akt_usd_fallback"
		}
		if rate <= 0 {
			fmt.Fprintf(os.Stderr,
				"pzctl: no AKT/USD rate available for denom %s (pass --akt-usd, or set akash.price.akt_usd_fallback);\n"+
					"       rendering with the server.pricing_amount placeholder of %d instead\n",
				d, cfg.Server.PricingAmount)
			return 0, nil
		}
	}
	max, err := denom.CeilingPerBlock(cfg.Akash.Price.MaxUSDPerDay, d, cfg.Akash.BlocksPerDay, rate)
	if err != nil {
		return 0, err
	}
	deposit := sdl.InitialDepositUSD(
		cfg.Akash.Price.MaxUSDPerDay,
		cfg.Akash.InitialDepositDays,
		cfg.Akash.Funds.Margin,
		cfg.Akash.Funds.MinTopupUSD,
	)
	fmt.Fprintf(os.Stderr,
		"pzctl: AKT/USD=%g (%s) · ceiling=%d %s/block (%g USD/day over %d blocks/day) · initial deposit=%.2f USD\n",
		rate, source, max, d, cfg.Akash.Price.MaxUSDPerDay, cfg.Akash.BlocksPerDay, deposit)
	return max, nil
}
