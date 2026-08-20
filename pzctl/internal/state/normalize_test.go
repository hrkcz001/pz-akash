package state

import (
	"strings"
	"testing"
)

// Normalization is a postcondition of Unmarshal, not something callers opt into.
// These tests go through Unmarshal for exactly that reason: if the hook were
// removed, they would fail even though Normalize itself still worked.

func TestNormalizeDropsALeaseWithNoDSeq(t *testing.T) {
	c := NewController(prague(t))
	rep := Unmarshal([]byte(`{"status":"online","lease":{"gseq":1,"oseq":1}}`), c)

	if c.Lease != nil {
		t.Errorf("lease = %+v; a lease with no dseq is unqueryable and must be dropped", c.Lease)
	}
	if !strings.Contains(rep.String(), "reconcile") {
		t.Errorf("the repair should tell the operator to reconcile, got: %s", rep)
	}
	// Status is left alone: online with no lease is a contradiction, but resolving
	// it is the reconciler's job against Akash, not the decoder's guess.
	if c.Status != StatusOnline {
		t.Errorf("status = %q; normalization must not invent a lifecycle change", c.Status)
	}
}

func TestNormalizeUnusableStatusDependsOnTheLease(t *testing.T) {
	loc := prague(t)

	withLease := NewController(loc)
	Unmarshal([]byte(`{"status":"whatever","lease":{"dseq":"20603991"}}`), withLease)
	if withLease.Status != StatusFailed {
		t.Errorf("status = %q, want failed: something is billing and we cannot say what", withLease.Status)
	}

	noLease := NewController(loc)
	Unmarshal([]byte(`{"status":"whatever"}`), noLease)
	if noLease.Status != StatusOffline {
		t.Errorf("status = %q, want offline: nothing is leased", noLease.Status)
	}
}

func TestNormalizeDropsAnEmptyStopAt(t *testing.T) {
	c := NewController(prague(t))
	rep := Unmarshal([]byte(`{"stop_at":""}`), c)
	if c.StopAt != nil {
		t.Errorf("stop_at = %s; the zero time is in the past forever and would halt on every tick", c.StopAt)
	}
	if !strings.Contains(rep.String(), "stop_at") {
		t.Errorf("the repair should name stop_at, got: %s", rep)
	}
}

func TestNormalizeCleansTheDedupRing(t *testing.T) {
	c := NewController(prague(t))
	rep := Unmarshal([]byte(`{"processed_shas":["","a1","a1","","b2"]}`), c)
	if got, want := c.ProcessedSHAs, []string{"a1", "b2"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("processed_shas = %v, want %v", got, want)
	}
	if !strings.Contains(rep.String(), "processed_shas") {
		t.Errorf("the repair should name processed_shas, got: %s", rep)
	}
	// An empty SHA in the ring would make MarkProcessed("") look already-seen,
	// which is how a real webhook gets dropped.
	if c.WasProcessed("") {
		t.Error("the empty string is still in the ring")
	}
}

func TestNormalizeRejectsARestoreTargetThatIsNotAFilename(t *testing.T) {
	c := NewController(prague(t))
	// The agent turns this value into a path under backups.dir, so anything that
	// is not a plain backup filename is at best a failed boot.
	rep := Unmarshal([]byte(`{"restore_target":"../../etc/passwd"}`), c)
	if c.RestoreTarget != "" {
		t.Errorf("restore_target = %q, want it dropped", c.RestoreTarget)
	}
	if !strings.Contains(rep.String(), "restore_target") {
		t.Errorf("the repair should name restore_target, got: %s", rep)
	}
}

func TestNormalizeFlagsANewerDocumentWithoutManglingIt(t *testing.T) {
	c := NewController(prague(t))
	rep := Unmarshal([]byte(`{"version":99,"status":"online","lease":{"dseq":"20603991"}}`), c)
	if c.Version != 99 {
		t.Errorf("version = %d; a newer version must be preserved, not rewritten", c.Version)
	}
	if !strings.Contains(rep.String(), "read-only") {
		t.Errorf("the repair should mark the document read-only, got: %s", rep)
	}
	// The dseq has to survive, since dropping it is precisely how a rolling
	// upgrade would orphan a paid lease.
	if c.Lease == nil || c.Lease.DSeq != "20603991" {
		t.Errorf("lease = %+v, want dseq 20603991 preserved", c.Lease)
	}
}

