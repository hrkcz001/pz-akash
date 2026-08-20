package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// Boot-time behaviour: what the agent does with the world before it launches
// anything, and what it does when it must not launch at all.

func TestAgentParksWithoutLaunchingWhenIntentIsStopped(t *testing.T) {
	h := newHarness(t)
	// The container came back on its own — Kubernetes restarted it mid-halt, which
	// is what restartPolicy: Always does to a process that exits.
	h.doc.Intent = state.IntentStopped
	h.publish("halt in progress")
	h.start()

	h.waitPhase(state.PhaseStopped, 30*time.Second)

	// Nothing was launched, and nothing will be. This is bug 2 at its root: v1's
	// entrypoint had no way to ask what the desired state was, so a restart during
	// a halt brought the world back up and the controller read it as a flap.
	time.Sleep(8 * h.cfg.Agent.Reconcile.D())
	if fileExists(h.touch) {
		t.Errorf("PZ was launched %d time(s) while intent was stopped", h.launches())
	}
	if !h.sawLog("parking without launching") {
		t.Error("the agent did not say why it parked")
	}
	if doc := h.agentDoc(); doc.Phase != state.PhaseStopped {
		t.Errorf("phase = %s, want it to stay stopped", doc.Phase)
	}

	// Parked is not exited, and that distinction is the fix. The loop keeps
	// publishing, so the controller can tell a parked container from a dead one.
	before := h.agentDoc()
	h.doc.Intent = state.IntentStopped
	h.publish("still halted")
	h.waitFor("a liveness stamp from the parked agent", 30*time.Second, func() bool {
		return h.agentDoc().LivenessAt.Time.After(before.LivenessAt.Time)
	})

	h.stop()
}

func TestParkedAgentStillAnswersABackupRequest(t *testing.T) {
	h := newHarness(t)
	// A world on disk with no process to save it: the container is parked, and the
	// controller is asking for the archive precisely because it is about to close
	// the lease. Refusing here would throw the session away.
	writeFile(t, filepath.Join(h.savesDir(), "Multiplayer", h.cfg.Identity.ServerName, "map_t.bin"), "the world at halt")
	h.doc.Intent = state.IntentStopped
	h.publish("halted")
	h.start()
	h.waitPhase(state.PhaseStopped, 30*time.Second)

	h.doc.BackupRequest = &state.BackupRequest{
		ID:          "req-parked",
		Reason:      "halt",
		RequestedAt: state.Now(h.cfg.Location()),
	}
	h.publish("backup requested from a parked agent")

	h.waitFor("the backup from the parked agent", 60*time.Second, func() bool {
		up, _ := h.uploadFor("req-parked")
		return up.Name != ""
	})
	up, _ := h.uploadFor("req-parked")
	rel := "Multiplayer/" + h.cfg.Identity.ServerName + "/map_t.bin"
	if body, ok := zipEntry(t, up.Body, rel); !ok || body != "the world at halt" {
		t.Errorf("%s = %q (present %v); the archive must hold the world on disk", rel, body, ok)
	}

	h.waitFor("the report", 30*time.Second, func() bool {
		d := h.agentDoc()
		return d.Backup != nil && d.Backup.RequestID == "req-parked" && d.Backup.State == state.BackupDone
	})
	// Still parked, still stopped. Reporting "saving" here would tell the controller
	// the server had come back, and it would answer with another halt.
	if doc := h.agentDoc(); doc.Phase != state.PhaseStopped {
		t.Errorf("phase = %s after a backup from a parked agent, want stopped", doc.Phase)
	}
	if h.sawLog("-> saving") {
		t.Error("the parked agent reported the saving phase; there was no process to save")
	}
	if fileExists(h.touch) {
		t.Error("the backup request launched PZ")
	}

	h.stop()
}

func TestAgentRestoresTheRequestedBackupBeforeLaunching(t *testing.T) {
	h := newHarness(t)
	// A world already on disk, which the restore must replace rather than merge
	// with. v1 unpacked over the top, so files the archive did not contain survived
	// into the "restored" world.
	stale := filepath.Join(h.savesDir(), "Multiplayer", h.cfg.Identity.ServerName, "stale.bin")
	writeFile(t, stale, "a world nobody asked for")

	const name = "backup_20260819_120000.zip"
	h.serveBackup(name, map[string]string{
		"Multiplayer/" + h.cfg.Identity.ServerName + "/map_t.bin": "the restored world",
	}, "")
	h.doc.RestoreTarget = name
	h.publish("restore " + name)
	h.start()

	h.waitPhase(state.PhaseOnline, 60*time.Second)

	restored := filepath.Join(h.savesDir(), "Multiplayer", h.cfg.Identity.ServerName, "map_t.bin")
	if body := readFile(t, restored); body != "the restored world" {
		t.Errorf("%s = %q", restored, body)
	}
	if fileExists(stale) {
		t.Error("the old world survived the restore; a restore is a replacement, not a merge")
	}
	// The restore happened before the launch, in that order. Unpacking a save under
	// a running JVM produces a world that is half of each.
	if ok, both := h.logOrder("restore: "+name+" applied", "phase: restoring -> starting"); !both || !ok {
		t.Errorf("PZ was launched before the restore finished (ok=%v both=%v)", ok, both)
	}
	if n := h.launches(); n != 1 {
		t.Errorf("PZ was launched %d times, want 1", n)
	}

	h.stop()
}

