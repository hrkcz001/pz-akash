package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// The Prague requirement, as a test. The archive is created at an instant that is
// 23:36 UTC and 01:36 in Prague; the table must read 01:36 regardless of what the
// process thinks its own timezone is.
func TestBackupDatesRenderInTheConfiguredTimezone(t *testing.T) {
	o := testOptions(t)
	created := time.Date(2026, 8, 18, 23, 36, 23, 0, time.UTC)

	in := Inputs{
		Unlocked: Unlocked{Backups: true},
		Backups: &state.Backups{Items: []state.Backup{
			{Name: "backup_20260819_013623.zip", Size: 3 * 1024 * 1024, CreatedAt: state.At(created)},
		}},
	}

	p := BuildBackupsPage(o, in, RU)
	if len(p.Rows) != 1 {
		t.Fatalf("%d rows, want 1", len(p.Rows))
	}
	if got, want := p.Rows[0].Date, "2026-08-19 01:36:23"; got != want {
		t.Fatalf("Date = %q, want %q", got, want)
	}

	// And it is the configured zone, not the stamp's own: the same instant handed
	// over already rendered in UTC still reads as Prague.
	in.Backups.Items[0].CreatedAt = state.At(created.In(time.UTC))
	if got := BuildBackupsPage(o, in, RU).Rows[0].Date; got != "2026-08-19 01:36:23" {
		t.Fatalf("Date = %q for a UTC-carrying stamp", got)
	}

	// A stamp that was never set renders as a blank cell, not as the year 1.
	in.Backups.Items[0].CreatedAt = state.Stamp{}
	if got := BuildBackupsPage(o, in, RU).Rows[0].Date; got != "" {
		t.Fatalf("Date = %q for an unset stamp, want empty", got)
	}
}

// A locked page carries no row data at all. v1's lock was server-side too, but
// this is the property worth pinning: the archive names never reach the document.
func TestLockedBackupsPageHasNoRows(t *testing.T) {
	o := testOptions(t)
	in := Inputs{
		Backups: &state.Backups{Items: []state.Backup{
			{Name: "backup_20260819_013623.zip", Size: 1024, CreatedAt: state.Now(o.Loc)},
		}},
		DiskUsedPercent: 99,
	}

	p := BuildBackupsPage(o, in, RU)
	if p.Unlocked {
		t.Fatal("the page unlocked itself")
	}
	if len(p.Rows) != 0 {
		t.Fatalf("%d rows on a locked page", len(p.Rows))
	}
	// Nor the aggregates, which are also facts about what is on the disk.
	if p.Count != "" || p.Total != "" || p.Warning != "" {
		t.Fatalf("locked page leaked count=%q total=%q warning=%q", p.Count, p.Total, p.Warning)
	}
	// The chrome is still there: it is the same page with the table withheld.
	if p.Title == "" || len(p.Switcher) != len(Langs) {
		t.Fatalf("locked page lost its chrome: title=%q switcher=%d", p.Title, len(p.Switcher))
	}
}

