package fsm

import (
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// restoreHarness is the shape every test below wants: online, with the periodic
// cadence off so that advancing the clock does not queue backups nobody asked
// for, and with one archive already in the index that predates the test.
//
// The pre-existing archive matters. The failure this whole group is about only
// appears when there is an older good world to prefer over a newer bad one.
func restoreHarness(t *testing.T, policy string) *harness {
	t.Helper()
	h := newHarness(t, func(c *config.Config) {
		c.Backups.Interval = 0
		c.Backups.RestorePolicy = policy
	})
	h.bringOnline()
	h.m.idx.Upsert(state.Backup{Name: theGoodWorld, CreatedAt: h.stamp()})
	return h
}

// Names, not literals, because the test reads as an argument about which of the
// two the next boot gets.
const (
	theGoodWorld   = "backup_20260818_120000.zip"
	theBrokenWorld = "backup_20260819_110000.zip"
)

// takeBackup runs one operator backup through to a report and settles it.
func (h *harness) takeBackup(name string) {
	h.t.Helper()
	h.trigger("backup", "")
	h.poll()
	h.agentBackup(state.BackupDone, name, 1<<20)
	h.poll()
}

// TestAnOperatorPinBeatsTheNextBackup is the §7 flaw itself.
//
// An operator pins an older archive precisely because the recent ones are bad —
// a backup that faithfully captured a broken world is the one failure automatic
// following cannot see. Before the fix, recordBackup overwrote restore_target
// unconditionally, so their choice lasted until the next periodic backup: some
// minutes, silently, with no error anywhere.
func TestAnOperatorPinBeatsTheNextBackup(t *testing.T) {
	t.Parallel()
	h := restoreHarness(t, config.RestoreLatest)

	h.trigger("restore", theGoodWorld)
	h.poll()
	if got := h.m.doc.RestoreTarget; got != theGoodWorld {
		t.Fatalf("restore_target = %q, want %q", got, theGoodWorld)
	}
	if !h.m.doc.RestorePinned {
		t.Fatal("naming an archive did not pin it, so the next backup will overwrite the choice")
	}

	h.clk.add(time.Hour)
	h.takeBackup(theBrokenWorld)

	if got := h.m.doc.RestoreTarget; got != theGoodWorld {
		h.dumpLogs()
		t.Fatalf("restore_target = %q, want the pinned %q — the operator's choice was overwritten by a backup",
			got, theGoodWorld)
	}
	if got := h.published().RestoreTarget; got != theGoodWorld {
		t.Fatalf("published restore_target = %q, want %q", got, theGoodWorld)
	}
	// The archive is still recorded: the pin governs what boots, not what exists.
	if !h.m.idx.Has(theBrokenWorld) {
		t.Fatalf("%s is missing from the index: %+v", theBrokenWorld, h.m.idx.Items)
	}
	if !h.logged("keeping pinned restore target") {
		h.dumpLogs()
		t.Fatal("the pin was honoured without saying so, which is how an operator learns nothing")
	}
}

// TestFollowingTheNewestAgainReleasesThePin covers the way back. A pin that could
// only be replaced by another pin would mean an operator who once chose an archive
// has to keep choosing one before every start, forever.
func TestFollowingTheNewestAgainReleasesThePin(t *testing.T) {
	t.Parallel()
	h := restoreHarness(t, config.RestoreLatest)

	h.trigger("restore", theGoodWorld)
	h.poll()
	h.clk.add(time.Hour)
	h.takeBackup(theBrokenWorld)

	// "latest" releases the pin, and applies immediately: leaving the stale target
	// in place until the next backup completed would be a boot into the world the
	// operator has just stopped asking for.
	h.trigger("restore", "latest\n")
	h.poll()
	if h.m.doc.RestorePinned {
		t.Fatal("the pin survived an explicit request to follow the newest backup")
	}
	if got := h.m.doc.RestoreTarget; got != theBrokenWorld {
		h.dumpLogs()
		t.Fatalf("restore_target = %q, want the newest %q", got, theBrokenWorld)
	}

	// And following really has resumed, rather than the flag merely being cleared.
	const newer = "backup_20260819_130000.zip"
	h.clk.add(time.Hour)
	h.takeBackup(newer)
	if got := h.m.doc.RestoreTarget; got != newer {
		h.dumpLogs()
		t.Fatalf("restore_target = %q, want %q — following did not resume", got, newer)
	}
}

// TestRestorePolicyPinnedNeverFollows: under this policy the target is only ever
// an operator's decision. It trades the risk latest cannot see (a good backup of a
// broken world) for the one it can (a start that restores something older than the
// last session), and the trade has to be explicit in config either way.
func TestRestorePolicyPinnedNeverFollows(t *testing.T) {
	t.Parallel()
	h := restoreHarness(t, config.RestorePinned)

	h.clk.add(time.Hour)
	h.takeBackup(theBrokenWorld)
	if got := h.m.doc.RestoreTarget; got != "" {
		h.dumpLogs()
		t.Fatalf("restore_target = %q, want it left alone under the %s policy", got, config.RestorePinned)
	}

	// Naming one still works — that is the only thing that does.
	h.trigger("restore", theGoodWorld)
	h.poll()
	if got := h.m.doc.RestoreTarget; got != theGoodWorld || !h.m.doc.RestorePinned {
		t.Fatalf("restore_target = %q pinned=%v, want %q pinned", got, h.m.doc.RestorePinned, theGoodWorld)
	}

	// "latest" is not a refusal here: the newest archive is a perfectly good thing
	// to ask for. It simply does not become a standing instruction, and the log has
	// to say so rather than let an operator believe following is back on.
	h.trigger("restore", "latest")
	h.poll()
	if got := h.m.doc.RestoreTarget; got != theBrokenWorld {
		h.dumpLogs()
		t.Fatalf("restore_target = %q, want the newest %q", got, theBrokenWorld)
	}
	if !h.m.doc.RestorePinned {
		t.Fatalf("restore_pinned is false under the %s policy, where nothing follows anything", config.RestorePinned)
	}
	if !h.logged("will not move again") {
		h.dumpLogs()
		t.Fatal("the policy was applied without telling the operator it is one-shot")
	}
}

// TestRestorePolicyNoneRefusesARestore: a request that goes nowhere must not look
// like one that worked. Under this policy every start is a fresh world, so an
// operator whose restore was quietly dropped loses the one they asked for.
func TestRestorePolicyNoneRefusesARestore(t *testing.T) {
	t.Parallel()
	h := restoreHarness(t, config.RestoreNone)

	h.clk.add(time.Hour)
	h.takeBackup(theBrokenWorld)
	if got := h.m.doc.RestoreTarget; got != "" {
		t.Fatalf("restore_target = %q, want empty under the %s policy", got, config.RestoreNone)
	}

	h.trigger("restore", theGoodWorld)
	h.poll()
	if got := h.m.doc.RestoreTarget; got != "" {
		t.Fatalf("restore_target = %q, want the request refused", got)
	}
	if h.m.doc.LastError == "" {
		h.dumpLogs()
		t.Fatal("the refusal set no last_error, so the dashboard shows nothing")
	}
	if !h.logged("refused") {
		h.dumpLogs()
		t.Fatal("the refusal was not logged")
	}
}

// TestAVanishedRestoreTargetIsCleared is the redeploy case, and it was a live
// bug: closing a controller and starting a new one left a document naming an
// archive whose file died with the old container's ephemeral disk.
//
// Nothing else in the machine revisits a target once it is set. The existence check
// in consumeRestore guards the moment one is chosen and on rejection deliberately
// keeps what was already there, so the stale name survived every tick, every
// publish and every start — and a start is what turns it into a world that cannot
// boot: 404, permanent, restore_failed, failed, and the next start sends the same
// dead name again. Clearing it is the difference between a fresh world and a
// halt/restart loop that only a hand-pushed trigger can break.
func TestAVanishedRestoreTargetIsCleared(t *testing.T) {
	t.Parallel()
	h := restoreHarness(t, config.RestoreLatest)

	h.trigger("restore", theGoodWorld)
	h.poll()
	if got := h.m.doc.RestoreTarget; got != theGoodWorld || !h.m.doc.RestorePinned {
		t.Fatalf("restore_target = %q pinned = %v, want %q pinned", got, h.m.doc.RestorePinned, theGoodWorld)
	}

	// The disk the archive lived on is gone. seedIndex is what discovers this on a
	// replaced controller; here the index is emptied directly, because the fact the
	// machine acts on is the reconciled index and not how it was reconciled.
	h.m.idx = state.NewBackups()
	h.poll()

	if got := h.m.doc.RestoreTarget; got != "" {
		t.Fatalf("restore_target = %q, want it cleared — that name 404s and a 404 is permanent", got)
	}
	// The pin has to go with the target. Left set with nothing pinned it would
	// suppress the automatic follow, so the one path back to a working target — the
	// next successful backup — would never be taken.
	if h.m.doc.RestorePinned {
		t.Error("restore_pinned survived the archive it pinned; nothing would ever follow again")
	}
	if got := h.published().RestoreTarget; got != "" {
		t.Errorf("published restore_target = %q; the agent reads git, so an unpublished clear changes nothing", got)
	}
	if !h.logged("is gone from the index") {
		h.dumpLogs()
		t.Error("the target was cleared silently; an operator cannot tell why their restore stopped happening")
	}

	// And the way back works: the next backup becomes the target under this policy.
	h.clk.add(time.Hour)
	h.takeBackup(theBrokenWorld)
	if got := h.m.doc.RestoreTarget; got != theBrokenWorld {
		h.dumpLogs()
		t.Errorf("restore_target = %q after a fresh backup, want %q — following did not resume", got, theBrokenWorld)
	}
}

// TestAPresentRestoreTargetIsLeftAlone: the clear above runs on every event, so the
// case that must not move is the ordinary one. A target that is in the index is the
// operator's instruction and survives ticks, polls and publishes untouched.
func TestAPresentRestoreTargetIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := restoreHarness(t, config.RestoreLatest)

	h.trigger("restore", theGoodWorld)
	h.poll()
	h.poll()
	h.tick()
	h.clk.add(time.Hour)
	h.tick()

	if got := h.m.doc.RestoreTarget; got != theGoodWorld {
		h.dumpLogs()
		t.Errorf("restore_target = %q, want the pinned %q left alone", got, theGoodWorld)
	}
	if !h.m.doc.RestorePinned {
		t.Error("restore_pinned was cleared for an archive that is still in the index")
	}
	if h.logged("is gone from the index") {
		h.dumpLogs()
		t.Error("an archive that is present was reported as gone")
	}
}
