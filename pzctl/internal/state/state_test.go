package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func prague(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Europe/Prague")
	if err != nil {
		t.Fatalf("Europe/Prague unavailable: %v", err)
	}
	return loc
}

// --- transitions ---

func TestTransitionTableRejectsShortcuts(t *testing.T) {
	legal := [][2]Status{
		{StatusOffline, StatusDeploying},
		{StatusDeploying, StatusBooting},
		{StatusBooting, StatusOnline},
		{StatusOnline, StatusBackingUp},
		{StatusBackingUp, StatusOnline},
		{StatusOnline, StatusStopping},
		{StatusStopping, StatusClosing},
		{StatusClosing, StatusOffline},
		{StatusFailed, StatusOffline},
		{StatusOnline, StatusOnline}, // no-op
	}
	for _, tc := range legal {
		if !CanTransition(tc[0], tc[1]) {
			t.Errorf("%s -> %s should be legal", tc[0], tc[1])
		}
	}

	illegal := [][2]Status{
		{StatusOffline, StatusOnline},   // cannot skip the deploy
		{StatusOffline, StatusBooting},  // ditto
		{StatusClosing, StatusOnline},   // cannot un-close
		{StatusOffline, StatusClosing},  // nothing to close
		{StatusFailed, StatusOnline},    // must go through offline
		{StatusOnline, Status("weird")}, // unknown target
	}
	for _, tc := range illegal {
		if CanTransition(tc[0], tc[1]) {
			t.Errorf("%s -> %s should be illegal", tc[0], tc[1])
		}
	}
}

// Every status must be reachable from offline and able to get back to it,
// otherwise the table contains a state the system can enter and never leave.
func TestEveryStatusCanReachOffline(t *testing.T) {
	for _, from := range Statuses() {
		seen := map[Status]bool{from: true}
		queue := []Status{from}
		found := from == StatusOffline
		for len(queue) > 0 && !found {
			cur := queue[0]
			queue = queue[1:]
			for _, next := range transitions[cur] {
				if next == StatusOffline {
					found = true
					break
				}
				if !seen[next] {
					seen[next] = true
					queue = append(queue, next)
				}
			}
		}
		if !found {
			t.Errorf("%s cannot reach offline: the system could get stuck billing", from)
		}
	}
}

func TestSetStatusRejectsIllegalAndKeepsSince(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	if err := c.SetStatus(StatusOnline, Now(loc)); err == nil {
		t.Fatal("offline -> online should be rejected")
	}
	if c.Status != StatusOffline {
		t.Errorf("a rejected transition changed the status to %s", c.Status)
	}

	if err := c.SetStatus(StatusDeploying, Now(loc)); err != nil {
		t.Fatal(err)
	}
	since := c.Since
	// A repeat is a no-op that must not move Since, or every tick would reset
	// the clock a timeout is measured against.
	if err := c.SetStatus(StatusDeploying, Now(loc)); err != nil {
		t.Fatal(err)
	}
	if !c.Since.Time.Equal(since.Time) {
		t.Error("repeating a status moved Since")
	}
}

func TestBusyCoversEveryMultiStepStatus(t *testing.T) {
	// The halt-loop fix depends on this set: any status where an operation is
	// in flight must report Busy so duplicate triggers are dropped.
	for _, s := range []Status{StatusDeploying, StatusBooting, StatusBackingUp, StatusStopping, StatusClosing} {
		if !s.Busy() {
			t.Errorf("%s must be Busy", s)
		}
	}
	for _, s := range []Status{StatusOffline, StatusOnline, StatusFailed} {
		if s.Busy() {
			t.Errorf("%s must not be Busy", s)
		}
	}
}

func TestParkedPhasesAreTheTerminalOnes(t *testing.T) {
	for _, p := range []Phase{PhaseStopped, PhaseCrashed, PhaseRestoreFailed} {
		if !p.Parked() {
			t.Errorf("%s must be Parked: the agent may never exit and let the kubelet restart it", p)
		}
		if p.ImpliedStatus() != "" {
			t.Errorf("%s implies status %q; a parked phase means different things depending on intent",
				p, p.ImpliedStatus())
		}
	}
	for _, p := range []Phase{PhaseStarting, PhaseRestoring, PhaseOnline, PhaseSaving, PhaseStopping} {
		if p.Parked() {
			t.Errorf("%s must not be Parked", p)
		}
	}
}

