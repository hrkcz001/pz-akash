package fsm

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hrkcz001/pz-akash/pzctl/internal/config"
	"github.com/hrkcz001/pz-akash/pzctl/internal/state"
)

// fakeStore is a BackupStore with no directory behind it.
//
// The contract is what these tests are about — who decides what exists, and what
// the machine does when the two disagree. internal/httpapi owns the filesystem half
// and tests it against a real directory; repeating that here would only make these
// tests slower and no more convincing.
type fakeStore struct {
	mu    sync.Mutex
	items []state.Backup
	now   func() time.Time

	// seeded is what Seed was handed, so a test can assert the machine offered its
	// published index rather than starting from nothing.
	seeded  *state.Backups
	seedN   int
	pruneN  int
	protect []string
}

func (f *fakeStore) hold(b state.Backup) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.items = append(f.items, b)
}

func (f *fakeStore) holds(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range f.items {
		if e.Name == name {
			return true
		}
	}
	return false
}

// downloaded records a fetch, the way the real store's MarkDownloaded does.
func (f *fakeStore) downloaded(name string, at state.Stamp) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.items {
		if f.items[i].Name == name {
			f.items[i].DownloadedAt = at
		}
	}
}

// forgetDownloads is what a fresh process sees. The disk carries names, sizes and
// mtimes; nothing on it says whether an operator ever fetched a copy, so a store
// that has only scanned the directory knows nothing about downloads.
func (f *fakeStore) forgetDownloads() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.items {
		f.items[i].DownloadedAt = state.Stamp{}
	}
}

// Index is a fresh copy every time, the way the real store's is: a caller that
// mutated a shared slice would be a second writer, which is the thing being
// prevented.
func (f *fakeStore) Index() *state.Backups {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := state.NewBackups()
	idx.Items = append(idx.Items, f.items...)
	idx.Sort()
	if f.now != nil {
		idx.UpdatedAt = state.At(f.now())
	}
	return idx
}

// Seed keeps the download stamps the published index carries and drops everything
// the directory does not have, which is the real store's rule.
func (f *fakeStore) Seed(published *state.Backups) *state.Backups {
	f.mu.Lock()
	f.seedN++
	f.seeded = published
	if published != nil {
		for i := range f.items {
			if old := published.Find(f.items[i].Name); old != nil {
				f.items[i].DownloadedAt = old.DownloadedAt
			}
		}
	}
	f.mu.Unlock()
	return f.Index()
}

func (f *fakeStore) Prune(policy state.RetentionPolicy, protect ...string) ([]string, error) {
	f.mu.Lock()
	f.pruneN++
	f.protect = protect
	f.mu.Unlock()

	policy.Protect = append(append([]string{}, policy.Protect...), protect...)
	now := time.Now()
	if f.now != nil {
		now = f.now()
	}
	doomed := f.Index().Expired(policy, now)

	f.mu.Lock()
	kept := f.items[:0]
	for _, e := range f.items {
		gone := false
		for _, d := range doomed {
			if e.Name == d {
				gone = true
				break
			}
		}
		if !gone {
			kept = append(kept, e)
		}
	}
	f.items = kept
	f.mu.Unlock()
	return doomed, nil
}

// storeHarness is online, with a storage layer wired in and the periodic cadence
// off so advancing the clock queues no backups nobody asked for.
func storeHarness(t *testing.T, tune func(*config.Config)) (*harness, *fakeStore) {
	t.Helper()
	f := &fakeStore{}
	h := newHarness(t, func(c *config.Config) {
		c.Backups.Interval = 0
		if tune != nil {
			tune(c)
		}
	}, func(d *Deps) { d.Backups = f })
	f.now = h.clk.now
	h.bringOnline()
	return h, f
}

// TestTheIndexComesFromTheStoreAndNotFromTheReport is invariant I10 at the seam.
//
// The agent's report and the directory can disagree about an archive's size, and
// only one of them is looking at the file: the report carries what the agent
// measured before uploading, the index carries what arrived. v1 believed the
// report — its backup_log was written from it — and that is one of the ways its
// index came to describe archives that were not what it said they were.
func TestTheIndexComesFromTheStoreAndNotFromTheReport(t *testing.T) {
	t.Parallel()
	h, f := storeHarness(t, nil)

	const name = "backup_20260819_120000.zip"
	f.hold(state.Backup{
		Name: name, Size: 4096, SHA256: strings.Repeat("cd", 32), CreatedAt: h.stamp(),
	})

	h.trigger("backup", "")
	h.poll()
	// A different size and a different digest from the ones the store holds. The
	// harness always reports "ab"*32.
	h.agentBackup(state.BackupDone, name, 1<<20)
	h.poll()

	idx := h.m.idx
	e := idx.Find(name)
	if e == nil {
		h.dumpLogs()
		t.Fatalf("the archive is not in the index: %v", idx.Names())
	}
	if e.Size != 4096 || e.SHA256 != strings.Repeat("cd", 32) {
		t.Fatalf("index entry = size %d sha %s, want the store's 4096 and cd… — "+
			"the machine wrote the report's numbers over the directory's",
			e.Size, e.SHA256)
	}
	if len(h.m.idx.Items) != 1 {
		t.Fatalf("index = %v, want exactly what the store holds", h.m.idx.Names())
	}
	// And the archive is what the next boot restores, since nothing pinned anything
	// else.
	if got := h.m.doc.RestoreTarget; got != name {
		t.Fatalf("restore_target = %q, want %q", got, name)
	}
}

