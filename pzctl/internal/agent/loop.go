package agent

import (
	"context"
	"fmt"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// loop is the agent's whole runtime after boot. It runs until ctx is cancelled.
//
// Everything that can block for minutes — stopping PZ, saving, zipping, uploading
// — happens in a goroutine that reports back over a channel, so the loop itself
// never stops reconciling. That matters for one specific reason: a halt is only
// seen on a reconcile, and an agent busy uploading a 2 GiB archive while the
// controller waits out halt_confirm is indistinguishable from an agent that died.
func (a *Agent) loop(ctx context.Context) error {
	reconcile := time.NewTicker(a.cfg.Agent.Reconcile.D())
	defer reconcile.Stop()
	liveness := time.NewTicker(a.cfg.Agent.LivenessPush.D())
	defer liveness.Stop()
	players := time.NewTicker(a.cfg.Agent.PZ.PlayersInterval.D())
	defer players.Stop()

	// relaunch is armed only while a crash backoff is pending. A timer rather
	// than a sleep, so a halt arriving during the backoff is still noticed.
	var relaunch <-chan time.Time

	for {
		var backupDone chan *state.BackupReport
		if a.backup != nil {
			backupDone = a.backup.done
		}

		select {
		case <-ctx.Done():
			// The container is being torn down (SIGTERM from the kubelet, or the
			// lease closing). Give PZ the same courtesy a halt gets — procCtx is
			// deliberately not this ctx, so the JVM is still alive to be asked.
			a.shutdown()
			return nil

		case ev := <-a.events:
			a.handleEvent(ctx, ev, &relaunch)

		case rep := <-backupDone:
			a.finishBackup(ctx, rep)

		case <-relaunch:
			relaunch = nil
			a.startGame(ctx)

		case <-reconcile.C:
			a.reconcile(ctx)

		case <-players.C:
			a.pollPlayers()

		case <-liveness.C:
			// Unconditional: an unchanged document still has to prove the agent is
			// alive, and this stamp is the only thing that distinguishes a quiet
			// server from a wedged one.
			a.mark("liveness")
			a.publish(ctx, true)
		}

		a.publish(ctx, false)
	}
}

// handleEvent processes one message from the PZ process.
func (a *Agent) handleEvent(ctx context.Context, ev event, relaunch *<-chan time.Time) {
	switch ev.kind {
	case evOnline:
		a.setPhase(state.PhaseOnline, "ready banner seen")
		// Ask immediately rather than waiting out players_interval: the first
		// count is the one the dashboard shows for the next half minute.
		a.pollPlayers()

	case evPlayers:
		a.observePlayers(ev.players)

	case evExit:
		a.handleExit(ctx, ev, relaunch)
	}
}

// observePlayers records a measured count. This is the only path that writes a
// player count, and it is only reached from a console line that actually carried
// one — which is the whole of the bug 1 fix. v1 had eleven write sites and most
// of them wrote a hardcoded 0.
func (a *Agent) observePlayers(n int) {
	if a.pz == nil || !a.pz.Running() {
		// A line scanned just before the process exited. Publishing it now would
		// attribute a live count to a world that is already down — the same class of
		// mistake as inventing a zero, in the other direction.
		return
	}
	a.lastPlayersSeen = time.Now()
	a.unanswered = 0

	// Re-stamped on every measurement, including one that did not change the
	// count. PlayersAt means "when was this last known to be true" and the
	// dashboard judges freshness from it, so returning early on an unchanged
	// count froze the stamp at the first measurement: an idle world measures
	// 0 == 0 forever, so the stamp aged without bound and the page called a count
	// taken thirty seconds ago stale. No value of dashboard.players_stale_after
	// could fix that from config — the stamp has to move. The write is free:
	// publish pushes only when something marked the document, and an unchanged
	// count marks nothing.
	changed := a.doc.PlayersCount != n
	a.doc.SetPlayers(n, a.now())
	if !changed {
		// The fresh stamp still reaches the controller, carried by the liveness
		// push that exists for exactly this — proving an unchanged document is
		// current. config.validateDashboard keeps players_stale_after above that
		// cadence, so a quiet world never reads as a broken one.
		return
	}
	// Rate-limited on purpose: with git as the bus, a busy evening of joins and
	// leaves would otherwise be a commit per event.
	if time.Since(a.lastPlayersPush) < a.cfg.Agent.PlayersPushMinInterval.D() {
		return
	}
	a.lastPlayersPush = time.Now()
	a.mark(fmt.Sprintf("players=%d", n))
}

// playerPollTolerance is how many polls may go unanswered before the count is
// reported as unknown.
const playerPollTolerance = 3

// pollPlayers asks the console for a count. The answer arrives asynchronously as
// an evPlayers event, or not at all.
func (a *Agent) pollPlayers() {
	if a.pz == nil || !a.pz.Running() || a.doc.Phase != state.PhaseOnline {
		return
	}
	if err := a.pz.Send(a.cfg.Agent.PZ.PlayersCommand); err != nil {
		a.log("players: cannot write to the console: %v", err)
		return
	}
	a.unanswered++

	// Degrading to "unknown" is the honest report when the console has stopped
	// answering — but only then. The measure is unanswered polls and an empty event
	// queue, deliberately not wall-clock time since the last answer: a loop busy
	// pushing to git is behind, not blind, and a wall clock makes the agent publish
	// its own latency as a player count. Doing that flapped the count between the
	// real value and unknown on every slow tick, which with git as the bus is a
	// commit apiece.
	if a.unanswered <= playerPollTolerance || len(a.events) > 0 || !a.doc.PlayersKnown() {
		return
	}
	since := "ever"
	if !a.lastPlayersSeen.IsZero() {
		since = time.Since(a.lastPlayersSeen).Truncate(time.Second).String()
	}
	a.log("players: %d polls unanswered (nothing recognised for %s) — reporting unknown", a.unanswered, since)
	a.doc.SetPlayers(state.PlayersUnknown, a.now())
	a.mark("players=unknown")
}

// handleExit decides what a dead PZ process means. Three answers: expected
// (we asked), intended (the controller wants it stopped), or a crash.
func (a *Agent) handleExit(ctx context.Context, ev event, relaunch *<-chan time.Time) {
	a.pz = nil
	a.unanswered = 0
	a.doc.SetPlayers(state.PlayersUnknown, a.now())

	switch {
	case a.doc.Phase == state.PhaseStopping:
		a.setPhase(state.PhaseStopped, "PZ exited after a requested stop")
		a.park("stopped on request")
		a.publish(ctx, true)

	case a.intent == state.IntentStopped:
		// Raced: the process died while a halt was already in flight. Parking is
		// the same answer as above, and specifically not a relaunch — that race is
		// what v1 lost, because its entrypoint never read the desired state.
		a.setPhase(state.PhaseStopped, "PZ exited and intent is stopped")
		a.park("intent is stopped")
		a.publish(ctx, true)

	default:
		a.restarts++
		why := "exit code 0"
		if ev.exitErr != nil {
			why = ev.exitErr.Error()
		}
		a.fail(fmt.Errorf("PZ exited unexpectedly (%s), restart %d of %d",
			why, a.restarts, a.cfg.Server.Crash.MaxRestarts))

		if a.restarts > a.cfg.Server.Crash.MaxRestarts {
			a.setPhase(state.PhaseCrashed, "restart budget exhausted")
			// Parked, not exited: the container staying up is what lets the
			// controller decide to close the lease rather than have the kubelet
			// restart us into another crash loop.
			a.park(fmt.Sprintf("PZ crashed %d times", a.restarts))
			a.publish(ctx, true)
			return
		}
		a.setPhase(state.PhaseStarting, "relaunching after a crash")
		a.publish(ctx, true)
		*relaunch = time.After(a.cfg.Server.Crash.Backoff.D())
		a.log("relaunching in %v", a.cfg.Server.Crash.Backoff.D())
	}
}

// reconcile re-reads the controller's document and acts on it.
//
// The order is deliberate: answer an outstanding backup request before acting on
// a stop. A halt is "back up, then stop", and the controller signals both at
// once — it sets intent to stopped and files a backup request. Acting on the stop
// first would upload nothing and lose the session.
func (a *Agent) reconcile(ctx context.Context) {
	if err := a.bus.Fetch(ctx); err != nil {
		a.log("reconcile: fetch failed: %v", err)
		return
	}
	ctrl, index, repairs, err := a.bus.ReadController()
	if err != nil {
		a.log("reconcile: cannot read the controller document: %v", err)
		return
	}
	if !repairs.OK() {
		a.log("reconcile: repaired the controller document on read: %s", repairs)
	}

	prevIntent, prevTarget := a.intent, a.restoreTarget
	a.applyController(ctrl)

	if a.intent != prevIntent {
		a.log("reconcile: intent %s -> %s", prevIntent, a.intent)
	}
	if a.restoreTarget != prevTarget && a.restoreTarget != "" && a.pz != nil {
		// Restores only happen at boot, by construction: unpacking a save over a
		// running world produces a corrupt one. The controller's own sequence is
		// stop, set the target, start — so this only fires if someone edited state
		// out of band, and then a log line is the right amount of noise.
		a.log("reconcile: restore_target is now %q; it will be applied on the next boot, not to the running world", a.restoreTarget)
	}

	if req := ctrl.BackupRequest; req != nil && req.ID != "" && !a.answered[req.ID] && a.backup == nil {
		a.startBackup(ctx, *req, index)
	}

	// Only stop once nothing is in flight. A backup interrupted halfway is worse
	// than a halt that takes another minute, and the controller waits out
	// halt_timeout for exactly this.
	if a.intent == state.IntentStopped && a.backup == nil {
		a.stopGame(ctx, "controller intent is stopped")
	}
}

// stopGame asks PZ to quit and parks. Idempotent.
func (a *Agent) stopGame(ctx context.Context, why string) {
	if a.pz == nil || !a.pz.Running() {
		if !a.doc.Phase.Parked() {
			a.setPhase(state.PhaseStopped, why)
			a.publish(ctx, true)
		}
		a.park(why)
		return
	}
	if a.doc.Phase == state.PhaseStopping {
		return
	}
	a.setPhase(state.PhaseStopping, why)
	// Published before the stop begins, so the controller sees "stopping" within
	// one reconcile instead of waiting out quit_timeout in the dark. This is the
	// stamp its halt_confirm timer is watching.
	a.publish(ctx, true)
	a.park(why)

	pz := a.pz
	go pz.Stop(a.log) // evExit arrives on its own; Stop only escalates signals
}

// shutdown is the ctx-cancelled path: the container is going away.
func (a *Agent) shutdown() {
	a.log("shutting down: context cancelled")
	if a.pz != nil && a.pz.Running() {
		// Save first. A SIGTERM from the kubelet usually means the lease is
		// closing, and whatever is not saved here is gone with the volume.
		if err := a.save(); err != nil {
			a.log("shutdown: save failed: %v", err)
		}
		a.pz.Stop(a.log)
	}
	a.procCancel()
}