// --- players count ---

// The bug was not that the count was wrong; it was that "unmeasured" and "zero
// players" were the same value. They must stay distinguishable through a round
// trip and through a decode of a document that omits the field.
func TestPlayersUnknownIsNotZero(t *testing.T) {
	loc := prague(t)
	a := NewAgent(loc)
	if a.PlayersKnown() {
		t.Error("a fresh agent must not claim to know the player count")
	}
	if a.PlayersCount == 0 {
		t.Error("unmeasured must not encode as 0")
	}

	a.SetPlayers(0, Now(loc))
	if !a.PlayersKnown() || a.PlayersCount != 0 {
		t.Error("a measured zero must be known and equal to 0")
	}
	if a.PlayersAt.Zero() {
		t.Error("a measured count must carry a timestamp")
	}

	a.SetPlayers(-5, Now(loc))
	if a.PlayersKnown() || a.PlayersCount != PlayersUnknown {
		t.Errorf("a negative measurement must normalise to unknown, got %d", a.PlayersCount)
	}
	if !a.PlayersAt.Zero() {
		t.Error("an unknown count must not carry a measurement timestamp")
	}

	// A document with no players_count keeps the default rather than becoming 0.
	fresh := NewAgent(loc)
	rep := Unmarshal([]byte(`{"phase":"online"}`), fresh)
	if !rep.OK() {
		t.Fatalf("unexpected repairs: %s", rep)
	}
	if fresh.PlayersKnown() {
		t.Errorf("omitted players_count decoded as the measurement %d", fresh.PlayersCount)
	}
}

// TestRestorePinWithoutATargetIsCleared covers the shape that would suppress the
// automatic follow forever while naming no archive: a server that never restores,
// reported as one that does. Both routes into it are checked, because the second is
// the one no writer intended — the target was dropped by the normalizer for not
// being a backup name, leaving the flag behind.
func TestRestorePinWithoutATargetIsCleared(t *testing.T) {
	loc := prague(t)

	c := NewController(loc)
	rep := Unmarshal([]byte(`{"restore_pinned":true}`), c)
	if c.RestorePinned {
		t.Error("a pin with no restore_target survived the read")
	}
	if !strings.Contains(rep.String(), "restore_pinned") {
		t.Errorf("the repair was silent: %s", rep)
	}

	c = NewController(loc)
	rep = Unmarshal([]byte(`{"restore_target":"not-a-backup","restore_pinned":true}`), c)
	if c.RestoreTarget != "" || c.RestorePinned {
		t.Errorf("target %q pinned=%v, want both cleared", c.RestoreTarget, c.RestorePinned)
	}
	if !rep.OK() && !strings.Contains(rep.String(), "restore_target") {
		t.Errorf("the dropped target was not reported: %s", rep)
	}

	// A real pin is left alone.
	c = NewController(loc)
	rep = Unmarshal([]byte(`{"restore_target":"backup_20260819_100500.zip","restore_pinned":true}`), c)
	if !rep.OK() {
		t.Fatalf("unexpected repairs: %s", rep)
	}
	if !c.RestorePinned || c.RestoreTarget != "backup_20260819_100500.zip" {
		t.Errorf("a well-formed pin was altered: target %q pinned=%v", c.RestoreTarget, c.RestorePinned)
	}
}

// --- codec: repair on read ---

// The exact bytes from the live repo. Offset 105 is the comma after the empty
// price_per_hour, which is the "column 106 (char 105)" the controller logged
// every polling cycle.
const liveCorruptServerInfo = `{"ip": "", "port": 2222, "game_port": 16261, "status": "stopping", "players_count": 0, "price_per_hour": , "price_per_day": }`

