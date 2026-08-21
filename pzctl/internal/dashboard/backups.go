package dashboard

import (
	"fmt"

	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// BackupsPage is the archive table.
//
// It has two shapes: a lock prompt, and the table. Which one it is comes from a
// cookie the unlock endpoint set, not from a token in the URL — v1 put the
// backups password in the query string of every row's download link, and in the
// form action, so it reached browser history and the access log.
type BackupsPage struct {
	Chrome

	Unlocked bool
	// Error is the message under the password field after a failed attempt.
	Error string

	Rows  []BackupRow
	Count string
	Total string

	// Warning is the disk-pressure line, empty when there is no pressure or no
	// measurement. It is the only place the retention policy becomes visible to
	// whoever has to act on it.
	Warning string
}

// BackupRow is one archive.
type BackupRow struct {
	Name string
	Date string
	Size string

	// Downloaded is the property that matters most on this page and that v1 had
	// no way to know. With no persistent storage, an archive nobody has fetched
	// exists in exactly one place — a disk that dies with the lease.
	Downloaded     bool
	DownloadedText string

	// Restore marks the archive the next start will restore.
	Restore     bool
	RestoreText string

	Href string
}

// BuildBackupsPage assembles the archive table for one locale.
//
// A locked render still returns the chrome and the switcher, so the lock prompt
// is the same page with the table withheld — and it carries no row data at all,
// rather than rendering hidden rows the way a client-side lock would.
func BuildBackupsPage(o Options, in Inputs, want Lang) BackupsPage {
	lang := o.lang(want)
	t := catalog[lang]

	p := BackupsPage{
		Chrome: Chrome{
			Lang:     lang,
			T:        t,
			Switcher: o.switcher(lang),
			Title:    t.BackupsPageTitle,
			Active:   "backups",
		},
		Unlocked: in.Unlocked.Backups,
	}
	if !p.Unlocked {
		return p
	}

	var target string
	if in.Controller != nil {
		target = in.Controller.RestoreTarget
	}

	var total int64
	items := []state.Backup{}
	if in.Backups != nil {
		items = in.Backups.Items
	}
	for _, b := range items {
		row := BackupRow{
			Name: b.Name,
			// In o.Loc, so the table reads in the configured timezone rather
			// than wherever the container happens to think it is. v1 called
			// datetime.fromtimestamp with no tzinfo, which is the host clock.
			Date:       stampText(b.CreatedAt, o),
			Size:       sizeText(b.Size, 2, "0 KB"),
			Downloaded: !b.DownloadedAt.Zero(),
			Href:       "/backups/" + b.Name,
		}
		if row.Downloaded {
			row.DownloadedText = t.Downloaded
		} else {
			row.DownloadedText = t.NotDownloaded
		}
		if b.Name != "" && b.Name == target {
			row.Restore, row.RestoreText = true, t.RestoreTarget
		}
		total += b.Size
		p.Rows = append(p.Rows, row)
	}

	n := len(p.Rows)
	p.Count = fmt.Sprintf("%d %s", n, lang.pluralize(n, t.Archives))
	p.Total = sizeText(total, 2, "0 KB")

	if o.DiskWarnPercent > 0 && in.DiskUsedPercent >= o.DiskWarnPercent {
		p.Warning = fmt.Sprintf(t.DiskWarning, in.DiskUsedPercent)
	}
	return p
}

// stampText formats an instant in the configured timezone, or returns the empty
// string for a stamp that was never set — so a missing timestamp renders as a
// blank cell instead of the year 1.
func stampText(s state.Stamp, o Options) string {
	if s.Zero() {
		return ""
	}
	return s.In(o.Loc).Time.Format("2006-01-02 15:04:05")
}
