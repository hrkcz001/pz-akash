package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/fsm"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/webhook"
)

// busOptions are the overrides an operator applies to the committed git settings,
// which are written for the container rather than for a laptop: cache_dir is an
// absolute container path and repo_url is an SSH remote needing the deploy key.
type busOptions struct {
	repoURL  string
	cacheDir string
	// requireKey makes a missing deploy key fatal, for a remote that can
	// actually use one. Read-only commands leave it false — requiring a write
	// credential to perform a read would make the tool unavailable in exactly
	// the situation where git auth is what you are trying to diagnose.
	requireKey bool
	logf       func(string, ...any)
}

// needsDeployKey reports whether repoURL is a remote an SSH key applies to.
//
// A path or file:// remote is served by the local git binary over a pipe, with no
// authentication anywhere in the transport, so demanding a key for one would fail
// a run that could not have used it. That is not hypothetical: it is how
// --dry-run --repo is exercised against a throwaway clone, which is the only way to
// walk the lifecycle without touching the live repository.
func needsDeployKey(repoURL string) bool {
	switch {
	case repoURL == "":
		return false
	case strings.HasPrefix(repoURL, "file://"):
		return false
	case strings.HasPrefix(repoURL, "http://"), strings.HasPrefix(repoURL, "https://"):
		// A token in the URL, or a public read. Either way not our key.
		return false
	case strings.HasPrefix(repoURL, "ssh://"), strings.Contains(repoURL, "@"):
		return true
	default:
		// A bare path, absolute or relative, on either platform.
		return false
	}
}

// openBus resolves the git settings and opens the controller's view of the
// repository. The Repo is returned alongside the bus for the few callers that
// need to read a raw ref — the v1 importer being the only one. The returned func
// releases anything temporary and must be called.
func openBus(cfg *config.Config, o busOptions) (*gitbus.Repo, *gitbus.ControllerBus, func(), error) {
	cleanup := func() {}
	if o.logf == nil {
		o.logf = func(string, ...any) {}
	}

	repoURL, cacheDir := cfg.Git.RepoURL, cfg.Git.CacheDir
	if o.repoURL != "" {
		repoURL = o.repoURL
	}
	switch {
	case o.cacheDir != "":
		cacheDir = o.cacheDir
	case !localDir(cacheDir):
		// The configured mirror belongs to the container. Cloning into a throwaway
		// directory cannot corrupt a running controller's mirror, which is the one
		// thing a command run from a laptop must not do.
		tmp, err := os.MkdirTemp("", "pzctl-mirror-")
		if err != nil {
			return nil, nil, cleanup, err
		}
		cleanup = func() { os.RemoveAll(tmp) }
		cacheDir = tmp
	}

	set := secrets.LoadOptional()
	key, err := set.DeployKeyPEM()
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	if o.requireKey && len(key) == 0 && needsDeployKey(repoURL) {
		if key, err = set.RequireDeployKey(); err != nil {
			cleanup()
			return nil, nil, func() {}, err
		}
	}

	repo, err := gitbus.Open(gitbus.Options{
		RepoURL:             repoURL,
		CacheDir:            cacheDir,
		UserName:            cfg.Git.UserName,
		UserEmail:           cfg.Git.UserEmail,
		KnownHosts:          cfg.Git.KnownHosts,
		AllowUnverifiedHost: cfg.Git.AllowUnverifiedHost,
		DeployKeyPEM:        key,
		Location:            cfg.Location(),
		NetTimeout:          cfg.Git.NetTimeout.D(),
		Logf:                o.logf,
	})
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	bl := cfg.Git.BranchLayout()
	bus, err := gitbus.NewControllerBus(repo, gitbus.Branches{
		Main:        bl.Main,
		Controller:  bl.Controller,
		Agent:       bl.Agent,
		TriggersDir: bl.TriggersDir,
	})
	if err != nil {
		cleanup()
		return nil, nil, func() {}, err
	}
	return repo, bus, cleanup, nil
}

