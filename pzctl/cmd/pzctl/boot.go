package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/hrkcz001/pz-akash/pzctl/internal/bootstrap"
	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
)

// bootConfig resolves the config file for a long-running role — `pzctl
// controller` and `pzctl agent`, the two commands that run as PID 1 of a
// container.
//
// The container images carry a binary and no config: config.yaml lives on the
// state repository's main branch, and the four PZ_* variables the SDL renders
// name where. So there are two authorities for the file, and which one applies is
// decided by the environment rather than by luck:
//
//  1. -c FILE, or $PZ_CONFIG. Someone named a file; that is the file, and nothing
//     is fetched over the top of it.
//  2. No $PZ_REPO_URL. A workstation. config.Find, exactly as every other
//     subcommand resolves it.
//  3. $PZ_REPO_URL set. A container. Fetch from the repository, because the point
//     of shipping config through git is that a push plus a restart is a config
//     change — a file left over from the last boot would pin the container to the
//     configuration it first started with.
//
// Case 3 falls back to whatever is on disk when the fetch fails, loudly. A
// controller that cannot reach GitHub for ninety seconds should come up with the
// configuration it had rather than refuse to start: it can still serve the
// dashboard and its next tick will fetch again, whereas a container that exits is
// a lease burning money with nothing behind the domain.
func bootConfig(explicit string) (string, error) {
	named := explicit != "" || strings.TrimSpace(os.Getenv("PZ_CONFIG")) != ""
	if named || !bootstrap.Configured() {
		return config.Find(explicit)
	}

	ctx, stop := rootContext(0)
	defer stop()

	o := bootstrap.FromEnv()
	o.Logf = bootLogf
	out, err := bootstrap.Fetch(ctx, o)
	if err == nil {
		return out, nil
	}

	fallback, findErr := config.Find("")
	if findErr != nil {
		// Report the fetch error, not "no config file found": the second is a
		// symptom of the first, and only the first says what to fix.
		return "", err
	}
	bootLogf("WARNING: %v", err)
	bootLogf("WARNING: continuing with %s from a previous boot", fallback)
	return fallback, nil
}

// bootLogf logs the fetch. It carries no timestamp on purpose: every other line
// this program writes is stamped in identity.timezone, and this runs before the
// config that names the timezone has been read. A host-clock timestamp sitting
// among Prague ones would be worse than none.
func bootLogf(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "pzctl: "+f+"\n", a...)
}