func TestUnmarshalOfTheLiveCorruptDocument(t *testing.T) {
	if got := strings.IndexByte(liveCorruptServerInfo[105:], ','); got != 0 {
		t.Fatalf("test fixture drifted: byte 105 is %q, not the offending comma",
			liveCorruptServerInfo[105])
	}

	var si LegacyServerInfo
	si.GamePort = 16261 // a caller-supplied default
	rep := Unmarshal([]byte(liveCorruptServerInfo), &si)

	if !rep.Fatal() {
		t.Fatalf("a document no parser accepts must be reported as fatal, got: %s", rep)
	}
	if si.GamePort != 16261 {
		t.Errorf("the caller's default was clobbered: game_port = %d", si.GamePort)
	}
	if !strings.Contains(rep.String(), "default") {
		t.Errorf("the repair should say the values fell back to defaults, got: %s", rep)
	}
}

func TestUnmarshalSalvagesSiblingsOfABadField(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	raw := `{
	  "version": 1,
	  "intent": "running",
	  "status": "online",
	  "endpoint": {"ip": "194.107.163.7", "game_port": "not-a-number", "udp_port": 16262},
	  "restore_target": "backup_20260819_013623.zip"
	}`
	rep := Unmarshal([]byte(raw), c)

	if rep.OK() {
		t.Fatal("the bad game_port should have been reported")
	}
	if rep.Fatal() {
		t.Fatalf("one bad leaf must not be fatal: %s", rep)
	}
	// Everything except the bad leaf survived.
	if c.Intent != IntentRunning {
		t.Errorf("intent = %q", c.Intent)
	}
	if c.Status != StatusOnline {
		t.Errorf("status = %q", c.Status)
	}
	if c.Endpoint.IP != "194.107.163.7" {
		t.Errorf("sibling ip lost: %q", c.Endpoint.IP)
	}
	if c.Endpoint.UDPPort != 16262 {
		t.Errorf("sibling udp_port lost: %d", c.Endpoint.UDPPort)
	}
	if c.Endpoint.GamePort != 0 {
		t.Errorf("the bad field should be its zero value, got %d", c.Endpoint.GamePort)
	}
	if c.RestoreTarget != "backup_20260819_013623.zip" {
		t.Errorf("restore_target lost: %q", c.RestoreTarget)
	}
	if !strings.Contains(rep.String(), "endpoint.game_port") {
		t.Errorf("the repair should name the field path, got: %s", rep)
	}
}

func TestUnmarshalReportsUnknownFieldsWithoutFailing(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	// The v1 key set, as it will appear during cutover.
	rep := Unmarshal([]byte(`{"status":"offline","players_count":0,"price_per_hour":0.011}`), c)
	if rep.Fatal() {
		t.Fatalf("unknown fields must not be fatal: %s", rep)
	}
	s := rep.String()
	for _, want := range []string{"players_count", "price_per_hour", "unknown field"} {
		if !strings.Contains(s, want) {
			t.Errorf("repairs should mention %q, got: %s", want, s)
		}
	}
	if c.Status != StatusOffline {
		t.Errorf("known fields must still decode, status = %q", c.Status)
	}
}

func TestUnmarshalNullKeepsDefault(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	c.Lease = &Lease{DSeq: "12345"}
	rep := Unmarshal([]byte(`{"lease": null, "intent": "running"}`), c)
	if !rep.OK() {
		t.Fatalf("unexpected repairs: %s", rep)
	}
	// An explicit null must not be mistaken for a zero-value lease, which would
	// read as "a deployment exists with dseq 0".
	if c.Lease == nil || c.Lease.DSeq != "12345" {
		t.Errorf("null overwrote the default lease: %+v", c.Lease)
	}
}