// The structural half of the player-count fix: v1's own bytes must read as
// "unknown", because they were never a measurement.
func TestNormalizeRejectsAnUnstampedPlayerCount(t *testing.T) {
	loc := prague(t)
	for _, in := range []string{`{"players_count":0}`, `{"players_count":7}`} {
		a := NewAgent(loc)
		rep := Unmarshal([]byte(in), a)
		if a.PlayersKnown() {
			t.Errorf("%s decoded as the measurement %d", in, a.PlayersCount)
		}
		if !strings.Contains(rep.String(), "not a measurement") {
			t.Errorf("%s: the repair should explain why, got: %s", in, rep)
		}
	}

	// A properly stamped count is accepted unchanged.
	a := NewAgent(loc)
	rep := Unmarshal([]byte(`{"players_count":3,"players_at":"2026-08-19T01:36:23+02:00"}`), a)
	if !rep.OK() {
		t.Fatalf("a stamped count needed repairs: %s", rep)
	}
	if !a.PlayersKnown() || a.PlayersCount != 3 {
		t.Errorf("players_count = %d, known = %v", a.PlayersCount, a.PlayersKnown())
	}
}

func TestNormalizeUnusablePhaseParks(t *testing.T) {
	a := NewAgent(prague(t))
	rep := Unmarshal([]byte(`{"phase":"no-such-phase"}`), a)
	if a.Phase != PhaseCrashed {
		t.Errorf("phase = %q, want crashed", a.Phase)
	}
	if !a.Phase.Parked() {
		t.Error("an unusable phase must not leave the agent looking busy")
	}
	if a.Phase.ImpliedStatus() != "" {
		t.Errorf("an unusable phase implied the status %q", a.Phase.ImpliedStatus())
	}
	if !strings.Contains(rep.String(), "phase") {
		t.Errorf("the repair should name phase, got: %s", rep)
	}
}

// Bug 4's shape: a report that cannot be tied to a request will satisfy whatever
// request happens to be outstanding.
func TestNormalizeDropsAnUnkeyedBackupReport(t *testing.T) {
	loc := prague(t)
	a := NewAgent(loc)
	rep := Unmarshal([]byte(`{"backup":{"state":"done","name":"backup_20260819_013623.zip"}}`), a)
	if a.Backup != nil {
		t.Errorf("backup = %+v, want it dropped for having no request_id", a.Backup)
	}
	if !strings.Contains(rep.String(), "request_id") {
		t.Errorf("the repair should name request_id, got: %s", rep)
	}

	// Success with no filename is not success: the controller would go looking for
	// an archive that was never named.
	b := NewAgent(loc)
	Unmarshal([]byte(`{"backup":{"request_id":"r1","state":"done"}}`), b)
	if b.Backup == nil {
		t.Fatal("a keyed report should be kept")
	}
	if b.Backup.State != BackupFailed {
		t.Errorf("state = %q, want failed", b.Backup.State)
	}
	if b.Backup.Error == "" {
		t.Error("the downgrade should explain itself in the report")
	}

	// An unknown state is failed, not silently trusted.
	c := NewAgent(loc)
	Unmarshal([]byte(`{"backup":{"request_id":"r1","state":"probably-fine","name":"backup_20260819_013623.zip"}}`), c)
	if c.Backup == nil || c.Backup.State != BackupFailed {
		t.Errorf("backup = %+v, want state failed", c.Backup)
	}
}

