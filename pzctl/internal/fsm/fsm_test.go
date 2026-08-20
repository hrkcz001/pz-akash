package fsm

import (
	"fmt"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// TestFullCycle is the gate for this step: start, run, back up, halt, close —
// driven entirely by trigger files pushed to a real remote, with a simulated agent
// answering on its own branch.
func TestFullCycle(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.wantStatus(state.StatusOffline)
	h.wantIntent(state.IntentStopped)

	// --- start ---
	h.trigger("start", "")
	h.poll()
	h.wantStatus(state.StatusDeploying)
	h.wantIntent(state.IntentRunning)
	if left := h.triggersLeft(); len(left) != 0 {
		t.Fatalf("triggers still pending after the consume commit: %v", left)
	}
	h.settle()
	h.wantStatus(state.StatusBooting)
	h.wantLease(true)
	h.wantLive(1)

	// The endpoint has to be published before players can be told about it.
	if got := h.published().Endpoint; !got.Ready() {
		t.Fatalf("published endpoint is not ready: %+v", got)
	}

	// --- online ---
	h.agentPhase(state.PhaseOnline)
	h.poll()
	h.wantStatus(state.StatusOnline)

	// --- operator backup ---
	h.trigger("backup", "")
	h.poll()
	h.wantStatus(state.StatusBackingUp)
	req := h.published().BackupRequest
	if req == nil {
		t.Fatal("no backup request was published, so the agent has nothing to answer")
	}
	if req.Reason != "operator" {
		t.Fatalf("request reason = %q, want operator", req.Reason)
	}

	h.agentPhase(state.PhaseSaving)
	h.agentBackup(state.BackupDone, "backup_20260819_100500.zip", 1<<20)
	h.agentPhase(state.PhaseOnline)
	h.poll()
	h.wantStatus(state.StatusOnline)
	if h.m.doc.BackupRequest != nil {
		t.Fatal("the answered request is still outstanding, so no further backup can run")
	}
	if !h.m.idx.Has("backup_20260819_100500.zip") {
		t.Fatalf("backup missing from the index: %+v", h.m.idx.Items)
	}
	if got := h.published().RestoreTarget; got != "backup_20260819_100500.zip" {
		h.dumpLogs()
		t.Fatalf("restore_target = %q (in memory %q), want the backup just taken — otherwise the next start boots a fresh world",
			got, h.m.doc.RestoreTarget)
	}

	// --- halt ---
	h.trigger("halt", "")
	h.poll()
	h.wantStatus(state.StatusStopping)
	h.wantIntent(state.IntentStopped)
	halt := h.published().BackupRequest
	if halt == nil {
		t.Fatal("halt published no final backup request")
	}
	if halt.ID == req.ID {
		t.Fatal("the halt reused the operator request's ID, so a stale report could sign it off")
	}
	if halt.Reason != "halt" {
		t.Fatalf("halt request reason = %q, want halt", halt.Reason)
	}

	// Intent must reach the branch before anything long happens: it is what stops a
	// restarted container from announcing itself as booting mid-halt.
	if got := h.published().Intent; got != state.IntentStopped {
		t.Fatalf("published intent = %s during a halt, want stopped", got)
	}

	h.agentBackup(state.BackupDone, "backup_20260819_103000.zip", 2<<20)
	h.poll()
	// The backup is in, but PZ has not confirmed it is down, so the lease stays.
	h.wantStatus(state.StatusStopping)
	h.wantLive(1)

	h.agentPhase(state.PhaseStopped)
	h.poll()
	h.settle()

	h.wantStatus(state.StatusOffline)
	h.wantIntent(state.IntentStopped)
	h.wantLease(false)
	h.wantLive(0)

	final := h.published()
	if final.Status != state.StatusOffline {
		t.Fatalf("published status = %s, want offline", final.Status)
	}
	if final.RestoreTarget != "backup_20260819_103000.zip" {
		t.Fatalf("restore_target = %q, want the halt backup", final.RestoreTarget)
	}
	if final.BackupRequest != nil {
		t.Fatal("a request is still outstanding after the halt completed")
	}
}

// TestDuplicateHaltTriggers is bug 2 in miniature. Three halts arriving during one
// halt must produce one backup request and one close.
func TestDuplicateHaltTriggers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()

	h.trigger("halt", "")
	h.poll()
	h.wantStatus(state.StatusStopping)
	first := h.m.doc.BackupRequest
	if first == nil {
		t.Fatal("no halt backup request")
	}

	for i := 0; i < 3; i++ {
		h.trigger("halt", "")
		h.poll()
		h.wantStatus(state.StatusStopping)
		if got := h.m.doc.BackupRequest; got == nil || got.ID != first.ID {
			h.dumpLogs()
			t.Fatalf("duplicate halt #%d changed the outstanding request: %+v", i+1, got)
		}
	}
	if !h.logged("halt ignored, already stopping") {
		h.dumpLogs()
		t.Fatal("the duplicate halts were not visibly dropped")
	}

	// One backup, one close, one offline.
	h.agentBackup(state.BackupDone, "backup_20260819_100000.zip", 1)
	h.agentPhase(state.PhaseStopped)
	h.poll()
	h.settle()
	h.wantStatus(state.StatusOffline)
	h.wantLive(0)
	if n := len(h.m.idx.Items); n != 1 {
		t.Fatalf("index holds %d backups, want 1 — duplicate halts produced duplicate backups", n)
	}
}

