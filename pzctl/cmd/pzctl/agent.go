package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/agent"
	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
)

// cmdAgent implements `pzctl agent`, the process that replaces v1's
// entrypoint.sh as PID 1 of the game container.
//
// It is the same binary as the controller on purpose. In v1 the two sides were a
// bash script and a Python program that had to agree on a JSON schema, a URL
// layout and a set of environment variables by convention alone, and every one of
// the four bugs lived in that gap. Here they share internal/state, internal/config
// and internal/httpapi, so a mismatch is a compile error.
func cmdAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	controllerURL := fs.String("controller-url", "",
		"controller base URL (default: $PZ_CONTROLLER_URL, else discovered from the controller's state branch)")
	repoURL := fs.String("repo", "", "override git.repo_url, e.g. a local clone's .git")
	cacheDir := fs.String("cache-dir", "", "override agent.paths.repo_cache")
	timeout := fs.Duration("timeout", 0, "exit after this long; 0 runs until interrupted")
	if err := fs.Parse(args); err != nil {
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

	logf := func(f string, a ...any) {
		fmt.Fprintf(os.Stderr, time.Now().In(cfg.Location()).Format("15:04:05")+" "+f+"\n", a...)
	}

	// The agent needs three: the deploy key to publish its phase, and the two
	// download tokens. Notably not the admin password — it is substituted into the
	// .ini by the controller, so it never reaches this container.
	set, err := secrets.Load(secrets.RoleAgent, secrets.Requirements{
		RCON:         cfg.Server.RCON.Enabled,
		DNS:          cfg.DNS.Enabled,
		JoinPassword: cfg.Game.PasswordProtected,
	})
	if err != nil {
		return err
	}

	url := *controllerURL
	if url == "" {
		url = os.Getenv("PZ_CONTROLLER_URL")
	}

	bus, cleanup, err := openAgentBus(cfg, *repoURL, *cacheDir, set, logf)
	if err != nil {
		return err
	}
	defer cleanup()

	a, err := agent.New(agent.Options{
		Config:        cfg,
		Secrets:       set,
		Bus:           bus,
		ControllerURL: url,
		Logf:          logf,
	})
	if err != nil {
		return err
	}

	ctx, stop := rootContext(*timeout)
	defer stop()

	logf("agent: %s, config %s, timezone %s", version, resolved, cfg.Identity.Timezone)
	return a.Run(ctx)
}

// openAgentBus opens the agent's own view of pz-saves.
//
// It mirrors into agent.paths.repo_cache rather than git.cache_dir: that path
// belongs to the controller's container, and in a local run both processes would
// otherwise fetch and force-push through one bare repository.
func openAgentBus(cfg *config.Config, repoOverride, cacheOverride string,
	set *secrets.Set, logf func(string, ...any)) (*gitbus.AgentBus, func(), error) {

	cleanup := func() {}

	repoURL := cfg.Git.RepoURL
	if repoOverride != "" {
		repoURL = repoOverride
	}
	cache := cfg.Agent.Paths.RepoCache
	switch {
	case cacheOverride != "":
		cache = cacheOverride
	case !localDir(cache):
		// Running outside the container: /home/steam does not exist, and creating it
		// on an operator's machine would be worse than using a temporary directory.
		tmp, err := os.MkdirTemp("", "pzctl-agent-mirror-")
		if err != nil {
			return nil, cleanup, err
		}
		cleanup = func() { os.RemoveAll(tmp) }
		cache = tmp
	}

	key, err := set.DeployKeyPEM()
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if len(key) == 0 && needsDeployKey(repoURL) {
		// Fatal at startup rather than at the first phase change: an agent that
		// cannot publish is invisible to the controller, which will eventually
		// conclude the container is wedged and close the lease.
		cleanup()
		return nil, func() {}, fmt.Errorf("agent: %s is required to publish state to %s", secrets.DeployKeyEnv, repoURL)
	}

	repo, err := gitbus.Open(gitbus.Options{
		RepoURL:             repoURL,
		CacheDir:            cache,
		UserName:            cfg.Git.UserName,
		UserEmail:           cfg.Git.UserEmail,
		KnownHosts:          cfg.Git.KnownHosts,
		AllowUnverifiedHost: cfg.Git.AllowUnverifiedHost,
		DeployKeyPEM:        key,
		Location:            cfg.Location(),
		NetTimeout:          cfg.Git.NetTimeout.D(),
		Logf:                logf,
	})
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}

	bl := cfg.Git.BranchLayout()
	bus, err := gitbus.NewAgentBus(repo, gitbus.Branches{
		Main:        bl.Main,
		Controller:  bl.Controller,
		Agent:       bl.Agent,
		TriggersDir: bl.TriggersDir,
	})
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	if bus == nil {
		cleanup()
		return nil, func() {}, errors.New("agent: git bus could not be opened")
	}
	return bus, cleanup, nil
}
