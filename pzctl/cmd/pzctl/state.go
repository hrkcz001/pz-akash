package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// rootContext cancels on interrupt so a hung fetch answers Ctrl-C instead of
// waiting out its timeout.
func rootContext(timeout time.Duration) (context.Context, func()) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if timeout <= 0 {
		return ctx, stop
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	return ctx, func() { cancel(); stop() }
}

// cmdState implements `pzctl state show`, the read-only inspector.
//
// It is the first command that talks to the live repository, and it is
// deliberately the only kind that does so without writing: every diagnosis of a
// stuck system starts by asking what the two sides currently believe, and in v1
// that question could only be answered by reading raw JSON out of a git
// checkout and interpreting it by hand.
func cmdState(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("state: want `state show`")
	}
	sub, rest := args[0], args[1:]
	if sub != "show" {
		return fmt.Errorf("state: unknown subcommand %q (want show)", sub)
	}

	fs := flag.NewFlagSet("state show", flag.ContinueOnError)
	path := fs.String("c", "", "path to config.yaml")
	dir := fs.String("dir", "",
		"read a v1 checkout on disk instead of git (e.g. the pz-saves working copy)")
	asJSON := fs.Bool("json", false, "emit the normalized documents as JSON")
	doFetch := fs.Bool("fetch", true, "fetch the remote before reading (git mode)")
	legacy := fs.Bool("legacy", false,
		"force the v1 importer in git mode instead of reading the state branches")
	timeout := fs.Duration("timeout", time.Minute, "give up on the fetch after this long")
	// Both of these exist because config.yaml is written for the container: its
	// cache_dir is an absolute container path and its repo_url is an SSH remote
	// needing the deploy key. An operator diagnosing from a laptop overrides them
	// rather than editing the committed config.
	cacheDir := fs.String("cache-dir", "", "override git.cache_dir (default: a temp mirror)")
	repoURL := fs.String("repo", "", "override git.repo_url, e.g. a local clone's .git")
	if err := fs.Parse(rest); err != nil {
		return err
	}

	if *dir != "" {
		// No config needed: a directory of v1 files is self-describing, and the
		// only thing config would contribute is the timezone.
		loc := time.UTC
		if resolved, err := config.Find(*path); err == nil {
			if cfg, err := config.Load(resolved); err == nil {
				loc = cfg.Location()
			}
		}
		doc, idx, rep := state.ImportLegacy(state.DirFetcher(*dir), loc)
		return report(reportInput{
			Source:     "v1 checkout " + *dir,
			Legacy:     true,
			Controller: doc, Backups: idx, Repairs: rep,
			Loc: loc, JSON: *asJSON,
		})
	}

	resolved, err := config.Find(*path)
	if err != nil {
		return err
	}
	cfg, err := config.Load(resolved)
	if err != nil {
		return err
	}
	return showFromGit(cfg, gitOpts{
		fetch: *doFetch, legacy: *legacy, asJSON: *asJSON,
		timeout: *timeout, cacheDir: *cacheDir, repoURL: *repoURL,
	})
}

type gitOpts struct {
	fetch    bool
	legacy   bool
	asJSON   bool
	timeout  time.Duration
	cacheDir string
	repoURL  string
}

func showFromGit(cfg *config.Config, o gitOpts) error {
	loc := cfg.Location()
	bl := cfg.Git.BranchLayout()

	repoURL := cfg.Git.RepoURL
	if o.repoURL != "" {
		repoURL = o.repoURL
	}
	// requireKey is false: a public remote, or a path remote in a test, needs no
	// credential, and requiring a write key to perform a read would make this
	// command unavailable in exactly the situation where git auth is the thing
	// you are trying to diagnose.
	repo, bus, cleanup, err := openBus(cfg, busOptions{
		repoURL: o.repoURL, cacheDir: o.cacheDir,
		logf: func(f string, a ...any) { fmt.Fprintf(os.Stderr, "pzctl: "+f+"\n", a...) },
	})
	if err != nil {
		return err
	}
	defer cleanup()

	if o.fetch {
		ctx, cancel := rootContext(o.timeout)
		defer cancel()
		if err := bus.Fetch(ctx); err != nil {
			return fmt.Errorf("fetch %s: %w", repoURL, err)
		}
	}

	in := reportInput{Source: repoURL, Loc: loc, JSON: o.asJSON}
	if head, err := bus.Head(); err == nil {
		in.Head = head
	}

	trigs, err := bus.Triggers()
	if err != nil {
		return err
	}
	in.Triggers = trigs

	doc, idx, rep, err := bus.ReadOwn()
	if err != nil {
		return err
	}

	// A repository that has never run v2 has no state branch, so the documents
	// read as pristine defaults. That is indistinguishable from a stopped v2
	// system, and on the live repo it is the wrong answer — the v1 files are
	// right there on the operator branch. Fall back to the importer and say so.
	if o.legacy || (bl.Controller != bl.Main && !published(repo, bl.Controller)) {
		doc, idx, rep = state.ImportLegacy(repo.Fetcher(bl.Main), loc)
		in.Legacy = true
		in.Source = repoURL + " (branch " + bl.Main + ")"
	}
	in.Controller, in.Backups, in.Repairs = doc, idx, rep

	agent, arep, err := bus.ReadAgent()
	if err != nil {
		return err
	}
	if !in.Legacy {
		// v1 had no agent document at all, so reporting one after an import would
		// be reporting a default as a reading.
		in.Agent, in.AgentRepairs = agent, arep
	}
	return report(in)
}

// localDir reports whether path exists on this machine, which is how a container
// path in config is told apart from an operator's own mirror.
func localDir(path string) bool {
	if path == "" {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}

func published(repo *gitbus.Repo, branch string) bool {
	_, err := repo.Head(branch)
	return err == nil
}

// reportInput is everything `state show` prints, gathered before any of it is
// formatted so the JSON and human forms cannot drift.
type reportInput struct {
	Source string
	Head   string
	Legacy bool
	Loc    *time.Location
	JSON   bool

	Controller *state.Controller
	Backups    *state.Backups
	Repairs    *state.Repairs

	Agent        *state.Agent
	AgentRepairs *state.Repairs

	Triggers []gitbus.Trigger
}

func report(in reportInput) error {
	if in.JSON {
		out := map[string]any{"source": in.Source, "legacy_import": in.Legacy}
		if in.Head != "" {
			out["head"] = in.Head
		}
		out["controller"] = in.Controller
		out["backups"] = in.Backups
		if in.Agent != nil {
			out["agent"] = in.Agent
		}
		if names := triggerNames(in.Triggers); len(names) > 0 {
			out["triggers"] = names
		}
		out["repairs"] = append(repairStrings(in.Repairs), repairStrings(in.AgentRepairs)...)
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	printReport(os.Stdout, in)
	return nil
}

func triggerNames(ts []gitbus.Trigger) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

func repairStrings(r *state.Repairs) []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.Items))
	for _, it := range r.Items {
		out = append(out, it.String())
	}
	return out
}