// TestAReportForAnArchiveTheStoreDoesNotHoldIsNotFollowed is bug 4's shape.
//
// The agent says done and the upload did not land — it failed, or it went somewhere
// else. Following the name anyway points the next boot at an archive the controller
// cannot serve, and the failure surfaces at the worst possible moment: a restore.
// Refusing it leaves the previous target in place, which is a world that exists.
func TestAReportForAnArchiveTheStoreDoesNotHoldIsNotFollowed(t *testing.T) {
	t.Parallel()
	h, f := storeHarness(t, nil)

	const good = "backup_20260818_120000.zip"
	f.hold(state.Backup{Name: good, Size: 8, CreatedAt: h.stamp()})
	h.poll()
	h.m.doc.RestoreTarget = good

	h.trigger("backup", "")
	h.poll()
	h.agentBackup(state.BackupDone, "backup_20260819_130000.zip", 1<<20)
	h.poll()

	if got := h.m.doc.RestoreTarget; got != good {
		h.dumpLogs()
		t.Fatalf("restore_target = %q, want the archive that exists (%q)", got, good)
	}
	if !h.logged("not in the index") {
		h.dumpLogs()
		t.Fatal("an upload that did not land was followed silently")
	}
	// The request still settles. A halt whose final backup vanished must still reach
	// closing, or the lease bills until someone notices.
	h.wantStatus(state.StatusOnline)
	if h.m.doc.BackupRequest != nil {
		t.Fatalf("the request is still outstanding: %+v", h.m.doc.BackupRequest)
	}
}

// TestTheTickPrunesAndProtectsTheRestoreTarget: retention runs on the housekeeping
// event, and it may not delete the archive the next boot is going to ask for.
//
// Retention was implemented and had no caller until step 6, which on a 20 GiB
// volume is a controller that fills its own disk. And an unprotected prune is worse
// than none: it deletes the one archive whose name is written into the document as
// the thing to restore.
func TestTheTickPrunesAndProtectsTheRestoreTarget(t *testing.T) {
	t.Parallel()
	h, f := storeHarness(t, func(c *config.Config) {
		c.Backups.RetentionCount = 1
		c.Backups.RetentionDays = 0
	})

	const (
		target = "backup_20260817_120000.zip"
		middle = "backup_20260818_120000.zip"
		newest = "backup_20260819_120000.zip"
	)
	base := h.clk.now()
	f.hold(state.Backup{Name: target, Size: 8, CreatedAt: state.At(base.Add(-48 * time.Hour))})
	f.hold(state.Backup{Name: middle, Size: 8, CreatedAt: state.At(base.Add(-24 * time.Hour))})
	f.hold(state.Backup{Name: newest, Size: 8, CreatedAt: state.At(base)})

	h.poll()
	h.m.doc.RestoreTarget = target

	h.tick()

	if f.pruneN == 0 {
		h.dumpLogs()
		t.Fatal("the tick did not prune, so retention has no driver at all")
	}
	if !f.holds(target) {
		h.dumpLogs()
		t.Fatalf("the prune deleted restore_target %q", target)
	}
	if f.holds(middle) {
		t.Fatalf("retention_count: 1 kept %q", middle)
	}
	if !f.holds(newest) {
		t.Fatalf("retention_count: 1 deleted the newest archive")
	}
	// And the published index followed the deletion. An index that still lists a
	// pruned archive is exactly the drift I10 forbids.
	if h.m.idx.Has(middle) {
		t.Fatalf("the index still lists the pruned %q: %v", middle, h.m.idx.Names())
	}
	if got := f.protect; len(got) != 1 || got[0] != target {
		t.Fatalf("Prune was protected with %v, want just the restore target", got)
	}
}

// TestLoadSeedsTheStoreFromThePublishedIndex: the disk decides what exists, but the
// published index is the only record of what has been downloaded.
//
// With no persistent storage — the locked design — a restarted controller may come
// back on an empty volume or a warm one, and it cannot tell which without looking.
// Seeding is how the answer gets taken from the directory while the one fact the
// directory cannot hold is taken from git.
func TestLoadSeedsTheStoreFromThePublishedIndex(t *testing.T) {
	t.Parallel()
	h, f := storeHarness(t, nil)

	const name = "backup_20260819_120000.zip"
	f.hold(state.Backup{Name: name, Size: 8, CreatedAt: h.stamp()})
	h.poll()
	// An operator fetches it. The store is what records that, and the poll publishes
	// the index it hands over.
	f.downloaded(name, h.stamp())
	h.poll()
	if e := h.m.idx.Find(name); e == nil || e.DownloadedAt.Zero() {
		h.dumpLogs()
		t.Fatalf("the download was never published: %+v", e)
	}

	// The restart. The process comes back on the same volume, so the files are there,
	// but its store has only scanned the directory and the directory does not record
	// downloads.
	f.forgetDownloads()
	f.seeded, f.seedN = nil, 0
	if err := h.m.load(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.seedN != 1 {
		t.Fatalf("Seed called %d time(s) during load, want exactly 1", f.seedN)
	}
	if f.seeded == nil || !f.seeded.Has(name) {
		t.Fatalf("the machine seeded the store with %v, want the published index", f.seeded)
	}
	if e := h.m.idx.Find(name); e == nil || e.DownloadedAt.Zero() {
		h.dumpLogs()
		t.Fatalf("the download stamp did not survive the restart: %+v", e)
	}
}
