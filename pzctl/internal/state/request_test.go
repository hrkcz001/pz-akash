package state

import (
	"testing"
	"time"
)

// The request/report pair is the fix for bug 4, and its whole value is in the ID.
// v1 had none: a boolean said a backup was wanted, and the next archive to appear
// satisfied whatever was outstanding — so a halt could be signed off by a periodic
// backup that had started minutes before the halt did, and the world that got
// saved was not the world that had just been stopped.
//
// Every test here is about identity or about the request outliving a restart.

func TestRequestBackupNeedsAnID(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	c := NewController(loc)

	// A request with no ID could never be matched to a report, so it would keep a
	// halt waiting forever. Refusing to publish one is better than publishing it.
	if got := c.RequestBackup("", "periodic", Now(loc)); got != nil {
		t.Errorf("an unidentified request was accepted: %+v", got)
	}
	if c.BackupRequest != nil {
		t.Error("an unidentified request reached the document")
	}
}

func TestRequestBackupDoesNotOverwriteOneInFlight(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	c := NewController(loc)
	at := At(time.Date(2026, 8, 19, 10, 0, 0, 0, loc))

	first := c.RequestBackup("req1", "periodic", at)
	if first == nil {
		t.Fatal("the first request was refused")
	}

	// This is the halt-adopts-the-in-flight-backup case. Overwriting would orphan
	// the agent's work: the report it eventually pushes carries req1, would match
	// nothing, and the halt would wait out its timeout over a backup that had
	// already been taken.
	second := c.RequestBackup("req2", "halt", At(at.Add(time.Minute)))
	if second == nil {
		t.Fatal("the second request returned nil rather than the outstanding one")
	}
	if second.ID != "req1" {
		t.Errorf("outstanding request is now %q, want req1 kept", second.ID)
	}
	if second.Reason != "periodic" {
		t.Errorf("reason = %q, want the original periodic", second.Reason)
	}
	if !second.RequestedAt.Time.Equal(at.Time) {
		t.Error("RequestedAt moved, so the request's age — and its timeout — reset")
	}
}

func TestBackupAnswerRequiresAMatchingID(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	at := Now(loc)

	for _, tc := range []struct {
		name      string
		request   string // "" for none outstanding
		report    string // "" for no report at all
		state     BackupState
		wantMatch bool
	}{
		{name: "matching", request: "req1", report: "req1", state: BackupDone, wantMatch: true},
		{name: "matching failure", request: "req1", report: "req1", state: BackupFailed, wantMatch: true},
		{name: "stale report", request: "req2", report: "req1", state: BackupDone},
		{name: "no request outstanding", report: "req1", state: BackupDone},
		{name: "no report yet", request: "req1"},
	} {
		c := NewController(loc)
		if tc.request != "" {
			c.RequestBackup(tc.request, "halt", at)
		}
		a := NewAgent(loc)
		if tc.report != "" {
			a.Backup = &BackupReport{RequestID: tc.report, State: tc.state, EndedAt: at}
		}

		got, matched := c.BackupAnswer(a)
		if matched != tc.wantMatch {
			t.Errorf("%s: matched = %v, want %v", tc.name, matched, tc.wantMatch)
			continue
		}
		if matched && got != tc.state {
			t.Errorf("%s: state = %q, want %q", tc.name, got, tc.state)
		}
	}

	// A nil agent document — the agent has never reported — answers nothing rather
	// than panicking. The controller calls this on every pass, including the ones
	// before the container exists.
	c := NewController(loc)
	c.RequestBackup("req1", "halt", at)
	if _, matched := c.BackupAnswer(nil); matched {
		t.Error("a nil agent document answered the request")
	}
}

func TestBackupRequestAgeMeasuresTheOutstandingWait(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	start := time.Date(2026, 8, 19, 10, 0, 0, 0, loc)
	c := NewController(loc)
	req := c.RequestBackup("req1", "halt", At(start))

	if got := req.Age(start.Add(7 * time.Minute)); got != 7*time.Minute {
		t.Errorf("Age = %s, want 7m — the halt timeout is measured against this", got)
	}

	// A nil request and an unstamped one both answer zero, so a caller comparing
	// Age against a timeout cannot accidentally time out something that has not
	// been asked for. A negative would be worse than wrong: it would make the
	// timeout unreachable.
	var none *BackupRequest
	if got := none.Age(start); got != 0 {
		t.Errorf("nil request Age = %s, want 0", got)
	}
	if got := (&BackupRequest{ID: "x"}).Age(start); got != 0 {
		t.Errorf("unstamped request Age = %s, want 0", got)
	}
}