func TestAgentRefusesACorruptRestoreAndKeepsTheWorld(t *testing.T) {
	h := newHarness(t)
	stale := filepath.Join(h.savesDir(), "Multiplayer", h.cfg.Identity.ServerName, "map_t.bin")
	writeFile(t, stale, "the world that is actually here")

	const name = "backup_20260819_130000.zip"
	// Indexed with a digest the archive does not have: a truncated upload, or a
	// half-written file on the controller's ephemeral disk.
	h.serveBackup(name, map[string]string{"Multiplayer/x/map_t.bin": "truncated"},
		strings.Repeat("0", 64))
	h.doc.RestoreTarget = name
	h.publish("restore " + name)
	h.start()

	doc := h.waitPhase(state.PhaseRestoreFailed, 60*time.Second)

	// Its own phase, not "crashed": the controller must not retry this as a crash,
	// and the operator has to know the world running is not the one they asked for.
	if !strings.Contains(doc.LastError, "corrupt") {
		t.Errorf("last_error = %q, want it to name the digest mismatch", doc.LastError)
	}
	// The world that was here is still here. The digest is checked before Saves is
	// emptied, precisely so a bad archive cannot destroy a good world.
	if body := readFile(t, stale); body != "the world that is actually here" {
		t.Errorf("%s = %q; the failed restore damaged the world on disk", stale, body)
	}
	if fileExists(h.touch) {
		t.Error("PZ was launched after a failed restore; the operator would not know which world is running")
	}

	h.stop()
}

func TestAgentStopsRelaunchingAtTheCrashBudget(t *testing.T) {
	h := newHarness(t)
	// A server that dies shortly after reporting itself ready: a mod that throws on
	// the first tick, or an OOM kill.
	t.Setenv(fakeExitAfter, "400ms")
	t.Setenv(fakeExitCode, "1")
	h.start()

	doc := h.waitPhase(state.PhaseCrashed, 90*time.Second)

	want := h.cfg.Server.Crash.MaxRestarts + 1
	if n := h.launches(); n != want {
		t.Errorf("PZ was launched %d times, want %d (the first boot plus max_restarts)", n, want)
	}
	if doc.Restarts != h.cfg.Server.Crash.MaxRestarts+1 {
		t.Errorf("restarts = %d, want %d", doc.Restarts, h.cfg.Server.Crash.MaxRestarts+1)
	}
	if doc.LastError == "" {
		t.Error("last_error is empty after a crash loop")
	}

	// Crashed means parked, not exited: the container stays up so the controller can
	// decide to close the lease, instead of the kubelet restarting us into the same
	// crash loop with a fresh budget every time.
	time.Sleep(10 * h.cfg.Agent.Reconcile.D())
	if n := h.launches(); n != want {
		t.Errorf("PZ was launched %d times after the budget was spent, want %d", n, want)
	}
	if d := h.agentDoc(); d.Phase != state.PhaseCrashed {
		t.Errorf("phase = %s, want it to stay crashed", d.Phase)
	}

	h.stop()
}

func TestAgentReportsPlayersUnknownRatherThanZero(t *testing.T) {
	h := newHarness(t)
	players := filepath.Join(h.dir, "players.txt")
	writeFile(t, players, "5")
	t.Setenv(fakePlayersFile, players)
	h.start()

	h.waitPhase(state.PhaseOnline, 60*time.Second)
	h.waitFor("the measured count", 30*time.Second, func() bool {
		return h.agentDoc().PlayersCount == 5
	})

	// The console stops answering with anything a parser recognises — a build whose
	// wording changed, or a JVM too busy to reply.
	writeFile(t, players, "silent")
	h.waitFor("the count to become unknown", 30*time.Second, func() bool {
		return h.agentDoc().PlayersCount == state.PlayersUnknown
	})

	// Never a fabricated zero on the way. That single value is bug 1: it made the
	// dashboard claim an empty server, and it is what the controller's
	// pause-when-empty logic would have acted on.
	if h.sawLog("players=0") {
		t.Error("a zero player count was published from an unanswered poll")
	}
	if !h.sawLog("reporting unknown") {
		t.Error("the agent did not report why the count became unknown")
	}
	if doc := h.agentDoc(); !doc.PlayersAt.Zero() {
		t.Errorf("players_at = %s on an unknown count, want it cleared", doc.PlayersAt.Time)
	}

	h.stop()
}
