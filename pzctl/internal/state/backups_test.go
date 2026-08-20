package state

import (
	"fmt"
	"io/fs"
	"strings"
	"testing"
	"time"
)

func TestNewBackupNameUsesPragueWallClock(t *testing.T) {
	loc := prague(t)
	// 23:36 UTC on 18 August is already 01:36 on the 19th in Prague. v1 named
	// this file backup_20260818_233623.zip; the whole point of the change is that
	// it is now named for the night the operator remembers.
	utc := time.Date(2026, 8, 18, 23, 36, 23, 0, time.UTC)
	if got, want := NewBackupName(utc, loc, nil), "backup_20260819_013623.zip"; got != want {
		t.Errorf("NewBackupName = %s, want %s", got, want)
	}

	// Winter, to prove the offset is looked up per-instant and not fixed at +2.
	winter := time.Date(2026, 1, 15, 23, 36, 23, 0, time.UTC)
	if got, want := NewBackupName(winter, loc, nil), "backup_20260116_003623.zip"; got != want {
		t.Errorf("NewBackupName (CET) = %s, want %s", got, want)
	}
}

func TestNewBackupNameAvoidsCollisions(t *testing.T) {
	loc := prague(t)
	idx := NewBackups()
	base := time.Date(2026, 8, 19, 1, 36, 23, 0, loc)

	first := NewBackupName(base, loc, idx.Has)
	idx.Upsert(Backup{Name: first, CreatedAt: At(base)})
	second := NewBackupName(base, loc, idx.Has)
	if second == first {
		t.Fatalf("two backups in the same second got the same name %s", first)
	}
	if second != "backup_20260819_013624.zip" {
		t.Errorf("second name = %s, want the next second", second)
	}
}

func TestParseBackupName(t *testing.T) {
	loc := prague(t)
	got, err := ParseBackupName("backup_20260819_013623.zip", loc)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 8, 19, 1, 36, 23, 0, loc); !got.Equal(want) {
		t.Errorf("ParseBackupName = %s, want %s", got, want)
	}
	for _, bad := range []string{"", "backup.zip", "backup_20260819.zip", "world.zip", "backup_20260819_013623.tar"} {
		if _, err := ParseBackupName(bad, loc); err == nil {
			t.Errorf("ParseBackupName(%q) should have failed", bad)
		}
		if IsBackupName(bad) {
			t.Errorf("IsBackupName(%q) = true", bad)
		}
	}
}

// The autumn rollback is the case the design comment warns about: the later of
// two backups can carry the earlier-sorting name, because Prague repeats
// 02:00–03:00. Sorting on CreatedAt has to get it right anyway.
func TestSortIsCorrectAcrossTheDSTRollback(t *testing.T) {
	loc := prague(t)
	// Prague shifts back at 01:00 UTC on 2026-10-25.
	early := time.Date(2026, 10, 25, 0, 45, 0, 0, time.UTC) // 02:45 CEST
	late := time.Date(2026, 10, 25, 1, 30, 0, 0, time.UTC)  // 02:30 CET, 45 min later

	idx := NewBackups()
	nameEarly := NewBackupName(early, loc, idx.Has)
	idx.Upsert(Backup{Name: nameEarly, CreatedAt: At(early.In(loc))})
	nameLate := NewBackupName(late, loc, idx.Has)
	idx.Upsert(Backup{Name: nameLate, CreatedAt: At(late.In(loc))})

	// Confirm the hazard is real, so the assertion below is not vacuous.
	if nameLate >= nameEarly {
		t.Fatalf("fixture no longer exercises the rollback: %s then %s", nameEarly, nameLate)
	}
	if got := idx.Names(); got[0] != nameLate {
		t.Errorf("newest first gave %v; sorting must follow CreatedAt, not the name", got)
	}
	// And retention must agree: a count of 1 expires the older instant, which is
	// the one with the *later*-sorting name.
	if got := idx.Expired(RetentionPolicy{Count: 1}, late.Add(time.Hour)); len(got) != 1 || got[0] != nameEarly {
		t.Errorf("expired %v, want [%s]", got, nameEarly)
	}
}

