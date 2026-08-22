package fsm

import (
	"context"
	"fmt"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// advance moves the machine as far as the current facts allow.
//
// It is a function of the two published documents and the clock, and of nothing
// else. That is what makes the lifecycle recoverable: a controller restarted
// halfway through a halt reads the same documents, calls this, and continues —
// there is no sequence held on a stack to lose.
func (m *Machine) advance(ctx context.Context) {
	now := m.now()

	switch m.doc.Status {
	case state.StatusOffline:
		// Only a start trigger leaves offline. Intent is deliberately not a
		// level the controller acts on: `intent: running` with the server down
		// would otherwise redeploy in a loop against a provider that keeps
		// failing. The agent reads intent as a level; the controller reads
		// triggers as edges.

	case state.StatusDeploying:
		if m.job != nil {
			return // the deploy reports back as an event
		}
		// No job and still "deploying" means we were restarted mid-deploy. We
		// cannot know whether a lease was created, so ask.
		m.logf("fsm: restarted while deploying; reconciling")
		m.adopt(ctx)
		if m.doc.Lease == nil && m.doc.Status == state.StatusDeploying {
			m.toStatus(state.StatusOffline, "deploy was interrupted and created nothing")
			m.doc.Intent = state.IntentStopped
		}

	case state.StatusBooting:
		rep := m.agentReport()
		switch {
		case m.doc.Intent == state.IntentStopped:
			m.beginHalt(ctx, "halt requested while booting")
		case rep.Phase == state.PhaseOnline:
			m.toStatus(state.StatusOnline, "agent reports the server is accepting connections")
		case rep.Phase == state.PhaseRestoreFailed:
			// The agent has parked rather than boot a fresh world over a restore
			// the operator asked for. Do not close the lease from under it: an
			// operator may want to look. Fail, and let them decide.
			m.fail(fmt.Errorf("agent could not restore %q: %s",
				m.doc.RestoreTarget, rep.LastError))
		case rep.Phase.Parked():
			m.beginHalt(ctx, "agent parked during boot: "+string(rep.Phase))
		case m.since(now) > m.cfg.Server.OnlineTimeout.D():
			m.beginHalt(ctx, fmt.Sprintf("no ready banner within %s", m.cfg.Server.OnlineTimeout))
		}

	case state.StatusOnline:
		rep := m.agentReport()
		switch {
		case m.doc.Intent == state.IntentStopped:
			m.beginHalt(ctx, "halt requested")
		case m.doc.StopDue(now):
			m.beginHalt(ctx, "scheduled stop reached")
		case rep.Phase.Parked():
			m.beginHalt(ctx, "agent parked: "+string(rep.Phase))
		case m.doc.BackupRequest != nil:
			// A request published while online but not yet acknowledged: reflect
			// it in the status so the dashboard and the timeout logic agree.
			m.toStatus(state.StatusBackingUp, "backup "+m.doc.BackupRequest.Reason+" in flight")
		case m.backupDue(now):
			m.requestBackup(ctx, "periodic")
		}

	case state.StatusBackingUp:
		if m.doc.Intent == state.IntentStopped {
			// A halt during a backup keeps the backup: it is the same work the
			// halt would ask for, and its report is matched by ID either way.
			m.beginHalt(ctx, "halt requested during a backup")
			return
		}
		if m.settleBackup(now) {
			m.toStatus(state.StatusOnline, "backup settled")
		}

	case state.StatusStopping:
		if m.doc.BackupRequest != nil {
			if !m.settleBackup(now) {
				return
			}
		}
		// The agent parks after PZ exits; that is the signal that the world is
		// safely down. A timeout closes anyway, because a lease left open costs
		// money and the backup — the thing worth protecting — is already done.
		switch rep := m.agentReport(); {
		case rep.Phase.Parked():
			m.beginClose(ctx, "agent parked: "+string(rep.Phase))
		case m.since(now) > m.stoppingBudget():
			m.beginClose(ctx, fmt.Sprintf("agent did not confirm the stop within %s", m.stoppingBudget()))
		}

	case state.StatusClosing:
		if m.job == nil {
			// Either we were restarted mid-close, or a retry is due. Close is
			// idempotent by contract, so re-issuing it is safe.
			m.beginClose(ctx, "resuming close")
		}

	case state.StatusFailed:
		// Failed is sticky: it means an operator should look, and a status that
		// heals itself is a status nobody looks at. The one thing that cannot
		// wait is a lease that is still billing.
		if m.doc.Lease != nil && m.job == nil {
			alive, err := m.akash.Alive(ctx, *m.doc.Lease)
			if err != nil {
				m.logf("fsm: failed state, could not verify dseq %s: %v", m.doc.Lease.DSeq, err)
				return
			}
			if alive {
				m.logf("fsm: failed state with dseq %s still billing; closing it", m.doc.Lease.DSeq)
				m.beginClose(ctx, "closing the lease left by a failure")
			}
		}
	}
}

// since is how long the current status has been in force.
func (m *Machine) since(now time.Time) time.Duration {
	if m.doc.Since.Zero() {
		return 0
	}
	return now.Sub(m.doc.Since.Time)
}

// stoppingBudget bounds the whole of "stopping": the backup, then the wait for
// the agent to confirm PZ is down.
func (m *Machine) stoppingBudget() time.Duration {
	return m.cfg.Backups.HaltTimeout.D() + m.cfg.Backups.HaltConfirm.D()
}

// toStatus applies a transition, logging and refusing an illegal one rather than
// forcing it. A rejected transition is a bug in this file, not in the document,
// so it must be visible without being fatal.
func (m *Machine) toStatus(to state.Status, why string) {
	from := m.doc.Status
	if err := m.doc.SetStatus(to, m.stamp()); err != nil {
		m.logf("fsm: refusing %v (%s)", err, why)
		return
	}
	if from == to {
		return
	}
	m.logf("fsm: %s -> %s (%s)", from, to, why)
	// Set after the transition: SetStatus clears LastError by design, so the
	// explanation has to be written on top of it.
	m.doc.LastError = why
	m.dirty(string(to) + ": " + why)
}

// fail records an unrecoverable problem with the current cycle.
func (m *Machine) fail(err error) {
	m.logf("fsm: FAILED: %v", err)
	// Failed is where an operator takes over, so the retry budget goes back to
	// full: their next start trigger should not inherit a spent one.
	m.deployAttempts = 0
	m.doc.Fail(err, m.stamp())
	m.dirty("failed: " + err.Error())
}

// --- backups ---

// backupDue reports whether the periodic cadence has come round.
//
// The reference point is the newer of "when this online period began" and "when
// the most recent backup was taken", both of which are published facts. So a
// controller restart does not restart the clock, and a fresh boot does not
// immediately take a backup of a world nobody has played yet.
func (m *Machine) backupDue(now time.Time) bool {
	iv := m.cfg.Backups.Interval.D()
	if iv <= 0 {
		return false
	}
	if m.paused() {
		return false
	}
	last := m.doc.Since.Time
	if newest := m.idx.Newest(); newest != nil && newest.CreatedAt.After(last) {
		last = newest.CreatedAt.Time
	}
	return !last.IsZero() && now.Sub(last) >= iv
}

// paused reports whether the operator has parked periodic backups by committing
// backups.pause_file to the operator branch.
//
// It is a plain file at the branch root, not a trigger: a trigger is an edge and
// is consumed, whereas "do not back up for a while" is a level that has to stay
// true until it is deleted.
func (m *Machine) paused() bool {
	name := m.cfg.Backups.PauseFile
	if name == "" {
		return false
	}
	on, err := m.bus.Exists(m.br.Main, name)
	if err != nil {
		m.logf("fsm: could not check %s: %v — treating backups as active", name, err)
		return false
	}
	// Logged on the edge, not on every poll, but re-armed when the file goes away
	// so the operator sees the pause end as well as begin.
	if on != m.warned["paused"] {
		m.warned["paused"] = on
		if on {
			m.logf("fsm: periodic backups paused by %s on %s", name, m.br.Main)
		} else {
			m.logf("fsm: %s is gone; periodic backups resume", name)
		}
	}
	return on
}

// requestBackup publishes an ask for a backup and moves into backing_up.
func (m *Machine) requestBackup(_ context.Context, reason string) {
	if old := m.doc.BackupRequest; old != nil {
		// Checked here rather than by inspecting RequestBackup's return value:
		// that returns the outstanding request unchanged, which is
		// indistinguishable from success when both carry the same reason.
		m.logf("fsm: %s backup skipped: request %s (%s) is still outstanding",
			reason, old.ID, old.Reason)
		return
	}
	req := m.doc.RequestBackup(m.newID(), reason, m.stamp())
	if req == nil {
		m.logf("fsm: could not create a backup request (no id)")
		return
	}
	m.logf("fsm: requesting a %s backup (%s)", reason, req.ID)
	m.dirty("request " + reason + " backup " + req.ID)
	if m.doc.Status == state.StatusOnline {
		m.toStatus(state.StatusBackingUp, "backup "+reason+" requested")
	}
}

// settleBackup reports whether the outstanding request has been answered, one way
// or another, and clears it if so.
//
// Only a report carrying the same ID counts. That is the fix for the halt that
// was signed off by an unrelated backup: v1 had no request identity at all, so
// any archive that appeared satisfied any wait.
func (m *Machine) settleBackup(now time.Time) bool {
	req := m.doc.BackupRequest
	if req == nil {
		return true
	}
	rep := m.agentReport()
	switch st, matched := m.doc.BackupAnswer(rep); {
	case matched && st == state.BackupDone:
		m.recordBackup(rep.Backup)
		m.logf("fsm: backup %s done: %s", req.ID, rep.Backup.Name)
		m.doc.ClearBackupRequest(m.stamp())
		m.dirty("backup " + req.ID + " done")
		return true
	case matched && st == state.BackupFailed:
		m.logf("fsm: backup %s failed: %s", req.ID, rep.Backup.Error)
		m.doc.ClearBackupRequest(m.stamp())
		m.doc.LastError = "backup failed: " + rep.Backup.Error
		m.dirty("backup " + req.ID + " failed")
		return true
	case req.Age(now) > m.cfg.Backups.HaltTimeout.D():
		m.logf("fsm: backup %s gave up after %s", req.ID, req.Age(now).Round(time.Second))
		m.doc.ClearBackupRequest(m.stamp())
		m.doc.LastError = fmt.Sprintf("backup %s timed out after %s", req.ID, m.cfg.Backups.HaltTimeout)
		m.dirty("backup " + req.ID + " timed out")
		return true
	}
	return false // still running, or not yet acknowledged
}

// recordBackup folds a completed report into the index and, under the latest
// policy, points the next boot at it.
//
// Following the newest backup is a deliberate behavioural choice, and it is the
// default. With no persistent storage the server's disk does not survive its
// lease, so a boot that does not restore starts a fresh world — silently, over a
// perfectly good archive. Naming the newest backup makes "start again" mean
// "continue", which is what an operator means by it.
//
// It is nonetheless not unconditional, and that is the point of both the policy
// and the pin. Automatic following cannot see the one failure that matters: a
// backup that faithfully captured a broken world. An operator who has noticed that
// pins an older archive by name — and the very next periodic backup would
// otherwise overwrite their choice, quietly, some minutes later. So a pin wins
// over the policy, always; backups.restore_policy decides only what happens when
// nobody has pinned anything.
//
// The index entry comes from the report only when there is no storage layer to ask.
// With one, the archive is already on disk — the agent uploads before it reports
// done — and the store regenerated the index from the directory as part of
// accepting it. Upserting from the report as well would make this a second writer,
// and the one that is guessing: the report carries the size the agent measured,
// while the index carries the size of the file that is there.
func (m *Machine) recordBackup(rep *state.BackupReport) {
	if rep == nil || rep.Name == "" {
		return
	}
	created := rep.EndedAt
	if created.Zero() {
		created = m.stamp()
	}
	if m.store == nil {
		m.idx.Upsert(state.Backup{
			Name: rep.Name, Size: rep.Size, SHA256: rep.SHA256, CreatedAt: created,
		})
	} else {
		m.refreshIndex()
	}
	if !m.idx.Has(rep.Name) {
		// A report for an archive the store does not hold. The upload failed, or
		// landed somewhere else, and following it would point restore_target at a
		// name the next boot cannot fetch — which is bug 4 wearing a different hat.
		m.logf("fsm: agent reported backup %q but it is not in the index; not following it",
			rep.Name)
		return
	}
	switch {
	case m.cfg.Backups.RestorePolicy != config.RestoreLatest:
		// pinned and none both mean "this is not my decision to make".
	case m.doc.RestorePinned:
		m.logf("fsm: keeping pinned restore target %q; %s was not followed",
			m.doc.RestoreTarget, rep.Name)
	case m.doc.RestoreTarget == rep.Name:
		// A re-reported backup, or the same name twice. Nothing to say.
	default:
		m.doc.RestoreTarget = rep.Name
		m.dirty("restore_target " + rep.Name)
	}
}

// --- halt and close ---

// beginHalt starts, or continues, an orderly shutdown.
//
// The order is the part that matters, and it is the reverse of v1's. The trigger
// has already been consumed by the caller before we get here; intent goes to
// stopped first, so a container that restarts under us reads "stopped" and parks
// instead of announcing itself as booting; and only then does anything long
// happen. A second halt arriving mid-sequence lands on the drop below.
func (m *Machine) beginHalt(ctx context.Context, reason string) {
	m.doc.Intent = state.IntentStopped
	if m.doc.StopAt != nil {
		// A schedule that has fired must not fire again after the next start.
		m.doc.StopAt = nil
		m.dirty("cleared stop_at")
	}

	switch m.doc.Status {
	case state.StatusStopping, state.StatusClosing:
		m.logf("fsm: halt ignored, already %s (%s)", m.doc.Status, reason)
		return

	case state.StatusOffline:
		m.logf("fsm: halt ignored, already offline (%s)", reason)
		m.dirty("intent stopped")
		return

	case state.StatusFailed:
		// Nothing to stop gracefully; advance's failed branch closes any lease.
		m.dirty("intent stopped")
		return

	case state.StatusDeploying:
		// The deploy may already have created a lease, so we cannot decide the
		// next status here. Cancel it and let the result — which carries any
		// lease it managed to create — choose between closing and offline.
		// Intent is already stopped, which is the recoverable record that a halt
		// is pending, so a restart mid-deploy reaches the same place.
		m.cancelJob("halt requested: " + reason)
		m.dirty("halt during deploy: " + reason)
		return
	}

	m.toStatus(state.StatusStopping, reason)
	if m.doc.Status != state.StatusStopping {
		return // the transition was refused and logged
	}

	switch rep := m.agentReport(); {
	case !m.cfg.Backups.OnHalt:
		m.logf("fsm: halt without a final backup (backups.on_halt is false)")
	case rep.Phase.Parked():
		m.logf("fsm: halt without a final backup: the agent is already parked (%s)", rep.Phase)
	case m.doc.BackupRequest != nil:
		m.logf("fsm: halt adopting the backup already in flight (%s)", m.doc.BackupRequest.ID)
	default:
		m.requestBackup(ctx, "halt")
	}
}

// beginClose moves to closing and starts the job. Reaching offline ends with intent
// stopped, so the document never claims to want a server it has just torn down —
// the one exception being a close that is clearing the way for a deploy retry, which
// has not stopped wanting one.
func (m *Machine) beginClose(ctx context.Context, reason string) {
	m.doc.ClearBackupRequest(m.stamp())
	if m.doc.Lease == nil {
		m.doc.Intent = state.IntentStopped
		m.doc.Endpoint = state.Endpoint{}
		m.toStatus(state.StatusOffline, "nothing to close: "+reason)
		return
	}
	m.toStatus(state.StatusClosing, reason)
	if m.doc.Status != state.StatusClosing {
		return
	}
	lease := *m.doc.Lease
	m.start(ctx, "close dseq "+lease.DSeq, func(jctx context.Context) Event {
		err := m.akash.Close(jctx, lease)
		return Event{Kind: KindCloseResult, Source: "close",
			closed: &closeOutcome{lease: lease, err: err}}
	})
}

func (m *Machine) onCloseResult(ctx context.Context, out *closeOutcome) {
	m.job = nil
	if out == nil {
		return
	}
	if out.err != nil {
		m.closeAttempts++
		if m.closeAttempts >= m.cfg.Akash.MaxAttempts {
			m.fail(fmt.Errorf("could not close dseq %s after %d attempts: %w",
				out.lease.DSeq, m.closeAttempts, out.err))
			return
		}
		// Stay in closing; advance retries on the next tick. The lease stays in
		// the document precisely so the retry has something to close.
		m.logf("fsm: close dseq %s failed (attempt %d): %v",
			out.lease.DSeq, m.closeAttempts, out.err)
		return
	}
	m.closeAttempts = 0
	m.doc.Lease = nil
	m.doc.Endpoint = state.Endpoint{}
	m.doc.Price = state.Price{}
	m.toStatus(state.StatusOffline, "closed dseq "+out.lease.DSeq)
	m.dirty("closed dseq " + out.lease.DSeq)

	if m.retryDeploy() {
		// The lease is gone, so I1 holds and a second deploy is legal. The provider
		// that just failed is on the driver's skip list, so this is an attempt at a
		// different one rather than a repeat of the same wall.
		m.beginDeploy(ctx, fmt.Sprintf("retrying the deploy (attempt %d of %d)",
			m.deployAttempts+1, m.cfg.Akash.MaxDeployAttempts))
		return
	}
	m.deployAttempts = 0
	m.doc.Intent = state.IntentStopped
	m.advance(ctx)
}

// retryDeploy decides whether the close that just completed should be followed by
// another deploy.
//
// Three things all have to hold. The close has to have been cleaning up a failed
// deploy (deployAttempts is nonzero only then); the operator must not have asked
// for a halt in the meantime, because that ask outranks our own retry; and the
// budget must not be spent. Note that the budget is rarely what stops this — the
// driver skip-lists each provider that fails, so after a few attempts there is
// nothing eligible left and the deploy fails before it creates anything, which is
// both cheaper and a better error message than "attempt 15 of 15".
func (m *Machine) retryDeploy() bool {
	return m.deployAttempts > 0 &&
		m.doc.Intent == state.IntentRunning &&
		m.deployAttempts < m.cfg.Akash.MaxDeployAttempts
}

// --- deploy ---

// beginDeploy starts a deployment. Two things reach here: a start trigger, and the
// retry that follows a deploy which created a lease and then could not be reached
// (see retryDeploy). Nothing else may — offline does not redeploy on its own.
func (m *Machine) beginDeploy(ctx context.Context, reason string) {
	if m.job != nil {
		m.logf("fsm: start ignored, %s is in flight", m.job.what)
		return
	}
	if m.doc.Lease != nil {
		// Invariant I1. Two leases means two servers writing one world through
		// one DNS name, and no way to tell which is which.
		m.logf("fsm: start refused: dseq %s is still recorded", m.doc.Lease.DSeq)
		m.doc.LastError = "start refused: dseq " + m.doc.Lease.DSeq + " is still active"
		m.dirty("start refused, dseq " + m.doc.Lease.DSeq)
		return
	}
	if m.doc.Status == state.StatusFailed {
		// Failed's only legal exit. Doing it explicitly, and logging it, is how
		// an operator learns that their start also cleared a failure.
		m.logf("fsm: clearing failure before starting: %s", m.doc.LastError)
		m.toStatus(state.StatusOffline, "cleared by a start trigger")
	}

	m.doc.Intent = state.IntentRunning
	m.toStatus(state.StatusDeploying, reason)
	if m.doc.Status != state.StatusDeploying {
		return
	}
	// Publish before the deploy rather than after: the ask has to be on record
	// before anything can start billing for it.
	m.flushNow(ctx)

	m.deployAttempts++
	req := DeployRequest{
		ControllerURL: m.controllerURL(),
		RestoreTarget: m.doc.RestoreTarget,
		Attempt:       m.deployAttempts,
	}
	budget := m.cfg.Akash.Timeouts.BidWait.D() + m.cfg.Akash.Timeouts.LeaseReady.D()
	m.start(ctx, "deploy", func(jctx context.Context) Event {
		jctx, cancel := context.WithTimeout(jctx, budget)
		defer cancel()
		res, err := m.akash.Deploy(jctx, req)
		return Event{Kind: KindDeployResult, Source: "deploy",
			deploy: &deployOutcome{res: res, err: err}}
	})
}

func (m *Machine) onDeployResult(ctx context.Context, out *deployOutcome) {
	m.job = nil
	if out == nil {
		return
	}

	// Record the lease first, unconditionally, before looking at the error. A
	// deploy that created a lease and then failed is the one case where losing
	// the dseq costs money for as long as the escrow lasts.
	if out.res.Lease.DSeq != "" {
		l := out.res.Lease
		m.doc.Lease = &l
		m.doc.Price = out.res.Price
		m.dirty("dseq " + l.DSeq)
	}

	if out.err != nil {
		switch {
		case m.doc.Lease != nil:
			m.logf("fsm: deploy failed after creating dseq %s: %v", m.doc.Lease.DSeq, out.err)
			m.doc.LastError = "deploy failed: " + out.err.Error()
			m.beginClose(ctx, "deploy failed after creating dseq "+m.doc.Lease.DSeq)
		default:
			m.fail(fmt.Errorf("deploy failed: %w", out.err))
		}
		return
	}

	m.doc.Endpoint = out.res.Endpoint
	m.dirty("endpoint " + m.doc.Endpoint.IP)
	// The budget bought a routable lease, which is what it was for.
	m.deployAttempts = 0

	if m.doc.Intent == state.IntentStopped {
		// A halt arrived while we were deploying. The lease exists, so it has to
		// be closed rather than merely forgotten.
		m.logf("fsm: deploy completed into a pending halt; closing dseq %s", m.doc.Lease.DSeq)
		m.beginClose(ctx, "halt arrived during the deploy")
		return
	}

	m.toStatus(state.StatusBooting, fmt.Sprintf("dseq %s ready at %s:%d",
		m.doc.Lease.DSeq, m.doc.Endpoint.IP, m.doc.Endpoint.GamePort))
	m.advance(ctx)
}

// --- jobs ---

// job is a long operation running off the loop. At most one exists, which is what
// makes "busy" a property the machine can rely on rather than hope for.
type job struct {
	what   string
	cancel context.CancelFunc
}

// start launches fn on its own goroutine. fn returns the event to deliver; the
// send is blocking, because a job result that is dropped leaves a status waiting
// forever — the opposite trade-off from Send's.
func (m *Machine) start(ctx context.Context, what string, fn func(context.Context) Event) {
	jctx, cancel := context.WithCancel(ctx)
	m.job = &job{what: what, cancel: cancel}
	m.logf("fsm: %s started", what)
	go func() {
		ev := fn(jctx)
		cancel()
		select {
		case m.events <- ev:
		case <-ctx.Done():
			// Run is shutting down. The outcome is lost, which is why every job's
			// effect is also discoverable from the provider: the next start
			// reconciles before it deploys.
		}
	}()
}

// cancelJob asks the in-flight job to stop. It does not wait: the result arrives
// as an event like any other, and blocking here would be blocking the only
// goroutine that can process it.
func (m *Machine) cancelJob(why string) {
	if m.job == nil {
		return
	}
	m.logf("fsm: cancelling %s (%s)", m.job.what, why)
	m.job.cancel()
}

// flushNow publishes immediately, bypassing the coalescing window. Reserved for
// the writes that must be on record before an external side effect — creating a
// lease, and nothing else.
func (m *Machine) flushNow(ctx context.Context) {
	m.lastPush = time.Time{}
	m.flush(ctx)
}