// The two facts v1 could not report, because it had no index: whether a copy of
// an archive exists anywhere but on the ephemeral disk, and which archive the next
// boot will restore.
func TestBackupRowTags(t *testing.T) {
	o := testOptions(t)
	now := state.Now(o.Loc)
	in := Inputs{
		Unlocked:   Unlocked{Backups: true},
		Controller: &state.Controller{RestoreTarget: "backup_b.zip"},
		Backups: &state.Backups{Items: []state.Backup{
			{Name: "backup_a.zip", Size: 1024, CreatedAt: now, DownloadedAt: now},
			{Name: "backup_b.zip", Size: 2048, CreatedAt: now},
		}},
	}

	p := BuildBackupsPage(o, in, RU)
	if !p.Rows[0].Downloaded || p.Rows[0].DownloadedText != catalog[RU].Downloaded {
		t.Fatalf("row 0 = %v/%q, want a downloaded archive", p.Rows[0].Downloaded, p.Rows[0].DownloadedText)
	}
	if p.Rows[0].Restore {
		t.Fatal("row 0 was marked as the restore target")
	}
	if p.Rows[1].Downloaded || p.Rows[1].DownloadedText != catalog[RU].NotDownloaded {
		t.Fatalf("row 1 = %v/%q, want an archive with no copy", p.Rows[1].Downloaded, p.Rows[1].DownloadedText)
	}
	if !p.Rows[1].Restore || p.Rows[1].RestoreText == "" {
		t.Fatalf("row 1 = %v/%q, want the restore target marked", p.Rows[1].Restore, p.Rows[1].RestoreText)
	}
	if p.Rows[1].Href != "/backups/backup_b.zip" {
		t.Fatalf("Href = %q, want the archive path with no token in it", p.Rows[1].Href)
	}
	if strings.Contains(p.Rows[1].Href, "token") {
		t.Fatalf("Href = %q carries a credential", p.Rows[1].Href)
	}

	// An empty RestoreTarget marks nothing, rather than matching the archive whose
	// name is also empty.
	in.Controller.RestoreTarget = ""
	in.Backups.Items = append(in.Backups.Items, state.Backup{CreatedAt: now})
	for i, row := range BuildBackupsPage(o, in, RU).Rows {
		if row.Restore {
			t.Fatalf("row %d marked as the restore target with none set", i)
		}
	}
}

func TestBackupsCountAndTotal(t *testing.T) {
	o := testOptions(t)
	now := state.Now(o.Loc)

	mk := func(n int) Inputs {
		items := make([]state.Backup, n)
		for i := range items {
			items[i] = state.Backup{Name: "b.zip", Size: 1024 * 1024, CreatedAt: now}
		}
		return Inputs{Unlocked: Unlocked{Backups: true}, Backups: &state.Backups{Items: items}}
	}

	// The Russian rule, on the page rather than in a unit test of the rule.
	cases := map[int]string{0: "0 архивов", 1: "1 архив", 3: "3 архива", 5: "5 архивов", 11: "11 архивов", 21: "21 архив"}
	for n, want := range cases {
		if got := BuildBackupsPage(o, mk(n), RU).Count; got != want {
			t.Errorf("%d archives: Count = %q, want %q", n, got, want)
		}
	}

	if got := BuildBackupsPage(o, mk(3), RU).Total; got != "3.00 MB" {
		t.Fatalf("Total = %q, want two decimals", got)
	}
	if got := BuildBackupsPage(o, mk(0), EN).Count; got != "0 archives" {
		t.Fatalf("EN count = %q", got)
	}
	// No index document at all is an empty table, not a missing page.
	empty := BuildBackupsPage(o, Inputs{Unlocked: Unlocked{Backups: true}}, RU)
	if len(empty.Rows) != 0 || empty.Count != "0 архивов" {
		t.Fatalf("no index: %d rows, count %q", len(empty.Rows), empty.Count)
	}
}

// The disk warning is the only place the retention policy becomes visible to
// whoever has to act on it, so the threshold has to be the configured one.
func TestDiskWarning(t *testing.T) {
	o := testOptions(t) // warns at 85
	in := Inputs{Unlocked: Unlocked{Backups: true}, DiskUsedPercent: 84}

	if got := BuildBackupsPage(o, in, RU).Warning; got != "" {
		t.Fatalf("Warning = %q below the threshold", got)
	}

	in.DiskUsedPercent = 85
	warn := BuildBackupsPage(o, in, RU).Warning
	if !strings.Contains(warn, "85%") {
		t.Fatalf("Warning = %q, want the measured percentage in it", warn)
	}

	// -1 is "could not measure". A warning would be a claim about a disk nobody
	// looked at, and it must not read as 0% either.
	in.DiskUsedPercent = -1
	if got := BuildBackupsPage(o, in, RU).Warning; got != "" {
		t.Fatalf("Warning = %q for an unmeasured disk", got)
	}

	// Zero disables it, which is what an operator who does not want the line sets.
	o.DiskWarnPercent = 0
	in.DiskUsedPercent = 100
	if got := BuildBackupsPage(o, in, RU).Warning; got != "" {
		t.Fatalf("Warning = %q with the warning disabled", got)
	}
}