func TestExpiredNeverEmptiesTheDirectory(t *testing.T) {
	loc := prague(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, loc)
	idx := NewBackups()
	// Ten archives, one per day going back, so every one but the first is older
	// than a 7-day policy.
	for i := 0; i < 10; i++ {
		when := now.AddDate(0, 0, -i*2)
		idx.Upsert(Backup{Name: NewBackupName(when, loc, idx.Has), CreatedAt: At(when)})
	}

	// A policy that would delete everything still keeps the newest.
	doomed := idx.Expired(RetentionPolicy{Days: 1, Count: 1}, now)
	if len(doomed) != len(idx.Items)-1 {
		t.Errorf("expired %d of %d, want all but the newest", len(doomed), len(idx.Items))
	}
	newest := idx.Names()[0]
	for _, n := range doomed {
		if n == newest {
			t.Fatalf("the newest archive %s was expired; that loses the world", n)
		}
	}
	// Oldest first, so a partial delete leaves the index consistent.
	if len(doomed) > 1 && !(doomed[0] < doomed[len(doomed)-1]) {
		t.Errorf("expired list is not oldest-first: %v", doomed)
	}
}

func TestExpiredHonoursProtect(t *testing.T) {
	loc := prague(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, loc)
	idx := NewBackups()
	var names []string
	for i := 0; i < 5; i++ {
		when := now.AddDate(0, 0, -i*3)
		n := NewBackupName(when, loc, idx.Has)
		names = append(names, n)
		idx.Upsert(Backup{Name: n, CreatedAt: At(when)})
	}
	// names[4] is the oldest and would certainly expire; protecting it as the
	// restore target must save it.
	target := names[4]
	for _, n := range idx.Expired(RetentionPolicy{Days: 1, Count: 2, Protect: []string{target}}, now) {
		if n == target {
			t.Fatalf("%s is the restore target and was expired anyway", n)
		}
	}
}

func TestExpiredIgnoresEntriesWithNoInstant(t *testing.T) {
	loc := prague(t)
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, loc)
	idx := NewBackups()
	idx.Upsert(Backup{Name: "backup_20260819_120000.zip", CreatedAt: At(now)})
	// An imported v1 name whose instant could not be recovered. The age rule must
	// not treat a zero stamp as "infinitely old" and delete it; only the count
	// rule may.
	idx.Upsert(Backup{Name: "backup_20260101_000000.zip"})

	for _, n := range idx.Expired(RetentionPolicy{Days: 7}, now) {
		if n == "backup_20260101_000000.zip" {
			t.Error("an entry with no recorded instant was expired by the age rule")
		}
	}
	if got := idx.Expired(RetentionPolicy{Count: 1}, now); len(got) != 1 {
		t.Errorf("the count rule should still apply, got %v", got)
	}
}

func TestUpsertPreservesDownloadedAt(t *testing.T) {
	loc := prague(t)
	name := "backup_20260819_013623.zip"
	idx := NewBackups()
	idx.Upsert(Backup{Name: name, CreatedAt: Now(loc), DownloadedAt: Now(loc)})
	// A rebuild from disk knows the size but not that it was already served.
	idx.Upsert(Backup{Name: name, Size: 4096, CreatedAt: Now(loc)})

	e := idx.Find(name)
	if e == nil {
		t.Fatal("entry vanished")
	}
	if e.Size != 4096 {
		t.Errorf("size = %d, want 4096", e.Size)
	}
	if e.DownloadedAt.Zero() {
		t.Error("DownloadedAt was lost, so the dashboard would call a saved backup unsaved")
	}
	if len(idx.Undownloaded()) != 0 {
		t.Errorf("Undownloaded = %v, want empty", idx.Undownloaded())
	}
}

func TestTotalBytesAndRemove(t *testing.T) {
	loc := prague(t)
	idx := NewBackups()
	idx.Upsert(Backup{Name: "backup_20260819_013623.zip", Size: 100, CreatedAt: Now(loc)})
	idx.Upsert(Backup{Name: "backup_20260819_003520.zip", Size: 200, CreatedAt: Now(loc)})
	if got := idx.TotalBytes(); got != 300 {
		t.Errorf("TotalBytes = %d, want 300", got)
	}
	if !idx.Remove("backup_20260819_013623.zip") {
		t.Error("Remove reported nothing removed")
	}
	if idx.Remove("backup_20260819_013623.zip") {
		t.Error("Remove of an absent name reported success")
	}
	if got := idx.TotalBytes(); got != 200 {
		t.Errorf("TotalBytes after remove = %d, want 200", got)
	}
}