// cmdController implements `pzctl controller`.
//
// Step 3 ships it with the Akash driver stubbed, so --dry-run walks the whole
// lifecycle against a real repository and real trigger files while creating
// nothing and billing nothing. Running it without --dry-run is refused rather
// than quietly dry-run: a controller that reports "deployed" without deploying
// would be worse than one that does not start.
func cmdController(args []string) error {
	fs := flag.NewFlagSet("controller", flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	dryRun := fs.Bool("dry-run", false,
		"use the stub Akash driver: creates nothing, bills nothing")
	once := fs.Bool("once", false,
		"perform one reconcile pass, wait for any job it starts, then exit")
	dryDelay := fs.Duration("dry-delay", 0,
		"make each stubbed deploy/close take this long, to exercise cancellation")
	dryState := fs.String("dry-state", "",
		"file the stub driver keeps its simulated leases in, so --once can be chained")
	httpAddr := fs.String("webhook-addr", "",
		"listen address for the GitHub webhook (default: :controller.webhook_port; empty with --dry-run disables it)")
	controllerURL := fs.String("controller-url", "",
		"public base URL handed to the agent (default: the agent discovers it from git)")
	timeout := fs.Duration("timeout", 0, "exit after this long; 0 runs until interrupted")
	repoURL := fs.String("repo", "", "override git.repo_url, e.g. a local clone's .git")
	cacheDir := fs.String("cache-dir", "", "override git.cache_dir")
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

	// The stub is chosen explicitly, never as a fallback. Step 5 replaces this
	// branch with the real client.
	var driver fsm.Akash
	switch {
	case *dryRun:
		// --dry-state matters only for chained --once runs: without it each process
		// starts with a provider that has forgotten every lease, and the controller
		// correctly declares the lease it created last pass to be gone. A long-lived
		// --dry-run process needs no file, because its provider outlives its passes
		// the way a real one does.
		driver = &fsm.DryRun{Cfg: cfg, Logf: logf, Delay: *dryDelay, StateFile: *dryState}
		logf("controller: DRY RUN — no deployment will be created and no lease will be paid for")
	default:
		return errors.New("controller: the Akash driver arrives in step 5; run with --dry-run for now")
	}

	_, bus, cleanup, err := openBus(cfg, busOptions{
		repoURL: *repoURL, cacheDir: *cacheDir,
		// The controller writes: consuming a trigger and publishing state both
		// push. A missing key must fail now, at startup, rather than at the first
		// halt.
		requireKey: true,
		logf:       logf,
	})
	if err != nil {
		return err
	}
	defer cleanup()

	m, err := fsm.New(fsm.Deps{
		Cfg: cfg, Bus: bus, Akash: driver,
		ControllerURL: *controllerURL,
		Logf:          logf,
	})
	if err != nil {
		return err
	}

	ctx, stop := rootContext(*timeout)
	defer stop()

	if *once {
		return m.Once(ctx)
	}

	// Serving is optional and non-fatal by design: the webhook is a latency
	// optimisation, and a controller that refused to run without it would trade a
	// slower reaction for no reaction at all.
	srv, err := webhookServer(cfg, m, *httpAddr, *dryRun, logf)
	if err != nil {
		return err
	}
	if srv != nil {
		go func() {
			logf("controller: webhook listening on %s%s", srv.Addr, webhook.Path)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logf("controller: webhook server stopped: %v", err)
			}
		}()
		defer func() {
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
	}

	logf("controller: running against %s (branch %s)", cfg.Git.RepoURL, cfg.Git.Branch)
	return m.Run(ctx)
}

// webhookServer builds the receiver, or returns nil when it is deliberately not
// running.
func webhookServer(cfg *config.Config, m *fsm.Machine, addr string, dry bool,
	logf func(string, ...any)) (*http.Server, error) {

	set := secrets.LoadOptional()
	if set.WebhookSecret == "" {
		if !dry {
			// In a real deployment the secret is mandatory: without it we would be
			// serving an unauthenticated endpoint that can start a funded lease.
			return nil, fmt.Errorf("controller: set %s, or run with --dry-run", "PZ_WEBHOOK_SECRET")
		}
		logf("controller: no PZ_WEBHOOK_SECRET — webhook disabled; triggers will be picked up by polling every %s",
			cfg.Controller.Poll.Idle)
		return nil, nil
	}
	if addr == "" && dry {
		logf("controller: no --webhook-addr — webhook disabled; triggers will be picked up by polling")
		return nil, nil
	}
	if addr == "" {
		addr = net.JoinHostPort("", fmt.Sprint(cfg.EffectiveWebhookPort()))
	}

	bl := cfg.Git.BranchLayout()
	h, err := webhook.New(webhook.Options{
		Secret:      set.WebhookSecret,
		Branch:      bl.Main,
		TriggersDir: bl.TriggersDir,
		Logf:        logf,
		OnPush: func(p webhook.Push) {
			// Send never blocks. A dropped event costs latency only, because the
			// poll loop asks the same question on its own schedule.
			m.Send(fsm.Poll("webhook", p.After))
		},
	})
	if err != nil {
		return nil, err
	}

	mux := http.NewServeMux()
	mux.Handle(webhook.Path, h)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		s := m.State()
		fmt.Fprintf(w, "status=%s intent=%s phase=%s\n", s.Status, s.Intent, s.Phase)
	})
	return &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}, nil
}