// TestStaleBackupCannotSignOffHalt is bug 4's root cause. A report carrying an old
// request ID must not satisfy the halt: v1 had no IDs, so the halt accepted the
// next archive to appear and closed the lease over a world several minutes stale.
func TestStaleBackupCannotSignOffHalt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()

	// A periodic backup starts first.
	h.clk.add(2 * time.Hour)
	h.tick()
	h.wantStatus(state.StatusBackingUp)
	periodic := h.m.doc.BackupRequest
	if periodic == nil {
		t.Fatal("no periodic backup request")
	}

	// The halt adopts the backup already in flight rather than starting a second
	// one: two concurrent archives of one Saves directory could only be torn.
	h.trigger("halt", "")
	h.poll()
	h.wantStatus(state.StatusStopping)
	if got := h.m.doc.BackupRequest; got == nil || got.ID != periodic.ID {
		t.Fatalf("halt did not adopt the in-flight request: %+v", got)
	}

	// A report for a request that no longer exists answers nothing.
	h.agentBackupID("req-ancient", state.BackupDone, "backup_20260819_120000.zip", 1)
	h.agentPhase(state.PhaseStopped)
	h.poll()
	if h.m.doc.Status != state.StatusStopping {
		h.dumpLogs()
		t.Fatalf("status = %s: a stale report signed off the halt", h.m.doc.Status)
	}
	if h.m.idx.Has("backup_20260819_120000.zip") {
		t.Fatal("a stale report was folded into the index")
	}

	// The real answer completes it.
	h.agentBackupID(periodic.ID, state.BackupDone, "backup_20260819_120500.zip", 2)
	h.poll()
	h.settle()
	h.wantStatus(state.StatusOffline)
	if !h.m.idx.Has("backup_20260819_120500.zip") {
		t.Fatal("the matching report was not recorded")
	}
	h.wantLive(0)
}

// TestHaltDuringDeploy is the expensive case: a halt arrives while a deploy is in
// flight, and the deploy has already created a lease. Cancelling must not lose it.
func TestHaltDuringDeploy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	release := h.holdDeploys()
	defer release()

	h.trigger("start", "")
	h.poll()
	h.wantStatus(state.StatusDeploying)

	h.trigger("halt", "")
	h.poll()
	// Still deploying: the status cannot move until we know whether a lease exists.
	h.wantStatus(state.StatusDeploying)
	h.wantIntent(state.IntentStopped)

	h.settle() // the cancelled deploy's result, then the close it triggers
	h.wantStatus(state.StatusOffline)
	h.wantLease(false)
	h.wantLive(0)
	if !h.logged("cancelling deploy") {
		h.dumpLogs()
		t.Fatal("the deploy was not cancelled")
	}
	if !h.logged("deploy failed after creating dseq") {
		h.dumpLogs()
		t.Fatal("the lease the cancelled deploy created was not carried back")
	}
}

// TestDeployFailureAfterLeaseClosesIt covers the shape of failure that leaks
// money: the lease is created and then the deploy fails. It must be closed, and —
// because the usual cause is one bad provider rather than a bad network — another
// provider must be tried, up to akash.max_deploy_attempts.
func TestDeployFailureAfterLeaseClosesIt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.dry.FailDeploy = true

	h.trigger("start", "")
	h.poll()
	h.settle()

	h.wantStatus(state.StatusOffline)
	h.wantLease(false)
	// The assertion that matters: every deployment the retries created was closed.
	h.wantLive(0)
	if !h.logged("deploy failed after creating dseq") {
		h.dumpLogs()
		t.Fatal("the partial lease was not recognised")
	}
	if !h.logged("retrying the deploy") {
		h.dumpLogs()
		t.Fatal("a deploy that failed after leasing was not retried against another provider")
	}
	// Bounded, and by the deploy knob rather than the close one: an unbounded retry
	// is v1's bug 2 with a different trigger.
	max := h.m.cfg.Akash.MaxDeployAttempts
	if got := h.logCount("fsm: deploy started"); got != max {
		h.dumpLogs()
		t.Fatalf("made %d deploys, want exactly max_deploy_attempts (%d)", got, max)
	}
	if !h.logged(fmt.Sprintf("attempt %d of %d", max, max)) {
		h.dumpLogs()
		t.Errorf("the last retry did not report itself as attempt %d of %d", max, max)
	}
	// Giving up leaves the document not wanting a server, so nothing resumes it
	// behind the operator's back.
	h.wantIntent(state.IntentStopped)
}