// --- stop_at ---

func TestParseStopAtInterpretsBareTimesLocally(t *testing.T) {
	loc := prague(t)
	cases := []struct {
		in   string
		want time.Time
	}{
		// The v1 bug: this was stamped UTC, so "stop at 23:00" fired at 01:00.
		{"2026-08-19 23:00:00", time.Date(2026, 8, 19, 23, 0, 0, 0, loc)},
		{"2026-08-19 23:00", time.Date(2026, 8, 19, 23, 0, 0, 0, loc)},
		{"2026-08-19T23:00:00", time.Date(2026, 8, 19, 23, 0, 0, 0, loc)},
		// An explicit offset always wins over the configured zone.
		{"2026-08-19T23:00:00Z", time.Date(2026, 8, 19, 23, 0, 0, 0, time.UTC)},
		{"1787099745", time.Unix(1787099745, 0)},
	}
	for _, tc := range cases {
		got, err := ParseStopAt(tc.in, loc)
		if err != nil {
			t.Errorf("ParseStopAt(%q): %v", tc.in, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("ParseStopAt(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
	for _, bad := range []string{"", "tomorrow", "23:00", "2026-13-45 99:99",
		"0001-01-01 0:00", "0"} { // the zero time, which would halt on every tick
		if _, err := ParseStopAt(bad, loc); err == nil {
			t.Errorf("ParseStopAt(%q) should have failed", bad)
		}
	}
}

// --- legacy import ---

// mapFetcher serves an in-memory file set, so an import test does not depend on
// the live repo's contents. Absence is reported the way a Fetcher over a
// directory or a git ref reports it, since ImportLegacy distinguishes "not there"
// from "could not be read".
func mapFetcher(files map[string]string) Fetcher {
	return func(path string) ([]byte, error) {
		v, ok := files[path]
		if !ok {
			return nil, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
		}
		return []byte(v), nil
	}
}

// The live pz-saves file set as of the cutover, corrupt server_info.json and
// drifted restore_target included.
func liveLegacyFiles() map[string]string {
	return map[string]string{
		"server_info.json":     liveCorruptServerInfo,
		"controller_info.json": `{"storage_url": "https://vsrania.online", "raw_storage_url": "http://provider.example:32167", "webhook_url": "https://vsrania.online/webhook", "updated_at": 1787099745}`,
		"backup_log":           "backup_20260819_013623.zip\nbackup_20260819_003520.zip\n",
		"desired_state":        "stopped",
		"restore_target":       "backup_20260819_013704.zip",
		"request_restore":      "requested",
		"start":                "",
		"active_dseq":          "20603991",
	}
}

func TestImportLegacyOfTheLiveRepo(t *testing.T) {
	loc := prague(t)
	c, idx, rep := ImportLegacy(mapFetcher(liveLegacyFiles()), loc)

	// The corrupt server_info.json means the status is unknown while a dseq is
	// live. That has to surface as failed, not offline: offline would tell the
	// FSM there is nothing to reconcile while the escrow keeps draining.
	if c.Status != StatusFailed {
		t.Errorf("status = %q, want failed (a live dseq with an unreadable status)", c.Status)
	}
	if c.Lease == nil || c.Lease.DSeq != "20603991" {
		t.Errorf("lease = %+v, want dseq 20603991", c.Lease)
	}
	if c.Intent != IntentStopped {
		t.Errorf("intent = %q, want stopped", c.Intent)
	}

	// The drifted restore target names a file backup_log never listed, so it must
	// not become a boot instruction.
	if c.RestoreTarget != "" {
		t.Errorf("restore_target = %q; it is absent from backup_log and must be dropped", c.RestoreTarget)
	}

	if got := idx.Names(); len(got) != 2 || got[0] != "backup_20260819_013623.zip" {
		t.Errorf("index = %v, want the two logged names newest first", got)
	}
	// Imported instants are derived from the UTC filenames v1 wrote, then shown
	// in Prague.
	if e := idx.Find("backup_20260819_013623.zip"); e == nil || e.CreatedAt.Zero() {
		t.Error("imported entry has no instant")
	} else if _, off := e.CreatedAt.Time.Zone(); off != 7200 {
		t.Errorf("imported instant offset = %ds, want CEST +7200", off)
	}

	s := rep.String()
	for _, want := range []string{
		"players_count", // discarded, not laundered
		"restore_target",
		"start",              // unconsumed trigger, reported not acted on
		"server_info.json",   // the fatal parse
		"backup_20260819_01", // the drift, named in the message
	} {
		if !strings.Contains(s, want) {
			t.Errorf("repairs should mention %q; got: %s", want, s)
		}
	}
	if !rep.Fatal() {
		t.Error("the corrupt server_info.json should be reported as a fatal read")
	}
}

func TestImportLegacyDiscardsPlayersCount(t *testing.T) {
	loc := prague(t)
	files := liveLegacyFiles()
	// A well-formed v1 document, still hardcoding the count.
	files["server_info.json"] = `{"ip": "194.107.163.7", "port": 2222, "game_port": 16261, "status": "online", "players_count": 0, "price_per_hour": 0.011, "price_per_day": 0.26}`
	c, _, rep := ImportLegacy(mapFetcher(files), loc)

	if c.Status != StatusOnline {
		t.Errorf("status = %q, want online", c.Status)
	}
	if c.Endpoint.IP != "194.107.163.7" || c.Endpoint.GamePort != 16261 {
		t.Errorf("endpoint = %+v", c.Endpoint)
	}
	if c.Endpoint.RCONPort != 0 {
		t.Errorf("rcon_port = %d; v1's `port` was sshd and must not be reused", c.Endpoint.RCONPort)
	}
	if c.Price.USDPerHour != 0.011 || c.Price.QuotedAt.Zero() {
		t.Errorf("price = %+v", c.Price)
	}
	// The Controller document has nowhere to put a player count by construction —
	// it lives on the agent branch. This asserts the repair note exists so the
	// operator knows the dashboard will read "unknown" until the agent reports.
	if !strings.Contains(rep.String(), "never a measurement") {
		t.Errorf("repairs should explain why players_count was dropped; got: %s", rep)
	}
}

func TestImportLegacyDropsPendingSentinelAndHaltWins(t *testing.T) {
	loc := prague(t)
	files := liveLegacyFiles()
	files["server_info.json"] = `{"ip": "pending", "game_port": 16261, "status": "booting", "players_count": 0}`
	files["desired_state"] = "running"
	files["halt"] = ""
	c, _, rep := ImportLegacy(mapFetcher(files), loc)

	if c.Endpoint.IP != "" {
		t.Errorf("ip = %q, want empty", c.Endpoint.IP)
	}
	if c.Endpoint.Ready() {
		t.Error("an endpoint with no IP must not report Ready")
	}
	// A halt file outranks desired_state: it is the more recent operator action,
	// and guessing "running" would redeploy during a shutdown.
	if c.Intent != IntentStopped {
		t.Errorf("intent = %q, want stopped when a halt trigger is present", c.Intent)
	}
	if !strings.Contains(rep.String(), "pending") {
		t.Errorf("repairs should note the dropped sentinel; got: %s", rep)
	}
}

func TestImportLegacyOfAnEmptyRepo(t *testing.T) {
	loc := prague(t)
	c, idx, rep := ImportLegacy(mapFetcher(map[string]string{}), loc)
	// Nothing on disk and nothing leased is genuinely offline; this is the only
	// case where the import may assume it.
	if c.Status != StatusOffline {
		t.Errorf("status = %q, want offline", c.Status)
	}
	if c.Lease != nil {
		t.Errorf("lease = %+v, want nil", c.Lease)
	}
	if c.Intent != IntentStopped {
		t.Errorf("intent = %q, want stopped", c.Intent)
	}
	if len(idx.Items) != 0 {
		t.Errorf("index = %v, want empty", idx.Names())
	}
	if rep.Fatal() {
		t.Errorf("an empty repo is not a corrupt one: %s", rep)
	}
}

func TestImportLegacyKeepsAValidRestoreTarget(t *testing.T) {
	loc := prague(t)
	files := liveLegacyFiles()
	files["restore_target"] = "backup_20260819_003520.zip" // present in backup_log
	c, _, _ := ImportLegacy(mapFetcher(files), loc)
	if c.RestoreTarget != "backup_20260819_003520.zip" {
		t.Errorf("restore_target = %q, want the indexed name kept", c.RestoreTarget)
	}
}

func TestImportLegacyParsesStopAtInPrague(t *testing.T) {
	loc := prague(t)
	files := liveLegacyFiles()
	files["stop_at"] = "2026-08-19 23:00:00"
	c, _, _ := ImportLegacy(mapFetcher(files), loc)
	if c.StopAt == nil {
		t.Fatal("stop_at was dropped")
	}
	if want := time.Date(2026, 8, 19, 23, 0, 0, 0, loc); !c.StopAt.Time.Equal(want) {
		t.Errorf("stop_at = %s, want %s", c.StopAt, want)
	}
}

// --- fuzz ---

// FuzzUnmarshalController is the corrupt-input gate. The contract is not that
// arbitrary bytes decode to anything sensible — it is that they never panic and
// never leave the document in a state the FSM would act on. v1's failure mode was
// exactly this: one malformed byte, and two features silently stopped.
func FuzzUnmarshalController(f *testing.F) {
	loc := time.UTC
	f.Add(liveCorruptServerInfo)
	f.Add(`{}`)
	f.Add(``)
	f.Add(`null`)
	f.Add(`[]`)
	f.Add(`{"status": "online"`)
	f.Add(`{"lease": {}}`) // found by the fuzzer: a lease with no dseq
	f.Add(`{"lease": {"dseq": "20603991", "gseq": "x"}}`)
	f.Add(`{"since": "not-a-time", "stop_at": 1787099745}`)
	f.Add(`{"stop_at": ""}`)
	f.Add(`{"processed_shas": ["", "", "a", "a"]}`)
	f.Add(`{"processed_shas": "not-a-list"}`)
	f.Add(`{"version": 99999999999999999999}`)
	f.Add(`{"endpoint": {"ip": {"nested": true}}}`)
	f.Add(`{"restore_target": "../../etc/passwd"}`)
	if data, err := Marshal(NewController(loc)); err == nil {
		f.Add(string(data))
	}

	f.Fuzz(func(t *testing.T, in string) {
		c := NewController(loc)
		rep := Unmarshal([]byte(in), c)
		if rep == nil {
			t.Fatal("Unmarshal returned a nil Repairs")
		}
		_ = rep.String() // must not panic on any repair it produced

		// Whatever came out has to be safe to act on. A status the transition
		// table does not know would wedge the FSM, so a bad one must have been
		// rejected and left at a usable value.
		if !c.Status.Valid() {
			t.Fatalf("input %q produced the unusable status %q", in, c.Status)
		}
		if !c.Intent.Valid() {
			t.Fatalf("input %q produced the unusable intent %q", in, c.Intent)
		}
		// A lease is money. If one is present its dseq must be non-empty, or the
		// reconciler would query Akash for deployment "".
		if c.Lease != nil && c.Lease.DSeq == "" {
			t.Fatalf("input %q produced a lease with no dseq: %+v", in, c.Lease)
		}
		// A zero StopAt is in the past forever, so it would halt on every tick.
		if c.StopAt != nil && c.StopAt.Zero() {
			t.Fatalf("input %q produced an empty stop_at", in)
		}
		// A restore target must name a file the agent could actually fetch.
		if c.RestoreTarget != "" && !IsBackupName(c.RestoreTarget) {
			t.Fatalf("input %q produced the restore target %q", in, c.RestoreTarget)
		}
		if len(c.ProcessedSHAs) > ProcessedSHACap {
			t.Fatalf("input %q produced %d processed SHAs, cap is %d", in, len(c.ProcessedSHAs), ProcessedSHACap)
		}
		for _, sha := range c.ProcessedSHAs {
			if sha == "" {
				t.Fatalf("input %q left an empty SHA in the dedup ring", in)
			}
		}
		// Re-marshalling must succeed, because the controller writes the document
		// back after every read.
		if _, err := Marshal(c); err != nil {
			t.Fatalf("input %q produced an unmarshallable document: %v", in, err)
		}
	})
}

// FuzzUnmarshalAgent guards the document the controller trusts for the player
// count and the backup outcome — the two places v1 got wrong.
func FuzzUnmarshalAgent(f *testing.F) {
	loc := time.UTC
	f.Add(`{}`)
	f.Add(``)
	f.Add(`{"phase": "no-such-phase"}`)
	f.Add(`{"players_count": 0}`) // the v1 hardcoded value, unstamped
	f.Add(`{"players_count": 7}`) // ditto, non-zero
	f.Add(`{"players_count": -9, "restarts": -3}`)
	f.Add(`{"backup": {}}`) // report with no request id
	f.Add(`{"backup": {"request_id": "r1", "state": "weird"}}`)
	f.Add(`{"backup": {"request_id": "r1", "state": "done"}}`) // done with no name
	if data, err := Marshal(NewAgent(loc)); err == nil {
		f.Add(string(data))
	}

	f.Fuzz(func(t *testing.T, in string) {
		a := NewAgent(loc)
		rep := Unmarshal([]byte(in), a)
		_ = rep.String()

		if !a.Phase.Valid() {
			t.Fatalf("input %q produced the unusable phase %q", in, a.Phase)
		}
		// The invariant behind the player-count fix: a count is either a
		// timestamped measurement or it is PlayersUnknown. There is no third case,
		// so no consumer can be fooled by a number nobody measured.
		if a.PlayersKnown() && a.PlayersAt.Zero() {
			t.Fatalf("input %q produced the count %d with no measurement timestamp", in, a.PlayersCount)
		}
		if !a.PlayersKnown() && a.PlayersCount != PlayersUnknown {
			t.Fatalf("input %q produced the unknown-count value %d, want %d", in, a.PlayersCount, PlayersUnknown)
		}
		if a.Restarts < 0 {
			t.Fatalf("input %q produced %d restarts", in, a.Restarts)
		}
		if a.Backup != nil {
			if a.Backup.RequestID == "" {
				t.Fatalf("input %q produced a backup report with no request id", in)
			}
			if !a.Backup.State.Valid() {
				t.Fatalf("input %q produced the backup state %q", in, a.Backup.State)
			}
			if a.Backup.State == BackupDone && a.Backup.Name == "" {
				t.Fatalf("input %q produced a successful backup with no filename", in)
			}
		}
		if _, err := Marshal(a); err != nil {
			t.Fatalf("input %q produced an unmarshallable document: %v", in, err)
		}
	})
}

func FuzzParseStopAt(f *testing.F) {
	loc := time.UTC
	for _, s := range []string{"2026-08-19 23:00:00", "2026-08-19T23:00:00Z", "1787099745", "", "tomorrow", "0000-00-00 00:00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		when, err := ParseStopAt(in, loc)
		if err != nil {
			return
		}
		// A successful parse must be a usable instant. A zero time would read as
		// "stop immediately" forever, halting the server on every tick.
		if when.IsZero() {
			t.Fatalf("ParseStopAt(%q) succeeded with the zero time", in)
		}
	})
}

func FuzzParseBackupName(f *testing.F) {
	loc := time.UTC
	for _, s := range []string{"backup_20260819_013623.zip", "backup_00000000_000000.zip", "backup_20261332_996199.zip", "world.zip", ""} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		when, err := ParseBackupName(in, loc)
		if err != nil {
			return
		}
		// A name that parses must round-trip, or the index and the directory would
		// disagree about what a file is called.
		if got := NewBackupName(when, loc, nil); got != strings.TrimSpace(in) {
			t.Fatalf("ParseBackupName(%q) -> %s -> NewBackupName = %q", in, when, got)
		}
		if !IsBackupName(in) {
			t.Fatalf("%q parsed but IsBackupName says no", in)
		}
	})
}