func TestUnmarshalRoundTrip(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	c.Intent = IntentRunning
	c.Status = StatusOnline
	c.Lease = &Lease{DSeq: "20603991", GSeq: 1, OSeq: 1, Provider: "akash1abc", CreatedAt: Now(loc)}
	c.Endpoint = Endpoint{IP: "194.107.163.7", GamePort: 16261, UDPPort: 16262}
	c.Price = Price{UAKTPerBlock: 198, AKTUSD: 1.05, USDPerHour: 0.011, USDPerDay: 0.26, QuotedAt: Now(loc)}
	c.URLs = URLs{Public: "https://vsrania.online", Raw: "http://provider:32167", Webhook: "https://vsrania.online/webhook"}
	c.RestoreTarget = "backup_20260819_013623.zip"
	stop := Now(loc)
	c.StopAt = &stop
	c.MarkProcessed("deadbeef")

	data, err := Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	back := NewController(loc)
	rep := Unmarshal(data, back)
	if !rep.OK() {
		t.Fatalf("a document we wrote ourselves needed repairs: %s", rep)
	}
	a, _ := json.Marshal(c)
	b, _ := json.Marshal(back)
	if string(a) != string(b) {
		t.Errorf("round trip changed the document:\n%s\n%s", a, b)
	}
}

// Timestamps must survive as instants and render in the configured zone, since
// that is what makes a state diff readable and retention correct.
func TestStampKeepsOffsetAndAcceptsUnixSeconds(t *testing.T) {
	loc := prague(t)
	s := At(time.Date(2026, 8, 19, 1, 36, 23, 0, loc))
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	if want := `"2026-08-19T01:36:23+02:00"`; string(data) != want {
		t.Errorf("marshal = %s, want %s", data, want)
	}
	var back Stamp
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !back.Time.Equal(s.Time) {
		t.Errorf("round trip moved the instant: %s vs %s", back, s)
	}

	// v1 wrote Unix seconds; those must still load.
	var legacy Stamp
	if err := json.Unmarshal([]byte("1787099745"), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Time.Unix() != 1787099745 {
		t.Errorf("unix seconds decoded to %s", legacy)
	}

	var empty Stamp
	if err := json.Unmarshal([]byte(`""`), &empty); err != nil {
		t.Fatal(err)
	}
	if !empty.Zero() {
		t.Error(`"" should decode to a zero stamp`)
	}
	if empty.Age() < 100*365*24*time.Hour {
		t.Error("a zero stamp must read as very stale, never as fresh")
	}
}

// --- atomic write ---

func TestWriteFileIsAtomicAndLeavesNoTemps(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	if err := WriteFile(path, []byte("first\n")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("second\n")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second\n" {
		t.Errorf("content = %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("temp files left behind: %v", names)
	}
}

func TestWriteFileCreatesMissingDirs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a", "b", "state.json")
	if err := WriteFile(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestReadFileIntoTreatsMissingAsUnwritten(t *testing.T) {
	loc := prague(t)
	c := NewController(loc)
	rep, err := ReadFileInto(filepath.Join(t.TempDir(), "nope.json"), c)
	if err != nil {
		t.Fatalf("a missing state file must not be an error: %v", err)
	}
	if !rep.OK() {
		t.Errorf("a missing file should need no repairs, got: %s", rep)
	}
	if c.Status != StatusOffline {
		t.Errorf("defaults lost: %q", c.Status)
	}
}

// --- processed SHA ring ---

func TestProcessedSHARingDedupsAndBounds(t *testing.T) {
	c := NewController(prague(t))
	if !c.MarkProcessed("aaa") {
		t.Fatal("first sighting should be new")
	}
	if c.MarkProcessed("aaa") {
		t.Fatal("a redelivery must be reported as already processed")
	}
	if c.MarkProcessed("") {
		t.Fatal("an empty SHA must never be recorded")
	}

	for i := 0; i < ProcessedSHACap+20; i++ {
		c.MarkProcessed(string(rune('a'+i%26)) + string(rune('0'+i/26)) + "-x")
	}
	if len(c.ProcessedSHAs) > ProcessedSHACap {
		t.Errorf("ring grew to %d, cap is %d", len(c.ProcessedSHAs), ProcessedSHACap)
	}
	// The most recent entry must still be there; eviction takes from the front.
	last := c.ProcessedSHAs[len(c.ProcessedSHAs)-1]
	if !c.WasProcessed(last) {
		t.Error("the newest SHA was evicted")
	}
}