// TestDeployRetryYieldsToAHalt: the retry is ours, the halt is the operator's. If
// one arrives while a failed attempt is being cleaned up, the cleanup is the end of
// it — otherwise a halt during a bad patch of providers would be ignored for as
// many attempts as the budget allows.
func TestDeployRetryYieldsToAHalt(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.dry.FailDeploy = true
	release := h.holdDeploys()
	defer release()

	h.trigger("start", "")
	h.poll()
	h.wantStatus(state.StatusDeploying)

	// The halt has to be consumed while the deploy is still parked: that is the
	// window in which the operator's ask and our retry contend.
	h.trigger("halt", "")
	h.poll()
	h.wantIntent(state.IntentStopped)

	release()
	h.settle()

	h.wantStatus(state.StatusOffline)
	h.wantLease(false)
	h.wantLive(0)
	h.wantIntent(state.IntentStopped)
	if h.logged("retrying the deploy") {
		h.dumpLogs()
		t.Fatal("the deploy was retried after a halt was asked for")
	}
	if got := h.logCount("fsm: deploy started"); got != 1 {
		h.dumpLogs()
		t.Errorf("made %d deploys after a halt, want 1", got)
	}
}

// TestStartRefusedWhileLeased is invariant I1: two leases means two servers
// writing one world.
func TestStartRefusedWhileLeased(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()

	h.trigger("start", "")
	h.poll()
	h.wantStatus(state.StatusOnline)
	h.wantLive(1)
	if !h.logged("start refused") {
		h.dumpLogs()
		t.Fatal("a second start was not refused")
	}
}

