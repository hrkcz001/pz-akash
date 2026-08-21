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
	"github.com/hrkcz001/pz-akash/pzctl/internal/httpapi"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
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
// With --dry-run it walks the whole lifecycle against a real repository and real
// trigger files while creating nothing and billing nothing; without it, the live
// Akash driver is wired in and every deploy costs money.
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
	backupsDir := fs.String("backups-dir", "",
		"override backups.dir, which is a container path; use a scratch directory on a laptop")
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

	// The stub is chosen explicitly, never as a fallback: a controller that
	// reported "deployed" without deploying would be worse than one that refuses
	// to start.
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
		if driver, err = newAkashDriver(cfg, logf); err != nil {
			return err
		}
		logf("controller: LIVE — deployments will be created against %s and will cost money",
			cfg.Akash.APIBase)
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

	// The machine does not exist yet, so the store's change hook reaches it through
	// this variable. Nothing can call the hook before the assignment below: the only
	// callers are the HTTP handlers and the prune, and the first request cannot
	// arrive until the server starts, which is further down still.
	var m *fsm.Machine
	store, err := openStore(cfg, *backupsDir, logf, func() {
		// A nudge rather than the index itself. The machine reads the store when it
		// handles the event, so a burst of uploads costs one publish and not one per
		// notification — and the machine stays the only thing that writes the branch.
		if m != nil {
			m.Send(fsm.Tick("backups"))
		}
	})
	switch {
	case err != nil && !*dryRun:
		// Live, this is fatal. A controller that cannot open backups.dir cannot serve
		// server.zip and cannot accept the halt backup, and one that starts anyway is
		// v1's failure mode: a healthy-looking controller serving nothing.
		return err
	case err != nil:
		logf("controller: no storage layer (%v) — no HTTP service, and the index will "+
			"come from agent reports only", err)
	}

	// A nil *Store must not be handed over as a non-nil interface: the machine tests
	// `store == nil` to decide whether it owns the index.
	var backups fsm.BackupStore
	if store != nil {
		backups = store
	}

	m, err = fsm.New(fsm.Deps{
		Cfg: cfg, Bus: bus, Akash: driver,
		Backups:       backups,
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

	// Serving the webhook is optional and non-fatal by design: it is a latency
	// optimisation, and a controller that refused to run without it would trade a
	// slower reaction for no reaction at all. The file service is not optional, and
	// is started below.
	hook, err := webhookHandler(cfg, m, *dryRun, logf)
	if err != nil {
		return err
	}

	// One port or two. controller.webhook_port: 0 folds the webhook onto http_port,
	// so Akash exposes one endpoint instead of two — the SDL follows the same
	// setting, so the two cannot disagree. Folding can only be honoured when there
	// is a file service to fold into; config.yaml currently names 8080, which is two.
	var extra http.Handler
	if hook != nil && cfg.WebhookOnHTTPPort() && store != nil {
		extra = foldedRoutes(hook, m)
		hook = nil
		logf("controller: webhook folded onto http_port %d at %s",
			cfg.Controller.HTTPPort, webhook.Path)
	}
	if hook != nil {
		srv, err := webhookServer(cfg, hook, m, *httpAddr, *dryRun, logf)
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
	}

	httpErr := make(chan error, 1)
	if store != nil {
		files, err := httpapi.NewServer(httpapi.ServerOptions{
			Store:   store,
			Cfg:     cfg,
			Secrets: secrets.LoadOptional(),
			Logf:    logf,
			Extra:   extra,
		})
		if err != nil {
			return err
		}
		addr := net.JoinHostPort("", fmt.Sprint(cfg.Controller.HTTPPort))
		go func() {
			err := files.ListenAndServe(ctx, addr)
			httpErr <- err
			if err != nil {
				// Not survivable. This is how the agent fetches server.zip and how a
				// halt backup gets uploaded; without it the lease is running blind, so
				// the whole process comes down and the supervisor restarts it.
				logf("controller: HTTP service stopped: %v — shutting down", err)
				stop()
			}
		}()
	}

	logf("controller: running against %s (branch %s)", cfg.Git.RepoURL, cfg.Git.Branch)
	if err := m.Run(ctx); err != nil {
		return err
	}
	// The HTTP failure, if that is what ended the run. A clean shutdown puts nil
	// here or nothing at all, depending on which side finished draining first.
	select {
	case err := <-httpErr:
		return err
	default:
		return nil
	}
}

// openStore opens the storage layer that owns backups.dir.
//
// It is the single generator of backups.json — see httpapi.Store — and onChange is
// how the controller learns that the directory moved under it.
func openStore(cfg *config.Config, dirOverride string, logf func(string, ...any),
	onChange func()) (*httpapi.Store, error) {

	dir := cfg.Backups.Dir
	if dirOverride != "" {
		dir = dirOverride
	}
	return httpapi.NewStore(httpapi.StoreOptions{
		Dir: dir,
		// identity.timezone, never the host's: a backup filename is Prague
		// wall-clock wherever the container happened to run.
		Loc:       cfg.Location(),
		MaxUpload: cfg.Backups.UploadMaxBytes,
		MinFree:   cfg.Controller.Storage.MinFreeBytes,
		Logf:      logf,
		OnChange:  func(*state.Backups) { onChange() },
	})
}

// foldedRoutes is what the file service mounts under everything it does not claim,
// when the webhook shares its port.
//
// /healthz is deliberately absent: httpapi owns that path and answers it with
// liveness, which is what the Akash probe wants. The machine's status line moves to
// /state, where step 7's dashboard will replace it.
func foldedRoutes(hook http.Handler, m *fsm.Machine) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(webhook.Path, hook)
	mux.HandleFunc("/state", func(w http.ResponseWriter, _ *http.Request) {
		s := m.State()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		fmt.Fprintf(w, "status=%s intent=%s phase=%s\n", s.Status, s.Intent, s.Phase)
	})
	return mux
}

// webhookHandler builds the receiver, or returns nil when it is deliberately not
// running.
func webhookHandler(cfg *config.Config, m *fsm.Machine, dry bool,
	logf func(string, ...any)) (http.Handler, error) {

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

	bl := cfg.Git.BranchLayout()
	return webhook.New(webhook.Options{
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
}

// webhookServer wraps the handler in a server of its own, for the case where
// controller.webhook_port names a second port.
func webhookServer(cfg *config.Config, hook http.Handler, m *fsm.Machine, addr string,
	dry bool, logf func(string, ...any)) (*http.Server, error) {

	if addr == "" && dry {
		logf("controller: no --webhook-addr — webhook disabled; triggers will be picked up by polling")
		return nil, nil
	}
	if addr == "" {
		addr = net.JoinHostPort("", fmt.Sprint(cfg.EffectiveWebhookPort()))
	}

	mux := http.NewServeMux()
	mux.Handle(webhook.Path, hook)
	// This server's own liveness path. On the folded arrangement httpapi answers
	// /healthz instead, and the status line lives at /state.
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
