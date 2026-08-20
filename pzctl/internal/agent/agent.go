// Package agent runs inside the game container. It owns the PZ process, the
// container's filesystem, and exactly one document on the agent state branch.
//
// Three properties define it, and each one is a bug from v1 turned into a rule.
//
// It never exits (invariant I8). Akash runs the container under Kubernetes with
// restartPolicy: Always, so a process that ends is restarted — v1's entrypoint
// finished with `exit $EXIT_CODE`, and the pod coming back mid-halt is what made
// the controller see the server go offline, then online, then offline, spawning a
// duplicate backup and a webhook storm on each flap (bug 2). When there is
// nothing left to do the agent parks: no PZ process, but a live loop that keeps
// publishing liveness, so "parked" and "wedged" stay distinguishable.
//
// It never decides to run. Intent lives in the controller's document; the agent
// reads it. That is why a container restart cannot resurrect a world that was
// being halted: the first thing a fresh agent does is read intent, and if it is
// stopped the agent parks without launching anything.
//
// It never writes a player count it did not measure. state.PlayersUnknown is the
// value for "not measured" (bug 1).
package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/gitbus"
	"github.com/hrkcz001/pz-akash/pzctl/internal/secrets"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Options is everything the agent needs from its caller.
type Options struct {
	Config  *config.Config
	Secrets *secrets.Set
	Bus     *gitbus.AgentBus

	// ControllerURL overrides discovery. Empty is the normal case: the agent
	// reads the controller's URLs from the state branch, which is what removed
	// v1's unresolvable CONTROLLER_URL=http://controller:8000.
	ControllerURL string

	Logf func(string, ...any)
}

// Agent is the supervisor. Every field is owned by the Run goroutine; the only
// concurrency is the PZ output scanner and a running backup, both of which
// communicate over channels.
type Agent struct {
	cfg *config.Config
	sec *secrets.Set
	bus *gitbus.AgentBus
	cli *Client
	loc *time.Location
	log func(string, ...any)

	doc *state.Agent

	// procCtx is the PZ process's lifetime, deliberately not derived from Run's
	// context: a SIGTERM must let the JVM save and exit, and a context-killed
	// child dies immediately.
	procCtx    context.Context
	procCancel context.CancelFunc
	pz         *pzProcess
	launcher   string

	intent        state.Intent
	restoreTarget string
	controllerURL string

	restarts int
	parked   bool
	parkWhy  string

	// answered remembers backup request IDs this process has already reported
	// on, so a request that stays in the controller's document until it reads our
	// answer does not start a second archive.
	answered map[string]bool
	backup   *backupJob

	// unanswered counts player polls sent since the last recognised answer. A
	// count, not a wall clock: see pollPlayers.
	unanswered int

	events  chan event
	pending []string
	lastPush,
	lastPlayersPush,
	lastPlayersSeen time.Time
}

// New builds an agent. It performs no I/O.
func New(o Options) (*Agent, error) {
	if o.Config == nil {
		return nil, errors.New("agent: Config is required")
	}
	if o.Bus == nil {
		return nil, errors.New("agent: Bus is required")
	}
	logf := o.Logf
	if logf == nil {
		logf = func(string, ...any) {}
	}
	loc := o.Config.Location()
	a := &Agent{
		cfg:           o.Config,
		sec:           o.Secrets,
		bus:           o.Bus,
		loc:           loc,
		log:           logf,
		doc:           state.NewAgent(loc),
		controllerURL: o.ControllerURL,
		answered:      map[string]bool{},
		events:        make(chan event, 256),
	}
	a.procCtx, a.procCancel = context.WithCancel(context.Background())
	return a, nil
}

// now is the agent's only clock reading, in the configured timezone.
func (a *Agent) now() state.Stamp { return state.Now(a.loc) }

// Run boots the server and then supervises it forever. It returns only when ctx
// is cancelled — a returned nil means "the container is being torn down", never
// "there is nothing more to do".
func (a *Agent) Run(ctx context.Context) error {
	if err := a.resume(ctx); err != nil {
		return err
	}

	switch {
	case a.intent == state.IntentStopped:
		// The container came back on its own: Kubernetes restarted it, or the pod
		// moved. Bringing the world up here is exactly bug 2, so it parks instead
		// and lets the controller finish whatever it was doing.
		a.log("boot: controller intent is %q — parking without launching", a.intent)
		a.setPhase(state.PhaseStopped, "intent is stopped")
		a.park("intent was stopped at boot")
	default:
		if err := a.boot(ctx); err != nil {
			a.fail(err)
			// A failed restore is reported as its own phase: the operator needs to
			// know that the world on disk is not the world they asked for, and the
			// controller must not treat it as a crash to be retried.
			if errors.Is(err, errRestore) {
				a.setPhase(state.PhaseRestoreFailed, "restore failed")
			} else {
				a.setPhase(state.PhaseCrashed, "boot failed")
			}
			a.park(err.Error())
		}
	}
	a.publish(ctx, true)
	return a.loop(ctx)
}