func TestNormalizeIndexDropsJunkAndDerivesInstants(t *testing.T) {
	idx := NewBackups()
	raw := `{
	  "items": [
	    {"name": "backup_20260819_013623.zip", "size": 100},
	    {"name": "backup_20260819_013623.zip", "size": 100},
	    {"name": "world.zip", "size": 5},
	    {"name": "", "size": 0},
	    {"name": "backup_20260819_003520.zip", "size": -7}
	  ]
	}`
	rep := Unmarshal([]byte(raw), idx)

	if got := idx.Names(); len(got) != 2 {
		t.Fatalf("index = %v, want the two real backups", got)
	}
	if idx.Has("world.zip") || idx.Has("") {
		t.Errorf("junk survived: %v", idx.Names())
	}
	// Newest first, and derived from the filenames since the document carried no
	// created_at.
	if got := idx.Names()[0]; got != "backup_20260819_013623.zip" {
		t.Errorf("first = %s, want the newer name", got)
	}
	for _, e := range idx.Items {
		if e.CreatedAt.Zero() {
			t.Errorf("%s has no instant, so the age rule would never see it", e.Name)
		}
		if e.Size < 0 {
			t.Errorf("%s has size %d", e.Name, e.Size)
		}
	}
	for _, want := range []string{"duplicate", "world.zip", "derived"} {
		if !strings.Contains(rep.String(), want) {
			t.Errorf("repairs should mention %q, got: %s", want, rep)
		}
	}
}

// A read of a file that was never written still has to satisfy the invariants,
// or the very first tick after a fresh deploy would be operating on a document
// nothing had checked.
func TestNormalizeRunsOnEveryReadPath(t *testing.T) {
	loc := prague(t)

	// The fatal path.
	c := NewController(loc)
	c.Lease = &Lease{} // a caller-supplied default that is itself invalid
	rep := Unmarshal([]byte(liveCorruptServerInfo), c)
	if !rep.Fatal() {
		t.Fatalf("expected a fatal repair: %s", rep)
	}
	if c.Lease != nil {
		t.Error("normalization did not run on the fatal path")
	}

	// The empty-document path.
	d := NewController(loc)
	d.Status = Status("bogus")
	Unmarshal([]byte("   "), d)
	if !d.Status.Valid() {
		t.Errorf("normalization did not run on the empty-document path: status %q", d.Status)
	}

	// The missing-file path.
	e := NewController(loc)
	e.StopAt = &Stamp{}
	if _, err := ReadFileInto(t.TempDir()+"/absent.json", e); err != nil {
		t.Fatal(err)
	}
	if e.StopAt != nil {
		t.Error("normalization did not run on the missing-file path")
	}
}

// Normalize must be idempotent: it runs on every read, and a rule that kept
// appending repairs would fill the log with the same line forever.
func TestNormalizeIsIdempotent(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	first := &Repairs{}
	c.Lease = &Lease{}
	c.Status = Status("bogus")
	c.Intent = Intent("maybe")
	c.RestoreTarget = "nope"
	c.ProcessedSHAs = []string{"", "a", "a"}
	c.Normalize(first)
	if first.OK() {
		t.Fatal("expected repairs on the first pass")
	}

	second := &Repairs{}
	c.Normalize(second)
	if !second.OK() {
		t.Errorf("second pass still reported %s", second)
	}

	a := NewAgent(loc)
	a.Phase = Phase("bogus")
	a.PlayersCount = 4
	a.Restarts = -1
	a.Backup = &BackupReport{State: BackupDone}
	firstA := &Repairs{}
	a.Normalize(firstA)
	if firstA.OK() {
		t.Fatal("expected repairs on the first pass")
	}
	secondA := &Repairs{}
	a.Normalize(secondA)
	if !secondA.OK() {
		t.Errorf("second pass still reported %s", secondA)
	}

	idx := NewBackups()
	idx.Items = []Backup{{Name: "junk"}, {Name: "backup_20260819_013623.zip"}}
	firstI := &Repairs{}
	idx.Normalize(firstI)
	if firstI.OK() {
		t.Fatal("expected repairs on the first pass")
	}
	secondI := &Repairs{}
	idx.Normalize(secondI)
	if !secondI.OK() {
		t.Errorf("second pass still reported %s", secondI)
	}
}
