package state

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// backupStamp is the wall-clock layout inside a backup filename. It matches v1
// byte for byte (backup_20260819_013623.zip) so existing archives stay valid.
const backupStamp = "20060102_150405"

var backupNameRe = regexp.MustCompile(`^backup_(\d{8}_\d{6})\.zip$`)

// NewBackupName builds a backup filename from an instant, rendered in loc.
//
// The name is deliberately local time, not UTC: an operator scanning a list of
// backups is reasoning about "the one from last night", and a name eight hours
// off makes that guesswork. loc comes from identity.timezone, so it is one
// config value rather than a property of whichever machine happened to run the
// backup — which is what made v1's names UTC by accident (the controller image
// has no zoneinfo and no TZ set).
//
// The corollary is that a local-time name is NOT a valid sort key. Across the
// autumn DST boundary, Prague repeats 02:00–03:00, so two distinct backups can
// carry names that sort in the wrong order or, in the worst case, collide.
// Ordering and retention therefore always use Backup.CreatedAt, which is an
// absolute instant. NewBackupName never returns a name already present in the
// index; it steps forward a second until it is unique.
func NewBackupName(t time.Time, loc *time.Location, taken func(string) bool) string {
	if loc == nil {
		loc = time.UTC
	}
	t = t.In(loc)
	for i := 0; ; i++ {
		name := "backup_" + t.Add(time.Duration(i)*time.Second).Format(backupStamp) + ".zip"
		if taken == nil || !taken(name) {
			return name
		}
		if i > 120 {
			// Pathological: 120 names in the same two minutes are all taken.
			// Better a slightly odd name than an infinite loop.
			return "backup_" + t.Format(backupStamp) + fmt.Sprintf("_%d.zip", i)
		}
	}
}

// ParseBackupName recovers the wall-clock time encoded in a backup filename,
// interpreted in loc. Used only for archives with no index entry — files left
// behind by v1, or recovered from an operator's download folder.
//
// During a DST fallback the encoded time is ambiguous; ParseInLocation resolves
// it to the first (pre-shift) occurrence, so a recovered timestamp can be off by
// an hour. That is why the index stores the instant separately.
func ParseBackupName(name string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	m := backupNameRe.FindStringSubmatch(strings.TrimSpace(name))
	if m == nil {
		return time.Time{}, fmt.Errorf("%q is not a backup filename (want backup_YYYYMMDD_HHMMSS.zip)", name)
	}
	return time.ParseInLocation(backupStamp, m[1], loc)
}

// IsBackupName reports whether name has the backup filename shape.
func IsBackupName(name string) bool { return backupNameRe.MatchString(strings.TrimSpace(name)) }