// resume reads both documents before touching anything: the controller's for
// intent, and its own last publication for the restart counter.
//
// Re-reading our own document is what makes server.crash.max_restarts
// enforceable. A counter held only in memory is reset by the very event it is
// supposed to bound.
func (a *Agent) resume(ctx context.Context) error {
	if err := a.bus.Fetch(ctx); err != nil {
		return fmt.Errorf("fetch state repository: %w", err)
	}

	if own, repairs, err := a.bus.ReadOwn(); err != nil {
		a.log("boot: cannot read our previous document (%v); starting fresh", err)
	} else {
		if !repairs.OK() {
			a.log("boot: repaired our previous document: %s", repairs)
		}
		a.restarts = own.Restarts
		// The backup report is carried over so a request answered just before a
		// restart is not answered twice.
		if own.Backup != nil {
			a.doc.Backup = own.Backup
			a.answered[own.Backup.RequestID] = own.Backup.State != state.BackupRunning
		}
	}

	ctrl, _, repairs, err := a.bus.ReadController()
	if err != nil {
		return fmt.Errorf("read controller document: %w", err)
	}
	if !repairs.OK() {
		a.log("boot: repaired the controller document on read: %s", repairs)
	}
	a.applyController(ctrl)

	a.log("boot: intent=%s restore_target=%q controller=%s restarts=%d",
		a.intent, a.restoreTarget, a.controllerURL, a.restarts)
	return nil
}

// applyController copies the fields the agent is allowed to act on. It is the
// only place the controller's document is read into agent state, so "what the
// agent obeys" is one short list rather than a scatter of field accesses.
func (a *Agent) applyController(c *state.Controller) {
	a.intent = c.Intent
	a.restoreTarget = c.RestoreTarget
	if a.controllerURL == "" && c.URLs.Base() != "" {
		a.controllerURL = c.URLs.Base()
	}
}

// client returns the controller HTTP client, building it once the URL is known.
func (a *Agent) client() (*Client, error) {
	if a.cli != nil {
		return a.cli, nil
	}
	if a.controllerURL == "" {
		return nil, errors.New("no controller URL: PZ_CONTROLLER_URL is unset and the controller has not published its URLs yet")
	}
	a.cli = NewClient(a.controllerURL, a.sec, a.cfg.Agent, a.log)
	return a.cli, nil
}

// --- document ownership ---

// setPhase records a phase change and queues a publish. Every phase change in
// the agent goes through here, which is what keeps the reason in the commit
// message next to the transition that caused it.
func (a *Agent) setPhase(p state.Phase, why string) {
	if a.doc.Phase == p {
		return
	}
	a.log("phase: %s -> %s (%s)", a.doc.Phase, p, why)
	a.doc.SetPhase(p, a.now())
	a.mark(string(p) + ": " + why)
}

// fail records an error on the document without changing phase.
func (a *Agent) fail(err error) {
	a.log("error: %v", err)
	a.doc.LastError = err.Error()
	a.mark("error")
}

// park stops the agent from ever launching PZ again in this process.
//
// It is not a `select{}`: the loop keeps running so liveness keeps being
// published and a backup request can still be answered from the world on disk.
// What it gives up is relaunching — a parked agent has decided that whatever
// comes next needs a fresh container, which is the only way to be certain a halt
// is not undone by the process that was being halted.
func (a *Agent) park(why string) {
	if a.parked {
		return
	}
	a.parked = true
	a.parkWhy = why
	a.log("parked: %s (this process will not launch PZ again)", why)
}

// mark queues a publish reason.
func (a *Agent) mark(reason string) { a.pending = append(a.pending, reason) }

// publish writes the document if anything is pending. force skips the
// min_push_interval floor, which phase changes and liveness use — those are rare
// and the controller may be blocking on them.
func (a *Agent) publish(ctx context.Context, force bool) {
	if len(a.pending) == 0 {
		return
	}
	if !force && time.Since(a.lastPush) < a.cfg.Git.MinPushInterval.D() {
		return
	}
	a.doc.Restarts = a.restarts
	// A count belongs to a running process, and this is the one choke point every
	// document passes through. Without it the reconcile ticker and the exit event
	// race: whichever moves the phase to stopped first wins, and if it is the
	// ticker the controller reads "stopped, 3 players online" — a contradiction the
	// dashboard would render verbatim.
	if a.pz == nil || !a.pz.Running() {
		a.doc.SetPlayers(state.PlayersUnknown, a.now())
	}
	reason := a.pending[len(a.pending)-1]
	if n := len(a.pending); n > 1 {
		reason = fmt.Sprintf("%s (+%d more)", reason, n-1)
	}
	sha, err := a.bus.Publish(ctx, a.doc, reason)
	if err != nil {
		// Not fatal, and deliberately not retried here: the pending list survives,
		// so the next tick tries again, and the controller sees a stale liveness
		// stamp in the meantime. Failing the agent over a push would take the game
		// server down for a git outage.
		a.log("publish failed (%d pending): %v", len(a.pending), err)
		return
	}
	a.pending = nil
	a.lastPush = time.Now()
	if sha != "" {
		a.log("published %s: %s", sha[:min(8, len(sha))], reason)
	}
}

// logPath is where PZ's console is mirrored, ensured to exist so the scanner does
// not race an operator's `tail -f`.
func (a *Agent) logPath() string {
	p := a.cfg.Agent.Paths.LogFile
	if p == "" {
		return ""
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		a.log("cannot create the log directory for %s: %v", p, err)
		return ""
	}
	return p
}