// TestVanishedLeaseFails covers a lease that disappeared provider-side while the
// controller was down — the case where the document is confidently wrong.
func TestVanishedLeaseFails(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()

	// Close it behind the controller's back, then restart the controller.
	if err := h.dry.Close(t.Context(), *h.m.doc.Lease); err != nil {
		t.Fatal(err)
	}
	if err := h.m.load(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	h.wantStatus(state.StatusFailed)
	h.wantLease(false)

	// load records the failure but does not push; the first pass afterwards does,
	// which is what makes the failure visible on the dashboard.
	h.poll()
	if got := h.published().Status; got != state.StatusFailed {
		t.Fatalf("published status = %s, want failed", got)
	}

	// Failed is sticky: a tick must not heal it on its own.
	h.tick()
	h.wantStatus(state.StatusFailed)

	// A start clears it, going through offline as the transition table requires.
	// The agent is a fresh container, so it reports starting rather than online.
	h.agentPhase(state.PhaseStarting)
	h.trigger("start", "")
	h.poll()
	h.wantStatus(state.StatusDeploying)
	h.settle()
	h.wantStatus(state.StatusBooting)
	if !h.logged("clearing failure before starting") {
		h.dumpLogs()
		t.Fatal("the start did not say that it had cleared a failure")
	}
}

// TestAdoptOnUnreadableState is the last line of defence for I1: the document is
// corrupt, so the only way to learn about a running lease is to ask the provider.
func TestAdoptOnUnreadableState(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()
	dseq := h.m.doc.Lease.DSeq

	// Replace the published document with something that parses as nothing.
	h.corruptControllerState()
	if err := h.m.load(t.Context()); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if h.m.doc.Lease == nil {
		h.dumpLogs()
		t.Fatal("the lease was not adopted, so it would bill unwatched")
	}
	if got := h.m.doc.Lease.DSeq; got != dseq {
		t.Fatalf("adopted dseq %s, want %s", got, dseq)
	}
	h.wantStatus(state.StatusFailed)
}

// TestRestoreTargetMustExist covers the silent data-loss path: a target that names
// nothing makes the agent boot a fresh world over a good save.
func TestRestoreTargetMustExist(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	// The name is a well-formed backup name that simply is not in the index: a
	// malformed one would be rejected by the normalizer on read, which is a
	// different defence and would let this test pass for the wrong reason.
	h.trigger("restore", "backup_20260101_000000.zip")
	h.poll()
	if got := h.m.doc.RestoreTarget; got != "" {
		t.Fatalf("restore_target = %q, want it rejected", got)
	}
	if !h.logged("is not in the index") {
		h.dumpLogs()
		t.Fatal("the unknown target was not reported")
	}

	// A known one is accepted.
	h.m.idx.Upsert(state.Backup{Name: "backup_20260819_090000.zip", CreatedAt: h.stamp()})
	h.trigger("restore", "backup_20260819_090000.zip\n")
	h.poll()
	if got := h.m.doc.RestoreTarget; got != "backup_20260819_090000.zip" {
		t.Fatalf("restore_target = %q, want backup_20260819_090000.zip", got)
	}
}

// TestStopAtSchedulesAndFires covers the scheduled shutdown, including that a
// fired schedule is cleared so it cannot fire again after the next start.
func TestStopAtSchedulesAndFires(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()

	h.trigger("stop_at", "2026-08-19T23:30")
	h.poll()
	h.wantStatus(state.StatusOnline)
	if h.m.doc.StopAt == nil {
		t.Fatal("stop_at was not scheduled")
	}
	// Read in identity.timezone, not the host's: 23:30 Prague is 21:30 UTC.
	if got := h.m.doc.StopAt.Time.UTC().Format("15:04"); got != "21:30" {
		t.Fatalf("stop_at is %s UTC, want 21:30 — it was not read in Europe/Prague", got)
	}

	h.clk.add(14 * time.Hour)
	h.tick()
	h.wantStatus(state.StatusStopping)
	if h.m.doc.StopAt != nil {
		t.Fatal("the fired schedule was not cleared, so it fires again after the next start")
	}
}

// TestPeriodicBackupPause covers the level signal: a pause file at the branch root
// suspends the cadence without being consumed.
func TestPeriodicBackupPause(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.bringOnline()

	h.push(map[string]string{h.cfg.Backups.PauseFile: "updating mods\n"})
	h.clk.add(2 * time.Hour)
	h.poll()
	h.wantStatus(state.StatusOnline)
	if h.m.doc.BackupRequest != nil {
		t.Fatal("a periodic backup ran while paused")
	}

	h.removePause()
	h.poll()
	h.wantStatus(state.StatusBackingUp)
	if h.m.doc.BackupRequest == nil {
		t.Fatal("the cadence did not resume when the pause file went away")
	}
}

// TestUnknownTriggerIsLeftInPlace: a name we do not recognise is not ours to
// delete, and must not be consumed silently.
func TestUnknownTriggerIsLeftInPlace(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.trigger("backup-please", "reason: before the update\n")
	h.poll()
	if left := h.triggersLeft(); len(left) != 1 || left[0] != "backup-please" {
		t.Fatalf("triggers left = %v, want the unknown file untouched", left)
	}
	if !h.logged(`ignoring unknown trigger "backup-please"`) {
		h.dumpLogs()
		t.Fatal("the unknown trigger was not reported")
	}
}

// TestProcessedSHADedup is bug 2's webhook half: our own state push must not come
// back as an operator request, across a restart as well as within one process.
func TestProcessedSHADedup(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)

	h.trigger("start", "")
	h.poll()
	h.settle()

	// The consume commit is on the operator branch, and its delivery would arrive
	// after we had already acted on it.
	head, err := h.m.bus.Head()
	if err != nil {
		t.Fatal(err)
	}
	if !h.m.doc.WasProcessed(head) {
		h.dumpLogs()
		t.Fatalf("head %s is not in the dedup ring, so its delivery would replay the trigger", head)
	}
	h.m.handle(t.Context(), Poll("webhook", head))
	if !h.logged("already processed") {
		h.dumpLogs()
		t.Fatal("the echo of our own consume commit was not dropped")
	}
}

// TestOnceWalksAStart is the CLI-shaped path: one pass must not return until the
// job it started has reported back.
func TestOnceWalksAStart(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	h.dry.Delay = 200 * time.Millisecond

	h.trigger("start", "")
	if err := h.m.Once(t.Context()); err != nil {
		h.dumpLogs()
		t.Fatalf("Once: %v", err)
	}
	h.wantStatus(state.StatusBooting)
	h.wantLease(true)
	if h.m.job != nil {
		t.Fatal("Once returned with a job still in flight")
	}
	if h.published().Status != state.StatusBooting {
		t.Fatal("Once did not publish its conclusion")
	}
}