// Backup is one archive in the index.
type Backup struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`

	// CreatedAt is the authoritative instant, carrying an offset. Sort and
	// expire on this, never on Name.
	CreatedAt Stamp `json:"created_at"`

	// DownloadedAt is set the first time the archive is served to an operator.
	// With no persistent storage, a backup nobody has downloaded exists in
	// exactly one place, on an ephemeral disk; the dashboard uses this to say so.
	DownloadedAt Stamp `json:"downloaded_at,omitempty"`
}

// Backups is the index on the controller's state branch. It is derived from the
// contents of backups.dir and regenerated after every mutation, so the two
// cannot drift the way v1's backup_log drifted from restore_target.
type Backups struct {
	Version   int      `json:"version"`
	UpdatedAt Stamp    `json:"updated_at"`
	Items     []Backup `json:"items"`
}

// NewBackups returns an empty index at the current schema version.
func NewBackups() *Backups { return &Backups{Version: DocVersion, Items: []Backup{}} }

// Sort orders the index newest first, which is the order every consumer wants.
// Ties break on name so the output is deterministic.
func (b *Backups) Sort() {
	sort.SliceStable(b.Items, func(i, j int) bool {
		ti, tj := b.Items[i].CreatedAt.Time, b.Items[j].CreatedAt.Time
		if ti.Equal(tj) {
			return b.Items[i].Name > b.Items[j].Name
		}
		return ti.After(tj)
	})
}

// Find returns the entry for name, or nil.
func (b *Backups) Find(name string) *Backup {
	for i := range b.Items {
		if b.Items[i].Name == name {
			return &b.Items[i]
		}
	}
	return nil
}

// Has reports whether name is in the index.
func (b *Backups) Has(name string) bool { return b.Find(name) != nil }

// Newest returns the most recent entry, or nil for an empty index.
//
// The controller measures the periodic backup cadence from this rather than from
// a timer, so the schedule is a property of the published index and survives a
// controller restart: a controller that came back after five minutes of downtime
// does not owe the world an immediate backup, and one that came back after five
// hours does.
func (b *Backups) Newest() *Backup {
	if len(b.Items) == 0 {
		return nil
	}
	b.Sort()
	return &b.Items[0]
}

// Upsert adds or replaces an entry, preserving a DownloadedAt already recorded
// for that name, and re-sorts.
func (b *Backups) Upsert(e Backup) {
	if old := b.Find(e.Name); old != nil {
		if e.DownloadedAt.Zero() {
			e.DownloadedAt = old.DownloadedAt
		}
		*old = e
		b.Sort()
		return
	}
	b.Items = append(b.Items, e)
	b.Sort()
}

// Remove drops an entry. It reports whether anything was removed.
func (b *Backups) Remove(name string) bool {
	for i := range b.Items {
		if b.Items[i].Name == name {
			b.Items = append(b.Items[:i], b.Items[i+1:]...)
			return true
		}
	}
	return false
}

// Names returns the index names, newest first.
func (b *Backups) Names() []string {
	b.Sort()
	out := make([]string, 0, len(b.Items))
	for _, e := range b.Items {
		out = append(out, e.Name)
	}
	return out
}

// TotalBytes is the on-disk cost of every indexed archive.
func (b *Backups) TotalBytes() int64 {
	var n int64
	for _, e := range b.Items {
		n += e.Size
	}
	return n
}

// Undownloaded lists archives that have never been served, newest first. With
// no persistent storage these are the only ones a lease close would destroy.
func (b *Backups) Undownloaded() []Backup {
	b.Sort()
	var out []Backup
	for _, e := range b.Items {
		if e.DownloadedAt.Zero() {
			out = append(out, e)
		}
	}
	return out
}

// RetentionPolicy is the config-driven expiry rule.
type RetentionPolicy struct {
	// Days expires by age. Zero disables the age rule.
	Days int
	// Count keeps at most this many. Zero disables the count rule.
	Count int
	// Protect names archives that must survive regardless — in practice the
	// current restore_target, so a scheduled prune cannot delete the file the
	// next boot is going to ask for.
	Protect []string
}

// Expired returns the names that fail either rule, oldest first, ready to
// delete.
//
// The newest archive is never expired. Backups here are the only copy until an
// operator downloads one, so a policy that could empty the directory is a policy
// that loses the world.
func (b *Backups) Expired(p RetentionPolicy, now time.Time) []string {
	b.Sort()
	protected := make(map[string]bool, len(p.Protect))
	for _, n := range p.Protect {
		protected[n] = true
	}

	var doomed []string
	for i, e := range b.Items {
		if i == 0 || protected[e.Name] {
			continue
		}
		tooMany := p.Count > 0 && i >= p.Count
		tooOld := p.Days > 0 && !e.CreatedAt.Zero() &&
			now.Sub(e.CreatedAt.Time) > time.Duration(p.Days)*24*time.Hour
		if tooMany || tooOld {
			doomed = append(doomed, e.Name)
		}
	}
	// Oldest first: deleting in this order keeps the directory consistent with
	// the index at every intermediate step.
	for i, j := 0, len(doomed)-1; i < j; i, j = i+1, j-1 {
		doomed[i], doomed[j] = doomed[j], doomed[i]
	}
	return doomed
}
