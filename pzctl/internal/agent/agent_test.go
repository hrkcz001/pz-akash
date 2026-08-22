package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// The step 4 gate: one agent, one fake controller, one fake PZ, and the whole
// lifecycle the container actually lives through — boot, online, a requested
// backup, a halt.
//
// It is deliberately one long test rather than four short ones. Three of the four
// v1 bugs were sequencing bugs, and a sequence can only be asserted end to end:
// that the count published while online is a measurement, that the archive
// answering a request contains the world as it was at that moment, that the same
// request is not answered twice, and that a halt is a halt.

func TestAgentLifecycle(t *testing.T) {
	h := newHarness(t)
	t.Setenv(fakePlayersEnv, "3")
	h.start()

	// --- boot ---

	h.waitPhase(state.PhaseOnline, 60*time.Second)

	// The .ini came from the controller with the passwords substituted in, and the
	// agent patched only the keys config.yaml owns. Both halves matter: losing the
	// passwords locks every player out, and not patching means config.yaml is not
	// the source of truth it claims to be.
	iniPath := filepath.Join(h.cfg.Agent.Paths.DataDir, "Server", h.cfg.Identity.ServerName+".ini")
	ini, err := os.ReadFile(iniPath)
	if err != nil {
		t.Fatalf("the .ini from server.zip is not on disk: %v", err)
	}
	for _, want := range []string{"Password=lobbyword", "RCONPassword=rconword", "AdminPassword=adminword", "ZombieConfig=hordes"} {
		if !strings.Contains(string(ini), want) {
			t.Errorf("%q is missing from %s:\n%s", want, iniPath, ini)
		}
	}
	if !strings.Contains(string(ini), "MaxPlayers=24") {
		t.Errorf("MaxPlayers was not taken from config.yaml:\n%s", ini)
	}
	// AdminPassword is in that file for the agent, not for PZ: PZ has no such .ini
	// key and keeps the admin account in the world's user database, reachable only
	// through -adminpassword. So the value has to make one more hop, from the .ini
	// onto the command line, or the world boots with no admin password — which is
	// what v1 shipped and what the substituted placeholder alone would still do.
	args := h.launchArgs()
	adminFlag := -1
	for i, a := range args {
		if a == "-adminpassword" {
			adminFlag = i
		}
	}
	if adminFlag < 0 || adminFlag+1 >= len(args) {
		t.Errorf("PZ was launched without -adminpassword and a value: %v", args)
	} else if got := args[adminFlag+1]; got != "adminword" {
		t.Errorf("-adminpassword got %q, want the value substituted into the .ini", got)
	}
	// common.zip 404'd, and boot continued anyway.
	if !h.sawLog("/common.zip is absent") {
		t.Error("boot did not report the absent common.zip; it must be optional, not fatal")
	}

	// The heap belongs in the launcher's JSON, because pzexe drops -Xmx from the
	// command line without saying so.
	js, err := os.ReadFile(filepath.Join(h.cfg.Agent.Paths.GameDir, "ProjectZomboid64.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(js), "-Xmx"+h.cfg.Server.MemoryMax) {
		t.Errorf("the launcher JSON was not patched with the heap:\n%s", js)
	}
	if !strings.Contains(string(js), steamOff) {
		t.Errorf("the launcher JSON still has Steam enabled:\n%s", js)
	}

	// ~/zomboid must reach ~/Zomboid. The game builds some internal paths in the
	// lowercase name whatever -cachedir says, so on ext4 the two are different
	// directories and the world splits in half. Asserted rather than assumed
	// because linkLowercase only logs a failed symlink — the right call on a
	// filesystem that has none, but it means nothing else would notice on the one
	// filesystem where it matters.
	if link := h.cfg.Agent.Paths.LowercaseLink; link != "" {
		dst, err := os.Readlink(link)
		if err != nil {
			t.Errorf("%s is not a symlink: %v", link, err)
		} else if dst != h.cfg.Agent.Paths.DataDir {
			t.Errorf("%s points at %s, want %s", link, dst, h.cfg.Agent.Paths.DataDir)
		}
	}

	// --- a measured player count (bug 1) ---

	h.waitFor("a real player count", 30*time.Second, func() bool {
		return h.agentDoc().PlayersCount == 3
	})
	doc := h.agentDoc()
	if doc.PlayersAt.Zero() {
		t.Error("players_at is unset on a measured count; the dashboard cannot tell how fresh it is")
	}
	// The stamp must carry the configured timezone's offset, not the host's. This is
	// the whole of the Prague requirement: the container's clock is UTC, and after a
	// JSON round trip the offset is all that survives of the zone.
	_, want := time.Now().In(h.cfg.Location()).Zone()
	if _, got := doc.PlayersAt.Time.Zone(); got != want {
		t.Errorf("players_at offset = %ds, want %ds — identity.timezone is the only clock", got, want)
	}

	// --- a requested backup (bug 4) ---

	// A world to archive, planted now so the archive's contents prove it was taken
	// from this directory and not from anywhere else.
	worldFile := filepath.Join(h.savesDir(), "Multiplayer", h.cfg.Identity.ServerName, "map_t.bin")
	writeFile(t, worldFile, "the world as of the operator's request")

	h.doc.BackupRequest = &state.BackupRequest{
		ID:          "req-operator-1",
		Reason:      "operator",
		RequestedAt: state.Now(h.cfg.Location()),
	}
	h.publish("backup requested")

	h.waitFor("the backup answering req-operator-1", 60*time.Second, func() bool {
		up, _ := h.uploadFor("req-operator-1")
		return up.Name != ""
	})
	first, count := h.uploadFor("req-operator-1")
	if count != 1 {
		t.Errorf("%d uploads, want 1", count)
	}
	if !state.IsBackupName(first.Name) {
		t.Errorf("upload name = %q, which the controller's index would reject", first.Name)
	}
	// The archive holds the contents of Saves, so a hand `unzip` into Saves restores
	// it — the shape v1 produced, kept for compatibility with archives the operator
	// already has.
	rel := "Multiplayer/" + h.cfg.Identity.ServerName + "/map_t.bin"
	body, ok := zipEntry(t, first.Body, rel)
	if !ok {
		t.Fatalf("the archive has no %s; entries are %v", rel, zipNames(t, first.Body))
	}
	if body != "the world as of the operator's request" {
		t.Errorf("%s = %q", rel, body)
	}
	// A save was asked for and confirmed before the archive was built. v1 zipped
	// the world with no save at all, so every backup it took was as stale as the
	// game's last autosave.
	if !h.sawLog("save: confirmed") {
		t.Error("the backup did not confirm a save first")
	}

	h.waitFor("the backup report", 30*time.Second, func() bool {
		d := h.agentDoc()
		return d.Backup != nil && d.Backup.State == state.BackupDone
	})
	doc = h.agentDoc()
	if doc.Backup.RequestID != "req-operator-1" {
		t.Errorf("report request_id = %q, want req-operator-1 — an unmatched report is what let v1 sign off a halt with a stale archive", doc.Backup.RequestID)
	}
	if doc.Backup.SHA256 != first.SHA256 || doc.Backup.SHA256 == "" {
		t.Errorf("report sha256 = %q, upload header said %q", doc.Backup.SHA256, first.SHA256)
	}
	if doc.Backup.Size != int64(len(first.Body)) {
		t.Errorf("report size = %d, the controller received %d bytes", doc.Backup.Size, len(first.Body))
	}
	if doc.Phase != state.PhaseOnline {
		t.Errorf("phase = %s after the backup, want online again", doc.Phase)
	}
	// The saving phase was entered and left. Only visible in the log, because the
	// controller may well fetch either side of it.
	if !h.sawLog("phase: online -> saving") {
		t.Error("the backup never reported the saving phase; the controller cannot tell a paused world from a live one")
	}

	// The request is still outstanding in the controller's document — that is by
	// design, it stays until the controller has read the answer. The agent must not
	// treat it as a second ask. In v1 this was the duplicate-backup loop.
	time.Sleep(6 * h.cfg.Agent.Reconcile.D())
	if _, n := h.uploadFor("req-operator-1"); n != 1 {
		t.Errorf("%d uploads after the request stayed outstanding, want 1", n)
	}

	// --- the halt (bug 2) ---

	// A real halt is both signals at once: back up the session, then stop. The
	// controller clears the answered request and files the halt's own.
	h.doc.BackupRequest = &state.BackupRequest{
		ID:          "req-halt-2",
		Reason:      "halt",
		RequestedAt: state.Now(h.cfg.Location()),
	}
	h.doc.Intent = state.IntentStopped
	h.publish("halt")

	h.waitFor("the halt's backup", 60*time.Second, func() bool {
		up, _ := h.uploadFor("req-halt-2")
		return up.Name != ""
	})
	second, count := h.uploadFor("req-halt-2")
	if count != 2 {
		t.Errorf("%d uploads in total, want 2", count)
	}
	if second.Name == first.Name {
		t.Errorf("both archives are called %q; the second would overwrite the first", second.Name)
	}
	if second.Phase == "" {
		t.Error("the upload carried no phase header; the controller cannot tell a halt archive from a periodic one")
	}

	// Only now may the server go down: the archive is the session, and stopping
	// first would lose it. The order is the assertion — checked once the halt has
	// run to completion, because the upload is recorded by the controller before
	// the agent has logged either line and sampling here would prove nothing.
	doc = h.waitPhase(state.PhaseStopped, 60*time.Second)

	if ok, both := h.logOrder("backup: "+second.Name+" uploaded", "-> stopping"); !both || !ok {
		t.Errorf("the halt stopped the server before the backup finished (uploaded-then-stopping: ok=%v both=%v)", ok, both)
	}
	if doc.PlayersCount != state.PlayersUnknown {
		t.Errorf("players_count = %d with no process running, want unknown", doc.PlayersCount)
	}
	if !h.sawLog("parked:") {
		t.Error("the agent did not park after the halt")
	}

	// The heart of bug 2. The container is still up, the loop is still running, and
	// the world must stay down: v1's entrypoint exited here, Kubernetes restarted
	// the pod, and the fresh container brought the world back up mid-halt — which
	// the controller read as a flap, and answered with another halt, another
	// webhook, and another backup.
	time.Sleep(8 * h.cfg.Agent.Reconcile.D())
	if n := h.launches(); n != 1 {
		t.Errorf("PZ was launched %d times, want exactly 1 — the halted world came back", n)
	}
	if doc := h.agentDoc(); doc.Phase != state.PhaseStopped {
		t.Errorf("phase = %s after several reconciles, want it to stay stopped", doc.Phase)
	}
	if _, n := h.uploadFor(""); n != 2 {
		t.Errorf("%d uploads after the halt settled, want 2", n)
	}

	// Cancellation is the container being torn down: Run returns nil, and only nil.
	// Checked inside stop().
	h.stop()
}