func TestClearBackupRequestIsIdempotent(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	at := Now(loc)
	c := NewController(loc)
	c.RequestBackup("req1", "operator", at)

	c.ClearBackupRequest(at)
	if c.BackupRequest != nil {
		t.Fatal("the request survived being cleared")
	}
	// Called again on the next pass, which happens whenever a halt re-enters
	// beginClose. It must not fail, and must not stamp a document it did not
	// change — an UpdatedAt that moves every tick makes the state branch's history
	// unreadable.
	before := c.UpdatedAt
	c.ClearBackupRequest(At(at.Add(time.Hour)))
	if !c.UpdatedAt.Time.Equal(before.Time) {
		t.Error("clearing nothing still stamped the document")
	}
}

// TestBackupRequestSurvivesARoundTrip is what makes the ask recoverable: the
// controller may be redeployed while a backup is running, and the request has to
// still be there — with the same ID — when it comes back.
func TestBackupRequestSurvivesARoundTrip(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	at := At(time.Date(2026, 8, 19, 10, 0, 0, 0, loc))
	c := NewController(loc)
	c.RequestBackup("req1", "halt", at)
	if err := c.SetStatus(StatusDeploying, at); err != nil {
		t.Fatal(err)
	}

	raw, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	back := NewController(loc)
	if rep := Unmarshal(raw, back); !rep.OK() {
		t.Fatalf("round trip needed repairs: %s", rep)
	}
	if back.BackupRequest == nil {
		t.Fatal("the outstanding request did not survive; the halt would wait forever")
	}
	if back.BackupRequest.ID != "req1" || back.BackupRequest.Reason != "halt" {
		t.Errorf("request came back as %+v", back.BackupRequest)
	}
	if !back.BackupRequest.RequestedAt.Time.Equal(at.Time) {
		t.Errorf("RequestedAt came back as %s, want %s — the age would restart",
			back.BackupRequest.RequestedAt.Time, at.Time)
	}
}

// TestMutatorsUseTheSuppliedInstant pins the API decision that the state package
// owns no clock. The controller measures its timeouts as "now minus a stamp in
// this document", so a document that stamped itself from time.Now while the
// machine measured from an injected clock would give two answers to one question —
// and every timeout would be untestable.
func TestMutatorsUseTheSuppliedInstant(t *testing.T) {
	t.Parallel()
	loc := prague(t)
	// Deliberately not the wall clock, and in the past, so a stray time.Now inside
	// any of these shows up as a wildly wrong stamp rather than a near miss.
	at := At(time.Date(2020, 1, 2, 3, 4, 5, 0, loc))

	c := NewController(loc)
	if err := c.SetStatus(StatusDeploying, at); err != nil {
		t.Fatal(err)
	}
	if !c.Since.Time.Equal(at.Time) || !c.UpdatedAt.Time.Equal(at.Time) {
		t.Errorf("SetStatus stamped %s/%s, want %s", c.Since.Time, c.UpdatedAt.Time, at.Time)
	}

	req := c.RequestBackup("req1", "periodic", at)
	if !req.RequestedAt.Time.Equal(at.Time) {
		t.Errorf("RequestBackup stamped %s, want %s", req.RequestedAt.Time, at.Time)
	}

	c.Fail(errShouldBeRecorded, at)
	if !c.Since.Time.Equal(at.Time) || c.LastError != errShouldBeRecorded.Error() {
		t.Errorf("Fail stamped %s / recorded %q", c.Since.Time, c.LastError)
	}

	a := NewAgent(loc)
	a.SetPhase(PhaseOnline, at)
	if !a.Since.Time.Equal(at.Time) || !a.LivenessAt.Time.Equal(at.Time) {
		t.Errorf("SetPhase stamped %s/%s", a.Since.Time, a.LivenessAt.Time)
	}
	a.SetPlayers(4, at)
	if !a.PlayersAt.Time.Equal(at.Time) {
		t.Errorf("SetPlayers stamped %s", a.PlayersAt.Time)
	}
}

// errShouldBeRecorded is a package-level value so the test above can compare
// against it without restating the string.
var errShouldBeRecorded = errTest("the provider had no bids")

type errTest string

func (e errTest) Error() string { return string(e) }
